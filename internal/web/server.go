// Package web serves the status UI and a small JSON API.
package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
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
	Config  *config.Config
	Version string
	Logger  *slog.Logger
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
	mux.HandleFunc("GET /api/config", s.config)
	mux.HandleFunc("GET /api/jobs/{id}", s.job)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /api/drives/{drive}/eject", s.eject)
	mux.HandleFunc("POST /api/drives/{drive}/rescan", s.rescan)
	mux.HandleFunc("POST /api/series/reset", s.resetSeries)
	mux.HandleFunc("POST /api/discs/forget", s.forgetDisc)
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

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Config.Redacted())
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	j, ok := s.Manager.Job(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, j)
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
