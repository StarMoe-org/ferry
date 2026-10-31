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

	"github.com/StarMoe-org/ferry/internal/kube"
	"github.com/StarMoe-org/ferry/internal/kube/fake"
	"github.com/StarMoe-org/ferry/internal/state"
)

const dnsZone = "dev.example.com"

// fakeGcore is the slice of Gcore's DNS API Ferry calls, one page at a time.
type fakeGcore struct {
	mu      sync.Mutex
	records map[string]string // name -> "TYPE ip"
	writes  []string
}

func (f *fakeGcore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "APIKey gc-tok" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if r.URL.Path == "/dns/v2/zones/"+dnsZone+"/rrsets" {
		type rec struct {
			Content []string `json:"content"`
		}
		sets := []map[string]any{}
		for name, v := range f.records {
			typ, ip, _ := strings.Cut(v, " ")
			sets = append(sets, map[string]any{"name": name, "type": typ, "resource_records": []rec{{Content: []string{ip}}}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"rrsets": sets, "total_amount": len(sets)})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/dns/v2/zones/"+dnsZone+"/"), "/")
	if len(parts) != 2 || parts[1] != "A" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	name := parts[0]
	_, exists := f.records[name]
	f.writes = append(f.writes, r.Method+" "+name)
	switch {
	case r.Method == http.MethodDelete && exists:
		delete(f.records, name)
	case r.Method == http.MethodPost && !exists, r.Method == http.MethodPut && exists:
		var body struct {
			RR []struct {
				Content []string `json:"content"`
			} `json:"resource_records"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.records[name] = "A " + body.RR[0].Content[0]
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (f *fakeGcore) takeWrites() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.writes
	f.writes = nil
	return out
}

func (f *fakeGcore) record(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[name]
}

func ingress(ns, name string, hosts ...string) map[string]any {
	rules := []any{}
	for _, h := range hosts {
		rules = append(rules, map[string]any{"host": h})
	}
	return map[string]any{
		"kind":     "Ingress",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     map[string]any{"rules": rules},
	}
}

func TestDNSSyncFollowsIngresses(t *testing.T) {
	ctx := context.Background()
	gc := &fakeGcore{records: map[string]string{
		dnsZone:                  "NS ns1.example.net",
		"*." + dnsZone:           "A 10.0.0.1",
		"handmade." + dnsZone:    "A 203.0.113.9",
		"old." + dnsZone:         "A 10.0.0.2",
		"moved." + dnsZone:       "A 10.0.0.2",
		"unreachable." + dnsZone: "A 10.0.0.1",
	}}
	api := httptest.NewServer(gc)
	defer api.Close()

	a, b := fake.New(), fake.New()
	defer a.Close()
	defer b.Close()
	a.Seed(
		ingress("dev", "web", "web."+dnsZone, "shop.example.com"),
		ingress("dev", "moved", "moved."+dnsZone),
		ingress("apps", "x", "unreachable."+dnsZone),
	)
	b.Seed(ingress("dev", "api", "api."+dnsZone))
	clA, _ := a.Client("a")
	clB, _ := b.Client("b")

	tmp := t.TempDir()
	st, err := state.NewFileStore(ctx, filepath.Join(tmp, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		WorkDir:    filepath.Join(tmp, "work"),
		Namespaces: []string{"dev"},
		Auth:       AuthConfig{Disabled: true},
		DNS: DNSConfig{
			Targets: map[string]string{"a": "10.0.0.1", "b": "10.0.0.2"},
			Gcore:   GcoreConfig{API: api.URL, Token: "gc-tok"},
		},
	}
	eng, err := New(cfg, []*kube.Client{clA, clB}, st)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	// Zone left out above so the engine starts no background loop; the test drives each pass.
	cfg.DNS.Zone = dnsZone
	d := &dnsSyncer{eng: eng, gcore: newGcoreDNS(cfg.DNS), ctx: ctx, problems: map[string]string{}}

	d.sync(ctx)
	for name, want := range map[string]string{
		"web." + dnsZone:      "A 10.0.0.1",
		"api." + dnsZone:      "A 10.0.0.2",
		"moved." + dnsZone:    "A 10.0.0.1", // a's now
		"handmade." + dnsZone: "A 203.0.113.9",
		"*." + dnsZone:        "A 10.0.0.1",
		dnsZone:               "NS ns1.example.net",
	} {
		if got := gc.record(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := gc.record("old." + dnsZone); got != "" {
		t.Errorf("old record kept: %q", got)
	}
	if got := gc.record("shop.example.com"); got != "" {
		t.Errorf("host outside the zone got a record: %q", got)
	}
	if len(gc.takeWrites()) != 4 {
		t.Fatal("first pass should make four writes")
	}

	// Nothing changed: nothing written.
	d.sync(ctx)
	if w := gc.takeWrites(); len(w) != 0 {
		t.Fatalf("second pass wrote %v", w)
	}

	// One cluster can't be read: its host disappears from view, and nothing may be deleted.
	a.SetForbidden(http.MethodGet, "ingresses", true)
	d.sync(ctx)
	if w := gc.takeWrites(); len(w) != 0 {
		t.Fatalf("pass with an unreadable cluster wrote %v", w)
	}
	a.SetForbidden(http.MethodGet, "ingresses", false)

	// The same host on two clusters: the record stays where it was.
	b.Seed(ingress("dev", "dup", "web."+dnsZone))
	d.sync(ctx)
	if w := gc.takeWrites(); len(w) != 0 {
		t.Fatalf("conflict wrote %v", w)
	}
	if got := d.problems["web."+dnsZone]; !strings.Contains(got, "several clusters") {
		t.Fatalf("conflict not reported: %q", got)
	}
}

func TestDNSConfigValidation(t *testing.T) {
	base := `
auth: {disabled: true}
state: {file: "/tmp/s.json"}
namespaces: ["app"]
clusters:
  - {name: de, inCluster: true}
`
	if _, err := ParseConfig([]byte(base + `dns: {zone: dev.example.com, targets: {de: 1.2.3.4}, gcore: {token: t}}`)); err != nil {
		t.Fatalf("valid dns config: %v", err)
	}
	for name, dns := range map[string]string{
		"no token":        `dns: {zone: dev.example.com, targets: {de: 1.2.3.4}}`,
		"no targets":      `dns: {zone: dev.example.com, gcore: {token: t}}`,
		"unknown cluster": `dns: {zone: dev.example.com, targets: {us: 1.2.3.4}, gcore: {token: t}}`,
		"bad address":     `dns: {zone: dev.example.com, targets: {de: "1.2.3"}, gcore: {token: t}}`,
	} {
		if _, err := ParseConfig([]byte(base + dns)); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}
