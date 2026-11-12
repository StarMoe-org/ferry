package ferry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/StarMoe-org/ferry/internal/kube"
	"github.com/StarMoe-org/ferry/internal/kube/fake"
	"github.com/StarMoe-org/ferry/internal/state"
)

// hungCluster is an API server that accepts requests and never answers them, like an edge node
// whose port still takes connections after the machine behind it is gone.
func hungCluster(t *testing.T, name string) *kube.Client {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	cl, err := kube.New(kube.Config{Name: name, Server: srv.URL, Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

func deploymentRunning(name, ns, image string) map[string]any {
	return map[string]any{
		"kind":     "Deployment",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": name, "image": image}},
				},
			},
		},
	}
}

func TestDeployDoesNotWaitForAHungCluster(t *testing.T) {
	ctx := context.Background()
	const img = "ghcr.io/org/notes"
	good := fake.New()
	defer good.Close()
	good.Seed(deploymentRunning("notes", "app", img+":old"))
	goodCl, _ := good.Client("jp1")

	st, err := state.NewFileStore(ctx, filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{WorkDir: t.TempDir(), Namespaces: []string{"app", "dev"}, Auth: AuthConfig{Disabled: true}}
	eng, err := New(cfg, []*kube.Client{hungCluster(t, "tl"), goodCl}, st)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	eng.clusterTimeout = 300 * time.Millisecond

	start := time.Now()
	results, ev, err := eng.Deploy(ctx, "ci", img, "new", "test", false)
	if err != nil {
		t.Fatal(err)
	}
	// Eight list calls on the hung cluster run at once under one deadline, not one after another.
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("deploy took %s with one cluster hung", took)
	}
	var deployed, hungErrs int
	for _, r := range results {
		switch {
		case r.Cluster == "jp1" && r.Result == "deployed":
			deployed++
		case r.Cluster == "tl" && r.Result == "error":
			hungErrs++
			if !strings.Contains(r.Error, "cluster tl did not answer within") {
				t.Errorf("hung cluster error = %q", r.Error)
			}
		default:
			t.Errorf("unexpected result %+v", r)
		}
	}
	if deployed != 1 || hungErrs != 2*len(workloadKinds) {
		t.Fatalf("deployed=%d hungErrs=%d, results %+v", deployed, hungErrs, results)
	}
	obj, _ := good.GetObject("deployments", "app", "notes")
	if cs := extractContainers(obj, "/spec/template/spec"); len(cs) != 1 || cs[0].image != img+":new" {
		t.Fatalf("healthy cluster was not updated: %+v", cs)
	}
	if ev == nil || ev.Status != "partial" {
		t.Fatalf("event = %+v, want partial", ev)
	}

	// The dashboard inventory is bounded the same way.
	start = time.Now()
	inv, err := eng.Workloads(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("inventory took %s with one cluster hung", took)
	}
	if inv.Clusters[0].Reachable || !inv.Clusters[1].Reachable || len(inv.Workloads) != 1 {
		t.Fatalf("inventory = %+v", inv)
	}
}

func TestDNSSyncHungClusterLeavesOthersAndKeepsItsRecords(t *testing.T) {
	ctx := context.Background()
	gc := &fakeGcore{records: map[string]string{
		"gone-node-app." + dnsZone: "A 10.0.0.9", // served by the hung cluster: must survive
		"api." + dnsZone:           "A 10.0.0.1", // moving to b
	}}
	api := httptest.NewServer(gc)
	defer api.Close()

	b := fake.New()
	defer b.Close()
	b.Seed(ingress("dev", "api", "api."+dnsZone), ingress("dev", "web", "web."+dnsZone))
	clB, _ := b.Client("b")

	st, err := state.NewFileStore(ctx, filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		WorkDir:    t.TempDir(),
		Namespaces: []string{"dev"},
		Auth:       AuthConfig{Disabled: true},
		DNS: DNSConfig{
			Targets: map[string]string{"tl": "10.0.0.9", "a": "10.0.0.1", "b": "10.0.0.2"},
			Gcore:   GcoreConfig{API: api.URL, Token: "gc-tok"},
		},
	}
	// The hung cluster comes first: before, it used up the whole pass and b never got its turn.
	eng, err := New(cfg, []*kube.Client{hungCluster(t, "tl"), clB}, st)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	cfg.DNS.Zone = dnsZone
	d := &dnsSyncer{eng: eng, gcore: newGcoreDNS(cfg.DNS), ctx: ctx, problems: map[string]string{}, clusterTimeout: 200 * time.Millisecond}

	passCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	d.sync(passCtx)

	for name, want := range map[string]string{
		"api." + dnsZone:           "A 10.0.0.2",
		"web." + dnsZone:           "A 10.0.0.2",
		"gone-node-app." + dnsZone: "A 10.0.0.9",
	} {
		if got := gc.record(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := d.problems["cluster/tl"]; !strings.Contains(got, "no answer within") {
		t.Fatalf("hung cluster not reported: %q", got)
	}
}

func TestSnapshotSkipsFailedClusterAndKeepsItsDirectory(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	snapBare := filepath.Join(tmp, "clusters.git")
	if err := os.MkdirAll(snapBare, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, snapBare, "init", "--bare", "-b", "main")
	seed := filepath.Join(tmp, "seed")
	for _, c := range []string{"jp1", "sg", "tl"} {
		if err := os.MkdirAll(filepath.Join(seed, "snapshots", c), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(seed, "snapshots", c, "app.yaml"), []byte("# previous "+c+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, seed, "init", "-b", "main")
	mustGit(t, seed, "-c", "user.name=Init", "-c", "user.email=init@example.com", "add", "-A")
	mustGit(t, seed, "-c", "user.name=Init", "-c", "user.email=init@example.com", "commit", "-m", "previous")
	mustGit(t, seed, "push", snapBare, "HEAD:refs/heads/main")

	jp1 := fake.New()
	defer jp1.Close()
	jp1.Seed(map[string]any{
		"kind":     "ConfigMap",
		"metadata": map[string]any{"name": "notes-config", "namespace": "app"},
		"data":     map[string]any{"k": "v"},
	})
	sg := fake.New()
	defer sg.Close()
	sg.SetForbidden(http.MethodGet, "secrets", true) // one kind fails: the whole cluster is left as it was
	jp1Cl, _ := jp1.Client("jp1")
	sgCl, _ := sg.Client("sg")

	st, _ := state.NewFileStore(ctx, filepath.Join(tmp, "state.json"))
	cfg := &Config{
		WorkDir:    filepath.Join(tmp, "work"),
		Namespaces: []string{"app"},
		Auth:       AuthConfig{Disabled: true},
		Snapshot:   SnapshotConfig{Repo: snapBare, Branch: "main", Dir: "snapshots"},
	}
	eng, err := New(cfg, []*kube.Client{hungCluster(t, "tl"), jp1Cl, sgCl}, st)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	eng.snapshotClusterTimeout = 300 * time.Millisecond

	res, err := eng.Snapshot(ctx, "test")
	if err != nil {
		t.Fatalf("snapshot with two failed clusters: %v", err)
	}
	if res.Status != "committed" || len(res.Failed) != 2 ||
		!strings.HasPrefix(res.Failed[0], "tl (") || !strings.HasPrefix(res.Failed[1], "sg (") {
		t.Fatalf("result = %+v", res)
	}

	check := filepath.Join(tmp, "check")
	mustGit(t, tmp, "clone", "-q", snapBare, check)
	read := func(c string) string {
		b, err := os.ReadFile(filepath.Join(check, "snapshots", c, "app.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := read("tl"); got != "# previous tl\n" {
		t.Errorf("hung cluster's snapshot changed: %q", got)
	}
	if got := read("sg"); got != "# previous sg\n" {
		t.Errorf("failed cluster's snapshot changed: %q", got)
	}
	if got := read("jp1"); !strings.Contains(got, "notes-config") {
		t.Errorf("healthy cluster not snapshotted: %q", got)
	}

	evs := st.Events(state.EventsFilter{Kind: "snapshot", Limit: 1})
	if len(evs) != 1 || evs[0].Status != "partial" ||
		!strings.Contains(evs[0].Message, "committed without tl (") || !strings.Contains(evs[0].Message, "sg (") {
		t.Fatalf("snapshot event = %+v", evs)
	}

	// With no cluster readable there is nothing worth committing.
	jp1.SetForbidden(http.MethodGet, "configmaps", true)
	if _, err := eng.Snapshot(ctx, "test"); err == nil || !strings.Contains(err.Error(), "no cluster could be read") {
		t.Fatalf("all clusters failed: err = %v", err)
	}
}
