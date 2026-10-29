// Package server exposes Ferry's HTTP API, legacy ci-relay endpoints, OIDC/CI auth, and embedded SPA.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/StarMoe-org/ferry/internal/ferry"
)

const (
	sessionCookieName = "ferry_session"
	oidcCookieName    = "ferry_oidc"
	oidcFlowTTL       = 10 * time.Minute
)

// SessionUser represents an authenticated browser user.
type SessionUser struct {
	Sub          string   `json:"sub"`
	Name         string   `json:"name"`
	Email        string   `json:"email"`
	Groups       []string `json:"groups,omitempty"`
	AuthDisabled bool     `json:"authDisabled"`
	Exp          int64    `json:"exp"`
}

func (u SessionUser) Actor() string {
	if u.Email != "" {
		return u.Email
	}
	if u.Name != "" {
		return u.Name
	}
	return u.Sub
}

type oidcFlowState struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Next     string `json:"next"`
	Exp      int64  `json:"exp"`
}

type browserAuth struct {
	cfg        ferry.AuthConfig
	sessionKey []byte
	secure     bool
	httpClient *http.Client

	mu       sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauthCfg oauth2.Config
}

// sessionTTL is how long a browser sign-in lasts (auth.sessionHours, 12 by default).
func (a *browserAuth) sessionTTL() time.Duration {
	if a.cfg.SessionHours <= 0 {
		return 12 * time.Hour
	}
	return time.Duration(a.cfg.SessionHours) * time.Hour
}

func newBrowserAuth(cfg ferry.AuthConfig, hc *http.Client) *browserAuth {
	var key []byte
	if strings.TrimSpace(cfg.SessionKey) != "" {
		sum := sha256.Sum256([]byte(cfg.SessionKey))
		key = sum[:]
	} else {
		key = make([]byte, 32)
		_, _ = rand.Read(key)
		if !cfg.Disabled {
			log.Printf("WARN: auth.sessionKey is empty; generated ephemeral session key (sessions will reset on restart)")
		}
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &browserAuth{
		cfg:        cfg,
		sessionKey: key,
		secure:     strings.HasPrefix(strings.ToLower(cfg.OIDC.PublicURL), "https://"),
		httpClient: hc,
	}
}

func (a *browserAuth) ensureOIDC(ctx context.Context) (*oidc.IDTokenVerifier, oauth2.Config, error) {
	a.mu.Lock()
	if a.verifier != nil {
		v, oc := a.verifier, a.oauthCfg
		a.mu.Unlock()
		return v, oc, nil
	}
	a.mu.Unlock()

	pCtx := oidc.ClientContext(ctx, a.httpClient)
	prov, err := oidc.NewProvider(pCtx, a.cfg.OIDC.Issuer)
	if err != nil {
		return nil, oauth2.Config{}, fmt.Errorf("oidc discovery failed: %w", err)
	}
	ver := prov.Verifier(&oidc.Config{
		ClientID: a.cfg.OIDC.ClientID,
		SupportedSigningAlgs: []string{
			oidc.RS256, oidc.RS384, oidc.RS512,
			oidc.ES256, oidc.ES384, oidc.ES512,
			oidc.PS256, oidc.PS384, oidc.PS512,
			oidc.EdDSA,
		},
	})
	oc := oauth2.Config{
		ClientID:     a.cfg.OIDC.ClientID,
		ClientSecret: a.cfg.OIDC.ClientSecret,
		Endpoint:     prov.Endpoint(),
		RedirectURL:  strings.TrimRight(a.cfg.OIDC.PublicURL, "/") + "/auth/callback",
		Scopes:       a.cfg.OIDC.Scopes,
	}

	a.mu.Lock()
	a.provider = prov
	a.verifier = ver
	a.oauthCfg = oc
	a.mu.Unlock()
	return ver, oc, nil
}

func (a *browserAuth) currentUser(r *http.Request) (*SessionUser, bool) {
	if a.cfg.Disabled {
		return &SessionUser{
			Sub:          "dev",
			Name:         "Dev User",
			Email:        "dev@localhost",
			AuthDisabled: true,
		}, true
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil, false
	}
	var u SessionUser
	if err := a.verifyCookiePayload(c.Value, &u); err != nil {
		return nil, false
	}
	if time.Now().Unix() > u.Exp {
		return nil, false
	}
	return &u, true
}

func (a *browserAuth) handleLogin(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Disabled {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	_, oauthCfg, err := a.ensureOIDC(r.Context())
	if err != nil {
		http.Error(w, "OIDC provider unavailable", http.StatusBadGateway)
		return
	}

	stateStr, err := randomURLToken(18)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nonceStr, err := randomURLToken(18)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()
	next := sanitizeNextPath(r.URL.Query().Get("next"))

	flow := oidcFlowState{
		State:    stateStr,
		Nonce:    nonceStr,
		Verifier: verifier,
		Next:     next,
		Exp:      time.Now().Add(oidcFlowTTL).Unix(),
	}
	signed, err := a.signCookiePayload(flow)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oidcCookieName,
		Value:    signed,
		Path:     "/",
		MaxAge:   int(oidcFlowTTL.Seconds()),
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})

	authURL := oauthCfg.AuthCodeURL(
		stateStr,
		oauth2.S256ChallengeOption(verifier),
		oidc.Nonce(nonceStr),
	)
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (a *browserAuth) handleCallback(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Disabled {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	ver, oauthCfg, err := a.ensureOIDC(r.Context())
	if err != nil {
		http.Error(w, "OIDC provider unavailable", http.StatusBadGateway)
		return
	}

	c, err := r.Cookie(oidcCookieName)
	// Always clear the one-time OIDC flow cookie.
	http.SetCookie(w, &http.Cookie{
		Name:     oidcCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
	if err != nil || c.Value == "" {
		http.Error(w, "missing login state cookie", http.StatusBadRequest)
		return
	}
	var flow oidcFlowState
	if err := a.verifyCookiePayload(c.Value, &flow); err != nil || time.Now().Unix() > flow.Exp {
		http.Error(w, "invalid or expired login state", http.StatusBadRequest)
		return
	}

	gotState := r.URL.Query().Get("state")
	if len(gotState) != len(flow.State) || subtle.ConstantTimeCompare([]byte(gotState), []byte(flow.State)) != 1 {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}

	pCtx := oidc.ClientContext(r.Context(), a.httpClient)
	tok, err := oauthCfg.Exchange(pCtx, code, oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		http.Error(w, "token exchange failed", http.StatusUnauthorized)
		return
	}
	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		http.Error(w, "missing id_token in token response", http.StatusUnauthorized)
		return
	}
	idTok, err := ver.Verify(pCtx, rawIDToken)
	if err != nil {
		http.Error(w, "invalid id_token", http.StatusUnauthorized)
		return
	}
	if len(idTok.Nonce) != len(flow.Nonce) || subtle.ConstantTimeCompare([]byte(idTok.Nonce), []byte(flow.Nonce)) != 1 {
		http.Error(w, "nonce mismatch", http.StatusUnauthorized)
		return
	}

	var rawClaims map[string]any
	if err := idTok.Claims(&rawClaims); err != nil {
		http.Error(w, "invalid id_token claims", http.StatusUnauthorized)
		return
	}
	email, _ := rawClaims["email"].(string)
	name, _ := rawClaims["name"].(string)
	prefUser, _ := rawClaims["preferred_username"].(string)
	if name == "" {
		name = prefUser
	}
	if name == "" {
		name = email
	}
	if name == "" {
		name = idTok.Subject
	}
	groups := extractStringSlice(rawClaims[a.cfg.OIDC.GroupsClaim])

	if !a.isAllowed(idTok.Subject, email, prefUser, groups) {
		accountLabel := email
		if accountLabel == "" {
			accountLabel = prefUser
		}
		if accountLabel == "" {
			accountLabel = idTok.Subject
		}
		renderForbiddenHTML(w, accountLabel)
		return
	}

	sess := SessionUser{
		Sub:    idTok.Subject,
		Name:   name,
		Email:  email,
		Groups: groups,
		Exp:    time.Now().Add(a.sessionTTL()).Unix(),
	}
	signed, err := a.signCookiePayload(sess)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    signed,
		Path:     "/",
		MaxAge:   int(a.sessionTTL().Seconds()),
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, sanitizeNextPath(flow.Next), http.StatusFound)
}

func (a *browserAuth) handleLogout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (a *browserAuth) isAllowed(sub, email, prefUser string, groups []string) bool {
	for _, want := range a.cfg.OIDC.AllowedGroups {
		for _, g := range groups {
			if g == want {
				return true
			}
		}
	}
	for _, want := range a.cfg.OIDC.AllowedUsers {
		if want == "" {
			continue
		}
		if sub == want || (email != "" && strings.EqualFold(email, want)) || (prefUser != "" && prefUser == want) {
			return true
		}
	}
	return false
}

func (a *browserAuth) signCookiePayload(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + sig, nil
}

func (a *browserAuth) verifyCookiePayload(val string, out any) error {
	dot := strings.IndexByte(val, '.')
	if dot <= 0 || dot >= len(val)-1 {
		return errors.New("malformed cookie")
	}
	payload, sigB64 := val[:dot], val[dot+1:]
	gotSig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write([]byte(payload))
	wantSig := mac.Sum(nil)
	if !hmac.Equal(gotSig, wantSig) {
		return errors.New("bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func randomURLToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func sanitizeNextPath(next string) string {
	next = strings.TrimSpace(next)
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, `\`) {
		return "/"
	}
	return next
}

func extractStringSlice(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if x != "" {
			return []string{x}
		}
	}
	return nil
}

var forbiddenTmpl = template.Must(template.New("forbidden").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Access Denied · Ferry</title>
  <style>
    body { margin: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: #FAF9F5; color: #141413; display: grid; place-items: center; min-height: 100vh; padding: 24px; }
    .card { max-width: 440px; background: #FFFFFF; border: 1px solid #E8E6DC; border-radius: 14px; padding: 32px; box-shadow: 0 2px 12px rgba(20, 20, 19, 0.04); }
    h1 { font-family: Georgia, serif; font-weight: 500; font-size: 24px; margin: 0 0 12px; }
    p { font-size: 14px; line-height: 1.6; color: #6B6A65; margin: 0 0 20px; }
    code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; background: #F3F1EA; color: #141413; padding: 2px 6px; border-radius: 5px; }
    a { display: inline-block; background: #C96442; color: #FFFFFF; text-decoration: none; padding: 9px 16px; border-radius: 8px; font-size: 14px; font-weight: 500; }
  </style>
</head>
<body>
  <div class="card">
    <h1>Access not permitted</h1>
    <p>Signed in as <code>{{.Account}}</code>, which is not in an allowed group or user list for this Ferry instance.</p>
    <a href="/auth/login">Sign in with another account</a>
  </div>
</body>
</html>`))

func renderForbiddenHTML(w http.ResponseWriter, account string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_ = forbiddenTmpl.Execute(w, struct{ Account string }{Account: account})
}
