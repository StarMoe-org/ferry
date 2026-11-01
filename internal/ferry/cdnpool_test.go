package ferry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/StarMoe-org/ferry/internal/kube"
	"github.com/StarMoe-org/ferry/internal/kube/fake"
	"github.com/StarMoe-org/ferry/internal/state"
)

const (
	cdnCNAME = "cl-test.gcdn.example"
	certName = "ferry-pool-" + dnsZone
)

var cdnEpoch = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// fakeGcoreCDN is Gcore's DNS and CDN APIs as Ferry sees them, with the limits measured on the real
// one: ten secondary hostnames, a hostname on one resource at a time, no CNAME beside an A record.
type fakeGcoreCDN struct {
	mu        sync.Mutex
	now       time.Time
	records   map[string]string // "TYPE name" -> content
	resources []*fakeResource
	certs     []*fakeCert
	nextID    int64
	writes    []string
	fail      map[string]string // "METHOD path" -> error body
	bareLists bool
}

type fakeResource struct {
	cdnResource
	body map[string]any
}

type fakeCert struct {
	ID       int64
	Name     string
	NotAfter string
	Cert     string
}

func newFakeGcoreCDN() *fakeGcoreCDN {
	return &fakeGcoreCDN{now: cdnEpoch, records: map[string]string{}, nextID: 100, fail: map[string]string{}}
}

func (f *fakeGcoreCDN) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "APIKey gc-tok" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
	}
	if body, ok := f.fail[r.Method+" "+r.URL.Path]; ok {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case strings.HasPrefix(r.URL.Path, "/dns/v2/zones/"+dnsZone+"/"):
		f.serveDNS(w, r, body)
	case r.URL.Path == "/cdn/clients/me":
		writeJSON(w, map[string]any{"cname": cdnCNAME})
	case r.URL.Path == "/cdn/resources" && r.Method == http.MethodGet:
		items := make([]cdnResource, 0, len(f.resources))
		for _, res := range f.resources {
			items = append(items, res.cdnResource)
		}
		f.writeList(w, items)
	case r.URL.Path == "/cdn/resources" && r.Method == http.MethodPost:
		f.nextID++
		res := &fakeResource{body: body, cdnResource: cdnResource{
			ID: f.nextID, Name: body["name"].(string), Primary: body["cname"].(string), Hostnames: []string{},
			Active: true, Status: "processed", Created: f.now.Format(time.RFC3339),
		}}
		f.resources = append(f.resources, res)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, res.cdnResource)
	case strings.HasPrefix(r.URL.Path, "/cdn/resources/") && r.Method == http.MethodPatch:
		f.patchResource(w, r, body)
	case r.URL.Path == "/cdn/sslData" && r.Method == http.MethodGet:
		items := make([]sslData, 0, len(f.certs))
		for _, c := range f.certs {
			items = append(items, sslData{ID: c.ID, Name: c.Name, NotAfter: c.NotAfter})
		}
		f.writeList(w, items)
	case r.URL.Path == "/cdn/sslData" && r.Method == http.MethodPost:
		f.nextID++
		f.certs = append(f.certs, &fakeCert{ID: f.nextID, Name: body["name"].(string), NotAfter: f.now.Add(90 * 24 * time.Hour).Format(time.RFC3339), Cert: body["sslCertificate"].(string)})
		w.WriteHeader(http.StatusCreated)
	case strings.HasPrefix(r.URL.Path, "/cdn/sslData/") && r.Method == http.MethodPut:
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/cdn/sslData/"), 10, 64)
		for _, c := range f.certs {
			if c.ID == id {
				c.Cert = body["sslCertificate"].(string)
				c.NotAfter = f.now.Add(90 * 24 * time.Hour).Format(time.RFC3339)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// writeList answers the way the CDN API does: bare unless asked for a page.
func (f *fakeGcoreCDN) writeList(w http.ResponseWriter, items any) {
	if f.bareLists {
		writeJSON(w, items)
		return
	}
	writeJSON(w, map[string]any{"count": 0, "next": nil, "previous": nil, "results": items})
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGcoreCDN) serveDNS(w http.ResponseWriter, r *http.Request, body map[string]any) {
	if strings.HasSuffix(r.URL.Path, "/rrsets") {
		sets := []map[string]any{}
		for key, content := range f.records {
			typ, name, _ := strings.Cut(key, " ")
			sets = append(sets, map[string]any{"name": name, "type": typ, "resource_records": []map[string]any{{"content": []string{content}}}})
		}
		writeJSON(w, map[string]any{"rrsets": sets, "total_amount": len(sets)})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/dns/v2/zones/"+dnsZone+"/"), "/")
	if len(parts) != 2 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	name, typ := parts[0], parts[1]
	key := typ + " " + name
	_, exists := f.records[key]
	switch {
	case r.Method == http.MethodDelete && exists:
		delete(f.records, key)
	case r.Method == http.MethodPost && !exists:
		other := "A " + name
		if typ == "A" {
			other = "CNAME " + name
		}
		if _, clash := f.records[other]; clash {
			w.WriteHeader(http.StatusConflict)
			return
		}
		rec := body["resource_records"].([]any)[0].(map[string]any)
		f.records[key] = rec["content"].([]any)[0].(string)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (f *fakeGcoreCDN) patchResource(w http.ResponseWriter, r *http.Request, body map[string]any) {
	id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/cdn/resources/"), 10, 64)
	var res *fakeResource
	for _, c := range f.resources {
		if c.ID == id {
			res = c
		}
	}
	list, isList := body["secondaryHostnames"].([]any)
	if res == nil || !isList || len(body) != 1 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(list) > 10 {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"You cannot add more than 10 additional CNAMEs."}`))
		return
	}
	next := []string{}
	for _, h := range list {
		for _, other := range f.resources {
			if other != res && slices.Contains(other.Hostnames, h.(string)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		next = append(next, h.(string))
	}
	res.Hostnames = next
	writeJSON(w, res.cdnResource)
}

func (f *fakeGcoreCDN) addResource(name string, age time.Duration, hosts ...string) *fakeResource {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	res := &fakeResource{cdnResource: cdnResource{
		ID: f.nextID, Name: name, Primary: name + "." + dnsZone, Hostnames: append([]string{}, hosts...),
		Active: true, Status: "active", Created: f.now.Add(-age).Format("2006-01-02T15:04:05.000000Z"),
	}}
	f.resources = append(f.resources, res)
	return res
}

func (f *fakeGcoreCDN) addCert(notAfter time.Time) *fakeCert {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	c := &fakeCert{ID: f.nextID, Name: certName, NotAfter: notAfter.Format(time.RFC3339), Cert: "old"}
	f.certs = append(f.certs, c)
	return c
}

func (f *fakeGcoreCDN) takeWrites() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.writes
	f.writes = nil
	return out
}

func (f *fakeGcoreCDN) hostnames(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.resources {
		if r.Name == name {
			return slices.Clone(r.Hostnames)
		}
	}
	return nil
}

func (f *fakeGcoreCDN) resourceNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.resources {
		out = append(out, r.Name)
	}
	return out
}

func (f *fakeGcoreCDN) record(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[key]
}

func (f *fakeGcoreCDN) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// fakeIssuer stands in for Let's Encrypt.
type fakeIssuer struct {
	orders []string
	err    error
}

func (f *fakeIssuer) Issue(_ context.Context, domain string) (string, string, error) {
	f.orders = append(f.orders, domain)
	if f.err != nil {
		return "", "", f.err
	}
	return fmt.Sprintf("cert-%d", len(f.orders)), "key", nil
}

type cdnEnv struct {
	gc     *fakeGcoreCDN
	a, b   *fake.Server
	d      *dnsSyncer
	issuer *fakeIssuer
}

// newCDNEnv has two clusters and a syncer the test drives. With pool set, Gcore already holds a good
// certificate and a live, empty resource for each cluster.
func newCDNEnv(t *testing.T, pool bool) *cdnEnv {
	t.Helper()
	ctx := context.Background()
	gc := newFakeGcoreCDN()
	api := httptest.NewServer(gc)
	t.Cleanup(api.Close)
	a, b := fake.New(), fake.New()
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
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
			TTL:     120,
			Gcore:   GcoreConfig{API: api.URL, Token: "gc-tok"},
			CDN:     CDNConfig{Enabled: true, Prefix: "ferry-pool", MinFree: 2, ACMEEmail: "ops@example.com"},
		},
	}
	eng, err := New(cfg, []*kube.Client{clA, clB}, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Close)
	// Zone left out above so the engine starts no background loop; the test drives each pass.
	cfg.DNS.Zone = dnsZone
	issuer := &fakeIssuer{}
	d := &dnsSyncer{eng: eng, gcore: newGcoreDNS(cfg.DNS), ctx: ctx, problems: map[string]string{}}
	d.cdn = newCDNPool(cfg.DNS, d.gcore, issuer)
	d.cdn.now = func() time.Time {
		gc.mu.Lock()
		defer gc.mu.Unlock()
		return gc.now
	}
	if pool {
		gc.addCert(cdnEpoch.Add(60 * 24 * time.Hour))
		gc.addResource("ferry-pool-a-1", 24*time.Hour)
		gc.addResource("ferry-pool-b-1", 24*time.Hour)
	}
	return &cdnEnv{gc: gc, a: a, b: b, d: d, issuer: issuer}
}

func (e *cdnEnv) sync() {
	e.d.sync(context.Background())
}

func host(label string) string { return label + "." + dnsZone }

func hostPath(typ, label string) string {
	return "/dns/v2/zones/" + dnsZone + "/" + host(label) + "/" + typ
}

func (f *fakeGcoreCDN) resourcePath(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.resources {
		if r.Name == name {
			return fmt.Sprintf("/cdn/resources/%d", r.ID)
		}
	}
	return ""
}

func TestCDNBuildsPoolThenMovesARecords(t *testing.T) {
	e := newCDNEnv(t, false)
	e.gc.records["A "+host("web")] = "10.0.0.1"
	e.a.Seed(ingress("dev", "web", host("web")))

	// The certificate comes first, on a pass of its own.
	e.sync()
	if w := e.gc.takeWrites(); !slices.Equal(w, []string{"POST /cdn/sslData"}) {
		t.Fatalf("first pass wrote %v", w)
	}
	if !slices.Equal(e.issuer.orders, []string{"*." + dnsZone}) {
		t.Fatalf("orders = %v", e.issuer.orders)
	}

	// Then a resource for each cluster, while the old record keeps serving.
	e.sync()
	if w := e.gc.takeWrites(); !slices.Equal(w, []string{"POST /cdn/resources", "POST /cdn/resources"}) {
		t.Fatalf("second pass wrote %v", w)
	}
	if got := e.gc.resourceNames(); !slices.Equal(got, []string{"ferry-pool-a-1", "ferry-pool-b-1"}) {
		t.Fatalf("resources = %v", got)
	}
	res := e.gc.resources[0]
	cert := e.gc.certs[0]
	if res.Primary != "ferry-pool-a-1."+dnsZone || res.body["origin"] != "10.0.0.1" || res.body["originProtocol"] != "HTTPS" ||
		res.body["sslEnabled"] != true || res.body["sslData"] != float64(cert.ID) {
		t.Fatalf("resource body = %v", res.body)
	}
	opts := res.body["options"].(map[string]any)
	for _, opt := range []string{"forward_host_header", "websockets", "redirect_http_to_https", "edge_cache_settings", "browser_cache_settings", "proxy_read_timeout"} {
		if on, _ := opts[opt].(map[string]any); on["enabled"] != true {
			t.Errorf("option %s = %v", opt, opts[opt])
		}
	}
	if got := e.d.problems[host("web")]; !strings.Contains(got, "waiting") {
		t.Fatalf("waiting host not reported: %q", got)
	}
	if got := e.gc.record("A " + host("web")); got != "10.0.0.1" {
		t.Fatalf("A record gone during warm-up: %q", got)
	}

	// Still warming: nothing more to do, and no second resource.
	e.gc.advance(10 * time.Minute)
	e.sync()
	if w := e.gc.takeWrites(); len(w) != 0 {
		t.Fatalf("warm-up pass wrote %v", w)
	}

	// Warm: hostname first, then the A record goes, then the CNAME.
	e.gc.advance(11 * time.Minute)
	e.sync()
	want := []string{"PATCH " + e.gc.resourcePath("ferry-pool-a-1"), "DELETE " + hostPath("A", "web"), "POST " + hostPath("CNAME", "web")}
	if w := e.gc.takeWrites(); !slices.Equal(w, want) {
		t.Fatalf("migration wrote %v, want %v", w, want)
	}
	if got := e.gc.record("CNAME " + host("web")); got != cdnCNAME+"." {
		t.Fatalf("CNAME = %q", got)
	}
	if got := e.gc.record("A " + host("web")); got != "" {
		t.Fatalf("A record left: %q", got)
	}

	// Settled.
	e.sync()
	if w := e.gc.takeWrites(); len(w) != 0 {
		t.Fatalf("settled pass wrote %v", w)
	}
}

func TestCDNAddsHost(t *testing.T) {
	e := newCDNEnv(t, true)
	e.a.Seed(ingress("dev", "web", host("web"), "shop.example.com"))
	e.b.Seed(ingress("dev", "api", host("api")))

	e.sync()
	if got := e.gc.hostnames("ferry-pool-a-1"); !slices.Equal(got, []string{host("web")}) {
		t.Fatalf("a-1 = %v", got)
	}
	if got := e.gc.hostnames("ferry-pool-b-1"); !slices.Equal(got, []string{host("api")}) {
		t.Fatalf("b-1 = %v", got)
	}
	w := e.gc.takeWrites()
	if patch, post := slices.Index(w, "PATCH "+e.gc.resourcePath("ferry-pool-a-1")), slices.Index(w, "POST "+hostPath("CNAME", "web")); patch < 0 || patch > post {
		t.Fatalf("hostname must land before the CNAME: %v", w)
	}
	if got := e.gc.record("CNAME shop.example.com"); got != "" {
		t.Fatalf("host outside the zone got a record: %q", got)
	}

	// A failed CNAME is picked up next pass without touching the hostname again.
	e.gc.fail["POST "+hostPath("CNAME", "new")] = "nope"
	e.a.Seed(ingress("dev", "new", host("new")))
	e.sync()
	if !strings.Contains(e.d.problems[host("new")], "set "+host("new")) {
		t.Fatalf("problems = %v", e.d.problems)
	}
	delete(e.gc.fail, "POST "+hostPath("CNAME", "new"))
	e.gc.takeWrites()
	e.sync()
	if w := e.gc.takeWrites(); !slices.Equal(w, []string{"POST " + hostPath("CNAME", "new")}) {
		t.Fatalf("retry wrote %v", w)
	}
}

func TestCDNMovesHostBetweenClusters(t *testing.T) {
	e := newCDNEnv(t, true)
	e.gc.addResource("ferry-pool-a-2", 24*time.Hour)
	e.gc.records["CNAME "+host("web")] = cdnCNAME + "."
	e.gc.resources[0].Hostnames = []string{host("keep"), host("web")}
	e.gc.records["CNAME "+host("keep")] = cdnCNAME
	e.a.Seed(ingress("dev", "keep", host("keep")))
	e.b.Seed(ingress("dev", "web", host("web")))

	e.sync()
	want := []string{"PATCH " + e.gc.resourcePath("ferry-pool-a-1"), "PATCH " + e.gc.resourcePath("ferry-pool-b-1")}
	if w := e.gc.takeWrites(); !slices.Equal(w, want) {
		t.Fatalf("move wrote %v, want %v", w, want)
	}
	if got := e.gc.hostnames("ferry-pool-a-1"); !slices.Equal(got, []string{host("keep")}) {
		t.Fatalf("a-1 = %v", got)
	}
	if got := e.gc.hostnames("ferry-pool-b-1"); !slices.Equal(got, []string{host("web")}) {
		t.Fatalf("b-1 = %v", got)
	}
}

func TestCDNMoveWaitsForRoomOnTheNewCluster(t *testing.T) {
	e := newCDNEnv(t, true)
	full := make([]string, 0, 10)
	for i := range 10 {
		full = append(full, host(fmt.Sprintf("full%d", i)))
		e.b.Seed(ingress("dev", fmt.Sprintf("full%d", i), full[i]))
		e.gc.records["CNAME "+full[i]] = cdnCNAME + "."
	}
	e.gc.resources[1].Hostnames = full
	e.gc.addResource("ferry-pool-b-2", time.Minute) // too young to take it
	e.gc.resources[0].Hostnames = []string{host("web")}
	e.gc.records["CNAME "+host("web")] = cdnCNAME + "."
	e.b.Seed(ingress("dev", "web", host("web")))

	e.sync()
	if got := e.gc.hostnames("ferry-pool-a-1"); !slices.Equal(got, []string{host("web")}) {
		t.Fatalf("host left its working resource: %v", got)
	}
	if !strings.Contains(e.d.problems[host("web")], "waiting") {
		t.Fatalf("problems = %v", e.d.problems)
	}
}

func TestCDNDeletes(t *testing.T) {
	e := newCDNEnv(t, true)
	outside := e.gc.addResource("customer", 24*time.Hour, host("theirs"))
	e.gc.resources[0].Hostnames = []string{host("gone")}
	for key, content := range map[string]string{
		"CNAME " + host("gone"):      cdnCNAME + ".",
		"CNAME " + host("orphan"):    cdnCNAME,
		"CNAME " + host("theirs"):    cdnCNAME + ".",
		"CNAME " + host("elsewhere"): "other.example.net.",
		"CNAME *." + dnsZone:         cdnCNAME + ".",
		"A " + host("stale"):         "10.0.0.2",
		"A " + host("handmade"):      "203.0.113.9",
		"A *." + dnsZone:             "10.0.0.1",
		"NS " + dnsZone:              "ns1.example.net",
	} {
		e.gc.records[key] = content
	}

	e.sync()
	if got := e.gc.hostnames("ferry-pool-a-1"); len(got) != 0 {
		t.Fatalf("hostname kept: %v", got)
	}
	for _, key := range []string{"CNAME " + host("gone"), "CNAME " + host("orphan"), "A " + host("stale")} {
		if got := e.gc.record(key); got != "" {
			t.Errorf("%s kept: %q", key, got)
		}
	}
	for _, key := range []string{"CNAME " + host("theirs"), "CNAME " + host("elsewhere"), "CNAME *." + dnsZone, "A " + host("handmade"), "A *." + dnsZone, "NS " + dnsZone} {
		if got := e.gc.record(key); got == "" {
			t.Errorf("%s deleted", key)
		}
	}
	if got := e.gc.hostnames("customer"); !slices.Equal(got, []string{host("theirs")}) {
		t.Fatalf("outside resource changed: %v", got)
	}
	for _, w := range e.gc.takeWrites() {
		if strings.Contains(w, fmt.Sprint(outside.ID)) {
			t.Fatalf("wrote to outside resource: %s", w)
		}
	}
}

func TestCDNDeletesNothingWithoutFullView(t *testing.T) {
	e := newCDNEnv(t, true)
	e.gc.resources[0].Hostnames = []string{host("web")}
	e.gc.records["CNAME "+host("web")] = cdnCNAME + "."
	e.gc.records["A "+host("legacy")] = "10.0.0.1"
	e.a.SetForbidden(http.MethodGet, "ingresses", true)
	e.b.Seed(ingress("dev", "api", host("api")))

	e.sync()
	if got := e.gc.hostnames("ferry-pool-a-1"); !slices.Equal(got, []string{host("web")}) {
		t.Fatalf("hostname removed: %v", got)
	}
	if e.gc.record("CNAME "+host("web")) == "" || e.gc.record("A "+host("legacy")) == "" {
		t.Fatal("record removed with a cluster unread")
	}
	// Adds still go through.
	if e.gc.record("CNAME "+host("api")) == "" {
		t.Fatal("reachable cluster's host not added")
	}

	e.a.SetForbidden(http.MethodGet, "ingresses", false)
	e.sync()
	if got := e.gc.hostnames("ferry-pool-a-1"); len(got) != 0 {
		t.Fatalf("hostname kept once the view was full: %v", got)
	}
}

func TestCDNExpandsAhead(t *testing.T) {
	e := newCDNEnv(t, true)
	hosts := []string{}
	for i := range 8 {
		hosts = append(hosts, host(fmt.Sprintf("h%d", i)))
		e.a.Seed(ingress("dev", fmt.Sprintf("h%d", i), hosts[i]))
		e.gc.records["CNAME "+hosts[i]] = cdnCNAME + "."
	}
	e.gc.resources[0].Hostnames = hosts

	// Two slots left on a: enough.
	e.sync()
	if w := e.gc.takeWrites(); len(w) != 0 {
		t.Fatalf("wrote %v with slots to spare", w)
	}

	// The ninth host drops it below the threshold, and only for a.
	e.a.Seed(ingress("dev", "h8", host("h8")))
	e.sync()
	if got := e.gc.resourceNames(); !slices.Equal(got, []string{"ferry-pool-a-1", "ferry-pool-b-1", "ferry-pool-a-2"}) {
		t.Fatalf("resources = %v", got)
	}
	if got := e.gc.resources[2].body["origin"]; got != "10.0.0.1" {
		t.Fatalf("origin = %v", got)
	}

	// Warming slots count: one new resource is enough.
	e.gc.takeWrites()
	e.sync()
	if w := e.gc.takeWrites(); len(w) != 0 {
		t.Fatalf("second pass wrote %v", w)
	}
}

func TestCDNSkipsSuspendedResource(t *testing.T) {
	e := newCDNEnv(t, true)
	e.gc.resources[0].Status = "suspended"
	e.a.Seed(ingress("dev", "web", host("web")))

	e.sync()
	if !slices.Contains(e.gc.resourceNames(), "ferry-pool-a-2") {
		t.Fatalf("no replacement: %v", e.gc.resourceNames())
	}
	if !strings.Contains(e.d.problems["cdn/ferry-pool-a-1"], "suspended") {
		t.Fatalf("problems = %v", e.d.problems)
	}
	if len(e.gc.hostnames("ferry-pool-a-1")) != 0 {
		t.Fatal("host put on a suspended resource")
	}
}

func TestCDNTenHostnamesPerResource(t *testing.T) {
	e := newCDNEnv(t, true)
	full := []string{}
	for i := range 10 {
		full = append(full, host(fmt.Sprintf("full%d", i)))
		e.a.Seed(ingress("dev", fmt.Sprintf("full%d", i), full[i]))
		e.gc.records["CNAME "+full[i]] = cdnCNAME + "."
	}
	e.gc.resources[0].Hostnames = full
	e.gc.addResource("ferry-pool-a-2", 24*time.Hour)
	e.a.Seed(ingress("dev", "web", host("web")))

	e.sync()
	if got := e.gc.hostnames("ferry-pool-a-1"); len(got) != 10 {
		t.Fatalf("a-1 has %d", len(got))
	}
	if got := e.gc.hostnames("ferry-pool-a-2"); !slices.Equal(got, []string{host("web")}) {
		t.Fatalf("a-2 = %v", got)
	}

	// Twenty-first host, no room anywhere ready: it waits rather than draw a 400.
	more := []string{}
	for i := range 9 {
		more = append(more, host(fmt.Sprintf("more%d", i)))
		e.a.Seed(ingress("dev", fmt.Sprintf("more%d", i), more[i]))
	}
	e.a.Seed(ingress("dev", "last", host("last")))
	e.sync()
	if got := e.gc.hostnames("ferry-pool-a-2"); len(got) != 10 {
		t.Fatalf("a-2 = %v, problems %v", got, e.d.problems)
	}
	waiting := 0
	for _, msg := range e.d.problems {
		if strings.Contains(msg, "waiting") {
			waiting++
		}
	}
	if waiting != 1 || !slices.Contains(e.gc.resourceNames(), "ferry-pool-a-3") {
		t.Fatalf("waiting = %d, resources %v", waiting, e.gc.resourceNames())
	}
}

func TestCDNLeavesOtherResourcesAlone(t *testing.T) {
	e := newCDNEnv(t, true)
	e.gc.addResource("customer", 24*time.Hour, host("theirs"))
	e.gc.addResource("ferry-pool-notes", 24*time.Hour, host("lookalike"))
	e.gc.addResource("ferry-pooled-a-1", 24*time.Hour, host("near"))
	e.gc.records["A "+host("theirs")] = "198.51.100.7"
	e.a.Seed(ingress("dev", "web", host("theirs")))

	e.sync()
	if w := e.gc.takeWrites(); len(w) != 0 {
		t.Fatalf("wrote %v", w)
	}
	if !strings.Contains(e.d.problems[host("theirs")], `"customer"`) {
		t.Fatalf("problems = %v", e.d.problems)
	}
	if got := e.gc.record("A " + host("theirs")); got != "198.51.100.7" {
		t.Fatalf("record changed: %q", got)
	}
}

func TestCDNLeavesHostsItCannotServeAlone(t *testing.T) {
	e := newCDNEnv(t, true)
	e.gc.records["CNAME "+host("alias")] = "other.example.net."
	e.gc.records["A deep.down."+dnsZone] = "10.0.0.1"
	e.a.Seed(ingress("dev", "alias", host("alias")), ingress("dev", "deep", "deep.down."+dnsZone))
	e.a.Seed(ingress("dev", "both", host("both")))
	e.b.Seed(ingress("dev", "both", host("both")))

	e.sync()
	if w := e.gc.takeWrites(); len(w) != 0 {
		t.Fatalf("wrote %v", w)
	}
	for name, want := range map[string]string{
		host("alias"):          "CNAME to other.example.net.",
		"deep.down." + dnsZone: "too deep",
		host("both"):           "several clusters",
	} {
		if got := e.d.problems[name]; !strings.Contains(got, want) {
			t.Errorf("%s: %q", name, got)
		}
	}
}

func TestCDNProblemsLogOnceWithoutSecrets(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	e := newCDNEnv(t, true)
	e.a.Seed(ingress("dev", "web", host("web")))
	e.gc.fail["PATCH "+e.gc.resourcePath("ferry-pool-a-1")] = "bad token gc-tok"
	e.sync()
	e.sync()
	if got := strings.Count(buf.String(), "add "+host("web")); got != 1 {
		t.Fatalf("logged %d times:\n%s", got, buf.String())
	}
	if strings.Contains(buf.String(), "gc-tok") {
		t.Fatalf("token in log:\n%s", buf.String())
	}
	if e.gc.record("CNAME "+host("web")) != "" {
		t.Fatal("CNAME written for a host with no resource")
	}
}

func TestCDNCertificateRenewal(t *testing.T) {
	day := 24 * time.Hour
	for name, tc := range map[string]struct {
		left  time.Duration
		renew bool
	}{
		"plenty left": {60 * day, false},
		"just enough": {30*day + time.Hour, false},
		"due":         {29 * day, true},
		"expired":     {-day, true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newCDNEnv(t, true)
			e.gc.certs[0].NotAfter = cdnEpoch.Add(tc.left).Format(time.RFC3339)
			e.sync()
			if !tc.renew {
				if len(e.issuer.orders) != 0 || e.gc.certs[0].Cert != "old" {
					t.Fatalf("renewed early: %v", e.issuer.orders)
				}
				return
			}
			want := fmt.Sprintf("PUT /cdn/sslData/%d", e.gc.certs[0].ID)
			if w := e.gc.takeWrites(); !slices.Equal(w, []string{want}) {
				t.Fatalf("wrote %v, want %v", w, want)
			}
			if len(e.gc.certs) != 1 || e.gc.certs[0].Cert != "cert-1" {
				t.Fatalf("certs = %+v", e.gc.certs)
			}
		})
	}
}

func TestCDNCertificateFailures(t *testing.T) {
	t.Run("no certificate: nothing is built", func(t *testing.T) {
		e := newCDNEnv(t, false)
		e.issuer.err = errors.New("rate limited")
		e.a.Seed(ingress("dev", "web", host("web")))
		e.sync()
		if w := e.gc.takeWrites(); len(w) != 0 {
			t.Fatalf("wrote %v", w)
		}
		if !strings.Contains(e.d.problems["cdn/cert"], "rate limited") {
			t.Fatalf("problems = %v", e.d.problems)
		}
	})
	t.Run("a failed order waits an hour", func(t *testing.T) {
		e := newCDNEnv(t, false)
		e.issuer.err = errors.New("rate limited")
		e.sync()
		e.gc.advance(30 * time.Minute)
		e.sync()
		if len(e.issuer.orders) != 1 {
			t.Fatalf("orders = %d", len(e.issuer.orders))
		}
		e.gc.advance(31 * time.Minute)
		e.issuer.err = nil
		e.sync()
		if len(e.issuer.orders) != 2 || len(e.gc.certs) != 1 {
			t.Fatalf("orders = %d, certs %d", len(e.issuer.orders), len(e.gc.certs))
		}
	})
	t.Run("a failed renewal keeps the pool working", func(t *testing.T) {
		e := newCDNEnv(t, true)
		e.gc.certs[0].NotAfter = cdnEpoch.Add(10 * 24 * time.Hour).Format(time.RFC3339)
		e.issuer.err = errors.New("rate limited")
		e.a.Seed(ingress("dev", "web", host("web")))
		e.sync()
		if e.gc.record("CNAME "+host("web")) == "" {
			t.Fatal("host not added on a still-valid certificate")
		}
	})
	t.Run("a failed upload doesn't order again", func(t *testing.T) {
		e := newCDNEnv(t, false)
		e.gc.fail["POST /cdn/sslData"] = "busy"
		e.sync()
		e.sync()
		delete(e.gc.fail, "POST /cdn/sslData")
		e.sync()
		if len(e.issuer.orders) != 1 || len(e.gc.certs) != 1 || e.gc.certs[0].Cert != "cert-1" {
			t.Fatalf("orders = %d, certs %+v", len(e.issuer.orders), e.gc.certs)
		}
	})
}

func TestCDNReadsBareLists(t *testing.T) {
	e := newCDNEnv(t, true)
	e.gc.bareLists = true
	e.a.Seed(ingress("dev", "web", host("web")))
	e.sync()
	if got := e.gc.hostnames("ferry-pool-a-1"); !slices.Equal(got, []string{host("web")}) {
		t.Fatalf("a-1 = %v, problems %v", got, e.d.problems)
	}
}

// With the switch off the CDN API is never called and hosts get A records as before.
func TestCDNOffKeepsARecords(t *testing.T) {
	e := newCDNEnv(t, true)
	e.d.cdn = nil
	e.gc.records["A "+host("old")] = "10.0.0.2"
	e.gc.records["CNAME "+host("alias")] = cdnCNAME + "."
	e.a.Seed(ingress("dev", "web", host("web")))

	e.sync()
	if got := e.gc.record("A " + host("web")); got != "10.0.0.1" {
		t.Fatalf("A = %q", got)
	}
	if got := e.gc.record("A " + host("old")); got != "" {
		t.Fatalf("old record kept: %q", got)
	}
	if e.gc.record("CNAME "+host("alias")) == "" {
		t.Fatal("CNAME touched")
	}
	for _, w := range e.gc.takeWrites() {
		if strings.Contains(w, "/cdn/") {
			t.Fatalf("wrote to the CDN: %s", w)
		}
	}
	if len(e.issuer.orders) != 0 {
		t.Fatal("ordered a certificate")
	}
}

func TestCDNSwitchPicksTheRealIssuer(t *testing.T) {
	e := newCDNEnv(t, true) // a good certificate on Gcore: the loop has no reason to call lego
	eng := e.d.eng
	eng.initDNS(time.Hour)
	if eng.dnsSync == nil || eng.dnsSync.cdn == nil {
		t.Fatal("cdn enabled but no pool")
	}
	got, ok := eng.dnsSync.cdn.issuer.(legoIssuer)
	if !ok || got.email != "ops@example.com" || got.token != "gc-tok" {
		t.Fatalf("issuer = %T", eng.dnsSync.cdn.issuer)
	}
}

func TestParsePoolName(t *testing.T) {
	for name, want := range map[string]struct {
		cluster string
		n       int
		ok      bool
	}{
		"ferry-pool-tl-1":       {"tl", 1, true},
		"ferry-pool-us-east-12": {"us-east", 12, true},
		"ferry-pool-tl":         {"", 0, false},
		"ferry-pool-tl-x":       {"", 0, false},
		"ferry-pool-tl-0":       {"", 0, false},
		"ferry-pool--1":         {"", 0, false},
		"ferry-pooled-tl-1":     {"", 0, false},
		"other":                 {"", 0, false},
	} {
		cluster, n, ok := parsePoolName("ferry-pool", name)
		if cluster != want.cluster || n != want.n || ok != want.ok {
			t.Errorf("%s = %q %d %v", name, cluster, n, ok)
		}
	}
}

func TestCDNReadyGoesByAge(t *testing.T) {
	r := cdnResource{Created: cdnEpoch.Format(time.RFC3339), Status: "processed"}
	if r.ready(cdnEpoch.Add(cdnWarmup - time.Second)) {
		t.Fatal("ready too soon")
	}
	if !r.ready(cdnEpoch.Add(cdnWarmup)) {
		t.Fatal("not ready after the warm-up")
	}
	unreadable := cdnResource{Created: "yesterday", Status: "active"}
	if !unreadable.ready(cdnEpoch) {
		t.Fatal("an unreadable date should fall back on status")
	}
	unreadable.Status = "processed"
	if unreadable.ready(cdnEpoch) {
		t.Fatal("an unreadable date should fall back on status")
	}
}

func TestCDNConfigValidation(t *testing.T) {
	base := `
auth: {disabled: true}
state: {file: "/tmp/s.json"}
namespaces: ["app"]
clusters:
  - {name: de, inCluster: true}
dns: {zone: dev.example.com, targets: {de: 1.2.3.4}, gcore: {token: t}, cdn: `
	cfg, err := ParseConfig([]byte(base + `{enabled: true, acmeEmail: ops@example.com}}`))
	if err != nil {
		t.Fatalf("valid cdn config: %v", err)
	}
	if cfg.DNS.CDN.Prefix != "ferry-pool" || cfg.DNS.CDN.MinFree != 2 {
		t.Fatalf("defaults = %+v", cfg.DNS.CDN)
	}
	if _, err := ParseConfig([]byte(base + `{enabled: false}}`)); err != nil {
		t.Fatalf("disabled cdn needs nothing: %v", err)
	}
	for name, cdn := range map[string]string{
		"no email":       `{enabled: true}}`,
		"bad prefix":     `{enabled: true, acmeEmail: ops@example.com, prefix: "Ferry_Pool"}}`,
		"threshold low":  `{enabled: true, acmeEmail: ops@example.com, minFree: -1}}`,
		"threshold high": `{enabled: true, acmeEmail: ops@example.com, minFree: 11}}`,
	} {
		if _, err := ParseConfig([]byte(base + cdn)); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	noZone := strings.Replace(base, "zone: dev.example.com, ", "", 1)
	if _, err := ParseConfig([]byte(noZone + `{enabled: true, acmeEmail: ops@example.com}}`)); err == nil {
		t.Error("cdn without a zone: expected a validation error")
	}
}
