// Package web serves the status UI and a small JSON API.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/pipeline"
	"github.com/sourcequality/media-ripper/internal/store"
	"github.com/sourcequality/media-ripper/internal/updates"
)

//go:embed ui/*
var uiFS embed.FS

// Server exposes the manager over HTTP.
type Server struct {
	Manager *pipeline.Manager
	Store   *store.Store
	Version string
	Logger  *slog.Logger
	Updates *updates.Watcher
	// TLS describes the HTTPS certificate in use (nil when off).
	TLS *TLSInfo

	mu              sync.Mutex
	restartRequired []string
	auth            authState
}

// Handler builds the mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	ui, _ := fs.Sub(uiFS, "ui")
	mux.Handle("GET /", http.FileServer(http.FS(ui)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/history", s.history)
	mux.HandleFunc("GET /api/series", s.series)
	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("PUT /api/config", s.putConfig)
	mux.HandleFunc("GET /api/jobs/{id}", s.job)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /api/jobs/{id}/retry-import", s.retryImport)
	mux.HandleFunc("POST /api/drives/{drive}/eject", s.eject)
	mux.HandleFunc("POST /api/drives/{drive}/rescan", s.rescan)
	mux.HandleFunc("POST /api/series/reset", s.resetSeries)
	mux.HandleFunc("POST /api/discs/forget", s.forgetDisc)
	mux.HandleFunc("POST /api/notify/test", s.testNotify)
	mux.HandleFunc("GET /api/reviews", s.reviews)
	mux.HandleFunc("POST /api/reviews/{id}/approve", s.approveReview)
	mux.HandleFunc("POST /api/reviews/{id}/discard", s.discardReview)
	mux.HandleFunc("GET /api/lookup", s.lookup)
	mux.HandleFunc("GET /api/boxsets", s.boxSets)
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /api/jobs/{id}/manifest", s.discManifest)
	mux.HandleFunc("GET /api/jobs/{id}/files", s.discFiles)
	mux.HandleFunc("GET /api/jobs/{id}/contribution", s.contribution)
	mux.HandleFunc("GET /api/auth/status", s.authStatus)
	mux.HandleFunc("POST /api/auth/setup", s.authSetup)
	mux.HandleFunc("POST /api/auth/login", s.authLogin)
	mux.HandleFunc("POST /api/auth/logout", s.authLogout)
	mux.HandleFunc("POST /api/auth/password", s.authPassword)
	mux.HandleFunc("POST /api/auth/token", s.authToken)
	mux.HandleFunc("DELETE /api/auth/token", s.authToken)
	return logRequests(s.guard(mux), s.Logger)
}

func logRequests(h http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if log != nil && r.Method != http.MethodGet {
			log.Info("http", "method", r.Method, "path", r.URL.Path)
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	snap := s.Manager.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"version": s.Version,
		"started": snap.Started,
		"now":     time.Now(),
		"drives":  snap.Drives,
		"recent":  snap.Recent,
		"storage": snap.Storage,
		"update":  s.Updates.Available(),
	})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	h, err := s.Store.History(limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if h == nil {
		h = []json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) series(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Store.Series())
}

// configResponse is what the settings page works with. Secrets never leave
// the server; "secrets" says which are set so the form can show that.
type configResponse struct {
	Config          config.Config   `json:"config"`
	Secrets         map[string]bool `json:"secrets"`
	Env             []string        `json:"env"`
	Path            string          `json:"path"`
	RestartRequired []string        `json:"restart_required"`
	TLS             *TLSInfo        `json:"tls,omitempty"`
}

func (s *Server) configView() configResponse {
	cfg := s.Manager.Config()
	red, secrets := cfg.Redacted()
	s.mu.Lock()
	restart := append([]string(nil), s.restartRequired...)
	s.mu.Unlock()
	env := cfg.EnvOverrides
	if env == nil {
		env = []string{}
	}
	if restart == nil {
		restart = []string{}
	}
	return configResponse{Config: red, Secrets: secrets, Env: env, Path: cfg.Path, RestartRequired: restart, TLS: s.TLS}
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.configView())
}

// putConfig validates, saves and applies a full configuration. Keys set by
// the environment keep their current values because the environment would
// win again on the next start.
func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Config       json.RawMessage `json:"config"`
		ClearSecrets []string        `json:"clear_secrets"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	prev := s.Manager.Config()
	next := config.Default()
	dec := json.NewDecoder(bytes.NewReader(body.Config))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	next.MergeSecrets(prev, body.ClearSecrets)
	next.KeepEnvOverrides(prev)
	if err := next.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	path := prev.Path
	if path == "" {
		path = config.DefaultPath()
	}
	if err := next.Save(path); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	restart := s.Manager.SetConfig(&next)
	s.mu.Lock()
	s.restartRequired = mergeKeys(s.restartRequired, restart)
	s.mu.Unlock()
	if s.Logger != nil {
		s.Logger.Info("config saved", "file", path, "restart_required", restart)
	}
	writeJSON(w, http.StatusOK, s.configView())
}

func mergeKeys(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range append(a, b...) {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if j, ok := s.Manager.Job(id); ok {
		writeJSON(w, http.StatusOK, j)
		return
	}
	// Jobs from before a restart are only in the saved history, which
	// keeps the outcome but not the step-by-step log.
	h, err := s.Store.History(0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for _, raw := range h {
		var rec struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &rec) == nil && rec.ID == id {
			writeJSON(w, http.StatusOK, raw)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if err := s.Manager.Cancel(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func drivePath(r *http.Request) string {
	d := r.PathValue("drive")
	if !strings.HasPrefix(d, "/dev/") {
		d = "/dev/" + d
	}
	return d
}

func (s *Server) eject(w http.ResponseWriter, r *http.Request) {
	if err := s.Manager.Eject(drivePath(r)); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) rescan(w http.ResponseWriter, r *http.Request) {
	if err := s.Manager.Rescan(drivePath(r)); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) resetSeries(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Series string `json:"series"`
		Season int    `json:"season"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Series == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "series required"})
		return
	}
	if err := s.Store.ResetSeries(body.Series, body.Season); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) forgetDisc(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Fingerprint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "fingerprint required"})
		return
	}
	if err := s.Store.ForgetDisc(body.Fingerprint); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// testNotify sends a test message with the saved notification settings.
func (s *Server) testNotify(w http.ResponseWriter, r *http.Request) {
	if err := s.Manager.TestNotify(r.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) reviews(w http.ResponseWriter, r *http.Request) {
	list := s.Manager.Reviews()
	if list == nil {
		list = []pipeline.Job{}
	}
	writeJSON(w, http.StatusOK, list)
}

// approveReview imports a held disc, with the person's correction when the
// body carries one. Importing can take minutes (the app moves the files).
func (s *Server) approveReview(w http.ResponseWriter, r *http.Request) {
	var edit *pipeline.ReviewEdit
	if r.ContentLength != 0 {
		var e pipeline.ReviewEdit
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&e); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, err)
			return
		} else if err == nil {
			edit = &e
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Minute)
	defer cancel()
	j, err := s.Manager.ApproveReview(ctx, r.PathValue("id"), edit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// retryImport runs a finished disc's Radarr/Sonarr import again. It keeps
// going if the browser leaves; a restart picks it up again.
func (s *Server) retryImport(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Minute)
	defer cancel()
	j, err := s.Manager.RetryImport(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) discardReview(w http.ResponseWriter, r *http.Request) {
	if err := s.Manager.DiscardReview(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "q required"})
		return
	}
	res, err := s.Manager.Lookup(r.Context(), metadata.Kind(r.URL.Query().Get("kind")), q)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) boxSets(w http.ResponseWriter, r *http.Request) {
	sets := s.Manager.BoxSets(r.Context())
	if sets == nil {
		sets = []pipeline.BoxSetView{}
	}
	writeJSON(w, http.StatusOK, sets)
}

// discManifest downloads a finished disc's Optical Disc Manifest, to upload
// on thediscdb.com/contribute.
// discFiles lists the files on a disc, as read for its content hash.
func (s *Server) discFiles(w http.ResponseWriter, r *http.Request) {
	inv, err := s.Manager.DiscFiles(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

func (s *Server) discManifest(w http.ResponseWriter, r *http.Request) {
	m, err := s.Manager.DiscManifest(r.PathValue("id"), s.Version)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	name := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '"' || r < 32 {
			return '_'
		}
		return r
	}, m.Disc.Name)
	if name == "" {
		name = "disc"
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.odm.json"`)
	writeJSON(w, http.StatusOK, m)
}

// contribution is the title-by-title mapping to enter with the manifest.
func (s *Server) contribution(w http.ResponseWriter, r *http.Request) {
	text, err := s.Manager.ContributionText(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(text))
}

// TLSInfo is shown in Settings so the certificate a browser asks about can
// be checked against the one the ripper uses.
type TLSInfo struct {
	Mode        string    `json:"mode"`
	Fingerprint string    `json:"fingerprint"`
	Expires     time.Time `json:"expires"`
}
