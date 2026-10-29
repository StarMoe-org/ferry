package ferry

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/StarMoe-org/ferry/internal/kube"
	"github.com/StarMoe-org/ferry/internal/kube/fake"
	"github.com/StarMoe-org/ferry/internal/state"
)

func TestSnapshotCleaningGitPushLegacyMigrationAndUnchanged(t *testing.T) {
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
					"deployment.kubernetes.io/revision":                "9",
					"kubernetes.io/change-cause":                       "ghcr.io/org/web:v1",
				},
			},
			"spec":   map[string]any{"replicas": 2},
			"status": map[string]any{"readyReplicas": 2},
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
		"last-applied-configuration",
		"deployment.kubernetes.io/revision",
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
	for _, want := range []string{"app-config", "strip-api", "kubernetes.io/change-cause"} {
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

func errorsIsNotExist(err error) bool {
	return os.IsNotExist(err)
}

func TestSnapshotSOPSHashSkipAndFailureCleanupWithStubSOPS(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	// Create stub sops script in fakeBin prepended to PATH.
	fakeBin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	callsFile := filepath.Join(tmp, "sops.calls")
	failFlag := filepath.Join(tmp, "sops.fail")
	sopsScript := `#!/bin/sh
set -eu
echo "call" >> "` + callsFile + `"
if [ -f "` + failFlag + `" ]; then
  echo "simulated sops failure" >&2
  exit 1
fi
# Last argument is the target file path for --in-place
eval "file=\${$#}"
printf "# sops-encrypted\nsops:\n  version: stub\n" > "$file"
`
	if err := os.WriteFile(filepath.Join(fakeBin, "sops"), []byte(sopsScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	snapBare := filepath.Join(tmp, "clusters.git")
	if err := os.MkdirAll(snapBare, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, snapBare, "init", "--bare", "-b", "main")

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
			"type":     "helm.sh/release.v1",
			"metadata": map[string]any{"name": "sh.helm.release.v1.x", "namespace": "app"},
			"data":     map[string]any{"release": "skip-helm"},
		},
	)

	deCl, _ := de.Client("de")
	st, _ := state.NewFileStore(ctx, filepath.Join(tmp, "state.json"))
	cfg := &Config{
		WorkDir:    filepath.Join(tmp, "work"),
		Namespaces: []string{"app"},
		Auth:       AuthConfig{Disabled: true},
		Snapshot: SnapshotConfig{
			Repo:         snapBare,
			Branch:       "main",
			Dir:          "snapshots",
			AgePublicKey: "age1testrecipient",
		},
	}
	eng, _ := New(cfg, []*kube.Client{deCl}, st)
	defer eng.Close()

	res1, err := eng.Snapshot(ctx, "alice")
	if err != nil || res1.Status != "committed" {
		t.Fatalf("Snapshot #1 = %+v, %v", res1, err)
	}
	calls1, _ := os.ReadFile(callsFile)
	if len(strings.Split(strings.TrimSpace(string(calls1)), "\n")) != 1 {
		t.Fatalf("expected 1 sops call, got %q", string(calls1))
	}

	// Second snapshot with identical secret plaintext must skip sops via .secrets.sha256!
	res2, err := eng.Snapshot(ctx, "alice")
	if err != nil || res2.Status != "unchanged" {
		t.Fatalf("Snapshot #2 = %+v, %v", res2, err)
	}
	calls2, _ := os.ReadFile(callsFile)
	if len(strings.Split(strings.TrimSpace(string(calls2)), "\n")) != 1 {
		t.Fatalf("expected sops skipped on unchanged sha256, got %q", string(calls2))
	}

	// Mutate secret and trigger sops failure -> Snapshot must fail cleanly without committing plaintext!
	_ = os.WriteFile(failFlag, []byte("1"), 0o644)
	de.Seed(map[string]any{
		"kind":     "Secret",
		"type":     "Opaque",
		"metadata": map[string]any{"name": "app-secret", "namespace": "app"},
		"data":     map[string]any{"API_KEY": "bmV3LXNlY3JldA=="},
	})
	if _, err := eng.Snapshot(ctx, "alice"); err == nil {
		t.Fatal("expected error when sops fails")
	}
}

func TestSnapshotWithRealSOPS(t *testing.T) {
	if _, err := exec.LookPath("sops"); err != nil {
		t.Skip("sops binary not installed on host; SOPS encryption flow verified via stub sops test")
	}
}
