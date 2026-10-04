// Package updates tells the web UI when a newer media-ripper release is
// out, from the project's GitHub releases.
package updates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Release is a published version.
type Release struct {
	Tag string `json:"tag"`
	URL string `json:"url"`
}

// Settings are read on every round, so changes in the web UI apply
// without a restart.
type Settings struct {
	Check bool
	Repo  string // "owner/name"
	Token string // needed for a private repository
}

// Watcher polls for the latest release. The zero value is not usable; set
// Current and Settings.
type Watcher struct {
	Current  string // this build's version, e.g. "v0.5.0" or "v0.5.0-3-gabc123"
	Settings func() Settings
	Interval time.Duration
	HTTP     *http.Client
	Logger   *slog.Logger
	APIBase  string // tests

	mu     sync.Mutex
	latest *Release
	warned bool
}

// Available returns the newer release, or nil when this build is current
// or nothing could be checked.
func (w *Watcher) Available() *Release {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.latest == nil || !Newer(w.Current, w.latest.Tag) {
		return nil
	}
	r := *w.latest
	return &r
}

// Run checks now and then every Interval until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = 12 * time.Hour
	}
	for {
		w.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// errNotVisible is GitHub's 404 for a private repository without a token.
var errNotVisible = errors.New("repository not visible: private without a token, or no releases")

func (w *Watcher) check(ctx context.Context) {
	s := w.Settings()
	if !s.Check || s.Repo == "" {
		return
	}
	rel, err := w.Latest(ctx, s)
	if err != nil {
		w.mu.Lock()
		first := !w.warned
		w.warned = true
		w.mu.Unlock()
		if first && w.Logger != nil {
			w.Logger.Info("update check unavailable", "repo", s.Repo, "err", err)
		}
		return
	}
	w.mu.Lock()
	w.latest = rel
	w.mu.Unlock()
	if Newer(w.Current, rel.Tag) && w.Logger != nil {
		w.Logger.Info("update available", "current", w.Current, "latest", rel.Tag, "url", rel.URL)
	}
}

// Latest asks GitHub for the newest published release.
func (w *Watcher) Latest(ctx context.Context, s Settings) (*Release, error) {
	base := w.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/repos/"+s.Repo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "media-ripper/"+w.Current)
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	client := w.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNotVisible
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("github: %s", resp.Status)
	}
	var r struct {
		Tag string `json:"tag_name"`
		URL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Tag == "" {
		return nil, fmt.Errorf("github: unexpected release data")
	}
	return &Release{Tag: r.Tag, URL: r.URL}, nil
}

// Newer reports whether latest is a later version than current. A build
// past a tag ("v0.5.0-3-gabc") counts as that tag; unversioned builds
// ("dev", a bare commit) never ask to be updated.
func Newer(current, latest string) bool {
	c, ok1 := parse(current)
	l, ok2 := parse(latest)
	if !ok1 || !ok2 {
		return false
	}
	for i := range c {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
