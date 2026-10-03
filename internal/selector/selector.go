// Package selector decides which titles on a disc are the movie or the
// episodes, leaving trailers, extras, menus and decoy playlists behind.
package selector

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
)

// Options tunes the heuristics. Zero values fall back to sane defaults.
type Options struct {
	MovieRuntimeTolerance time.Duration
	TVEpisodeTolerance    float64
	MinMovieDuration      time.Duration
	MinEpisodeDuration    time.Duration
	UnidentifiedStrategy  string // longest | all | skip
	AllowDoubleEpisodes   bool
	// NextEpisode is the first episode number to assign on a TV disc.
	NextEpisode int
}

func (o *Options) defaults() {
	if o.MovieRuntimeTolerance == 0 {
		o.MovieRuntimeTolerance = 8 * time.Minute
	}
	if o.TVEpisodeTolerance == 0 {
		o.TVEpisodeTolerance = 0.35
	}
	if o.MinMovieDuration == 0 {
		o.MinMovieDuration = 40 * time.Minute
	}
	if o.MinEpisodeDuration == 0 {
		o.MinEpisodeDuration = 8 * time.Minute
	}
	if o.UnidentifiedStrategy == "" {
		o.UnidentifiedStrategy = "longest"
	}
	if o.NextEpisode <= 0 {
		o.NextEpisode = 1
	}
}

// Pick is one title to rip and how to name it.
type Pick struct {
	Title        *makemkv.Title `json:"title"`
	Season       int            `json:"season,omitempty"`
	Episode      int            `json:"episode,omitempty"`
	EpisodeEnd   int            `json:"episode_end,omitempty"` // > Episode for double episodes
	EpisodeTitle string         `json:"episode_title,omitempty"`
	Reason       string         `json:"reason"`
}

// Selection is the outcome for one disc.
type Selection struct {
	Kind    metadata.Kind `json:"kind"`
	Picks   []Pick        `json:"picks"`
	Skipped []Skipped     `json:"skipped,omitempty"`
	Notes   []string      `json:"notes,omitempty"`
	// NextEpisode is the episode number the next disc of this season starts at.
	NextEpisode int `json:"next_episode,omitempty"`
}

// Skipped records why a title was left on the disc; it shows in the UI/logs.
type Skipped struct {
	TitleID  int           `json:"title_id"`
	Duration time.Duration `json:"duration"`
	Reason   string        `json:"reason"`
}

// ErrNothingToRip is returned when the strategy decides to skip the disc.
var ErrNothingToRip = errors.New("no titles selected")

// Select picks the titles for a disc.
func Select(disc *makemkv.Disc, id *metadata.Identity, opts Options) (*Selection, error) {
	opts.defaults()
	if disc == nil || len(disc.Titles) == 0 {
		return nil, errors.New("disc has no titles")
	}
	sel := &Selection{Kind: metadata.KindUnknown}
	if id != nil {
		sel.Kind = id.Kind
	}
	switch {
	case id != nil && id.Kind == metadata.KindMovie:
		selectMovie(disc, id, opts, sel)
	case id != nil && id.Kind == metadata.KindTV:
		selectTV(disc, id, opts, sel)
	default:
		selectUnidentified(disc, opts, sel)
	}
	if len(sel.Picks) == 0 {
		return sel, ErrNothingToRip
	}
	return sel, nil
}

// dedupe collapses titles that are the same content under different playlist
// numbers (Blu-ray obfuscation). The preferred representative has the most
// chapters, then the lowest id.
func dedupe(titles []*makemkv.Title, sel *Selection) []*makemkv.Title {
	type key struct {
		segs string
		dur  time.Duration
	}
	best := map[key]*makemkv.Title{}
	var order []key
	for _, t := range titles {
		k := key{segs: segKey(t), dur: t.Duration}
		if cur, ok := best[k]; ok {
			if better(t, cur) {
				sel.Skipped = append(sel.Skipped, Skipped{TitleID: cur.ID, Duration: cur.Duration, Reason: fmt.Sprintf("duplicate of title %d", t.ID)})
				best[k] = t
			} else {
				sel.Skipped = append(sel.Skipped, Skipped{TitleID: t.ID, Duration: t.Duration, Reason: fmt.Sprintf("duplicate of title %d", cur.ID)})
			}
			continue
		}
		best[k] = t
		order = append(order, k)
	}
	out := make([]*makemkv.Title, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

func segKey(t *makemkv.Title) string {
	if len(t.Segments) == 0 {
		return fmt.Sprintf("id:%d", t.ID)
	}
	segs := append([]int(nil), t.Segments...)
	sort.Ints(segs)
	var b strings.Builder
	for _, s := range segs {
		fmt.Fprintf(&b, "%d,", s)
	}
	return b.String()
}

func better(a, b *makemkv.Title) bool {
	if a.Chapters != b.Chapters {
		return a.Chapters > b.Chapters
	}
	if sequential(a) != sequential(b) {
		return sequential(a)
	}
	if a.AudioCount() != b.AudioCount() {
		return a.AudioCount() > b.AudioCount()
	}
	return a.ID < b.ID
}

// sequential reports whether the segments play in increasing order, which is
// what a genuine main-feature playlist does; decoys shuffle them.
func sequential(t *makemkv.Title) bool {
	for i := 1; i < len(t.Segments); i++ {
		if t.Segments[i] < t.Segments[i-1] {
			return false
		}
	}
	return true
}

func selectMovie(disc *makemkv.Disc, id *metadata.Identity, opts Options, sel *Selection) {
	titles := dedupe(disc.Titles, sel)
	var cands []*makemkv.Title
	for _, t := range titles {
		if t.Duration < opts.MinMovieDuration {
			sel.Skipped = append(sel.Skipped, Skipped{TitleID: t.ID, Duration: t.Duration, Reason: "shorter than min_movie_duration"})
			continue
		}
		cands = append(cands, t)
	}
	if len(cands) == 0 {
		sel.Notes = append(sel.Notes, "no title long enough to be the feature")
		return
	}
	var pick *makemkv.Title
	reason := ""
	if id.Runtime > 0 {
		// Closest to the known runtime wins, ties broken by chapters/sequence.
		var within []*makemkv.Title
		for _, t := range cands {
			if absDur(t.Duration-id.Runtime) <= opts.MovieRuntimeTolerance {
				within = append(within, t)
			}
		}
		if len(within) > 0 {
			sort.SliceStable(within, func(i, j int) bool {
				di := absDur(within[i].Duration - id.Runtime)
				dj := absDur(within[j].Duration - id.Runtime)
				// Treat near-identical durations as ties so chapters decide.
				if absDur(di-dj) > 30*time.Second {
					return di < dj
				}
				return better(within[i], within[j])
			})
			pick = within[0]
			reason = fmt.Sprintf("runtime %s matches %s", fmtDur(pick.Duration), fmtDur(id.Runtime))
		} else {
			sel.Notes = append(sel.Notes, fmt.Sprintf("no title within %s of runtime %s; using longest", fmtDur(opts.MovieRuntimeTolerance), fmtDur(id.Runtime)))
		}
	}
	if pick == nil {
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].Duration != cands[j].Duration {
				return cands[i].Duration > cands[j].Duration
			}
			return better(cands[i], cands[j])
		})
		pick = cands[0]
		reason = "longest title"
	}
	for _, t := range cands {
		if t != pick {
			sel.Skipped = append(sel.Skipped, Skipped{TitleID: t.ID, Duration: t.Duration, Reason: "not the main feature"})
		}
	}
	sel.Picks = append(sel.Picks, Pick{Title: pick, Reason: reason})
}

func selectTV(disc *makemkv.Disc, id *metadata.Identity, opts Options, sel *Selection) {
	titles := dedupe(disc.Titles, sel)
	runtime := id.EpisodeRuntime
	if runtime == 0 {
		runtime = guessEpisodeRuntime(titles)
		sel.Notes = append(sel.Notes, fmt.Sprintf("episode runtime unknown; guessed %s from disc", fmtDur(runtime)))
	}
	if runtime == 0 {
		sel.Notes = append(sel.Notes, "could not determine an episode length")
		return
	}
	lo := time.Duration(float64(runtime) * (1 - opts.TVEpisodeTolerance))
	hi := time.Duration(float64(runtime) * (1 + opts.TVEpisodeTolerance))
	if lo < opts.MinEpisodeDuration {
		lo = opts.MinEpisodeDuration
	}
	dlo := time.Duration(float64(runtime) * (2 - opts.TVEpisodeTolerance))
	dhi := time.Duration(float64(runtime) * (2 + opts.TVEpisodeTolerance))

	var cands []cand
	var totalEp time.Duration
	for _, t := range titles {
		switch {
		case t.Duration >= lo && t.Duration <= hi:
			cands = append(cands, cand{t: t})
			totalEp += t.Duration
		case opts.AllowDoubleEpisodes && t.Duration >= dlo && t.Duration <= dhi:
			cands = append(cands, cand{t: t, double: true})
			totalEp += t.Duration
		default:
			sel.Skipped = append(sel.Skipped, Skipped{TitleID: t.ID, Duration: t.Duration, Reason: fmt.Sprintf("outside episode window %s–%s", fmtDur(lo), fmtDur(hi))})
		}
	}
	// A "play all" title spans several episodes; drop anything whose segments
	// are a superset of another candidate's or that is roughly the sum of
	// the others.
	filtered := cands[:0]
	for i, c := range cands {
		if isPlayAll(c.t, cands, i, totalEp) {
			sel.Skipped = append(sel.Skipped, Skipped{TitleID: c.t.ID, Duration: c.t.Duration, Reason: "play-all title"})
			continue
		}
		filtered = append(filtered, c)
	}
	cands = filtered
	if len(cands) == 0 {
		sel.Notes = append(sel.Notes, "no titles match the episode length")
		return
	}
	// Episodes play in disc order. MakeMKV lists titles roughly by playlist
	// number; use the source file name when present for a stable order.
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i].t, cands[j].t
		if a.SourceFile != "" && b.SourceFile != "" && a.SourceFile != b.SourceFile {
			return a.SourceFile < b.SourceFile
		}
		if a.OrderWeight != b.OrderWeight && a.OrderWeight != 0 && b.OrderWeight != 0 {
			return a.OrderWeight < b.OrderWeight
		}
		return a.ID < b.ID
	})

	ep := opts.NextEpisode
	for _, c := range cands {
		p := Pick{Title: c.t, Season: id.Season, Episode: ep, Reason: fmt.Sprintf("%s fits episode length %s", fmtDur(c.t.Duration), fmtDur(runtime))}
		if c.double {
			p.EpisodeEnd = ep + 1
			p.Reason = fmt.Sprintf("%s fits two episodes of %s", fmtDur(c.t.Duration), fmtDur(runtime))
		}
		if e := episodeTitle(id, ep); e != "" {
			p.EpisodeTitle = e
			if c.double {
				if e2 := episodeTitle(id, ep+1); e2 != "" {
					p.EpisodeTitle = e + " + " + e2
				}
			}
		}
		if c.double {
			ep += 2
		} else {
			ep++
		}
		sel.Picks = append(sel.Picks, p)
	}
	sel.NextEpisode = ep
	if n := len(id.Episodes); n > 0 && ep-1 > n {
		sel.Notes = append(sel.Notes, fmt.Sprintf("assigned episodes beyond the %d listed for season %d; check numbering", n, id.Season))
	}
}

func episodeTitle(id *metadata.Identity, n int) string {
	for _, e := range id.Episodes {
		if e.Number == n {
			return e.Title
		}
	}
	return ""
}

type cand struct {
	t      *makemkv.Title
	double bool
}

func isPlayAll(t *makemkv.Title, cands []cand, idx int, total time.Duration) bool {
	if len(cands) < 3 {
		return false
	}
	// Segment superset check.
	if len(t.Segments) > 1 {
		mine := map[int]bool{}
		for _, s := range t.Segments {
			mine[s] = true
		}
		covered := 0
		for i, c := range cands {
			if i == idx || len(c.t.Segments) == 0 {
				continue
			}
			all := true
			for _, s := range c.t.Segments {
				if !mine[s] {
					all = false
					break
				}
			}
			if all {
				covered++
			}
		}
		if covered >= 2 {
			return true
		}
	}
	// Duration check: this title alone is more than 60% of all candidate time.
	return t.Duration*10 > total*6
}

func guessEpisodeRuntime(titles []*makemkv.Title) time.Duration {
	// Median of titles between 15 and 75 minutes, which covers sitcoms through
	// hour-long dramas without picking up a play-all.
	var durs []time.Duration
	for _, t := range titles {
		if t.Duration >= 15*time.Minute && t.Duration <= 75*time.Minute {
			durs = append(durs, t.Duration)
		}
	}
	if len(durs) == 0 {
		return 0
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	return durs[len(durs)/2]
}

func selectUnidentified(disc *makemkv.Disc, opts Options, sel *Selection) {
	titles := dedupe(disc.Titles, sel)
	switch opts.UnidentifiedStrategy {
	case "skip":
		sel.Notes = append(sel.Notes, "disc not identified; skipped by configuration")
		return
	case "all":
		for _, t := range titles {
			sel.Picks = append(sel.Picks, Pick{Title: t, Reason: "unidentified: ripping every title"})
		}
		return
	}
	// Default: the longest title, unless the disc looks like a TV set.
	sort.SliceStable(titles, func(i, j int) bool {
		if titles[i].Duration != titles[j].Duration {
			return titles[i].Duration > titles[j].Duration
		}
		return better(titles[i], titles[j])
	})
	if looksLikeTV(titles) {
		rt := guessEpisodeRuntime(titles)
		fake := &metadata.Identity{Kind: metadata.KindTV, EpisodeRuntime: rt, Season: 1}
		selectTV(disc, fake, opts, sel)
		sel.Notes = append(sel.Notes, "disc not identified; treated as a TV set")
		return
	}
	sel.Picks = append(sel.Picks, Pick{Title: titles[0], Reason: "unidentified: longest title"})
	for _, t := range titles[1:] {
		sel.Skipped = append(sel.Skipped, Skipped{TitleID: t.ID, Duration: t.Duration, Reason: "not the longest title"})
	}
}

// looksLikeTV is true when several titles share an episode-like length.
func looksLikeTV(titles []*makemkv.Title) bool {
	rt := guessEpisodeRuntime(titles)
	if rt == 0 {
		return false
	}
	n := 0
	for _, t := range titles {
		if absDur(t.Duration-rt) <= rt/4 {
			n++
		}
	}
	return n >= 3
}

// DiscHints summarises the disc for the metadata provider.
func DiscHints(disc *makemkv.Disc) metadata.DiscHints {
	var h metadata.DiscHints
	if disc == nil {
		return h
	}
	for _, t := range disc.Titles {
		if t.Duration > h.LongestTitle {
			h.LongestTitle = t.Duration
		}
	}
	rt := guessEpisodeRuntime(disc.Titles)
	h.MedianDuration = rt
	if rt > 0 {
		for _, t := range disc.Titles {
			if absDur(t.Duration-rt) <= rt/4 {
				h.SimilarTitles++
			}
		}
	}
	return h
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	s := (d % time.Minute) / time.Second
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}
