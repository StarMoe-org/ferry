package ferry

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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

func TestWebhookMirrorCNBBuildTriggerMultiOwnerTokensAndScrub(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	// 1. Create bare source GitHub repos for TWO distinct owners (/org-a/web-repo.git and /org-b/worker-repo.git)
	// plus bare CNB mirror repo (/org/builder.git).
	gitRoot := filepath.Join(tmp, "git-srv")
	ghBareA := filepath.Join(gitRoot, "org-a", "web-repo.git")
	ghBareB := filepath.Join(gitRoot, "org-b", "worker-repo.git")
	cnbBare := filepath.Join(gitRoot, "org", "builder.git")
	for _, d := range []string{ghBareA, ghBareB, cnbBare} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		mustGit(t, d, "init", "--bare", "-b", "main")
	}
	mustGit(t, cnbBare, "config", "http.receivepack", "true")

	seedRepo := func(name, bare string) string {
		seedWork := filepath.Join(tmp, "seed-"+name)
		if err := os.MkdirAll(seedWork, 0o755); err != nil {
			t.Fatal(err)
		}
		mustGit(t, seedWork, "init", "-b", "main")
		mustGit(t, seedWork, "config", "user.name", "Test")
		mustGit(t, seedWork, "config", "user.email", "test@example.com")
		if err := os.WriteFile(filepath.Join(seedWork, "Dockerfile"), []byte("FROM scratch # "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, seedWork, "add", "Dockerfile")
		mustGit(t, seedWork, "commit", "-m", "initial "+name)
		sha := mustGit(t, seedWork, "rev-parse", "HEAD")
		mustGit(t, seedWork, "push", bare, "HEAD:refs/heads/main")
		return sha
	}
	headSHAA := seedRepo("a", ghBareA)
	headSHAB := seedRepo("b", ghBareB)

	// Git HTTP server strictly requires owner-specific token per GitHub owner!
	gitHTTP := startGitSmartHTTPServer(t, gitRoot, func(r *http.Request) bool {
		u, p, ok := r.BasicAuth()
		if !ok {
			return false
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/org-a/"):
			return u == "x-access-token" && p == "gh-tok-owner-a"
		case strings.HasPrefix(r.URL.Path, "/org-b/"):
			return u == "x-access-token" && p == "gh-tok-owner-b"
		case strings.HasPrefix(r.URL.Path, "/org/builder.git"):
			return u == "cnb" && p == "cnb-tok-456"
		default:
			return false
		}
	})

	// 2. Fake CNB API server capturing POST /org/builder/-/build/start calls by branch.
	var cnbMu sync.Mutex
	cnbStartByBranch := map[string]map[string]any{}
	cnbAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/org/builder/-/build/start" || r.Header.Get("Authorization") != "Bearer cnb-tok-456" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		br, _ := body["branch"].(string)
		cnbMu.Lock()
		cnbStartByBranch[br] = body
		cnbMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":     true,
			"buildLogUrl": "https://cnb.example/org/builder/-/build/logs/" + strings.ReplaceAll(br, "/", "-"),
		})
	}))
	defer cnbAPI.Close()

	// 3. Fake GitHub REST API server strictly verifying owner-specific token on commits + hooks!
	var ghHooksMu sync.Mutex
	ghHooksByRepo := map[string][]map[string]any{}
	ghAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/org-a/"):
			if auth != "Bearer gh-tok-owner-a" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		case strings.HasPrefix(r.URL.Path, "/repos/org-b/"):
			if auth != "Bearer gh-tok-owner-b" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		default:
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org-a/web-repo/commits/main":
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": headSHAA})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org-b/worker-repo/commits/main":
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": headSHAB})
		case r.Method == http.MethodGet && (r.URL.Path == "/repos/org-a/web-repo/hooks" || r.URL.Path == "/repos/org-b/worker-repo/hooks"):
			ghHooksMu.Lock()
			defer ghHooksMu.Unlock()
			_ = json.NewEncoder(w).Encode(ghHooksByRepo[r.URL.Path])
		case r.Method == http.MethodPost && (r.URL.Path == "/repos/org-a/web-repo/hooks" || r.URL.Path == "/repos/org-b/worker-repo/hooks"):
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
			ghHooksByRepo[r.URL.Path] = append(ghHooksByRepo[r.URL.Path], hook)
			ghHooksMu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hook)
		case r.URL.Path == "/repos/org-a/forbidden/hooks":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ghAPI.Close()

	pipeFile := filepath.Join(tmp, "pipeline.yml")
	if err := os.WriteFile(pipeFile, []byte("\"__BRANCH__\":\n  api_trigger_build:\n    - stages:\n        - name: build\n"), 0o644); err != nil {
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
			Tokens: map[string]string{
				"org-a": "gh-tok-owner-a",
				"org-b": "gh-tok-owner-b",
			},
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

	// scrubSecrets must redact ALL configured owner tokens and their base64 basic-auth forms!
	b64A := base64.StdEncoding.EncodeToString([]byte("x-access-token:gh-tok-owner-a"))
	b64B := base64.StdEncoding.EncodeToString([]byte("x-access-token:gh-tok-owner-b"))
	scrubbed := eng.scrubSecrets("leak1=gh-tok-owner-a leak2=gh-tok-owner-b b64a=" + b64A + " b64b=" + b64B + " cnb=cnb-tok-456")
	for _, secret := range []string{"gh-tok-owner-a", "gh-tok-owner-b", b64A, b64B, "cnb-tok-456"} {
		if strings.Contains(scrubbed, secret) {
			t.Fatalf("scrubSecrets leaked %q in %q", secret, scrubbed)
		}
	}

	// Source A: non-empty BuildArgs, Image "web-portal" (deliberately distinct from repo name "web-repo" to verify branchPrefix + image!)
	srcA, err := st.CreateSource(ctx, state.Source{
		Repo:      "org-a/web-repo",
		Branch:    "main",
		Builder:   "cnb",
		Image:     "web-portal",
		BuildArgs: "TARGET=prod",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Source B: EMPTY BuildArgs ("") on second GitHub owner "org-b" to verify BUILD_ARGS="" is still passed!
	srcB, err := st.CreateSource(ctx, state.Source{
		Repo:      "org-b/worker-repo",
		Branch:    "main",
		Builder:   "cnb",
		Image:     "bg-worker",
		BuildArgs: "",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Test webhook HMAC validation, ping, non-push, deleted branch, and matching push for Source A.
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
	delPush := []byte(`{"ref":"refs/heads/main","after":"` + headSHAA + `","deleted":true,"repository":{"full_name":"org-a/web-repo"}}`)
	if code, body := eng.HandleGitHubWebhook(ctx, "push", signPayload("wh-sec-789", delPush), delPush); code != http.StatusAccepted || body != "skipped" {
		t.Fatalf("deleted push = %d %q", code, body)
	}

	pushBody := []byte(`{"ref":"refs/heads/main","after":"` + headSHAA + `","deleted":false,"repository":{"full_name":"org-a/web-repo"}}`)
	if code, body := eng.HandleGitHubWebhook(ctx, "push", signPayload("wh-sec-789", pushBody), pushBody); code != http.StatusAccepted || body != "queued" {
		t.Fatalf("valid push = %d %q", code, body)
	}

	// Trigger Source B via TriggerBuild (exercises owner-b commit lookup + empty BUILD_ARGS!).
	manualEvB, err := eng.TriggerBuild(ctx, "alice@example.com", srcB.ID)
	if err != nil || manualEvB.Commit != headSHAB {
		t.Fatalf("TriggerBuild srcB = %+v, %v", manualEvB, err)
	}

	// Wait for BOTH builds (src/web-portal and src/bg-worker) to reach started.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		evs := st.Events(state.EventsFilter{Kind: "build"})
		done := 0
		for _, ev := range evs {
			if ev.Status == "started" || ev.Status == "error" {
				done++
			}
		}
		if done >= 2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	for _, ev := range st.Events(state.EventsFilter{Kind: "build"}) {
		if ev.Status != "started" {
			t.Fatalf("build event failed: %+v", ev)
		}
	}

	// Verify CNB start_build payload for Source A:
	// - branch = "src/web-portal" (branchPrefix + image)
	// - event = "api_trigger_build"
	// - env = {IMAGE_NAME, SRC_REPO, SRC_SHA, BUILD_ARGS}
	// - commit-tree message = "<repo>@<sha>"
	cnbMu.Lock()
	bodyA := cnbStartByBranch["src/web-portal"]
	bodyB := cnbStartByBranch["src/bg-worker"]
	cnbMu.Unlock()

	if bodyA == nil || bodyB == nil {
		t.Fatalf("missing CNB start calls: A=%+v B=%+v", bodyA, bodyB)
	}
	if bodyA["event"] != "api_trigger_build" {
		t.Fatalf("expected event=api_trigger_build on A, got %+v", bodyA["event"])
	}
	cfgA, _ := bodyA["config"].(string)
	if !strings.HasPrefix(cfgA, `"src/web-portal":`) {
		t.Fatalf("unexpected rendered config on A: %q", cfgA)
	}
	envA, _ := bodyA["env"].(map[string]any)
	if envA["IMAGE_NAME"] != "web-portal" || envA["SRC_REPO"] != "org-a/web-repo" || envA["SRC_SHA"] != headSHAA || envA["BUILD_ARGS"] != "TARGET=prod" {
		t.Fatalf("unexpected CNB env on A: %+v", envA)
	}
	if _, bad := envA["SOURCE_REPO"]; bad {
		t.Fatalf("env must not use SOURCE_REPO: %+v", envA)
	}

	// Verify Source B passes BUILD_ARGS="" explicitly AND event="api_trigger_build"!
	if bodyB["event"] != "api_trigger_build" {
		t.Fatalf("expected event=api_trigger_build on B, got %+v", bodyB["event"])
	}
	envB, _ := bodyB["env"].(map[string]any)
	buildArgsVal, hasBuildArgs := envB["BUILD_ARGS"]
	if !hasBuildArgs || buildArgsVal != "" {
		t.Fatalf("expected BUILD_ARGS=\"\" to be present even when empty, got %+v", envB)
	}
	if envB["IMAGE_NAME"] != "bg-worker" || envB["SRC_REPO"] != "org-b/worker-repo" || envB["SRC_SHA"] != headSHAB {
		t.Fatalf("unexpected CNB env on B: %+v", envB)
	}

	// Verify commit-tree message is "<repo>@<sha>" and tree matches original commit!
	pushedSHAA := mustGit(t, cnbBare, "rev-parse", "refs/heads/src/web-portal")
	commitMsgA := mustGit(t, cnbBare, "log", "-1", "--format=%B", pushedSHAA)
	if commitMsgA != "org-a/web-repo@"+headSHAA {
		t.Fatalf("commit-tree message = %q, want %q", commitMsgA, "org-a/web-repo@"+headSHAA)
	}
	treeOrig := mustGit(t, ghBareA, "rev-parse", headSHAA+"^{tree}")
	treeMirrored := mustGit(t, cnbBare, "rev-parse", pushedSHAA+"^{tree}")
	if treeOrig != treeMirrored {
		t.Fatalf("mirrored tree %s != original tree %s", treeMirrored, treeOrig)
	}

	// Verify hooks management uses owner-specific tokens (org-a, org-b, and unconfigured org-c).
	whA1, err := eng.GetWebhookStatus(ctx, srcA.ID)
	if err != nil || whA1.Status != "missing" {
		t.Fatalf("GetWebhookStatus A initial = %+v, %v", whA1, err)
	}
	whA2, err := eng.SetupWebhook(ctx, srcA.ID, "https://ferry.example.com")
	if err != nil || whA2.Status != "ok" || whA2.HookID != 77 {
		t.Fatalf("SetupWebhook A = %+v, %v", whA2, err)
	}
	whB2, err := eng.SetupWebhook(ctx, srcB.ID, "https://ferry.example.com")
	if err != nil || whB2.Status != "ok" {
		t.Fatalf("SetupWebhook B (owner org-b) = %+v, %v", whB2, err)
	}
	rawWH, _ := json.Marshal(whA2)
	if strings.Contains(string(rawWH), "wh-sec-789") {
		t.Fatalf("webhook secret leaked in response: %s", rawWH)
	}

	unconfSrc, _ := st.CreateSource(ctx, state.Source{Repo: "org-c/other", Branch: "main", Builder: "cnb", Image: "other"})
	whUnconf, err := eng.GetWebhookStatus(ctx, unconfSrc.ID)
	if err != nil || whUnconf.Status != "no-permission" {
		t.Fatalf("expected no-permission when owner token missing, got %+v, %v", whUnconf, err)
	}

	forbSrc, _ := st.CreateSource(ctx, state.Source{Repo: "org-a/forbidden", Branch: "main", Builder: "cnb", Image: "forb"})
	whForb, err := eng.GetWebhookStatus(ctx, forbSrc.ID)
	if err != nil || whForb.Status != "no-permission" {
		t.Fatalf("expected no-permission for 403 repo, got %+v, %v", whForb, err)
	}
}
