// Package config loads and validates the media-ripper configuration.
//
// Every setting is decided before a disc is inserted; nothing in the pipeline
// prompts. The file is YAML, and a handful of secrets can be supplied through
// environment variables so they stay out of the file.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration document.
type Config struct {
	Drives       []string      `yaml:"drives"`
	PollInterval time.Duration `yaml:"poll_interval"`
	Workspace    string        `yaml:"workspace"`

	Output      Output      `yaml:"output"`
	MakeMKV     MakeMKV     `yaml:"makemkv"`
	Metadata    Metadata    `yaml:"metadata"`
	Selection   Selection   `yaml:"selection"`
	PostProcess PostProcess `yaml:"postprocess"`
	Eject       Eject       `yaml:"eject"`
	Web         Web         `yaml:"web"`
	Notify      Notify      `yaml:"notify"`
	Log         Log         `yaml:"log"`
}

// Output controls where finished files land and how they are named.
type Output struct {
	Path                 string `yaml:"path"`
	MoviesSubdir         string `yaml:"movies_subdir"`
	TVSubdir             string `yaml:"tv_subdir"`
	UnidentifiedDir      string `yaml:"unidentified_subdir"`
	MovieTemplate        string `yaml:"movie_template"`
	TVTemplate           string `yaml:"tv_template"`
	UnknownTemplate      string `yaml:"unidentified_template"`
	Overwrite            bool   `yaml:"overwrite"`
	DirMode              uint32 `yaml:"dir_mode"`
	FileMode             uint32 `yaml:"file_mode"`
	KeepWorkspaceOnError bool   `yaml:"keep_workspace_on_error"`
}

// MakeMKV configures the makemkvcon invocation.
type MakeMKV struct {
	Binary        string        `yaml:"binary"`
	Key           string        `yaml:"key"`
	MinLength     int           `yaml:"min_length"`
	Timeout       time.Duration `yaml:"timeout"`
	ScanTimeout   time.Duration `yaml:"scan_timeout"`
	Retries       int           `yaml:"retries"`
	ExtraArgs     []string      `yaml:"extra_args"`
	SettingsDir   string        `yaml:"settings_dir"`
	WriteSettings bool          `yaml:"write_settings"`
}

// Metadata configures disc identification.
type Metadata struct {
	Provider       string            `yaml:"provider"`
	TMDBAPIKey     string            `yaml:"tmdb_api_key"`
	Language       string            `yaml:"language"`
	Timeout        time.Duration     `yaml:"timeout"`
	LabelOverrides map[string]string `yaml:"label_overrides"`
}

// Selection tunes how titles are matched to the movie or episodes.
type Selection struct {
	Languages             []string      `yaml:"languages"`
	MovieRuntimeTolerance time.Duration `yaml:"movie_runtime_tolerance"`
	TVEpisodeTolerance    float64       `yaml:"tv_episode_tolerance"`
	MinMovieDuration      time.Duration `yaml:"min_movie_duration"`
	MinEpisodeDuration    time.Duration `yaml:"min_episode_duration"`
	UnidentifiedStrategy  string        `yaml:"unidentified_strategy"`
	AllowDoubleEpisodes   bool          `yaml:"allow_double_episodes"`
}

// PostProcess controls the optional pass after MakeMKV has written the MKV.
type PostProcess struct {
	Mode          string        `yaml:"mode"`
	Tool          string        `yaml:"tool"`
	SetTitle      bool          `yaml:"set_title"`
	CustomCommand []string      `yaml:"custom_command"`
	CustomExt     string        `yaml:"custom_extension"`
	Timeout       time.Duration `yaml:"timeout"`
}

// Eject controls tray behaviour.
type Eject struct {
	OnSuccess        bool `yaml:"on_success"`
	OnFailure        bool `yaml:"on_failure"`
	CloseTrayOnStart bool `yaml:"close_tray_on_start"`
	ReripSameDisc    bool `yaml:"rerip_same_disc"`
}

// Web configures the embedded status UI and API.
type Web struct {
	Listen  string `yaml:"listen"`
	Enabled bool   `yaml:"enabled"`
}

// Notify configures completion/failure notifications.
type Notify struct {
	WebhookURL string `yaml:"webhook_url"`
	NtfyURL    string `yaml:"ntfy_url"`
	NtfyToken  string `yaml:"ntfy_token"`
}

// Log configures logging.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Default returns the built-in defaults. Only output.path is mandatory.
func Default() Config {
	return Config{
		PollInterval: 3 * time.Second,
		Workspace:    "/var/lib/media-ripper",
		Output: Output{
			MoviesSubdir:    "Movies",
			TVSubdir:        "TV Shows",
			UnidentifiedDir: "_unidentified",
			MovieTemplate:   "{title} ({year})/{title} ({year}).mkv",
			TVTemplate:      "{series} ({year})/Season {season:02}/{series} - S{season:02}E{episode:02} - {episode_title}.mkv",
			UnknownTemplate: "{label} {date}/{label} - t{title_id:02}.mkv",
			DirMode:         0o775,
			FileMode:        0o664,
		},
		MakeMKV: MakeMKV{
			Binary:        "makemkvcon",
			MinLength:     120,
			Timeout:       6 * time.Hour,
			ScanTimeout:   20 * time.Minute,
			Retries:       1,
			WriteSettings: true,
		},
		Metadata: Metadata{
			Provider: "tmdb",
			Language: "en-US",
			Timeout:  20 * time.Second,
		},
		Selection: Selection{
			MovieRuntimeTolerance: 8 * time.Minute,
			TVEpisodeTolerance:    0.35,
			MinMovieDuration:      40 * time.Minute,
			MinEpisodeDuration:    8 * time.Minute,
			UnidentifiedStrategy:  "longest",
			AllowDoubleEpisodes:   true,
		},
		PostProcess: PostProcess{
			Mode:      "remux",
			Tool:      "auto",
			SetTitle:  true,
			CustomExt: "mkv",
			Timeout:   6 * time.Hour,
		},
		Eject: Eject{
			OnSuccess: true,
			OnFailure: true,
		},
		Web: Web{
			Listen:  ":8080",
			Enabled: true,
		},
		Log: Log{Level: "info", Format: "text"},
	}
}

// Load reads the YAML file at path (if non-empty), applies environment
// overrides, fills defaults and validates the result.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func applyEnv(cfg *Config) {
	set := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			*dst = v
		}
	}
	set("MR_OUTPUT_PATH", &cfg.Output.Path)
	set("MR_WORKSPACE", &cfg.Workspace)
	set("MR_TMDB_API_KEY", &cfg.Metadata.TMDBAPIKey)
	set("MR_MAKEMKV_KEY", &cfg.MakeMKV.Key)
	set("MR_MAKEMKV_BINARY", &cfg.MakeMKV.Binary)
	set("MR_WEB_LISTEN", &cfg.Web.Listen)
	set("MR_LOG_LEVEL", &cfg.Log.Level)
	set("MR_NTFY_URL", &cfg.Notify.NtfyURL)
	set("MR_NTFY_TOKEN", &cfg.Notify.NtfyToken)
	set("MR_WEBHOOK_URL", &cfg.Notify.WebhookURL)
	if v := os.Getenv("MR_DRIVES"); v != "" {
		cfg.Drives = nil
		for _, d := range strings.Split(v, ",") {
			if d = strings.TrimSpace(d); d != "" {
				cfg.Drives = append(cfg.Drives, d)
			}
		}
	}
}

// Validate checks the configuration for problems that would stop a rip from
// completing unattended.
func (c *Config) Validate() error {
	var errs []error
	if c.Output.Path == "" {
		errs = append(errs, errors.New("output.path is required"))
	}
	if c.Workspace == "" {
		errs = append(errs, errors.New("workspace is required"))
	}
	if c.PollInterval < 500*time.Millisecond {
		errs = append(errs, errors.New("poll_interval must be at least 500ms"))
	}
	switch c.PostProcess.Mode {
	case "none", "remux", "custom":
	default:
		errs = append(errs, fmt.Errorf("postprocess.mode %q must be none, remux or custom", c.PostProcess.Mode))
	}
	switch c.PostProcess.Tool {
	case "auto", "mkvmerge", "ffmpeg":
	default:
		errs = append(errs, fmt.Errorf("postprocess.tool %q must be auto, mkvmerge or ffmpeg", c.PostProcess.Tool))
	}
	if c.PostProcess.Mode == "custom" && len(c.PostProcess.CustomCommand) == 0 {
		errs = append(errs, errors.New("postprocess.custom_command is required when mode is custom"))
	}
	switch c.Metadata.Provider {
	case "tmdb", "none":
	default:
		errs = append(errs, fmt.Errorf("metadata.provider %q must be tmdb or none", c.Metadata.Provider))
	}
	switch c.Selection.UnidentifiedStrategy {
	case "longest", "all", "skip":
	default:
		errs = append(errs, fmt.Errorf("selection.unidentified_strategy %q must be longest, all or skip", c.Selection.UnidentifiedStrategy))
	}
	if c.Selection.TVEpisodeTolerance <= 0 || c.Selection.TVEpisodeTolerance >= 1 {
		errs = append(errs, errors.New("selection.tv_episode_tolerance must be between 0 and 1"))
	}
	for _, t := range []string{c.Output.MovieTemplate, c.Output.TVTemplate, c.Output.UnknownTemplate} {
		if strings.TrimSpace(t) == "" || filepath.IsAbs(t) {
			errs = append(errs, fmt.Errorf("output template %q must be a non-empty relative path", t))
		}
	}
	if len(c.Selection.Languages) > 0 {
		for i, l := range c.Selection.Languages {
			c.Selection.Languages[i] = strings.ToLower(strings.TrimSpace(l))
		}
	}
	return errors.Join(errs...)
}

// RipDir is where MakeMKV writes files before delivery.
func (c *Config) RipDir() string { return filepath.Join(c.Workspace, "rips") }

// StateDir holds history and series progress.
func (c *Config) StateDir() string { return filepath.Join(c.Workspace, "state") }

// Redacted returns a copy with secrets blanked, for display in the UI.
func (c Config) Redacted() Config {
	if c.MakeMKV.Key != "" {
		c.MakeMKV.Key = "••••"
	}
	if c.Metadata.TMDBAPIKey != "" {
		c.Metadata.TMDBAPIKey = "••••"
	}
	if c.Notify.NtfyToken != "" {
		c.Notify.NtfyToken = "••••"
	}
	return c
}
