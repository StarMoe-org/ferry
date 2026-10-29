// Package fake provides an in-memory Kubernetes API server backed by net/http/httptest
// for offline tests and ferry -demo mode.
package fake

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/StarMoe-org/ferry/internal/kube"
)

type resourceMeta struct {
	apiVersion string
	kind       string
	listKind   string
	plural     string
}

var knownResources = map[string]resourceMeta{
	"apis/apps/v1:deployments":                        {apiVersion: "apps/v1", kind: "Deployment", listKind: "DeploymentList", plural: "deployments"},
	"apis/apps/v1:statefulsets":                       {apiVersion: "apps/v1", kind: "StatefulSet", listKind: "StatefulSetList", plural: "statefulsets"},
	"apis/apps/v1:daemonsets":                         {apiVersion: "apps/v1", kind: "DaemonSet", listKind: "DaemonSetList", plural: "daemonsets"},
	"apis/batch/v1:cronjobs":                          {apiVersion: "batch/v1", kind: "CronJob", listKind: "CronJobList", plural: "cronjobs"},
	"api/v1:services":                                 {apiVersion: "v1", kind: "Service", listKind: "ServiceList", plural: "services"},
	"api/v1:configmaps":                               {apiVersion: "v1", kind: "ConfigMap", listKind: "ConfigMapList", plural: "configmaps"},
	"api/v1:secrets":                                  {apiVersion: "v1", kind: "Secret", listKind: "SecretList", plural: "secrets"},
	"api/v1:persistentvolumeclaims":                   {apiVersion: "v1", kind: "PersistentVolumeClaim", listKind: "PersistentVolumeClaimList", plural: "persistentvolumeclaims"},
	"apis/networking.k8s.io/v1:ingresses":             {apiVersion: "networking.k8s.io/v1", kind: "Ingress", listKind: "IngressList", plural: "ingresses"},
	"apis/traefik.io/v1alpha1:middlewares":            {apiVersion: "traefik.io/v1alpha1", kind: "Middleware", listKind: "MiddlewareList", plural: "middlewares"},
}

var kindToPlural = map[string]resourceMeta{
	"Deployment":            knownResources["apis/apps/v1:deployments"],
	"StatefulSet":           knownResources["apis/apps/v1:statefulsets"],
	"DaemonSet":             knownResources["apis/apps/v1:daemonsets"],
	"CronJob":               knownResources["apis/batch/v1:cronjobs"],
	"Service":               knownResources["api/v1:services"],
	"ConfigMap":             knownResources["api/v1:configmaps"],
	"Secret":                knownResources["api/v1:secrets"],
	"PersistentVolumeClaim": knownResources["api/v1:persistentvolumeclaims"],
	"Ingress":               knownResources["apis/networking.k8s.io/v1:ingresses"],
	"Middleware":            knownResources["apis/traefik.io/v1alpha1:middlewares"],
}

// RecordedPatch captures a PATCH request received by the fake API server.
type RecordedPatch struct {
	Resource     string
	Namespace    string
	Name         string
	ContentType  string
	FieldManager string
	Body         []byte
}

type objKey struct {
	plural    string
	namespace string
	name      string
}

// Server is an in-memory Kubernetes API server for tests and -demo mode.
type Server struct {
	HTTP *httptest.Server

	mu             sync.Mutex
	gitVersion     string
	token          string
	disableTraefik bool
	conflictOnce   bool
	forbidden      map[string]bool
	rev            int64
	objects        map[objKey]map[string]any
	patches        []RecordedPatch
}

// New starts an HTTP fake Kubernetes API server.
func New() *Server {
	s := newState()
	s.HTTP = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	return s
}

// NewTLS starts an HTTPS fake Kubernetes API server with a self-signed certificate.
func NewTLS() *Server {
	s := newState()
	s.HTTP = httptest.NewTLSServer(http.HandlerFunc(s.serveHTTP))
	return s
}

func newState() *Server {
	return &Server{
		gitVersion: "v1.34.1+k3s1",
		forbidden:  make(map[string]bool),
		objects:    make(map[objKey]map[string]any),
	}
}

// Close shuts down the underlying httptest.Server.
func (s *Server) Close() {
	if s.HTTP != nil {
		s.HTTP.Close()
	}
}

// URL returns the base URL of the fake API server.
func (s *Server) URL() string {
	return s.HTTP.URL
}

// CAData returns the base64-encoded PEM certificate of a TLS server, or empty for plain HTTP.
func (s *Server) CAData() string {
	if s.HTTP.TLS == nil || len(s.HTTP.TLS.Certificates) == 0 {
		return ""
	}
	raw := s.HTTP.TLS.Certificates[0].Certificate[0]
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw})
	return base64.StdEncoding.EncodeToString(pemBytes)
}

// Config returns a kube.Config wired to this server.
func (s *Server) Config(name string) kube.Config {
	s.mu.Lock()
	tok := s.token
	s.mu.Unlock()
	return kube.Config{
		Name:   name,
		Server: s.URL(),
		CAData: s.CAData(),
		Token:  tok,
	}
}

// Client constructs a kube.Client connected to this server.
func (s *Server) Client(name string) (*kube.Client, error) {
	return kube.New(s.Config(name))
}

func (s *Server) SetGitVersion(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gitVersion = v
}

func (s *Server) SetToken(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = tok
}

func (s *Server) SetDisableTraefik(disabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disableTraefik = disabled
}

// SetForbidden configures a 403 response for method ("GET", "PATCH", "POST", "PUT") and plural resource (e.g. "deployments").
func (s *Server) SetForbidden(method, plural string, forbidden bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToUpper(method) + ":" + plural
	if forbidden {
		s.forbidden[key] = true
	} else {
		delete(s.forbidden, key)
	}
}

// InjectConflictOnce causes the next ConfigMap PUT to fail with 409 Conflict after bumping resourceVersion.
func (s *Server) InjectConflictOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conflictOnce = true
}

// Patches returns a copy of all recorded PATCH operations.
func (s *Server) Patches() []RecordedPatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RecordedPatch, len(s.patches))
	for i, p := range s.patches {
		cp := p
		cp.Body = append([]byte(nil), p.Body...)
		out[i] = cp
	}
	return out
}

// Seed inserts or replaces one or more Kubernetes objects in the store.
func (s *Server) Seed(objs ...map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, raw := range objs {
		obj := deepCopyMap(raw)
		kind, _ := obj["kind"].(string)
		rm, ok := kindToPlural[kind]
		if !ok {
			panic(fmt.Sprintf("fake kube: unsupported kind %q", kind))
		}
		if obj["apiVersion"] == nil || obj["apiVersion"] == "" {
			obj["apiVersion"] = rm.apiVersion
		}
		meta := ensureMap(obj, "metadata")
		ns, _ := meta["namespace"].(string)
		if ns == "" {
			ns = "default"
			meta["namespace"] = ns
		}
		name, _ := meta["name"].(string)
		if name == "" {
			panic(fmt.Sprintf("fake kube: %s missing metadata.name", kind))
		}
		s.rev++
		meta["resourceVersion"] = strconv.FormatInt(s.rev, 10)
		s.objects[objKey{plural: rm.plural, namespace: ns, name: name}] = obj
	}
}

// GetObject returns a deep copy of a stored object if present.
func (s *Server) GetObject(plural, namespace, name string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[objKey{plural: plural, namespace: namespace, name: name}]
	if !ok {
		return nil, false
	}
	return deepCopyMap(obj), true
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	expectedTok := s.token
	ver := s.gitVersion
	s.mu.Unlock()

	if expectedTok != "" {
		if r.Header.Get("Authorization") != "Bearer "+expectedTok {
			writeStatus(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
	}

	if r.URL.Path == "/version" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"gitVersion": ver})
		return
	}

	groupVer, ns, plural, name, ok := parseResourcePath(r.URL.Path)
	if !ok {
		writeStatus(w, http.StatusNotFound, "not found: "+r.URL.Path)
		return
	}

	s.mu.Lock()
	if s.disableTraefik && groupVer == "apis/traefik.io/v1alpha1" {
		s.mu.Unlock()
		writeStatus(w, http.StatusNotFound, "the server could not find the requested resource")
		return
	}
	rm, known := knownResources[groupVer+":"+plural]
	if !known {
		s.mu.Unlock()
		writeStatus(w, http.StatusNotFound, "the server could not find the requested resource")
		return
	}
	if s.forbidden[r.Method+":"+plural] {
		s.mu.Unlock()
		writeStatus(w, http.StatusForbidden, fmt.Sprintf("%s is forbidden on %s", strings.ToLower(r.Method), plural))
		return
	}
	s.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		if name == "" {
			s.handleList(w, rm, ns)
		} else {
			s.handleGet(w, rm, ns, name)
		}
	case http.MethodPost:
		if plural != "configmaps" || name != "" {
			writeStatus(w, http.StatusMethodNotAllowed, "POST only supported on configmap collection")
			return
		}
		s.handleCreateConfigMap(w, r, rm, ns)
	case http.MethodPut:
		if plural != "configmaps" || name == "" {
			writeStatus(w, http.StatusMethodNotAllowed, "PUT only supported on named configmap")
			return
		}
		s.handlePutConfigMap(w, r, rm, ns, name)
	case http.MethodPatch:
		if name == "" {
			writeStatus(w, http.StatusMethodNotAllowed, "PATCH requires resource name")
			return
		}
		s.handlePatch(w, r, rm, ns, name)
	default:
		writeStatus(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func parseResourcePath(p string) (groupVer, ns, plural, name string, ok bool) {
	p = strings.TrimPrefix(p, "/")
	parts := strings.Split(p, "/")
	// /api/v1/namespaces/{ns}/{plural}[/{name}]
	if len(parts) >= 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "namespaces" {
		if len(parts) == 5 {
			return "api/v1", parts[3], parts[4], "", true
		}
		if len(parts) == 6 {
			return "api/v1", parts[3], parts[4], parts[5], true
		}
		return "", "", "", "", false
	}
	// /apis/{group}/{ver}/namespaces/{ns}/{plural}[/{name}]
	if len(parts) >= 6 && parts[0] == "apis" && parts[3] == "namespaces" {
		gv := "apis/" + parts[1] + "/" + parts[2]
		if len(parts) == 6 {
			return gv, parts[4], parts[5], "", true
		}
		if len(parts) == 7 {
			return gv, parts[4], parts[5], parts[6], true
		}
		return "", "", "", "", false
	}
	return "", "", "", "", false
}

func (s *Server) handleList(w http.ResponseWriter, rm resourceMeta, ns string) {
	s.mu.Lock()
	var items []map[string]any
	for k, obj := range s.objects {
		if k.plural == rm.plural && k.namespace == ns {
			items = append(items, deepCopyMap(obj))
		}
	}
	s.mu.Unlock()

	sort.Slice(items, func(i, j int) bool {
		ni, _ := ensureMap(items[i], "metadata")["name"].(string)
		nj, _ := ensureMap(items[j], "metadata")["name"].(string)
		return ni < nj
	})
	if items == nil {
		items = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": rm.apiVersion,
		"kind":       rm.listKind,
		"metadata":   map[string]any{},
		"items":      items,
	})
}

func (s *Server) handleGet(w http.ResponseWriter, rm resourceMeta, ns, name string) {
	s.mu.Lock()
	obj, ok := s.objects[objKey{plural: rm.plural, namespace: ns, name: name}]
	var cp map[string]any
	if ok {
		cp = deepCopyMap(obj)
	}
	s.mu.Unlock()
	if !ok {
		writeStatus(w, http.StatusNotFound, fmt.Sprintf("%s %q not found", rm.plural, name))
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

func (s *Server) handleCreateConfigMap(w http.ResponseWriter, r *http.Request, rm resourceMeta, ns string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	meta := ensureMap(obj, "metadata")
	name, _ := meta["name"].(string)
	if name == "" {
		writeStatus(w, http.StatusBadRequest, "metadata.name is required")
		return
	}

	s.mu.Lock()
	key := objKey{plural: rm.plural, namespace: ns, name: name}
	if _, exists := s.objects[key]; exists {
		s.mu.Unlock()
		writeStatus(w, http.StatusConflict, fmt.Sprintf("configmaps %q already exists", name))
		return
	}
	s.rev++
	obj["apiVersion"] = rm.apiVersion
	obj["kind"] = rm.kind
	meta["namespace"] = ns
	meta["resourceVersion"] = strconv.FormatInt(s.rev, 10)
	s.objects[key] = deepCopyMap(obj)
	cp := deepCopyMap(obj)
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, cp)
}

func (s *Server) handlePutConfigMap(w http.ResponseWriter, r *http.Request, rm resourceMeta, ns, name string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.Lock()
	key := objKey{plural: rm.plural, namespace: ns, name: name}
	cur, exists := s.objects[key]
	if !exists {
		s.mu.Unlock()
		writeStatus(w, http.StatusNotFound, fmt.Sprintf("configmaps %q not found", name))
		return
	}
	if s.conflictOnce {
		s.conflictOnce = false
		s.rev++
		ensureMap(cur, "metadata")["resourceVersion"] = strconv.FormatInt(s.rev, 10)
	}
	curRV, _ := ensureMap(cur, "metadata")["resourceVersion"].(string)
	inMeta := ensureMap(obj, "metadata")
	inRV, _ := inMeta["resourceVersion"].(string)
	if inRV == "" || inRV != curRV {
		s.mu.Unlock()
		writeStatus(w, http.StatusConflict, fmt.Sprintf("Operation cannot be fulfilled on configmaps %q: the object has been modified", name))
		return
	}
	s.rev++
	obj["apiVersion"] = rm.apiVersion
	obj["kind"] = rm.kind
	inMeta["name"] = name
	inMeta["namespace"] = ns
	inMeta["resourceVersion"] = strconv.FormatInt(s.rev, 10)
	s.objects[key] = deepCopyMap(obj)
	cp := deepCopyMap(obj)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, cp)
}

func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request, rm resourceMeta, ns, name string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	ct := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	fm := r.URL.Query().Get("fieldManager")

	s.mu.Lock()
	key := objKey{plural: rm.plural, namespace: ns, name: name}
	cur, exists := s.objects[key]
	if !exists {
		s.mu.Unlock()
		writeStatus(w, http.StatusNotFound, fmt.Sprintf("%s %q not found", rm.plural, name))
		return
	}
	working := deepCopyMap(cur)

	switch ct {
	case "application/json-patch+json":
		if err := applyJSONPatch(working, body); err != nil {
			s.mu.Unlock()
			writeStatus(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	case "application/merge-patch+json":
		var patch map[string]any
		if err := json.Unmarshal(body, &patch); err != nil {
			s.mu.Unlock()
			writeStatus(w, http.StatusBadRequest, err.Error())
			return
		}
		applyMergePatch(working, patch)
	default:
		s.mu.Unlock()
		writeStatus(w, http.StatusUnsupportedMediaType, "unsupported patch type: "+ct)
		return
	}

	s.rev++
	meta := ensureMap(working, "metadata")
	meta["name"] = name
	meta["namespace"] = ns
	meta["resourceVersion"] = strconv.FormatInt(s.rev, 10)
	if fm != "" {
		meta["managedFields"] = []any{
			map[string]any{"manager": fm, "operation": "Update"},
		}
	}
	s.objects[key] = working
	s.patches = append(s.patches, RecordedPatch{
		Resource:     rm.plural,
		Namespace:    ns,
		Name:         name,
		ContentType:  ct,
		FieldManager: fm,
		Body:         append([]byte(nil), body...),
	})
	cp := deepCopyMap(working)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, cp)
}

type jsonPatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

func applyJSONPatch(doc map[string]any, raw []byte) error {
	var ops []jsonPatchOp
	if err := json.Unmarshal(raw, &ops); err != nil {
		return err
	}
	for i, op := range ops {
		tokens, err := parseJSONPointer(op.Path)
		if err != nil {
			return fmt.Errorf("op %d: %w", i, err)
		}
		var val any
		if len(op.Value) > 0 {
			if err := json.Unmarshal(op.Value, &val); err != nil {
				return fmt.Errorf("op %d value: %w", i, err)
			}
		}
		switch op.Op {
		case "test":
			got, ok := getAtPointer(doc, tokens)
			if !ok || !jsonEqual(got, val) {
				return fmt.Errorf("op %d test %s failed: got %v, want %v", i, op.Path, got, val)
			}
		case "replace":
			if _, ok := getAtPointer(doc, tokens); !ok {
				return fmt.Errorf("op %d replace %s: target does not exist", i, op.Path)
			}
			if err := setAtPointer(doc, tokens, val); err != nil {
				return fmt.Errorf("op %d replace %s: %w", i, op.Path, err)
			}
		case "add":
			if err := setAtPointer(doc, tokens, val); err != nil {
				return fmt.Errorf("op %d add %s: %w", i, op.Path, err)
			}
		case "remove":
			if err := removeAtPointer(doc, tokens); err != nil {
				return fmt.Errorf("op %d remove %s: %w", i, op.Path, err)
			}
		default:
			return fmt.Errorf("op %d: unsupported op %q", i, op.Op)
		}
	}
	return nil
}

func parseJSONPointer(p string) ([]string, error) {
	if p == "" || p[0] != '/' {
		return nil, fmt.Errorf("invalid JSON pointer %q", p)
	}
	parts := strings.Split(p[1:], "/")
	for i, part := range parts {
		part = strings.ReplaceAll(part, "~1", "/")
		part = strings.ReplaceAll(part, "~0", "~")
		parts[i] = part
	}
	return parts, nil
}

func getAtPointer(cur any, tokens []string) (any, bool) {
	for _, tok := range tokens {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[tok]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(tok)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

func setAtPointer(root map[string]any, tokens []string, val any) error {
	if len(tokens) == 0 {
		return fmt.Errorf("empty pointer")
	}
	parentTokens, last := tokens[:len(tokens)-1], tokens[len(tokens)-1]
	var parent any = root
	if len(parentTokens) > 0 {
		var ok bool
		parent, ok = getAtPointer(root, parentTokens)
		if !ok {
			return fmt.Errorf("parent path missing")
		}
	}
	switch node := parent.(type) {
	case map[string]any:
		node[last] = val
		return nil
	case []any:
		idx, err := strconv.Atoi(last)
		if err != nil || idx < 0 || idx >= len(node) {
			return fmt.Errorf("invalid slice index %q", last)
		}
		node[idx] = val
		return nil
	default:
		return fmt.Errorf("cannot index into %T", parent)
	}
}

func removeAtPointer(root map[string]any, tokens []string) error {
	if len(tokens) == 0 {
		return fmt.Errorf("empty pointer")
	}
	parentTokens, last := tokens[:len(tokens)-1], tokens[len(tokens)-1]
	var parent any = root
	if len(parentTokens) > 0 {
		var ok bool
		parent, ok = getAtPointer(root, parentTokens)
		if !ok {
			return fmt.Errorf("parent path missing")
		}
	}
	m, ok := parent.(map[string]any)
	if !ok {
		return fmt.Errorf("remove only supported on map parent")
	}
	if _, exists := m[last]; !exists {
		return fmt.Errorf("key %q not found", last)
	}
	delete(m, last)
	return nil
}

func applyMergePatch(target, patch map[string]any) {
	for k, v := range patch {
		if v == nil {
			delete(target, k)
			continue
		}
		pm, pok := v.(map[string]any)
		if pok {
			tm, tok := target[k].(map[string]any)
			if !tok {
				tm = make(map[string]any)
				target[k] = tm
			}
			applyMergePatch(tm, pm)
			continue
		}
		target[k] = v
	}
}

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return reflect.DeepEqual(a, b)
	}
	return bytes.Equal(ab, bb)
}

func ensureMap(m map[string]any, key string) map[string]any {
	sub, ok := m[key].(map[string]any)
	if !ok || sub == nil {
		sub = make(map[string]any)
		m[key] = sub
	}
	return sub
}

func deepCopyMap(m map[string]any) map[string]any {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}

func writeStatus(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{
		"kind":    "Status",
		"status":  "Failure",
		"code":    code,
		"message": msg,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
