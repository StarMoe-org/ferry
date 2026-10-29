package ferry

import (
	"context"
	"testing"
)

func TestStartDemoEnvironment(t *testing.T) {
	ctx := context.Background()
	demo, err := StartDemo(ctx, ":0")
	if err != nil {
		t.Fatalf("StartDemo: %v", err)
	}
	defer demo.Close()

	inv, err := demo.Engine.Workloads(ctx, true)
	if err != nil {
		t.Fatalf("Workloads: %v", err)
	}
	if len(inv.Clusters) != 3 {
		t.Fatalf("clusters = %d, want 3", len(inv.Clusters))
	}
	if len(inv.Workloads) < 10 {
		t.Fatalf("workloads = %d, want >= 10", len(inv.Workloads))
	}

	var sawArgo, sawPaused, sawCron, sawDaemon bool
	for _, w := range inv.Workloads {
		if w.ArgoCD {
			sawArgo = true
		}
		if w.Paused {
			sawPaused = true
		}
		if w.Kind == "CronJob" {
			sawCron = true
		}
		if w.Kind == "DaemonSet" {
			sawDaemon = true
		}
	}
	if !sawArgo || !sawPaused || !sawCron || !sawDaemon {
		t.Fatalf("missing demo workload traits: argo=%v paused=%v cron=%v daemon=%v", sawArgo, sawPaused, sawCron, sawDaemon)
	}

	// Verify live preview & deploy work in demo mode.
	res, _, err := demo.Engine.Deploy(ctx, "dev", "docker.cnb.cool/acme/builder/web-portal", "ff0011223344", "demo", true)
	if err != nil || len(res) != 3 {
		t.Fatalf("demo preview = %+v, %v", res, err)
	}
}
