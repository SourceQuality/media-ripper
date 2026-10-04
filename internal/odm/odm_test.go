package odm

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/udf"
)

// The specification's own example (examples/v1/blu-ray/basic.json) declares
// both identifiers for this inventory.
var specFiles = []File{
	{Path: "AACS/Unit_Key_RO.inf", SizeBytes: 524288},
	{Path: "BDMV/PLAYLIST/00800.mpls", SizeBytes: 298},
	{Path: "BDMV/STREAM/01000.m2ts", SizeBytes: 27027738624},
}

func TestIdentifiersMatchTheSpecExample(t *testing.T) {
	if got := Matrix256(specFiles); got != "3643553d77a85c39ce21a1ed54557f6e91480b90582e80d9829c981a2bb80121" {
		t.Fatalf("matrix256 = %s", got)
	}
	var files []udf.File
	for _, f := range specFiles {
		files = append(files, udf.File{Path: f.Path, Size: f.SizeBytes})
	}
	if got, err := udf.ContentHash(files); err != nil || got != "0633802636ACA37258C27FBDAEB527A6" {
		t.Fatalf("content hash = %s %v", got, err)
	}
	if Matrix256([]File{{Path: "BDMV/STREAM/Été.m2ts", SizeBytes: 1}}) != "" {
		t.Fatal("non-ASCII paths are omitted rather than hashed unnormalized")
	}
}

func TestNew(t *testing.T) {
	m, err := New("v0.6.0", "blu-ray", "TWILIGHT_ZONE_SEASON1_DISC4", "0633802636aca37258c27fbdaeb527a6", specFiles,
		[]Title{{Source: TitleSource{Path: "BDMV/PLAYLIST/00800.mpls"}, DurationSeconds: 1556, SizeBytes: 4583301120, ChapterCount: 5}, {}}, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(m)
	s := string(data)
	for _, want := range []string{`"schemaVersion":1`, `"thediscdb-content-hash","value":"0633802636ACA37258C27FBDAEB527A6"`, `"kind":"matrix256"`, `"source":{"path":"BDMV/PLAYLIST/00800.mpls"}`, `"format":"blu-ray"`} {
		if !strings.Contains(s, want) {
			t.Errorf("manifest lacks %s: %s", want, s)
		}
	}
	if len(m.Disc.Titles) != 1 {
		t.Fatal("titles without a source must be left out (source is required)")
	}
	if _, err := New("v", "blu-ray", "", "", specFiles, nil, time.Now()); err == nil {
		t.Fatal("a manifest needs its content hash")
	}
}

func TestBlurayTitleSource(t *testing.T) {
	if s, ok := BlurayTitleSource("00800.mpls"); !ok || s.Path != "BDMV/PLAYLIST/00800.mpls" {
		t.Fatalf("mpls: %+v", s)
	}
	if s, ok := BlurayTitleSource("00001.m2ts"); !ok || s.Path != "BDMV/STREAM/00001.m2ts" {
		t.Fatalf("m2ts: %+v", s)
	}
	if _, ok := BlurayTitleSource(""); ok {
		t.Fatal("no source")
	}
}
