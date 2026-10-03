// Package store persists job history and per-season episode progress as
// plain JSON files, so the state survives restarts and is easy to inspect or
// fix by hand.
package store

import (
	"bufio"
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

	series map[string]SeriesProgress
	discs  map[string]DiscRecord
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
	s := &Store{dir: dir, series: map[string]SeriesProgress{}, discs: map[string]DiscRecord{}}
	if err := s.loadJSON("series.json", &s.series); err != nil {
		return nil, err
	}
	if err := s.loadJSON("discs.json", &s.discs); err != nil {
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
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	return all, nil
}
