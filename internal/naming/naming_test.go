package naming

import (
	"testing"
	"time"
)

func TestRender(t *testing.T) {
	v := Vars{Title: "Mission: Impossible", Year: 1996}
	got, err := Render("{title} ({year})/{title} ({year}).mkv", v)
	if err != nil || got != "Mission - Impossible (1996)/Mission - Impossible (1996).mkv" {
		t.Fatalf("got %q %v", got, err)
	}
	got, _ = Render("{title} ({year})/{title} ({year}).mkv", Vars{Title: "No Year"})
	if got != "No Year/No Year.mkv" {
		t.Fatalf("no year: %q", got)
	}
	tv := Vars{Series: "The Office", Year: 2005, Season: 2, Episode: 3, EpisodeTitle: "Office Olympics"}
	got, _ = Render("{series} ({year})/Season {season:02}/{series} - S{season:02}E{episode:02} - {episode_title}.mkv", tv)
	if got != "The Office (2005)/Season 02/The Office - S02E03 - Office Olympics.mkv" {
		t.Fatalf("tv: %q", got)
	}
	tv.EpisodeTitle = ""
	tv.EpisodeEnd = 4
	got, _ = Render("{series}/S{season:02}E{episode:02} - {episode_title}.mkv", tv)
	if got != "The Office/S02E03-E04.mkv" {
		t.Fatalf("double: %q", got)
	}
	got, _ = Render("{label} {date}/{label} - t{title_id:02}.mkv", Vars{Label: "BD_ROM", TitleID: 3, Date: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)})
	if got != "BD_ROM 2026-01-02/BD_ROM - t03.mkv" {
		t.Fatalf("label: %q", got)
	}
	if _, err := Render("{episode_title}.mkv", Vars{}); err == nil {
		t.Fatal("expected error for empty file name")
	}
}

func TestSanitize(t *testing.T) {
	if s := Sanitize(`a/b\c:d*e?f"g<h>i|j`); s != "a-bc-defghij" {
		t.Fatalf("got %q", s)
	}
	if s := Sanitize("  ..trailing.. "); s != "trailing" {
		t.Fatalf("got %q", s)
	}
}
