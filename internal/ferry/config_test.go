package ferry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestParseConfigEnvExpansionGitHubTokensAndClustersFile(t *testing.T) {
	t.Setenv("OIDC_SECRET", "s3cr3t$keep")
	t.Setenv("GH_TOKENS", `{"org-a":"ghp_alpha","org-b":"ghp_bravo"}`)

	dir := t.TempDir()
	cf := filepath.Join(dir, "clusters.json")
	if err := os.WriteFile(cf, []byte(`{
		"sg": {"server": "https://k8s-sg.example.com:6443", "ca": "cGVt", "token": "tok-sg"},
		"us": {"server": "https://k8s-us.example.com:6443", "ca": "", "token": "tok-us"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	raw := []byte(`
auth:
  oidc:
    issuer: "https://id.example.com"
    clientID: "ferry"
    clientSecret: "${OIDC_SECRET}"
    publicURL: "https://ferry.example.com"
    allowedGroups: ["ops"]
state:
  kubernetes:
    namespace: "app"
    name: "ferry-state"
namespaces: ["app"]
clusters:
  - name: de
    inCluster: true
clustersFile: "` + cf + `"
github:
  tokens: ${GH_TOKENS}
snapshot:
  hourUTC: 0
`)

	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Auth.OIDC.ClientSecret != "s3cr3t$keep" {
		t.Fatalf("ClientSecret = %q", cfg.Auth.OIDC.ClientSecret)
	}
	if cfg.GitHub.TokenForOwner("org-a") != "ghp_alpha" || cfg.GitHub.TokenForOwner("org-b") != "ghp_bravo" {
		t.Fatalf("GitHub.Tokens = %+v", cfg.GitHub.Tokens)
	}
	if cfg.GitHub.TokenForOwner("ORG-A") != "ghp_alpha" {
		t.Fatalf("expected case-insensitive owner lookup, got %q", cfg.GitHub.TokenForOwner("ORG-A"))
	}
	if cfg.GitHub.TokenForOwner("missing") != "" {
		t.Fatalf("unexpected token for missing owner")
	}
	if !cfg.Snapshot.EffectiveSOPS() {
		t.Fatalf("expected Snapshot.EffectiveSOPS default true")
	}
	if len(cfg.Clusters) != 3 || cfg.Clusters[1].Name != "sg" || cfg.Clusters[1].CAData != "cGVt" {
		t.Fatalf("Clusters = %+v", cfg.Clusters)
	}
	if cfg.Snapshot.EffectiveHourUTC() != 0 {
		t.Fatalf("HourUTC = %d, want 0", cfg.Snapshot.EffectiveHourUTC())
	}
	stateCluster, err := cfg.ResolveStateCluster()
	if err != nil || stateCluster != "de" {
		t.Fatalf("ResolveStateCluster = %q, %v", stateCluster, err)
	}
}

func TestConfigValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "missing clusters",
			yaml: `
auth: {disabled: true}
state: {file: "/tmp/s.json"}
namespaces: ["app"]
`,
		},
		{
			name: "duplicate cluster",
			yaml: `
auth: {disabled: true}
state: {file: "/tmp/s.json"}
namespaces: ["app"]
clusters:
  - {name: de, inCluster: true}
  - {name: de, server: "https://x"}
`,
		},
		{
			name: "both state backends",
			yaml: `
auth: {disabled: true}
state:
  file: "/tmp/s.json"
  kubernetes: {namespace: "app", name: "ferry-state"}
namespaces: ["app"]
clusters:
  - {name: de, inCluster: true}
`,
		},
		{
			name: "oidc without allowlist",
			yaml: `
auth:
  oidc:
    issuer: "https://id.example.com"
    clientID: "ferry"
    clientSecret: "sec"
    publicURL: "https://ferry.example.com"
state: {file: "/tmp/s.json"}
namespaces: ["app"]
clusters:
  - {name: de, inCluster: true}
`,
		},
		{
			name: "invalid namespace",
			yaml: `
auth: {disabled: true}
state: {file: "/tmp/s.json"}
namespaces: ["../bad"]
clusters:
  - {name: de, inCluster: true}
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(tc.yaml)); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestLoadSeedSources(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "repos.json")
	if err := os.WriteFile(p, []byte(`{
		"org/web": {"branch": "main", "image": "web-app", "build_args": "ENV=prod"},
		"org/worker": {}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSeedSources(p)
	if err != nil {
		t.Fatalf("LoadSeedSources: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Repo != "org/web" || got[0].Image != "web-app" || got[0].BuildArgs != "ENV=prod" {
		t.Fatalf("got[0] = %+v", got[0])
	}
	if got[1].Repo != "org/worker" || got[1].Branch != "main" || got[1].Image != "worker" {
		t.Fatalf("got[1] = %+v", got[1])
	}
}

func TestDockerfileTiniEntrypointAndPipelineContract(t *testing.T) {
	repoRoot := filepath.Join("..", "..")

	dfBytes, err := os.ReadFile(filepath.Join(repoRoot, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	df := string(dfBytes)
	lastFrom := strings.LastIndex(df, "\nFROM ")
	if lastFrom < 0 {
		t.Fatalf("Dockerfile missing final FROM stage:\n%s", df)
	}
	finalStage := df[lastFrom:]
	if !strings.Contains(finalStage, "tini") {
		t.Fatalf("final Dockerfile stage must install tini:\n%s", finalStage)
	}
	if !strings.Contains(finalStage, `ENTRYPOINT ["tini", "--", "ferry"]`) {
		t.Fatalf("final Dockerfile stage must set ENTRYPOINT [\"tini\", \"--\", \"ferry\"]:\n%s", finalStage)
	}

	pipeBytes, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "pipeline.yml"))
	if err != nil {
		t.Fatalf("read deploy/pipeline.yml: %v", err)
	}
	pipe := string(pipeBytes)
	for _, want := range []string{
		`"__BRANCH__":`,
		`api_trigger_build:`,
		`IMAGE_NAME`,
		`SRC_REPO`,
		`SRC_SHA`,
		`BUILD_ARGS`,
		`hack/bump.sh`,
	} {
		if !strings.Contains(pipe, want) {
			t.Fatalf("deploy/pipeline.yml missing %q:\n%s", want, pipe)
		}
	}
	for _, forbidden := range []string{"RELAY_URL", "SOURCE_REPO", "SOURCE_SHA"} {
		if strings.Contains(pipe, forbidden) {
			t.Fatalf("deploy/pipeline.yml must not introduce %q:\n%s", forbidden, pipe)
		}
	}

	rendered := strings.ReplaceAll(pipe, "__BRANCH__", "src/my-app")
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(rendered), &parsed); err != nil {
		t.Fatalf("deploy/pipeline.yml invalid YAML: %v", err)
	}
	branchCfg, ok := parsed["src/my-app"].(map[string]any)
	if !ok || branchCfg["api_trigger_build"] == nil {
		t.Fatalf("deploy/pipeline.yml did not render src/my-app.api_trigger_build: %+v", parsed)
	}
}
