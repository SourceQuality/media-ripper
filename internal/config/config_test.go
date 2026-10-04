package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := Default()
	cfg.Output.Path = "/mnt/x"
	cfg.Drives = []string{"/dev/sr0"}
	cfg.Selection.Languages = []string{"eng", "jpn"}
	cfg.Metadata.LabelOverrides = map[string]string{"BD_ROM": "Blade Runner 2049"}
	cfg.PostProcess.CustomCommand = []string{"ffmpeg", "-i", "{input}", "{output}"}
	cfg.MakeMKV.Key = "KEY"
	cfg.Output.DirMode = 0o750
	cfg.PollInterval = Duration(5 * time.Second)
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "poll_interval: 5s") || !strings.Contains(string(raw), "dir_mode: \"0750\"") {
		t.Fatalf("yaml:\n%s", raw)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != path || got.PollInterval.D() != 5*time.Second || got.Output.DirMode != 0o750 || got.MakeMKV.Key != "KEY" ||
		got.Metadata.LabelOverrides["BD_ROM"] != "Blade Runner 2049" || len(got.PostProcess.CustomCommand) != 4 || got.Selection.Languages[1] != "jpn" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestLoadExampleConfig(t *testing.T) {
	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Output.Path != "/mnt/media" || cfg.MakeMKV.Timeout.D() != 6*time.Hour || cfg.Output.FileMode != 0o664 {
		t.Fatalf("example config: %+v", cfg)
	}
}

func TestJSONDurationsAndModes(t *testing.T) {
	cfg := Default()
	cfg.Output.Path = "/x"
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"poll_interval":"3s"`) || !strings.Contains(string(data), `"dir_mode":"0775"`) {
		t.Fatalf("json: %s", data)
	}
	var back Config
	if err := json.Unmarshal([]byte(`{"output":{"path":"/y","dir_mode":"0o700"},"poll_interval":"1m","makemkv":{"timeout":"2h"}}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.Output.DirMode != 0o700 || back.PollInterval.D() != time.Minute || back.MakeMKV.Timeout.D() != 2*time.Hour {
		t.Fatalf("decoded: %+v", back)
	}
	if err := json.Unmarshal([]byte(`{"poll_interval":"soon"}`), &back); err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("expected duration error, got %v", err)
	}
}

func TestSecretsAndEnv(t *testing.T) {
	prev := Default()
	prev.Output.Path = "/x"
	prev.MakeMKV.Key = "K1"
	prev.Metadata.TMDBAPIKey = "T1"
	prev.Notify.NtfyToken = "N1"
	red, set := prev.Redacted()
	if red.MakeMKV.Key != "" || red.Metadata.TMDBAPIKey != "" || !set["makemkv.key"] || !set["metadata.tmdb_api_key"] {
		t.Fatalf("redacted: %+v %v", red, set)
	}
	next := Default()
	next.Output.Path = "/x"
	next.Metadata.TMDBAPIKey = "T2"
	next.MergeSecrets(&prev, []string{"notify.ntfy_token"})
	if next.MakeMKV.Key != "K1" || next.Metadata.TMDBAPIKey != "T2" || next.Notify.NtfyToken != "" {
		t.Fatalf("merge: %+v", next)
	}

	t.Setenv("MR_OUTPUT_PATH", "/from-env")
	t.Setenv("MR_DRIVES", "/dev/sr3, /dev/sr4")
	loaded, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Output.Path != "/from-env" || len(loaded.Drives) != 2 || len(loaded.EnvOverrides) != 2 {
		t.Fatalf("env: %+v", loaded)
	}
	edited := Default()
	edited.Output.Path = "/user-typed"
	edited.Drives = []string{"/dev/sr0"}
	edited.KeepEnvOverrides(loaded)
	if edited.Output.Path != "/from-env" || edited.Drives[0] != "/dev/sr3" {
		t.Fatalf("env not kept: %+v", edited)
	}
	if r := prev.NeedsRestart(&edited); len(r) == 0 {
		t.Fatal("drives change should need restart")
	}
	same := prev
	if r := prev.NeedsRestart(&same); len(r) != 0 {
		t.Fatalf("no change should not need restart: %v", r)
	}
}

func TestValidate(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "output.path") {
		t.Fatalf("missing output.path: %v", err)
	}
	cfg.Output.Path = "/x"
	cfg.PostProcess.Mode = "custom"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "custom_command") {
		t.Fatalf("custom without command: %v", err)
	}
	cfg.PostProcess.Mode = "transcode"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "postprocess.mode") {
		t.Fatalf("bad mode: %v", err)
	}
}
