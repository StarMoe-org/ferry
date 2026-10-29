// Package state persists Ferry's sources, deploy tokens, and recent activity events
// in either a Kubernetes ConfigMap or an atomic local JSON file.
package state

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/StarMoe-org/ferry/internal/kube"
)

const MaxEvents = 300

var ErrNotFound = errors.New("not found")

// Source maps a GitHub repository + branch to a container image and builder.
type Source struct {
	ID        string    `json:"id"`
	Repo      string    `json:"repo"`                // "owner/name"
	Branch    string    `json:"branch"`              // "main"
	Builder   string    `json:"builder"`             // "cnb" | "external"
	Image     string    `json:"image"`               // short name when cnb, full repo when external
	BuildArgs string    `json:"buildArgs,omitempty"` // passed to CNB as BUILD_ARGS
	CreatedAt time.Time `json:"createdAt"`
	Deleted   bool      `json:"deleted,omitempty"`
}

// ImageRef returns the full container image repository (without tag/digest) produced by this source.
func (s Source) ImageRef(cnbImagePrefix string) string {
	if s.Builder == "cnb" && cnbImagePrefix != "" && !strings.Contains(s.Image, "/") {
		return strings.TrimRight(cnbImagePrefix, "/") + "/" + s.Image
	}
	return s.Image
}

// Token is a persisted deploy token record (only SHA-256 hex digest is stored).
type Token struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"` // first 10 chars, for display
	SHA256    string     `json:"sha256"` // hex digest of the full token
	CreatedAt time.Time  `json:"createdAt"`
	CreatedBy string     `json:"createdBy"`
	LastUsed  *time.Time `json:"lastUsed,omitempty"`
}

// TokenInfo is the safe public representation returned by GET /api/tokens (omits SHA256).
type TokenInfo struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"createdAt"`
	CreatedBy string     `json:"createdBy"`
	LastUsed  *time.Time `json:"lastUsed,omitempty"`
}

func (t Token) Info() TokenInfo {
	return TokenInfo{
		ID:        t.ID,
		Name:      t.Name,
		Prefix:    t.Prefix,
		CreatedAt: t.CreatedAt,
		CreatedBy: t.CreatedBy,
		LastUsed:  t.LastUsed,
	}
}

// Event is an entry in Ferry's activity feed (newest first, capped at MaxEvents).
type Event struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`             // "deploy" | "rollout" | "rollback" | "build" | "snapshot" | "pause" | "resume"
	Actor    string    `json:"actor"`            // user email/sub, "token:<name>", "github", "schedule", "ci", "cnb"
	Image    string    `json:"image,omitempty"`  // repo without tag
	Tag      string    `json:"tag,omitempty"`    // image tag
	Source   string    `json:"source,omitempty"` // e.g. "github:org/app@1234567"
	SourceID string    `json:"sourceId,omitempty"`
	Commit   string    `json:"commit,omitempty"`
	LogURL   string    `json:"logUrl,omitempty"`
	BuildSN  string    `json:"buildSn,omitempty"` // CNB build serial number, polled until the build ends
	Status   string    `json:"status"`            // "ok" | "partial" | "error" | "queued" | "started" | "skipped"
	Message  string    `json:"message,omitempty"`
	Results  []Result  `json:"results,omitempty"`
}

// Result is the per-container outcome of a Deploy, Rollout, or Rollback call.
type Result struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Container string `json:"container"`
	From      string `json:"from"`
	To        string `json:"to"`
	Result    string `json:"result"` // "deployed" | "current" | "paused" | "argo-cd" | "would-deploy" | "error"
	Error     string `json:"error,omitempty"`
}

// SeedInput describes a legacy repos.json entry to seed when sources are empty.
type SeedInput struct {
	Repo      string
	Branch    string
	Image     string
	BuildArgs string
}

type persistedState struct {
	Sources []Source `json:"sources"`
	Tokens  []Token  `json:"tokens"`
}

type filePayload struct {
	Sources []Source `json:"sources"`
	Tokens  []Token  `json:"tokens"`
	Events  []Event  `json:"events"`
}

type Data struct {
	Sources []Source
	Tokens  []Token
	Events  []Event
}

func cloneData(d Data) Data {
	b, err := json.Marshal(filePayload{
		Sources: d.Sources,
		Tokens:  d.Tokens,
		Events:  d.Events,
	})
	if err != nil {
		panic(err)
	}
	var out filePayload
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	if out.Sources == nil {
		out.Sources = []Source{}
	}
	if out.Tokens == nil {
		out.Tokens = []Token{}
	}
	if out.Events == nil {
		out.Events = []Event{}
	}
	return Data{Sources: out.Sources, Tokens: out.Tokens, Events: out.Events}
}

type backend interface {
	load(ctx context.Context) (Data, error)
	mutate(ctx context.Context, cur Data, fn func(*Data) error) (Data, error)
}

// Store manages thread-safe access and persistence of Ferry's state.
type Store struct {
	mu   sync.Mutex
	b    backend
	data Data
}

// NewFileStore opens or initializes a JSON state file at path.
func NewFileStore(ctx context.Context, path string) (*Store, error) {
	fb := &fileBackend{path: path}
	d, err := fb.load(ctx)
	if err != nil {
		return nil, err
	}
	return &Store{b: fb, data: d}, nil
}

// NewConfigMapStore opens or creates the ConfigMap <namespace>/<name> on client.
func NewConfigMapStore(ctx context.Context, client *kube.Client, namespace, name string) (*Store, error) {
	if !kube.ValidName(namespace) || !kube.ValidName(name) {
		return nil, fmt.Errorf("invalid state configmap %q/%q", namespace, name)
	}
	cb := &configMapBackend{client: client, namespace: namespace, name: name}
	d, err := cb.load(ctx)
	if err != nil {
		return nil, err
	}
	return &Store{b: cb, data: d}, nil
}

// NewID generates a time-sortable unique identifier with prefix (e.g. "ev_0195..._a1b2c3d4").
func NewID(prefix string) string {
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	ms := time.Now().UTC().UnixMilli()
	if prefix == "" {
		return fmt.Sprintf("%013d-%s", ms, hex.EncodeToString(rnd[:]))
	}
	return fmt.Sprintf("%s_%013d_%s", prefix, ms, hex.EncodeToString(rnd[:]))
}

var nonSlugChars = regexp.MustCompile(`[^a-z0-9-]+`)

func slugSourceID(repo, branch string) string {
	base := strings.ToLower(strings.ReplaceAll(repo+"-"+branch, "/", "-"))
	base = nonSlugChars.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-")
	}
	var rnd [3]byte
	_, _ = rand.Read(rnd[:])
	if base == "" {
		return "src-" + hex.EncodeToString(rnd[:])
	}
	return base + "-" + hex.EncodeToString(rnd[:])
}

// SeedIfEmpty imports legacy repos.json entries when no sources exist yet (even soft-deleted ones).
func (s *Store) SeedIfEmpty(ctx context.Context, seeds []SeedInput) error {
	if len(seeds) == 0 {
		return nil
	}
	return s.update(ctx, func(d *Data) error {
		if len(d.Sources) > 0 {
			return nil
		}
		now := time.Now().UTC()
		for _, in := range seeds {
			d.Sources = append(d.Sources, Source{
				ID:        slugSourceID(in.Repo, in.Branch),
				Repo:      in.Repo,
				Branch:    in.Branch,
				Builder:   "cnb",
				Image:     in.Image,
				BuildArgs: in.BuildArgs,
				CreatedAt: now,
			})
		}
		return nil
	})
}

// Sources returns all non-deleted sources.
func (s *Store) Sources() []Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Source, 0, len(s.data.Sources))
	for _, src := range s.data.Sources {
		if !src.Deleted {
			out = append(out, src)
		}
	}
	return out
}

// GetSource returns a non-deleted source by ID.
func (s *Store) GetSource(id string) (Source, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, src := range s.data.Sources {
		if src.ID == id && !src.Deleted {
			return src, true
		}
	}
	return Source{}, false
}

// CreateSource validates and appends a new Source.
func (s *Store) CreateSource(ctx context.Context, in Source) (Source, error) {
	var created Source
	err := s.update(ctx, func(d *Data) error {
		in.Repo = strings.TrimSpace(in.Repo)
		in.Branch = strings.TrimSpace(in.Branch)
		if in.Branch == "" {
			in.Branch = "main"
		}
		in.Image = strings.TrimSpace(in.Image)
		if err := validateSource(in); err != nil {
			return err
		}
		in.ID = slugSourceID(in.Repo, in.Branch)
		in.CreatedAt = time.Now().UTC()
		in.Deleted = false
		d.Sources = append(d.Sources, in)
		created = in
		return nil
	})
	return created, err
}

// UpdateSource updates mutable fields of an existing non-deleted Source.
func (s *Store) UpdateSource(ctx context.Context, id string, patch Source) (Source, error) {
	var updated Source
	err := s.update(ctx, func(d *Data) error {
		for i := range d.Sources {
			cur := &d.Sources[i]
			if cur.ID == id && !cur.Deleted {
				next := *cur
				if v := strings.TrimSpace(patch.Repo); v != "" {
					next.Repo = v
				}
				if v := strings.TrimSpace(patch.Branch); v != "" {
					next.Branch = v
				}
				if v := strings.TrimSpace(patch.Builder); v != "" {
					next.Builder = v
				}
				if v := strings.TrimSpace(patch.Image); v != "" {
					next.Image = v
				}
				next.BuildArgs = patch.BuildArgs
				if err := validateSource(next); err != nil {
					return err
				}
				*cur = next
				updated = next
				return nil
			}
		}
		return ErrNotFound
	})
	return updated, err
}

// DeleteSource soft-deletes a source by ID so historical events still resolve its metadata.
func (s *Store) DeleteSource(ctx context.Context, id string) error {
	return s.update(ctx, func(d *Data) error {
		for i := range d.Sources {
			if d.Sources[i].ID == id && !d.Sources[i].Deleted {
				d.Sources[i].Deleted = true
				return nil
			}
		}
		return ErrNotFound
	})
}

func validateSource(s Source) error {
	parts := strings.Split(s.Repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(s.Repo, " \t\r\n") {
		return fmt.Errorf("invalid repo %q: expected owner/name", s.Repo)
	}
	if s.Branch == "" || strings.ContainsAny(s.Branch, " \t\r\n") {
		return fmt.Errorf("invalid branch %q", s.Branch)
	}
	if s.Builder != "cnb" && s.Builder != "external" {
		return fmt.Errorf("invalid builder %q: expected cnb or external", s.Builder)
	}
	if s.Image == "" || strings.ContainsAny(s.Image, " \t\r\n") {
		return fmt.Errorf("invalid image %q", s.Image)
	}
	return nil
}

// SourcesSnapshotJSON serializes non-deleted sources (and never tokens!) for <dir>/ferry.json in snapshots.
func (s *Store) SourcesSnapshotJSON() ([]byte, error) {
	srcs := s.Sources()
	payload := struct {
		Sources []Source `json:"sources"`
	}{Sources: srcs}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Tokens returns public metadata for all active deploy tokens (never hashes or raw tokens).
func (s *Store) Tokens() []TokenInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TokenInfo, len(s.data.Tokens))
	for i, t := range s.data.Tokens {
		out[i] = t.Info()
	}
	return out
}

// CreateToken generates a new deploy token, persists only its SHA-256 hex digest, and returns the plaintext once.
func (s *Store) CreateToken(ctx context.Context, name, createdBy string) (TokenInfo, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return TokenInfo{}, "", errors.New("token name is required")
	}
	var rawBytes [24]byte
	if _, err := rand.Read(rawBytes[:]); err != nil {
		return TokenInfo{}, "", err
	}
	plain := "fy_" + hex.EncodeToString(rawBytes[:])
	sum := sha256.Sum256([]byte(plain))
	digest := hex.EncodeToString(sum[:])

	rec := Token{
		ID:        NewID("tok"),
		Name:      name,
		Prefix:    plain[:10],
		SHA256:    digest,
		CreatedAt: time.Now().UTC(),
		CreatedBy: createdBy,
	}
	err := s.update(ctx, func(d *Data) error {
		d.Tokens = append(d.Tokens, rec)
		return nil
	})
	if err != nil {
		return TokenInfo{}, "", err
	}
	return rec.Info(), plain, nil
}

// RevokeToken deletes a deploy token by ID.
func (s *Store) RevokeToken(ctx context.Context, id string) error {
	return s.update(ctx, func(d *Data) error {
		for i := range d.Tokens {
			if d.Tokens[i].ID == id {
				d.Tokens = append(d.Tokens[:i], d.Tokens[i+1:]...)
				return nil
			}
		}
		return ErrNotFound
	})
}

// AuthenticateToken checks bearer against stored token SHA-256 digests in constant time
// and updates LastUsed when matched.
func (s *Store) AuthenticateToken(ctx context.Context, bearer string) (TokenInfo, bool) {
	if bearer == "" {
		return TokenInfo{}, false
	}
	sum := sha256.Sum256([]byte(bearer))
	gotHex := []byte(hex.EncodeToString(sum[:]))

	s.mu.Lock()
	matchedIdx := -1
	for i, tok := range s.data.Tokens {
		want := []byte(tok.SHA256)
		if len(want) == len(gotHex) && subtle.ConstantTimeCompare(gotHex, want) == 1 {
			matchedIdx = i
		}
	}
	if matchedIdx < 0 {
		s.mu.Unlock()
		return TokenInfo{}, false
	}
	tokID := s.data.Tokens[matchedIdx].ID
	info := s.data.Tokens[matchedIdx].Info()
	lastUsed := s.data.Tokens[matchedIdx].LastUsed
	s.mu.Unlock()

	now := time.Now().UTC()
	// Throttle LastUsed persistence to once per minute so burst CI traffic avoids excessive writes,
	// while always updating immediately on first use.
	if lastUsed == nil || now.Sub(*lastUsed) >= time.Minute {
		_ = s.update(ctx, func(d *Data) error {
			for i := range d.Tokens {
				if d.Tokens[i].ID == tokID {
					t := now
					d.Tokens[i].LastUsed = &t
					info = d.Tokens[i].Info()
					break
				}
			}
			return nil
		})
	}
	return info, true
}

// EventsFilter restricts the returned event slice.
type EventsFilter struct {
	Kind  string
	Image string
	Limit int
}

// Events returns newest-first events matching filter.
func (s *Store) Events(f EventsFilter) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := f.Limit
	if limit <= 0 || limit > MaxEvents {
		limit = MaxEvents
	}
	out := make([]Event, 0, min(limit, len(s.data.Events)))
	for _, ev := range s.data.Events {
		if f.Kind != "" && ev.Kind != f.Kind {
			continue
		}
		if f.Image != "" && ev.Image != f.Image {
			continue
		}
		cp := ev
		if len(ev.Results) > 0 {
			cp.Results = append([]Result(nil), ev.Results...)
		}
		out = append(out, cp)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// GetEvent finds an event by ID.
func (s *Store) GetEvent(id string) (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.data.Events {
		if ev.ID == id {
			cp := ev
			if len(ev.Results) > 0 {
				cp.Results = append([]Result(nil), ev.Results...)
			}
			return cp, true
		}
	}
	return Event{}, false
}

// AppendEvent prepends ev (filling ID and At if empty) and trims the list to MaxEvents.
func (s *Store) AppendEvent(ctx context.Context, ev Event) (Event, error) {
	if ev.ID == "" {
		ev.ID = NewID("ev")
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	err := s.update(ctx, func(d *Data) error {
		prependEvent(d, ev)
		return nil
	})
	return ev, err
}

// RecordDeployEvent prepends ev and, if ev.Kind == "deploy", links any matching "started" build event
// whose Image == ev.Image and Commit[:12] == ev.Tag -> Status="ok", Message="deployed".
func (s *Store) RecordDeployEvent(ctx context.Context, ev Event) (Event, error) {
	if ev.ID == "" {
		ev.ID = NewID("ev")
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	err := s.update(ctx, func(d *Data) error {
		if ev.Kind == "deploy" && ev.Image != "" && ev.Tag != "" {
			for i := range d.Events {
				b := &d.Events[i]
				if b.Kind == "build" && b.Status == "started" && b.Image == ev.Image && len(b.Commit) >= 12 && b.Commit[:12] == ev.Tag {
					b.Status = "ok"
					b.Message = "deployed"
					break
				}
			}
		}
		prependEvent(d, ev)
		return nil
	})
	return ev, err
}

// UpdateEvent modifies an existing event by ID in place.
func (s *Store) UpdateEvent(ctx context.Context, id string, fn func(*Event)) (Event, error) {
	var updated Event
	err := s.update(ctx, func(d *Data) error {
		for i := range d.Events {
			if d.Events[i].ID == id {
				fn(&d.Events[i])
				updated = d.Events[i]
				return nil
			}
		}
		return ErrNotFound
	})
	return updated, err
}

func prependEvent(d *Data, ev Event) {
	next := make([]Event, 0, min(len(d.Events)+1, MaxEvents))
	next = append(next, ev)
	remaining := MaxEvents - 1
	if len(d.Events) < remaining {
		remaining = len(d.Events)
	}
	next = append(next, d.Events[:remaining]...)
	d.Events = next
}

func (s *Store) update(ctx context.Context, fn func(*Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := s.b.mutate(ctx, s.data, fn)
	if err != nil {
		return err
	}
	s.data = next
	return nil
}

// --- File Backend ---

type fileBackend struct {
	path string
}

func (f *fileBackend) load(_ context.Context) (Data, error) {
	raw, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		d := Data{Sources: []Source{}, Tokens: []Token{}, Events: []Event{}}
		if err := f.writeAtomic(d); err != nil {
			return Data{}, err
		}
		return d, nil
	}
	if err != nil {
		return Data{}, fmt.Errorf("read state file %s: %w", f.path, err)
	}
	var fp filePayload
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &fp); err != nil {
			return Data{}, fmt.Errorf("parse state file %s: %w", f.path, err)
		}
	}
	return cloneData(Data{Sources: fp.Sources, Tokens: fp.Tokens, Events: fp.Events}), nil
}

func (f *fileBackend) mutate(_ context.Context, cur Data, fn func(*Data) error) (Data, error) {
	working := cloneData(cur)
	if err := fn(&working); err != nil {
		return Data{}, err
	}
	if err := f.writeAtomic(working); err != nil {
		return Data{}, err
	}
	return working, nil
}

func (f *fileBackend) writeAtomic(d Data) error {
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir state dir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(filePayload{
		Sources: d.Sources,
		Tokens:  d.Tokens,
		Events:  d.Events,
	}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, ".ferry-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, f.path); err != nil {
		return fmt.Errorf("rename state file: %w", err)
	}
	cleanup = false
	return nil
}

// --- ConfigMap Backend ---

type configMapBackend struct {
	client    *kube.Client
	namespace string
	name      string
	rv        string
}

type configMapObj struct {
	APIVersion string            `json:"apiVersion,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Metadata   configMapMetadata `json:"metadata"`
	Data       map[string]string `json:"data"`
}

type configMapMetadata struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

func (c *configMapBackend) itemPath() string {
	return "/api/v1/namespaces/" + c.namespace + "/configmaps/" + c.name
}

func (c *configMapBackend) colPath() string {
	return "/api/v1/namespaces/" + c.namespace + "/configmaps"
}

func (c *configMapBackend) load(ctx context.Context) (Data, error) {
	var cm configMapObj
	err := c.client.Get(ctx, c.itemPath(), &cm)
	if kube.IsNotFound(err) {
		empty := Data{Sources: []Source{}, Tokens: []Token{}, Events: []Event{}}
		body, mErr := marshalConfigMap(c.namespace, c.name, "", empty)
		if mErr != nil {
			return Data{}, mErr
		}
		var created configMapObj
		if cErr := c.client.Do(ctx, http.MethodPost, c.colPath(), body, "application/json", &created); cErr != nil {
			if kube.IsConflict(cErr) {
				if gErr := c.client.Get(ctx, c.itemPath(), &cm); gErr != nil {
					return Data{}, gErr
				}
			} else {
				return Data{}, fmt.Errorf("create state configmap %s/%s: %w", c.namespace, c.name, cErr)
			}
		} else {
			cm = created
		}
	} else if err != nil {
		return Data{}, fmt.Errorf("get state configmap %s/%s: %w", c.namespace, c.name, err)
	}

	d, err := decodeConfigMap(cm)
	if err != nil {
		return Data{}, err
	}
	c.rv = cm.Metadata.ResourceVersion
	return d, nil
}

func (c *configMapBackend) mutate(ctx context.Context, cur Data, fn func(*Data) error) (Data, error) {
	base := cur
	rv := c.rv

	const maxAttempts = 6
	for attempt := 0; attempt < maxAttempts; attempt++ {
		working := cloneData(base)
		if err := fn(&working); err != nil {
			return Data{}, err
		}
		body, err := marshalConfigMap(c.namespace, c.name, rv, working)
		if err != nil {
			return Data{}, err
		}
		var updated configMapObj
		err = c.client.Do(ctx, http.MethodPut, c.itemPath(), body, "application/json", &updated)
		if err == nil {
			c.rv = updated.Metadata.ResourceVersion
			return working, nil
		}
		if !kube.IsConflict(err) {
			return Data{}, fmt.Errorf("update state configmap %s/%s: %w", c.namespace, c.name, err)
		}
		// 409 Conflict: re-read latest ConfigMap and re-apply mutation closure.
		var freshCM configMapObj
		if gErr := c.client.Get(ctx, c.itemPath(), &freshCM); gErr != nil {
			return Data{}, fmt.Errorf("reload state configmap after conflict: %w", gErr)
		}
		freshData, dErr := decodeConfigMap(freshCM)
		if dErr != nil {
			return Data{}, dErr
		}
		base = freshData
		rv = freshCM.Metadata.ResourceVersion
	}
	return Data{}, fmt.Errorf("update state configmap %s/%s: conflict persisted after %d retries", c.namespace, c.name, maxAttempts)
}

func marshalConfigMap(ns, name, rv string, d Data) (configMapObj, error) {
	stBytes, err := json.Marshal(persistedState{
		Sources: d.Sources,
		Tokens:  d.Tokens,
	})
	if err != nil {
		return configMapObj{}, err
	}
	evBytes, err := json.Marshal(d.Events)
	if err != nil {
		return configMapObj{}, err
	}
	return configMapObj{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Metadata: configMapMetadata{
			Name:            name,
			Namespace:       ns,
			ResourceVersion: rv,
		},
		Data: map[string]string{
			"state.json":  string(stBytes),
			"events.json": string(evBytes),
		},
	}, nil
}

func decodeConfigMap(cm configMapObj) (Data, error) {
	var ps persistedState
	if raw := strings.TrimSpace(cm.Data["state.json"]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &ps); err != nil {
			return Data{}, fmt.Errorf("decode configmap state.json: %w", err)
		}
	}
	var evs []Event
	if raw := strings.TrimSpace(cm.Data["events.json"]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &evs); err != nil {
			return Data{}, fmt.Errorf("decode configmap events.json: %w", err)
		}
	}
	return cloneData(Data{Sources: ps.Sources, Tokens: ps.Tokens, Events: evs}), nil
}
