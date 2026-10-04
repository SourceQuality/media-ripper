package pipeline

import (
	"strings"
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/selector"
)

func TestProgressLines(t *testing.T) {
	pick := func(id, ep int, name string) selector.Pick {
		return selector.Pick{Title: &makemkv.Title{ID: id}, Season: 1, Episode: ep, EpisodeTitle: name}
	}
	s := Job{Stage: StageRipping, Current: 3,
		Identity:  &metadata.Identity{Kind: metadata.KindTV, Title: "The Twilight Zone"},
		Selection: &selector.Selection{Picks: []selector.Pick{pick(6, 16, "The Hitch-Hiker"), pick(5, 17, "The Fever"), pick(4, 18, "The Last Flight"), pick(3, 19, "The Purple Testament")}},
		Outputs:   []Output{{TitleID: 6, Import: ""}},
		Activities: []Activity{
			{Kind: "copy", Item: "S01E17", Percent: 66, Speed: 17.4e6, ETASeconds: 97},
			{Kind: "rip", Item: "S01E18", Percent: 40, Speed: 20.1e6, ETASeconds: 180},
		},
	}
	lines, summary := progressLines(s)
	want := []string{
		"✅ S01E16 The Hitch-Hiker · delivered",
		"▶️ S01E17 The Fever · copying 66% · 17.4 MB/s · 1m37s left",
		"▶️ S01E18 The Last Flight · ripping 40% · 20.1 MB/s · 3m0s left",
		"⏳ S01E19 The Purple Testament",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines:\n%s", strings.Join(lines, "\n"))
	}
	if summary != "Ripping 3 of 4 · 1 delivered" {
		t.Fatalf("summary = %q", summary)
	}

	// Ripping finished: titles not yet delivered are waiting to copy.
	s.Stage, s.Activities = StageDelivering, nil
	s.Outputs = append(s.Outputs, Output{TitleID: 5, Import: "imported"})
	lines, summary = progressLines(s)
	if lines[1] != "✅ S01E17 The Fever · imported" || lines[2] != "☑️ S01E18 The Last Flight · ripped, waiting to copy" {
		t.Fatalf("after the rip:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(summary, "Disc ripped, tray open") {
		t.Fatalf("summary = %q", summary)
	}
	s.Stage, s.Elapsed = StageDone, (40 * time.Minute).String()
	if _, summary = progressLines(s); summary != "Done · 2 of 4 delivered in 40m0s" {
		t.Fatalf("done summary = %q", summary)
	}
}
