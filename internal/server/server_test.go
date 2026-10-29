package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/StarMoe-org/ferry/internal/ferry"
	"github.com/StarMoe-org/ferry/internal/kube"
	"github.com/StarMoe-org/ferry/internal/kube/fake"
	"github.com/StarMoe-org/ferry/internal/state"
)

type fakeOIDCProvider struct {
	srv         *httptest.Server
	priv        *ecdsa.PrivateKey
	lastNonce   string
	lastChall   string
	nextEmail   string
	nextSub     string
	nextGroups  []string
}

func newFakeOIDCProvider(t *testing.T) *fakeOIDCProvider {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeOIDCProvider{priv: priv}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.srv.URL,
			"authorization_endpoint":                p.srv.URL + "/authorize",
			"token_endpoint":                        p.srv.URL + "/token",
			"jwks_uri":                              p.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"ES256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		byteLen := (elliptic.P256().Params().BitSize + 7) / 8
		xBytes := priv.PublicKey.X.FillBytes(make([]byte, byteLen))
		yBytes := priv.PublicKey.Y.FillBytes(make([]byte, byteLen))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []any{
				map[string]any{
					"kty": "EC",
					"crv": "P-256",
					"alg": "ES256",
					"use": "sig",
					"kid": "k1",
					"x":   base64.RawURLEncoding.EncodeToString(xBytes),
					"y":   base64.RawURLEncoding.EncodeToString(yBytes),
				},
			},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		verifier := r.Form.Get("code_verifier")
		if verifier == "" {
			http.Error(w, "missing PKCE code_verifier", http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256([]byte(verifier))
		gotChall := base64.RawURLEncoding.EncodeToString(sum[:])
		if gotChall != p.lastChall {
			http.Error(w, "PKCE S256 mismatch", http.StatusBadRequest)
			return
		}
		jwt := p.signES256Token(t, map[string]any{
			"iss":    p.srv.URL,
			"sub":    p.nextSub,
			"aud":    "ferry-client",
			"exp":    time.Now().Add(time.Hour).Unix(),
			"iat":    time.Now().Unix(),
			"nonce":  p.lastNonce,
			"email":  p.nextEmail,
			"name":   "Test Operator",
			"groups": p.nextGroups,
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "acc-123",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     jwt,
		})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeOIDCProvider) signES256Token(t *testing.T, claims map[string]any) string {
	t.Helper()
	hdrJSON, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": "k1"})
	claimsJSON, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(hdrJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	rInt, sInt, err := ecdsa.Sign(rand.Reader, p.priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	rInt.FillBytes(sig[:32])
	sInt.FillBytes(sig[32:])
	_ = crypto.SHA256
	_ = big.NewInt(0)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestOIDC_ES256_Groups_CSRF_CIAuth_Deploy_And_SPA(t *testing.T) {
	ctx := context.Background()
	idp := newFakeOIDCProvider(t)

	// Fake gitRead server counting probes to verify 10-minute positive cache.
	var gitProbes atomic.Int32
	gitSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gitProbes.Add(1)
		u, pass, ok := r.BasicAuth()
		if !ok || u != "cnb" || pass != "cnb-valid-tok" || r.URL.RawQuery != "service=git-upload-pack" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer gitSrv.Close()

	kSrv := fake.New()
	defer kSrv.Close()
	kSrv.Seed(map[string]any{
		"kind":     "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "app"},
		"spec": map[string]any{
			"replicas": 1,
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "web", "image": "ghcr.io/org/web:old"}},
				},
			},
		},
	})
	kCl, _ := kSrv.Client("de")
	st, err := state.NewFileStore(ctx, filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := &ferry.Config{
		Namespaces: []string{"app"},
		Auth: ferry.AuthConfig{
			SessionKey: "test-session-secret-key",
			OIDC: ferry.OIDCConfig{
				Issuer:        idp.srv.URL,
				ClientID:      "ferry-client",
				ClientSecret:  "ferry-secret",
				PublicURL:     "http://ferry.local",
				AllowedGroups: []string{"ops"},
			},
			GitRead: []ferry.GitReadTarget{
				{URL: gitSrv.URL + "/org/builder.git", Username: "cnb"},
			},
		},
	}
	eng, err := ferry.New(cfg, []*kube.Client{kCl}, st)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	spaFS := fstest.MapFS{
		"index.html":        &fstest.MapFile{Data: []byte("<!doctype html><title>Ferry SPA</title>")},
		"assets/app-123.js": &fstest.MapFile{Data: []byte("console.log('ferry')")},
	}
	handler, err := New(eng, Options{SPAFS: spaFS})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	defer ts.Close()

	noRedirect := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// 1. Healthz requires no auth.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %v, %v", resp, err)
	}
	resp.Body.Close()

	// 2. Unauthenticated /api/me -> 401.
	resp, _ = http.Get(ts.URL + "/api/me")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on /api/me, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 3. OIDC login -> unauthorized group -> friendly 403 HTML naming account!
	loginResp, err := noRedirect.Get(ts.URL + "/auth/login?next=/services")
	if err != nil || loginResp.StatusCode != http.StatusFound {
		t.Fatalf("GET /auth/login = %v, %v", loginResp, err)
	}
	authLoc, _ := url.Parse(loginResp.Header.Get("Location"))
	idp.lastNonce = authLoc.Query().Get("nonce")
	idp.lastChall = authLoc.Query().Get("code_challenge")
	if authLoc.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("expected PKCE S256 challenge, got %s", authLoc.RawQuery)
	}
	var oidcCookie *http.Cookie
	for _, c := range loginResp.Cookies() {
		if c.Name == oidcCookieName {
			oidcCookie = c
		}
	}
	loginResp.Body.Close()
	if oidcCookie == nil || !oidcCookie.HttpOnly {
		t.Fatalf("missing HttpOnly oidc flow cookie")
	}

	idp.nextSub = "u-outsider"
	idp.nextEmail = "outsider@example.com"
	idp.nextGroups = []string{"marketing"}
	cbReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/auth/callback?code=c1&state="+url.QueryEscape(authLoc.Query().Get("state")), nil)
	cbReq.AddCookie(oidcCookie)
	cbResp, err := noRedirect.Do(cbReq)
	if err != nil || cbResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for unauthorized group, got %v, %v", cbResp, err)
	}
	cbBody, _ := io.ReadAll(cbResp.Body)
	cbResp.Body.Close()
	if !strings.Contains(string(cbBody), "outsider@example.com") {
		t.Fatalf("403 page did not name user account: %s", cbBody)
	}

	// 4. OIDC login -> allowed group "ops" -> 302 to /services with session cookie!
	loginResp2, _ := noRedirect.Get(ts.URL + "/auth/login?next=/services")
	authLoc2, _ := url.Parse(loginResp2.Header.Get("Location"))
	idp.lastNonce = authLoc2.Query().Get("nonce")
	idp.lastChall = authLoc2.Query().Get("code_challenge")
	var oidcCookie2 *http.Cookie
	for _, c := range loginResp2.Cookies() {
		if c.Name == oidcCookieName {
			oidcCookie2 = c
		}
	}
	loginResp2.Body.Close()

	idp.nextSub = "u-alice"
	idp.nextEmail = "alice@example.com"
	idp.nextGroups = []string{"ops"}
	cbReq2, _ := http.NewRequest(http.MethodGet, ts.URL+"/auth/callback?code=c2&state="+url.QueryEscape(authLoc2.Query().Get("state")), nil)
	cbReq2.AddCookie(oidcCookie2)
	cbResp2, err := noRedirect.Do(cbReq2)
	if err != nil || cbResp2.StatusCode != http.StatusFound || cbResp2.Header.Get("Location") != "/services" {
		t.Fatalf("expected 302 to /services, got %v, %v", cbResp2, err)
	}
	var sessCookie *http.Cookie
	for _, c := range cbResp2.Cookies() {
		if c.Name == sessionCookieName {
			sessCookie = c
		}
	}
	cbResp2.Body.Close()
	if sessCookie == nil || !sessCookie.HttpOnly || sessCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("invalid session cookie: %+v", sessCookie)
	}

	// Tampered session cookie must 401.
	badReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/me", nil)
	badReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessCookie.Value + "x"})
	badResp, _ := http.DefaultClient.Do(badReq)
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tampered session cookie returned %d", badResp.StatusCode)
	}
	badResp.Body.Close()

	// Unknown /api/* route returns 404 JSON (not 403 or HTML).
	unkReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/does-not-exist", nil)
	unkReq.AddCookie(sessCookie)
	unkResp, _ := http.DefaultClient.Do(unkReq)
	if unkResp.StatusCode != http.StatusNotFound || !strings.HasPrefix(unkResp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("unknown /api route = %d %s", unkResp.StatusCode, unkResp.Header.Get("Content-Type"))
	}
	unkResp.Body.Close()

	// 5. CSRF check: mutating POST without Content-Type: application/json -> 415.
	csrfReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/tokens", strings.NewReader(`{"name":"ci"}`))
	csrfReq.AddCookie(sessCookie)
	csrfReq.Header.Set("Content-Type", "text/plain")
	csrfResp, _ := http.DefaultClient.Do(csrfReq)
	if csrfResp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 CSRF rejection, got %d", csrfResp.StatusCode)
	}
	csrfResp.Body.Close()

	// Create deploy token via POST /api/tokens with JSON Content-Type.
	tokReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/tokens", strings.NewReader(`{"name":"gha"}`))
	tokReq.AddCookie(sessCookie)
	tokReq.Header.Set("Content-Type", "application/json")
	tokResp, err := http.DefaultClient.Do(tokReq)
	if err != nil || tokResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/tokens = %v, %v", tokResp, err)
	}
	var createdTok struct {
		Token string          `json:"token"`
		Info  state.TokenInfo `json:"info"`
	}
	_ = json.NewDecoder(tokResp.Body).Decode(&createdTok)
	tokResp.Body.Close()

	// 6. Legacy POST /deploy compatibility:
	// (a) Invalid bearer -> 401 with "needs a deploy token or a token that can read ..."
	badDepReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/deploy", strings.NewReader(`{"image":"ghcr.io/org/web","tag":"v1"}`))
	badDepReq.Header.Set("Authorization", "Bearer bad-tok")
	badDepResp, _ := http.DefaultClient.Do(badDepReq)
	badDepBody, _ := io.ReadAll(badDepResp.Body)
	badDepResp.Body.Close()
	if badDepResp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(badDepBody), "needs a deploy token or a token that can read") {
		t.Fatalf("bad deploy auth = %d %q", badDepResp.StatusCode, string(badDepBody))
	}

	// (b) Deploy token works -> 200 JSON array of Results
	depReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/deploy", bytes.NewBufferString(`{"image":"ghcr.io/org/web","tag":"v1","source":"github:org/web@1234"}`))
	depReq.Header.Set("Authorization", "Bearer "+createdTok.Token)
	depReq.Header.Set("Content-Type", "application/json")
	depResp, err := http.DefaultClient.Do(depReq)
	if err != nil || depResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /deploy with deploy token = %v, %v", depResp, err)
	}
	var depResults []state.Result
	_ = json.NewDecoder(depResp.Body).Decode(&depResults)
	depResp.Body.Close()
	if len(depResults) != 1 || depResults[0].Result != "deployed" {
		t.Fatalf("unexpected deploy results: %+v", depResults)
	}

	// (c) gitRead bearer works and caches positive result for 10m across multiple calls!
	probesBefore := gitProbes.Load()
	for i := 0; i < 2; i++ {
		grReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/deploy", bytes.NewBufferString(`{"image":"ghcr.io/org/web","tag":"v1"}`))
		grReq.Header.Set("Authorization", "Bearer cnb-valid-tok")
		grResp, err := http.DefaultClient.Do(grReq)
		if err != nil || grResp.StatusCode != http.StatusOK {
			t.Fatalf("POST /deploy gitRead #%d = %v, %v", i, grResp, err)
		}
		grResp.Body.Close()
	}
	if got := gitProbes.Load() - probesBefore; got != 1 {
		t.Fatalf("expected 1 gitRead HTTP probe due to 10m cache, got %d", got)
	}

	// (d) Cluster PATCH error returns HTTP 500 with Results JSON array (preserving bump.sh contract!).
	kSrv.SetForbidden("PATCH", "deployments", true)
	errDepReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/deploy", bytes.NewBufferString(`{"image":"ghcr.io/org/web","tag":"v2"}`))
	errDepReq.Header.Set("Authorization", "Bearer "+createdTok.Token)
	errDepResp, _ := http.DefaultClient.Do(errDepReq)
	if errDepResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 when patch fails, got %d", errDepResp.StatusCode)
	}
	var errResults []state.Result
	_ = json.NewDecoder(errDepResp.Body).Decode(&errResults)
	errDepResp.Body.Close()
	if len(errResults) != 1 || errResults[0].Result != "error" {
		t.Fatalf("expected error Result in 500 body, got %+v", errResults)
	}

	// 7. SPA serving & Cache-Control headers.
	spaRoot, _ := http.Get(ts.URL + "/services")
	if spaRoot.StatusCode != http.StatusOK || spaRoot.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("SPA fallback = %d cache=%q", spaRoot.StatusCode, spaRoot.Header.Get("Cache-Control"))
	}
	spaRoot.Body.Close()

	assetResp, _ := http.Get(ts.URL + "/assets/app-123.js")
	if assetResp.StatusCode != http.StatusOK || !strings.Contains(assetResp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("SPA asset = %d cache=%q", assetResp.StatusCode, assetResp.Header.Get("Cache-Control"))
	}
	assetResp.Body.Close()

	missingAsset, _ := http.Get(ts.URL + "/assets/missing.js")
	if missingAsset.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing hashed asset, got %d", missingAsset.StatusCode)
	}
	missingAsset.Body.Close()

	// 8. Legacy POST / webhook alias compatibility (Compatibility #2):
	// GET / serves SPA HTML, while POST / routes to GitHub webhook handler!
	postRoot, _ := http.Post(ts.URL+"/", "application/json", strings.NewReader(`{}`))
	postRootBody, _ := io.ReadAll(postRoot.Body)
	postRoot.Body.Close()
	if postRoot.StatusCode != http.StatusUnauthorized || strings.TrimSpace(string(postRootBody)) != "bad signature" {
		t.Fatalf("POST / = %d %q, want 401 bad signature", postRoot.StatusCode, string(postRootBody))
	}
}
