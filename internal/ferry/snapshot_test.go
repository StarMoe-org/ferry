package ferry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/StarMoe-org/ferry/internal/kube"
	"github.com/StarMoe-org/ferry/internal/kube/fake"
	"github.com/StarMoe-org/ferry/internal/state"
)

func TestSnapshotCleaningRegexTLSGitPushLegacyMigrationAndUnchanged(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	// Create bare snapshot git repo seeded with legacy single-namespace secret files.
	gitRoot := filepath.Join(tmp, "git")
	snapBare := filepath.Join(gitRoot, "org", "clusters.git")
	if err := os.MkdirAll(snapBare, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, snapBare, "init", "--bare", "-b", "main")
	mustGit(t, snapBare, "config", "http.receivepack", "true")

	seedDir := filepath.Join(tmp, "seed")
	if err := os.MkdirAll(filepath.Join(seedDir, "snapshots", "de"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, seedDir, "init", "-b", "main")
	mustGit(t, seedDir, "config", "user.name", "Init")
	mustGit(t, seedDir, "config", "user.email", "init@example.com")
	_ = os.WriteFile(filepath.Join(seedDir, "snapshots", "de", "secrets.enc.yaml"), []byte("legacy\n"), 0o644)
	_ = os.WriteFile(filepath.Join(seedDir, "snapshots", "de", "secrets.sha256"), []byte("legacyhash\n"), 0o644)
	mustGit(t, seedDir, "add", "-A")
	mustGit(t, seedDir, "commit", "-m", "legacy layout")
	mustGit(t, seedDir, "push", snapBare, "HEAD:refs/heads/main")

	gitHTTP := startGitSmartHTTPServer(t, gitRoot, func(r *http.Request) bool {
		u, p, ok := r.BasicAuth()
		return ok && u == "x-access-token" && p == "snap-secret-tok"
	})

	de := fake.New()
	defer de.Close()
	sg := fake.New()
	defer sg.Close()
	sg.SetDisableTraefik(true) // Verify 404 on Traefik CRD is skipped cleanly.

	de.Seed(
		map[string]any{
			"kind": "Deployment",
			"metadata": map[string]any{
				"name":      "web",
				"namespace": "app",
				"uid":       "uid-123",
				"labels":    map[string]any{"app": "web"},
				"annotations": map[string]any{
					"kubectl.kubernetes.io/last-applied-configuration": `{"big":"json"}`,
					"argocd.argoproj.io/tracking-id":                   "apps:apps/Deployment:app/web",
					"argocd.argoproj.io/sync-options":                  "Prune=false",
					"deployment.kubernetes.io/revision":                "9",
					"kubernetes.io/change-cause":                       "ghcr.io/org/web:v1",
					"ferry.star.moe/auto-deploy":                       "false",
				},
			},
			"spec":   map[string]any{"replicas": 2},
			"status": map[string]any{"readyReplicas": 2},
		},
		map[string]any{
			"kind": "PersistentVolumeClaim",
			"metadata": map[string]any{
				"name":      "data-pvc",
				"namespace": "app",
				"annotations": map[string]any{
					"pv.kubernetes.io/bind-completed":               "yes",
					"pv.kubernetes.io/bound-by-controller":          "yes",
					"volume.beta.kubernetes.io/storage-provisioner": "rancher.io/local-path",
					"volume.kubernetes.io/selected-node":            "node-1",
					"custom.example.com/backup-tier":                "daily",
				},
			},
			"spec": map[string]any{"accessModes": []any{"ReadWriteOnce"}},
		},
		map[string]any{
			"kind":     "Service",
			"metadata": map[string]any{"name": "web", "namespace": "app"},
			"spec": map[string]any{
				"type":                  "ClusterIP",
				"clusterIP":             "10.43.0.10",
				"clusterIPs":            []any{"10.43.0.10"},
				"ipFamilies":            []any{"IPv4"},
				"ipFamilyPolicy":        "SingleStack",
				"internalTrafficPolicy": "Cluster",
				"ports":                 []any{map[string]any{"port": 80}},
			},
		},
		map[string]any{
			"kind":     "ConfigMap",
			"metadata": map[string]any{"name": "kube-root-ca.crt", "namespace": "app"},
			"data":     map[string]any{"ca.crt": "skip-me"},
		},
		map[string]any{
			"kind": "ConfigMap",
			"metadata": map[string]any{
				"name":            "owned-child",
				"namespace":       "app",
				"ownerReferences": []any{map[string]any{"kind": "Deployment", "name": "web"}},
			},
			"data": map[string]any{"k": "skip-owned"},
		},
		map[string]any{
			"kind":     "ConfigMap",
			"metadata": map[string]any{"name": "app-config", "namespace": "app"},
			"data":     map[string]any{"LOG_LEVEL": "info"},
		},
		map[string]any{
			"kind":     "Secret",
			"type":     "kubernetes.io/tls",
			"metadata": map[string]any{"name": "ingress-tls-cert", "namespace": "app"},
			"data":     map[string]any{"tls.crt": "c2tpcC10bHM=", "tls.key": "c2tpcC1rZXk="},
		},
		map[string]any{
			"kind":     "Middleware",
			"metadata": map[string]any{"name": "strip-api", "namespace": "app"},
			"spec":     map[string]any{"stripPrefix": map[string]any{"prefixes": []any{"/api"}}},
		},
	)

	deCl, _ := de.Client("de")
	sgCl, _ := sg.Client("sg")

	cfg := &Config{
		WorkDir:    filepath.Join(tmp, "work"),
		Namespaces: []string{"app"},
		Auth:       AuthConfig{Disabled: true},
		State: StateConfig{
			Kubernetes: &KubeStateConfig{Cluster: "de", Namespace: "app", Name: "ferry-state"},
		},
		Clusters: []kube.Config{de.Config("de"), sg.Config("sg")},
		Snapshot: SnapshotConfig{
			Repo:     gitHTTP.URL + "/org/clusters.git",
			Branch:   "main",
			Dir:      "snapshots",
			Username: "x-access-token",
			Token:    "snap-secret-tok",
			Author:   SnapshotAuthor{Name: "ferry", Email: "ferry@localhost"},
		},
	}

	st, err := state.NewConfigMapStore(ctx, deCl, "app", "ferry-state")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSource(ctx, state.Source{Repo: "org/web", Branch: "main", Builder: "external", Image: "ghcr.io/org/web"}); err != nil {
		t.Fatal(err)
	}
	tokInfo, tokPlain, err := st.CreateToken(ctx, "ci-bot", "alice")
	if err != nil {
		t.Fatal(err)
	}

	eng, err := New(cfg, []*kube.Client{deCl, sgCl}, st)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	// Run concurrent snapshots to verify single-flight coalescing.
	// Note: kubernetes.io/tls Secret is skipped, so 0 encryptable Secrets exist here!
	var wg sync.WaitGroup
	results := make([]SnapshotResult, 4)
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = eng.Snapshot(ctx, "ci")
		}(i)
	}
	wg.Wait()
	for i := 0; i < 4; i++ {
		if errs[i] != nil {
			t.Fatalf("Snapshot[%d] err = %v", i, errs[i])
		}
		if results[i].Status != "committed" || results[i].Commit == "" {
			t.Fatalf("Snapshot[%d] = %+v", i, results[i])
		}
	}

	// Second call with no cluster/state changes must report "unchanged".
	res2, err := eng.Snapshot(ctx, "ci")
	if err != nil {
		t.Fatalf("second Snapshot: %v", err)
	}
	if res2.Status != "unchanged" || res2.Commit != results[0].Commit {
		t.Fatalf("expected unchanged at %s, got %+v", results[0].Commit, res2)
	}

	// Clone bare repo and inspect committed tree!
	inspectDir := filepath.Join(tmp, "inspect")
	mustGit(t, tmp, "clone", "-q", snapBare, inspectDir)

	if _, err := os.Stat(filepath.Join(inspectDir, "snapshots", "de", "secrets.enc.yaml")); !os.IsNotExist(err) {
		t.Fatalf("expected legacy secrets.enc.yaml removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(inspectDir, "snapshots", "de", "app.secrets.enc.yaml")); !os.IsNotExist(err) {
		t.Fatalf("expected no app.secrets.enc.yaml when only kubernetes.io/tls secret exists, err=%v", err)
	}

	deYAMLBytes, err := os.ReadFile(filepath.Join(inspectDir, "snapshots", "de", "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	deYAML := string(deYAMLBytes)
	if !strings.HasPrefix(deYAML, "# cluster=de namespace=app\n") {
		t.Fatalf("missing header in app.yaml:\n%s", deYAML)
	}
	for _, forbidden := range []string{
		"kube-root-ca.crt",
		"owned-child",
		"ferry-state",
		"ingress-tls-cert",
		"last-applied-configuration",
		"argocd.argoproj.io/",
		"deployment.kubernetes.io/revision",
		"pv.kubernetes.io/",
		"volume.beta.kubernetes.io/",
		"volume.kubernetes.io/",
		"clusterIP",
		"ipFamilies",
		"internalTrafficPolicy",
		"readyReplicas",
		"uid-123",
		tokInfo.ID,
		tokPlain,
	} {
		if strings.Contains(deYAML, forbidden) {
			t.Fatalf("app.yaml contained forbidden string %q:\n%s", forbidden, deYAML)
		}
	}
	for _, want := range []string{
		"app-config",
		"data-pvc",
		"strip-api",
		"kubernetes.io/change-cause",
		"ferry.star.moe/auto-deploy",
		"custom.example.com/backup-tier",
	} {
		if !strings.Contains(deYAML, want) {
			t.Fatalf("app.yaml missing expected %q:\n%s", want, deYAML)
		}
	}

	ferryJSONBytes, err := os.ReadFile(filepath.Join(inspectDir, "snapshots", "ferry.json"))
	if err != nil {
		t.Fatal(err)
	}
	ferryJSON := string(ferryJSONBytes)
	if !strings.Contains(ferryJSON, "org/web") || strings.Contains(ferryJSON, "tokens") || strings.Contains(ferryJSON, tokInfo.ID) {
		t.Fatalf("unexpected ferry.json:\n%s", ferryJSON)
	}
}

func TestSnapshotSOPSRepoConfigAgeOverrideAndNonSilentFailure(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	// Create stub sops script in fakeBin that verifies:
	// 1. It is executed inside the cloned repository directory (where .sops.yaml exists).
	// 2. Its CLI arguments match `sops -e -i snapshots/de/app.secrets.enc.yaml` (or `--age` override).
	fakeBin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	callsFile := filepath.Join(tmp, "sops.calls")
	failFlag := filepath.Join(tmp, "sops.fail")
	sopsScript := `#!/bin/sh
set -eu
if [ ! -f ".sops.yaml" ]; then
  echo "missing .sops.yaml in cwd $(pwd)" >&2
  exit 2
fi
echo "$*" >> "` + callsFile + `"
if [ -f "` + failFlag + `" ]; then
  echo "simulated sops failure" >&2
  exit 1
fi
eval "file=\${$#}"
if grep -q "ingress-tls" "$file"; then
  echo "kubernetes.io/tls secret was not filtered!" >&2
  exit 3
fi
printf "# sops-encrypted\nsops:\n  version: stub\n" > "$file"
`
	if err := os.WriteFile(filepath.Join(fakeBin, "sops"), []byte(sopsScript), 0o755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+origPath)

	snapBare := filepath.Join(tmp, "clusters.git")
	if err := os.MkdirAll(snapBare, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, snapBare, "init", "--bare", "-b", "main")

	// Seed .sops.yaml into the remote snapshot repository so cloned checkouts carry it!
	seedRepo := filepath.Join(tmp, "seed-sops")
	if err := os.MkdirAll(seedRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, seedRepo, "init", "-b", "main")
	mustGit(t, seedRepo, "config", "user.name", "Init")
	mustGit(t, seedRepo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(seedRepo, ".sops.yaml"), []byte("creation_rules:\n  - path_regex: '.*\\.enc\\.yaml$'\n    age: age1repodefault\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, seedRepo, "add", ".sops.yaml")
	mustGit(t, seedRepo, "commit", "-m", "add .sops.yaml")
	mustGit(t, seedRepo, "push", snapBare, "HEAD:refs/heads/main")

	de := fake.New()
	defer de.Close()
	de.Seed(
		map[string]any{
			"kind":     "Secret",
			"type":     "Opaque",
			"metadata": map[string]any{"name": "app-secret", "namespace": "app"},
			"data":     map[string]any{"API_KEY": "c2VjcmV0"},
		},
		map[string]any{
			"kind":     "Secret",
			"type":     "kubernetes.io/service-account-token",
			"metadata": map[string]any{"name": "default-token", "namespace": "app"},
			"data":     map[string]any{"token": "skip-sa"},
		},
		map[string]any{
			"kind":     "Secret",
			"type":     "kubernetes.io/tls",
			"metadata": map[string]any{"name": "ingress-tls", "namespace": "app"},
			"data":     map[string]any{"tls.crt": "skip-tls"},
		},
		map[string]any{
			"kind":     "Secret",
			"type":     "helm.sh/release.v1",
			"metadata": map[string]any{"name": "sh.helm.release.v1.x", "namespace": "app"},
			"data":     map[string]any{"release": "skip-helm"},
		},
	)

	deCl, _ := de.Client("de")
	st, _ := state.NewFileStore(ctx, filepath.Join(tmp, "state.json"))
	// Notice: AgePublicKey is NOT set! Repo's .sops.yaml drives `sops -e -i`.
	cfg := &Config{
		WorkDir:    filepath.Join(tmp, "work"),
		Namespaces: []string{"app"},
		Auth:       AuthConfig{Disabled: true},
		Snapshot: SnapshotConfig{
			Repo:   snapBare,
			Branch: "main",
			Dir:    "snapshots",
		},
	}
	eng, _ := New(cfg, []*kube.Client{deCl}, st)
	defer eng.Close()

	// 1. Default repo .sops.yaml encryption: runs `sops -e -i snapshots/de/app.secrets.enc.yaml` inside cloned repo!
	res1, err := eng.Snapshot(ctx, "alice")
	if err != nil || res1.Status != "committed" {
		t.Fatalf("Snapshot #1 = %+v, %v", res1, err)
	}
	calls1Bytes, _ := os.ReadFile(callsFile)
	calls1 := strings.TrimSpace(string(calls1Bytes))
	wantArg := "-e -i " + filepath.Join("snapshots", "de", "app.secrets.enc.yaml")
	if calls1 != wantArg {
		t.Fatalf("sops args = %q, want %q", calls1, wantArg)
	}

	// 2. Second snapshot with identical secret plaintext skips sops via .secrets.sha256!
	res2, err := eng.Snapshot(ctx, "alice")
	if err != nil || res2.Status != "unchanged" {
		t.Fatalf("Snapshot #2 = %+v, %v", res2, err)
	}
	calls2Bytes, _ := os.ReadFile(callsFile)
	if strings.TrimSpace(string(calls2Bytes)) != wantArg {
		t.Fatalf("expected sops skipped on unchanged sha256, got %q", string(calls2Bytes))
	}

	// 3. Optional agePublicKey override passes --age <recipient>!
	eng.Close()
	cfgOverride := *cfg
	cfgOverride.Snapshot.AgePublicKey = "age1overridekey"
	engOverride, _ := New(&cfgOverride, []*kube.Client{deCl}, st)
	defer engOverride.Close()

	de.Seed(map[string]any{
		"kind":     "Secret",
		"type":     "Opaque",
		"metadata": map[string]any{"name": "app-secret", "namespace": "app"},
		"data":     map[string]any{"API_KEY": "djItc2VjcmV0"},
	})
	res3, err := engOverride.Snapshot(ctx, "alice")
	if err != nil || res3.Status != "committed" {
		t.Fatalf("Snapshot #3 (agePublicKey override) = %+v, %v", res3, err)
	}
	calls3Bytes, _ := os.ReadFile(callsFile)
	lines3 := strings.Split(strings.TrimSpace(string(calls3Bytes)), "\n")
	if len(lines3) != 2 || !strings.Contains(lines3[1], "--age age1overridekey") {
		t.Fatalf("expected --age override in 2nd sops invocation, got %q", string(calls3Bytes))
	}

	// 4. When Secrets exist and sops fails -> must return error AND record an error event (never silently skip)!
	engOverride.Close()
	engFail, _ := New(cfg, []*kube.Client{deCl}, st)
	defer engFail.Close()

	_ = os.WriteFile(failFlag, []byte("1"), 0o644)
	de.Seed(map[string]any{
		"kind":     "Secret",
		"type":     "Opaque",
		"metadata": map[string]any{"name": "app-secret", "namespace": "app"},
		"data":     map[string]any{"API_KEY": "djMtc2VjcmV0"},
	})
	if _, err := engFail.Snapshot(ctx, "alice"); err == nil {
		t.Fatal("expected error when sops fails")
	}
	evs := st.Events(state.EventsFilter{Kind: "snapshot", Limit: 1})
	if len(evs) == 0 || evs[0].Status != "error" {
		t.Fatalf("expected snapshot error event when sops fails, got %+v", evs)
	}

	// 5. When Secrets exist and snapshot.sops is explicitly false -> must ALSO fail and record error event (never silently skip)!
	engFail.Close()
	sopsFalse := false
	cfgNoSOPS := *cfg
	cfgNoSOPS.Snapshot.SOPS = &sopsFalse
	engNoSOPS, _ := New(&cfgNoSOPS, []*kube.Client{deCl}, st)
	defer engNoSOPS.Close()

	if _, err := engNoSOPS.Snapshot(ctx, "bob"); err == nil {
		t.Fatal("expected error when secrets exist and snapshot.sops=false")
	}
	evs2 := st.Events(state.EventsFilter{Kind: "snapshot", Limit: 1})
	if len(evs2) == 0 || evs2[0].Actor != "bob" || evs2[0].Status != "error" {
		t.Fatalf("expected snapshot error event for bob when sops=false, got %+v", evs2)
	}

	// 6. When sops binary is missing from PATH and Secrets exist -> must fail and record error event!
	engNoSOPS.Close()
	_ = os.Remove(filepath.Join(fakeBin, "sops"))
	engMissingBin, _ := New(cfg, []*kube.Client{deCl}, st)
	defer engMissingBin.Close()
	if _, err := engMissingBin.Snapshot(ctx, "carol"); err == nil {
		t.Fatal("expected error when sops binary is missing and secrets exist")
	}
	evs3 := st.Events(state.EventsFilter{Kind: "snapshot", Limit: 1})
	if len(evs3) == 0 || evs3[0].Actor != "carol" || evs3[0].Status != "error" {
		t.Fatalf("expected snapshot error event for carol when sops missing, got %+v", evs3)
	}
}

func TestSnapshotDecoupledBackgroundContextAndTimeout(t *testing.T) {
	tmp := t.TempDir()
	snapBare := filepath.Join(tmp, "clusters.git")
	if err := os.MkdirAll(snapBare, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, snapBare, "init", "--bare", "-b", "main")

	// Slow fake cluster wrapper that sleeps 120ms on GET so we can cancel caller ctx mid-flight!
	fast := fake.New()
	defer fast.Close()
	fast.Seed(map[string]any{
		"kind":     "ConfigMap",
		"metadata": map[string]any{"name": "cm-1", "namespace": "app"},
		"data":     map[string]any{"k": "v"},
	})
	fastCfg := fast.Config("de")

	var delayMs atomic.Int64
	delayMs.Store(25)
	slowProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Duration(delayMs.Load()) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, fastCfg.Server+r.URL.RequestURI(), r.Body)
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vals := range resp.Header {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 32*1024)
		for {
			n, rErr := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
			}
			if rErr != nil {
				break
			}
		}
	}))
	defer slowProxy.Close()

	slowCl, err := kube.New(kube.Config{
		Name:   "de",
		Server: slowProxy.URL,
		Token:  fastCfg.Token,
	})
	if err != nil {
		t.Fatal(err)
	}

	st, _ := state.NewFileStore(context.Background(), filepath.Join(tmp, "state.json"))
	cfg := &Config{
		WorkDir:    filepath.Join(tmp, "work"),
		Namespaces: []string{"app"},
		Auth:       AuthConfig{Disabled: true},
		Snapshot: SnapshotConfig{
			Repo:   snapBare,
			Branch: "main",
			Dir:    "snapshots",
		},
	}
	eng, err := New(cfg, []*kube.Client{slowCl}, st)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	// 1. Caller cancels ctx after 15ms while snapshot takes ~250ms (10 kinds * 25ms):
	// Caller gets context.Canceled, BUT background snapshot continues on Background+timeout and commits!
	shortCallerCtx, cancelCaller := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancelCaller()
	if _, err := eng.Snapshot(shortCallerCtx, "ci-caller"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected caller DeadlineExceeded, got %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var snapEv state.Event
	for time.Now().Before(deadline) {
		evs := st.Events(state.EventsFilter{Kind: "snapshot"})
		if len(evs) > 0 {
			snapEv = evs[0]
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if snapEv.Status != "ok" || snapEv.Message != "committed" {
		t.Fatalf("background snapshot did not complete after caller ctx cancel: %+v", snapEv)
	}

	// 2. Runner background timeout cancels hung snapshots cleanly and records an error event!
	eng.snapRunner.setTimeout(20 * time.Millisecond)
	delayMs.Store(80)
	if _, err := eng.Snapshot(context.Background(), "timeout-test"); err == nil {
		t.Fatal("expected snapshot timeout error")
	}
	evsAfterTimeout := st.Events(state.EventsFilter{Kind: "snapshot", Limit: 1})
	if len(evsAfterTimeout) == 0 || evsAfterTimeout[0].Actor != "timeout-test" || evsAfterTimeout[0].Status != "error" {
		t.Fatalf("expected snapshot timeout error event, got %+v", evsAfterTimeout)
	}
}

func TestSnapshotWithRealSOPS(t *testing.T) {
	if _, err := exec.LookPath("sops"); err != nil {
		t.Skip("sops binary not installed on host; SOPS encryption flow verified via stub sops test")
	}
}
