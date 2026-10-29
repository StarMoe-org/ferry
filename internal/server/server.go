package server

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/StarMoe-org/ferry/internal/ferry"
	"github.com/StarMoe-org/ferry/internal/state"
	"github.com/StarMoe-org/ferry/web"
)

const (
	maxJSONBodyBytes    = 1 << 20  // 1 MiB
	maxWebhookBodyBytes = 25 << 20 // 25 MiB
)

// Options customizes HTTP client dependencies for tests.
type Options struct {
	OIDCHTTPClient    *http.Client
	GitReadHTTPClient *http.Client
	SPAFS             fs.FS
}

// Server is Ferry's HTTP handler.
type Server struct {
	eng     *ferry.Engine
	browser *browserAuth
	ci      *ciAuthenticator
	spa     fs.FS
	mux     *http.ServeMux
}

// New constructs a Server for engine.
func New(eng *ferry.Engine, opts Options) (*Server, error) {
	spaFS := opts.SPAFS
	if spaFS == nil {
		var err error
		spaFS, err = web.Dist()
		if err != nil {
			return nil, err
		}
	}
	cfg := eng.Config()
	s := &Server{
		eng:     eng,
		browser: newBrowserAuth(cfg.Auth, opts.OIDCHTTPClient),
		ci:      newCIAuthenticator(eng.Store(), cfg.Auth.GitRead, opts.GitReadHTTPClient),
		spa:     spaFS,
		mux:     http.NewServeMux(),
	}
	s.registerRoutes()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.mux.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	// Unauthenticated healthz
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Browser OIDC endpoints
	s.mux.HandleFunc("GET /auth/login", s.browser.handleLogin)
	s.mux.HandleFunc("GET /auth/callback", s.browser.handleCallback)
	s.mux.HandleFunc("POST /auth/logout", s.browser.handleLogout)

	// GitHub webhook + legacy POST / alias
	s.mux.HandleFunc("POST /webhook/github", s.handleWebhook)
	s.mux.HandleFunc("POST /{$}", s.handleWebhook)

	// Legacy CI-only endpoints
	s.mux.HandleFunc("POST /deploy", s.handleLegacyDeploy)
	s.mux.HandleFunc("POST /snapshot", s.handleLegacySnapshot)

	// Authenticated /api/* routes
	s.mux.HandleFunc("GET /api/me", s.withSession(s.handleMe))
	s.mux.HandleFunc("GET /api/clusters", s.withSession(s.handleClusters))
	s.mux.HandleFunc("GET /api/workloads", s.withSession(s.handleWorkloads))
	s.mux.HandleFunc("POST /api/deploy/preview", s.withSession(s.handleDeployPreview))
	s.mux.HandleFunc("POST /api/deploy", s.withSessionOrCI(s.handleAPIDeploy))
	s.mux.HandleFunc("POST /api/rollout", s.withSession(s.handleRollout))
	s.mux.HandleFunc("POST /api/workloads/rollout", s.withSession(s.handleRollout))
	s.mux.HandleFunc("POST /api/rollback", s.withSession(s.handleRollback))
	s.mux.HandleFunc("POST /api/pause", s.withSession(s.handlePause))
	s.mux.HandleFunc("POST /api/workloads/pause", s.withSession(s.handlePause))

	s.mux.HandleFunc("GET /api/sources", s.withSession(s.handleListSources))
	s.mux.HandleFunc("POST /api/sources", s.withSession(s.handleCreateSource))
	s.mux.HandleFunc("PUT /api/sources/{id}", s.withSession(s.handleUpdateSource))
	s.mux.HandleFunc("DELETE /api/sources/{id}", s.withSession(s.handleDeleteSource))
	s.mux.HandleFunc("POST /api/sources/{id}/build", s.withSession(s.handleBuildSource))
	s.mux.HandleFunc("GET /api/sources/{id}/webhook", s.withSession(s.handleGetSourceWebhook))
	s.mux.HandleFunc("POST /api/sources/{id}/webhook", s.withSession(s.handleSetupSourceWebhook))

	s.mux.HandleFunc("GET /api/events", s.withSession(s.handleEvents))

	s.mux.HandleFunc("GET /api/tokens", s.withSession(s.handleListTokens))
	s.mux.HandleFunc("POST /api/tokens", s.withSession(s.handleCreateToken))
	s.mux.HandleFunc("DELETE /api/tokens/{id}", s.withSession(s.handleDeleteToken))

	s.mux.HandleFunc("POST /api/snapshot", s.withSessionOrCI(s.handleAPISnapshot))

	// Catch-all for unknown /api/* routes: always 404 JSON (after checking auth), never SPA HTML or 403.
	s.mux.HandleFunc("/api/", s.withSession(func(w http.ResponseWriter, _ *http.Request, _ string) {
		writeJSONError(w, http.StatusNotFound, "not found")
	}))

	// SPA static files and history-mode fallback.
	s.mux.HandleFunc("/", s.handleSPA)
}

type authedHandler func(w http.ResponseWriter, r *http.Request, actor string)

func isMutating(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func hasJSONContentType(r *http.Request) bool {
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	return ct == "application/json" || strings.HasPrefix(ct, "application/json;")
}

func (s *Server) withSession(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.browser.currentUser(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if isMutating(r.Method) && !hasJSONContentType(r) {
			writeJSONError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
		next(w, r, u.Actor())
	}
}

func (s *Server) withSessionOrCI(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, hasBearer := extractBearer(r); hasBearer {
			actor, ok, msg := s.ci.authenticate(r.Context(), r)
			if !ok {
				writeJSONError(w, http.StatusUnauthorized, msg)
				return
			}
			next(w, r, actor)
			return
		}
		s.withSession(next)(w, r)
	}
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	evType := r.Header.Get("X-GitHub-Event")
	sig := r.Header.Get("X-Hub-Signature-256")
	code, text := s.eng.HandleGitHubWebhook(r.Context(), evType, sig, body)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	if text != "" {
		_, _ = w.Write([]byte(text))
	}
}

type deployPayload struct {
	Image  string `json:"image"`
	Tag    string `json:"tag"`
	Source string `json:"source"`
}

func (s *Server) handleLegacyDeploy(w http.ResponseWriter, r *http.Request) {
	actor, ok, msg := s.ci.authenticate(r.Context(), r)
	if !ok {
		writePlainText(w, http.StatusUnauthorized, msg)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writePlainText(w, http.StatusBadRequest, `expected {"image", "tag", "source"}`)
		return
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		writePlainText(w, http.StatusBadRequest, `expected {"image", "tag", "source"}`)
		return
	}
	rawImg, hasImg := raw["image"]
	rawTag, hasTag := raw["tag"]
	if !hasImg || !hasTag || string(rawImg) == "null" || string(rawTag) == "null" {
		writePlainText(w, http.StatusBadRequest, `expected {"image", "tag", "source"}`)
		return
	}
	var img, tag string
	if err := json.Unmarshal(rawImg, &img); err != nil {
		writePlainText(w, http.StatusBadRequest, `expected {"image", "tag", "source"}`)
		return
	}
	if err := json.Unmarshal(rawTag, &tag); err != nil {
		writePlainText(w, http.StatusBadRequest, `expected {"image", "tag", "source"}`)
		return
	}
	var src string
	if rawSrc, ok := raw["source"]; ok && string(rawSrc) != "null" {
		if err := json.Unmarshal(rawSrc, &src); err != nil {
			writePlainText(w, http.StatusBadRequest, `expected {"image", "tag", "source"}`)
			return
		}
	}
	src = ferry.TruncateSource(src)

	if img != strings.TrimSpace(img) || tag != strings.TrimSpace(tag) || ferry.ValidateImageAndTag(img, tag) != nil {
		writePlainText(w, http.StatusBadRequest, "bad image or tag")
		return
	}

	results, _, err := s.eng.Deploy(r.Context(), actor, img, tag, src, false)
	if err != nil {
		if errors.Is(err, ferry.ErrBadInput) {
			writePlainText(w, http.StatusBadRequest, "bad image or tag")
			return
		}
		writePlainText(w, mapEngineErr(err), err.Error())
		return
	}
	writeDeployResults(w, results)
}

func (s *Server) handleAPIDeploy(w http.ResponseWriter, r *http.Request, actor string) {
	var p deployPayload
	if err := readJSONBody(w, r, &p); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	results, _, err := s.eng.Deploy(r.Context(), actor, p.Image, p.Tag, p.Source, false)
	if err != nil {
		writeJSONError(w, mapEngineErr(err), err.Error())
		return
	}
	writeDeployResults(w, results)
}

func (s *Server) handleDeployPreview(w http.ResponseWriter, r *http.Request, actor string) {
	var p deployPayload
	if err := readJSONBody(w, r, &p); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	results, _, err := s.eng.Deploy(r.Context(), actor, p.Image, p.Tag, p.Source, true)
	if err != nil {
		writeJSONError(w, mapEngineErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, results)
}

func writeDeployResults(w http.ResponseWriter, results []state.Result) {
	code := http.StatusOK
	for _, r := range results {
		if r.Result == "error" {
			code = http.StatusInternalServerError
			break
		}
	}
	writeJSON(w, code, results)
}

func (s *Server) handleLegacySnapshot(w http.ResponseWriter, r *http.Request) {
	actor, ok, msg := s.ci.authenticate(r.Context(), r)
	if !ok {
		writePlainText(w, http.StatusUnauthorized, msg)
		return
	}
	if err := s.eng.QueueSnapshot(actor); err != nil {
		writePlainText(w, mapEngineErr(err), err.Error())
		return
	}
	writePlainText(w, http.StatusAccepted, "snapshot queued")
}

func (s *Server) handleAPISnapshot(w http.ResponseWriter, _ *http.Request, actor string) {
	if err := s.eng.QueueSnapshot(actor); err != nil {
		writeJSONError(w, mapEngineErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, ferry.SnapshotResult{Status: "queued"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, _ string) {
	u, _ := s.browser.currentUser(r)
	cfg := s.eng.Config()
	writeJSON(w, http.StatusOK, map[string]any{
		"sub":                     u.Sub,
		"name":                    u.Name,
		"email":                   u.Email,
		"groups":                  u.Groups,
		"authDisabled":            u.AuthDisabled,
		"publicURL":               cfg.Auth.OIDC.PublicURL,
		"annotationPrefix":        cfg.AnnotationPrefix,
		"namespaces":              cfg.Namespaces,
		"cnbConfigured":           cfg.CNB.Repo != "",
		"cnbRepo":                 cfg.CNB.Repo,
		"cnbImagePrefix":          cfg.CNB.ImagePrefix,
		"githubTokenConfigured":   len(cfg.GitHub.Tokens) > 0,
		"githubWebhookConfigured": cfg.GitHub.WebhookSecret != "",
		"snapshotConfigured":      cfg.Snapshot.Repo != "",
		"snapshotRepo":            cfg.Snapshot.Repo,
		"snapshotBranch":          cfg.Snapshot.Branch,
		"snapshotDir":             cfg.Snapshot.Dir,
		"snapshotHourUTC":         cfg.Snapshot.EffectiveHourUTC(),
	})
}

func (s *Server) handleClusters(w http.ResponseWriter, r *http.Request, _ string) {
	fresh := r.URL.Query().Get("fresh") == "1"
	inv, err := s.eng.Workloads(r.Context(), fresh)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, inv.Clusters)
}

func (s *Server) handleWorkloads(w http.ResponseWriter, r *http.Request, _ string) {
	fresh := r.URL.Query().Get("fresh") == "1"
	inv, err := s.eng.Workloads(r.Context(), fresh)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

func (s *Server) handleRollout(w http.ResponseWriter, r *http.Request, actor string) {
	var req ferry.RolloutRequest
	if err := readJSONBody(w, r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	ev, err := s.eng.Rollout(r.Context(), actor, req)
	if err != nil {
		writeJSONError(w, mapEngineErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		EventID string `json:"eventId"`
	}
	if err := readJSONBody(w, r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.EventID) == "" {
		writeJSONError(w, http.StatusBadRequest, "eventId is required")
		return
	}
	ev, err := s.eng.Rollback(r.Context(), actor, body.EventID)
	if err != nil {
		writeJSONError(w, mapEngineErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request, actor string) {
	var req ferry.PauseRequest
	if err := readJSONBody(w, r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	ev, err := s.eng.Pause(r.Context(), actor, req)
	if err != nil {
		writeJSONError(w, mapEngineErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

type sourceResponse struct {
	state.Source
	ImageRef string `json:"imageRef"`
}

func (s *Server) toSourceResponse(src state.Source) sourceResponse {
	return sourceResponse{
		Source:   src,
		ImageRef: src.ImageRef(s.eng.Config().CNB.ImagePrefix),
	}
}

func (s *Server) handleListSources(w http.ResponseWriter, _ *http.Request, _ string) {
	srcs := s.eng.Store().Sources()
	out := make([]sourceResponse, len(srcs))
	for i, src := range srcs {
		out[i] = s.toSourceResponse(src)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request, _ string) {
	var in state.Source
	if err := readJSONBody(w, r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := s.eng.Store().CreateSource(r.Context(), in)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.toSourceResponse(created))
}

func (s *Server) handleUpdateSource(w http.ResponseWriter, r *http.Request, _ string) {
	id := r.PathValue("id")
	var patch state.Source
	if err := readJSONBody(w, r, &patch); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.eng.Store().UpdateSource(r.Context(), id, patch)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "source not found")
			return
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.toSourceResponse(updated))
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request, _ string) {
	id := r.PathValue("id")
	if err := s.eng.Store().DeleteSource(r.Context(), id); err != nil {
		if errors.Is(err, state.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "source not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBuildSource(w http.ResponseWriter, r *http.Request, actor string) {
	id := r.PathValue("id")
	ev, err := s.eng.TriggerBuild(r.Context(), actor, id)
	if err != nil {
		writeJSONError(w, mapEngineErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, ev)
}

func (s *Server) handleGetSourceWebhook(w http.ResponseWriter, r *http.Request, _ string) {
	id := r.PathValue("id")
	st, err := s.eng.GetWebhookStatus(r.Context(), id)
	if err != nil {
		writeJSONError(w, mapEngineErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSetupSourceWebhook(w http.ResponseWriter, r *http.Request, _ string) {
	id := r.PathValue("id")
	pub := s.eng.Config().Auth.OIDC.PublicURL
	if pub == "" && r.Host != "" {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		pub = scheme + "://" + r.Host
	}
	st, err := s.eng.SetupWebhook(r.Context(), id, pub)
	if err != nil {
		writeJSONError(w, mapEngineErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, _ string) {
	q := r.URL.Query()
	limit := 0
	if l := q.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	evs := s.eng.Store().Events(state.EventsFilter{
		Kind:  q.Get("kind"),
		Image: q.Get("image"),
		Limit: limit,
	})
	writeJSON(w, http.StatusOK, evs)
}

func (s *Server) handleListTokens(w http.ResponseWriter, _ *http.Request, _ string) {
	writeJSON(w, http.StatusOK, s.eng.Store().Tokens())
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		Name string `json:"name"`
	}
	if err := readJSONBody(w, r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	info, plain, err := s.eng.Store().CreateToken(r.Context(), body.Name, actor)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token": plain,
		"info":  info,
	})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request, _ string) {
	id := r.PathValue("id")
	if err := s.eng.Store().RevokeToken(r.Context(), id); err != nil {
		if errors.Is(err, state.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "token not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clean := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if clean != "" {
		if f, err := s.spa.Open(clean); err == nil {
			fi, stErr := f.Stat()
			_ = f.Close()
			if stErr == nil && !fi.IsDir() {
				if strings.HasPrefix(clean, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else if clean == "index.html" {
					w.Header().Set("Cache-Control", "no-cache")
				}
				http.ServeFileFS(w, r, s.spa, clean)
				return
			}
		}
		// Missing asset file under /assets/ should 404, not fall back to index.html.
		if strings.HasPrefix(clean, "assets/") {
			http.NotFound(w, r)
			return
		}
	}

	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, s.spa, "index.html")
}

func readJSONBody(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(out); err != nil {
		return err
	}
	return nil
}

func mapEngineErr(err error) int {
	switch {
	case errors.Is(err, ferry.ErrBadInput):
		return http.StatusBadRequest
	case errors.Is(err, ferry.ErrArgoManaged):
		return http.StatusConflict
	case errors.Is(err, state.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ferry.ErrNotConfigured):
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writePlainText(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg + "\n"))
}
