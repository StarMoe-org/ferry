package ferry

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfigEnvExpansionAndClustersFile(t *testing.T) {
	t.Setenv("OIDC_SECRET", "s3cr3t$keep")
	t.Setenv("GH_TOK", "ghp_test")

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
  token: "${GH_TOK}"
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
	if cfg.GitHub.Token != "ghp_test" {
		t.Fatalf("GitHub.Token = %q", cfg.GitHub.Token)
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
