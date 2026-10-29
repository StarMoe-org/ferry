package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/StarMoe-org/ferry/internal/ferry"
	"github.com/StarMoe-org/ferry/internal/state"
)

const gitReadCacheTTL = 10 * time.Minute

type ciAuthenticator struct {
	store   *state.Store
	targets []ferry.GitReadTarget
	hc      *http.Client

	mu    sync.Mutex
	cache map[string]time.Time
}

func newCIAuthenticator(store *state.Store, targets []ferry.GitReadTarget, hc *http.Client) *ciAuthenticator {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &ciAuthenticator{
		store:   store,
		targets: targets,
		hc:      hc,
		cache:   make(map[string]time.Time),
	}
}

func extractBearer(r *http.Request) (string, bool) {
	hdr := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(hdr) < 8 || !strings.EqualFold(hdr[:7], "Bearer ") {
		return "", false
	}
	tok := strings.TrimSpace(hdr[7:])
	if tok == "" {
		return "", false
	}
	return tok, true
}

func (a *ciAuthenticator) authenticate(ctx context.Context, r *http.Request) (actor string, ok bool, errMsg string) {
	tok, has := extractBearer(r)
	if !has {
		return "", false, a.unauthorizedMessage()
	}

	// 1. Check Ferry deploy tokens first (constant-time SHA-256 hex compare).
	if info, matched := a.store.AuthenticateToken(ctx, tok); matched {
		return "token:" + info.Name, true, ""
	}

	// 2. Fall back to configured gitRead endpoints, cached 10 minutes by token SHA-256.
	if len(a.targets) == 0 {
		return "", false, a.unauthorizedMessage()
	}

	sum := sha256.Sum256([]byte(tok))
	cacheKey := hex.EncodeToString(sum[:])
	now := time.Now()

	a.mu.Lock()
	if exp, exists := a.cache[cacheKey]; exists {
		if now.Before(exp) {
			a.mu.Unlock()
			return "ci", true, ""
		}
		delete(a.cache, cacheKey)
	}
	a.mu.Unlock()

	for _, target := range a.targets {
		if a.probeGitRead(ctx, target, tok) {
			a.mu.Lock()
			a.cache[cacheKey] = time.Now().Add(gitReadCacheTTL)
			a.mu.Unlock()
			return "ci", true, ""
		}
	}
	return "", false, a.unauthorizedMessage()
}

func (a *ciAuthenticator) probeGitRead(ctx context.Context, target ferry.GitReadTarget, token string) bool {
	u := strings.TrimRight(target.URL, "/") + "/info/refs?service=git-upload-pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	req.SetBasicAuth(target.Username, token)
	req.Header.Set("User-Agent", "git/2.43")
	resp, err := a.hc.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (a *ciAuthenticator) unauthorizedMessage() string {
	if len(a.targets) > 0 {
		urls := make([]string, len(a.targets))
		for i, t := range a.targets {
			urls[i] = t.URL
		}
		return fmt.Sprintf("needs a deploy token or a token that can read %s", strings.Join(urls, " or "))
	}
	return "needs a deploy token"
}
