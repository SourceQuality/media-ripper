package metadata

import "testing"

func TestParseLabel(t *testing.T) {
	cases := []struct {
		in                 string
		query              string
		year, season, disc int
		looksTV, junk      bool
	}{
		{"THE_MATRIX", "The Matrix", 0, 0, 0, false, false},
		{"THE_MATRIX_1999", "The Matrix", 1999, 0, 0, false, false},
		{"BIG_BUCK_BUNNY_2008_BD", "Big Buck Bunny", 2008, 0, 0, false, false},
		{"FRIENDS_S3_D2", "Friends", 0, 3, 2, true, false},
		{"FRIENDS_SEASON_3_DISC_2", "Friends", 0, 3, 2, true, false},
		{"BAND_OF_BROTHERS_DISC_1", "Band of Brothers", 0, 0, 1, false, false},
		{"THE_OFFICE_SEASON_2", "The Office", 0, 2, 0, true, false},
		{"BREAKING_BAD_S01_DISC1", "Breaking Bad", 0, 1, 1, true, false},
		{"LORD_OF_THE_RINGS_EXTENDED_EDITION_DISC_1", "Lord of the Rings", 0, 0, 1, false, false},
		{"BD_ROM", "", 0, 0, 0, false, true},
		{"LOGICAL_VOLUME_ID", "", 0, 0, 0, false, true},
		{"", "", 0, 0, 0, false, true},
		{"2001", "2001", 0, 0, 0, false, false},
		{"2001_A_SPACE_ODYSSEY", "2001 a Space Odyssey", 0, 0, 0, false, false},
		{"BLADE_RUNNER_2049", "Blade Runner 2049", 0, 0, 0, false, false},
		{"WB_INCEPTION", "Inception", 0, 0, 0, false, false},
		{"Star Wars Episode IV 1977", "Star Wars Episode Iv", 1977, 0, 0, false, false},
		{"MASH_S2D1", "Mash", 0, 2, 1, true, false},
	}
	for _, c := range cases {
		h := ParseLabel(c.in)
		if h.Query != c.query || h.Year != c.year || h.Season != c.season || h.Disc != c.disc || h.LooksTV != c.looksTV || h.Junk != c.junk {
			t.Errorf("%q: got %+v", c.in, h)
		}
	}
}

func TestSimilarity(t *testing.T) {
	if s := Similarity("The Matrix", "The Matrix"); s != 1 {
		t.Fatalf("identical = %v", s)
	}
	if a, b := Similarity("Matrix", "The Matrix"), Similarity("Matrix", "The Matrix Reloaded"); a <= b {
		t.Fatalf("exact (%v) should beat superset (%v)", a, b)
	}
	if s := Similarity("Friends", "Seinfeld"); s != 0 {
		t.Fatalf("unrelated = %v", s)
	}
}
