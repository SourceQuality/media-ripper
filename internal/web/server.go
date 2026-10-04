// Package web serves the status UI and a small JSON API.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/pipeline"
	"github.com/sourcequality/media-ripper/internal/store"
)

//go:embed ui/*
var uiFS embed.FS

// Server exposes the manager over HTTP.
type Server struct {
	Manager *pipeline.Manager
	Store   *store.Store
	Version string
	Logger  *slog.Logger

	mu              sync.Mutex
	restartRequired []string
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
	mux.HandleFunc("POST /api/drives/{drive}/eject", s.eject)
	mux.HandleFunc("POST /api/drives/{drive}/rescan", s.rescan)
	mux.HandleFunc("POST /api/series/reset", s.resetSeries)
	mux.HandleFunc("POST /api/discs/forget", s.forgetDisc)
	mux.HandleFunc("POST /api/notify/test", s.testNotify)
	return logRequests(mux, s.Logger)
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
	return configResponse{Config: red, Secrets: secrets, Env: env, Path: cfg.Path, RestartRequired: restart}
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
