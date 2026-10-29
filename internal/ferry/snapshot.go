package ferry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/StarMoe-org/ferry/internal/kube"
	"github.com/StarMoe-org/ferry/internal/state"
)

const defaultSnapshotTimeout = 10 * time.Minute

var ErrNotConfigured = errors.New("feature is not configured")

// WebhookStatus describes the state of a GitHub push webhook on a source repository.
type WebhookStatus struct {
	Status     string `json:"status"` // "ok" | "missing" | "no-permission" | "unconfigured"
	WebhookURL string `json:"webhookUrl,omitempty"`
	HookID     int64  `json:"hookId,omitempty"`
	Message    string `json:"message,omitempty"`
}

// SnapshotResult is returned by synchronous Snapshot calls.
type SnapshotResult struct {
	Status string `json:"status"` // "committed" | "unchanged" | "queued"
	Commit string `json:"commit,omitempty"`
}

type snapshotKindSpec struct {
	APIVersion string
	APIPrefix  string
	Plural     string
	Kind       string
}

var snapshotKinds = []snapshotKindSpec{
	{APIVersion: "apps/v1", APIPrefix: "/apis/apps/v1", Plural: "deployments", Kind: "Deployment"},
	{APIVersion: "apps/v1", APIPrefix: "/apis/apps/v1", Plural: "statefulsets", Kind: "StatefulSet"},
	{APIVersion: "apps/v1", APIPrefix: "/apis/apps/v1", Plural: "daemonsets", Kind: "DaemonSet"},
	{APIVersion: "batch/v1", APIPrefix: "/apis/batch/v1", Plural: "cronjobs", Kind: "CronJob"},
	{APIVersion: "v1", APIPrefix: "/api/v1", Plural: "services", Kind: "Service"},
	{APIVersion: "v1", APIPrefix: "/api/v1", Plural: "configmaps", Kind: "ConfigMap"},
	{APIVersion: "v1", APIPrefix: "/api/v1", Plural: "secrets", Kind: "Secret"},
	{APIVersion: "v1", APIPrefix: "/api/v1", Plural: "persistentvolumeclaims", Kind: "PersistentVolumeClaim"},
	{APIVersion: "networking.k8s.io/v1", APIPrefix: "/apis/networking.k8s.io/v1", Plural: "ingresses", Kind: "Ingress"},
	{APIVersion: "traefik.io/v1alpha1", APIPrefix: "/apis/traefik.io/v1alpha1", Plural: "middlewares", Kind: "Middleware"},
}

var skipSecretTypes = map[string]bool{
	"kubernetes.io/service-account-token": true,
	"kubernetes.io/tls":                   true,
	"helm.sh/release.v1":                  true,
}

var dropAnnotationsRe = regexp.MustCompile(`^(kubectl\.kubernetes\.io/last-applied-configuration|argocd\.argoproj\.io/.*|deployment\.kubernetes\.io/revision|pv\.kubernetes\.io/.*|volume\..*)$`)

var dropServiceSpecFields = []string{
	"clusterIP",
	"clusterIPs",
	"ipFamilies",
	"ipFamilyPolicy",
	"internalTrafficPolicy",
}

type inflightSnapshot struct {
	done chan struct{}
	res  SnapshotResult
	err  error
}

type snapshotRunner struct {
	eng     *Engine
	timeout time.Duration
	mu      sync.Mutex
	call    *inflightSnapshot

	ctx  context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup
}

func (e *Engine) initSnapshots() {
	ctx, cancel := context.WithCancel(context.Background())
	sr := &snapshotRunner{
		eng:     e,
		timeout: defaultSnapshotTimeout,
		ctx:     ctx,
		stop:    cancel,
	}
	if strings.TrimSpace(e.cfg.Snapshot.Repo) != "" {
		hourUTC := e.cfg.Snapshot.EffectiveHourUTC()
		sr.wg.Add(1)
		go sr.scheduleLoop(hourUTC)
	}
	e.snapRunner = sr
}

func (s *snapshotRunner) setTimeout(d time.Duration) {
	s.mu.Lock()
	s.timeout = d
	s.mu.Unlock()
}

func (s *snapshotRunner) Close() {
	s.stop()
	s.wg.Wait()
}

func nextDailyUTCRun(now time.Time, hourUTC int) time.Time {
	u := now.UTC()
	target := time.Date(u.Year(), u.Month(), u.Day(), hourUTC, 0, 0, 0, time.UTC)
	if !target.After(u) {
		target = target.Add(24 * time.Hour)
	}
	return target
}

func (s *snapshotRunner) scheduleLoop(hourUTC int) {
	defer s.wg.Done()
	for {
		wait := time.Until(nextDailyUTCRun(time.Now(), hourUTC))
		timer := time.NewTimer(wait)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			call := s.startOrJoin("schedule")
			select {
			case <-call.done:
			case <-s.ctx.Done():
				return
			}
		}
	}
}

// QueueSnapshot triggers a single-flight snapshot in the background and returns immediately.
// The snapshot runs with a context.Background() + timeout decoupled from any HTTP request.
func (e *Engine) QueueSnapshot(actor string) error {
	if strings.TrimSpace(e.cfg.Snapshot.Repo) == "" {
		return ErrNotConfigured
	}
	e.snapRunner.startOrJoin(actor)
	return nil
}

// Snapshot exports cluster manifests and SOPS-encrypted secrets to the configured git repository.
// Concurrent requests join the single in-flight snapshot run, whose execution context is decoupled
// from individual caller contexts using context.Background() with a timeout.
func (e *Engine) Snapshot(ctx context.Context, actor string) (SnapshotResult, error) {
	if strings.TrimSpace(e.cfg.Snapshot.Repo) == "" {
		return SnapshotResult{}, ErrNotConfigured
	}
	call := e.snapRunner.startOrJoin(actor)
	select {
	case <-call.done:
		return call.res, call.err
	case <-ctx.Done():
		return SnapshotResult{}, ctx.Err()
	}
}

func (s *snapshotRunner) startOrJoin(actor string) *inflightSnapshot {
	s.mu.Lock()
	if s.call != nil {
		call := s.call
		s.mu.Unlock()
		return call
	}
	call := &inflightSnapshot{done: make(chan struct{})}
	s.call = call
	timeout := s.timeout
	if timeout <= 0 {
		timeout = defaultSnapshotTimeout
	}
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		runCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		res, err := s.eng.runSnapshotOnce(runCtx, actor)

		call.res = res
		call.err = err

		s.mu.Lock()
		s.call = nil
		close(call.done)
		s.mu.Unlock()
	}()

	return call
}

func (e *Engine) runSnapshotOnce(ctx context.Context, actor string) (SnapshotResult, error) {
	res, err := e.executeSnapshot(ctx)
	if err != nil {
		msg := e.scrubSecrets(err.Error())
		_, _ = e.store.AppendEvent(context.Background(), state.Event{
			Kind:    "snapshot",
			Actor:   actor,
			Status:  "error",
			Message: msg,
		})
		return SnapshotResult{}, errors.New(msg)
	}

	_, _ = e.store.AppendEvent(context.Background(), state.Event{
		Kind:    "snapshot",
		Actor:   actor,
		Commit:  res.Commit,
		Status:  "ok",
		Message: res.Status,
	})
	return res, nil
}

func (e *Engine) executeSnapshot(ctx context.Context) (SnapshotResult, error) {
	sc := e.cfg.Snapshot
	if err := os.MkdirAll(e.cfg.WorkDir, 0o755); err != nil {
		return SnapshotResult{}, fmt.Errorf("mkdir workDir: %w", err)
	}
	work, err := os.MkdirTemp(e.cfg.WorkDir, "snapshot-*")
	if err != nil {
		return SnapshotResult{}, fmt.Errorf("mkdirtemp snapshot: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	authHdr := ""
	if sc.Token != "" {
		user := sc.Username
		if user == "" {
			user = "x-access-token"
		}
		authHdr = basicAuthHeader(user, sc.Token)
	}

	// Shallow clone target branch; if remote repository is empty/unborn, fall back to plain clone + checkout -B.
	if _, err := runGit(ctx, e.cfg.WorkDir, authHdr, "clone", "-q", "--depth=1", "--branch", sc.Branch, sc.Repo, work); err != nil {
		_ = os.RemoveAll(work)
		if mkErr := os.MkdirAll(work, 0o755); mkErr != nil {
			return SnapshotResult{}, mkErr
		}
		if _, err2 := runGit(ctx, e.cfg.WorkDir, authHdr, "clone", "-q", "--depth=1", sc.Repo, work); err2 != nil {
			return SnapshotResult{}, fmt.Errorf("git clone snapshot repo: %w", err2)
		}
		if _, err3 := runGit(ctx, work, "", "checkout", "-q", "-B", sc.Branch); err3 != nil {
			return SnapshotResult{}, fmt.Errorf("git checkout %s: %w", sc.Branch, err3)
		}
	}

	snapSubDir := filepath.Clean(sc.Dir)
	if snapSubDir == "." || strings.HasPrefix(snapSubDir, "..") || filepath.IsAbs(snapSubDir) {
		return SnapshotResult{}, fmt.Errorf("unsafe snapshot.dir %q", sc.Dir)
	}
	snapRoot := filepath.Join(work, snapSubDir)
	if err := os.MkdirAll(snapRoot, 0o755); err != nil {
		return SnapshotResult{}, err
	}

	stateCluster, _ := e.cfg.ResolveStateCluster()

	for _, cl := range e.clusters {
		cdir := filepath.Join(snapRoot, cl.Name)
		if err := os.MkdirAll(cdir, 0o755); err != nil {
			return SnapshotResult{}, err
		}
		// Migrate from ci-relay's legacy single-namespace secret layout if present.
		_ = os.Remove(filepath.Join(cdir, "secrets.enc.yaml"))
		_ = os.Remove(filepath.Join(cdir, "secrets.sha256"))

		for _, ns := range e.cfg.Namespaces {
			var pubDocs []string
			var secDocs []string

			for _, sk := range snapshotKinds {
				listPath := sk.APIPrefix + "/namespaces/" + ns + "/" + sk.Plural
				var listResp struct {
					Items []map[string]any `json:"items"`
				}
				if err := cl.Get(ctx, listPath, &listResp); err != nil {
					if kube.IsNotFound(err) {
						// Optional CRD (e.g. Traefik Middleware) not installed on this cluster.
						continue
					}
					return SnapshotResult{}, fmt.Errorf("cluster %s %s/%s: %w", cl.Name, ns, sk.Plural, err)
				}

				sort.Slice(listResp.Items, func(i, j int) bool {
					ni, _ := nestedName(listResp.Items[i])
					nj, _ := nestedName(listResp.Items[j])
					return ni < nj
				})

				for _, rawItem := range listResp.Items {
					if e.isOwnStateConfigMap(cl.Name, stateCluster, ns, sk.Kind, rawItem) {
						continue
					}
					cleaned := CleanResource(rawItem, sk.APIVersion, sk.Kind)
					if cleaned == nil {
						continue
					}
					docBytes, mErr := yaml.Marshal(cleaned)
					if mErr != nil {
						return SnapshotResult{}, fmt.Errorf("marshal %s: %w", sk.Kind, mErr)
					}
					if sk.Kind == "Secret" {
						secDocs = append(secDocs, string(docBytes))
					} else {
						pubDocs = append(pubDocs, string(docBytes))
					}
				}
			}

			header := fmt.Sprintf("# cluster=%s namespace=%s\n", cl.Name, ns)
			pubContent := header + strings.Join(pubDocs, "---\n")
			if err := os.WriteFile(filepath.Join(cdir, ns+".yaml"), []byte(pubContent), 0o644); err != nil {
				return SnapshotResult{}, err
			}

			encPath := filepath.Join(cdir, ns+".secrets.enc.yaml")
			shaPath := filepath.Join(cdir, ns+".secrets.sha256")
			if len(secDocs) > 0 {
				if !sc.EffectiveSOPS() {
					_ = os.Remove(encPath)
					_ = os.Remove(shaPath)
					return SnapshotResult{}, fmt.Errorf("cluster %s namespace %s has %d secret(s) but snapshot.sops is disabled", cl.Name, ns, len(secDocs))
				}
				plain := strings.Join(secDocs, "---\n")
				sum := sha256.Sum256([]byte(plain))
				digest := hex.EncodeToString(sum[:])

				alreadyMatches := false
				if _, stErr := os.Stat(encPath); stErr == nil {
					if prevHash, hErr := os.ReadFile(shaPath); hErr == nil && strings.TrimSpace(string(prevHash)) == digest {
						alreadyMatches = true
					}
				}
				if !alreadyMatches {
					if err := encryptSecretsFile(ctx, work, encPath, plain, sc.AgePublicKey); err != nil {
						_ = os.Remove(shaPath)
						return SnapshotResult{}, err
					}
					if err := os.WriteFile(shaPath, []byte(digest+"\n"), 0o644); err != nil {
						return SnapshotResult{}, err
					}
				}
			} else {
				_ = os.Remove(encPath)
				_ = os.Remove(shaPath)
			}
		}
	}

	// Write <dir>/ferry.json containing sources only (never tokens!).
	ferryJSON, err := e.store.SourcesSnapshotJSON()
	if err != nil {
		return SnapshotResult{}, err
	}
	if err := os.WriteFile(filepath.Join(snapRoot, "ferry.json"), ferryJSON, 0o644); err != nil {
		return SnapshotResult{}, err
	}

	if _, err := runGit(ctx, work, "", "add", "-A", snapSubDir); err != nil {
		return SnapshotResult{}, fmt.Errorf("git add: %w", err)
	}

	diffCmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--quiet")
	diffCmd.Dir = work
	diffErr := diffCmd.Run()
	if diffErr == nil {
		head := currentHeadOrEmpty(ctx, work)
		return SnapshotResult{Status: "unchanged", Commit: head}, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(diffErr, &exitErr) || exitErr.ExitCode() != 1 {
		return SnapshotResult{}, fmt.Errorf("git diff --cached: %w", diffErr)
	}

	commitMsg := "snapshot: " + strings.Join(e.cfg.Namespaces, ", ")
	if _, err := runGit(ctx, work, "",
		"-c", "user.name="+sc.Author.Name,
		"-c", "user.email="+sc.Author.Email,
		"commit", "-q", "-m", commitMsg,
	); err != nil {
		return SnapshotResult{}, fmt.Errorf("git commit snapshot: %w", err)
	}
	commitSHA := currentHeadOrEmpty(ctx, work)
	if _, err := runGit(ctx, work, authHdr, "push", "-q", "origin", "HEAD:"+sc.Branch); err != nil {
		return SnapshotResult{}, fmt.Errorf("git push snapshot: %w", err)
	}

	return SnapshotResult{Status: "committed", Commit: commitSHA}, nil
}

func (e *Engine) isOwnStateConfigMap(cluster, stateCluster, ns, kind string, item map[string]any) bool {
	if kind != "ConfigMap" || e.cfg.State.Kubernetes == nil {
		return false
	}
	name, _ := nestedName(item)
	return cluster == stateCluster &&
		ns == e.cfg.State.Kubernetes.Namespace &&
		name == e.cfg.State.Kubernetes.Name
}

// CleanResource strips cluster-generated fields, managed child objects, noisy secrets, and dynamic annotations,
// matching ci-relay's clean() contract. Returns nil if the object should be omitted from snapshots.
func CleanResource(item map[string]any, defaultAPIVersion, defaultKind string) map[string]any {
	meta, _ := item["metadata"].(map[string]any)
	if meta == nil {
		return nil
	}
	if owners, ok := meta["ownerReferences"].([]any); ok && len(owners) > 0 {
		return nil
	}
	name, _ := meta["name"].(string)
	kind, _ := item["kind"].(string)
	if kind == "" {
		kind = defaultKind
	}
	apiVer, _ := item["apiVersion"].(string)
	if apiVer == "" {
		apiVer = defaultAPIVersion
	}

	if kind == "ConfigMap" && name == "kube-root-ca.crt" {
		return nil
	}
	if kind == "Secret" {
		secType, _ := item["type"].(string)
		if skipSecretTypes[secType] {
			return nil
		}
	}

	cleanMeta := map[string]any{
		"name":      name,
		"namespace": meta["namespace"],
	}
	if labels, ok := meta["labels"].(map[string]any); ok && len(labels) > 0 {
		cpLabels := make(map[string]any, len(labels))
		for k, v := range labels {
			cpLabels[k] = v
		}
		cleanMeta["labels"] = cpLabels
	}
	if anns, ok := meta["annotations"].(map[string]any); ok && len(anns) > 0 {
		cpAnns := make(map[string]any)
		for k, v := range anns {
			if !dropAnnotationsRe.MatchString(k) {
				cpAnns[k] = v
			}
		}
		if len(cpAnns) > 0 {
			cleanMeta["annotations"] = cpAnns
		}
	}

	out := map[string]any{
		"apiVersion": apiVer,
		"kind":       kind,
		"metadata":   cleanMeta,
	}
	for _, key := range []string{"type", "data", "stringData", "spec"} {
		if v, exists := item[key]; exists && v != nil {
			out[key] = deepCopyValue(v)
		}
	}
	if kind == "Service" {
		if specMap, ok := out["spec"].(map[string]any); ok {
			for _, f := range dropServiceSpecFields {
				delete(specMap, f)
			}
		}
	}
	return out
}

func encryptSecretsFile(ctx context.Context, repoDir, encPath, plain, ageRecipient string) error {
	if err := os.WriteFile(encPath, []byte(plain), 0o600); err != nil {
		return err
	}
	targetArg := encPath
	if rel, err := filepath.Rel(repoDir, encPath); err == nil && !strings.HasPrefix(rel, "..") {
		targetArg = rel
	}

	var args []string
	if recipient := strings.TrimSpace(ageRecipient); recipient != "" {
		args = []string{
			"--age", recipient,
			"--encrypted-regex", "^(data|stringData)$",
			"-e", "-i", targetArg,
		}
	} else {
		args = []string{"-e", "-i", targetArg}
	}

	cmd := exec.CommandContext(ctx, "sops", args...)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Never leave unencrypted secret material on disk if sops fails or is missing.
		_ = os.Remove(encPath)
		return fmt.Errorf("sops encrypt: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func nestedName(item map[string]any) (string, bool) {
	meta, _ := item["metadata"].(map[string]any)
	if meta == nil {
		return "", false
	}
	n, ok := meta["name"].(string)
	return n, ok
}

func currentHeadOrEmpty(ctx context.Context, dir string) string {
	out, err := runGit(ctx, dir, "", "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func deepCopyValue(v any) any {
	b, err := yaml.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := yaml.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}
