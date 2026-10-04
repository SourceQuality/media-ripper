package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/pipeline"
	"github.com/sourcequality/media-ripper/internal/store"
)

func TestHandlers(t *testing.T) {
	cfg := config.Default()
	cfg.Output.Path = t.TempDir()
	cfg.Metadata.TMDBAPIKey = "sk-hidden-123"
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := pipeline.New(pipeline.Deps{Config: &cfg, Store: st})
	srv := httptest.NewServer((&Server{Manager: m, Store: st, Version: "test"}).Handler())
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
	if code, body := get("/api/config"); code != 200 || strings.Contains(body, "sk-hidden-123") || !strings.Contains(body, `"metadata.tmdb_api_key": true`) {
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

func TestPutConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Output.Path = t.TempDir()
	cfg.Path = filepath.Join(t.TempDir(), "config.yaml")
	cfg.MakeMKV.Key = "KEEP-ME"
	st, _ := store.Open(t.TempDir())
	m := pipeline.New(pipeline.Deps{Config: &cfg, Store: st})
	srv := httptest.NewServer((&Server{Manager: m, Store: st, Version: "test"}).Handler())
	defer srv.Close()

	put := func(body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/config", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// Fetch, edit, send back.
	resp, _ := http.Get(srv.URL + "/api/config")
	var view struct {
		Config json.RawMessage `json:"config"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&view)
	resp.Body.Close()
	var edited map[string]any
	_ = json.Unmarshal(view.Config, &edited)
	edited["poll_interval"] = "7s"
	edited["drives"] = []string{"/dev/sr1"}
	edited["selection"].(map[string]any)["languages"] = []string{"eng"}
	edited["metadata"].(map[string]any)["tmdb_api_key"] = "NEW-TMDB"
	body, _ := json.Marshal(map[string]any{"config": edited, "clear_secrets": []string{"notify.ntfy_token"}})
	code, out := put(string(body))
	if code != 200 || !strings.Contains(out, `"drives"`) {
		t.Fatalf("put: %d %s", code, out)
	}
	got := m.Config()
	if got.PollInterval.D() != 7*time.Second || got.MakeMKV.Key != "KEEP-ME" || got.Metadata.TMDBAPIKey != "NEW-TMDB" || got.Selection.Languages[0] != "eng" {
		t.Fatalf("applied config: %+v", got)
	}
	if !strings.Contains(out, `"restart_required": [
  "drives"
 ]`) {
		t.Fatalf("restart list: %s", out)
	}
	raw, err := os.ReadFile(cfg.Path)
	if err != nil || !strings.Contains(string(raw), "poll_interval: 7s") || !strings.Contains(string(raw), "key: KEEP-ME") {
		t.Fatalf("saved file: %v\n%s", err, raw)
	}
	// Invalid input is rejected and nothing changes.
	edited["postprocess"].(map[string]any)["mode"] = "bogus"
	body, _ = json.Marshal(map[string]any{"config": edited})
	if code, out := put(string(body)); code != 400 || !strings.Contains(out, "postprocess.mode") {
		t.Fatalf("invalid: %d %s", code, out)
	}
	if m.Config().PostProcess.Mode != "remux" {
		t.Fatal("invalid config was applied")
	}
	// Unknown keys are rejected too.
	if code, _ := put(`{"config":{"output":{"path":"/x"},"nope":1}}`); code != 400 {
		t.Fatalf("unknown key accepted: %d", code)
	}
}

// The Discord token and webhook URL are secrets; the test button reports
// what Discord said.
func TestDiscordSettingsAndTest(t *testing.T) {
	var posts int
	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message": "Unknown Webhook", "code": 10015}`))
	}))
	defer discord.Close()
	cfg := config.Default()
	cfg.Output.Path = t.TempDir()
	cfg.Notify.Discord.BotToken = "bot-token-secret"
	cfg.Notify.Discord.WebhookURL = discord.URL + "/api/webhooks/1/hook-secret"
	st, _ := store.Open(t.TempDir())
	m := pipeline.New(pipeline.Deps{Config: &cfg, Store: st})
	srv := httptest.NewServer((&Server{Manager: m, Store: st, Version: "test"}).Handler())
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/api/config")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, leak := range []string{"bot-token-secret", "hook-secret"} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("config leaks %q", leak)
		}
	}
	if !strings.Contains(string(body), `"notify.discord.bot_token": true`) || !strings.Contains(string(body), `"application_id": "1556123331047202997"`) {
		t.Fatalf("config view: %s", body)
	}

	// No channel id, so the webhook is used; Discord refuses it.
	resp, _ = http.Post(srv.URL+"/api/notify/test", "application/json", nil)
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(out), "Unknown Webhook") || posts != 1 {
		t.Fatalf("test: %d %s (posts %d)", resp.StatusCode, out, posts)
	}
}
