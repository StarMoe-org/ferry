package ferry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const dnsSyncEvery = 30 * time.Second

// dnsSyncer gives every Ingress host under dns.zone an A record at the public address of the cluster
// it runs in, and removes the record when the host is gone from every cluster. People add domains in
// the dashboard; nothing there has to know about the DNS provider.
type dnsSyncer struct {
	eng   *Engine
	gcore *gcoreDNS

	ctx  context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup

	mu       sync.Mutex
	problems map[string]string // logged once each, until they change
}

func (e *Engine) initDNS(every time.Duration) {
	if e.cfg.DNS.Zone == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &dnsSyncer{
		eng:      e,
		gcore:    newGcoreDNS(e.cfg.DNS),
		ctx:      ctx,
		stop:     cancel,
		problems: map[string]string{},
	}
	e.dnsSync = d
	d.wg.Add(1)
	go d.loop(every)
}

func (d *dnsSyncer) Close() {
	d.stop()
	d.wg.Wait()
}

func (d *dnsSyncer) loop(every time.Duration) {
	defer d.wg.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		ctx, cancel := context.WithTimeout(d.ctx, every)
		d.sync(ctx)
		cancel()
		select {
		case <-d.ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sync makes one pass. Without a full view of every cluster it still adds and moves records, but
// deletes nothing: a cluster that cannot be reached must not take its domains down with it.
func (d *dnsSyncer) sync(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()

	cfg := d.eng.cfg.DNS
	problems := map[string]string{}
	want, complete := d.ingressHosts(ctx, problems)
	current, err := d.gcore.records(ctx)
	if err != nil {
		problems["gcore"] = "list records: " + err.Error()
		d.report(problems)
		return
	}

	hosts := make([]string, 0, len(want))
	for host := range want {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		clusters := want[host]
		if len(clusters) > 1 {
			problems[host] = fmt.Sprintf("%s is on several clusters (%s); leaving its record alone", host, strings.Join(clusters, ", "))
			continue
		}
		ip, ok := cfg.Targets[clusters[0]]
		if !ok {
			problems[host] = fmt.Sprintf("%s is on cluster %s, which has no address in dns.targets", host, clusters[0])
			continue
		}
		got, exists := current[host]
		if exists && len(got) == 1 && got[0] == ip {
			continue
		}
		if err := d.gcore.set(ctx, host, ip, exists); err != nil {
			problems[host] = fmt.Sprintf("set %s: %v", host, err)
			continue
		}
		log.Printf("dns: %s -> %s (%s)", host, ip, clusters[0])
	}

	if !complete {
		d.report(problems)
		return
	}
	ours := make([]string, 0, len(cfg.Targets))
	for _, ip := range cfg.Targets {
		ours = append(ours, ip)
	}
	for name, ips := range current {
		if _, wanted := want[name]; wanted || !removable(name, cfg.Zone, ips, ours) {
			continue
		}
		if err := d.gcore.remove(ctx, name); err != nil {
			problems[name] = fmt.Sprintf("remove %s: %v", name, err)
			continue
		}
		log.Printf("dns: removed %s", name)
	}
	d.report(problems)
}

// removable keeps the zone's own records, wildcards, and anything pointing outside our clusters:
// those were made by hand.
func removable(name, zone string, ips, ours []string) bool {
	if name == zone || strings.HasPrefix(name, "*.") || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !slices.Contains(ours, ip) {
			return false
		}
	}
	return true
}

// ingressHosts maps each host under the zone to the clusters serving it. complete is false when a
// cluster could not be read.
func (d *dnsSyncer) ingressHosts(ctx context.Context, problems map[string]string) (map[string][]string, bool) {
	suffix := "." + d.eng.cfg.DNS.Zone
	want := map[string][]string{}
	complete := true
	for _, cl := range d.eng.clusters {
		var list struct {
			Items []struct {
				Metadata struct {
					DeletionTimestamp string `json:"deletionTimestamp"`
				} `json:"metadata"`
				Spec struct {
					Rules []struct {
						Host string `json:"host"`
					} `json:"rules"`
				} `json:"spec"`
			} `json:"items"`
		}
		if err := cl.Get(ctx, "/apis/networking.k8s.io/v1/ingresses", &list); err != nil {
			problems["cluster/"+cl.Name] = fmt.Sprintf("list ingresses on %s: %v", cl.Name, err)
			complete = false
			continue
		}
		for _, ing := range list.Items {
			if ing.Metadata.DeletionTimestamp != "" {
				continue
			}
			for _, rule := range ing.Spec.Rules {
				host := strings.ToLower(rule.Host)
				if !strings.HasSuffix(host, suffix) || strings.HasPrefix(host, "*.") {
					continue
				}
				if !slices.Contains(want[host], cl.Name) {
					want[host] = append(want[host], cl.Name)
				}
			}
		}
	}
	return want, complete
}

// report logs problems that are new or changed, so a standing one doesn't repeat every pass.
func (d *dnsSyncer) report(problems map[string]string) {
	for key, msg := range problems {
		if d.problems[key] != msg {
			log.Printf("dns: %s", d.eng.scrubSecrets(msg))
		}
	}
	d.problems = problems
}

// gcoreDNS is the part of Gcore's DNS API Ferry uses: static A records in one zone.
type gcoreDNS struct {
	api   string
	zone  string
	token string
	ttl   int
	hc    *http.Client
}

func newGcoreDNS(cfg DNSConfig) *gcoreDNS {
	return &gcoreDNS{
		api:   strings.TrimRight(cfg.Gcore.API, "/"),
		zone:  cfg.Zone,
		token: cfg.Gcore.Token,
		ttl:   cfg.TTL,
		hc:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (g *gcoreDNS) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.api+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "APIKey "+g.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("gcore %s: %d: %s", method, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// records returns the zone's A records: name to addresses.
func (g *gcoreDNS) records(ctx context.Context) (map[string][]string, error) {
	const page = 100
	out := map[string][]string{}
	for offset := 0; ; offset += page {
		var resp struct {
			RRSets []struct {
				Name            string `json:"name"`
				Type            string `json:"type"`
				ResourceRecords []struct {
					Content []any `json:"content"`
				} `json:"resource_records"`
			} `json:"rrsets"`
			TotalAmount int `json:"total_amount"`
		}
		path := fmt.Sprintf("/dns/v2/zones/%s/rrsets?limit=%d&offset=%d", url.PathEscape(g.zone), page, offset)
		if err := g.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		for _, rr := range resp.RRSets {
			if rr.Type != "A" {
				continue
			}
			var ips []string
			for _, rec := range rr.ResourceRecords {
				for _, c := range rec.Content {
					if ip, ok := c.(string); ok {
						ips = append(ips, ip)
					}
				}
			}
			out[strings.ToLower(rr.Name)] = ips
		}
		if len(resp.RRSets) == 0 || offset+page >= resp.TotalAmount {
			return out, nil
		}
	}
}

func (g *gcoreDNS) rrsetPath(name string) string {
	return "/dns/v2/zones/" + url.PathEscape(g.zone) + "/" + url.PathEscape(name) + "/A"
}

func (g *gcoreDNS) set(ctx context.Context, name, ip string, exists bool) error {
	method := http.MethodPost
	if exists {
		method = http.MethodPut
	}
	return g.do(ctx, method, g.rrsetPath(name), map[string]any{
		"ttl":              g.ttl,
		"resource_records": []any{map[string]any{"content": []string{ip}, "enabled": true}},
	}, nil)
}

func (g *gcoreDNS) remove(ctx context.Context, name string) error {
	return g.do(ctx, http.MethodDelete, g.rrsetPath(name), nil, nil)
}
