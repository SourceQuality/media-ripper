// Package discdb looks discs up in TheDiscDB (https://thediscdb.com), a
// community catalogue that maps each title of a published disc to what it
// holds: the main movie, episode S01E03, a named extra.
//
// The catalogue is read from its public source repository on GitHub
// (TheDiscDb/data) rather than the website: one tree listing a day, then
// only the disc files of the identified title, cached by content. A disc is
// matched by comparing the titles MakeMKV scanned (playlist and exact byte
// size) with each catalogued disc of that title, so nothing has to be read
// from the disc's filesystem.
package discdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Kind is the catalogue section.
type Kind string

const (
	Movie  Kind = "movie"
	Series Kind = "series"
)

// Disc is one catalogued disc (a discNN.json file).
type Disc struct {
	Index       int     `json:"Index"`
	Slug        string  `json:"Slug"`
	Name        string  `json:"Name"`
	Format      string  `json:"Format"`
	ContentHash string  `json:"ContentHash"`
	Titles      []Title `json:"Titles"`
}

// Title is one title of a catalogued disc.
type Title struct {
	Index      int    `json:"Index"`
	SourceFile string `json:"SourceFile"`
	SegmentMap string `json:"SegmentMap"`
	Duration   string `json:"Duration"`
	Size       int64  `json:"Size"`
	Item       *Item  `json:"Item"`
}

// Item says what a title is. Type is "MainMovie", "Episode", "Extra",
// "Trailer" and so on; untagged titles have no Item.
type Item struct {
	Type    string  `json:"Type"`
	Title   string  `json:"Title"`
	Season  flexInt `json:"Season"`
	Episode flexInt `json:"Episode"`
}

// flexInt reads a number written either as 1 or as "1".
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		*f = 0 // "Special", ranges and the like: not a plain number
		return nil
	}
	*f = flexInt(n)
	return nil
}

// ScanTitle is what the matcher needs from a MakeMKV title.
type ScanTitle struct {
	ID         int
	SourceFile string // "00010.mpls"; empty on DVDs
	Size       int64  // exact bytes
}

// Match is a catalogued disc that fits the scanned one.
type Match struct {
	Release  string // release folder, e.g. "the-complete-series-blu-ray-2021"
	Path     string // repository path of the disc file
	Disc     Disc
	Matched  int // scanned titles found on the catalogued disc
	Compared int // scanned titles that could be compared
	// ByID maps scanned title IDs to the catalogued title they matched.
	ByID map[int]Title
}

// Client reads the catalogue.
type Client struct {
	Repo     string        // "TheDiscDb/data"
	CacheDir string        // where the tree listing and disc files are kept
	TreeTTL  time.Duration // how long a tree listing is reused
	HTTP     *http.Client
	Logger   *slog.Logger

	apiBase, rawBase string // overridden in tests
}

// New returns a client for repo caching under dir.
func New(repo, dir string) *Client {
	if repo == "" {
		repo = "TheDiscDb/data"
	}
	return &Client{Repo: repo, CacheDir: dir, TreeTTL: 24 * time.Hour, HTTP: &http.Client{Timeout: 60 * time.Second},
		apiBase: "https://api.github.com", rawBase: "https://raw.githubusercontent.com"}
}

type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// Find looks for the catalogued disc of title (year) that matches the scan.
// It returns nil without error when the title or disc is not catalogued.
func (c *Client) Find(ctx context.Context, kind Kind, title string, year int, scan []ScanTitle) (*Match, error) {
	tree, err := c.tree(ctx)
	if err != nil {
		return nil, err
	}
	folders := map[string]bool{}
	prefix := "data/" + string(kind) + "/"
	for _, e := range tree {
		if e.Type != "tree" || !strings.HasPrefix(e.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(e.Path, prefix)
		if !strings.Contains(rest, "/") && folderMatches(rest, title, year) {
			folders[e.Path+"/"] = true
		}
	}
	if len(folders) == 0 {
		return nil, nil
	}
	var best, second *Match
	for _, e := range tree {
		if e.Type != "blob" || !discFile.MatchString(path.Base(e.Path)) {
			continue
		}
		dir := path.Dir(path.Dir(e.Path)) + "/"
		if !folders[dir] {
			continue
		}
		d, err := c.disc(ctx, e)
		if err != nil {
			return nil, err
		}
		m := score(d, scan)
		if m == nil {
			continue
		}
		m.Release, m.Path = path.Base(path.Dir(e.Path)), e.Path
		switch {
		case best == nil || m.Matched > best.Matched:
			best, second = m, best
		case second == nil || m.Matched > second.Matched:
			second = m
		}
	}
	if best == nil || !confident(best) {
		return nil, nil
	}
	// Two releases often share a pressing; only a tie on a different disc
	// layout is ambiguous.
	if second != nil && second.Matched == best.Matched && !sameLayout(best.Disc, second.Disc) {
		return nil, nil
	}
	return best, nil
}

var discFile = regexp.MustCompile(`^disc\d+\.json$`)

// confident requires most of the scanned titles to be found, and at least
// two, so a single coincidental size cannot decide.
func confident(m *Match) bool {
	return m.Matched >= 2 && m.Matched*10 >= m.Compared*6
}

// score compares a scan with a catalogued disc. Titles match on exact size,
// and on the playlist too when both sides name one.
func score(d *Disc, scan []ScanTitle) *Match {
	m := &Match{Disc: *d, ByID: map[int]Title{}}
	used := map[int]bool{}
	for _, s := range scan {
		if s.Size <= 0 {
			continue
		}
		m.Compared++
		for i, t := range d.Titles {
			if used[i] || t.Size != s.Size {
				continue
			}
			if s.SourceFile != "" && t.SourceFile != "" && !strings.EqualFold(s.SourceFile, t.SourceFile) {
				continue
			}
			used[i] = true
			m.Matched++
			m.ByID[s.ID] = t
			break
		}
	}
	if m.Matched == 0 {
		return nil
	}
	return m
}

func sameLayout(a, b Disc) bool {
	if a.ContentHash != "" && a.ContentHash == b.ContentHash {
		return true
	}
	if len(a.Titles) != len(b.Titles) {
		return false
	}
	sizes := map[int64]int{}
	for _, t := range a.Titles {
		sizes[t.Size]++
	}
	for _, t := range b.Titles {
		sizes[t.Size]--
	}
	for _, n := range sizes {
		if n != 0 {
			return false
		}
	}
	return true
}

// folderMatches compares a catalogue folder ("The Twilight Zone (1959)")
// with an identified title and year, allowing the year to be off by one
// (release vs. premiere dates) or unknown.
func folderMatches(folder, title string, year int) bool {
	name, fy := folder, 0
	if i := strings.LastIndex(folder, " ("); i > 0 && strings.HasSuffix(folder, ")") {
		if y, err := strconv.Atoi(folder[i+2 : len(folder)-1]); err == nil {
			name, fy = folder[:i], y
		}
	}
	if norm(name) != norm(title) {
		return false
	}
	return year == 0 || fy == 0 || abs(fy-year) <= 1
}

func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	out := b.String()
	return strings.TrimPrefix(out, "the")
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// tree returns the repository listing, refreshed at most once per TreeTTL.
// A stale copy is used when GitHub cannot be reached.
func (c *Client) tree(ctx context.Context) ([]treeEntry, error) {
	file := filepath.Join(c.CacheDir, "tree.json")
	if st, err := os.Stat(file); err == nil && time.Since(st.ModTime()) < c.TreeTTL {
		if t, err := readTree(file); err == nil {
			return t, nil
		}
	}
	var body struct {
		Tree      []treeEntry `json:"tree"`
		Truncated bool        `json:"truncated"`
	}
	u := fmt.Sprintf("%s/repos/%s/git/trees/HEAD?recursive=1", c.apiBase, c.Repo)
	err := c.getJSON(ctx, u, &body)
	if err == nil && body.Truncated {
		err = errors.New("tree listing truncated by GitHub")
	}
	if err != nil {
		if t, rerr := readTree(file); rerr == nil {
			c.log().Warn("thediscdb: using cached listing", "err", err)
			return t, nil
		}
		return nil, fmt.Errorf("thediscdb listing: %w", err)
	}
	if err := os.MkdirAll(c.CacheDir, 0o755); err == nil {
		if data, err := json.Marshal(body.Tree); err == nil {
			tmp := file + ".tmp"
			if os.WriteFile(tmp, data, 0o644) == nil {
				_ = os.Rename(tmp, file)
			}
		}
	}
	return body.Tree, nil
}

func readTree(file string) ([]treeEntry, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var t []treeEntry
	return t, json.Unmarshal(data, &t)
}

// disc returns a disc file, cached by its git blob hash (content
// addressed, so a cached copy is never stale).
func (c *Client) disc(ctx context.Context, e treeEntry) (*Disc, error) {
	file := filepath.Join(c.CacheDir, "blobs", e.SHA+".json")
	var d Disc
	if data, err := os.ReadFile(file); err == nil && json.Unmarshal(data, &d) == nil {
		return &d, nil
	}
	segs := strings.Split(e.Path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	u := fmt.Sprintf("%s/%s/HEAD/%s", c.rawBase, c.Repo, strings.Join(segs, "/"))
	data, err := c.get(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("thediscdb %s: %w", e.Path, err)
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("thediscdb %s: %w", e.Path, err)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err == nil {
		_ = os.WriteFile(file, data, 0o644)
	}
	return &d, nil
}

func (c *Client) getJSON(ctx context.Context, u string, out any) error {
	data, err := c.get(ctx, u)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (c *Client) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "media-ripper")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return data, nil
}

func (c *Client) log() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}
