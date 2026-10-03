package store

import (
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
