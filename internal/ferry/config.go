// Package ferry implements the core multi-cluster rollout, build, webhook, and snapshot engine.
package ferry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/StarMoe-org/ferry/internal/kube"
)

var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnv substitutes ${VAR} placeholders from environment variables while leaving bare $ untouched.
func ExpandEnv(raw []byte) []byte {
	return envVarPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		sub := envVarPattern.FindSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		return []byte(os.Getenv(string(sub[1])))
	})
}

// Config is the top-level YAML configuration for Ferry.
type Config struct {
	Listen           string         `json:"listen"`
	AnnotationPrefix string         `json:"annotationPrefix"`
	WorkDir          string         `json:"workDir"`
	Auth             AuthConfig     `json:"auth"`
	State            StateConfig    `json:"state"`
	Namespaces       []string       `json:"namespaces"`
	Clusters         []kube.Config  `json:"clusters"`
	ClustersFile     string         `json:"clustersFile"`
	CNB              CNBConfig      `json:"cnb"`
	GitHub           GitHubConfig   `json:"github"`
	Snapshot         SnapshotConfig `json:"snapshot"`
	SeedSources      string         `json:"seedSources"`
}

type AuthConfig struct {
	Disabled     bool            `json:"disabled"`
	SessionKey   string          `json:"sessionKey"`
	SessionHours int             `json:"sessionHours"` // how long a browser sign-in lasts
	OIDC         OIDCConfig      `json:"oidc"`
	GitRead      []GitReadTarget `json:"gitRead"`
}

type OIDCConfig struct {
	Issuer        string   `json:"issuer"`
	ClientID      string   `json:"clientID"`
	ClientSecret  string   `json:"clientSecret"`
	PublicURL     string   `json:"publicURL"`
	Scopes        []string `json:"scopes"`
	GroupsClaim   string   `json:"groupsClaim"`
	AllowedGroups []string `json:"allowedGroups"`
	AllowedUsers  []string `json:"allowedUsers"`
}

type GitReadTarget struct {
	URL      string `json:"url"`
	Username string `json:"username"`
}

type StateConfig struct {
	Kubernetes *KubeStateConfig `json:"kubernetes,omitempty"`
	File       string           `json:"file,omitempty"`
}

type KubeStateConfig struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type CNBConfig struct {
	Repo         string `json:"repo"`
	Token        string `json:"token"`
	ImagePrefix  string `json:"imagePrefix"`
	PipelineFile string `json:"pipelineFile"`
	BranchPrefix string `json:"branchPrefix"`
	API          string `json:"api"`
	Git          string `json:"git"`
	// DeployOnSuccess deploys a build's image once CNB reports the build succeeded (default true).
	DeployOnSuccess *bool `json:"deployOnSuccess"`
}

func (c CNBConfig) EffectiveDeployOnSuccess() bool {
	return c.DeployOnSuccess == nil || *c.DeployOnSuccess
}

type GitHubConfig struct {
	Tokens map[string]string `json:"tokens"`
	// HookTokens, by owner, list and create repository webhooks when the owner's entry in Tokens
	// cannot (fine-grained tokens are often split by permission). Optional.
	HookTokens    map[string]string `json:"hookTokens"`
	WebhookSecret string            `json:"webhookSecret"`
	API           string            `json:"api"`
	Git           string            `json:"git"`
}

// TokenForOwner returns the configured GitHub personal/fine-grained token for owner, if any.
func (g GitHubConfig) TokenForOwner(owner string) string {
	return tokenForOwner(g.Tokens, owner)
}

// HookTokenForOwner returns the token for owner's webhooks: hookTokens[owner], else tokens[owner].
func (g GitHubConfig) HookTokenForOwner(owner string) string {
	if tok := tokenForOwner(g.HookTokens, owner); tok != "" {
		return tok
	}
	return tokenForOwner(g.Tokens, owner)
}

func tokenForOwner(tokens map[string]string, owner string) string {
	if len(tokens) == 0 || owner == "" {
		return ""
	}
	if tok, ok := tokens[owner]; ok {
		return tok
	}
	keys := make([]string, 0, len(tokens))
	for k := range tokens {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.EqualFold(k, owner) {
			return tokens[k]
		}
	}
	return ""
}

type SnapshotConfig struct {
	Repo         string         `json:"repo"`
	Branch       string         `json:"branch"`
	Dir          string         `json:"dir"`
	Username     string         `json:"username"`
	Token        string         `json:"token"`
	SOPS         *bool          `json:"sops"`
	AgePublicKey string         `json:"agePublicKey"`
	HourUTC      *int           `json:"hourUTC"`
	Author       SnapshotAuthor `json:"author"`
}

func (s SnapshotConfig) EffectiveSOPS() bool {
	if s.SOPS == nil {
		return true
	}
	return *s.SOPS
}

func (s SnapshotConfig) EffectiveHourUTC() int {
	if s.HourUTC == nil {
		return 19
	}
	return *s.HourUTC
}

type SnapshotAuthor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// SeedRepoEntry matches one entry in ci-relay's repos.json.
type SeedRepoEntry struct {
	Repo      string
	Branch    string
	Image     string
	BuildArgs string
}

// LoadConfig reads, expands ${VAR} references, merges clustersFile, applies defaults, and validates path.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return ParseConfig(raw)
}

// ParseConfig expands ${VAR}, unmarshals YAML, loads clustersFile if configured, applies defaults, and validates.
func ParseConfig(raw []byte) (*Config, error) {
	expanded := ExpandEnv(raw)
	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parse config yaml: %w", err)
	}
	cfg.ApplyDefaults()
	if cfg.ClustersFile != "" {
		extra, err := LoadClustersFile(cfg.ClustersFile)
		if err != nil {
			return nil, err
		}
		cfg.Clusters = append(cfg.Clusters, extra...)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ApplyDefaults fills omitted configuration values with their documented defaults.
func (c *Config) ApplyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.AnnotationPrefix == "" {
		c.AnnotationPrefix = "ferry.star.moe"
	}
	if c.WorkDir == "" {
		c.WorkDir = "/work"
	}
	if c.Auth.SessionHours == 0 {
		c.Auth.SessionHours = 12
	}
	if len(c.Auth.OIDC.Scopes) == 0 {
		c.Auth.OIDC.Scopes = []string{"openid", "profile", "groups"}
	}
	if c.Auth.OIDC.GroupsClaim == "" {
		c.Auth.OIDC.GroupsClaim = "groups"
	}
	if c.CNB.BranchPrefix == "" {
		c.CNB.BranchPrefix = "src/"
	}
	if c.CNB.API == "" {
		c.CNB.API = "https://api.cnb.cool"
	}
	if c.CNB.Git == "" {
		c.CNB.Git = "https://cnb.cool"
	}
	if c.GitHub.API == "" {
		c.GitHub.API = "https://api.github.com"
	}
	if c.GitHub.Git == "" {
		c.GitHub.Git = "https://github.com"
	}
	if c.Snapshot.Branch == "" {
		c.Snapshot.Branch = "main"
	}
	if c.Snapshot.Dir == "" {
		c.Snapshot.Dir = "snapshots"
	}
	if c.Snapshot.SOPS == nil {
		b := true
		c.Snapshot.SOPS = &b
	}
	if c.Snapshot.HourUTC == nil {
		h := 19
		c.Snapshot.HourUTC = &h
	}
	if c.Snapshot.Author.Name == "" {
		c.Snapshot.Author.Name = "ferry"
	}
	if c.Snapshot.Author.Email == "" {
		c.Snapshot.Author.Email = "ferry@localhost"
	}
}

// Validate checks structural invariants at startup so misconfigurations fail early.
func (c *Config) Validate() error {
	if len(c.Clusters) == 0 {
		return errors.New("config: at least one cluster is required")
	}
	seenClusters := make(map[string]bool, len(c.Clusters))
	for _, cl := range c.Clusters {
		if !kube.ValidName(cl.Name) {
			return fmt.Errorf("config: invalid cluster name %q", cl.Name)
		}
		if seenClusters[cl.Name] {
			return fmt.Errorf("config: duplicate cluster name %q", cl.Name)
		}
		seenClusters[cl.Name] = true
		if !cl.InCluster && strings.TrimSpace(cl.Server) == "" {
			return fmt.Errorf("config: cluster %q requires server when inCluster is false", cl.Name)
		}
	}

	if c.Auth.SessionHours < 0 {
		return errors.New("config: auth.sessionHours must be positive")
	}

	if len(c.Namespaces) == 0 {
		return errors.New("config: at least one namespace is required")
	}
	for _, ns := range c.Namespaces {
		if !kube.ValidName(ns) {
			return fmt.Errorf("config: invalid namespace %q", ns)
		}
	}

	hasFile := strings.TrimSpace(c.State.File) != ""
	hasKube := c.State.Kubernetes != nil && (c.State.Kubernetes.Namespace != "" || c.State.Kubernetes.Name != "" || c.State.Kubernetes.Cluster != "")
	if hasFile == hasKube {
		return errors.New("config: set exactly one of state.kubernetes or state.file")
	}
	if hasKube {
		if !kube.ValidName(c.State.Kubernetes.Namespace) {
			return fmt.Errorf("config: invalid state.kubernetes.namespace %q", c.State.Kubernetes.Namespace)
		}
		if !kube.ValidName(c.State.Kubernetes.Name) {
			return fmt.Errorf("config: invalid state.kubernetes.name %q", c.State.Kubernetes.Name)
		}
		if _, err := c.ResolveStateCluster(); err != nil {
			return err
		}
	}

	if !c.Auth.Disabled {
		o := c.Auth.OIDC
		if o.Issuer == "" || o.ClientID == "" || o.ClientSecret == "" || o.PublicURL == "" {
			return errors.New("config: auth.oidc requires issuer, clientID, clientSecret, and publicURL unless auth.disabled is true")
		}
		if len(o.AllowedGroups) == 0 && len(o.AllowedUsers) == 0 {
			return errors.New("config: auth.oidc requires at least one entry in allowedGroups or allowedUsers")
		}
	}

	if c.CNB.Repo != "" {
		if c.CNB.Token == "" || c.CNB.ImagePrefix == "" || c.CNB.PipelineFile == "" {
			return errors.New("config: cnb requires token, imagePrefix, and pipelineFile when cnb.repo is set")
		}
	}

	if h := c.Snapshot.EffectiveHourUTC(); h < 0 || h > 23 {
		return fmt.Errorf("config: snapshot.hourUTC must be in 0..23, got %d", h)
	}
	return nil
}

// ResolveStateCluster picks the cluster name for ConfigMap state storage:
// explicit state.kubernetes.cluster if set, else the first inCluster entry, else the first cluster.
func (c *Config) ResolveStateCluster() (string, error) {
	if c.State.Kubernetes == nil {
		return "", errors.New("state.kubernetes is not configured")
	}
	if want := strings.TrimSpace(c.State.Kubernetes.Cluster); want != "" {
		for _, cl := range c.Clusters {
			if cl.Name == want {
				return want, nil
			}
		}
		return "", fmt.Errorf("config: state.kubernetes.cluster %q not found in clusters", want)
	}
	for _, cl := range c.Clusters {
		if cl.InCluster {
			return cl.Name, nil
		}
	}
	if len(c.Clusters) == 0 {
		return "", errors.New("config: no clusters configured")
	}
	return c.Clusters[0].Name, nil
}

// LoadClustersFile parses ci-relay's CLUSTERS_FILE JSON:
// {"sg": {"server": "https://...", "ca": "<base64 PEM>", "token": "..."}}
func LoadClustersFile(path string) ([]kube.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read clustersFile %s: %w", path, err)
	}
	var entries map[string]struct {
		Server string `json:"server"`
		CA     string `json:"ca"`
		Token  string `json:"token"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse clustersFile %s: %w", path, err)
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]kube.Config, 0, len(names))
	for _, name := range names {
		e := entries[name]
		out = append(out, kube.Config{
			Name:   name,
			Server: e.Server,
			CAData: e.CA,
			Token:  e.Token,
		})
	}
	return out, nil
}

// LoadSeedSources parses ci-relay's repos.json into deterministic SeedRepoEntry records.
func LoadSeedSources(path string) ([]SeedRepoEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read seedSources %s: %w", path, err)
	}
	var m map[string]struct {
		Branch    string `json:"branch"`
		Image     string `json:"image"`
		BuildArgs string `json:"build_args"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse seedSources %s: %w", path, err)
	}
	repos := make([]string, 0, len(m))
	for repo := range m {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	out := make([]SeedRepoEntry, 0, len(repos))
	for _, repo := range repos {
		v := m[repo]
		branch := strings.TrimSpace(v.Branch)
		if branch == "" {
			branch = "main"
		}
		img := strings.TrimSpace(v.Image)
		if img == "" {
			if idx := strings.LastIndex(repo, "/"); idx >= 0 {
				img = repo[idx+1:]
			} else {
				img = repo
			}
		}
		out = append(out, SeedRepoEntry{
			Repo:      repo,
			Branch:    branch,
			Image:     img,
			BuildArgs: v.BuildArgs,
		})
	}
	return out, nil
}
