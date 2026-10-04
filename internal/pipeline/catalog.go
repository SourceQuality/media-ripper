package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sourcequality/media-ripper/internal/discdb"
	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/selector"
)

// catalogLookupTimeout bounds a TheDiscDB lookup. The first lookup of a
// title fetches its disc files; later ones come from the cache.
const catalogLookupTimeout = 90 * time.Second

// catalogEntries looks the identified disc up in TheDiscDB and returns what
// it says each scanned title is, or nil to fall back to the usual rules.
// Lookup problems are logged and never fail the job.
func (m *Manager) catalogEntries(ctx context.Context, job *Job, disc *makemkv.Disc, id *metadata.Identity) (map[int]selector.CatalogEntry, string) {
	cat := job.rt.catalog
	if cat == nil || id == nil || !id.Identified() {
		return nil, ""
	}
	kind := discdb.Movie
	switch id.Kind {
	case metadata.KindMovie:
	case metadata.KindTV:
		kind = discdb.Series
	default:
		return nil, ""
	}
	scan := make([]discdb.ScanTitle, 0, len(disc.Titles))
	for _, t := range disc.Titles {
		scan = append(scan, discdb.ScanTitle{ID: t.ID, SourceFile: t.SourceFile, Size: t.SizeBytes})
	}
	ctx, cancel := context.WithTimeout(ctx, catalogLookupTimeout)
	defer cancel()
	match, err := cat.Find(ctx, kind, id.Title, id.Year, scan)
	switch {
	case err != nil:
		job.logf("thediscdb: %v; choosing titles by length", err)
		m.log.Warn("thediscdb lookup", "job", job.ID, "err", err)
		return nil, ""
	case match == nil:
		job.logf("thediscdb: no catalogued disc of %s matches; choosing titles by length", id.Title)
		return nil, ""
	}
	job.logf("thediscdb: %s, %s (%d of %d titles match)", match.Release, match.Disc.Name, match.Matched, match.Compared)
	job.set(func(j *Job) {
		j.Catalog = fmt.Sprintf("TheDiscDB (%s, %s)", releaseName(match.Release), match.Disc.Name)
	})
	entries := map[int]selector.CatalogEntry{}
	for tid, t := range match.ByID {
		e := selector.CatalogEntry{}
		if t.Item != nil {
			e = selector.CatalogEntry{Type: t.Item.Type, Title: t.Item.Title, Season: int(t.Item.Season), Episode: int(t.Item.Episode)}
		}
		entries[tid] = e
	}
	return entries, "TheDiscDB"
}

// releaseName turns a release folder slug into words:
// "the-complete-series-blu-ray-2021" → "The Complete Series Blu-ray 2021".
func releaseName(slug string) string {
	words := strings.Split(slug, "-")
	out := make([]string, 0, len(words))
	for i := 0; i < len(words); i++ {
		w := words[i]
		if strings.EqualFold(w, "blu") && i+1 < len(words) && strings.EqualFold(words[i+1], "ray") {
			out = append(out, "Blu-ray")
			i++
			continue
		}
		if w != "" {
			w = strings.ToUpper(w[:1]) + w[1:]
		}
		out = append(out, w)
	}
	return strings.Join(out, " ")
}

// adoptCatalogSeason trusts the catalogue's season over the label's when
// every episode it picked is from one other season.
func adoptCatalogSeason(job *Job, id *metadata.Identity, sel *selector.Selection) {
	if sel == nil || id.Kind != metadata.KindTV || len(sel.Picks) == 0 {
		return
	}
	season := sel.Picks[0].Season
	for _, p := range sel.Picks {
		if p.Season != season {
			return
		}
	}
	if season == 0 || season == id.Season {
		return
	}
	job.logf("thediscdb: season %d (label said %d)", season, id.Season)
	id.Season = season
	sel.NextEpisode = 0
	for _, p := range sel.Picks {
		if p.Episode >= sel.NextEpisode {
			sel.NextEpisode = p.Episode + 1
		}
	}
	job.set(func(j *Job) { j.Identity = id })
}
