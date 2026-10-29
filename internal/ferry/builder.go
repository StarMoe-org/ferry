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
	"sort"
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

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex

	// How often and how long to poll a started CNB build; tests shorten them.
	pollEvery time.Duration
	pollFor   time.Duration
}

func (e *Engine) initBuilder(pollEvery, pollFor time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	bm := &buildManager{
		eng:   e,
		hc:    &http.Client{Timeout: 30 * time.Second},
		jobs:  make(chan buildJob, 128),
		ctx:   ctx,
		stop:  cancel,
		locks: make(map[string]*sync.Mutex),

		pollEvery: pollEvery,
		pollFor:   pollFor,
	}
	for i := 0; i < buildWorkers; i++ {
		bm.wg.Add(1)
		go bm.worker()
	}
	e.buildQueue = bm
	bm.resumeWatches()
}

func (b *buildManager) Close() {
	b.stop()
	b.wg.Wait()
}

func (b *buildManager) acquireLocks(keys ...string) func() {
	uniq := make(map[string]struct{}, len(keys))
	sorted := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		if _, ok := uniq[k]; !ok {
			uniq[k] = struct{}{}
			sorted = append(sorted, k)
		}
	}
	sort.Strings(sorted)

	b.locksMu.Lock()
	mus := make([]*sync.Mutex, len(sorted))
	for i, k := range sorted {
		m := b.locks[k]
		if m == nil {
			m = &sync.Mutex{}
			b.locks[k] = m
		}
		mus[i] = m
	}
	b.locksMu.Unlock()

	for _, m := range mus {
		m.Lock()
	}
	return func() {
		for i := len(mus) - 1; i >= 0; i-- {
			mus[i].Unlock()
		}
	}
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

	unlock := b.acquireLocks("repo:"+job.source.Repo, "img:"+job.source.Image)
	dstBranch, orphanSHA, err := b.mirrorRepo(ctx, job.source, job.sha)
	unlock()

	if err == nil {
		var logURL, sn string
		logURL, sn, err = b.startCNBBuild(ctx, job.source, dstBranch, orphanSHA, job.sha)
		if err == nil {
			ev, uErr := b.eng.store.UpdateEvent(context.Background(), job.eventID, func(ev *state.Event) {
				ev.Status = "started"
				ev.LogURL = logURL
				ev.BuildSN = sn
				ev.Message = ""
			})
			if uErr == nil && sn != "" {
				b.watch(ev)
			}
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

func (b *buildManager) mirrorRepo(ctx context.Context, src state.Source, sha string) (dstBranch, orphanSHA string, err error) {
	if !validGitSHA.MatchString(sha) {
		return "", "", fmt.Errorf("invalid commit sha %q", sha)
	}
	ghRepo := strings.TrimSpace(src.Repo)
	owner, repoName, ok := strings.Cut(ghRepo, "/")
	if !ok || owner == "" || repoName == "" || strings.Contains(owner, "..") || strings.Contains(repoName, "..") ||
		strings.ContainsAny(owner, `/\ `) || strings.ContainsAny(repoName, `/\ `) {
		return "", "", fmt.Errorf("invalid repository %q", ghRepo)
	}
	img := strings.TrimSpace(src.Image)
	if img == "" || strings.Contains(img, "..") || strings.ContainsAny(img, " \t\r\n\\:") {
		return "", "", fmt.Errorf("invalid source image %q", src.Image)
	}

	cfg := b.eng.cfg
	dstBranch = cfg.CNB.BranchPrefix + img

	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir workDir: %w", err)
	}
	workDir := filepath.Join(cfg.WorkDir, owner+"__"+repoName)
	if rel, rErr := filepath.Rel(cfg.WorkDir, workDir); rErr != nil || rel == "." || strings.HasPrefix(rel, "..") {
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
	if tok := cfg.GitHub.TokenForOwner(owner); tok != "" {
		ghHeader = basicAuthHeader("x-access-token", tok)
	}
	if _, err := runGit(ctx, workDir, ghHeader, "fetch", "-q", "--depth=1", "--no-tags", srcURL, sha); err != nil {
		return "", "", fmt.Errorf("git fetch %s: %w", ghRepo, err)
	}

	authorEmail := strings.TrimSpace(cfg.Snapshot.Author.Email)
	if authorEmail == "" {
		authorEmail = "ferry@localhost"
	}
	msg := fmt.Sprintf("%s@%s", ghRepo, sha)
	out, err := runGitWithEnv(ctx, workDir, "", []string{
		"GIT_AUTHOR_NAME=ferry",
		"GIT_AUTHOR_EMAIL=" + authorEmail,
		"GIT_COMMITTER_NAME=ferry",
		"GIT_COMMITTER_EMAIL=" + authorEmail,
	}, "commit-tree", sha+"^{tree}", "-m", msg)
	if err != nil {
		return "", "", fmt.Errorf("git commit-tree: %w", err)
	}
	orphanSHA = strings.TrimSpace(out)

	dstURL := strings.TrimRight(cfg.CNB.Git, "/") + "/" + cfg.CNB.Repo + ".git"
	cnbHeader := basicAuthHeader("cnb", cfg.CNB.Token)
	if _, err := runGit(ctx, workDir, cnbHeader, "push", "-q", "--force", dstURL, orphanSHA+":refs/heads/"+dstBranch); err != nil {
		return "", "", fmt.Errorf("git push cnb: %w", err)
	}

	_, _ = runGit(ctx, workDir, "", "gc", "--prune=now", "-q")
	return dstBranch, orphanSHA, nil
}

func (b *buildManager) startCNBBuild(ctx context.Context, src state.Source, branch, orphanSHA, sourceSHA string) (logURL, sn string, err error) {
	cfg := b.eng.cfg
	rawPipe, err := os.ReadFile(cfg.CNB.PipelineFile)
	if err != nil {
		return "", "", fmt.Errorf("read pipelineFile: %w", err)
	}
	configYAML := strings.ReplaceAll(string(rawPipe), "__BRANCH__", branch)

	envMap := map[string]string{
		"IMAGE_NAME": src.Image,
		"SRC_REPO":   src.Repo,
		"SRC_SHA":    sourceSHA,
		"BUILD_ARGS": src.BuildArgs,
	}

	payload := map[string]any{
		"branch": branch,
		"sha":    orphanSHA,
		"event":  "api_trigger_build",
		"env":    envMap,
		"config": configYAML,
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", "", err
	}

	u := strings.TrimRight(cfg.CNB.API, "/") + "/" + cfg.CNB.Repo + "/-/build/start"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.CNB.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := b.hc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("cnb build start request: %w", err)
	}
	defer resp.Body.Close()
	respData, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", "", fmt.Errorf("cnb build start %d: %s", resp.StatusCode, strings.TrimSpace(string(respData)))
	}

	var parsed struct {
		Success     *bool  `json:"success"`
		BuildLogURL string `json:"buildLogUrl"`
		SN          string `json:"sn"`
		Message     string `json:"message"`
	}
	if err := json.Unmarshal(respData, &parsed); err == nil {
		if parsed.Success != nil && !*parsed.Success {
			return "", "", fmt.Errorf("cnb build start rejected: %s", strings.TrimSpace(string(respData)))
		}
		return parsed.BuildLogURL, parsed.SN, nil
	}
	return "", "", nil
}

func basicAuthHeader(username, token string) string {
	creds := base64.StdEncoding.EncodeToString([]byte(username + ":" + token))
	return "Authorization: Basic " + creds
}

func runGit(ctx context.Context, dir, authHeader string, args ...string) (string, error) {
	return runGitWithEnv(ctx, dir, authHeader, nil, args...)
}

func runGitWithEnv(ctx context.Context, dir, authHeader string, extraEnv []string, args ...string) (string, error) {
	var fullArgs []string
	if authHeader != "" {
		fullArgs = append(fullArgs, "-c", "http.extraHeader="+authHeader)
	}
	fullArgs = append(fullArgs, args...)

	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Dir = dir
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	env = append(env, extraEnv...)
	cmd.Env = env
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

	secrets := []string{
		e.cfg.CNB.Token,
		e.cfg.GitHub.WebhookSecret,
		e.cfg.Snapshot.Token,
		e.cfg.Auth.OIDC.ClientSecret,
		e.cfg.Auth.SessionKey,
	}
	for _, tok := range e.cfg.GitHub.Tokens {
		secrets = append(secrets, tok)
	}
	for _, cl := range e.cfg.Clusters {
		secrets = append(secrets, cl.Token)
	}

	// Sort longest first so substring secrets never leave partial prefixes of longer secrets.
	sort.Slice(secrets, func(i, j int) bool {
		return len(secrets[i]) > len(secrets[j])
	})

	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
		for _, user := range []string{"cnb", "x-access-token", e.cfg.Snapshot.Username} {
			if user != "" {
				b64 := base64.StdEncoding.EncodeToString([]byte(user + ":" + secret))
				s = strings.ReplaceAll(s, b64, "[REDACTED]")
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
