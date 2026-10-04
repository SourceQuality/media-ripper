// Package store persists job history and per-season episode progress as
// plain JSON files, so the state survives restarts and is easy to inspect or
// fix by hand.
package store

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store is a small file-backed state store.
type Store struct {
	dir string
	mu  sync.Mutex

	series  map[string]SeriesProgress
	discs   map[string]DiscRecord
	reviews map[string]json.RawMessage
	matches map[string]DiscMatch
	sets    map[string]BoxSet
	pending map[string]time.Time // jobs whose import must run again
}

// BoxSet tracks which discs of one catalogued release have been ripped.
type BoxSet struct {
	Kind    string `json:"kind"` // movie | series
	Title   string `json:"title"`
	Year    int    `json:"year,omitempty"`
	Release string `json:"release"` // catalogue release slug
	// Ripped maps a disc's catalogue slug ("S01D03") to when it was done.
	Ripped    map[string]RippedDisc `json:"ripped"`
	UpdatedAt time.Time             `json:"updated_at"`
}

// RippedDisc is one disc of a box set that went through.
type RippedDisc struct {
	Index int       `json:"index"`
	Name  string    `json:"name"`
	JobID string    `json:"job_id"`
	At    time.Time `json:"at"`
}

// DiscMatch is what a person confirmed a disc to be. It is used instead of
// the label lookup the next time the same disc (by fingerprint) goes in.
type DiscMatch struct {
	Kind   string `json:"kind"` // movie | tv
	Title  string `json:"title"`
	Year   int    `json:"year,omitempty"`
	TMDBID int    `json:"tmdb_id,omitempty"`
	TVDBID int    `json:"tvdb_id,omitempty"`
	Season int    `json:"season,omitempty"`
	Disc   int    `json:"disc,omitempty"`
	// Episodes maps MakeMKV title ids to the episode each holds; titles
	// not listed were left out.
	Episodes    map[int]int `json:"episodes,omitempty"`
	ConfirmedAt time.Time   `json:"confirmed_at"`
}

// SeriesProgress remembers where the next disc of a season starts.
type SeriesProgress struct {
	Series      string    `json:"series"`
	Season      int       `json:"season"`
	NextEpisode int       `json:"next_episode"`
	LastDisc    int       `json:"last_disc,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// DiscRecord notes that a fingerprint was already ripped.
type DiscRecord struct {
	Fingerprint string    `json:"fingerprint"`
	Label       string    `json:"label"`
	Title       string    `json:"title,omitempty"`
	RippedAt    time.Time `json:"ripped_at"`
	Outputs     []string  `json:"outputs,omitempty"`
}

// Open loads (or creates) the store in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o775); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, series: map[string]SeriesProgress{}, discs: map[string]DiscRecord{}, reviews: map[string]json.RawMessage{}, matches: map[string]DiscMatch{}, sets: map[string]BoxSet{}, pending: map[string]time.Time{}}
	if err := s.loadJSON("series.json", &s.series); err != nil {
		return nil, err
	}
	if err := s.loadJSON("discs.json", &s.discs); err != nil {
		return nil, err
	}
	if err := s.loadJSON("reviews.json", &s.reviews); err != nil {
		return nil, err
	}
	if err := s.loadJSON("matches.json", &s.matches); err != nil {
		return nil, err
	}
	if err := s.loadJSON("boxsets.json", &s.sets); err != nil {
		return nil, err
	}
	if err := s.loadJSON("pending-imports.json", &s.pending); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) loadJSON(name string, dst any) error {
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("%s is corrupt: %w (move it aside to reset)", name, err)
	}
	return nil
}

func (s *Store) saveJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.dir, name), data)
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func seriesKey(series string, season int) string {
	return fmt.Sprintf("%s|s%02d", strings.ToLower(strings.TrimSpace(series)), season)
}

// NextEpisode returns the episode number the next disc should start from.
func (s *Store) NextEpisode(series string, season int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.series[seriesKey(series, season)]; ok && p.NextEpisode > 0 {
		return p.NextEpisode
	}
	return 1
}

// SetNextEpisode records progress after a disc finishes.
func (s *Store) SetNextEpisode(series string, season, next, disc int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.series[seriesKey(series, season)] = SeriesProgress{Series: series, Season: season, NextEpisode: next, LastDisc: disc, UpdatedAt: time.Now()}
	return s.saveJSON("series.json", s.series)
}

// ResetSeries forgets progress for a season (or all seasons when season is 0).
func (s *Store) ResetSeries(series string, season int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.series {
		if strings.EqualFold(p.Series, series) && (season == 0 || p.Season == season) {
			delete(s.series, k)
		}
	}
	return s.saveJSON("series.json", s.series)
}

// Series lists progress entries.
func (s *Store) Series() []SeriesProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SeriesProgress, 0, len(s.series))
	for _, p := range s.series {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// Disc returns the record for a fingerprint, if it was ripped before.
func (s *Store) Disc(fp string) (DiscRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.discs[fp]
	return r, ok
}

// MarkDisc records a successful rip.
func (s *Store) MarkDisc(r DiscRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.RippedAt.IsZero() {
		r.RippedAt = time.Now()
	}
	s.discs[r.Fingerprint] = r
	return s.saveJSON("discs.json", s.discs)
}

// ForgetDisc allows a disc to be ripped again.
func (s *Store) ForgetDisc(fp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.discs, fp)
	return s.saveJSON("discs.json", s.discs)
}

// AppendHistory appends one record to history.jsonl.
func (s *Store) AppendHistory(v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "history.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

// History returns up to limit most recent raw history records, newest first.
func (s *Store) History(limit int) ([]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(filepath.Join(s.dir, "history.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var all []json.RawMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !json.Valid([]byte(line)) {
			continue
		}
		all = append(all, json.RawMessage(line))
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	// A job is recorded again when its import is retried; the newest
	// record is the one that counts.
	seen := map[string]bool{}
	out := all[:0]
	for _, raw := range all {
		var rec struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &rec) == nil && rec.ID != "" {
			if seen[rec.ID] {
				continue
			}
			seen[rec.ID] = true
		}
		out = append(out, raw)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SetImportPending marks (or clears) a job whose Radarr/Sonarr import has
// to run again, because it was cut short by a restart.
func (s *Store) SetImportPending(id string, pending bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pending {
		s.pending[id] = time.Now()
	} else if _, ok := s.pending[id]; ok {
		delete(s.pending, id)
	} else {
		return nil
	}
	return s.saveJSON("pending-imports.json", s.pending)
}

// PendingImports lists the jobs whose import has to run again, oldest
// first.
func (s *Store) PendingImports() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.pending))
	for id := range s.pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// SaveReview keeps a job that waits for a person to confirm its titles.
func (s *Store) SaveReview(id string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reviews[id] = data
	return s.saveJSON("reviews.json", s.reviews)
}

// Reviews returns the waiting jobs, oldest first.
func (s *Store) Reviews() []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.reviews))
	for id := range s.reviews {
		ids = append(ids, id)
	}
	sort.Strings(ids) // job ids start with their timestamp
	out := make([]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.reviews[id])
	}
	return out
}

// Review returns one waiting job.
func (s *Store) Review(id string) (json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.reviews[id]
	return r, ok
}

// DeleteReview removes a job once it has been dealt with.
func (s *Store) DeleteReview(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reviews, id)
	return s.saveJSON("reviews.json", s.reviews)
}

// SetDiscMatch remembers what a person confirmed a disc to be.
func (s *Store) SetDiscMatch(fp string, m DiscMatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ConfirmedAt.IsZero() {
		m.ConfirmedAt = time.Now()
	}
	s.matches[fp] = m
	return s.saveJSON("matches.json", s.matches)
}

// DiscMatch returns the confirmed match for a disc, if any.
func (s *Store) DiscMatch(fp string) (DiscMatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.matches[fp]
	return m, ok
}

// MarkBoxSetDisc records that a disc of a catalogued release was ripped.
func (s *Store) MarkBoxSetDisc(set BoxSet, slug string, d RippedDisc) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(set.Kind + "|" + set.Title + "|" + fmt.Sprint(set.Year) + "|" + set.Release)
	cur, ok := s.sets[key]
	if !ok {
		cur = set
		cur.Ripped = map[string]RippedDisc{}
	}
	cur.Ripped[slug] = d
	cur.UpdatedAt = time.Now()
	s.sets[key] = cur
	return s.saveJSON("boxsets.json", s.sets)
}

// BoxSets lists releases with ripped discs, most recently touched first.
func (s *Store) BoxSets() []BoxSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]BoxSet, 0, len(s.sets))
	for _, b := range s.sets {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// DiscInventory is what was read from a disc's filesystem, kept per job for
// the disc manifest (TheDiscDB contributions).
type DiscInventory struct {
	ContentHash string      `json:"content_hash"`
	Files       []FileEntry `json:"files"`
}

// FileEntry is one file on a disc.
type FileEntry struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified,omitempty"`
}

// SaveInventory writes a job's disc inventory to manifests/<job>.json.
func (s *Store) SaveInventory(jobID string, inv DiscInventory) error {
	dir := filepath.Join(s.dir, "manifests")
	if err := os.MkdirAll(dir, 0o775); err != nil {
		return err
	}
	data, err := json.Marshal(inv)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, filepath.Base(jobID)+".json"), data)
}

// Inventory reads a job's disc inventory.
func (s *Store) Inventory(jobID string) (DiscInventory, error) {
	var inv DiscInventory
	data, err := os.ReadFile(filepath.Join(s.dir, "manifests", filepath.Base(jobID)+".json"))
	if err != nil {
		return inv, err
	}
	return inv, json.Unmarshal(data, &inv)
}

// Secret returns a random key kept in the state folder, creating it on
// first use (mode 0600), e.g. the key that signs session cookies.
func (s *Store) Secret(name string, size int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, filepath.Base(name))
	if data, err := os.ReadFile(path); err == nil && len(data) >= size {
		return data[:size], nil
	}
	key := make([]byte, size)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, key, 0o600); err != nil {
		return nil, err
	}
	return key, os.Rename(tmp, path)
}
