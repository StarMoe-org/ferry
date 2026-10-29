package ferry

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/StarMoe-org/ferry/internal/kube"
	"github.com/StarMoe-org/ferry/internal/kube/fake"
	"github.com/StarMoe-org/ferry/internal/state"
)

// DemoEnv holds the resources started for ferry -demo mode.
type DemoEnv struct {
	Engine  *Engine
	TempDir string
	servers []*fake.Server
}

// Close tears down the demo Engine, fake Kubernetes servers, and temporary directory.
func (d *DemoEnv) Close() {
	if d.Engine != nil {
		d.Engine.Close()
	}
	for _, s := range d.servers {
		s.Close()
	}
	if d.TempDir != "" {
		_ = os.RemoveAll(d.TempDir)
	}
}

// StartDemo spins up three realistic in-memory Kubernetes clusters, temporary file state,
// seeded sources/events/tokens, and a local bare snapshot repository for ferry -demo.
func StartDemo(ctx context.Context, listenAddr string) (*DemoEnv, error) {
	if listenAddr == "" {
		listenAddr = ":8080"
	}
	tmp, err := os.MkdirTemp("", "ferry-demo-*")
	if err != nil {
		return nil, fmt.Errorf("create demo tempdir: %w", err)
	}

	de := fake.New()
	de.SetGitVersion("v1.34.1+k3s1")
	sg := fake.New()
	sg.SetGitVersion("v1.34.1+k3s1")
	jp := fake.New()
	jp.SetGitVersion("v1.33.4+k3s1")
	jp.SetDisableTraefik(true)

	servers := []*fake.Server{de, sg, jp}
	cleanupOnErr := func() {
		for _, s := range servers {
			s.Close()
		}
		_ = os.RemoveAll(tmp)
	}

	seedDemoClusters(de, sg, jp)

	deCl, err := de.Client("de")
	if err != nil {
		cleanupOnErr()
		return nil, err
	}
	sgCl, err := sg.Client("sg")
	if err != nil {
		cleanupOnErr()
		return nil, err
	}
	jpCl, err := jp.Client("jp")
	if err != nil {
		cleanupOnErr()
		return nil, err
	}

	// Optional local bare git repo if git is available so "Snapshot now" works live in demo mode.
	snapRepo := ""
	if _, lookErr := exec.LookPath("git"); lookErr == nil {
		bare := filepath.Join(tmp, "demo-snapshots.git")
		if mkErr := os.MkdirAll(bare, 0o755); mkErr == nil {
			if exec.Command("git", "init", "-q", "--bare", "-b", "main", bare).Run() == nil {
				snapRepo = bare
			}
		}
	}

	cfg := &Config{
		Listen:           listenAddr,
		AnnotationPrefix: "ferry.star.moe",
		WorkDir:          filepath.Join(tmp, "work"),
		Auth: AuthConfig{
			Disabled: true,
			OIDC: OIDCConfig{
				PublicURL: "http://localhost:8080",
			},
		},
		State: StateConfig{
			File: filepath.Join(tmp, "state.json"),
		},
		Namespaces: []string{"app", "edge"},
		Clusters: []kube.Config{
			de.Config("de"),
			sg.Config("sg"),
			jp.Config("jp"),
		},
		CNB: CNBConfig{
			ImagePrefix: "docker.cnb.cool/acme/builder",
		},
		Snapshot: SnapshotConfig{
			Repo:   snapRepo,
			Branch: "main",
			Dir:    "snapshots",
		},
	}
	cfg.ApplyDefaults()

	st, err := state.NewFileStore(ctx, cfg.State.File)
	if err != nil {
		cleanupOnErr()
		return nil, err
	}
	if err := seedDemoState(ctx, st); err != nil {
		cleanupOnErr()
		return nil, err
	}

	eng, err := New(cfg, []*kube.Client{deCl, sgCl, jpCl}, st)
	if err != nil {
		cleanupOnErr()
		return nil, err
	}

	return &DemoEnv{
		Engine:  eng,
		TempDir: tmp,
		servers: servers,
	}, nil
}

func seedDemoClusters(de, sg, jp *fake.Server) {
	const (
		gwImg     = "docker.cnb.cool/acme/builder/gateway-api"
		portalImg = "docker.cnb.cool/acme/builder/web-portal"
		agentImg  = "docker.cnb.cool/acme/builder/edge-agent"
		billImg   = "ghcr.io/acme/billing-worker"
		argoImg   = "ghcr.io/acme/core-ingress"
	)

	lastSched := time.Now().UTC().Add(-22 * time.Minute).Format(time.RFC3339)

	// Cluster de
	de.Seed(
		demoDeployment("gateway-api", "app", gwImg+":a81f29c4012e", 3, 3, map[string]any{
			"kubernetes.io/change-cause": gwImg + ":a81f29c4012e github:acme/gateway-api@a81f29c4012e",
		}),
		demoDeployment("web-portal", "app", portalImg+":c470b18e93a2", 2, 2, map[string]any{
			"kubernetes.io/change-cause": portalImg + ":c470b18e93a2 github:acme/web-portal@c470b18e93a2",
		}),
		map[string]any{
			"kind":     "StatefulSet",
			"metadata": map[string]any{"name": "billing-worker", "namespace": "app"},
			"spec": map[string]any{
				"replicas": 2,
				"template": map[string]any{
					"spec": map[string]any{
						"initContainers": []any{
							map[string]any{"name": "migrate", "image": billImg + ":v2.8.4"},
						},
						"containers": []any{
							map[string]any{"name": "worker", "image": billImg + ":v2.8.4"},
						},
					},
				},
			},
			"status": map[string]any{"readyReplicas": 2, "updatedReplicas": 2, "availableReplicas": 2},
		},
		map[string]any{
			"kind":     "CronJob",
			"metadata": map[string]any{"name": "usage-rollup", "namespace": "app"},
			"spec": map[string]any{
				"schedule": "15 * * * *",
				"suspend":  false,
				"jobTemplate": map[string]any{
					"spec": map[string]any{
						"template": map[string]any{
							"spec": map[string]any{
								"containers": []any{
									map[string]any{"name": "rollup", "image": billImg + ":v2.8.4"},
								},
							},
						},
					},
				},
			},
			"status": map[string]any{"lastScheduleTime": lastSched},
		},
		demoDaemonSet("edge-agent", "edge", agentImg+":71dc08fe4419", 4, 4, nil),
		demoDeployment("core-ingress", "edge", argoImg+":v1.14.0", 2, 2, map[string]any{
			"argocd.argoproj.io/tracking-id": "platform-ingress:apps/Deployment:edge/core-ingress",
		}),
	)

	// Cluster sg (web-portal is paused on older tag to highlight drift + paused controls)
	sg.Seed(
		demoDeployment("gateway-api", "app", gwImg+":a81f29c4012e", 3, 3, map[string]any{
			"kubernetes.io/change-cause": gwImg + ":a81f29c4012e github:acme/gateway-api@a81f29c4012e",
		}),
		demoDeployment("web-portal", "app", portalImg+":9f203d14aa80", 2, 2, map[string]any{
			"ferry.star.moe/auto-deploy": "false",
			"kubernetes.io/change-cause": portalImg + ":9f203d14aa80 rollout:dev@localhost",
		}),
		demoDaemonSet("edge-agent", "edge", agentImg+":71dc08fe4419", 3, 3, nil),
		demoDeployment("core-ingress", "edge", argoImg+":v1.14.0", 2, 2, map[string]any{
			"argocd.argoproj.io/tracking-id": "platform-ingress:apps/Deployment:edge/core-ingress",
		}),
	)

	// Cluster jp
	jp.Seed(
		demoDeployment("gateway-api", "app", gwImg+":a81f29c4012e", 2, 2, map[string]any{
			"kubernetes.io/change-cause": gwImg + ":a81f29c4012e github:acme/gateway-api@a81f29c4012e",
		}),
		demoDeployment("web-portal", "app", portalImg+":c470b18e93a2", 2, 2, map[string]any{
			"kubernetes.io/change-cause": portalImg + ":c470b18e93a2 github:acme/web-portal@c470b18e93a2",
		}),
		demoDaemonSet("edge-agent", "edge", agentImg+":71dc08fe4419", 3, 3, nil),
	)
}

func demoDeployment(name, ns, image string, desired, ready int, annotations map[string]any) map[string]any {
	meta := map[string]any{"name": name, "namespace": ns}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	return map[string]any{
		"kind":     "Deployment",
		"metadata": meta,
		"spec": map[string]any{
			"replicas": desired,
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{"name": name, "image": image},
					},
				},
			},
		},
		"status": map[string]any{
			"readyReplicas":     ready,
			"updatedReplicas":   ready,
			"availableReplicas": ready,
		},
	}
}

func demoDaemonSet(name, ns, image string, desired, ready int, annotations map[string]any) map[string]any {
	meta := map[string]any{"name": name, "namespace": ns}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	return map[string]any{
		"kind":     "DaemonSet",
		"metadata": meta,
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{"name": name, "image": image},
					},
				},
			},
		},
		"status": map[string]any{
			"desiredNumberScheduled": desired,
			"numberReady":            ready,
			"updatedNumberScheduled": ready,
			"numberAvailable":        ready,
		},
	}
}

func seedDemoState(ctx context.Context, st *state.Store) error {
	const (
		gwImg     = "docker.cnb.cool/acme/builder/gateway-api"
		portalImg = "docker.cnb.cool/acme/builder/web-portal"
		agentImg  = "docker.cnb.cool/acme/builder/edge-agent"
		billImg   = "ghcr.io/acme/billing-worker"
	)

	gwSrc, err := st.CreateSource(ctx, state.Source{
		Repo:    "acme/gateway-api",
		Branch:  "main",
		Builder: "cnb",
		Image:   "gateway-api",
	})
	if err != nil {
		return err
	}
	portalSrc, err := st.CreateSource(ctx, state.Source{
		Repo:      "acme/web-portal",
		Branch:    "main",
		Builder:   "cnb",
		Image:     "web-portal",
		BuildArgs: "VITE_ENV=production",
	})
	if err != nil {
		return err
	}
	_, err = st.CreateSource(ctx, state.Source{
		Repo:    "acme/edge-agent",
		Branch:  "main",
		Builder: "cnb",
		Image:   "edge-agent",
	})
	if err != nil {
		return err
	}
	billSrc, err := st.CreateSource(ctx, state.Source{
		Repo:    "acme/billing-worker",
		Branch:  "release",
		Builder: "external",
		Image:   billImg,
	})
	if err != nil {
		return err
	}

	_, plainTok, err := st.CreateToken(ctx, "github-actions-prod", "dev@localhost")
	if err != nil {
		return err
	}
	st.AuthenticateToken(ctx, plainTok)

	now := time.Now().UTC()
	// Append oldest to newest since AppendEvent prepends newest-first.
	events := []state.Event{
		{
			At:      now.Add(-6 * time.Hour),
			Kind:    "snapshot",
			Actor:   "schedule",
			Commit:  "4b91e07c22d84a91f00319bc82147ae0921f8c4a",
			Status:  "ok",
			Message: "committed",
		},
		{
			At:       now.Add(-3 * time.Hour),
			Kind:     "deploy",
			Actor:    "token:github-actions-prod",
			Image:    billImg,
			Tag:      "v2.8.4",
			Source:   "github:acme/billing-worker@90a41bc82d11",
			SourceID: billSrc.ID,
			Status:   "ok",
			Results: []state.Result{
				{Cluster: "de", Namespace: "app", Kind: "StatefulSet", Name: "billing-worker", Container: "migrate", From: billImg + ":v2.8.3", To: billImg + ":v2.8.4", Result: "deployed"},
				{Cluster: "de", Namespace: "app", Kind: "StatefulSet", Name: "billing-worker", Container: "worker", From: billImg + ":v2.8.3", To: billImg + ":v2.8.4", Result: "deployed"},
				{Cluster: "de", Namespace: "app", Kind: "CronJob", Name: "usage-rollup", Container: "rollup", From: billImg + ":v2.8.3", To: billImg + ":v2.8.4", Result: "deployed"},
			},
		},
		{
			At:      now.Add(-95 * time.Minute),
			Kind:    "pause",
			Actor:   "dev@localhost",
			Image:   portalImg,
			Status:  "ok",
			Message: "paused sg/app/Deployment (web-portal)",
		},
		{
			At:       now.Add(-48 * time.Minute),
			Kind:     "build",
			Actor:    "github",
			Image:    portalImg,
			Tag:      "c470b18e93a2",
			Source:   "github:acme/web-portal@c470b18e93a2",
			SourceID: portalSrc.ID,
			Commit:   "c470b18e93a29104f8bc2291048aa12093fe8102",
			LogURL:   "https://cnb.cool/acme/builder/-/build/logs/build-1042",
			Status:   "ok",
			Message:  "deployed",
		},
		{
			At:       now.Add(-46 * time.Minute),
			Kind:     "deploy",
			Actor:    "ci",
			Image:    portalImg,
			Tag:      "c470b18e93a2",
			Source:   "github:acme/web-portal@c470b18e93a2",
			SourceID: portalSrc.ID,
			Status:   "ok",
			Results: []state.Result{
				{Cluster: "de", Namespace: "app", Kind: "Deployment", Name: "web-portal", Container: "web-portal", From: portalImg + ":9f203d14aa80", To: portalImg + ":c470b18e93a2", Result: "deployed"},
				{Cluster: "sg", Namespace: "app", Kind: "Deployment", Name: "web-portal", Container: "web-portal", From: portalImg + ":9f203d14aa80", To: portalImg + ":c470b18e93a2", Result: "paused"},
				{Cluster: "jp", Namespace: "app", Kind: "Deployment", Name: "web-portal", Container: "web-portal", From: portalImg + ":9f203d14aa80", To: portalImg + ":c470b18e93a2", Result: "deployed"},
			},
		},
		{
			At:       now.Add(-14 * time.Minute),
			Kind:     "deploy",
			Actor:    "ci",
			Image:    gwImg,
			Tag:      "a81f29c4012e",
			Source:   "github:acme/gateway-api@a81f29c4012e",
			SourceID: gwSrc.ID,
			Status:   "ok",
			Results: []state.Result{
				{Cluster: "de", Namespace: "app", Kind: "Deployment", Name: "gateway-api", Container: "gateway-api", From: gwImg + ":53d801bc9201", To: gwImg + ":a81f29c4012e", Result: "deployed"},
				{Cluster: "sg", Namespace: "app", Kind: "Deployment", Name: "gateway-api", Container: "gateway-api", From: gwImg + ":53d801bc9201", To: gwImg + ":a81f29c4012e", Result: "deployed"},
				{Cluster: "jp", Namespace: "app", Kind: "Deployment", Name: "gateway-api", Container: "gateway-api", From: gwImg + ":53d801bc9201", To: gwImg + ":a81f29c4012e", Result: "deployed"},
			},
		},
	}

	for _, ev := range events {
		if _, err := st.AppendEvent(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}
