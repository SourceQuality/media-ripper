package discdb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Titles of The Twilight Zone S1 D1 (2021 Complete Series Blu-ray) as
// TheDiscDB has them, trimmed to the interesting ones.
const twilightDisc1 = `{"Index":1,"Slug":"S01D01","Name":"Season 1 Disc 1","Format":"Blu-Ray","ContentHash":"29715BD1FE3E2DFA8AA1D3E089257333","Titles":[
 {"Index":0,"SourceFile":"00010.mpls","Duration":"0:25:56","Size":4583301120,"Item":{"Type":"Episode","Title":"The Lonely","Season":"1","Episode":"7"}},
 {"Index":3,"SourceFile":"00011.mpls","Duration":"0:34:44","Size":1430212608,"Item":{"Type":"Extra","Title":"Where is Everybody with Pitch Intro","Season":"1","Episode":"1"}},
 {"Index":4,"SourceFile":"00005.mpls","Duration":"0:25:59","Size":4859381760,"Item":{"Type":"Episode","Title":"Escape Clause","Season":"1","Episode":"6"}},
 {"Index":9,"SourceFile":"00000.mpls","Duration":"0:25:55","Size":4888393728,"Item":{"Type":"Episode","Title":"Where Is Everybody?","Season":"1","Episode":"1"}},
 {"Index":8,"SourceFile":"00001.mpls","Duration":"0:25:55","Size":4898856960,"Item":{"Type":"Episode","Title":"One for the Angels","Season":1,"Episode":2}},
 {"Index":12,"SourceFile":"00013.mpls","Duration":"0:02:08","Size":80842752,"Item":null}]}`

const twilightDisc2 = `{"Index":2,"Slug":"S01D02","Name":"Season 1 Disc 2","Format":"Blu-Ray","Titles":[
 {"Index":0,"SourceFile":"00000.mpls","Duration":"0:25:30","Size":4700000000,"Item":{"Type":"Episode","Title":"Mr. Bevis","Season":"1","Episode":"33"}},
 {"Index":1,"SourceFile":"00001.mpls","Duration":"0:25:30","Size":4711111111,"Item":{"Type":"Episode","Title":"The After Hours","Season":"1","Episode":"34"}}]}`

type fakeGitHub struct {
	srv        *httptest.Server
	treeCalls  atomic.Int32
	rawCalls   atomic.Int32
	failAPI    atomic.Bool
	discByPath map[string]string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{discByPath: map[string]string{
		"data/series/The Twilight Zone (1959)/the-complete-series-blu-ray-2021/disc01.json": twilightDisc1,
		"data/series/The Twilight Zone (1959)/the-complete-series-blu-ray-2021/disc02.json": twilightDisc2,
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/TheDiscDb/data/git/trees/HEAD"):
			f.treeCalls.Add(1)
			if f.failAPI.Load() {
				w.WriteHeader(http.StatusForbidden) // rate limited
				return
			}
			tree := []treeEntry{
				{Path: "data/series", Type: "tree"},
				{Path: "data/series/The Twilight Zone (1959)", Type: "tree"},
				{Path: "data/series/The Twilight Zone (1959)/the-complete-series-blu-ray-2021", Type: "tree"},
				{Path: "data/series/The Twilight Zone (1959)/the-complete-series-blu-ray-2021/release.json", Type: "blob", SHA: "r"},
				{Path: "data/series/The Twilight Zone (1959)/the-complete-series-blu-ray-2021/disc01.json", Type: "blob", SHA: "aaa1"},
				{Path: "data/series/The Twilight Zone (1959)/the-complete-series-blu-ray-2021/disc02.json", Type: "blob", SHA: "aaa2"},
				{Path: "data/series/The Twilight Zone (2002)", Type: "tree"},
				{Path: "data/movie/The Thing (1982)", Type: "tree"},
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tree": tree, "truncated": false})
		case strings.HasPrefix(r.URL.Path, "/TheDiscDb/data/HEAD/"):
			f.rawCalls.Add(1)
			p := strings.TrimPrefix(r.URL.Path, "/TheDiscDb/data/HEAD/")
			if body, ok := f.discByPath[p]; ok {
				_, _ = w.Write([]byte(body))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) client(dir string) *Client {
	c := New("", dir)
	c.apiBase, c.rawBase = f.srv.URL, f.srv.URL
	return c
}

// The scan of the real disc: seven episodes and the SD extra, with
// MakeMKV's exact sizes (they equal TheDiscDB's byte for byte).
var twilightScan = []ScanTitle{
	{ID: 0, SourceFile: "00010.mpls", Size: 4583301120},
	{ID: 1, SourceFile: "00011.mpls", Size: 1430212608},
	{ID: 2, SourceFile: "00005.mpls", Size: 4859381760},
	{ID: 7, SourceFile: "00000.mpls", Size: 4888393728},
	{ID: 6, SourceFile: "00001.mpls", Size: 4898856960},
}

func TestFindTwilightZoneDisc1(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(t.TempDir())
	m, err := c.Find(context.Background(), Series, "The Twilight Zone", 1959, twilightScan)
	if err != nil || m == nil {
		t.Fatalf("find: %v %+v", err, m)
	}
	if m.Disc.Slug != "S01D01" || m.Release != "the-complete-series-blu-ray-2021" || m.Matched != 5 {
		t.Fatalf("match = %s %s %d/%d", m.Disc.Slug, m.Release, m.Matched, m.Compared)
	}
	if it := m.ByID[1].Item; it == nil || it.Type != "Extra" {
		t.Fatalf("title 1 = %+v, want the extra", m.ByID[1])
	}
	if it := m.ByID[0].Item; it.Type != "Episode" || it.Season != 1 || it.Episode != 7 || it.Title != "The Lonely" {
		t.Fatalf("title 0 = %+v", it)
	}
	if it := m.ByID[6].Item; it.Season != 1 || it.Episode != 2 { // numbers written as JSON numbers
		t.Fatalf("title 6 = %+v", it)
	}
}

func TestFindCachesAndFallsBack(t *testing.T) {
	f := newFakeGitHub(t)
	dir := t.TempDir()
	c := f.client(dir)
	ctx := context.Background()
	if _, err := c.Find(ctx, Series, "Twilight Zone", 1959, twilightScan); err != nil {
		t.Fatal(err)
	}
	trees, raws := f.treeCalls.Load(), f.rawCalls.Load()
	if _, err := c.Find(ctx, Series, "The Twilight Zone", 1959, twilightScan); err != nil {
		t.Fatal(err)
	}
	if f.treeCalls.Load() != trees || f.rawCalls.Load() != raws {
		t.Fatalf("second lookup refetched: tree %d→%d raw %d→%d", trees, f.treeCalls.Load(), raws, f.rawCalls.Load())
	}
	// An expired listing that GitHub refuses to refresh is still used.
	c.TreeTTL = time.Nanosecond
	f.failAPI.Store(true)
	if m, err := c.Find(ctx, Series, "The Twilight Zone", 1959, twilightScan); err != nil || m == nil {
		t.Fatalf("stale fallback: %v %v", err, m)
	}
}

func TestFindNoMatch(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(t.TempDir())
	ctx := context.Background()
	cases := []struct {
		name  string
		title string
		year  int
		scan  []ScanTitle
	}{
		{"uncatalogued title", "Friends", 1994, twilightScan},
		{"other series of the same name", "The Twilight Zone", 2002, twilightScan},
		{"different disc of a catalogued title", "The Twilight Zone", 1959, []ScanTitle{{SourceFile: "00000.mpls", Size: 123}, {SourceFile: "00001.mpls", Size: 456}}},
		{"one coincidental size is not enough", "The Twilight Zone", 1959, []ScanTitle{{SourceFile: "00000.mpls", Size: 4888393728}, {Size: 1}, {Size: 2}}},
	}
	for _, tc := range cases {
		m, err := c.Find(ctx, Series, tc.title, tc.year, tc.scan)
		if err != nil || m != nil {
			t.Errorf("%s: got %+v, %v", tc.name, m, err)
		}
	}
}

func TestFolderMatches(t *testing.T) {
	cases := []struct {
		folder, title string
		year          int
		want          bool
	}{
		{"The Twilight Zone (1959)", "The Twilight Zone", 1959, true},
		{"The Twilight Zone (1959)", "Twilight Zone", 1959, true},
		{"The Twilight Zone (1959)", "The Twilight Zone", 1960, true},
		{"The Twilight Zone (1959)", "The Twilight Zone", 2002, false},
		{"The Twilight Zone (1959)", "The Twilight Zone", 0, true},
		{"'Round Midnight (1986)", "Round Midnight", 1986, true},
		{"Friends (1994)", "Friends with Benefits", 2011, false},
	}
	for _, c := range cases {
		if got := folderMatches(c.folder, c.title, c.year); got != c.want {
			t.Errorf("folderMatches(%q, %q, %d) = %v", c.folder, c.title, c.year, got)
		}
	}
}

func TestFindByHash(t *testing.T) {
	var gotHash string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotHash = req.Variables["hash"]
		if !strings.Contains(req.Query, "contentHash: { eq: $hash }") {
			t.Errorf("query = %s", req.Query)
		}
		if gotHash != "29715BD1FE3E2DFA8AA1D3E089257333" {
			_, _ = w.Write([]byte(`{"data":{"mediaItems":{"nodes":[]}}}`))
			return
		}
		// Shaped like TheDiscDB's schema: Long sizes, String season/episode/tmdb.
		_, _ = w.Write([]byte(`{"data":{"mediaItems":{"nodes":[{"title":"The Twilight Zone","year":1959,"type":"Series","externalids":{"tmdb":"6357"},
		 "releases":[{"slug":"the-complete-series-blu-ray-2021","discs":[
		  {"index":2,"name":"Season 1 Disc 2","slug":"S01D02","contentHash":"OTHER","titles":[]},
		  {"index":1,"name":"Season 1 Disc 1","slug":"S01D01","contentHash":"29715BD1FE3E2DFA8AA1D3E089257333","titles":[
		   {"index":0,"sourceFile":"00010.mpls","size":4583301120,"item":{"title":"The Lonely","type":"Episode","season":"1","episode":"7"}},
		   {"index":3,"sourceFile":"00011.mpls","size":1430212608,"item":{"title":"Where is Everybody with Pitch Intro","type":"Extra","season":"1","episode":"1"}},
		   {"index":9,"sourceFile":"00000.mpls","size":4888393728,"item":{"title":"Where Is Everybody?","type":"Episode","season":"1","episode":"1"}}]}]}]}]}}}`))
	}))
	defer srv.Close()
	c := New("", t.TempDir())
	c.API = srv.URL
	ctx := context.Background()
	m, err := c.FindByHash(ctx, "29715BD1FE3E2DFA8AA1D3E089257333", twilightScan)
	if err != nil || m == nil {
		t.Fatalf("find: %v %+v", err, m)
	}
	if m.Kind != Series || m.Title != "The Twilight Zone" || m.Year != 1959 || m.TMDBID != 6357 || m.Disc.Name != "Season 1 Disc 1" || m.Release != "the-complete-series-blu-ray-2021" {
		t.Fatalf("match = %+v", m)
	}
	if it := m.ByID[0].Item; it == nil || it.Episode != 7 || it.Title != "The Lonely" {
		t.Fatalf("title 0 = %+v", m.ByID[0])
	}
	if it := m.ByID[1].Item; it == nil || it.Type != "Extra" {
		t.Fatalf("title 1 = %+v", m.ByID[1])
	}
	if m, err := c.FindByHash(ctx, "UNKNOWN", twilightScan); err != nil || m != nil {
		t.Fatalf("uncatalogued: %+v %v", m, err)
	}
}
