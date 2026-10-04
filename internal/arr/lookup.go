package arr

import (
	"context"

	"github.com/sourcequality/media-ripper/internal/metadata"
)

// LookupAdapter exposes a Client as a metadata.ArrLookup.
type LookupAdapter struct{ *Client }

// Lookup implements metadata.ArrLookup.
func (a LookupAdapter) Lookup(ctx context.Context, term string) ([]metadata.ArrItem, error) {
	items, err := a.Client.Lookup(ctx, term)
	if err != nil {
		return nil, err
	}
	out := make([]metadata.ArrItem, 0, len(items))
	for _, it := range items {
		out = append(out, metadata.ArrItem{ID: it.ID, TMDBID: it.TMDBID, TVDBID: it.TVDBID, Title: it.Title, Year: it.Year, Runtime: it.Runtime})
	}
	return out, nil
}

// SeasonEpisodes implements metadata.ArrLookup. Episodes are only known for
// series already in the Sonarr library.
func (a LookupAdapter) SeasonEpisodes(ctx context.Context, item metadata.ArrItem, season int) ([]metadata.Episode, error) {
	if a.Kind != Sonarr {
		return nil, nil
	}
	lib, ok, err := a.InLibrary(ctx, Item{TVDBID: item.TVDBID})
	if err != nil || !ok {
		return nil, err
	}
	eps, err := a.Episodes(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	var out []metadata.Episode
	for _, e := range eps {
		if e.Season == season {
			out = append(out, metadata.Episode{Number: e.Number, Title: e.Title, Runtime: e.Runtime})
		}
	}
	return out, nil
}
