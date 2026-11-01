package ferry

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	cdnSlotsPerResource = 10               // Gcore's limit on secondary hostnames
	cdnWarmup           = 20 * time.Minute // a new resource served 503s for 17 minutes when measured
	certRenewBefore     = 30 * 24 * time.Hour
	certRetryEvery      = time.Hour // Let's Encrypt limits failed orders and new accounts
	certTimeout         = 10 * time.Minute
)

// cdnPool keeps origins out of DNS: hosts become secondary hostnames on CDN resources Ferry owns, and
// their records CNAMEs to Gcore. A new resource takes 17 minutes to reach the edge, while a hostname
// added to a live one works within 30 seconds, so each cluster keeps slots open ahead of need.
//
// All of it is read back from Gcore on each pass. What's kept here only spares Let's Encrypt.
type cdnPool struct {
	cfg    DNSConfig
	gcore  *gcoreDNS
	issuer certIssuer
	now    func() time.Time

	issued  *issuedCert // signed, not on Gcore yet: uploading again must not cost another order
	retryAt time.Time   // no ACME order before this
}

type issuedCert struct {
	cert, key string
}

func newCDNPool(cfg DNSConfig, g *gcoreDNS, issuer certIssuer) *cdnPool {
	return &cdnPool{cfg: cfg, gcore: g, issuer: issuer, now: time.Now}
}

func poolName(prefix, cluster string, n int) string {
	return fmt.Sprintf("%s-%s-%d", prefix, cluster, n)
}

// parsePoolName splits <prefix>-<cluster>-<n>. Cluster names can hold dashes, so it counts from the
// end. Anything else is not Ferry's, whatever it starts with.
func parsePoolName(prefix, name string) (string, int, bool) {
	rest, ok := strings.CutPrefix(name, prefix+"-")
	if !ok {
		return "", 0, false
	}
	i := strings.LastIndexByte(rest, '-')
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(rest[i+1:])
	if err != nil || n < 1 {
		return "", 0, false
	}
	return rest[:i], n, true
}

// cdnResource is the part of a Gcore CDN resource Ferry reads.
type cdnResource struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Primary   string   `json:"cname"`
	Hostnames []string `json:"secondaryHostnames"`
	Active    bool     `json:"active"`
	Deleted   bool     `json:"deleted"`
	Status    string   `json:"status"`
	Created   string   `json:"created"`

	cluster string
	n       int
}

// live is false for a resource that won't serve, as after Gcore suspends one for 90 days of no traffic.
func (r *cdnResource) live() bool {
	return r.Active && r.Status != "suspended" && r.Status != "deleted"
}

// ready goes by age. Status says "processed" while a resource propagates, but what was measured is
// the wait for its first request to reach the origin.
func (r *cdnResource) ready(now time.Time) bool {
	created, err := time.Parse(time.RFC3339, r.Created)
	if err != nil {
		return r.Status == "active"
	}
	return now.Sub(created) >= cdnWarmup
}

// cdnView is one pass's picture of the account.
type cdnView struct {
	target  string                  // where every CDN host's CNAME points
	pool    []*cdnResource          // Ferry's, by cluster then number
	hosts   map[string]*cdnResource // secondary hostnames on the pool
	outside map[string]string       // every other hostname on the account, to the resource holding it
}

func (p *cdnPool) view(ctx context.Context) (*cdnView, error) {
	target, err := p.gcore.cdnTarget(ctx)
	if err != nil {
		return nil, err
	}
	resources, err := p.gcore.cdnResources(ctx)
	if err != nil {
		return nil, err
	}
	v := &cdnView{target: target, hosts: map[string]*cdnResource{}, outside: map[string]string{}}
	for i := range resources {
		r := &resources[i]
		if r.Deleted {
			continue
		}
		v.outside[strings.ToLower(r.Primary)] = r.Name
		cluster, n, ours := parsePoolName(p.cfg.CDN.Prefix, r.Name)
		for _, host := range r.Hostnames {
			if ours {
				v.hosts[strings.ToLower(host)] = r
			} else {
				v.outside[strings.ToLower(host)] = r.Name
			}
		}
		if ours {
			r.cluster, r.n = cluster, n
			v.pool = append(v.pool, r)
		}
	}
	sort.Slice(v.pool, func(i, j int) bool {
		if v.pool[i].cluster != v.pool[j].cluster {
			return v.pool[i].cluster < v.pool[j].cluster
		}
		return v.pool[i].n < v.pool[j].n
	})
	return v, nil
}

// slot finds a resource on the cluster that can take a host now.
func (v *cdnView) slot(cluster string, now time.Time) *cdnResource {
	for _, r := range v.pool {
		if r.cluster == cluster && r.live() && r.ready(now) && len(r.Hostnames) < cdnSlotsPerResource {
			return r
		}
	}
	return nil
}

// free counts open slots on the cluster, including those still warming up.
func (v *cdnView) free(cluster string) int {
	total := 0
	for _, r := range v.pool {
		if r.cluster == cluster && r.live() {
			total += cdnSlotsPerResource - len(r.Hostnames)
		}
	}
	return total
}

func (v *cdnView) nextNumber(cluster string) int {
	last := 0
	for _, r := range v.pool {
		if r.cluster == cluster {
			last = max(last, r.n)
		}
	}
	return last + 1
}

func (v *cdnView) pointsHere(contents []string) bool {
	return len(contents) == 1 && sameHost(contents[0], v.target)
}

func sameHost(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}

func (p *cdnPool) addHost(ctx context.Context, v *cdnView, r *cdnResource, host string) error {
	next := append(slices.Clone(r.Hostnames), host)
	if err := p.gcore.setHostnames(ctx, r.ID, next); err != nil {
		return err
	}
	r.Hostnames = next
	v.hosts[host] = r
	return nil
}

func (p *cdnPool) removeHost(ctx context.Context, v *cdnView, r *cdnResource, host string) error {
	next := slices.DeleteFunc(slices.Clone(r.Hostnames), func(h string) bool { return strings.EqualFold(h, host) })
	if err := p.gcore.setHostnames(ctx, r.ID, next); err != nil {
		return err
	}
	r.Hostnames = next
	delete(v.hosts, host)
	return nil
}

// syncCDN is sync with the pool in place of A records. A host's old record stays until it has a
// resource that can serve it, so turning this on doesn't take anything down while the pool warms up.
func (d *dnsSyncer) syncCDN(ctx context.Context, want map[string][]string, complete bool, zone map[string]map[string][]string, problems map[string]string) {
	p := d.cdn
	certID, ok := d.ensureCert(ctx, problems)
	if !ok {
		return
	}
	v, err := p.view(ctx)
	if err != nil {
		problems["gcore"] = "read cdn: " + err.Error()
		return
	}
	for _, r := range v.pool {
		if !r.live() {
			problems["cdn/"+r.Name] = fmt.Sprintf("%s is %s", r.Name, r.Status)
		}
	}

	for _, host := range slices.Sorted(maps.Keys(want)) {
		clusters := want[host]
		if len(clusters) > 1 {
			problems[host] = fmt.Sprintf("%s is on several clusters (%s); leaving its record alone", host, strings.Join(clusters, ", "))
			continue
		}
		if _, ok := p.cfg.Targets[clusters[0]]; !ok {
			problems[host] = fmt.Sprintf("%s is on cluster %s, which has no address in dns.targets", host, clusters[0])
			continue
		}
		if msg := d.routeHost(ctx, v, host, clusters[0], zone); msg != "" {
			problems[host] = msg
		}
	}

	if complete {
		d.pruneCDN(ctx, v, want, zone, problems)
	}

	for _, cluster := range slices.Sorted(maps.Keys(p.cfg.Targets)) {
		if v.free(cluster) >= p.cfg.CDN.MinFree {
			continue
		}
		name := poolName(p.cfg.CDN.Prefix, cluster, v.nextNumber(cluster))
		if err := p.gcore.createResource(ctx, name, name+"."+p.cfg.Zone, p.cfg.Targets[cluster], certID); err != nil {
			problems["cdn/"+name] = fmt.Sprintf("create %s: %v", name, err)
			continue
		}
		log.Printf("dns: created %s, usable in about %s", name, cdnWarmup)
	}
}

// routeHost puts one host on its cluster's pool and points its name there. It returns what's in the
// way, if anything.
func (d *dnsSyncer) routeHost(ctx context.Context, v *cdnView, host, cluster string, zone map[string]map[string][]string) string {
	p := d.cdn
	if label := strings.TrimSuffix(host, "."+p.cfg.Zone); strings.Contains(label, ".") {
		return fmt.Sprintf("%s is too deep for the certificate on *.%s; leaving it alone", host, p.cfg.Zone)
	}
	if name, taken := v.outside[host]; taken {
		return fmt.Sprintf("%s is on CDN resource %q, which isn't in the pool; leaving it alone", host, name)
	}
	cname, hasCNAME := zone["CNAME"][host]
	if hasCNAME && !v.pointsHere(cname) {
		return fmt.Sprintf("%s has a CNAME to %s; leaving it alone", host, strings.Join(cname, ", "))
	}

	cur := v.hosts[host]
	if cur == nil || cur.cluster != cluster || !cur.live() {
		// Find the new home first: a host with nowhere to go stays where it works.
		dst := v.slot(cluster, p.now())
		if dst == nil {
			return fmt.Sprintf("%s is waiting for a pool resource on %s", host, cluster)
		}
		if cur != nil {
			if err := p.removeHost(ctx, v, cur, host); err != nil {
				return fmt.Sprintf("move %s off %s: %v", host, cur.Name, err)
			}
		}
		if err := p.addHost(ctx, v, dst, host); err != nil {
			return fmt.Sprintf("add %s to %s: %v", host, dst.Name, err)
		}
		log.Printf("dns: %s on %s (%s)", host, dst.Name, cluster)
	}

	if hasCNAME {
		return ""
	}
	// Gcore refuses a CNAME beside an A record.
	if _, exists := zone["A"][host]; exists {
		if err := p.gcore.remove(ctx, "A", host); err != nil {
			return fmt.Sprintf("remove A record of %s: %v", host, err)
		}
	}
	if err := p.gcore.set(ctx, "CNAME", host, v.target+".", false); err != nil {
		return fmt.Sprintf("set %s: %v", host, err)
	}
	log.Printf("dns: %s -> %s", host, v.target)
	return ""
}

// pruneCDN clears out hosts gone from every cluster. The whole account shares one CNAME target, so a
// CNAME pointing there can still belong to a resource outside the pool: those stay.
func (d *dnsSyncer) pruneCDN(ctx context.Context, v *cdnView, want map[string][]string, zone map[string]map[string][]string, problems map[string]string) {
	p := d.cdn
	suffix := "." + p.cfg.Zone
	for _, r := range v.pool {
		for _, host := range slices.Clone(r.Hostnames) {
			host = strings.ToLower(host)
			if _, wanted := want[host]; wanted || !strings.HasSuffix(host, suffix) {
				continue
			}
			if err := p.removeHost(ctx, v, r, host); err != nil {
				problems[host] = fmt.Sprintf("remove %s from %s: %v", host, r.Name, err)
				continue
			}
			log.Printf("dns: %s off %s", host, r.Name)
		}
	}
	for name, cname := range zone["CNAME"] {
		_, wanted := want[name]
		_, held := v.outside[name]
		if wanted || held || name == p.cfg.Zone || strings.HasPrefix(name, "*.") || !v.pointsHere(cname) {
			continue
		}
		if err := p.gcore.remove(ctx, "CNAME", name); err != nil {
			problems[name] = fmt.Sprintf("remove %s: %v", name, err)
			continue
		}
		log.Printf("dns: removed %s", name)
	}
	ours := make([]string, 0, len(p.cfg.Targets))
	for _, ip := range p.cfg.Targets {
		ours = append(ours, ip)
	}
	for name, ips := range zone["A"] {
		if _, wanted := want[name]; wanted || !removable(name, p.cfg.Zone, ips, ours) {
			continue
		}
		if err := p.gcore.remove(ctx, "A", name); err != nil {
			problems[name] = fmt.Sprintf("remove %s: %v", name, err)
			continue
		}
		log.Printf("dns: removed %s", name)
	}
}

// ensureCert keeps the zone's wildcard certificate on Gcore current and returns its ID. ok is false
// when there's no certificate to build on yet, and after an upload: an order can outlast the pass, so
// the next pass picks up from there with time to spare.
func (d *dnsSyncer) ensureCert(ctx context.Context, problems map[string]string) (id int64, ok bool) {
	p := d.cdn
	name := p.cfg.CDN.Prefix + "-" + p.cfg.Zone
	cert, err := p.gcore.certificate(ctx, name)
	if err != nil {
		problems["gcore"] = "read certificates: " + err.Error()
		return 0, false
	}
	now := p.now()
	usable := false
	if cert != nil {
		notAfter, err := time.Parse(time.RFC3339, cert.NotAfter)
		if err != nil {
			problems["cdn/cert"] = fmt.Sprintf("certificate %s: %v", name, err)
			return cert.ID, true
		}
		if notAfter.Sub(now) >= certRenewBefore {
			p.issued = nil
			return cert.ID, true
		}
		id, usable = cert.ID, now.Before(notAfter)
	}

	aCtx, cancel := context.WithTimeout(d.ctx, certTimeout)
	defer cancel()
	if p.issued == nil {
		if now.Before(p.retryAt) {
			return id, usable
		}
		domain := "*." + p.cfg.Zone
		certPEM, keyPEM, err := p.issuer.Issue(aCtx, domain)
		if err != nil {
			p.retryAt = now.Add(certRetryEvery)
			problems["cdn/cert"] = fmt.Sprintf("issue %s: %v", domain, err)
			return id, usable
		}
		p.issued = &issuedCert{cert: certPEM, key: keyPEM}
		log.Printf("dns: issued certificate for %s", domain)
	}
	if err := p.gcore.uploadCertificate(aCtx, cert, name, p.issued); err != nil {
		problems["cdn/cert"] = fmt.Sprintf("upload certificate %s: %v", name, err)
		return id, usable
	}
	p.issued = nil
	log.Printf("dns: uploaded certificate %s", name)
	return id, false
}

// sslData is the part of a Gcore CDN certificate Ferry reads.
type sslData struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	NotAfter string `json:"validity_not_after"`
	Deleted  bool   `json:"deleted"`
}

// cdnList reads a list endpoint, which answers in an envelope when given a limit. The account is
// allowed far fewer objects than the limit, so one page is all of them.
func cdnList[T any](ctx context.Context, g *gcoreDNS, path string) ([]T, error) {
	var raw json.RawMessage
	if err := g.do(ctx, http.MethodGet, path+"?limit=1000", nil, &raw); err != nil {
		return nil, err
	}
	var items []T
	if json.Unmarshal(raw, &items) == nil {
		return items, nil
	}
	var page struct {
		Results []T `json:"results"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, err
	}
	return page.Results, nil
}

func (g *gcoreDNS) cdnTarget(ctx context.Context) (string, error) {
	var client struct {
		Cname string `json:"cname"`
	}
	if err := g.do(ctx, http.MethodGet, "/cdn/clients/me", nil, &client); err != nil {
		return "", err
	}
	if client.Cname == "" {
		return "", fmt.Errorf("gcore gave no cdn cname")
	}
	return strings.TrimSuffix(client.Cname, "."), nil
}

func (g *gcoreDNS) cdnResources(ctx context.Context) ([]cdnResource, error) {
	return cdnList[cdnResource](ctx, g, "/cdn/resources")
}

func (g *gcoreDNS) certificate(ctx context.Context, name string) (*sslData, error) {
	certs, err := cdnList[sslData](ctx, g, "/cdn/sslData")
	if err != nil {
		return nil, err
	}
	for i := range certs {
		if certs[i].Name == name && !certs[i].Deleted {
			return &certs[i], nil
		}
	}
	return nil, nil
}

// uploadCertificate replaces cert in place, which every resource using it picks up, or adds it new.
func (g *gcoreDNS) uploadCertificate(ctx context.Context, cert *sslData, name string, issued *issuedCert) error {
	body := map[string]any{"name": name, "sslCertificate": issued.cert, "sslPrivateKey": issued.key}
	if cert == nil {
		return g.do(ctx, http.MethodPost, "/cdn/sslData", body, nil)
	}
	return g.do(ctx, http.MethodPut, fmt.Sprintf("/cdn/sslData/%d", cert.ID), body, nil)
}

func (g *gcoreDNS) createResource(ctx context.Context, name, primary, origin string, certID int64) error {
	on := map[string]any{"enabled": true, "value": true}
	zero := map[string]any{"enabled": true, "value": "0s"}
	return g.do(ctx, http.MethodPost, "/cdn/resources", map[string]any{
		"name":               name,
		"cname":              primary,
		"origin":             origin,
		"originProtocol":     "HTTPS",
		"active":             true,
		"secondaryHostnames": []string{},
		"sslEnabled":         true,
		"sslData":            certID,
		"options": map[string]any{
			"forward_host_header":    on,
			"websockets":             on,
			"redirect_http_to_https": on,
			"edge_cache_settings":    zero,
			"browser_cache_settings": zero,
			"proxy_read_timeout":     map[string]any{"enabled": true, "value": "120s"},
		},
	}, nil)
}

// setHostnames replaces the resource's whole list.
func (g *gcoreDNS) setHostnames(ctx context.Context, id int64, hosts []string) error {
	return g.do(ctx, http.MethodPatch, fmt.Sprintf("/cdn/resources/%d", id), map[string]any{
		"secondaryHostnames": hosts,
	}, nil)
}
