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
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that reads and writes as "8m" or "6h" in YAML
// and JSON.
type Duration time.Duration

// D returns the plain time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}

// Mode is a file permission that reads and writes as octal text ("0775").
type Mode uint32

// MarshalText implements encoding.TextMarshaler.
func (m Mode) MarshalText() ([]byte, error) { return []byte(fmt.Sprintf("%04o", uint32(m))), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (m *Mode) UnmarshalText(b []byte) error {
	s := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(string(b)), "0o"), "0O")
	if s == "" {
		return fmt.Errorf("mode is required")
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil || v > 0o7777 {
		return fmt.Errorf("invalid mode %q (use octal such as 0775)", string(b))
	}
	*m = Mode(v)
	return nil
}

// Config is the top-level configuration document.
type Config struct {
	// Path is the file the config was loaded from (empty when none).
	Path string `yaml:"-" json:"-"`
	// EnvOverrides lists dotted keys that came from the environment and
	// therefore win over the file on every start.
	EnvOverrides []string `yaml:"-" json:"-"`

	Drives       []string `yaml:"drives" json:"drives"`
	PollInterval Duration `yaml:"poll_interval" json:"poll_interval"`
	Workspace    string   `yaml:"workspace" json:"workspace"`

	Output      Output      `yaml:"output" json:"output"`
	MakeMKV     MakeMKV     `yaml:"makemkv" json:"makemkv"`
	Metadata    Metadata    `yaml:"metadata" json:"metadata"`
	Selection   Selection   `yaml:"selection" json:"selection"`
	PostProcess PostProcess `yaml:"postprocess" json:"postprocess"`
	Arr         Arr         `yaml:"arr" json:"arr"`
	Eject       Eject       `yaml:"eject" json:"eject"`
	Web         Web         `yaml:"web" json:"web"`
	Notify      Notify      `yaml:"notify" json:"notify"`
	Updates     Updates     `yaml:"updates" json:"updates"`
	Log         Log         `yaml:"log" json:"log"`
}

// Updates shows a notice in the web UI when a newer release is published.
type Updates struct {
	Check bool   `yaml:"check" json:"check"`
	Repo  string `yaml:"repo" json:"repo"`
	// Token is a GitHub token with read access, needed while the
	// repository is private.
	Token string `yaml:"token" json:"token"`
}

// Output controls where finished files land and how they are named.
type Output struct {
	Path                 string `yaml:"path" json:"path"`
	MoviesSubdir         string `yaml:"movies_subdir" json:"movies_subdir"`
	TVSubdir             string `yaml:"tv_subdir" json:"tv_subdir"`
	UnidentifiedDir      string `yaml:"unidentified_subdir" json:"unidentified_subdir"`
	MovieTemplate        string `yaml:"movie_template" json:"movie_template"`
	TVTemplate           string `yaml:"tv_template" json:"tv_template"`
	UnknownTemplate      string `yaml:"unidentified_template" json:"unidentified_template"`
	Overwrite            bool   `yaml:"overwrite" json:"overwrite"`
	DirMode              Mode   `yaml:"dir_mode" json:"dir_mode"`
	FileMode             Mode   `yaml:"file_mode" json:"file_mode"`
	KeepWorkspaceOnError bool   `yaml:"keep_workspace_on_error" json:"keep_workspace_on_error"`
	// Resume keeps a failed or cancelled disc's ripped titles so the next
	// attempt on the same disc skips what is already ripped or delivered.
	Resume       bool     `yaml:"resume" json:"resume"`
	ResumeMaxAge Duration `yaml:"resume_max_age" json:"resume_max_age"`
}

// MakeMKV configures the makemkvcon invocation.
type MakeMKV struct {
	Binary        string   `yaml:"binary" json:"binary"`
	Key           string   `yaml:"key" json:"key"`
	MinLength     int      `yaml:"min_length" json:"min_length"`
	Timeout       Duration `yaml:"timeout" json:"timeout"`
	ScanTimeout   Duration `yaml:"scan_timeout" json:"scan_timeout"`
	Retries       int      `yaml:"retries" json:"retries"`
	ExtraArgs     []string `yaml:"extra_args" json:"extra_args"`
	SettingsDir   string   `yaml:"settings_dir" json:"settings_dir"`
	WriteSettings bool     `yaml:"write_settings" json:"write_settings"`
}

// Metadata configures disc identification.
type Metadata struct {
	Provider       string            `yaml:"provider" json:"provider"`
	TMDBAPIKey     string            `yaml:"tmdb_api_key" json:"tmdb_api_key"`
	Language       string            `yaml:"language" json:"language"`
	Timeout        Duration          `yaml:"timeout" json:"timeout"`
	LabelOverrides map[string]string `yaml:"label_overrides" json:"label_overrides"`
	OCR            OCR               `yaml:"ocr" json:"ocr"`
	TheDiscDB      TheDiscDB         `yaml:"thediscdb" json:"thediscdb"`
}

// TheDiscDB configures disc lookups in the TheDiscDB catalogue, which maps
// each title of a catalogued disc to its episode, main movie or extra.
type TheDiscDB struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Repo is the GitHub repository holding the catalogue data.
	Repo string `yaml:"repo" json:"repo"`
	// API is the GraphQL endpoint used to identify a disc by its content
	// hash when its label says nothing.
	API string `yaml:"api" json:"api"`
}

// OCR identifies discs with useless labels by reading the title card and
// credits of the ripped file.
type OCR struct {
	Enabled       bool     `yaml:"enabled" json:"enabled"`
	Tesseract     string   `yaml:"tesseract" json:"tesseract"`
	Languages     string   `yaml:"languages" json:"languages"`
	HeadMinutes   int      `yaml:"head_minutes" json:"head_minutes"`
	TailMinutes   int      `yaml:"tail_minutes" json:"tail_minutes"`
	FramesPerMin  int      `yaml:"frames_per_minute" json:"frames_per_minute"`
	MaxCandidates int      `yaml:"max_candidates" json:"max_candidates"`
	Timeout       Duration `yaml:"timeout" json:"timeout"`
}

// Arr configures the Radarr and Sonarr handoff.
type Arr struct {
	Radarr        ArrApp   `yaml:"radarr" json:"radarr"`
	Sonarr        ArrApp   `yaml:"sonarr" json:"sonarr"`
	StagingSubdir string   `yaml:"staging_subdir" json:"staging_subdir"`
	ImportTimeout Duration `yaml:"import_timeout" json:"import_timeout"`
	// ImportPolicy decides which discs are imported without a person
	// confirming the titles: always | confident | verified.
	ImportPolicy string `yaml:"import_policy" json:"import_policy"`
}

// ArrApp is one Radarr or Sonarr instance.
type ArrApp struct {
	Enabled        bool              `yaml:"enabled" json:"enabled"`
	URL            string            `yaml:"url" json:"url"`
	APIKey         string            `yaml:"api_key" json:"api_key"`
	RootFolder     string            `yaml:"root_folder" json:"root_folder"`
	QualityProfile string            `yaml:"quality_profile" json:"quality_profile"`
	AddMissing     bool              `yaml:"add_missing" json:"add_missing"`
	Monitored      bool              `yaml:"monitored" json:"monitored"`
	ImportMode     string            `yaml:"import_mode" json:"import_mode"`
	PathMap        map[string]string `yaml:"path_map" json:"path_map"`
}

// Configured reports whether the app can be talked to.
func (a ArrApp) Configured() bool { return a.Enabled && a.URL != "" && a.APIKey != "" }

// Selection tunes how titles are matched to the movie or episodes.
type Selection struct {
	Languages             []string `yaml:"languages" json:"languages"`
	MovieRuntimeTolerance Duration `yaml:"movie_runtime_tolerance" json:"movie_runtime_tolerance"`
	TVEpisodeTolerance    float64  `yaml:"tv_episode_tolerance" json:"tv_episode_tolerance"`
	MinMovieDuration      Duration `yaml:"min_movie_duration" json:"min_movie_duration"`
	MinEpisodeDuration    Duration `yaml:"min_episode_duration" json:"min_episode_duration"`
	UnidentifiedStrategy  string   `yaml:"unidentified_strategy" json:"unidentified_strategy"`
	AllowDoubleEpisodes   bool     `yaml:"allow_double_episodes" json:"allow_double_episodes"`
}

// PostProcess controls the optional pass after MakeMKV has written the MKV.
type PostProcess struct {
	Mode          string   `yaml:"mode" json:"mode"`
	Tool          string   `yaml:"tool" json:"tool"`
	SetTitle      bool     `yaml:"set_title" json:"set_title"`
	CustomCommand []string `yaml:"custom_command" json:"custom_command"`
	CustomExt     string   `yaml:"custom_extension" json:"custom_extension"`
	Timeout       Duration `yaml:"timeout" json:"timeout"`
}

// Eject controls tray behaviour.
type Eject struct {
	OnSuccess        bool `yaml:"on_success" json:"on_success"`
	AfterRip         bool `yaml:"after_rip" json:"after_rip"`
	OnFailure        bool `yaml:"on_failure" json:"on_failure"`
	CloseTrayOnStart bool `yaml:"close_tray_on_start" json:"close_tray_on_start"`
	ReripSameDisc    bool `yaml:"rerip_same_disc" json:"rerip_same_disc"`
}

// Web configures the embedded status UI and API.
type Web struct {
	Listen  string `yaml:"listen" json:"listen"`
	Enabled bool   `yaml:"enabled" json:"enabled"`
}

// Notify configures completion/failure notifications.
type Notify struct {
	WebhookURL string  `yaml:"webhook_url" json:"webhook_url"`
	NtfyURL    string  `yaml:"ntfy_url" json:"ntfy_url"`
	NtfyToken  string  `yaml:"ntfy_token" json:"ntfy_token"`
	Discord    Discord `yaml:"discord" json:"discord"`
}

// Discord posts to a channel as a bot (token + channel id) or through a
// channel webhook. ApplicationID builds the invite link for the bot.
type Discord struct {
	ApplicationID string `yaml:"application_id" json:"application_id"`
	BotToken      string `yaml:"bot_token" json:"bot_token"`
	ChannelID     string `yaml:"channel_id" json:"channel_id"`
	WebhookURL    string `yaml:"webhook_url" json:"webhook_url"`
}

// DefaultDiscordApp is the media-ripper Discord application.
const DefaultDiscordApp = "1556123331047202997"

// Log configures logging.
type Log struct {
	Level  string `yaml:"level" json:"level"`
	Format string `yaml:"format" json:"format"`
}

// Default returns the built-in defaults. Only output.path is mandatory.
func Default() Config {
	return Config{
		PollInterval: Duration(3 * time.Second),
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
			Resume:          true,
			ResumeMaxAge:    Duration(72 * time.Hour),
		},
		MakeMKV: MakeMKV{
			Binary:        "makemkvcon",
			MinLength:     120,
			Timeout:       Duration(6 * time.Hour),
			ScanTimeout:   Duration(20 * time.Minute),
			Retries:       1,
			WriteSettings: true,
		},
		Notify:  Notify{Discord: Discord{ApplicationID: DefaultDiscordApp}},
		Updates: Updates{Check: true, Repo: "SourceQuality/media-ripper"},
		Metadata: Metadata{
			TheDiscDB: TheDiscDB{Enabled: true, Repo: "TheDiscDb/data", API: "https://thediscdb.com/graphql"},
			Provider:  "auto",
			Language:  "en-US",
			Timeout:   Duration(20 * time.Second),
			OCR: OCR{
				Tesseract:     "tesseract",
				Languages:     "eng",
				HeadMinutes:   8,
				TailMinutes:   4,
				FramesPerMin:  20,
				MaxCandidates: 8,
				Timeout:       Duration(20 * time.Minute),
			},
		},
		Arr: Arr{
			Radarr:        ArrApp{AddMissing: true, ImportMode: "move"},
			Sonarr:        ArrApp{AddMissing: true, ImportMode: "move"},
			StagingSubdir: "_incoming",
			ImportPolicy:  "confident",
			ImportTimeout: Duration(10 * time.Minute),
		},
		Selection: Selection{
			MovieRuntimeTolerance: Duration(8 * time.Minute),
			TVEpisodeTolerance:    0.35,
			MinMovieDuration:      Duration(40 * time.Minute),
			MinEpisodeDuration:    Duration(8 * time.Minute),
			UnidentifiedStrategy:  "longest",
			AllowDoubleEpisodes:   true,
		},
		PostProcess: PostProcess{
			Mode:      "remux",
			Tool:      "auto",
			SetTitle:  true,
			CustomExt: "mkv",
			Timeout:   Duration(6 * time.Hour),
		},
		Eject: Eject{
			OnSuccess: true,
			AfterRip:  true,
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
	cfg.Path = path
	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Save writes the configuration as YAML to path (or c.Path), atomically.
// Comments in a hand-written file are not preserved.
func (c *Config) Save(path string) error {
	if path == "" {
		path = c.Path
	}
	if path == "" {
		return errors.New("no config file path")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := "# media-ripper configuration. Written by the settings page; see\n# config.example.yaml for a documented version of every key.\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(header), data...), 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	c.Path = path
	return nil
}

// DefaultPath is where Save writes when no file was loaded.
func DefaultPath() string {
	if p := os.Getenv("MR_CONFIG"); p != "" {
		return p
	}
	return "/etc/media-ripper/config.yaml"
}

// KeepEnvOverrides copies values that the environment set in prev back into
// c and carries the override list forward.
func (c *Config) KeepEnvOverrides(prev *Config) {
	c.EnvOverrides = prev.EnvOverrides
	for _, k := range prev.EnvOverrides {
		switch k {
		case "drives":
			c.Drives = prev.Drives
		case "output.path":
			c.Output.Path = prev.Output.Path
		case "workspace":
			c.Workspace = prev.Workspace
		case "metadata.tmdb_api_key":
			c.Metadata.TMDBAPIKey = prev.Metadata.TMDBAPIKey
		case "makemkv.key":
			c.MakeMKV.Key = prev.MakeMKV.Key
		case "makemkv.binary":
			c.MakeMKV.Binary = prev.MakeMKV.Binary
		case "web.listen":
			c.Web.Listen = prev.Web.Listen
		case "log.level":
			c.Log.Level = prev.Log.Level
		case "notify.ntfy_url":
			c.Notify.NtfyURL = prev.Notify.NtfyURL
		case "notify.ntfy_token":
			c.Notify.NtfyToken = prev.Notify.NtfyToken
		case "notify.webhook_url":
			c.Notify.WebhookURL = prev.Notify.WebhookURL
		case "notify.discord.bot_token":
			c.Notify.Discord.BotToken = prev.Notify.Discord.BotToken
		case "notify.discord.channel_id":
			c.Notify.Discord.ChannelID = prev.Notify.Discord.ChannelID
		case "notify.discord.webhook_url":
			c.Notify.Discord.WebhookURL = prev.Notify.Discord.WebhookURL
		case "updates.token":
			c.Updates.Token = prev.Updates.Token
		case "arr.radarr.api_key":
			c.Arr.Radarr.APIKey = prev.Arr.Radarr.APIKey
		case "arr.sonarr.api_key":
			c.Arr.Sonarr.APIKey = prev.Arr.Sonarr.APIKey
		}
	}
}

// NeedsRestart lists the dotted keys whose change cannot be applied to a
// running daemon.
func (c *Config) NeedsRestart(next *Config) []string {
	var out []string
	if strings.Join(c.Drives, ",") != strings.Join(next.Drives, ",") {
		out = append(out, "drives")
	}
	if c.Workspace != next.Workspace {
		out = append(out, "workspace")
	}
	if c.Web.Listen != next.Web.Listen {
		out = append(out, "web.listen")
	}
	if c.Web.Enabled != next.Web.Enabled {
		out = append(out, "web.enabled")
	}
	if c.Log != next.Log {
		out = append(out, "log")
	}
	return out
}

func applyEnv(cfg *Config) {
	cfg.EnvOverrides = nil
	set := func(key, dotted string, dst *string) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			*dst = v
			cfg.EnvOverrides = append(cfg.EnvOverrides, dotted)
		}
	}
	set("MR_OUTPUT_PATH", "output.path", &cfg.Output.Path)
	set("MR_WORKSPACE", "workspace", &cfg.Workspace)
	set("MR_TMDB_API_KEY", "metadata.tmdb_api_key", &cfg.Metadata.TMDBAPIKey)
	set("MR_MAKEMKV_KEY", "makemkv.key", &cfg.MakeMKV.Key)
	set("MR_MAKEMKV_BINARY", "makemkv.binary", &cfg.MakeMKV.Binary)
	set("MR_WEB_LISTEN", "web.listen", &cfg.Web.Listen)
	set("MR_LOG_LEVEL", "log.level", &cfg.Log.Level)
	set("MR_NTFY_URL", "notify.ntfy_url", &cfg.Notify.NtfyURL)
	set("MR_NTFY_TOKEN", "notify.ntfy_token", &cfg.Notify.NtfyToken)
	set("MR_WEBHOOK_URL", "notify.webhook_url", &cfg.Notify.WebhookURL)
	set("MR_DISCORD_BOT_TOKEN", "notify.discord.bot_token", &cfg.Notify.Discord.BotToken)
	set("MR_DISCORD_CHANNEL_ID", "notify.discord.channel_id", &cfg.Notify.Discord.ChannelID)
	set("MR_DISCORD_WEBHOOK_URL", "notify.discord.webhook_url", &cfg.Notify.Discord.WebhookURL)
	set("MR_GITHUB_TOKEN", "updates.token", &cfg.Updates.Token)
	set("MR_RADARR_API_KEY", "arr.radarr.api_key", &cfg.Arr.Radarr.APIKey)
	set("MR_SONARR_API_KEY", "arr.sonarr.api_key", &cfg.Arr.Sonarr.APIKey)
	if v := os.Getenv("MR_DRIVES"); v != "" {
		cfg.EnvOverrides = append(cfg.EnvOverrides, "drives")
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
	switch c.Arr.ImportPolicy {
	case "always", "confident", "verified":
	default:
		errs = append(errs, fmt.Errorf("arr.import_policy must be always, confident or verified, not %q", c.Arr.ImportPolicy))
	}
	if c.Output.Resume && c.Output.ResumeMaxAge.D() <= 0 {
		errs = append(errs, errors.New("output.resume_max_age must be positive"))
	}
	if c.PollInterval.D() < 500*time.Millisecond {
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
	case "auto", "tmdb", "arr", "none":
	default:
		errs = append(errs, fmt.Errorf("metadata.provider %q must be auto, tmdb, arr or none", c.Metadata.Provider))
	}
	for name, app := range map[string]ArrApp{"radarr": c.Arr.Radarr, "sonarr": c.Arr.Sonarr} {
		if !app.Enabled {
			continue
		}
		if app.URL == "" || app.APIKey == "" {
			errs = append(errs, fmt.Errorf("arr.%s needs url and api_key", name))
		}
		switch strings.ToLower(app.ImportMode) {
		case "move", "copy":
		default:
			errs = append(errs, fmt.Errorf("arr.%s.import_mode %q must be move or copy", name, app.ImportMode))
		}
		if app.AddMissing && app.RootFolder == "" {
			errs = append(errs, fmt.Errorf("arr.%s.root_folder is required when add_missing is on", name))
		}
	}
	if c.Metadata.OCR.Enabled && c.Metadata.OCR.FramesPerMin <= 0 {
		errs = append(errs, errors.New("metadata.ocr.frames_per_minute must be positive"))
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

// SecretKeys are the dotted keys whose values never leave the server.
var SecretKeys = []string{"makemkv.key", "metadata.tmdb_api_key", "notify.ntfy_token", "notify.discord.bot_token", "notify.discord.webhook_url", "updates.token", "arr.radarr.api_key", "arr.sonarr.api_key"}

func (c *Config) secret(key string) *string {
	switch key {
	case "makemkv.key":
		return &c.MakeMKV.Key
	case "metadata.tmdb_api_key":
		return &c.Metadata.TMDBAPIKey
	case "notify.ntfy_token":
		return &c.Notify.NtfyToken
	case "notify.discord.bot_token":
		return &c.Notify.Discord.BotToken
	case "notify.discord.webhook_url":
		return &c.Notify.Discord.WebhookURL
	case "updates.token":
		return &c.Updates.Token
	case "arr.radarr.api_key":
		return &c.Arr.Radarr.APIKey
	case "arr.sonarr.api_key":
		return &c.Arr.Sonarr.APIKey
	}
	return nil
}

// Redacted returns a copy with secrets blanked and a map of which secrets
// are set, for the UI.
func (c Config) Redacted() (Config, map[string]bool) {
	set := map[string]bool{}
	cp := c
	for _, k := range SecretKeys {
		p := cp.secret(k)
		set[k] = *p != ""
		*p = ""
	}
	return cp, set
}

// MergeSecrets copies secrets from prev into c where c has none, except for
// keys listed in clear. A settings form never sends secrets back, so an
// empty value means "keep".
func (c *Config) MergeSecrets(prev *Config, clear []string) {
	cleared := map[string]bool{}
	for _, k := range clear {
		cleared[k] = true
	}
	for _, k := range SecretKeys {
		if cleared[k] {
			*c.secret(k) = ""
			continue
		}
		if *c.secret(k) == "" {
			*c.secret(k) = *prev.secret(k)
		}
	}
}
