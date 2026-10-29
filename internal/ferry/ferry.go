package ferry

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/StarMoe-org/ferry/internal/kube"
	"github.com/StarMoe-org/ferry/internal/state"
)

const (
	argoTrackingAnnotation = "argocd.argoproj.io/tracking-id"
	maxDeploySourceChars   = 300
)

var (
	ErrArgoManaged = errors.New("workload is managed by Argo CD")
	ErrBadInput    = errors.New("invalid input")
)

var (
	validImageRe = regexp.MustCompile(`^(?:[a-z0-9._-]+(?::[0-9]+)?/)?[a-z0-9./_-]+$`)
	validTagRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

// TruncateSource caps a deploy source string at 300 Unicode characters.
func TruncateSource(s string) string {
	runes := []rune(s)
	if len(runes) > maxDeploySourceChars {
		return string(runes[:maxDeploySourceChars])
	}
	return s
}

// Engine coordinates multi-cluster workload queries, rollouts, builds, and snapshots.
type Engine struct {
	cfg       *Config
	clusters  []*kube.Client
	byCluster map[string]*kube.Client
	store     *state.Store

	invMu sync.Mutex
	invAt time.Time
	inv   *Inventory

	// Optional hooks wired in later stages (builder, snapshots).
	buildQueue *buildManager
	snapRunner *snapshotRunner
}

// New constructs an Engine from pre-built Kubernetes clients and state store.
func New(cfg *Config, clients []*kube.Client, store *state.Store) (*Engine, error) {
	return newEngineWithPolling(cfg, clients, store, buildPollEvery, buildPollFor)
}

// newEngineWithPolling is New with the CNB build polling interval and deadline chosen by the caller.
func newEngineWithPolling(cfg *Config, clients []*kube.Client, store *state.Store, pollEvery, pollFor time.Duration) (*Engine, error) {
	if cfg == nil {
		return nil, errors.New("nil config")
	}
	cfg.ApplyDefaults()
	byName := make(map[string]*kube.Client, len(clients))
	for _, cl := range clients {
		byName[cl.Name] = cl
	}
	e := &Engine{
		cfg:       cfg,
		clusters:  clients,
		byCluster: byName,
		store:     store,
	}
	e.initBuilder(pollEvery, pollFor)
	e.initSnapshots()
	return e, nil
}

// NewFromConfig initializes kube clients, opens the configured state store, seeds sources if configured, and returns an Engine.
func NewFromConfig(ctx context.Context, cfg *Config) (*Engine, error) {
	clients := make([]*kube.Client, 0, len(cfg.Clusters))
	byName := make(map[string]*kube.Client, len(cfg.Clusters))
	for _, kc := range cfg.Clusters {
		cl, err := kube.New(kc)
		if err != nil {
			return nil, err
		}
		clients = append(clients, cl)
		byName[cl.Name] = cl
	}

	var st *state.Store
	var err error
	if strings.TrimSpace(cfg.State.File) != "" {
		st, err = state.NewFileStore(ctx, cfg.State.File)
		if err != nil {
			return nil, err
		}
	} else {
		clusterName, rErr := cfg.ResolveStateCluster()
		if rErr != nil {
			return nil, rErr
		}
		cl := byName[clusterName]
		if cl == nil {
			return nil, fmt.Errorf("state cluster %q client missing", clusterName)
		}
		st, err = state.NewConfigMapStore(ctx, cl, cfg.State.Kubernetes.Namespace, cfg.State.Kubernetes.Name)
		if err != nil {
			return nil, err
		}
	}

	if cfg.SeedSources != "" {
		entries, sErr := LoadSeedSources(cfg.SeedSources)
		if sErr != nil {
			return nil, sErr
		}
		inputs := make([]state.SeedInput, len(entries))
		for i, e := range entries {
			inputs[i] = state.SeedInput{
				Repo:      e.Repo,
				Branch:    e.Branch,
				Image:     e.Image,
				BuildArgs: e.BuildArgs,
			}
		}
		if err := st.SeedIfEmpty(ctx, inputs); err != nil {
			return nil, err
		}
	}

	return New(cfg, clients, st)
}

func (e *Engine) Config() *Config     { return e.cfg }
func (e *Engine) Store() *state.Store { return e.store }
func (e *Engine) Clusters() []*kube.Client {
	return e.clusters
}

func (e *Engine) autoDeployAnnotation() string {
	return e.cfg.AnnotationPrefix + "/auto-deploy"
}

func (e *Engine) InvalidateInventory() {
	e.invMu.Lock()
	e.inv = nil
	e.invAt = time.Time{}
	e.invMu.Unlock()
}

// Close stops background workers cleanly.
func (e *Engine) Close() {
	if e.buildQueue != nil {
		e.buildQueue.Close()
	}
	if e.snapRunner != nil {
		e.snapRunner.Close()
	}
}

// Runs reports whether containerImage belongs to image repository repo (with any tag or digest).
func Runs(containerImage, repo string) bool {
	if repo == "" {
		return false
	}
	return containerImage == repo ||
		strings.HasPrefix(containerImage, repo+":") ||
		strings.HasPrefix(containerImage, repo+"@")
}

// SplitImage splits a container image reference into (repo, tagOrDigest).
func SplitImage(containerImage string) (repo, tag string) {
	base := containerImage
	digest := ""
	if at := strings.IndexByte(base, '@'); at >= 0 {
		digest = base[at+1:]
		base = base[:at]
	}
	lastSlash := strings.LastIndexByte(base, '/')
	lastColon := strings.LastIndexByte(base, ':')
	if lastColon > lastSlash {
		repo = base[:lastColon]
		tag = base[lastColon+1:]
	} else {
		repo = base
	}
	if tag == "" && digest != "" {
		if len(digest) > 19 {
			tag = digest[:19]
		} else {
			tag = digest
		}
	}
	if tag == "" {
		tag = "latest"
	}
	return repo, tag
}

// ValidateImageAndTag validates image repository and tag arguments before patching workloads.
func ValidateImageAndTag(image, tag string) error {
	image = strings.TrimSpace(image)
	tag = strings.TrimSpace(tag)
	if image == "" || strings.HasPrefix(image, "/") || strings.HasSuffix(image, "/") || strings.HasSuffix(image, ":") {
		return fmt.Errorf("%w: invalid image repository %q", ErrBadInput, image)
	}
	lastSlash := strings.LastIndexByte(image, '/')
	if colon := strings.LastIndexByte(image, ':'); colon > lastSlash {
		return fmt.Errorf("%w: image %q must not include a tag", ErrBadInput, image)
	}
	if !validImageRe.MatchString(image) {
		return fmt.Errorf("%w: invalid image repository %q", ErrBadInput, image)
	}
	if !validTagRe.MatchString(tag) {
		return fmt.Errorf("%w: invalid tag %q", ErrBadInput, tag)
	}
	return nil
}

type workloadKindSpec struct {
	Kind       string
	Plural     string
	APIPrefix  string
	PodSpecPtr string
}

var workloadKinds = []workloadKindSpec{
	{Kind: "Deployment", Plural: "deployments", APIPrefix: "/apis/apps/v1", PodSpecPtr: "/spec/template/spec"},
	{Kind: "StatefulSet", Plural: "statefulsets", APIPrefix: "/apis/apps/v1", PodSpecPtr: "/spec/template/spec"},
	{Kind: "DaemonSet", Plural: "daemonsets", APIPrefix: "/apis/apps/v1", PodSpecPtr: "/spec/template/spec"},
	{Kind: "CronJob", Plural: "cronjobs", APIPrefix: "/apis/batch/v1", PodSpecPtr: "/spec/jobTemplate/spec/template/spec"},
}

func lookupWorkloadKind(kind string) (workloadKindSpec, bool) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "deployment", "deployments":
		return workloadKinds[0], true
	case "statefulset", "statefulsets":
		return workloadKinds[1], true
	case "daemonset", "daemonsets":
		return workloadKinds[2], true
	case "cronjob", "cronjobs":
		return workloadKinds[3], true
	default:
		return workloadKindSpec{}, false
	}
}

func (wk workloadKindSpec) listPath(ns string) string {
	return wk.APIPrefix + "/namespaces/" + ns + "/" + wk.Plural
}

func (wk workloadKindSpec) itemPath(ns, name string) string {
	return wk.APIPrefix + "/namespaces/" + ns + "/" + wk.Plural + "/" + name
}

type containerLoc struct {
	listField string // "initContainers" or "containers"
	index     int
	name      string
	image     string
}

func extractContainers(obj map[string]any, podSpecPtr string) []containerLoc {
	podSpec := getNestedMap(obj, strings.Split(strings.TrimPrefix(podSpecPtr, "/"), "/"))
	if podSpec == nil {
		return nil
	}
	var out []containerLoc
	for _, field := range []string{"initContainers", "containers"} {
		arr, _ := podSpec[field].([]any)
		for idx, raw := range arr {
			cm, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			cname, _ := cm["name"].(string)
			cimg, _ := cm["image"].(string)
			out = append(out, containerLoc{
				listField: field,
				index:     idx,
				name:      cname,
				image:     cimg,
			})
		}
	}
	return out
}

func getNestedMap(cur map[string]any, path []string) map[string]any {
	m := cur
	for _, seg := range path {
		next, ok := m[seg].(map[string]any)
		if !ok {
			return nil
		}
		m = next
	}
	return m
}

func extractAnnotations(obj map[string]any) (ann map[string]string, hasAnnotationsMap bool) {
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		return nil, false
	}
	rawAnn, exists := meta["annotations"]
	if !exists || rawAnn == nil {
		return nil, false
	}
	annMap, ok := rawAnn.(map[string]any)
	if !ok {
		return nil, false
	}
	out := make(map[string]string, len(annMap))
	for k, v := range annMap {
		if sv, ok := v.(string); ok {
			out[k] = sv
		}
	}
	return out, true
}

// Deploy updates every matching workload across all configured clusters and namespaces (or previews when dryRun is true).
func (e *Engine) Deploy(ctx context.Context, actor, image, tag, source string, dryRun bool) ([]state.Result, *state.Event, error) {
	image = strings.TrimSpace(image)
	tag = strings.TrimSpace(tag)
	source = strings.TrimSpace(TruncateSource(source))
	if err := ValidateImageAndTag(image, tag); err != nil {
		return nil, nil, err
	}
	want := image + ":" + tag
	autoKey := e.autoDeployAnnotation()

	perCluster := make([][]state.Result, len(e.clusters))
	var wg sync.WaitGroup
	for ci, cl := range e.clusters {
		wg.Add(1)
		go func(ci int, cl *kube.Client) {
			defer wg.Done()
			var local []state.Result
			for _, ns := range e.cfg.Namespaces {
				for _, wk := range workloadKinds {
					var listResp struct {
						Items []map[string]any `json:"items"`
					}
					if err := cl.Get(ctx, wk.listPath(ns), &listResp); err != nil {
						local = append(local, state.Result{
							Cluster:   cl.Name,
							Namespace: ns,
							Kind:      wk.Kind,
							To:        want,
							Result:    "error",
							Error:     err.Error(),
						})
						continue
					}
					for _, item := range listResp.Items {
						meta, _ := item["metadata"].(map[string]any)
						wname, _ := meta["name"].(string)
						if !kube.ValidName(wname) {
							continue
						}
						ann, hasAnnMap := extractAnnotations(item)
						_, isArgo := ann[argoTrackingAnnotation]
						isPaused := ann[autoKey] == "false"

						containers := extractContainers(item, wk.PodSpecPtr)
						var toPatch []containerLoc
						for _, c := range containers {
							if !Runs(c.image, image) {
								continue
							}
							switch {
							case c.image == want:
								local = append(local, state.Result{
									Cluster:   cl.Name,
									Namespace: ns,
									Kind:      wk.Kind,
									Name:      wname,
									Container: c.name,
									From:      c.image,
									To:        want,
									Result:    "current",
								})
							case isArgo:
								local = append(local, state.Result{
									Cluster:   cl.Name,
									Namespace: ns,
									Kind:      wk.Kind,
									Name:      wname,
									Container: c.name,
									From:      c.image,
									To:        want,
									Result:    "argo-cd",
								})
							case isPaused:
								local = append(local, state.Result{
									Cluster:   cl.Name,
									Namespace: ns,
									Kind:      wk.Kind,
									Name:      wname,
									Container: c.name,
									From:      c.image,
									To:        want,
									Result:    "paused",
								})
							case dryRun:
								local = append(local, state.Result{
									Cluster:   cl.Name,
									Namespace: ns,
									Kind:      wk.Kind,
									Name:      wname,
									Container: c.name,
									From:      c.image,
									To:        want,
									Result:    "would-deploy",
								})
							default:
								toPatch = append(toPatch, c)
							}
						}

						if len(toPatch) == 0 {
							continue
						}
						patchOps := buildImageJSONPatch(wk.PodSpecPtr, toPatch, want, hasAnnMap, strings.TrimSpace(want+" "+source))
						err := cl.Patch(ctx, wk.itemPath(ns, wname)+"?fieldManager=ferry", "application/json-patch+json", patchOps, nil)
						for _, c := range toPatch {
							res := state.Result{
								Cluster:   cl.Name,
								Namespace: ns,
								Kind:      wk.Kind,
								Name:      wname,
								Container: c.name,
								From:      c.image,
								To:        want,
							}
							if err != nil {
								res.Result = "error"
								res.Error = err.Error()
							} else {
								res.Result = "deployed"
							}
							local = append(local, res)
						}
					}
				}
			}
			perCluster[ci] = local
		}(ci, cl)
	}
	wg.Wait()

	var results []state.Result
	for _, sub := range perCluster {
		results = append(results, sub...)
	}
	if results == nil {
		results = []state.Result{}
	}
	if dryRun {
		return results, nil, nil
	}

	status, deployedAny := summarizeResults(results)
	if deployedAny {
		e.InvalidateInventory()
	}
	ev, err := e.store.RecordDeployEvent(ctx, state.Event{
		Kind:    "deploy",
		Actor:   actor,
		Image:   image,
		Tag:     tag,
		Source:  source,
		Status:  status,
		Results: results,
	})
	if err != nil {
		return results, nil, err
	}
	return results, &ev, nil
}

// RolloutRequest specifies a targeted single-workload image tag update.
type RolloutRequest struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Image     string `json:"image"`
	Container string `json:"container,omitempty"`
	Tag       string `json:"tag"`
}

// Rollout updates a single workload, ignoring auto-deploy:false because a user explicitly requested it,
// while still refusing Argo CD-managed workloads.
func (e *Engine) Rollout(ctx context.Context, actor string, req RolloutRequest) (state.Event, error) {
	cl := e.byCluster[req.Cluster]
	if cl == nil {
		return state.Event{}, fmt.Errorf("%w: unknown cluster %q", ErrBadInput, req.Cluster)
	}
	if !kube.ValidName(req.Namespace) || !kube.ValidName(req.Name) {
		return state.Event{}, fmt.Errorf("%w: invalid namespace or workload name", ErrBadInput)
	}
	wk, ok := lookupWorkloadKind(req.Kind)
	if !ok {
		return state.Event{}, fmt.Errorf("%w: unsupported workload kind %q", ErrBadInput, req.Kind)
	}
	req.Image = strings.TrimSpace(req.Image)
	req.Container = strings.TrimSpace(req.Container)
	req.Tag = strings.TrimSpace(req.Tag)
	if req.Image == "" && req.Container == "" {
		return state.Event{}, fmt.Errorf("%w: image or container is required", ErrBadInput)
	}
	if req.Tag == "" && req.Image != "" {
		lastSlash := strings.LastIndexByte(req.Image, '/')
		if colon := strings.LastIndexByte(req.Image, ':'); colon > lastSlash {
			req.Tag = req.Image[colon+1:]
			req.Image = req.Image[:colon]
		}
	}
	if !validTagRe.MatchString(req.Tag) {
		return state.Event{}, fmt.Errorf("%w: invalid tag %q", ErrBadInput, req.Tag)
	}
	if req.Image != "" {
		if err := ValidateImageAndTag(req.Image, req.Tag); err != nil {
			return state.Event{}, err
		}
	}

	var item map[string]any
	if err := cl.Get(ctx, wk.itemPath(req.Namespace, req.Name), &item); err != nil {
		if kube.IsNotFound(err) {
			return state.Event{}, state.ErrNotFound
		}
		return state.Event{}, err
	}
	ann, hasAnnMap := extractAnnotations(item)
	if _, isArgo := ann[argoTrackingAnnotation]; isArgo {
		return state.Event{}, ErrArgoManaged
	}

	containers := extractContainers(item, wk.PodSpecPtr)
	var toPatch []containerLoc
	var results []state.Result
	resolvedImage := req.Image

	for _, c := range containers {
		if req.Container != "" && c.name != req.Container {
			continue
		}
		repo, _ := SplitImage(c.image)
		if req.Image != "" && !Runs(c.image, req.Image) {
			continue
		}
		if resolvedImage == "" {
			resolvedImage = repo
		}
		want := resolvedImage + ":" + req.Tag
		if c.image == want {
			results = append(results, state.Result{
				Cluster:   cl.Name,
				Namespace: req.Namespace,
				Kind:      wk.Kind,
				Name:      req.Name,
				Container: c.name,
				From:      c.image,
				To:        want,
				Result:    "current",
			})
			continue
		}
		toPatch = append(toPatch, c)
	}

	if len(toPatch) == 0 && len(results) == 0 {
		return state.Event{}, fmt.Errorf("%w: no matching container in %s/%s", ErrBadInput, req.Namespace, req.Name)
	}

	if len(toPatch) > 0 {
		want := resolvedImage + ":" + req.Tag
		cause := fmt.Sprintf("%s rolled out by %s", want, actor)
		patchOps := buildImageJSONPatch(wk.PodSpecPtr, toPatch, want, hasAnnMap, cause)
		pErr := cl.Patch(ctx, wk.itemPath(req.Namespace, req.Name)+"?fieldManager=ferry", "application/json-patch+json", patchOps, nil)
		for _, c := range toPatch {
			r := state.Result{
				Cluster:   cl.Name,
				Namespace: req.Namespace,
				Kind:      wk.Kind,
				Name:      req.Name,
				Container: c.name,
				From:      c.image,
				To:        want,
			}
			if pErr != nil {
				r.Result = "error"
				r.Error = pErr.Error()
			} else {
				r.Result = "deployed"
			}
			results = append(results, r)
		}
	}

	status, deployedAny := summarizeResults(results)
	if deployedAny {
		e.InvalidateInventory()
	}
	return e.store.AppendEvent(ctx, state.Event{
		Kind:    "rollout",
		Actor:   actor,
		Image:   resolvedImage,
		Tag:     req.Tag,
		Status:  status,
		Results: results,
	})
}

// Rollback reverts each "deployed" container result from eventID back to its recorded From image,
// using a JSON-patch test against To so workloads modified since eventID fail safely with "changed since".
func (e *Engine) Rollback(ctx context.Context, actor, eventID string) (state.Event, error) {
	orig, ok := e.store.GetEvent(eventID)
	if !ok {
		return state.Event{}, state.ErrNotFound
	}
	if orig.Kind != "deploy" && orig.Kind != "rollout" && orig.Kind != "rollback" {
		return state.Event{}, fmt.Errorf("%w: event %s (%s) cannot be rolled back", ErrBadInput, eventID, orig.Kind)
	}

	var deployed []state.Result
	for _, r := range orig.Results {
		if r.Result == "deployed" && r.From != "" && r.To != "" {
			deployed = append(deployed, r)
		}
	}
	if len(deployed) == 0 {
		return state.Event{}, fmt.Errorf("%w: event %s has no deployed workloads to roll back", ErrBadInput, eventID)
	}

	var results []state.Result
	var targetRepo, targetTag string
	for _, r := range deployed {
		if targetRepo == "" {
			targetRepo, targetTag = SplitImage(r.From)
		}
		cl := e.byCluster[r.Cluster]
		if cl == nil {
			results = append(results, state.Result{
				Cluster: r.Cluster, Namespace: r.Namespace, Kind: r.Kind, Name: r.Name,
				Container: r.Container, From: r.To, To: r.From,
				Result: "error", Error: "unknown cluster",
			})
			continue
		}
		wk, ok := lookupWorkloadKind(r.Kind)
		if !ok || !kube.ValidName(r.Namespace) || !kube.ValidName(r.Name) {
			results = append(results, state.Result{
				Cluster: r.Cluster, Namespace: r.Namespace, Kind: r.Kind, Name: r.Name,
				Container: r.Container, From: r.To, To: r.From,
				Result: "error", Error: "invalid workload target",
			})
			continue
		}

		var item map[string]any
		if err := cl.Get(ctx, wk.itemPath(r.Namespace, r.Name), &item); err != nil {
			results = append(results, state.Result{
				Cluster: r.Cluster, Namespace: r.Namespace, Kind: wk.Kind, Name: r.Name,
				Container: r.Container, From: r.To, To: r.From,
				Result: "error", Error: err.Error(),
			})
			continue
		}
		ann, hasAnnMap := extractAnnotations(item)
		if _, isArgo := ann[argoTrackingAnnotation]; isArgo {
			results = append(results, state.Result{
				Cluster: r.Cluster, Namespace: r.Namespace, Kind: wk.Kind, Name: r.Name,
				Container: r.Container, From: r.To, To: r.From,
				Result: "argo-cd",
			})
			continue
		}

		containers := extractContainers(item, wk.PodSpecPtr)
		var matched *containerLoc
		for i := range containers {
			if containers[i].name == r.Container {
				matched = &containers[i]
				break
			}
		}
		if matched == nil {
			results = append(results, state.Result{
				Cluster: r.Cluster, Namespace: r.Namespace, Kind: wk.Kind, Name: r.Name,
				Container: r.Container, From: r.To, To: r.From,
				Result: "error", Error: "container not found",
			})
			continue
		}
		if matched.image != r.To {
			results = append(results, state.Result{
				Cluster: r.Cluster, Namespace: r.Namespace, Kind: wk.Kind, Name: r.Name,
				Container: r.Container, From: matched.image, To: r.From,
				Result: "error", Error: "changed since",
			})
			continue
		}

		// Patch using test=r.To and replace=r.From.
		patchOps := buildImageJSONPatch(wk.PodSpecPtr, []containerLoc{*matched}, r.From, hasAnnMap, r.From+" rollback:"+eventID)
		err := cl.Patch(ctx, wk.itemPath(r.Namespace, r.Name)+"?fieldManager=ferry", "application/json-patch+json", patchOps, nil)
		if err != nil {
			errMsg := err.Error()
			if kube.IsUnprocessable(err) {
				errMsg = "changed since"
			}
			results = append(results, state.Result{
				Cluster: r.Cluster, Namespace: r.Namespace, Kind: wk.Kind, Name: r.Name,
				Container: r.Container, From: r.To, To: r.From,
				Result: "error", Error: errMsg,
			})
			continue
		}
		results = append(results, state.Result{
			Cluster: r.Cluster, Namespace: r.Namespace, Kind: wk.Kind, Name: r.Name,
			Container: r.Container, From: r.To, To: r.From,
			Result: "deployed",
		})
	}

	status, deployedAny := summarizeResults(results)
	if deployedAny {
		e.InvalidateInventory()
	}
	return e.store.AppendEvent(ctx, state.Event{
		Kind:    "rollback",
		Actor:   actor,
		Image:   targetRepo,
		Tag:     targetTag,
		Source:  "rollback:" + eventID,
		Status:  status,
		Results: results,
	})
}

// PauseRequest toggles the <prefix>/auto-deploy annotation on one workload.
type PauseRequest struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Paused    bool   `json:"paused"`
}

// Pause sets or clears <annotationPrefix>/auto-deploy="false" via JSON merge-patch and refuses Argo CD workloads.
func (e *Engine) Pause(ctx context.Context, actor string, req PauseRequest) (state.Event, error) {
	cl := e.byCluster[req.Cluster]
	if cl == nil {
		return state.Event{}, fmt.Errorf("%w: unknown cluster %q", ErrBadInput, req.Cluster)
	}
	if !kube.ValidName(req.Namespace) || !kube.ValidName(req.Name) {
		return state.Event{}, fmt.Errorf("%w: invalid namespace or workload name", ErrBadInput)
	}
	wk, ok := lookupWorkloadKind(req.Kind)
	if !ok {
		return state.Event{}, fmt.Errorf("%w: unsupported workload kind %q", ErrBadInput, req.Kind)
	}

	var item map[string]any
	if err := cl.Get(ctx, wk.itemPath(req.Namespace, req.Name), &item); err != nil {
		if kube.IsNotFound(err) {
			return state.Event{}, state.ErrNotFound
		}
		return state.Event{}, err
	}
	ann, _ := extractAnnotations(item)
	if _, isArgo := ann[argoTrackingAnnotation]; isArgo {
		return state.Event{}, ErrArgoManaged
	}

	var val any
	kind := "resume"
	msg := "resumed"
	if req.Paused {
		val = "false"
		kind = "pause"
		msg = "paused"
	}
	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				e.autoDeployAnnotation(): val,
			},
		},
	}
	if err := cl.Patch(ctx, wk.itemPath(req.Namespace, req.Name)+"?fieldManager=ferry", "application/merge-patch+json", patch, nil); err != nil {
		return state.Event{}, err
	}
	e.InvalidateInventory()

	var primaryRepo string
	if cs := extractContainers(item, wk.PodSpecPtr); len(cs) > 0 {
		primaryRepo, _ = SplitImage(cs[0].image)
	}
	return e.store.AppendEvent(ctx, state.Event{
		Kind:    kind,
		Actor:   actor,
		Image:   primaryRepo,
		Status:  "ok",
		Message: fmt.Sprintf("%s %s/%s/%s (%s)", msg, cl.Name, req.Namespace, wk.Kind, req.Name),
	})
}

func buildImageJSONPatch(podSpecPtr string, containers []containerLoc, want string, hasAnnMap bool, changeCause string) []map[string]any {
	ops := make([]map[string]any, 0, len(containers)*2+2)
	for _, c := range containers {
		ptr := podSpecPtr + "/" + c.listField + "/" + strconv.Itoa(c.index) + "/image"
		ops = append(ops,
			map[string]any{"op": "test", "path": ptr, "value": c.image},
			map[string]any{"op": "replace", "path": ptr, "value": want},
		)
	}
	if !hasAnnMap {
		ops = append(ops, map[string]any{
			"op":    "add",
			"path":  "/metadata/annotations",
			"value": map[string]any{},
		})
	}
	ops = append(ops, map[string]any{
		"op":    "add",
		"path":  "/metadata/annotations/kubernetes.io~1change-cause",
		"value": changeCause,
	})
	return ops
}

func summarizeResults(results []state.Result) (status string, deployedAny bool) {
	var hasErr bool
	for _, r := range results {
		if r.Result == "deployed" {
			deployedAny = true
		}
		if r.Result == "error" {
			hasErr = true
		}
	}
	switch {
	case hasErr && deployedAny:
		return "partial", true
	case hasErr:
		return "error", false
	case deployedAny:
		return "ok", true
	default:
		return "skipped", false
	}
}
