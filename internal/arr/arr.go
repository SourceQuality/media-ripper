// Package arr talks to Radarr and Sonarr: lookups for identification, and
// the import handoff after a rip is delivered.
package arr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sourcequality/media-ripper/internal/config"
)

// Kind distinguishes the two apps; they share most of the API shape.
type Kind string

const (
	Radarr Kind = "radarr"
	Sonarr Kind = "sonarr"
)

// Client is one Radarr or Sonarr instance.
type Client struct {
	Kind   Kind
	Cfg    config.ArrApp
	HTTP   *http.Client
	Logger *slog.Logger
	// Poll is how often a running command is re-checked (default 3s).
	Poll time.Duration
}

// New builds a client; nil when the app is not configured.
func New(kind Kind, cfg config.ArrApp, timeout time.Duration, log *slog.Logger) *Client {
	if !cfg.Configured() {
		return nil
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{Kind: kind, Cfg: cfg, HTTP: &http.Client{Timeout: timeout}, Logger: log}
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	u := strings.TrimRight(c.Cfg.URL, "/") + "/api/v3" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.Cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", c.Kind, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%s: invalid api key", c.Kind)
	}
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("%s: %s %s: http %d: %s", c.Kind, method, path, resp.StatusCode, msg)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Item is a movie (Radarr) or series (Sonarr) as the app describes it. The
// raw object is kept so an add can send it back unchanged plus our fields.
type Item struct {
	ID      int // 0 when not in the library
	TMDBID  int
	TVDBID  int
	Title   string
	Year    int
	Runtime time.Duration
	Seasons []Season
	Path    string
	raw     map[string]any
}

// Season is a season summary from a Sonarr lookup.
type Season struct {
	Number       int
	EpisodeCount int
}

// Episode is a Sonarr episode of a series in the library.
type Episode struct {
	Season  int
	Number  int
	Title   string
	Runtime time.Duration
}

func parseItem(raw map[string]any) Item {
	it := Item{raw: raw}
	it.ID = int(num(raw["id"]))
	it.TMDBID = int(num(raw["tmdbId"]))
	it.TVDBID = int(num(raw["tvdbId"]))
	it.Title, _ = raw["title"].(string)
	it.Year = int(num(raw["year"]))
	it.Runtime = time.Duration(num(raw["runtime"])) * time.Minute
	it.Path, _ = raw["path"].(string)
	if seasons, ok := raw["seasons"].([]any); ok {
		for _, s := range seasons {
			sm, _ := s.(map[string]any)
			sn := Season{Number: int(num(sm["seasonNumber"]))}
			if st, ok := sm["statistics"].(map[string]any); ok {
				sn.EpisodeCount = int(num(st["totalEpisodeCount"]))
			}
			it.Seasons = append(it.Seasons, sn)
		}
	}
	return it
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// Lookup searches the app's metadata source (TMDB for Radarr, TVDB/Skyhook
// for Sonarr). Results are in the app's relevance order.
func (c *Client) Lookup(ctx context.Context, term string) ([]Item, error) {
	path := "/movie/lookup"
	if c.Kind == Sonarr {
		path = "/series/lookup"
	}
	var raw []map[string]any
	if err := c.do(ctx, http.MethodGet, path, url.Values{"term": {term}}, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(raw))
	for _, r := range raw {
		out = append(out, parseItem(r))
	}
	return out, nil
}

// InLibrary finds the item by TMDB (Radarr) or TVDB (Sonarr) id.
func (c *Client) InLibrary(ctx context.Context, it Item) (Item, bool, error) {
	path, q := "/movie", url.Values{"tmdbId": {strconv.Itoa(it.TMDBID)}}
	if c.Kind == Sonarr {
		path, q = "/series", url.Values{"tvdbId": {strconv.Itoa(it.TVDBID)}}
	}
	var raw []map[string]any
	if err := c.do(ctx, http.MethodGet, path, q, nil, &raw); err != nil {
		return Item{}, false, err
	}
	for _, r := range raw {
		p := parseItem(r)
		if (c.Kind == Radarr && p.TMDBID == it.TMDBID) || (c.Kind == Sonarr && p.TVDBID == it.TVDBID) {
			return p, true, nil
		}
	}
	return Item{}, false, nil
}

// Add puts a looked-up item into the library using the configured root
// folder and quality profile, unmonitored unless configured otherwise.
func (c *Client) Add(ctx context.Context, it Item) (Item, error) {
	if it.raw == nil {
		return Item{}, errors.New("add needs a lookup result")
	}
	profile, err := c.qualityProfileID(ctx)
	if err != nil {
		return Item{}, err
	}
	body := map[string]any{}
	for k, v := range it.raw {
		body[k] = v
	}
	delete(body, "id")
	body["qualityProfileId"] = profile
	body["rootFolderPath"] = c.Cfg.RootFolder
	body["monitored"] = c.Cfg.Monitored
	path := "/movie"
	if c.Kind == Radarr {
		body["addOptions"] = map[string]any{"searchForMovie": false}
	} else {
		path = "/series"
		body["seasonFolder"] = true
		body["addOptions"] = map[string]any{"searchForMissingEpisodes": false}
		if seasons, ok := body["seasons"].([]any); ok {
			for _, s := range seasons {
				if sm, ok := s.(map[string]any); ok {
					sm["monitored"] = c.Cfg.Monitored
				}
			}
		}
	}
	var raw map[string]any
	if err := c.do(ctx, http.MethodPost, path, nil, body, &raw); err != nil {
		return Item{}, err
	}
	return parseItem(raw), nil
}

func (c *Client) qualityProfileID(ctx context.Context) (int, error) {
	var profiles []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, "/qualityprofile", nil, nil, &profiles); err != nil {
		return 0, err
	}
	if len(profiles) == 0 {
		return 0, fmt.Errorf("%s has no quality profiles", c.Kind)
	}
	want := strings.TrimSpace(c.Cfg.QualityProfile)
	if want == "" {
		return profiles[0].ID, nil
	}
	for _, p := range profiles {
		if strings.EqualFold(p.Name, want) || strconv.Itoa(p.ID) == want {
			return p.ID, nil
		}
	}
	return 0, fmt.Errorf("%s: quality profile %q not found", c.Kind, want)
}

// Episodes lists a library series' episodes (Sonarr only).
func (c *Client) Episodes(ctx context.Context, seriesID int) ([]Episode, error) {
	if c.Kind != Sonarr {
		return nil, nil
	}
	var raw []struct {
		SeasonNumber  int    `json:"seasonNumber"`
		EpisodeNumber int    `json:"episodeNumber"`
		Title         string `json:"title"`
		Runtime       int    `json:"runtime"`
	}
	if err := c.do(ctx, http.MethodGet, "/episode", url.Values{"seriesId": {strconv.Itoa(seriesID)}}, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Episode, 0, len(raw))
	for _, e := range raw {
		out = append(out, Episode{Season: e.SeasonNumber, Number: e.EpisodeNumber, Title: e.Title, Runtime: time.Duration(e.Runtime) * time.Minute})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Season != out[j].Season {
			return out[i].Season < out[j].Season
		}
		return out[i].Number < out[j].Number
	})
	return out, nil
}

// RemotePath translates a local path to how the app sees it, using the
// longest matching path_map prefix.
func (c *Client) RemotePath(local string) string {
	best := ""
	for from := range c.Cfg.PathMap {
		f := strings.TrimRight(from, "/")
		if (local == f || strings.HasPrefix(local, f+"/")) && len(f) > len(best) {
			best = f
		}
	}
	if best == "" {
		return local
	}
	to := strings.TrimRight(c.Cfg.PathMap[best], "/")
	if to == "" {
		to = c.Cfg.PathMap[best]
	}
	rest := strings.TrimPrefix(local, best)
	if !strings.HasPrefix(to, "/") && strings.Contains(to, "\\") {
		// Windows-side app: join with backslashes.
		return to + strings.ReplaceAll(rest, "/", "\\")
	}
	return to + rest
}

// Import asks the app to import everything under dir and waits for the
// command to finish. The app renames and moves (or copies) the files.
func (c *Client) Import(ctx context.Context, dir string, wait time.Duration) error {
	name := "DownloadedMoviesScan"
	if c.Kind == Sonarr {
		name = "DownloadedEpisodesScan"
	}
	mode := "Move"
	if strings.EqualFold(c.Cfg.ImportMode, "copy") {
		mode = "Copy"
	}
	body := map[string]any{"name": name, "path": c.RemotePath(filepath.ToSlash(dir)), "importMode": mode}
	var cmd struct {
		ID     int    `json:"id"`
		Status string `json:"status"`
	}
	if err := c.do(ctx, http.MethodPost, "/command", nil, body, &cmd); err != nil {
		return err
	}
	if wait <= 0 {
		wait = 10 * time.Minute
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		switch strings.ToLower(cmd.Status) {
		case "completed":
			return nil
		case "failed", "aborted", "cancelled":
			return fmt.Errorf("%s: import command %s", c.Kind, cmd.Status)
		}
		poll := c.Poll
		if poll <= 0 {
			poll = 3 * time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
		if err := c.do(ctx, http.MethodGet, "/command/"+strconv.Itoa(cmd.ID), nil, nil, &cmd); err != nil {
			return err
		}
	}
	return fmt.Errorf("%s: import did not finish within %s", c.Kind, wait)
}
