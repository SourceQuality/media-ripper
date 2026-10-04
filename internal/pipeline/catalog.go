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
// When the disc was already found by its content hash (byHash), that match
// is used as it is.
func (m *Manager) catalogEntries(ctx context.Context, job *Job, disc *makemkv.Disc, id *metadata.Identity, byHash *discdb.Match) (map[int]selector.CatalogEntry, string) {
	cat := job.rt.catalog
	if byHash != nil {
		return m.useMatch(job, byHash), "TheDiscDB"
	}
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
	ctx, cancel := context.WithTimeout(ctx, catalogLookupTimeout)
	defer cancel()
	match, err := cat.Find(ctx, kind, id.Title, id.Year, scanTitles(disc))
	switch {
	case err != nil:
		job.logf("thediscdb: %v; choosing titles by length", err)
		m.log.Warn("thediscdb lookup", "job", job.ID, "err", err)
		return nil, ""
	case match == nil:
		job.logf("thediscdb: no catalogued disc of %s matches; choosing titles by length", id.Title)
		return nil, ""
	}
	return m.useMatch(job, match), "TheDiscDB"
}

// useMatch records a catalogue match on the job and returns what it says
// each scanned title is. A content hash equal to the catalogued disc's
// proves it is that exact pressing.
func (m *Manager) useMatch(job *Job, match *discdb.Match) map[int]selector.CatalogEntry {
	s := job.Snapshot()
	sameDisc := s.ContentHash != "" && strings.EqualFold(s.ContentHash, match.Disc.ContentHash)
	job.logf("thediscdb: %s, %s (%d of %d titles match%s)", match.Release, match.Disc.Name, match.Matched, match.Compared, map[bool]string{true: "; content hash matches", false: ""}[sameDisc])
	job.set(func(j *Job) {
		j.Catalog = fmt.Sprintf("TheDiscDB (%s, %s)", releaseName(match.Release), match.Disc.Name)
		j.CatalogMatched, j.CatalogCompared = match.Matched, match.Compared
		j.HashMatched = sameDisc
		if j.Identity != nil {
			kind := "movie"
			if j.Identity.Kind == metadata.KindTV {
				kind = "series"
			}
			j.CatalogDisc = &CatalogDisc{Kind: kind, Title: j.Identity.Title, Year: j.Identity.Year, Release: match.Release,
				Index: match.Disc.Index, Name: match.Disc.Name, Slug: match.Disc.Slug}
		}
	})
	entries := map[int]selector.CatalogEntry{}
	for tid, t := range match.ByID {
		e := selector.CatalogEntry{}
		if t.Item != nil {
			e = selector.CatalogEntry{Type: t.Item.Type, Title: t.Item.Title, Season: int(t.Item.Season), Episode: int(t.Item.Episode)}
		}
		entries[tid] = e
	}
	return entries
}

// identifyByHash asks TheDiscDB which disc has this content hash, for discs
// the label could not identify. thediscdb.com being unreachable (often a
// DNS filter) is logged once per job and the usual rules carry on.
func (m *Manager) identifyByHash(ctx context.Context, job *Job, disc *makemkv.Disc, hint metadata.Hint) (*metadata.Identity, *discdb.Match) {
	cat := job.rt.catalog
	hash := job.Snapshot().ContentHash
	if cat == nil || hash == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, catalogLookupTimeout)
	defer cancel()
	hm, err := cat.FindByHash(ctx, hash, scanTitles(disc))
	switch {
	case err != nil:
		job.logf("thediscdb: content hash lookup failed (%v); allow thediscdb.com through your DNS filter to identify discs by content", err)
		return nil, nil
	case hm == nil:
		job.logf("thediscdb: content hash %s is not catalogued", hash)
		return nil, nil
	}
	id := &metadata.Identity{Title: hm.Title, Year: hm.Year, TMDBID: hm.TMDBID, Confidence: 1, Source: "thediscdb", Hint: hint, Kind: metadata.KindMovie}
	if hm.Kind == discdb.Series {
		id.Kind = metadata.KindTV
		for _, t := range hm.ByID {
			if t.Item != nil && t.Item.Season > 0 {
				id.Season = int(t.Item.Season)
				break
			}
		}
	}
	job.logf("thediscdb: content hash identifies %s (%d)", hm.Title, hm.Year)
	match := hm.Match
	return id, &match
}

func scanTitles(disc *makemkv.Disc) []discdb.ScanTitle {
	scan := make([]discdb.ScanTitle, 0, len(disc.Titles))
	for _, t := range disc.Titles {
		scan = append(scan, discdb.ScanTitle{ID: t.ID, SourceFile: t.SourceFile, Size: t.SizeBytes})
	}
	return scan
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
		switch strings.ToLower(w) {
		case "dvd", "uhd", "4k", "hd":
			w = strings.ToUpper(w)
		default:
			if w != "" {
				w = strings.ToUpper(w[:1]) + w[1:]
			}
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
