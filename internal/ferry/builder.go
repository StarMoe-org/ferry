package ferry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/StarMoe-org/ferry/internal/state"
)

const buildWorkers = 2

type buildJob struct {
	eventID string
	source  state.Source
	sha     string
}

type buildManager struct {
	eng  *Engine
	hc   *http.Client
	jobs chan buildJob
	ctx  context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup

	repoLocksMu sync.Mutex
	repoLocks   map[string]*sync.Mutex
}

func (e *Engine) initBuilder() {
	ctx, cancel := context.WithCancel(context.Background())
	bm := &buildManager{
		eng:       e,
		hc:        &http.Client{Timeout: 30 * time.Second},
		jobs:      make(chan buildJob, 128),
		ctx:       ctx,
		stop:      cancel,
		repoLocks: make(map[string]*sync.Mutex),
	}
	for i := 0; i < buildWorkers; i++ {
		bm.wg.Add(1)
		go bm.worker()
	}
	e.buildQueue = bm
}

func (b *buildManager) Close() {
	b.stop()
	b.wg.Wait()
}

func (b *buildManager) lockForRepo(repo string) *sync.Mutex {
	b.repoLocksMu.Lock()
	defer b.repoLocksMu.Unlock()
	m := b.repoLocks[repo]
	if m == nil {
		m = &sync.Mutex{}
		b.repoLocks[repo] = m
	}
	return m
}

func (b *buildManager) enqueue(ctx context.Context, actor string, src state.Source, sha string) (state.Event, error) {
	cfg := b.eng.cfg
	if cfg.CNB.Repo == "" || cfg.CNB.Token == "" || cfg.CNB.PipelineFile == "" {
		return state.Event{}, ErrNotConfigured
	}
	short := shortSHA(sha)
	ev, err := b.eng.store.AppendEvent(ctx, state.Event{
		Kind:     "build",
		Actor:    actor,
		Image:    src.ImageRef(cfg.CNB.ImagePrefix),
		Tag:      short,
		Source:   fmt.Sprintf("github:%s@%s", src.Repo, short),
		SourceID: src.ID,
		Commit:   sha,
		Status:   "queued",
	})
	if err != nil {
		return state.Event{}, err
	}

	select {
	case b.jobs <- buildJob{eventID: ev.ID, source: src, sha: sha}:
		return ev, nil
	default:
		_, _ = b.eng.store.UpdateEvent(ctx, ev.ID, func(cur *state.Event) {
			cur.Status = "error"
			cur.Message = "build queue is full"
		})
		return state.Event{}, errors.New("build queue is full")
	}
}

func (b *buildManager) worker() {
	defer b.wg.Done()
	for {
		select {
		case <-b.ctx.Done():
			return
		case job := <-b.jobs:
			b.runJob(job)
		}
	}
}

func (b *buildManager) runJob(job buildJob) {
	ctx, cancel := context.WithTimeout(b.ctx, 4*time.Minute)
	defer cancel()

	mu := b.lockForRepo(job.source.Repo)
	mu.Lock()
	dstBranch, orphanSHA, err := b.mirrorRepo(ctx, job.source.Repo, job.sha)
	mu.Unlock()

	if err == nil {
		var logURL string
		logURL, err = b.startCNBBuild(ctx, job.source, dstBranch, orphanSHA, job.sha)
		if err == nil {
			_, _ = b.eng.store.UpdateEvent(context.Background(), job.eventID, func(ev *state.Event) {
				ev.Status = "started"
				ev.LogURL = logURL
				ev.Message = ""
			})
			return
		}
	}

	msg := b.eng.scrubSecrets(err.Error())
	_, _ = b.eng.store.UpdateEvent(context.Background(), job.eventID, func(ev *state.Event) {
		ev.Status = "error"
		ev.Message = msg
	})
}

var validGitSHA = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

func (b *buildManager) mirrorRepo(ctx context.Context, ghRepo, sha string) (dstBranch, orphanSHA string, err error) {
	if !validGitSHA.MatchString(sha) {
		return "", "", fmt.Errorf("invalid commit sha %q", sha)
	}
	slug := strings.ReplaceAll(ghRepo, "/", "--")
	if slug == "" || strings.Contains(slug, "..") || strings.ContainsAny(slug, `/\ `) {
		return "", "", fmt.Errorf("invalid repository slug for %q", ghRepo)
	}

	cfg := b.eng.cfg
	dstBranch = cfg.CNB.BranchPrefix + slug

	mirrorRoot := filepath.Join(cfg.WorkDir, "mirror")
	workDir := filepath.Join(mirrorRoot, slug)
	if rel, rErr := filepath.Rel(mirrorRoot, workDir); rErr != nil || strings.HasPrefix(rel, "..") {
		return "", "", fmt.Errorf("unsafe workdir for %q", ghRepo)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir mirror workdir: %w", err)
	}

	if _, stErr := os.Stat(filepath.Join(workDir, ".git")); errors.Is(stErr, os.ErrNotExist) {
		if _, err := runGit(ctx, workDir, "", "init", "-q"); err != nil {
			return "", "", fmt.Errorf("git init: %w", err)
		}
	}

	srcURL := strings.TrimRight(cfg.GitHub.Git, "/") + "/" + ghRepo + ".git"
	ghHeader := ""
	if cfg.GitHub.Token != "" {
		ghHeader = basicAuthHeader("x-access-token", cfg.GitHub.Token)
	}
	if _, err := runGit(ctx, workDir, ghHeader, "fetch", "--depth=1", "--no-tags", srcURL, sha); err != nil {
		return "", "", fmt.Errorf("git fetch %s: %w", ghRepo, err)
	}

	msg := fmt.Sprintf("mirror %s@%s", ghRepo, shortSHA(sha))
	out, err := runGit(ctx, workDir, "",
		"-c", "user.name=ci-relay",
		"-c", "user.email=ci@localhost",
		"commit-tree", sha+"^{tree}", "-m", msg,
	)
	if err != nil {
		return "", "", fmt.Errorf("git commit-tree: %w", err)
	}
	orphanSHA = strings.TrimSpace(out)

	dstURL := strings.TrimRight(cfg.CNB.Git, "/") + "/" + cfg.CNB.Repo + ".git"
	cnbHeader := basicAuthHeader("cnb", cfg.CNB.Token)
	if _, err := runGit(ctx, workDir, cnbHeader, "push", "--force", dstURL, orphanSHA+":refs/heads/"+dstBranch); err != nil {
		return "", "", fmt.Errorf("git push cnb: %w", err)
	}

	_, _ = runGit(ctx, workDir, "", "gc", "--prune=now", "-q")
	return dstBranch, orphanSHA, nil
}

func (b *buildManager) startCNBBuild(ctx context.Context, src state.Source, branch, orphanSHA, sourceSHA string) (string, error) {
	cfg := b.eng.cfg
	rawPipe, err := os.ReadFile(cfg.CNB.PipelineFile)
	if err != nil {
		return "", fmt.Errorf("read pipelineFile: %w", err)
	}
	configYAML := strings.ReplaceAll(string(rawPipe), "__BRANCH__", branch)

	envMap := map[string]string{
		"IMAGE_NAME":  src.Image,
		"SOURCE_REPO": src.Repo,
		"SOURCE_SHA":  sourceSHA,
	}
	if src.BuildArgs != "" {
		envMap["BUILD_ARGS"] = src.BuildArgs
	}

	payload := map[string]any{
		"branch": branch,
		"sha":    orphanSHA,
		"config": configYAML,
		"env":    envMap,
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	u := strings.TrimRight(cfg.CNB.API, "/") + "/" + cfg.CNB.Repo + "/-/build/start"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.CNB.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.cnb.api+json")

	resp, err := b.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("cnb build start request: %w", err)
	}
	defer resp.Body.Close()
	respData, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("cnb build start %d: %s", resp.StatusCode, strings.TrimSpace(string(respData)))
	}

	var parsed struct {
		Success     *bool  `json:"success"`
		BuildLogURL string `json:"buildLogUrl"`
		Message     string `json:"message"`
	}
	if err := json.Unmarshal(respData, &parsed); err == nil {
		if parsed.Success != nil && !*parsed.Success {
			return "", fmt.Errorf("cnb build start rejected: %s", strings.TrimSpace(string(respData)))
		}
		return parsed.BuildLogURL, nil
	}
	return "", nil
}

func basicAuthHeader(username, token string) string {
	creds := base64.StdEncoding.EncodeToString([]byte(username + ":" + token))
	return "Authorization: Basic " + creds
}

func runGit(ctx context.Context, dir, authHeader string, args ...string) (string, error) {
	var fullArgs []string
	if authHeader != "" {
		fullArgs = append(fullArgs, "-c", "http.extraHeader="+authHeader)
	}
	fullArgs = append(fullArgs, args...)

	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if authHeader != "" {
			msg = strings.ReplaceAll(msg, authHeader, "Authorization: [REDACTED]")
		}
		return "", fmt.Errorf("%w: %s", err, msg)
	}
	return string(out), nil
}

var authHeaderScrubRe = regexp.MustCompile(`(?i)authorization:\s*(basic|bearer)\s+\S+`)

func (e *Engine) scrubSecrets(s string) string {
	s = authHeaderScrubRe.ReplaceAllString(s, "Authorization: [REDACTED]")
	for _, secret := range []string{
		e.cfg.CNB.Token,
		e.cfg.GitHub.Token,
		e.cfg.GitHub.WebhookSecret,
		e.cfg.Snapshot.Token,
		e.cfg.Auth.OIDC.ClientSecret,
		e.cfg.Auth.SessionKey,
	} {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
			for _, user := range []string{"cnb", "x-access-token", e.cfg.Snapshot.Username} {
				if user != "" {
					b64 := base64.StdEncoding.EncodeToString([]byte(user + ":" + secret))
					s = strings.ReplaceAll(s, b64, "[REDACTED]")
				}
			}
		}
	}
	if len(s) > 400 {
		s = s[:400]
	}
	return s
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
