package metadata

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// ArrLookup is the slice of the Radarr/Sonarr API the provider needs. The
// arr package implements it; keeping it as an interface avoids an import
// cycle and makes it easy to fake.
type ArrLookup interface {
	Lookup(ctx context.Context, term string) ([]ArrItem, error)
	// SeasonEpisodes returns the episodes of one season when the series is
	// in the library, else nil.
	SeasonEpisodes(ctx context.Context, item ArrItem, season int) ([]Episode, error)
}

// ArrItem is a lookup result in provider-neutral form.
type ArrItem struct {
	ID      int
	TMDBID  int
	TVDBID  int
	Title   string
	Year    int
	Runtime time.Duration
}

// ArrProvider identifies discs through Radarr and Sonarr lookups, which
// proxy TMDB and TVDB without needing separate API keys.
type ArrProvider struct {
	Radarr ArrLookup
	Sonarr ArrLookup
	Logger *slog.Logger
}

// Identify implements Provider.
func (p *ArrProvider) Identify(ctx context.Context, hint Hint, disc DiscHints) (*Identity, error) {
	id := &Identity{Kind: KindUnknown, Hint: hint, Source: "arr", Season: hint.Season, Disc: hint.Disc}
	if hint.Junk || hint.Query == "" {
		return id, nil
	}
	tvFirst := hint.LooksTV || (hint.Disc > 0 && disc.SimilarTitles >= 3) || (disc.SimilarTitles >= 4 && disc.LongestTitle < 75*time.Minute)
	order := []Kind{KindMovie, KindTV}
	if tvFirst {
		order = []Kind{KindTV, KindMovie}
	}
	type best struct {
		kind  Kind
		item  ArrItem
		score float64
	}
	var top *best
	var firstErr error
	for i, k := range order {
		var src ArrLookup
		if k == KindMovie {
			src = p.Radarr
		} else {
			src = p.Sonarr
		}
		if src == nil {
			continue
		}
		items, err := src.Lookup(ctx, hint.Query)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, it := range items {
			s := Similarity(hint.Query, it.Title)
			if hint.Year > 0 && abs(it.Year-hint.Year) <= 1 {
				s += 0.25
			}
			if top == nil || s > top.score+0.001 {
				top = &best{kind: k, item: it, score: s}
			}
		}
		if i == 0 && top != nil && top.score >= 0.8 {
			break
		}
	}
	if top == nil {
		if firstErr != nil {
			return id, firstErr
		}
		return id, nil
	}
	if top.score < 0.4 {
		return id, nil
	}
	if top.score > 1 {
		top.score = 1
	}
	id.Kind = top.kind
	id.Title = top.item.Title
	id.Year = top.item.Year
	id.TMDBID = top.item.TMDBID
	id.TVDBID = top.item.TVDBID
	id.Confidence = top.score
	switch top.kind {
	case KindMovie:
		id.Source = "radarr"
		id.Runtime = top.item.Runtime
	case KindTV:
		id.Source = "sonarr"
		id.EpisodeRuntime = top.item.Runtime
		if id.Season == 0 {
			id.Season = 1
		}
		if eps, err := p.Sonarr.SeasonEpisodes(ctx, top.item, id.Season); err == nil {
			id.Episodes = eps
		} else if p.Logger != nil {
			p.Logger.Warn("sonarr episodes", "err", err)
		}
	}
	return id, nil
}

// Chain tries providers in order and returns the first identification.
type Chain []Provider

// Identify implements Provider.
func (c Chain) Identify(ctx context.Context, hint Hint, disc DiscHints) (*Identity, error) {
	var last *Identity
	var firstErr error
	for _, p := range c {
		id, err := p.Identify(ctx, hint, disc)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if id.Identified() {
			return id, nil
		}
		last = id
	}
	if last != nil {
		return last, nil
	}
	if firstErr != nil {
		return &Identity{Kind: KindUnknown, Hint: hint}, firstErr
	}
	return &Identity{Kind: KindUnknown, Hint: hint}, fmt.Errorf("no metadata providers configured")
}
