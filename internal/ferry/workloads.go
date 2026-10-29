package ferry

import (
	"context"
	"sync"
	"time"

	"github.com/StarMoe-org/ferry/internal/kube"
)

const inventoryTTL = 10 * time.Second

// Inventory is the cached snapshot of clusters and workloads returned to the SPA.
type Inventory struct {
	FetchedAt time.Time        `json:"fetchedAt"`
	Clusters  []ClusterSummary `json:"clusters"`
	Workloads []WorkloadItem   `json:"workloads"`
}

// ClusterSummary describes connectivity, API version, and health counts for one cluster.
type ClusterSummary struct {
	Name          string `json:"name"`
	Server        string `json:"server"`
	Version       string `json:"version,omitempty"`
	Reachable     bool   `json:"reachable"`
	Error         string `json:"error,omitempty"`
	WorkloadCount int    `json:"workloadCount"`
	NotReadyCount int    `json:"notReadyCount"`
}

// WorkloadItem describes one Deployment, StatefulSet, DaemonSet, or CronJob in a cluster.
type WorkloadItem struct {
	Cluster          string          `json:"cluster"`
	Namespace        string          `json:"namespace"`
	Kind             string          `json:"kind"`
	Name             string          `json:"name"`
	Containers       []ContainerItem `json:"containers"`
	Desired          int             `json:"desired"`
	Ready            int             `json:"ready"`
	Updated          int             `json:"updated"`
	Available        int             `json:"available"`
	Schedule         string          `json:"schedule,omitempty"`
	Suspend          bool            `json:"suspend,omitempty"`
	LastScheduleTime string          `json:"lastScheduleTime,omitempty"`
	ArgoCD           bool            `json:"argoCD"`
	Paused           bool            `json:"paused"`
	ChangeCause      string          `json:"changeCause,omitempty"`
	Healthy          bool            `json:"healthy"`
}

// ContainerItem describes one container or initContainer inside a workload.
type ContainerItem struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	Repo  string `json:"repo"`
	Tag   string `json:"tag"`
	Init  bool   `json:"init,omitempty"`
}

// Workloads returns the cluster + workload inventory, using a 10-second cache unless fresh is true.
func (e *Engine) Workloads(ctx context.Context, fresh bool) (Inventory, error) {
	if !fresh {
		e.invMu.Lock()
		if e.inv != nil && time.Since(e.invAt) < inventoryTTL {
			cached := *e.inv
			e.invMu.Unlock()
			return cached, nil
		}
		e.invMu.Unlock()
	}

	autoKey := e.autoDeployAnnotation()
	summaries := make([]ClusterSummary, len(e.clusters))
	perClusterItems := make([][]WorkloadItem, len(e.clusters))

	var wg sync.WaitGroup
	for ci, cl := range e.clusters {
		wg.Add(1)
		go func(ci int, cl *kube.Client) {
			defer wg.Done()
			cs := ClusterSummary{
				Name:   cl.Name,
				Server: cl.ServerHost(),
			}
			ver, err := cl.Version(ctx)
			if err != nil {
				cs.Reachable = false
				cs.Error = err.Error()
				summaries[ci] = cs
				return
			}
			cs.Reachable = true
			cs.Version = ver

			var items []WorkloadItem
			var firstListErr string
			for _, ns := range e.cfg.Namespaces {
				for _, wk := range workloadKinds {
					var listResp struct {
						Items []map[string]any `json:"items"`
					}
					if err := cl.Get(ctx, wk.listPath(ns), &listResp); err != nil {
						if firstListErr == "" {
							firstListErr = err.Error()
						}
						continue
					}
					for _, raw := range listResp.Items {
						wi, ok := parseWorkloadItem(cl.Name, ns, wk, raw, autoKey)
						if !ok {
							continue
						}
						items = append(items, wi)
						cs.WorkloadCount++
						if !wi.Healthy {
							cs.NotReadyCount++
						}
					}
				}
			}
			if firstListErr != "" && cs.Error == "" {
				cs.Error = firstListErr
			}
			summaries[ci] = cs
			perClusterItems[ci] = items
		}(ci, cl)
	}
	wg.Wait()

	var all []WorkloadItem
	for _, sub := range perClusterItems {
		all = append(all, sub...)
	}
	if all == nil {
		all = []WorkloadItem{}
	}

	inv := Inventory{
		FetchedAt: time.Now().UTC(),
		Clusters:  summaries,
		Workloads: all,
	}
	e.invMu.Lock()
	e.inv = &inv
	e.invAt = inv.FetchedAt
	e.invMu.Unlock()
	return inv, nil
}

func parseWorkloadItem(cluster, ns string, wk workloadKindSpec, obj map[string]any, autoKey string) (WorkloadItem, bool) {
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if !kube.ValidName(name) {
		return WorkloadItem{}, false
	}
	ann, _ := extractAnnotations(obj)
	_, isArgo := ann[argoTrackingAnnotation]
	isPaused := ann[autoKey] == "false"

	locs := extractContainers(obj, wk.PodSpecPtr)
	containers := make([]ContainerItem, len(locs))
	for i, c := range locs {
		repo, tag := SplitImage(c.image)
		containers[i] = ContainerItem{
			Name:  c.name,
			Image: c.image,
			Repo:  repo,
			Tag:   tag,
			Init:  c.listField == "initContainers",
		}
	}

	spec, _ := obj["spec"].(map[string]any)
	status, _ := obj["status"].(map[string]any)

	wi := WorkloadItem{
		Cluster:     cluster,
		Namespace:   ns,
		Kind:        wk.Kind,
		Name:        name,
		Containers:  containers,
		ArgoCD:      isArgo,
		Paused:      isPaused,
		ChangeCause: ann["kubernetes.io/change-cause"],
	}

	switch wk.Kind {
	case "Deployment", "StatefulSet":
		wi.Desired = intVal(spec, "replicas", 1)
		wi.Ready = intVal(status, "readyReplicas", 0)
		wi.Updated = intVal(status, "updatedReplicas", 0)
		wi.Available = intVal(status, "availableReplicas", wi.Ready)
		wi.Healthy = wi.Ready >= wi.Desired
	case "DaemonSet":
		wi.Desired = intVal(status, "desiredNumberScheduled", 0)
		wi.Ready = intVal(status, "numberReady", 0)
		wi.Updated = intVal(status, "updatedNumberScheduled", 0)
		wi.Available = intVal(status, "numberAvailable", wi.Ready)
		wi.Healthy = wi.Ready >= wi.Desired
	case "CronJob":
		wi.Schedule, _ = spec["schedule"].(string)
		wi.Suspend, _ = spec["suspend"].(bool)
		wi.LastScheduleTime, _ = status["lastScheduleTime"].(string)
		wi.Healthy = true
	}

	return wi, true
}

func intVal(m map[string]any, key string, def int) int {
	if m == nil {
		return def
	}
	raw, exists := m[key]
	if !exists || raw == nil {
		return def
	}
	switch v := raw.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	default:
		return def
	}
}
