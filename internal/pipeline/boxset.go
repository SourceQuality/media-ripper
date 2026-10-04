package pipeline

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sourcequality/media-ripper/internal/discdb"
	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/selector"
	"github.com/sourcequality/media-ripper/internal/store"
)

// markBoxSet records a finished disc against its catalogued release.
func (m *Manager) markBoxSet(s Job) {
	cd := s.CatalogDisc
	if cd == nil || cd.Release == "" || cd.Slug == "" {
		return
	}
	set := store.BoxSet{Kind: cd.Kind, Title: cd.Title, Year: cd.Year, Release: cd.Release}
	if err := m.deps.Store.MarkBoxSetDisc(set, cd.Slug, store.RippedDisc{Index: cd.Index, Name: cd.Name, JobID: s.ID, At: time.Now()}); err != nil {
		m.log.Warn("record box set", "err", err)
	}
}

// BoxSetView is a release with the discs ripped so far and the ones still
// missing, grouped by season.
type BoxSetView struct {
	Kind        string        `json:"kind"`
	Title       string        `json:"title"`
	Year        int           `json:"year,omitempty"`
	Release     string        `json:"release"`
	ReleaseName string        `json:"release_name"`
	Ripped      int           `json:"ripped"`
	Total       int           `json:"total"` // 0 when the catalogue could not be read
	Groups      []BoxSetGroup `json:"groups"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

// BoxSetGroup is one season (or "Discs" for releases without seasons).
type BoxSetGroup struct {
	Name  string       `json:"name"`
	Discs []BoxSetDisc `json:"discs"`
}

// BoxSetDisc is one disc chip.
type BoxSetDisc struct {
	Index  int       `json:"index"`
	Name   string    `json:"name"`
	Short  string    `json:"short"` // "D3"
	Ripped bool      `json:"ripped"`
	JobID  string    `json:"job_id,omitempty"`
	At     time.Time `json:"at,omitempty"`
}

var seasonDisc = regexp.MustCompile(`(?i)^(season\s+\d+)\s+disc\s+(\d+)`)

// BoxSets lists releases in progress with every catalogued disc, ripped or
// not. Without the catalogue only the ripped discs are known.
func (m *Manager) BoxSets(ctx context.Context) []BoxSetView {
	cat := m.rt.Load().catalog
	var out []BoxSetView
	for _, b := range m.deps.Store.BoxSets() {
		v := BoxSetView{Kind: b.Kind, Title: b.Title, Year: b.Year, Release: b.Release, ReleaseName: releaseName(b.Release), UpdatedAt: b.UpdatedAt}
		var all []discdb.DiscInfo
		if cat != nil {
			lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			all, _ = cat.ReleaseDiscs(lctx, discdb.Kind(b.Kind), b.Title, b.Year, b.Release)
			cancel()
		}
		known := map[string]bool{}
		for _, d := range all {
			known[d.Slug] = true
		}
		for slug, r := range b.Ripped {
			if !known[slug] { // catalogue unavailable or changed: still show it
				all = append(all, discdb.DiscInfo{Index: r.Index, Name: r.Name, Slug: slug})
			}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Index < all[j].Index })
		groups := map[string]*BoxSetGroup{}
		var order []string
		for _, d := range all {
			group, short := "Discs", "D"+strconv.Itoa(d.Index)
			if m := seasonDisc.FindStringSubmatch(d.Name); m != nil {
				group, short = m[1], "D"+m[2]
			}
			g, ok := groups[group]
			if !ok {
				g = &BoxSetGroup{Name: group}
				groups[group] = g
				order = append(order, group)
			}
			chip := BoxSetDisc{Index: d.Index, Name: d.Name, Short: short}
			if r, ok := b.Ripped[d.Slug]; ok {
				chip.Ripped, chip.JobID, chip.At = true, r.JobID, r.At
				v.Ripped++
			}
			g.Discs = append(g.Discs, chip)
		}
		for _, name := range order {
			v.Groups = append(v.Groups, *groups[name])
		}
		if len(all) > 0 && cat != nil {
			v.Total = len(all)
		}
		out = append(out, v)
	}
	return out
}

// backfillBoxSets fills the box-set record from history once, for discs
// ripped before it existed: each finished job that matched TheDiscDB is
// matched again from its recorded titles.
func (m *Manager) backfillBoxSets(ctx context.Context) {
	cat := m.rt.Load().catalog
	if cat == nil || len(m.deps.Store.BoxSets()) > 0 {
		return
	}
	hist, err := m.deps.Store.History(0)
	if err != nil {
		return
	}
	for _, raw := range hist {
		var h struct {
			ID         string              `json:"id"`
			Stage      Stage               `json:"stage"`
			FinishedAt time.Time           `json:"finished_at"`
			Catalog    string              `json:"catalog"`
			Identity   *metadata.Identity  `json:"identity"`
			Selection  *selector.Selection `json:"selection"`
		}
		if json.Unmarshal(raw, &h) != nil || (h.Stage != StageDone && h.Stage != StageReview) || h.Identity == nil || !h.Identity.Identified() || h.Selection == nil {
			continue
		}
		var scan []discdb.ScanTitle
		fromCatalog := h.Catalog != ""
		for _, p := range h.Selection.Picks {
			scan = append(scan, discdb.ScanTitle{ID: p.Title.ID, SourceFile: p.Title.SourceFile, Size: p.Title.SizeBytes})
			if strings.HasPrefix(p.Reason, "TheDiscDB") {
				fromCatalog = true
			}
		}
		if !fromCatalog {
			continue
		}
		kind := discdb.Movie
		if h.Identity.Kind == metadata.KindTV {
			kind = discdb.Series
		}
		lctx, cancel := context.WithTimeout(ctx, catalogLookupTimeout)
		match, err := cat.Find(lctx, kind, h.Identity.Title, h.Identity.Year, scan)
		cancel()
		if err != nil || match == nil {
			continue
		}
		set := store.BoxSet{Kind: string(kind), Title: h.Identity.Title, Year: h.Identity.Year, Release: match.Release}
		_ = m.deps.Store.MarkBoxSetDisc(set, match.Disc.Slug, store.RippedDisc{Index: match.Disc.Index, Name: match.Disc.Name, JobID: h.ID, At: h.FinishedAt})
		m.log.Info("box set backfilled", "title", h.Identity.Title, "disc", match.Disc.Name)
	}
}
