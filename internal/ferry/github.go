package ferry

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/StarMoe-org/ferry/internal/state"
)

// HandleGitHubWebhook verifies X-Hub-Signature-256 and processes GitHub ping/push events.
func (e *Engine) HandleGitHubWebhook(ctx context.Context, eventType, sig256 string, body []byte) (int, string) {
	secret := e.cfg.GitHub.WebhookSecret
	if secret == "" || !verifyGitHubSignature(secret, sig256, body) {
		return http.StatusUnauthorized, "bad signature"
	}

	switch eventType {
	case "ping":
		return http.StatusOK, "pong"
	case "push":
		// handled below
	default:
		return http.StatusNoContent, ""
	}

	var payload struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Deleted    bool   `json:"deleted"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return http.StatusBadRequest, "invalid json"
	}

	if payload.Deleted || !strings.HasPrefix(payload.Ref, "refs/heads/") || payload.After == "" || strings.Trim(payload.After, "0") == "" {
		return http.StatusAccepted, "skipped"
	}
	branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
	repo := payload.Repository.FullName

	var matched []state.Source
	for _, src := range e.store.Sources() {
		if src.Builder == "cnb" && src.Repo == repo && src.Branch == branch {
			matched = append(matched, src)
		}
	}
	if len(matched) == 0 || e.cfg.CNB.Repo == "" {
		return http.StatusAccepted, "skipped"
	}

	queued := 0
	for _, src := range matched {
		if _, err := e.buildQueue.enqueue(ctx, "github", src, payload.After); err == nil {
			queued++
		}
	}
	if queued == 0 {
		return http.StatusInternalServerError, "failed to enqueue build"
	}
	return http.StatusAccepted, "queued"
}

func verifyGitHubSignature(secret, sigHeader string, body []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(sigHeader, prefix) {
		return false
	}
	gotSig, err := hex.DecodeString(strings.TrimPrefix(sigHeader, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	wantSig := mac.Sum(nil)
	return hmac.Equal(gotSig, wantSig)
}

// TriggerBuild resolves the head commit SHA of a source's branch on GitHub and enqueues a CNB build.
func (e *Engine) TriggerBuild(ctx context.Context, actor, sourceID string) (state.Event, error) {
	src, ok := e.store.GetSource(sourceID)
	if !ok {
		return state.Event{}, state.ErrNotFound
	}
	if src.Builder != "cnb" {
		return state.Event{}, fmt.Errorf("%w: source %q uses external builder", ErrBadInput, src.Repo)
	}
	if e.cfg.CNB.Repo == "" {
		return state.Event{}, ErrNotConfigured
	}

	sha, err := e.resolveGitHubBranchSHA(ctx, src.Repo, src.Branch)
	if err != nil {
		return state.Event{}, err
	}
	return e.buildQueue.enqueue(ctx, actor, src, sha)
}

func (e *Engine) resolveGitHubBranchSHA(ctx context.Context, repo, branch string) (string, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return "", fmt.Errorf("%w: invalid repo %q", ErrBadInput, repo)
	}
	u := fmt.Sprintf("%s/repos/%s/%s/commits/%s",
		strings.TrimRight(e.cfg.GitHub.API, "/"),
		url.PathEscape(parts[0]),
		url.PathEscape(parts[1]),
		url.PathEscape(branch),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := e.cfg.GitHub.TokenForOwner(parts[0]); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("github commit lookup: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("github commit lookup %d: %s", resp.StatusCode, e.scrubSecrets(strings.TrimSpace(string(data))))
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(data, &out); err != nil || !validGitSHA.MatchString(out.SHA) {
		return "", fmt.Errorf("github commit lookup returned invalid sha")
	}
	return out.SHA, nil
}

// GetWebhookStatus checks whether a source repository has an active GitHub push webhook pointing at Ferry.
func (e *Engine) GetWebhookStatus(ctx context.Context, sourceID string) (WebhookStatus, error) {
	src, ok := e.store.GetSource(sourceID)
	if !ok {
		return WebhookStatus{}, state.ErrNotFound
	}
	pub := strings.TrimRight(e.cfg.Auth.OIDC.PublicURL, "/")
	wantURL := ""
	if pub != "" {
		wantURL = pub + "/webhook/github"
	}
	parts := strings.Split(src.Repo, "/")
	if len(parts) != 2 {
		return WebhookStatus{}, fmt.Errorf("%w: invalid repo %q", ErrBadInput, src.Repo)
	}
	tok := e.cfg.GitHub.TokenForOwner(parts[0])
	if tok == "" {
		return WebhookStatus{
			Status:     "no-permission",
			WebhookURL: wantURL,
			Message:    fmt.Sprintf("github.tokens[%q] is not configured", parts[0]),
		}, nil
	}
	u := fmt.Sprintf("%s/repos/%s/%s/hooks",
		strings.TrimRight(e.cfg.GitHub.API, "/"),
		url.PathEscape(parts[0]),
		url.PathEscape(parts[1]),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return WebhookStatus{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+tok)

	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return WebhookStatus{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return WebhookStatus{
			Status:     "no-permission",
			WebhookURL: wantURL,
		}, nil
	}
	if resp.StatusCode/100 != 2 {
		return WebhookStatus{}, fmt.Errorf("github hooks %d: %s", resp.StatusCode, e.scrubSecrets(strings.TrimSpace(string(data))))
	}

	var hooks []struct {
		ID     int64    `json:"id"`
		Active bool     `json:"active"`
		Events []string `json:"events"`
		Config struct {
			URL string `json:"url"`
		} `json:"config"`
	}
	if err := json.Unmarshal(data, &hooks); err != nil {
		return WebhookStatus{}, err
	}

	for _, h := range hooks {
		if !h.Active {
			continue
		}
		hasPush := false
		for _, ev := range h.Events {
			if ev == "push" || ev == "*" {
				hasPush = true
				break
			}
		}
		if !hasPush {
			continue
		}
		hookURL := strings.TrimSpace(h.Config.URL)
		if matchesWebhookTarget(hookURL, pub) {
			return WebhookStatus{
				Status:     "ok",
				WebhookURL: hookURL,
				HookID:     h.ID,
			}, nil
		}
	}

	return WebhookStatus{
		Status:     "missing",
		WebhookURL: wantURL,
	}, nil
}

func matchesWebhookTarget(hookURL, pub string) bool {
	if hookURL == "" {
		return false
	}
	trimmed := strings.TrimRight(hookURL, "/")
	if pub != "" {
		return trimmed == pub+"/webhook/github" || trimmed == pub
	}
	return strings.HasSuffix(trimmed, "/webhook/github")
}

// SetupWebhook creates a push webhook on GitHub pointing at <publicURL>/webhook/github.
func (e *Engine) SetupWebhook(ctx context.Context, sourceID, publicURL string) (WebhookStatus, error) {
	src, ok := e.store.GetSource(sourceID)
	if !ok {
		return WebhookStatus{}, state.ErrNotFound
	}
	pub := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if pub == "" {
		pub = strings.TrimRight(e.cfg.Auth.OIDC.PublicURL, "/")
	}
	if pub == "" {
		return WebhookStatus{}, fmt.Errorf("%w: publicURL is required to register a webhook", ErrBadInput)
	}
	wantURL := pub + "/webhook/github"

	if e.cfg.GitHub.WebhookSecret == "" {
		return WebhookStatus{}, ErrNotConfigured
	}
	parts := strings.Split(src.Repo, "/")
	if len(parts) != 2 {
		return WebhookStatus{}, fmt.Errorf("%w: invalid repo %q", ErrBadInput, src.Repo)
	}
	tok := e.cfg.GitHub.TokenForOwner(parts[0])
	if tok == "" {
		return WebhookStatus{
			Status:     "no-permission",
			WebhookURL: wantURL,
			Message:    fmt.Sprintf("github.tokens[%q] is not configured", parts[0]),
		}, nil
	}

	// Idempotent check first.
	if cur, err := e.GetWebhookStatus(ctx, sourceID); err == nil && cur.Status == "ok" {
		return cur, nil
	}

	u := fmt.Sprintf("%s/repos/%s/%s/hooks",
		strings.TrimRight(e.cfg.GitHub.API, "/"),
		url.PathEscape(parts[0]),
		url.PathEscape(parts[1]),
	)
	body, err := json.Marshal(map[string]any{
		"name":   "web",
		"active": true,
		"events": []string{"push"},
		"config": map[string]string{
			"url":          wantURL,
			"content_type": "json",
			"secret":       e.cfg.GitHub.WebhookSecret,
			"insecure_ssl": "0",
		},
	})
	if err != nil {
		return WebhookStatus{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return WebhookStatus{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return WebhookStatus{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity {
		return WebhookStatus{
			Status:     "no-permission",
			WebhookURL: wantURL,
		}, nil
	}
	if resp.StatusCode/100 != 2 {
		return WebhookStatus{}, fmt.Errorf("github create hook %d: %s", resp.StatusCode, e.scrubSecrets(strings.TrimSpace(string(data))))
	}

	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(data, &created)
	return WebhookStatus{
		Status:     "ok",
		WebhookURL: wantURL,
		HookID:     created.ID,
	}, nil
}
