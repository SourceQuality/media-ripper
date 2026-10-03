package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/pipeline"
	"github.com/sourcequality/media-ripper/internal/store"
)

func TestHandlers(t *testing.T) {
	cfg := config.Default()
	cfg.Output.Path = t.TempDir()
	cfg.Metadata.TMDBAPIKey = "secret"
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := pipeline.New(pipeline.Deps{Config: &cfg, Store: st})
	srv := httptest.NewServer((&Server{Manager: m, Store: st, Config: &cfg, Version: "test"}).Handler())
	defer srv.Close()

	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 1<<16)
		n, _ := resp.Body.Read(buf)
		return resp.StatusCode, string(buf[:n])
	}
	if code, body := get("/"); code != 200 || !strings.Contains(body, "<title>Media Ripper</title>") {
		t.Fatalf("index: %d %s", code, body)
	}
	if code, body := get("/api/status"); code != 200 || !strings.Contains(body, `"version": "test"`) {
		t.Fatalf("status: %d %s", code, body)
	}
	if code, body := get("/api/config"); code != 200 || strings.Contains(body, "secret") {
		t.Fatalf("config must be redacted: %d %s", code, body)
	}
	if code, body := get("/api/history"); code != 200 || strings.TrimSpace(body) != "[]" {
		t.Fatalf("history: %d %q", code, body)
	}
	if code, _ := get("/api/jobs/nope"); code != 404 {
		t.Fatalf("job 404: %d", code)
	}
	resp, _ := http.Post(srv.URL+"/api/drives/sr9/eject", "application/json", nil)
	if resp.StatusCode != 400 {
		t.Fatalf("eject unknown drive: %d", resp.StatusCode)
	}
	if code, _ := get("/healthz"); code != 200 {
		t.Fatalf("healthz: %d", code)
	}
}
