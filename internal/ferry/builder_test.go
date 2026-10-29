package ferry

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/StarMoe-org/ferry/internal/kube"
	"github.com/StarMoe-org/ferry/internal/kube/fake"
	"github.com/StarMoe-org/ferry/internal/state"
)

func startGitSmartHTTPServer(t *testing.T, rootDir string, checkAuth func(r *http.Request) bool) *httptest.Server {
	t.Helper()
	execPathOut, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	backendBin := filepath.Join(strings.TrimSpace(string(execPathOut)), "git-http-backend")
	if _, err := os.Stat(backendBin); err != nil {
		t.Skipf("git-http-backend not available at %s: %v", backendBin, err)
	}

	handler := &cgi.Handler{
		Path: backendBin,
		Env: []string{
			"GIT_PROJECT_ROOT=" + rootDir,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.User != nil {
			t.Errorf("credentials leaked into git URL: %s", r.URL.String())
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if checkAuth != nil && !checkAuth(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func signPayload(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookMirrorCNBBuildTriggerAndHooks(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	// 1. Create bare source GitHub repo (/org/web.git) with a real commit, and bare CNB repo (/org/builder.git).
	gitRoot := filepath.Join(tmp, "git-srv")
	ghBare := filepath.Join(gitRoot, "org", "web.git")
	cnbBare := filepath.Join(gitRoot, "org", "builder.git")
	if err := os.MkdirAll(ghBare, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cnbBare, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ghBare, "init", "--bare", "-b", "main")
	mustGit(t, cnbBare, "init", "--bare", "-b", "main")
	mustGit(t, cnbBare, "config", "http.receivepack", "true")

	seedWork := filepath.Join(tmp, "seed-src")
	if err := os.MkdirAll(seedWork, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, seedWork, "init", "-b", "main")
	mustGit(t, seedWork, "config", "user.name", "Test")
	mustGit(t, seedWork, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seedWork, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, seedWork, "add", "Dockerfile")
	mustGit(t, seedWork, "commit", "-m", "initial")
	headSHA := mustGit(t, seedWork, "rev-parse", "HEAD")
	mustGit(t, seedWork, "push", ghBare, "HEAD:refs/heads/main")

	gitHTTP := startGitSmartHTTPServer(t, gitRoot, func(r *http.Request) bool {
		u, p, ok := r.BasicAuth()
		if !ok {
			return false
		}
		if strings.HasPrefix(r.URL.Path, "/org/web.git") {
			return u == "x-access-token" && p == "gh-tok-123"
		}
		if strings.HasPrefix(r.URL.Path, "/org/builder.git") {
			return u == "cnb" && p == "cnb-tok-456"
		}
		return false
	})

	// 2. Fake CNB API server capturing POST /org/builder/-/build/start.
	var cnbMu sync.Mutex
	var cnbStartBody map[string]any
	cnbAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/org/builder/-/build/start" || r.Header.Get("Authorization") != "Bearer cnb-tok-456" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		cnbMu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&cnbStartBody)
		cnbMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":     true,
			"buildLogUrl": "https://cnb.example/org/builder/-/build/logs/b99",
		})
	}))
	defer cnbAPI.Close()

	// 3. Fake GitHub REST API server for commit SHA lookup + hooks management.
	var ghHooksMu sync.Mutex
	var ghHooks []map[string]any
	ghAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gh-tok-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/web/commits/main":
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": headSHA})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/web/hooks":
			ghHooksMu.Lock()
			defer ghHooksMu.Unlock()
			_ = json.NewEncoder(w).Encode(ghHooks)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/org/web/hooks":
			var reqBody struct {
				Events []string          `json:"events"`
				Config map[string]string `json:"config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&reqBody)
			ghHooksMu.Lock()
			hook := map[string]any{
				"id":     int64(77),
				"active": true,
				"events": reqBody.Events,
				"config": map[string]any{"url": reqBody.Config["url"]},
			}
			ghHooks = append(ghHooks, hook)
			ghHooksMu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hook)
		case r.URL.Path == "/repos/org/forbidden/hooks":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ghAPI.Close()

	pipeFile := filepath.Join(tmp, "pipeline.yml")
	if err := os.WriteFile(pipeFile, []byte("__BRANCH__:\n  push:\n    - stages:\n        - name: build\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	kSrv := fake.New()
	defer kSrv.Close()
	kCl, _ := kSrv.Client("de")
	st, _ := state.NewFileStore(ctx, filepath.Join(tmp, "state.json"))

	cfg := &Config{
		WorkDir:    filepath.Join(tmp, "work"),
		Namespaces: []string{"app"},
		Auth: AuthConfig{
			Disabled: true,
			OIDC:     OIDCConfig{PublicURL: "https://ferry.example.com"},
		},
		CNB: CNBConfig{
			Repo:         "org/builder",
			Token:        "cnb-tok-456",
			ImagePrefix:  "docker.cnb.cool/org/builder",
			PipelineFile: pipeFile,
			BranchPrefix: "src/",
			API:          cnbAPI.URL,
			Git:          gitHTTP.URL,
		},
		GitHub: GitHubConfig{
			Token:         "gh-tok-123",
			WebhookSecret: "wh-sec-789",
			API:           ghAPI.URL,
			Git:           gitHTTP.URL,
		},
	}

	eng, err := New(cfg, []*kube.Client{kCl}, st)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	src, err := st.CreateSource(ctx, state.Source{
		Repo:      "org/web",
		Branch:    "main",
		Builder:   "cnb",
		Image:     "web",
		BuildArgs: "TARGET=prod",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Test webhook HMAC validation, ping, non-push, deleted branch, and matching push.
	if code, body := eng.HandleGitHubWebhook(ctx, "ping", "sha256=deadbeef", []byte(`{}`)); code != http.StatusUnauthorized || body != "bad signature" {
		t.Fatalf("bad sig = %d %q", code, body)
	}
	pingBody := []byte(`{"zen":"keep it simple"}`)
	if code, body := eng.HandleGitHubWebhook(ctx, "ping", signPayload("wh-sec-789", pingBody), pingBody); code != http.StatusOK || body != "pong" {
		t.Fatalf("ping = %d %q", code, body)
	}
	issueBody := []byte(`{"action":"opened"}`)
	if code, _ := eng.HandleGitHubWebhook(ctx, "issues", signPayload("wh-sec-789", issueBody), issueBody); code != http.StatusNoContent {
		t.Fatalf("issues = %d, want 204", code)
	}
	delPush := []byte(`{"ref":"refs/heads/main","after":"` + headSHA + `","deleted":true,"repository":{"full_name":"org/web"}}`)
	if code, body := eng.HandleGitHubWebhook(ctx, "push", signPayload("wh-sec-789", delPush), delPush); code != http.StatusAccepted || body != "skipped" {
		t.Fatalf("deleted push = %d %q", code, body)
	}

	pushBody := []byte(`{"ref":"refs/heads/main","after":"` + headSHA + `","deleted":false,"repository":{"full_name":"org/web"}}`)
	if code, body := eng.HandleGitHubWebhook(ctx, "push", signPayload("wh-sec-789", pushBody), pushBody); code != http.StatusAccepted || body != "queued" {
		t.Fatalf("valid push = %d %q", code, body)
	}

	// Wait for background worker to mirror git commit and start CNB build.
	deadline := time.Now().Add(10 * time.Second)
	var startedEv state.Event
	for time.Now().Before(deadline) {
		evs := st.Events(state.EventsFilter{Kind: "build"})
		if len(evs) > 0 && (evs[0].Status == "started" || evs[0].Status == "error") {
			startedEv = evs[0]
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if startedEv.Status != "started" || startedEv.LogURL != "https://cnb.example/org/builder/-/build/logs/b99" {
		t.Fatalf("build event did not reach started: %+v", startedEv)
	}

	// Verify CNB start_build payload (Compatibility #7) and orphan branch pushed to CNB bare repo!
	cnbMu.Lock()
	gotBranch, _ := cnbStartBody["branch"].(string)
	gotConfig, _ := cnbStartBody["config"].(string)
	gotEnv, _ := cnbStartBody["env"].(map[string]any)
	cnbMu.Unlock()
	if gotBranch != "src/org--web" || !strings.HasPrefix(gotConfig, "src/org--web:\n") {
		t.Fatalf("unexpected CNB start payload: branch=%q config=%q", gotBranch, gotConfig)
	}
	if gotEnv["IMAGE_NAME"] != "web" || gotEnv["SOURCE_REPO"] != "org/web" || gotEnv["SOURCE_SHA"] != headSHA || gotEnv["BUILD_ARGS"] != "TARGET=prod" {
		t.Fatalf("unexpected CNB env: %+v", gotEnv)
	}
	pushedSHA := mustGit(t, cnbBare, "rev-parse", "refs/heads/src/org--web")
	if pushedSHA == "" || pushedSHA == headSHA {
		// Orphan commit-tree has same tree as headSHA, distinct parentless commit!
		treeOrig := mustGit(t, ghBare, "rev-parse", headSHA+"^{tree}")
		treeMirrored := mustGit(t, cnbBare, "rev-parse", pushedSHA+"^{tree}")
		if treeOrig != treeMirrored {
			t.Fatalf("mirrored tree %s != original tree %s", treeMirrored, treeOrig)
		}
	}

	// Manual TriggerBuild via GitHub commits API.
	manualEv, err := eng.TriggerBuild(ctx, "alice@example.com", src.ID)
	if err != nil || manualEv.Commit != headSHA {
		t.Fatalf("TriggerBuild = %+v, %v", manualEv, err)
	}

	// Webhook status: missing -> setup -> ok; forbidden repo -> no-permission (never leaking secret!).
	wh1, err := eng.GetWebhookStatus(ctx, src.ID)
	if err != nil || wh1.Status != "missing" {
		t.Fatalf("GetWebhookStatus initial = %+v, %v", wh1, err)
	}
	wh2, err := eng.SetupWebhook(ctx, src.ID, "https://ferry.example.com")
	if err != nil || wh2.Status != "ok" || wh2.HookID != 77 {
		t.Fatalf("SetupWebhook = %+v, %v", wh2, err)
	}
	rawWH, _ := json.Marshal(wh2)
	if strings.Contains(string(rawWH), "wh-sec-789") {
		t.Fatalf("webhook secret leaked in response: %s", rawWH)
	}

	forbSrc, _ := st.CreateSource(ctx, state.Source{Repo: "org/forbidden", Branch: "main", Builder: "cnb", Image: "forb"})
	whForb, err := eng.GetWebhookStatus(ctx, forbSrc.ID)
	if err != nil || whForb.Status != "no-permission" {
		t.Fatalf("expected no-permission for 403 repo, got %+v, %v", whForb, err)
	}
}
