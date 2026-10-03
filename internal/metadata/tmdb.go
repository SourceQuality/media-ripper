package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TMDB looks discs up on The Movie Database.
type TMDB struct {
	APIKey   string
	Language string
	BaseURL  string
	Client   *http.Client
	Logger   *slog.Logger
}

// NewTMDB builds a client with sane timeouts.
func NewTMDB(apiKey, language string, timeout time.Duration) *TMDB {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &TMDB{
		APIKey:   apiKey,
		Language: language,
		BaseURL:  "https://api.themoviedb.org/3",
		Client:   &http.Client{Timeout: timeout},
	}
}

type tmdbMovie struct {
	ID          int     `json:"id"`
	Title       string  `json:"title"`
	Original    string  `json:"original_title"`
	ReleaseDate string  `json:"release_date"`
	Popularity  float64 `json:"popularity"`
	Runtime     int     `json:"runtime"`
	VoteCount   int     `json:"vote_count"`
}

type tmdbTV struct {
	ID             int     `json:"id"`
	Name           string  `json:"name"`
	Original       string  `json:"original_name"`
	FirstAirDate   string  `json:"first_air_date"`
	Popularity     float64 `json:"popularity"`
	EpisodeRunTime []int   `json:"episode_run_time"`
	Seasons        []struct {
		SeasonNumber int `json:"season_number"`
		EpisodeCount int `json:"episode_count"`
	} `json:"seasons"`
	VoteCount int `json:"vote_count"`
}

type tmdbSeason struct {
	Episodes []struct {
		EpisodeNumber int    `json:"episode_number"`
		Name          string `json:"name"`
		Runtime       int    `json:"runtime"`
	} `json:"episodes"`
}

func (t *TMDB) get(ctx context.Context, path string, q url.Values, out any) error {
	if t.APIKey == "" {
		return fmt.Errorf("tmdb api key not configured")
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("api_key", t.APIKey)
	if t.Language != "" {
		q.Set("language", t.Language)
	}
	u := strings.TrimRight(t.BaseURL, "/") + path + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := t.Client.Do(req)
		if err != nil {
			lastErr = err
		} else {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusOK:
				return json.Unmarshal(body, out)
			case resp.StatusCode == http.StatusUnauthorized:
				return fmt.Errorf("tmdb: invalid api key")
			case resp.StatusCode == http.StatusNotFound:
				return fmt.Errorf("tmdb: not found")
			case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
				lastErr = fmt.Errorf("tmdb: http %d", resp.StatusCode)
			default:
				return fmt.Errorf("tmdb: http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 800 * time.Millisecond):
		}
	}
	return lastErr
}

type candidate struct {
	kind  Kind
	id    int
	title string
	year  int
	score float64
	pop   float64
	votes int
}

func yearOf(date string) int {
	if len(date) >= 4 {
		y, _ := strconv.Atoi(date[:4])
		return y
	}
	return 0
}

func (t *TMDB) searchMovies(ctx context.Context, q string, year int) ([]candidate, error) {
	v := url.Values{"query": {q}, "include_adult": {"false"}}
	if year > 0 {
		v.Set("year", strconv.Itoa(year))
	}
	var res struct {
		Results []tmdbMovie `json:"results"`
	}
	if err := t.get(ctx, "/search/movie", v, &res); err != nil {
		return nil, err
	}
	var out []candidate
	for _, m := range res.Results {
		s := Similarity(q, m.Title)
		if o := Similarity(q, m.Original); o > s {
			s = o
		}
		out = append(out, candidate{kind: KindMovie, id: m.ID, title: m.Title, year: yearOf(m.ReleaseDate), score: s, pop: m.Popularity, votes: m.VoteCount})
	}
	return out, nil
}

func (t *TMDB) searchTV(ctx context.Context, q string, year int) ([]candidate, error) {
	v := url.Values{"query": {q}, "include_adult": {"false"}}
	if year > 0 {
		v.Set("first_air_date_year", strconv.Itoa(year))
	}
	var res struct {
		Results []tmdbTV `json:"results"`
	}
	if err := t.get(ctx, "/search/tv", v, &res); err != nil {
		return nil, err
	}
	var out []candidate
	for _, m := range res.Results {
		s := Similarity(q, m.Name)
		if o := Similarity(q, m.Original); o > s {
			s = o
		}
		out = append(out, candidate{kind: KindTV, id: m.ID, title: m.Name, year: yearOf(m.FirstAirDate), score: s, pop: m.Popularity, votes: m.VoteCount})
	}
	return out, nil
}

func rank(cands []candidate, hintYear int) {
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		ay := hintYear > 0 && abs(a.year-hintYear) <= 1
		by := hintYear > 0 && abs(b.year-hintYear) <= 1
		as := a.score + boolf(ay)*0.25
		bs := b.score + boolf(by)*0.25
		if as != bs {
			return as > bs
		}
		return a.pop*float64(a.votes+1) > b.pop*float64(b.votes+1)
	})
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func boolf(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Identify implements Provider.
func (t *TMDB) Identify(ctx context.Context, hint Hint, disc DiscHints) (*Identity, error) {
	if hint.Junk || hint.Query == "" {
		return &Identity{Kind: KindUnknown, Hint: hint, Source: "tmdb"}, nil
	}
	id := &Identity{Kind: KindUnknown, Hint: hint, Source: "tmdb", Season: hint.Season, Disc: hint.Disc}

	// Decide the search order from the label and the shape of the disc.
	tvFirst := hint.LooksTV || (hint.Disc > 0 && disc.SimilarTitles >= 3) || (disc.SimilarTitles >= 4 && disc.LongestTitle < 75*time.Minute)

	type result struct {
		cands []candidate
		err   error
	}
	run := func(fn func(context.Context, string, int) ([]candidate, error)) result {
		c, err := fn(ctx, hint.Query, hint.Year)
		if err == nil && len(c) == 0 && hint.Year > 0 {
			c, err = fn(ctx, hint.Query, 0)
		}
		return result{c, err}
	}

	var order []Kind
	if tvFirst {
		order = []Kind{KindTV, KindMovie}
	} else {
		order = []Kind{KindMovie, KindTV}
	}
	var best *candidate
	var firstErr error
	for i, k := range order {
		var r result
		if k == KindMovie {
			r = run(t.searchMovies)
		} else {
			r = run(t.searchTV)
		}
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		rank(r.cands, hint.Year)
		if len(r.cands) == 0 {
			continue
		}
		c := r.cands[0]
		// Accept the preferred kind on a solid match; otherwise compare both.
		if i == 0 && c.score >= 0.8 {
			best = &c
			break
		}
		if best == nil || c.score > best.score+0.15 {
			best = &c
		}
	}
	if best == nil {
		if firstErr != nil {
			return id, firstErr
		}
		return id, nil
	}
	if best.score < 0.4 {
		if t.Logger != nil {
			t.Logger.Info("tmdb: no confident match", "query", hint.Query, "best", best.title, "score", best.score)
		}
		return id, nil
	}
	id.Kind = best.kind
	id.TMDBID = best.id
	id.Title = best.title
	id.Year = best.year
	id.Confidence = best.score

	switch best.kind {
	case KindMovie:
		var m tmdbMovie
		if err := t.get(ctx, "/movie/"+strconv.Itoa(best.id), nil, &m); err == nil {
			id.Runtime = time.Duration(m.Runtime) * time.Minute
			if m.Title != "" {
				id.Title = m.Title
			}
		}
	case KindTV:
		var tv tmdbTV
		if err := t.get(ctx, "/tv/"+strconv.Itoa(best.id), nil, &tv); err == nil {
			if len(tv.EpisodeRunTime) > 0 {
				id.EpisodeRuntime = time.Duration(tv.EpisodeRunTime[0]) * time.Minute
			}
			if tv.Name != "" {
				id.Title = tv.Name
			}
		}
		if id.Season == 0 {
			id.Season = 1
		}
		var season tmdbSeason
		if err := t.get(ctx, "/tv/"+strconv.Itoa(best.id)+"/season/"+strconv.Itoa(id.Season), nil, &season); err == nil {
			var total time.Duration
			var n int
			for _, e := range season.Episodes {
				ep := Episode{Number: e.EpisodeNumber, Title: e.Name, Runtime: time.Duration(e.Runtime) * time.Minute}
				id.Episodes = append(id.Episodes, ep)
				if ep.Runtime > 0 {
					total += ep.Runtime
					n++
				}
			}
			if id.EpisodeRuntime == 0 && n > 0 {
				id.EpisodeRuntime = total / time.Duration(n)
			}
		}
	}
	return id, nil
}
