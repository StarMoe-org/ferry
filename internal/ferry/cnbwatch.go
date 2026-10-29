package ferry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/StarMoe-org/ferry/internal/state"
)

const (
	buildPollEvery = 15 * time.Second
	buildPollFor   = 45 * time.Minute
)

// cnbBuildStatus is the part of GET <api>/<repo>/-/build/status/<sn> that Ferry reads.
type cnbBuildStatus struct {
	Status          string `json:"status"`
	PipelinesStatus map[string]struct {
		Stages []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"stages"`
	} `json:"pipelinesStatus"`
}

// failedStage names the first stage CNB reports as failed, if any.
func (s cnbBuildStatus) failedStage() string {
	keys := make([]string, 0, len(s.PipelinesStatus))
	for k := range s.PipelinesStatus {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, st := range s.PipelinesStatus[k].Stages {
			if buildFailed(st.Status) {
				if st.Name != "" {
					return st.Name
				}
				return st.ID
			}
		}
	}
	return ""
}

func buildFailed(status string) bool {
	switch strings.ToLower(status) {
	case "error", "failed", "failure", "cancel", "canceled", "cancelled", "timeout":
		return true
	}
	return false
}

// resumeWatches picks up builds that were still running when Ferry stopped.
func (b *buildManager) resumeWatches() {
	for _, ev := range b.eng.store.Events(state.EventsFilter{Kind: "build"}) {
		if ev.Status == "started" && ev.BuildSN != "" && time.Since(ev.At) < b.pollFor {
			b.watch(ev)
		}
	}
}

// watch polls the CNB build behind ev until it ends, then deploys its image when it succeeded.
// The pipeline cannot call POST /deploy itself: its CNB_TOKEN is only honoured inside CNB.
func (b *buildManager) watch(ev state.Event) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		deadline := ev.At.Add(b.pollFor)
		t := time.NewTicker(b.pollEvery)
		defer t.Stop()
		for {
			select {
			case <-b.ctx.Done():
				return
			case <-t.C:
			}
			// A deploy from the pipeline (or anyone) may already have settled the build.
			if cur, ok := b.eng.store.GetEvent(ev.ID); !ok || cur.Status != "started" {
				return
			}
			st, err := b.buildStatus(ev.BuildSN)
			switch {
			case err == nil && strings.EqualFold(st.Status, "success"):
				b.built(ev)
				return
			case err == nil && buildFailed(st.Status):
				msg := "CNB build " + strings.ToLower(st.Status)
				if stage := st.failedStage(); stage != "" {
					msg += " in stage " + stage
				}
				b.finish(ev.ID, "error", msg)
				return
			}
			// Still running, or a transient error: keep polling until the deadline.
			if time.Now().After(deadline) {
				b.finish(ev.ID, "error", fmt.Sprintf("gave up waiting for the CNB build after %s", b.pollFor))
				return
			}
		}
	}()
}

func (b *buildManager) buildStatus(sn string) (cnbBuildStatus, error) {
	cfg := b.eng.cfg
	u := strings.TrimRight(cfg.CNB.API, "/") + "/" + cfg.CNB.Repo + "/-/build/status/" + url.PathEscape(sn)
	ctx, cancel := context.WithTimeout(b.ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return cnbBuildStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.CNB.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := b.hc.Do(req)
	if err != nil {
		return cnbBuildStatus{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return cnbBuildStatus{}, fmt.Errorf("cnb build status %d", resp.StatusCode)
	}
	var st cnbBuildStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return cnbBuildStatus{}, err
	}
	return st, nil
}

// built deploys a successful build's image (unless deployOnSuccess is off) and settles the build event.
func (b *buildManager) built(ev state.Event) {
	if !b.eng.cfg.CNB.EffectiveDeployOnSuccess() || len(ev.Commit) < 12 {
		b.finish(ev.ID, "ok", "built")
		return
	}
	source := ev.Source
	if repo, _, ok := strings.Cut(strings.TrimPrefix(ev.Source, "github:"), "@"); ok && strings.HasPrefix(ev.Source, "github:") {
		source = "https://github.com/" + repo + "/commit/" + ev.Commit
	}
	ctx, cancel := context.WithTimeout(b.ctx, 2*time.Minute)
	defer cancel()
	_, dep, err := b.eng.Deploy(ctx, "cnb", ev.Image, ev.Commit[:12], source, false)
	switch {
	case err != nil:
		b.finish(ev.ID, "error", "built, but the deploy failed: "+b.eng.scrubSecrets(err.Error()))
	case dep == nil:
		b.finish(ev.ID, "ok", "built")
	case dep.Status == "error" || dep.Status == "partial":
		b.finish(ev.ID, dep.Status, "built; deploy "+dep.Status)
	case dep.Status == "skipped":
		b.finish(ev.ID, "ok", "built; nothing runs this image")
	default:
		b.finish(ev.ID, "ok", "deployed")
	}
}

func (b *buildManager) finish(id, status, msg string) {
	_, _ = b.eng.store.UpdateEvent(context.Background(), id, func(ev *state.Event) {
		ev.Status = status
		ev.Message = msg
	})
}
