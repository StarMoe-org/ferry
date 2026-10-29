package ferry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/StarMoe-org/ferry/internal/kube"
	"github.com/StarMoe-org/ferry/internal/kube/fake"
	"github.com/StarMoe-org/ferry/internal/state"
)

// fakeCNBStatus serves GET /org/builder/-/build/status/<sn> from a per-sn script of statuses;
// the last entry repeats.
func fakeCNBStatus(t *testing.T, script map[string][]string) (*httptest.Server, func(sn string) int) {
	t.Helper()
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := strings.CutPrefix(r.URL.Path, "/org/builder/-/build/status/")
		if !ok || r.Header.Get("Authorization") != "Bearer cnb-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		steps := script[sn]
		i := calls[sn]
		calls[sn]++
		mu.Unlock()
		if len(steps) == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if i >= len(steps) {
			i = len(steps) - 1
		}
		if steps[i] == "500" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		stage := "success"
		if steps[i] == "error" {
			stage = "error"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": steps[i],
			"pipelinesStatus": map[string]any{
				sn + "-001": map[string]any{"stages": []any{
					map[string]any{"id": "stage-0", "name": "docker build", "status": stage},
				}},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, func(sn string) int {
		mu.Lock()
		defer mu.Unlock()
		return calls[sn]
	}
}

func waitBuildStatus(t *testing.T, st *state.Store, id string, want ...string) state.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ev, _ := st.GetEvent(id)
		for _, w := range want {
			if ev.Status == w {
				return ev
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	ev, _ := st.GetEvent(id)
	t.Fatalf("build %s: status %q (%q), want one of %v", id, ev.Status, ev.Message, want)
	return ev
}

func TestCNBBuildWatchDeploysOnSuccessFailsOnErrorAndResumes(t *testing.T) {
	ctx := context.Background()
	const img = "docker.cnb.cool/org/builder/web"
	const commit = "0123456789abcdef0123456789abcdef01234567"

	de := fake.New()
	defer de.Close()
	de.Seed(map[string]any{
		"kind":     "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "app"},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "web", "image": img + ":old000000000"}},
		}}},
	})
	kCl, _ := de.Client("de")

	cnb, calls := fakeCNBStatus(t, map[string][]string{
		"sn-ok":      {"pending", "500", "running", "success"},
		"sn-fail":    {"running", "error"},
		"sn-resumed": {"success"},
		"sn-stuck":   {"running"},
	})

	tmp := t.TempDir()
	st, err := state.NewFileStore(ctx, filepath.Join(tmp, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	build := func(sn, tagCommit string) state.Event {
		ev, err := st.AppendEvent(ctx, state.Event{
			Kind: "build", Actor: "github", Image: img, Tag: tagCommit[:12], Commit: tagCommit,
			Source: "github:org/web@" + tagCommit[:12], Status: "started", BuildSN: sn,
		})
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	// Left "started" by an earlier run: picked up when the engine starts.
	resumed := build("sn-resumed", commit)

	cfg := &Config{
		WorkDir:    filepath.Join(tmp, "work"),
		Namespaces: []string{"app"},
		Auth:       AuthConfig{Disabled: true},
		CNB:        CNBConfig{Repo: "org/builder", Token: "cnb-tok", ImagePrefix: "docker.cnb.cool/org/builder", API: cnb.URL},
	}
	cfg.ApplyDefaults()
	// Poll fast; the resumed watch starts inside the constructor.
	eng, err := newEngineWithPolling(cfg, []*kube.Client{kCl}, st, 5*time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	// The resumed build succeeded: its image is deployed and the build settles as deployed.
	ev := waitBuildStatus(t, st, resumed.ID, "ok", "error", "partial")
	if ev.Status != "ok" || ev.Message != "deployed" {
		t.Fatalf("resumed build = %q %q, want ok deployed", ev.Status, ev.Message)
	}
	obj, _ := de.GetObject("deployments", "app", "web")
	if got := extractContainers(obj, "/spec/template/spec")[0].image; got != img+":"+commit[:12] {
		t.Fatalf("web image = %q", got)
	}
	deps := st.Events(state.EventsFilter{Kind: "deploy"})
	if len(deps) != 1 || deps[0].Actor != "cnb" || deps[0].Source != "https://github.com/org/web/commit/"+commit {
		t.Fatalf("deploy events = %+v", deps)
	}

	// Polling rides out pending/running and a 500, then deploys.
	commit2 := "fedcba9876543210fedcba9876543210fedcba98"
	okEv := build("sn-ok", commit2)
	eng.buildQueue.watch(okEv)
	if ev := waitBuildStatus(t, st, okEv.ID, "ok", "error"); ev.Status != "ok" || ev.Message != "deployed" {
		t.Fatalf("sn-ok = %q %q", ev.Status, ev.Message)
	}
	if n := calls("sn-ok"); n < 4 {
		t.Fatalf("sn-ok polled %d times, want >= 4", n)
	}

	// A failed build names the stage and deploys nothing.
	commit3 := "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddd"
	failEv := build("sn-fail", commit3)
	eng.buildQueue.watch(failEv)
	ev = waitBuildStatus(t, st, failEv.ID, "error", "ok")
	if ev.Status != "error" || !strings.Contains(ev.Message, "docker build") {
		t.Fatalf("sn-fail = %q %q", ev.Status, ev.Message)
	}
	if n := len(st.Events(state.EventsFilter{Kind: "deploy"})); n != 2 {
		t.Fatalf("deploy events after failed build = %d, want 2", n)
	}

	// A build already settled by someone else's deploy is left alone.
	commit4 := "1111111111112222222222223333333333334444"
	settled := build("sn-ok", commit4)
	if _, err := st.UpdateEvent(ctx, settled.ID, func(e *state.Event) { e.Status = "ok"; e.Message = "deployed" }); err != nil {
		t.Fatal(err)
	}
	eng.buildQueue.watch(settled)
	time.Sleep(50 * time.Millisecond)
	if n := len(st.Events(state.EventsFilter{Kind: "deploy"})); n != 2 {
		t.Fatalf("settled build deployed again: %d deploy events", n)
	}

	// A build that never ends gives up at the deadline.
	stuck := build("sn-stuck", "5555555555556666666666667777777777778888")
	stuck.At = time.Now().Add(-time.Second)
	eng.buildQueue.watch(stuck)
	if ev := waitBuildStatus(t, st, stuck.ID, "error"); !strings.Contains(ev.Message, "gave up") {
		t.Fatalf("stuck = %q", ev.Message)
	}
}

func TestCNBBuildWatchWithoutDeployOnSuccess(t *testing.T) {
	ctx := context.Background()
	de := fake.New()
	defer de.Close()
	kCl, _ := de.Client("de")
	cnb, _ := fakeCNBStatus(t, map[string][]string{"sn": {"success"}})
	tmp := t.TempDir()
	st, _ := state.NewFileStore(ctx, filepath.Join(tmp, "state.json"))
	off := false
	cfg := &Config{
		WorkDir: filepath.Join(tmp, "work"), Namespaces: []string{"app"}, Auth: AuthConfig{Disabled: true},
		CNB: CNBConfig{Repo: "org/builder", Token: "cnb-tok", API: cnb.URL, DeployOnSuccess: &off},
	}
	cfg.ApplyDefaults()
	eng, err := newEngineWithPolling(cfg, []*kube.Client{kCl}, st, 5*time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	ev, _ := st.AppendEvent(ctx, state.Event{Kind: "build", Image: "docker.cnb.cool/org/builder/x",
		Commit: "0123456789abcdef0123456789abcdef01234567", Status: "started", BuildSN: "sn"})
	eng.buildQueue.watch(ev)
	if got := waitBuildStatus(t, st, ev.ID, "ok", "error"); got.Message != "built" {
		t.Fatalf("message = %q, want built", got.Message)
	}
	if n := len(st.Events(state.EventsFilter{Kind: "deploy"})); n != 0 {
		t.Fatalf("deployOnSuccess=false still deployed (%d events)", n)
	}
}
