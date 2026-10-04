package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := s.NextEpisode("tmdb:1", 1); n != 1 {
		t.Fatalf("fresh next = %d", n)
	}
	if err := s.SetNextEpisode("tmdb:1", 1, 5, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDisc(DiscRecord{Fingerprint: "abc", Label: "X", Title: "X (2000)"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendHistory(map[string]string{"id": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendHistory(map[string]string{"id": "2"}); err != nil {
		t.Fatal(err)
	}

	// Reopen: everything persisted.
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := s2.NextEpisode("TMDB:1", 1); n != 5 {
		t.Fatalf("next after reopen = %d", n)
	}
	if r, ok := s2.Disc("abc"); !ok || r.Title != "X (2000)" || r.RippedAt.IsZero() {
		t.Fatalf("disc = %+v %v", r, ok)
	}
	h, err := s2.History(1)
	if err != nil || len(h) != 1 || string(h[0]) != `{"id":"2"}` {
		t.Fatalf("history = %s %v", h, err)
	}
	if err := s2.ResetSeries("tmdb:1", 0); err != nil {
		t.Fatal(err)
	}
	if n := s2.NextEpisode("tmdb:1", 1); n != 1 {
		t.Fatalf("after reset = %d", n)
	}
	if err := s2.ForgetDisc("abc"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Disc("abc"); ok {
		t.Fatal("disc still known")
	}
}

func TestCorruptFile(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "series.json"), []byte("{not json"), 0o644)
	if _, err := Open(dir); err == nil {
		t.Fatal("expected error for corrupt state")
	}
}

func TestReviewsAndMatchesPersist(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SaveReview("20261003-230000-002", map[string]string{"id": "b"})
	_ = s.SaveReview("20261003-220000-001", map[string]string{"id": "a"})
	_ = s.SetDiscMatch("fp", DiscMatch{Kind: "tv", Title: "The Twilight Zone", Year: 1959, Season: 1, Episodes: map[int]int{3: 16, 4: 17}})
	s2, _ := Open(dir)
	got := s2.Reviews()
	var first map[string]string
	if len(got) == 2 {
		_ = json.Unmarshal(got[0], &first)
	}
	if first["id"] != "a" {
		t.Fatalf("reviews = %s", got)
	}
	if m, ok := s2.DiscMatch("fp"); !ok || m.Episodes[4] != 17 || m.ConfirmedAt.IsZero() {
		t.Fatalf("match = %+v %v", m, ok)
	}
	_ = s2.DeleteReview("20261003-220000-001")
	if _, ok := s2.Review("20261003-220000-001"); ok || len(s2.Reviews()) != 1 {
		t.Fatal("review not deleted")
	}
}
