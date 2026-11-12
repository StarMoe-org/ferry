package ferry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/StarMoe-org/ferry/internal/kube"
)

// defaultClusterTimeout is how long one cluster may take inside any pass that touches every cluster
// (deploy, inventory, DNS sync, one cluster's part of a snapshot). A cluster that is down must cost
// the others nothing: it gets this long, then it is reported as failed and the pass goes on without
// it. It stays well under the 100 s Cloudflare and 120 s CI limits in front of /deploy.
const defaultClusterTimeout = 20 * time.Second

// clusterContext bounds the work done against one cluster.
func (e *Engine) clusterContext(ctx context.Context) (context.Context, context.CancelFunc) {
	d := e.clusterTimeout
	if d <= 0 {
		d = defaultClusterTimeout
	}
	return context.WithTimeout(ctx, d)
}

// clusterError says plainly when a cluster ran out of its own time, rather than a bare
// "context deadline exceeded" that reads as if the whole request had timed out.
func (e *Engine) clusterError(cctx, parent context.Context, cl *kube.Client, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) && parent.Err() == nil {
		d := e.clusterTimeout
		if d <= 0 {
			d = defaultClusterTimeout
		}
		return fmt.Errorf("cluster %s did not answer within %s: %w", cl.Name, d, err)
	}
	return err
}

type listResult struct {
	items []map[string]any
	err   error
}

// listAll GETs every list path on one cluster at once and returns the answers in path order, so a
// slow cluster costs one request's time instead of the sum of them.
func listAll(ctx context.Context, cl *kube.Client, paths []string) []listResult {
	out := make([]listResult, len(paths))
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var resp struct {
				Items []map[string]any `json:"items"`
			}
			if err := cl.Get(ctx, p, &resp); err != nil {
				out[i].err = err
				return
			}
			out[i].items = resp.Items
		}()
	}
	wg.Wait()
	return out
}

type workloadTarget struct {
	ns string
	wk workloadKindSpec
}

// workloadTargets lists every (namespace, workload kind) pair and its list path, in a fixed order.
func workloadTargets(namespaces []string) ([]workloadTarget, []string) {
	var targets []workloadTarget
	var paths []string
	for _, ns := range namespaces {
		for _, wk := range workloadKinds {
			targets = append(targets, workloadTarget{ns, wk})
			paths = append(paths, wk.listPath(ns))
		}
	}
	return targets, paths
}
