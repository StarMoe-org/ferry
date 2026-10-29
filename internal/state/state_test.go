package state

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/StarMoe-org/ferry/internal/kube/fake"
)

func TestFileStoreLifecycleTokensAndSnapshotSafety(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.json")

	st, err := NewFileStore(ctx, path)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 0600", fi.Mode().Perm())
	}

	// Seed once; deleting should soft-delete and prevent re-seeding.
	if err := st.SeedIfEmpty(ctx, []SeedInput{
		{Repo: "org/app", Branch: "main", Image: "app", BuildArgs: "FOO=1"},
	}); err != nil {
		t.Fatalf("SeedIfEmpty: %v", err)
	}
	srcs := st.Sources()
	if len(srcs) != 1 || srcs[0].ImageRef("docker.cnb.cool/org/builder") != "docker.cnb.cool/org/builder/app" {
		t.Fatalf("sources = %+v", srcs)
	}
	if err := st.DeleteSource(ctx, srcs[0].ID); err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}
	if len(st.Sources()) != 0 {
		t.Fatalf("expected 0 active sources after soft-delete")
	}
	if err := st.SeedIfEmpty(ctx, []SeedInput{{Repo: "org/other", Branch: "main", Image: "other"}}); err != nil {
		t.Fatalf("SeedIfEmpty second call: %v", err)
	}
	if len(st.Sources()) != 0 {
		t.Fatalf("expected re-seed skipped because soft-deleted source exists")
	}

	// Create external source and token; verify plaintext never persisted and ferry.json never includes tokens.
	ext, err := st.CreateSource(ctx, Source{
		Repo:    "org/ext",
		Branch:  "main",
		Builder: "external",
		Image:   "ghcr.io/org/ext",
	})
	if err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if ext.ImageRef("docker.cnb.cool/org/builder") != "ghcr.io/org/ext" {
		t.Fatalf("external ImageRef = %q", ext.ImageRef("docker.cnb.cool/org/builder"))
	}

	info, plain, err := st.CreateToken(ctx, "github-actions", "alice@example.com")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if !strings.HasPrefix(plain, "fy_") || info.Prefix != plain[:10] {
		t.Fatalf("unexpected token: plain=%q info=%+v", plain, info)
	}
	rawDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawDisk), plain) {
		t.Fatalf("plaintext token leaked into state file!")
	}
	snapJSON, err := st.SourcesSnapshotJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(snapJSON), "tokens") || strings.Contains(string(snapJSON), info.ID) {
		t.Fatalf("ferry.json snapshot leaked token metadata: %s", snapJSON)
	}

	// Authenticate token and check LastUsed.
	authed, ok := st.AuthenticateToken(ctx, plain)
	if !ok || authed.Name != "github-actions" || authed.LastUsed == nil {
		t.Fatalf("AuthenticateToken = %+v, %v", authed, ok)
	}
	if _, ok := st.AuthenticateToken(ctx, "fy_wrong"); ok {
		t.Fatal("expected wrong token rejected")
	}

	// Reload from disk and revoke.
	st2, err := NewFileStore(ctx, path)
	if err != nil {
		t.Fatalf("reload FileStore: %v", err)
	}
	if _, ok := st2.AuthenticateToken(ctx, plain); !ok {
		t.Fatal("expected token valid after reload")
	}
	if err := st2.RevokeToken(ctx, info.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, ok := st2.AuthenticateToken(ctx, plain); ok {
		t.Fatal("expected token invalid after revoke")
	}
}

func TestConfigMapStoreConflictRetryAndBuildLinking(t *testing.T) {
	ctx := context.Background()
	srv := fake.New()
	defer srv.Close()

	cl, err := srv.Client("de")
	if err != nil {
		t.Fatal(err)
	}

	stA, err := NewConfigMapStore(ctx, cl, "app", "ferry-state")
	if err != nil {
		t.Fatalf("NewConfigMapStore A: %v", err)
	}
	stB, err := NewConfigMapStore(ctx, cl, "app", "ferry-state")
	if err != nil {
		t.Fatalf("NewConfigMapStore B: %v", err)
	}

	// Concurrent writer B updates ConfigMap first; A has stale resourceVersion and must retry cleanly without losing B's source.
	if _, err := stB.CreateSource(ctx, Source{
		Repo:    "org/from-b",
		Branch:  "main",
		Builder: "cnb",
		Image:   "from-b",
	}); err != nil {
		t.Fatalf("stB.CreateSource: %v", err)
	}

	srv.InjectConflictOnce()
	if _, err := stA.CreateSource(ctx, Source{
		Repo:    "org/from-a",
		Branch:  "main",
		Builder: "cnb",
		Image:   "from-a",
	}); err != nil {
		t.Fatalf("stA.CreateSource after conflict: %v", err)
	}
	gotSources := stA.Sources()
	if len(gotSources) != 2 {
		t.Fatalf("expected both sources preserved across 409 retry, got %+v", gotSources)
	}

	// Build-to-deploy linking + 300-event cap.
	buildEv, err := stA.AppendEvent(ctx, Event{
		Kind:   "build",
		Actor:  "github",
		Image:  "docker.cnb.cool/org/builder/app",
		Commit: "abcdef1234567890fedcba0987654321abcdef12",
		Status: "started",
		LogURL: "https://cnb.cool/org/builder/-/build/logs/b1",
	})
	if err != nil {
		t.Fatalf("AppendEvent build: %v", err)
	}

	if _, err := stA.RecordDeployEvent(ctx, Event{
		Kind:   "deploy",
		Actor:  "ci",
		Image:  "docker.cnb.cool/org/builder/app",
		Tag:    "abcdef123456",
		Status: "ok",
	}); err != nil {
		t.Fatalf("RecordDeployEvent: %v", err)
	}

	linked, ok := stA.GetEvent(buildEv.ID)
	if !ok || linked.Status != "ok" || linked.Message != "deployed" {
		t.Fatalf("expected build event linked to deploy, got %+v (ok=%v)", linked, ok)
	}

	for i := 0; i < 310; i++ {
		if _, err := stA.AppendEvent(ctx, Event{Kind: "snapshot", Actor: "schedule", Status: "ok"}); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}
	all := stA.Events(EventsFilter{})
	if len(all) != MaxEvents {
		t.Fatalf("len(events) = %d, want %d", len(all), MaxEvents)
	}
}
