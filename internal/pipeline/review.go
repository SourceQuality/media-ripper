package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/selector"
	"github.com/sourcequality/media-ripper/internal/store"
)

// Import policies: which discs Radarr/Sonarr import without a person
// confirming the titles first.
const (
	PolicyAlways    = "always"    // everything, as before
	PolicyConfident = "confident" // verified discs, and movies whose runtime matched
	PolicyVerified  = "verified"  // only discs checked title by title
)

// sourceManual marks an identity a person confirmed.
const sourceManual = "manual"

// needsReview reports whether a delivered job should wait for a person
// instead of being imported straight away.
func needsReview(s Job, rt *runtime) bool {
	if rt.arrFor(s.Identity) == nil || len(s.Outputs) == 0 {
		return false // nothing would be imported anyway
	}
	policy := rt.cfg.Arr.ImportPolicy
	if policy == PolicyAlways {
		return false
	}
	if s.Identity.Source == sourceManual {
		return false
	}
	if s.CatalogMatched > 0 && s.CatalogMatched == s.CatalogCompared {
		return false
	}
	if policy == PolicyConfident && s.Identity.Kind == metadata.KindMovie && s.Identity.Runtime > 0 && s.Selection != nil && len(s.Selection.Picks) == 1 {
		d := s.Selection.Picks[0].Title.Duration - s.Identity.Runtime
		if d < 0 {
			d = -d
		}
		return d > rt.cfg.Selection.MovieRuntimeTolerance.D()
	}
	return true
}

// holdForReview ends a job with its files in staging, waiting for a person.
func (m *Manager) holdForReview(job *Job) {
	job.setStage(StageReview, "waiting for review")
	job.logf("held for review: titles not verified (arr.import_policy %s)", job.rt.cfg.Arr.ImportPolicy)
	if err := m.deps.Store.SaveReview(job.ID, job.Snapshot()); err != nil {
		m.log.Warn("save review", "job", job.ID, "err", err)
	}
}

// Reviews lists the jobs waiting for a person, oldest first.
func (m *Manager) Reviews() []Job {
	var out []Job
	for _, raw := range m.deps.Store.Reviews() {
		var j Job
		if json.Unmarshal(raw, &j) == nil {
			j.Verification = verification(j)
			out = append(out, j)
		}
	}
	return out
}

// ReviewEdit is a person's correction of a held disc. Episodes maps each
// title to keep to its episode number (0 for a movie's feature); titles
// not listed are left out.
type ReviewEdit struct {
	Kind     metadata.Kind `json:"kind"`
	Title    string        `json:"title"`
	Year     int           `json:"year"`
	TMDBID   int           `json:"tmdb_id"`
	TVDBID   int           `json:"tvdb_id"`
	Season   int           `json:"season"`
	Episodes map[int]int   `json:"episodes"`
}

var reviewMu sync.Mutex // one approval at a time: they rename shared staging

// ApproveReview imports a held job, as it was or with a person's edit:
// staged files are renamed to the confirmed episodes, the import runs, and
// the match is remembered for the next time this disc goes in.
func (m *Manager) ApproveReview(ctx context.Context, id string, edit *ReviewEdit) (Job, error) {
	reviewMu.Lock()
	defer reviewMu.Unlock()
	raw, ok := m.deps.Store.Review(id)
	if !ok {
		return Job{}, errors.New("no such review")
	}
	var snap Job
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Job{}, err
	}
	rt := m.rt.Load()
	job := new(Job)
	*job = snap
	job.mu, job.rt = &sync.Mutex{}, rt
	if edit != nil {
		if err := applyEdit(job, edit); err != nil {
			return Job{}, err
		}
	}
	job.set(func(j *Job) {
		if j.Identity != nil {
			id := *j.Identity
			id.Source, id.Confidence = sourceManual, 1
			j.Identity = &id
		}
	})
	if err := m.restage(job); err != nil {
		return Job{}, err
	}
	job.logf("approved by a person")
	m.arrHandoff(ctx, job)
	job.setStage(StageDone, "done")

	s := job.Snapshot()
	if err := m.deps.Store.SetDiscMatch(s.Fingerprint, matchOf(s)); err != nil {
		m.log.Warn("remember match", "err", err)
	}
	if s.Identity.Kind == metadata.KindTV && s.Selection != nil {
		next := 0
		for _, p := range s.Selection.Picks {
			if p.Season == s.Identity.Season && p.Episode >= next {
				next = p.Episode + 1
			}
		}
		if next > m.deps.Store.NextEpisode(seriesKey(s.Identity), s.Identity.Season) {
			_ = m.deps.Store.SetNextEpisode(seriesKey(s.Identity), s.Identity.Season, next, s.Identity.Disc)
		}
	}
	if err := m.deps.Store.AppendHistory(historyRecord(s)); err != nil {
		m.log.Warn("write history", "err", err)
	}
	_ = m.deps.Store.DeleteReview(id)
	ev := m.event(job, "done")
	ev.Elapsed, ev.Error = s.Elapsed, strings.Join(s.Warnings, "; ")
	for _, o := range s.Outputs {
		ev.Outputs = append(ev.Outputs, o.Path)
	}
	rt.notifier.Send(context.WithoutCancel(ctx), ev)
	return s, nil
}

// DiscardReview forgets a held job and leaves its files where they are.
func (m *Manager) DiscardReview(id string) error {
	if _, ok := m.deps.Store.Review(id); !ok {
		return errors.New("no such review")
	}
	return m.deps.Store.DeleteReview(id)
}

// applyEdit rewrites a held job's identity and picks from a person's edit.
func applyEdit(job *Job, e *ReviewEdit) error {
	if e.Kind != metadata.KindMovie && e.Kind != metadata.KindTV {
		return fmt.Errorf("kind must be movie or tv")
	}
	if strings.TrimSpace(e.Title) == "" {
		return errors.New("title is required")
	}
	if len(e.Episodes) == 0 {
		return errors.New("keep at least one title")
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.Selection == nil {
		return errors.New("job has no titles")
	}
	id := metadata.Identity{Kind: e.Kind, Title: e.Title, Year: e.Year, TMDBID: e.TMDBID, TVDBID: e.TVDBID, Season: e.Season, Confidence: 1, Source: sourceManual}
	if job.Identity != nil {
		id.Disc, id.Hint = job.Identity.Disc, job.Identity.Hint
		if id.Kind == job.Identity.Kind && id.Title == job.Identity.Title {
			id.Episodes, id.Runtime, id.EpisodeRuntime = job.Identity.Episodes, job.Identity.Runtime, job.Identity.EpisodeRuntime
		}
	}
	if e.Kind == metadata.KindTV && e.Season <= 0 {
		return errors.New("season is required for TV")
	}
	sel := *job.Selection
	sel.Kind = e.Kind
	var picks []selector.Pick
	for _, p := range job.Selection.Picks {
		ep, keep := e.Episodes[p.Title.ID]
		if !keep {
			sel.Skipped = append(sel.Skipped, selector.Skipped{TitleID: p.Title.ID, Duration: p.Title.Duration, Reason: "left out by a person"})
			continue
		}
		p.Reason = "confirmed by a person"
		if e.Kind == metadata.KindTV {
			if ep <= 0 {
				return fmt.Errorf("title %d needs an episode number", p.Title.ID)
			}
			if ep != p.Episode || e.Season != p.Season {
				p.EpisodeTitle = episodeName(&id, ep)
			}
			p.Season, p.Episode, p.EpisodeEnd = e.Season, ep, 0
		} else {
			p.Season, p.Episode, p.EpisodeEnd, p.EpisodeTitle = 0, 0, 0, ""
		}
		picks = append(picks, p)
	}
	sort.SliceStable(picks, func(i, j int) bool { return picks[i].Episode < picks[j].Episode })
	sel.Picks = picks
	job.Identity, job.Selection = &id, &sel
	job.Catalog, job.CatalogMatched, job.CatalogCompared = "", 0, 0
	return nil
}

func episodeName(id *metadata.Identity, ep int) string {
	for _, e := range id.Episodes {
		if e.Number == ep {
			return e.Title
		}
	}
	return ""
}

// restage renames the job's staged files to what its picks now say and
// drops outputs of titles that were left out (their files stay put).
func (m *Manager) restage(job *Job) error {
	s := job.Snapshot()
	cfg := job.rt.cfg
	bluray := strings.Contains(strings.ToLower(s.DiscType), "blu-ray")
	byTitle := map[int]selector.Pick{}
	for _, p := range s.Selection.Picks {
		byTitle[p.Title.ID] = p
	}
	var outs []Output
	for _, o := range s.Outputs {
		p, keep := byTitle[o.TitleID]
		if !keep {
			job.logf("left out %s; the file stays at %s", filepath.Base(o.Path), o.Path)
			continue
		}
		dest, err := destPath(cfg, job.rt, s, p, filepath.Ext(o.Path), bluray)
		if err != nil {
			return err
		}
		if dest != o.Path {
			if _, err := os.Stat(dest); err == nil {
				return fmt.Errorf("%s already exists", dest)
			}
			if err := os.MkdirAll(filepath.Dir(dest), os.FileMode(cfg.Output.DirMode)); err != nil {
				return err
			}
			if err := os.Rename(o.Path, dest); err != nil {
				return fmt.Errorf("rename %s: %w", filepath.Base(o.Path), err)
			}
			job.logf("renamed %s → %s", filepath.Base(o.Path), filepath.Base(dest))
			o.Path = dest
		}
		o.Import = ""
		outs = append(outs, o)
	}
	if len(outs) == 0 {
		return errors.New("no delivered files left to import")
	}
	job.set(func(j *Job) { j.Outputs, j.Warnings = outs, nil })
	return nil
}

// matchOf is the confirmed match to remember for a disc.
func matchOf(s Job) store.DiscMatch {
	id := s.Identity
	m := store.DiscMatch{Kind: string(id.Kind), Title: id.Title, Year: id.Year, TMDBID: id.TMDBID, TVDBID: id.TVDBID, Season: id.Season, Disc: id.Disc, Episodes: map[int]int{}}
	for _, p := range s.Selection.Picks {
		m.Episodes[p.Title.ID] = p.Episode
	}
	return m
}

// confirmedIdentity turns a remembered match into the disc's identity.
func confirmedIdentity(dm store.DiscMatch, hint metadata.Hint) *metadata.Identity {
	return &metadata.Identity{Kind: metadata.Kind(dm.Kind), Title: dm.Title, Year: dm.Year, TMDBID: dm.TMDBID, TVDBID: dm.TVDBID,
		Season: dm.Season, Disc: dm.Disc, Confidence: 1, Source: sourceManual, Hint: hint}
}

// confirmedEntries are a remembered match's titles in catalogue form.
func confirmedEntries(dm store.DiscMatch) map[int]selector.CatalogEntry {
	out := map[int]selector.CatalogEntry{}
	for tid, ep := range dm.Episodes {
		if dm.Kind == string(metadata.KindMovie) {
			out[tid] = selector.CatalogEntry{Type: "MainMovie"}
		} else {
			out[tid] = selector.CatalogEntry{Type: "Episode", Season: dm.Season, Episode: ep}
		}
	}
	return out
}

// LookupResult is one candidate for the review form's title search.
type LookupResult struct {
	Kind   metadata.Kind `json:"kind"`
	Title  string        `json:"title"`
	Year   int           `json:"year,omitempty"`
	TMDBID int           `json:"tmdb_id,omitempty"`
	TVDBID int           `json:"tvdb_id,omitempty"`
}

// Lookup searches Sonarr (tv) or Radarr (movie) for the review form.
func (m *Manager) Lookup(ctx context.Context, kind metadata.Kind, q string) ([]LookupResult, error) {
	rt := m.rt.Load()
	client := rt.radarr
	if kind == metadata.KindTV {
		client = rt.sonarr
	}
	if client == nil {
		return nil, fmt.Errorf("no %s app is configured to search", kind)
	}
	items, err := client.Lookup(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]LookupResult, 0, len(items))
	for i, it := range items {
		if i == 15 {
			break
		}
		out = append(out, LookupResult{Kind: kind, Title: it.Title, Year: it.Year, TMDBID: it.TMDBID, TVDBID: it.TVDBID})
	}
	return out, nil
}
