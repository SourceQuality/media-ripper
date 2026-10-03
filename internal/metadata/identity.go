package metadata

import (
	"context"
	"time"
)

// Episode is one TV episode of a season.
type Episode struct {
	Number  int           `json:"number"`
	Title   string        `json:"title"`
	Runtime time.Duration `json:"runtime,omitempty"`
}

// Identity is what the pipeline learned about the disc.
type Identity struct {
	Kind           Kind          `json:"kind"`
	Title          string        `json:"title"`
	Year           int           `json:"year,omitempty"`
	TMDBID         int           `json:"tmdb_id,omitempty"`
	TVDBID         int           `json:"tvdb_id,omitempty"`
	Runtime        time.Duration `json:"runtime,omitempty"` // movie runtime
	Season         int           `json:"season,omitempty"`
	Disc           int           `json:"disc,omitempty"`
	Episodes       []Episode     `json:"episodes,omitempty"` // season episodes, in order
	EpisodeRuntime time.Duration `json:"episode_runtime,omitempty"`
	Confidence     float64       `json:"confidence"`
	Source         string        `json:"source"`
	Hint           Hint          `json:"hint"`
}

// Identified reports whether a lookup produced a usable title.
func (id *Identity) Identified() bool {
	return id != nil && id.Kind != KindUnknown && id.Title != ""
}

// Provider resolves a label hint into an Identity.
type Provider interface {
	Identify(ctx context.Context, hint Hint, discHints DiscHints) (*Identity, error)
}

// DiscHints are facts from the disc scan that help choose movie vs TV when the
// label is ambiguous.
type DiscHints struct {
	LongestTitle   time.Duration
	SimilarTitles  int // number of titles within 25% of the median mid-length title
	MedianDuration time.Duration
}

// NoneProvider never identifies anything; the pipeline falls back to the
// unidentified strategy.
type NoneProvider struct{}

func (NoneProvider) Identify(_ context.Context, hint Hint, _ DiscHints) (*Identity, error) {
	return &Identity{Kind: KindUnknown, Hint: hint, Source: "none", Title: hint.Query, Year: hint.Year, Season: hint.Season, Disc: hint.Disc}, nil
}
