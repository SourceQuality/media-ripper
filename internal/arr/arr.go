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

// WaitReady waits after Add until Sonarr has loaded the new series'
// episodes. Sonarr fills them in with a background refresh; an import
// started before that matches nothing and still reports "completed".
// Radarr knows a movie as soon as it is added.
func (c *Client) WaitReady(ctx context.Context, it Item, wait time.Duration) error {
	if c.Kind != Sonarr || it.ID == 0 {
		return nil
	}
	poll := c.Poll
	if poll <= 0 {
		poll = 3 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		eps, err := c.Episodes(ctx, it.ID)
		if err != nil {
			return err
		}
		if len(eps) > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sonarr had not loaded the episodes of %s after %s", it.Title, wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
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

// ImportOptions tunes Import.
type ImportOptions struct {
	// Wait bounds how long the import command may run.
	Wait time.Duration
	// Quality names the quality to record, e.g. "Bluray-1080p Remux". The
	// app would otherwise guess from the file name, which carries no source
	// ("HDTV-1080p" for a Blu-ray remux). Unknown names keep the guess.
	Quality string
}

// ImportResult says what the app was given and why it left the rest.
type ImportResult struct {
	Imported int                 // files sent to the app
	Rejected map[string][]string // file name -> reasons
	ItemID   int                 // the series or movie they were matched to
}

// Import has the app import every file under dir. It asks the app to
// preview the folder (its manual import matching), then imports exactly
// the matched files with a ManualImport command. The folder scan
// (DownloadedEpisodesScan) was tried first and, on Sonarr 4, reported
// "Failed to import" for files the preview matched cleanly, with no
// reason logged at the default level.
//
// The app always copies: a move needs it to delete from staging, which it
// often cannot (another user, another machine), and then it rolls the
// import back while still reporting success. The caller checks what the
// app kept (Holds) and tidies staging itself.
func (c *Client) Import(ctx context.Context, dir string, opts ImportOptions) (ImportResult, error) {
	res := ImportResult{Rejected: map[string][]string{}}
	var preview []map[string]any
	q := url.Values{"folder": {c.RemotePath(filepath.ToSlash(dir))}, "filterExistingFiles": {"true"}}
	if err := c.do(ctx, http.MethodGet, "/manualimport", q, nil, &preview); err != nil {
		return res, err
	}
	quality, err := c.qualityByName(ctx, opts.Quality)
	if err != nil {
		return res, err
	}
	var files []map[string]any
	for _, p := range preview {
		path, _ := p["path"].(string)
		name := filepath.Base(filepath.FromSlash(strings.ReplaceAll(path, "\\", "/")))
		if reasons := rejections(p); len(reasons) > 0 {
			res.Rejected[name] = reasons
			continue
		}
		f := map[string]any{"path": path, "languages": p["languages"], "releaseGroup": p["releaseGroup"], "indexerFlags": p["indexerFlags"], "quality": p["quality"]}
		if quality != nil {
			f["quality"] = quality
		}
		if c.Kind == Sonarr {
			series, _ := p["series"].(map[string]any)
			var eps []any
			if list, ok := p["episodes"].([]any); ok {
				for _, e := range list {
					if em, ok := e.(map[string]any); ok {
						eps = append(eps, em["id"])
					}
				}
			}
			if series == nil || len(eps) == 0 {
				res.Rejected[name] = []string{"no matching series episode"}
				continue
			}
			f["seriesId"], f["episodeIds"], f["releaseType"] = series["id"], eps, p["releaseType"]
			if res.ItemID == 0 {
				res.ItemID = int(num(series["id"]))
			}
		} else {
			movie, _ := p["movie"].(map[string]any)
			if movie == nil {
				res.Rejected[name] = []string{"no matching movie"}
				continue
			}
			f["movieId"] = movie["id"]
			if res.ItemID == 0 {
				res.ItemID = int(num(movie["id"]))
			}
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		if len(preview) == 0 {
			return res, fmt.Errorf("%s found no video files in %s", c.Kind, q.Get("folder"))
		}
		return res, fmt.Errorf("%s matched none of the files: %s", c.Kind, describeRejected(res.Rejected))
	}
	cmd, err := c.runCommand(ctx, map[string]any{"name": "ManualImport", "files": files, "importMode": "copy"}, opts.Wait)
	if err != nil {
		return res, err
	}
	if strings.EqualFold(cmd.Result, "unsuccessful") {
		return res, fmt.Errorf("%s: import %s", c.Kind, firstNonEmpty(cmd.Message, "unsuccessful"))
	}
	res.Imported = len(files)
	return res, nil
}

// Holds returns the sizes of the files the app has for a series or movie,
// which is how an import is confirmed: a ManualImport can report success
// for files it then failed to take.
func (c *Client) Holds(ctx context.Context, itemID int) (map[int64]bool, error) {
	path, q := "/moviefile", url.Values{"movieId": {strconv.Itoa(itemID)}}
	if c.Kind == Sonarr {
		path, q = "/episodefile", url.Values{"seriesId": {strconv.Itoa(itemID)}}
	}
	var files []struct {
		Size int64 `json:"size"`
	}
	if err := c.do(ctx, http.MethodGet, path, q, nil, &files); err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(files))
	for _, f := range files {
		out[f.Size] = true
	}
	return out, nil
}

type command struct {
	ID      int    `json:"id"`
	Status  string `json:"status"`
	Result  string `json:"result"`
	Message string `json:"message"`
}

// runCommand posts a command and waits for it to finish.
func (c *Client) runCommand(ctx context.Context, body map[string]any, wait time.Duration) (command, error) {
	var cmd command
	if err := c.do(ctx, http.MethodPost, "/command", nil, body, &cmd); err != nil {
		return cmd, err
	}
	if wait <= 0 {
		wait = 10 * time.Minute
	}
	poll := c.Poll
	if poll <= 0 {
		poll = 3 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		switch strings.ToLower(cmd.Status) {
		case "completed":
			return cmd, nil
		case "failed", "aborted", "cancelled":
			return cmd, fmt.Errorf("%s: %v command %s: %s", c.Kind, body["name"], cmd.Status, cmd.Message)
		}
		if time.Now().After(deadline) {
			return cmd, fmt.Errorf("%s: %v did not finish within %s", c.Kind, body["name"], wait)
		}
		select {
		case <-ctx.Done():
			return cmd, ctx.Err()
		case <-time.After(poll):
		}
		if err := c.do(ctx, http.MethodGet, "/command/"+strconv.Itoa(cmd.ID), nil, nil, &cmd); err != nil {
			return cmd, err
		}
	}
}

// qualityByName returns the app's quality object for name, or nil to keep
// the app's own guess when name is empty or unknown to this app.
func (c *Client) qualityByName(ctx context.Context, name string) (map[string]any, error) {
	if name == "" {
		return nil, nil
	}
	var defs []struct {
		Quality map[string]any `json:"quality"`
	}
	if err := c.do(ctx, http.MethodGet, "/qualitydefinition", nil, nil, &defs); err != nil {
		return nil, err
	}
	for _, d := range defs {
		if n, _ := d.Quality["name"].(string); strings.EqualFold(n, name) {
			return map[string]any{"quality": d.Quality, "revision": map[string]any{"version": 1, "real": 0, "isRepack": false}}, nil
		}
	}
	return nil, nil
}

func rejections(p map[string]any) []string {
	list, _ := p["rejections"].([]any)
	var out []string
	for _, r := range list {
		if rm, ok := r.(map[string]any); ok {
			if reason, _ := rm["reason"].(string); reason != "" {
				out = append(out, reason)
			}
		}
	}
	return out
}

func describeRejected(m map[string][]string) string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+" ("+strings.Join(m[n], "; ")+")")
	}
	return strings.Join(parts, ", ")
}

// DescribeRejected formats rejected files for a log line or warning.
func (r ImportResult) DescribeRejected() string { return describeRejected(r.Rejected) }

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
