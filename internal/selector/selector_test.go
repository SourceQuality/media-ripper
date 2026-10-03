package selector

import (
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
)

func title(id int, d time.Duration, chapters int, segs []int, src string) *makemkv.Title {
	return &makemkv.Title{ID: id, Duration: d, Chapters: chapters, Segments: segs, SegmentsMap: "x", SourceFile: src,
		Streams: []makemkv.Stream{{Type: "Video", VideoSize: "1920x1080"}, {Type: "Audio", Lang: "eng"}}}
}

func TestMovieWithDecoys(t *testing.T) {
	disc := &makemkv.Disc{Type: "Blu-ray disc", Titles: []*makemkv.Title{
		title(0, 136*time.Minute, 24, []int{1, 2, 3, 4}, "00800.mpls"),
		title(1, 136*time.Minute, 2, []int{4, 1, 3, 2}, "00801.mpls"),      // shuffled decoy
		title(2, 136*time.Minute, 24, []int{1, 2, 3, 4}, "00802.mpls"),     // exact duplicate
		title(3, 2*time.Minute, 1, []int{50}, "00002.mpls"),                // trailer
		title(4, 25*time.Minute, 3, []int{60, 61}, "00010.mpls"),           // making-of
		title(5, 150*time.Minute, 30, []int{1, 2, 3, 4, 70}, "00803.mpls"), // extended cut
	}}
	id := &metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Runtime: 136 * time.Minute}
	sel, err := Select(disc, id, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Picks) != 1 || sel.Picks[0].Title.ID != 0 {
		t.Fatalf("picks = %+v", sel.Picks)
	}
	// Without a known runtime the longest wins (the extended cut).
	sel, _ = Select(disc, &metadata.Identity{Kind: metadata.KindMovie, Title: "x"}, Options{})
	if sel.Picks[0].Title.ID != 5 {
		t.Fatalf("longest = %d", sel.Picks[0].Title.ID)
	}
}

func TestMovieRuntimeOutsideTolerance(t *testing.T) {
	disc := &makemkv.Disc{Titles: []*makemkv.Title{
		title(0, 90*time.Minute, 10, []int{1}, ""),
		title(1, 95*time.Minute, 12, []int{2}, ""),
	}}
	id := &metadata.Identity{Kind: metadata.KindMovie, Title: "x", Runtime: 120 * time.Minute}
	sel, _ := Select(disc, id, Options{})
	if sel.Picks[0].Title.ID != 1 || len(sel.Notes) == 0 {
		t.Fatalf("expected longest with a note: %+v", sel)
	}
}

func TestTVDisc(t *testing.T) {
	ep := 44 * time.Minute
	disc := &makemkv.Disc{Type: "DVD disc", Titles: []*makemkv.Title{
		title(0, 4*ep+2*time.Minute, 20, []int{1, 2, 3, 4}, "VTS_01"), // play all
		title(1, ep, 5, []int{1}, "VTS_02"),
		title(2, ep+time.Minute, 5, []int{2}, "VTS_03"),
		title(3, ep-2*time.Minute, 5, []int{3}, "VTS_04"),
		title(4, ep+30*time.Second, 5, []int{4}, "VTS_05"),
		title(5, 3*time.Minute, 1, []int{9}, "VTS_06"),   // recap
		title(6, 22*time.Minute, 2, []int{10}, "VTS_07"), // featurette
	}}
	id := &metadata.Identity{Kind: metadata.KindTV, Title: "Show", Season: 2, EpisodeRuntime: ep,
		Episodes: []metadata.Episode{{Number: 5, Title: "Five"}, {Number: 6, Title: "Six"}, {Number: 7, Title: "Seven"}, {Number: 8, Title: "Eight"}}}
	sel, err := Select(disc, id, Options{NextEpisode: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Picks) != 4 {
		t.Fatalf("picks = %d: %+v", len(sel.Picks), sel.Picks)
	}
	for i, p := range sel.Picks {
		if p.Episode != 5+i || p.Season != 2 || p.Title.ID != 1+i {
			t.Fatalf("pick %d = %+v", i, p)
		}
	}
	if sel.Picks[0].EpisodeTitle != "Five" || sel.Picks[3].EpisodeTitle != "Eight" {
		t.Fatalf("episode titles: %+v", sel.Picks)
	}
	if sel.NextEpisode != 9 {
		t.Fatalf("next = %d", sel.NextEpisode)
	}
	playAllSkipped := false
	for _, s := range sel.Skipped {
		if s.TitleID == 0 {
			playAllSkipped = true
		}
	}
	if !playAllSkipped {
		t.Fatalf("play-all not skipped: %+v", sel.Skipped)
	}
}

func TestTVPlayAllWithinDoubleWindow(t *testing.T) {
	ep := 44 * time.Minute
	disc := &makemkv.Disc{Titles: []*makemkv.Title{
		title(0, 2*ep, 10, []int{1, 2}, "VTS_01"), // play-all of two episodes
		title(1, ep, 5, []int{1}, "VTS_02"),
		title(2, ep, 5, []int{2}, "VTS_03"),
	}}
	id := &metadata.Identity{Kind: metadata.KindTV, Title: "Show", Season: 1, EpisodeRuntime: ep}
	sel, err := Select(disc, id, Options{AllowDoubleEpisodes: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Picks) != 2 || sel.Picks[0].Title.ID != 1 || sel.Picks[1].Title.ID != 2 {
		t.Fatalf("picks = %+v", sel.Picks)
	}
}

func TestTVDoubleEpisode(t *testing.T) {
	ep := 22 * time.Minute
	disc := &makemkv.Disc{Titles: []*makemkv.Title{
		title(1, ep, 4, []int{1}, "a"),
		title(2, 2*ep+time.Minute, 8, []int{2}, "b"),
		title(3, ep, 4, []int{3}, "c"),
	}}
	id := &metadata.Identity{Kind: metadata.KindTV, Title: "Sitcom", Season: 1, EpisodeRuntime: ep}
	sel, err := Select(disc, id, Options{AllowDoubleEpisodes: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Picks) != 3 || sel.Picks[1].Episode != 2 || sel.Picks[1].EpisodeEnd != 3 || sel.Picks[2].Episode != 4 {
		t.Fatalf("picks = %+v", sel.Picks)
	}
}

func TestUnidentified(t *testing.T) {
	disc := &makemkv.Disc{Titles: []*makemkv.Title{
		title(0, 100*time.Minute, 12, []int{1}, ""),
		title(1, 3*time.Minute, 1, []int{2}, ""),
	}}
	sel, err := Select(disc, nil, Options{})
	if err != nil || len(sel.Picks) != 1 || sel.Picks[0].Title.ID != 0 {
		t.Fatalf("longest: %+v %v", sel, err)
	}
	if _, err := Select(disc, nil, Options{UnidentifiedStrategy: "skip"}); err != ErrNothingToRip {
		t.Fatalf("skip: %v", err)
	}
	sel, _ = Select(disc, nil, Options{UnidentifiedStrategy: "all"})
	if len(sel.Picks) != 2 {
		t.Fatalf("all: %d", len(sel.Picks))
	}
	// Looks like TV: four similar-length titles.
	tv := &makemkv.Disc{Titles: []*makemkv.Title{
		title(0, 43*time.Minute, 5, []int{1}, ""), title(1, 44*time.Minute, 5, []int{2}, ""),
		title(2, 42*time.Minute, 5, []int{3}, ""), title(3, 45*time.Minute, 5, []int{4}, ""),
	}}
	sel, _ = Select(tv, nil, Options{})
	if len(sel.Picks) != 4 || sel.Picks[0].Episode != 1 {
		t.Fatalf("unidentified tv: %+v", sel.Picks)
	}
}
