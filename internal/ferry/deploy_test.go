package ferry

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/StarMoe-org/ferry/internal/kube"
	"github.com/StarMoe-org/ferry/internal/kube/fake"
	"github.com/StarMoe-org/ferry/internal/state"
)

func TestRunsAndSplitImage(t *testing.T) {
	if !Runs("ghcr.io/org/app", "ghcr.io/org/app") {
		t.Fatal("exact match failed")
	}
	if !Runs("ghcr.io/org/app:v1", "ghcr.io/org/app") {
		t.Fatal("tag match failed")
	}
	if !Runs("ghcr.io/org/app@sha256:deadbeef", "ghcr.io/org/app") {
		t.Fatal("digest match failed")
	}
	if Runs("ghcr.io/org/app-worker:v1", "ghcr.io/org/app") {
		t.Fatal("prefix collision should not match")
	}
	if !Runs("reg.local:5000/org/app:1234", "reg.local:5000/org/app") {
		t.Fatal("registry with port match failed")
	}
	repo, tag := SplitImage("reg.local:5000/org/app:v2.1")
	if repo != "reg.local:5000/org/app" || tag != "v2.1" {
		t.Fatalf("SplitImage port+tag = %q, %q", repo, tag)
	}
	repo2, tag2 := SplitImage("reg.local:5000/org/app")
	if repo2 != "reg.local:5000/org/app" || tag2 != "latest" {
		t.Fatalf("SplitImage port untagged = %q, %q", repo2, tag2)
	}
}

func TestDeploySourceTruncationRolloutValidationChangeCauseAndResumeEvent(t *testing.T) {
	ctx := context.Background()
	de := fake.New()
	defer de.Close()
	sg := fake.New()
	defer sg.Close()

	const img = "docker.cnb.cool/org/builder/web"

	// Cluster de:
	// 1. Deployment "api" with both initContainer and container matching img
	// 2. Deployment "argo-svc" managed by Argo CD
	// 3. StatefulSet "db-sync" already current
	// 4. CronJob "nightly" matching img
	de.Seed(
		map[string]any{
			"kind":     "Deployment",
			"metadata": map[string]any{"name": "api", "namespace": "app"},
			"spec": map[string]any{
				"replicas": 2,
				"template": map[string]any{
					"spec": map[string]any{
						"initContainers": []any{
							map[string]any{"name": "migrate", "image": img + ":oldtag000001"},
						},
						"containers": []any{
							map[string]any{"name": "api", "image": img + ":oldtag000001"},
							map[string]any{"name": "sidecar", "image": "envoyproxy/envoy:v1.30"},
						},
					},
				},
			},
			"status": map[string]any{"readyReplicas": 2, "updatedReplicas": 2, "availableReplicas": 2},
		},
		map[string]any{
			"kind": "Deployment",
			"metadata": map[string]any{
				"name":        "argo-svc",
				"namespace":   "app",
				"annotations": map[string]any{"argocd.argoproj.io/tracking-id": "argo:apps/Deployment:app/argo-svc"},
			},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"containers": []any{map[string]any{"name": "argo-svc", "image": img + ":oldtag000001"}},
					},
				},
			},
		},
		map[string]any{
			"kind":     "StatefulSet",
			"metadata": map[string]any{"name": "db-sync", "namespace": "app"},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"containers": []any{map[string]any{"name": "sync", "image": img + ":abcdef123456"}},
					},
				},
			},
		},
		map[string]any{
			"kind":     "CronJob",
			"metadata": map[string]any{"name": "nightly", "namespace": "app"},
			"spec": map[string]any{
				"schedule": "0 3 * * *",
				"jobTemplate": map[string]any{
					"spec": map[string]any{
						"template": map[string]any{
							"spec": map[string]any{
								"containers": []any{map[string]any{"name": "job", "image": img + ":oldtag000001"}},
							},
						},
					},
				},
			},
		},
	)

	// Cluster sg: paused DaemonSet
	sg.Seed(map[string]any{
		"kind": "DaemonSet",
		"metadata": map[string]any{
			"name":        "edge-agent",
			"namespace":   "app",
			"annotations": map[string]any{"ferry.star.moe/auto-deploy": "false"},
		},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "agent", "image": img + ":oldtag000001"}},
				},
			},
		},
		"status": map[string]any{"desiredNumberScheduled": 3, "numberReady": 3},
	})

	deCl, _ := de.Client("de")
	sgCl, _ := sg.Client("sg")
	st, err := state.NewFileStore(ctx, filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Namespaces: []string{"app"},
		Auth:       AuthConfig{Disabled: true},
	}
	eng, err := New(cfg, []*kube.Client{deCl, sgCl}, st)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	// Seed a started CNB build event to verify build-to-deploy linking.
	buildEv, err := st.AppendEvent(ctx, state.Event{
		Kind:   "build",
		Actor:  "github",
		Image:  img,
		Commit: "abcdef1234567890000000000000000000000000",
		Status: "started",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Dry-run preview must NOT patch or record events.
	dryRes, dryEv, err := eng.Deploy(ctx, "alice", img, "abcdef123456", "github:org/web@abcdef123456", true)
	if err != nil || dryEv != nil {
		t.Fatalf("dryRun = %v, %v", dryEv, err)
	}
	if len(de.Patches()) != 0 || len(sg.Patches()) != 0 {
		t.Fatalf("dryRun sent patches!")
	}
	gotByKind := map[string]string{}
	for _, r := range dryRes {
		gotByKind[r.Name+"/"+r.Container] = r.Result
	}
	if gotByKind["api/migrate"] != "would-deploy" ||
		gotByKind["api/api"] != "would-deploy" ||
		gotByKind["argo-svc/argo-svc"] != "argo-cd" ||
		gotByKind["db-sync/sync"] != "current" ||
		gotByKind["nightly/job"] != "would-deploy" ||
		gotByKind["edge-agent/agent"] != "paused" {
		t.Fatalf("unexpected dryRun results: %+v", gotByKind)
	}

	// 2. Real deploy truncates >300-char source to 300 chars, patches Deployment + CronJob, and links build event.
	longSource := "https://github.com/org/web/commit/" + strings.Repeat("a", 400)
	res, deployEv, err := eng.Deploy(ctx, "token:ci", img, "abcdef123456", longSource, false)
	if err != nil || deployEv == nil || deployEv.Status != "ok" {
		t.Fatalf("Deploy = %+v, %v", deployEv, err)
	}
	if len([]rune(deployEv.Source)) != 300 {
		t.Fatalf("deployEv.Source len = %d, want 300", len([]rune(deployEv.Source)))
	}
	if len(res) != 6 {
		t.Fatalf("len(res) = %d, want 6: %+v", len(res), res)
	}
	patches := de.Patches()
	if len(patches) != 2 {
		t.Fatalf("expected 2 patches on de (api + nightly), got %d", len(patches))
	}
	for _, p := range patches {
		if p.FieldManager != "ferry" || p.ContentType != "application/json-patch+json" {
			t.Fatalf("unexpected patch metadata: %+v", p)
		}
		var ops []map[string]any
		if err := json.Unmarshal(p.Body, &ops); err != nil {
			t.Fatal(err)
		}
		if ops[0]["op"] != "test" || ops[1]["op"] != "replace" {
			t.Fatalf("expected test+replace JSON patch ops, got %+v", ops)
		}
		lastOp := ops[len(ops)-1]
		wantCause := img + ":abcdef123456 " + longSource[:300]
		if lastOp["value"] != wantCause {
			t.Fatalf("deploy change-cause = %q, want %q", lastOp["value"], wantCause)
		}
	}
	linked, _ := st.GetEvent(buildEv.ID)
	if linked.Status != "ok" || linked.Message != "deployed" {
		t.Fatalf("build event not linked: %+v", linked)
	}

	// 3. Rollout validation & change-cause:
	// (a) Both image and container empty -> ErrBadInput (400)
	if _, err := eng.Rollout(ctx, "alice", RolloutRequest{
		Cluster: "sg", Namespace: "app", Kind: "DaemonSet", Name: "edge-agent", Tag: "abcdef123456",
	}); !errors.Is(err, ErrBadInput) {
		t.Fatalf("expected ErrBadInput when both image and container are empty, got %v", err)
	}

	// (b) Argo CD workload -> ErrArgoManaged (409)
	if _, err := eng.Rollout(ctx, "alice", RolloutRequest{
		Cluster: "de", Namespace: "app", Kind: "Deployment", Name: "argo-svc", Image: img, Tag: "abcdef123456",
	}); !errors.Is(err, ErrArgoManaged) {
		t.Fatalf("expected ErrArgoManaged on Rollout, got %v", err)
	}

	// (c) Explicit Rollout updates paused edge-agent on sg with change-cause "<image> rolled out by <user>"!
	rollEv, err := eng.Rollout(ctx, "alice", RolloutRequest{
		Cluster: "sg", Namespace: "app", Kind: "DaemonSet", Name: "edge-agent", Image: img, Tag: "abcdef123456",
	})
	if err != nil || rollEv.Status != "ok" || rollEv.Kind != "rollout" {
		t.Fatalf("Rollout paused DaemonSet = %+v, %v", rollEv, err)
	}
	sgPatches := sg.Patches()
	if len(sgPatches) != 1 {
		t.Fatalf("expected 1 patch on sg, got %d", len(sgPatches))
	}
	var sgOps []map[string]any
	if err := json.Unmarshal(sgPatches[0].Body, &sgOps); err != nil {
		t.Fatal(err)
	}
	gotRolloutCause := sgOps[len(sgOps)-1]["value"]
	wantRolloutCause := img + ":abcdef123456 rolled out by alice"
	if gotRolloutCause != wantRolloutCause {
		t.Fatalf("rollout change-cause = %q, want %q", gotRolloutCause, wantRolloutCause)
	}

	// 4. Pause & Resume:
	// - Refuses Argo CD
	// - Pause records Kind=="pause"
	// - Resume records Kind=="resume"!
	if _, err := eng.Pause(ctx, "alice", PauseRequest{
		Cluster: "de", Namespace: "app", Kind: "Deployment", Name: "argo-svc", Paused: true,
	}); !errors.Is(err, ErrArgoManaged) {
		t.Fatalf("expected ErrArgoManaged on Pause, got %v", err)
	}
	pauseEv, err := eng.Pause(ctx, "alice", PauseRequest{
		Cluster: "de", Namespace: "app", Kind: "Deployment", Name: "api", Paused: true,
	})
	if err != nil || pauseEv.Kind != "pause" {
		t.Fatalf("Pause event = %+v, %v (want kind=pause)", pauseEv, err)
	}
	resumeEv, err := eng.Pause(ctx, "alice", PauseRequest{
		Cluster: "sg", Namespace: "app", Kind: "DaemonSet", Name: "edge-agent", Paused: false,
	})
	if err != nil || resumeEv.Kind != "resume" {
		t.Fatalf("Resume event = %+v, %v (want kind=resume)", resumeEv, err)
	}

	// 5. Mutate nightly CronJob to another tag so Rollback of deployEv reports "changed since" for nightly,
	// while cleanly rolling back api (initContainer + container)!
	if _, err := eng.Rollout(ctx, "bob", RolloutRequest{
		Cluster: "de", Namespace: "app", Kind: "CronJob", Name: "nightly", Image: img, Tag: "newer9999999",
	}); err != nil {
		t.Fatalf("Rollout nightly: %v", err)
	}

	rbEv, err := eng.Rollback(ctx, "alice", deployEv.ID)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rbEv.Status != "partial" {
		t.Fatalf("expected partial rollback status, got %+v", rbEv)
	}
	foundChangedSince := false
	revertedCount := 0
	for _, r := range rbEv.Results {
		if r.Name == "nightly" && r.Result == "error" && r.Error == "changed since" {
			foundChangedSince = true
		}
		if r.Name == "api" && r.Result == "deployed" && r.To == img+":oldtag000001" {
			revertedCount++
		}
	}
	if !foundChangedSince || revertedCount != 2 {
		t.Fatalf("unexpected rollback results: %+v", rbEv.Results)
	}
}

func TestWorkloadsInventoryCacheAndUnreachableCluster(t *testing.T) {
	ctx := context.Background()
	de := fake.New()
	defer de.Close()
	sg := fake.New()

	de.Seed(map[string]any{
		"kind":     "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "app"},
		"spec": map[string]any{
			"replicas": 1,
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "web", "image": "ghcr.io/org/web:v1"}},
				},
			},
		},
		"status": map[string]any{"readyReplicas": 1},
	})

	deCl, _ := de.Client("de")
	sgCl, _ := sg.Client("sg")
	sg.Close() // Make sg unreachable!

	st, _ := state.NewFileStore(ctx, filepath.Join(t.TempDir(), "state.json"))
	eng, _ := New(&Config{Namespaces: []string{"app"}, Auth: AuthConfig{Disabled: true}}, []*kube.Client{deCl, sgCl}, st)
	defer eng.Close()

	inv1, err := eng.Workloads(ctx, false)
	if err != nil {
		t.Fatalf("Workloads: %v", err)
	}
	if !inv1.Clusters[0].Reachable || inv1.Clusters[1].Reachable {
		t.Fatalf("unexpected cluster reachability: %+v", inv1.Clusters)
	}
	if len(inv1.Workloads) != 1 {
		t.Fatalf("len(Workloads) = %d, want 1", len(inv1.Workloads))
	}

	// Add another deployment on de; cached call must return 1, fresh=true must return 2.
	de.Seed(map[string]any{
		"kind":     "Deployment",
		"metadata": map[string]any{"name": "worker", "namespace": "app"},
		"spec": map[string]any{
			"replicas": 1,
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "worker", "image": "ghcr.io/org/worker:v1"}},
				},
			},
		},
		"status": map[string]any{"readyReplicas": 1},
	})

	invCached, _ := eng.Workloads(ctx, false)
	if len(invCached.Workloads) != 1 {
		t.Fatalf("expected cached inventory (1 item), got %d", len(invCached.Workloads))
	}
	invFresh, _ := eng.Workloads(ctx, true)
	if len(invFresh.Workloads) != 2 {
		t.Fatalf("expected fresh inventory (2 items), got %d", len(invFresh.Workloads))
	}
}
