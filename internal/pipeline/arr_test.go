package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/arr"
	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/selector"
)

// fakeRadarr "imports" by copying the files it is given into its own root
// folder (media-ripper always asks for a copy) and lists them as its
// movie files.
func fakeRadarr(t *testing.T, root string, pathPrefix string) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var calls []string
	inLibrary := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/lookup":
			if r.URL.Query().Get("term") != "tmdb:603" {
				t.Errorf("lookup term = %q", r.URL.Query().Get("term"))
			}
			_, _ = w.Write([]byte(`[{"title":"The Matrix","year":1999,"tmdbId":603,"runtime":136}]`))
		case r.URL.Path == "/api/v3/movie" && r.Method == http.MethodGet:
			if inLibrary {
				_, _ = w.Write([]byte(`[{"id":1,"tmdbId":603,"title":"The Matrix","path":"` + root + `/The Matrix (1999)"}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case r.URL.Path == "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[{"id":1,"name":"Any"}]`))
		case r.URL.Path == "/api/v3/movie" && r.Method == http.MethodPost:
			inLibrary = true
			_, _ = w.Write([]byte(`{"id":1,"tmdbId":603,"title":"The Matrix","path":"` + root + `/The Matrix (1999)"}`))
		case r.URL.Path == "/api/v3/manualimport":
			p := r.URL.Query().Get("folder")
			if !strings.HasPrefix(p, pathPrefix) {
				t.Errorf("import path %q not under %q", p, pathPrefix)
			}
			local := strings.Replace(p, pathPrefix, filepath.Dir(root), 1)
			entries, _ := os.ReadDir(local)
			var items []map[string]any
			for _, e := range entries {
				items = append(items, map[string]any{"path": p + "/" + e.Name(), "movie": map[string]any{"id": 1}, "quality": map[string]any{}, "rejections": []any{}})
			}
			_ = json.NewEncoder(w).Encode(items)
		case r.URL.Path == "/api/v3/qualitydefinition":
			_, _ = w.Write([]byte(`[{"quality":{"id":30,"name":"Remux-1080p"}}]`))
		case r.URL.Path == "/api/v3/moviefile":
			_ = json.NewEncoder(w).Encode(heldFiles(filepath.Join(root, "The Matrix (1999)")))
		case r.URL.Path == "/api/v3/command" && r.Method == http.MethodPost:
			var body struct {
				Name       string `json:"name"`
				ImportMode string `json:"importMode"`
				Files      []struct {
					Path    string         `json:"path"`
					Quality map[string]any `json:"quality"`
				} `json:"files"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Name != "ManualImport" || body.ImportMode != "copy" {
				t.Errorf("command %q mode %q, want a ManualImport copy", body.Name, body.ImportMode)
			}
			_ = os.MkdirAll(filepath.Join(root, "The Matrix (1999)"), 0o755)
			for _, f := range body.Files {
				if q, _ := f.Quality["quality"].(map[string]any); q["name"] != "Remux-1080p" {
					t.Errorf("quality sent = %v, want Remux-1080p", f.Quality)
				}
				local := strings.Replace(f.Path, pathPrefix, filepath.Dir(root), 1)
				copyFile(t, local, filepath.Join(root, "The Matrix (1999)", filepath.Base(local)))
			}
			_, _ = w.Write([]byte(`{"id":9,"status":"completed"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	return srv, &calls
}

// heldFiles is an app's file list: the files in dir with their sizes.
func heldFiles(dir string) []map[string]any {
	out := []map[string]any{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() {
			out = append(out, map[string]any{"path": filepath.Join(dir, e.Name()), "size": info.Size()})
		}
	}
	return out
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Errorf("read %s: %v", src, err)
		return
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Errorf("write %s: %v", dst, err)
	}
}

func TestRadarrHandoffEndToEnd(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{&metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Year: 1999, TMDBID: 603, Runtime: 136 * time.Minute, Confidence: 1, Source: "test"}})
	radarrRoot := filepath.Join(e.out, "radarr-movies")
	// Radarr sees the staging area under a different mount.
	srv, calls := fakeRadarr(t, radarrRoot, "/remote/library")
	defer srv.Close()
	e.cfg.Arr.Radarr = config.ArrApp{Enabled: true, URL: srv.URL, APIKey: "k", RootFolder: radarrRoot, AddMissing: true, ImportMode: "move",
		PathMap: map[string]string{e.out: "/remote/library"}}
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)

	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitDone(t)
	if j.Stage != StageDone || len(j.Warnings) != 0 {
		t.Fatalf("job = %s err=%s warnings=%v log=%v", j.Stage, j.Error, j.Warnings, j.Log)
	}
	staged := filepath.Join(e.out, "_incoming", "The Matrix (1999)", "The Matrix (1999).mkv")
	if j.Outputs[0].Path != staged {
		t.Fatalf("delivered to %s", j.Outputs[0].Path)
	}
	if _, err := os.Stat(filepath.Join(radarrRoot, "The Matrix (1999)", "The Matrix (1999).mkv")); err != nil {
		t.Fatalf("radarr did not receive the file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.out, "_incoming", "The Matrix (1999)")); !os.IsNotExist(err) {
		t.Fatal("staging folder not cleaned after move import")
	}
	if _, err := os.Stat(filepath.Join(e.out, "_incoming")); err != nil {
		t.Fatal("staging root must stay")
	}
	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "POST /api/v3/movie") || !strings.Contains(joined, "POST /api/v3/command") {
		t.Fatalf("calls: %s", joined)
	}
	// Eject happened right after the rip, once.
	if !j.Ejected || e.drv.ejectCount() != 1 {
		t.Fatalf("ejected=%v count=%d", j.Ejected, e.drv.ejectCount())
	}
}

func TestRadarrHandoffFailureKeepsFiles(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{&metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Year: 1999, TMDBID: 603, Runtime: 136 * time.Minute, Confidence: 1, Source: "test"}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	e.cfg.Arr.Radarr = config.ArrApp{Enabled: true, URL: srv.URL, APIKey: "k", RootFolder: "/x", AddMissing: true, ImportMode: "move"}
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitDone(t)
	if j.Stage != StageDone || len(j.Warnings) != 1 || !strings.Contains(j.Warnings[0], "radarr import failed") {
		t.Fatalf("job = %s warnings=%v", j.Stage, j.Warnings)
	}
	if _, err := os.Stat(j.Outputs[0].Path); err != nil {
		t.Fatalf("file should remain in staging: %v", err)
	}
}

func TestArrProviderIdentifiesWhenNoTMDB(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/movie/lookup":
			_, _ = w.Write([]byte(`[{"title":"The Matrix","year":1999,"tmdbId":603,"runtime":136}]`))
		case "/api/v3/movie":
			_, _ = w.Write([]byte(`[{"id":1,"tmdbId":603,"title":"The Matrix","path":"/m"}]`))
		case "/api/v3/command":
			_, _ = w.Write([]byte(`{"id":1,"status":"completed"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	e := setup(t, movieInfo, nil) // no provider override: built from config
	e.cfg.Metadata.Provider = "auto"
	e.cfg.Arr.Radarr = config.ArrApp{Enabled: true, URL: srv.URL, APIKey: "k", ImportMode: "copy"}
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitDone(t)
	if j.Stage != StageDone || j.Identity == nil || j.Identity.Source != "radarr" || j.Identity.Runtime != 136*time.Minute {
		t.Fatalf("job = %+v", j.Identity)
	}
	if !strings.Contains(j.Outputs[0].Path, filepath.Join("_incoming", "The Matrix (1999)")) {
		t.Fatalf("path = %s", j.Outputs[0].Path)
	}
}

func TestCommonDirAndCleanup(t *testing.T) {
	if d := commonDir([]Output{{Path: "/a/b/c/x.mkv"}, {Path: "/a/b/c/y.mkv"}}); d != "/a/b/c" {
		t.Fatalf("same dir: %s", d)
	}
	if d := commonDir([]Output{{Path: "/a/b/c/x.mkv"}, {Path: "/a/b/d/y.mkv"}}); d != "/a/b" {
		t.Fatalf("sibling dirs: %s", d)
	}
	root := t.TempDir()
	deep := filepath.Join(root, "_incoming", "Show", "Season 01")
	_ = os.MkdirAll(deep, 0o755)
	removeEmptyUpTo(deep, filepath.Join(root, "_incoming"))
	if _, err := os.Stat(filepath.Join(root, "_incoming", "Show")); !os.IsNotExist(err) {
		t.Fatal("empty parents not removed")
	}
	if _, err := os.Stat(filepath.Join(root, "_incoming")); err != nil {
		t.Fatal("root removed")
	}
}

// fakeSonarr behaves like the Sonarr 4 seen on real hardware: a new
// series' episodes load in the background, the preview rejects files until
// they have, the folder scan "completes" without importing, and
// ManualImport copies exactly the files it is given. sonarrOpts tweak it.
type sonarrOpts struct {
	takeNothing *atomic.Bool  // ManualImport reports success but keeps nothing (as when it could not delete from staging)
	reject      string        // file name the preview rejects as a sample
	moveAfter   time.Duration // the files show up only this long after "completed"
	commands    *atomic.Int32 // counts ManualImport commands
}

func fakeSonarr(t *testing.T, root string, o sonarrOpts) *httptest.Server {
	var mu sync.Mutex
	added, epPolls := false, 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		series := `{"id":7,"tvdbId":79168,"title":"Friends","year":1994,"path":"` + root + `/Friends (1994)"}`
		switch {
		case r.URL.Path == "/api/v3/series/lookup":
			_, _ = w.Write([]byte(`[{"title":"Friends","year":1994,"tvdbId":79168}]`))
		case r.URL.Path == "/api/v3/series" && r.Method == http.MethodGet:
			if added {
				_, _ = w.Write([]byte(`[` + series + `]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case r.URL.Path == "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[{"id":1,"name":"Any"}]`))
		case r.URL.Path == "/api/v3/qualitydefinition":
			_, _ = w.Write([]byte(`[{"quality":{"id":2,"name":"DVD"}},{"quality":{"id":20,"name":"Bluray-1080p Remux"}}]`))
		case r.URL.Path == "/api/v3/series" && r.Method == http.MethodPost:
			added = true
			_, _ = w.Write([]byte(series))
		case r.URL.Path == "/api/v3/episode":
			epPolls++
			if epPolls < 2 { // still refreshing
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":101,"seasonNumber":1,"episodeNumber":1,"title":"Pilot"},{"id":102,"seasonNumber":1,"episodeNumber":2,"title":"Two"}]`))
		case r.URL.Path == "/api/v3/episodefile":
			if r.URL.Query().Get("seriesId") != "7" {
				t.Errorf("episodefile query = %v", r.URL.Query())
			}
			_ = json.NewEncoder(w).Encode(heldFiles(filepath.Join(root, "Friends (1994)")))
		case r.URL.Path == "/api/v3/manualimport":
			var items []map[string]any
			_ = filepath.Walk(r.URL.Query().Get("folder"), func(f string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}
				it := map[string]any{"path": f, "quality": map[string]any{"quality": map[string]any{"name": "HDTV-1080p"}}, "rejections": []any{}}
				switch {
				case epPolls < 2:
					it["rejections"] = []any{map[string]any{"reason": "Unknown episode"}}
				case info.Name() == o.reject:
					it["rejections"] = []any{map[string]any{"reason": "Sample"}}
				default:
					it["series"] = map[string]any{"id": 7}
					it["episodes"] = []any{map[string]any{"id": 101}}
				}
				items = append(items, it)
				return nil
			})
			_ = json.NewEncoder(w).Encode(items)
		case r.URL.Path == "/api/v3/command" && r.Method == http.MethodPost:
			var body struct {
				Name  string `json:"name"`
				Files []struct {
					Path    string         `json:"path"`
					Quality map[string]any `json:"quality"`
				} `json:"files"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Name != "ManualImport" {
				// What the real one did with the folder scan.
				_, _ = w.Write([]byte(`{"id":3,"status":"completed","result":"unsuccessful","message":"Failed to import"}`))
				return
			}
			if o.commands != nil {
				o.commands.Add(1)
			}
			dst := filepath.Join(root, "Friends (1994)")
			_ = os.MkdirAll(dst, 0o755)
			for _, f := range body.Files {
				if q, _ := f.Quality["quality"].(map[string]any); q["name"] != "DVD" { // tvInfo is a DVD
					t.Errorf("quality sent = %v", f.Quality)
				}
				if o.takeNothing == nil || !o.takeNothing.Load() {
					src, to := f.Path, filepath.Join(dst, filepath.Base(f.Path))
					data, err := os.ReadFile(src)
					if err != nil {
						t.Errorf("read staged file: %v", err)
						continue
					}
					if o.moveAfter > 0 {
						time.AfterFunc(o.moveAfter, func() { _ = os.WriteFile(to, data, 0o644) })
					} else {
						_ = os.WriteFile(to, data, 0o644)
					}
				}
			}
			_, _ = w.Write([]byte(`{"id":4,"status":"completed","result":"successful"}`))
		default:
			w.WriteHeader(404)
		}
	}))
}

func sonarrEnv(t *testing.T, o sonarrOpts) (*env, string) {
	id := &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TVDBID: 79168, EpisodeRuntime: 22 * time.Minute, Confidence: 1, Source: "test"}
	e := setup(t, tvInfo, fakeProvider{id})
	root := filepath.Join(e.out, "sonarr-tv")
	srv := fakeSonarr(t, root, o)
	t.Cleanup(srv.Close)
	e.cfg.Arr.Sonarr = config.ArrApp{Enabled: true, URL: srv.URL, APIKey: "k", RootFolder: root, AddMissing: true, ImportMode: "move"}
	// These tests are about the import itself; review is tested apart.
	e.cfg.Arr.ImportPolicy = PolicyAlways
	e.m.SetConfig(e.cfg)
	return e, root
}

// Seen on The Twilight Zone: the scan ran in the same second the series
// was added, imported nothing and was reported as a success.
func TestSonarrImportWaitsForNewSeries(t *testing.T) {
	e, root := sonarrEnv(t, sonarrOpts{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	if j.Stage != StageDone || len(j.Warnings) != 0 {
		t.Fatalf("stage=%s warnings=%v log=%v", j.Stage, j.Warnings, j.Log)
	}
	got, _ := filepath.Glob(filepath.Join(root, "Friends (1994)", "*.mkv"))
	if len(got) != 2 {
		t.Fatalf("sonarr received %d files, want 2", len(got))
	}
}

// A scan that takes nothing is a warning naming the files left behind.
func TestSonarrImportNothingTakenIsWarned(t *testing.T) {
	var none atomic.Bool
	none.Store(true)
	e, _ := sonarrEnv(t, sonarrOpts{takeNothing: &none})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	if j.Stage != StageDone || len(j.Warnings) != 1 || !strings.Contains(j.Warnings[0], "imported 0 of 2") {
		t.Fatalf("stage=%s warnings=%v", j.Stage, j.Warnings)
	}
	for _, o := range j.Outputs {
		if _, err := os.Stat(o.Path); err != nil {
			t.Fatalf("file should stay in staging: %v", err)
		}
	}
}

// A file the app rejects is named in a warning; the rest are imported.
func TestSonarrImportPartialRejection(t *testing.T) {
	e, root := sonarrEnv(t, sonarrOpts{reject: "Friends - S01E02.mkv"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	if j.Stage != StageDone || len(j.Warnings) == 0 || !strings.Contains(strings.Join(j.Warnings, " "), "Friends - S01E02.mkv (Sample)") {
		t.Fatalf("stage=%s warnings=%v", j.Stage, j.Warnings)
	}
	if got, _ := filepath.Glob(filepath.Join(root, "Friends (1994)", "*.mkv")); len(got) != 1 {
		t.Fatalf("sonarr received %d files, want 1", len(got))
	}
	// Each file records what Sonarr did with it, for the history page.
	status := map[string]string{}
	for _, o := range j.Outputs {
		status[filepath.Base(o.Path)] = o.Import
	}
	if status["Friends - S01E01.mkv"] != "imported" || status["Friends - S01E02.mkv"] != "not imported: Sample" {
		t.Fatalf("import status = %v", status)
	}
}

// On the real NAS Sonarr reported success while this machine's NFS cache
// still showed the files in staging: that is not a failed import.
func TestSonarrImportSeenLateIsNotAFailure(t *testing.T) {
	e, root := sonarrEnv(t, sonarrOpts{moveAfter: 300 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	if j.Stage != StageDone || len(j.Warnings) != 0 {
		t.Fatalf("stage=%s warnings=%v", j.Stage, j.Warnings)
	}
	for _, o := range j.Outputs {
		if o.Import != "imported" {
			t.Fatalf("%s: import = %q", o.Path, o.Import)
		}
	}
	if got, _ := filepath.Glob(filepath.Join(root, "Friends (1994)", "*.mkv")); len(got) != 2 {
		t.Fatalf("sonarr received %d files, want 2", len(got))
	}
}

// With the default policy a disc numbered by length waits for a person;
// approving with a correction renames the staged files, imports them and
// remembers the match for the next time the disc goes in.
func TestReviewHoldApproveAndRemember(t *testing.T) {
	e, root := sonarrEnv(t, sonarrOpts{})
	e.cfg.Arr.ImportPolicy = PolicyConfident
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)

	e.drv.insert("fp-friends-2", "FRIENDS_S1_D2")
	j := e.waitDone(t)
	if j.Stage != StageReview {
		t.Fatalf("stage = %s, want review", j.Stage)
	}
	if got, _ := filepath.Glob(filepath.Join(root, "Friends (1994)", "*.mkv")); len(got) != 0 {
		t.Fatalf("held disc was imported: %v", got)
	}
	if e.drv.ejectCount() != 1 {
		t.Fatal("a held disc should still leave the drive")
	}
	reviews := e.m.Reviews()
	if len(reviews) != 1 || reviews[0].ID != j.ID {
		t.Fatalf("reviews = %+v", reviews)
	}

	// It was really disc 2: episodes 5 and 6, and the second title is kept.
	edit := &ReviewEdit{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TVDBID: 79168, Season: 1, Episodes: map[int]int{1: 5, 2: 6}}
	done, err := e.m.ApproveReview(ctx, j.ID, edit)
	if err != nil {
		t.Fatal(err)
	}
	if done.Stage != StageDone || len(done.Warnings) != 0 {
		t.Fatalf("approved: stage=%s warnings=%v", done.Stage, done.Warnings)
	}
	got, _ := filepath.Glob(filepath.Join(root, "Friends (1994)", "*.mkv"))
	names := map[string]bool{}
	for _, g := range got {
		names[filepath.Base(g)] = true
	}
	if !names["Friends - S01E05.mkv"] || !names["Friends - S01E06.mkv"] {
		t.Fatalf("imported %v", got)
	}
	if len(e.m.Reviews()) != 0 {
		t.Fatal("review not cleared")
	}
	if next := e.st.NextEpisode("Friends", 1); next != 7 { // no TMDB id: keyed by title
		t.Fatalf("next episode = %d", next)
	}

	// Same disc again: the confirmed match is used, no review needed.
	_ = e.st.ForgetDisc("fp-friends-2")
	e.drv.insert("fp-friends-2", "FRIENDS_S1_D2")
	deadline := time.Now().Add(10 * time.Second)
	var again Job
	for time.Now().Before(deadline) {
		if r := e.m.Snapshot().Recent; len(r) == 2 && r[0].Stage.Terminal() {
			again = r[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if again.Stage != StageDone || again.Identity.Source != sourceManual || again.Selection.Picks[0].Episode != 5 {
		t.Fatalf("reinserted: stage=%s identity=%+v picks=%+v", again.Stage, again.Identity, again.Selection.Picks)
	}
	if !strings.HasPrefix(again.Verification, "✅ Confirmed by you") {
		t.Fatalf("verification = %q", again.Verification)
	}
}

func TestNeedsReview(t *testing.T) {
	e, _ := sonarrEnv(t, sonarrOpts{})
	rt := e.m.rt.Load()
	tv := &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Confidence: 1, Source: "sonarr"}
	movie := &metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Confidence: 1, Source: "radarr", Runtime: 136 * time.Minute}
	pick := func(d time.Duration) *selector.Selection {
		return &selector.Selection{Picks: []selector.Pick{{Title: &makemkv.Title{Duration: d}}}}
	}
	outs := []Output{{Path: "x"}}
	cases := []struct {
		name   string
		policy string
		job    Job
		want   bool
	}{
		{"always imports", PolicyAlways, Job{Identity: tv, Outputs: outs}, false},
		{"tv by length waits", PolicyConfident, Job{Identity: tv, Outputs: outs}, true},
		{"verified tv imports", PolicyVerified, Job{Identity: tv, Outputs: outs, CatalogMatched: 8, CatalogCompared: 8}, false},
		{"partly matched waits", PolicyConfident, Job{Identity: tv, Outputs: outs, CatalogMatched: 7, CatalogCompared: 9}, true},
		{"runtime-matched movie imports when confident", PolicyConfident, Job{Identity: movie, Outputs: outs, Selection: pick(136 * time.Minute)}, false},
		{"runtime-matched movie waits when verified only", PolicyVerified, Job{Identity: movie, Outputs: outs, Selection: pick(136 * time.Minute)}, true},
		{"movie off by 20 minutes waits", PolicyConfident, Job{Identity: movie, Outputs: outs, Selection: pick(156 * time.Minute)}, true},
	}
	for _, c := range cases {
		rt.cfg.Arr.ImportPolicy = c.policy
		if c.job.Identity.Kind == metadata.KindMovie {
			rt.cfg.Arr.Radarr.Enabled, rt.cfg.Arr.Radarr.URL, rt.cfg.Arr.Radarr.APIKey = true, "http://radarr", "k"
			rt.radarr = arr.New(arr.Radarr, rt.cfg.Arr.Radarr, time.Second, nil)
		}
		if got := needsReview(c.job, rt); got != c.want {
			t.Errorf("%s: needsReview = %v", c.name, got)
		}
	}
}

// Buttons pressed in Discord drive the same actions as the web UI.
func TestDiscordActions(t *testing.T) {
	e, root := sonarrEnv(t, sonarrOpts{})
	e.cfg.Arr.ImportPolicy = PolicyConfident
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	if j.Stage != StageReview {
		t.Fatalf("stage = %s", j.Stage)
	}
	if got := e.m.DiscordAction(ctx, "mr:approve:"+j.ID); !strings.HasPrefix(got, "✅ Approved and imported 2") {
		t.Fatalf("approve: %q", got)
	}
	if files, _ := filepath.Glob(filepath.Join(root, "Friends (1994)", "*.mkv")); len(files) != 2 {
		t.Fatalf("imported %v", files)
	}
	if got := e.m.DiscordAction(ctx, "mr:approve:"+j.ID); !strings.Contains(got, "no such review") {
		t.Fatalf("second approve: %q", got)
	}
	if got := e.m.DiscordAction(ctx, "mr:eject:fake0"); got != "Tray opened" {
		t.Fatalf("eject: %q", got)
	}
	if got := e.m.DiscordAction(ctx, "nonsense"); got != "Unknown button" {
		t.Fatalf("unknown: %q", got)
	}
}

// Seen on The Twilight Zone S1 D3: Sonarr could not delete the staged
// files, rolled the import back and still reported success. The disc is
// recorded with its files not imported, and a retry gets them in.
func TestSonarrImportRetry(t *testing.T) {
	var none atomic.Bool
	none.Store(true)
	e, root := sonarrEnv(t, sonarrOpts{takeNothing: &none})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	if !importFailed(j) {
		t.Fatalf("import should have failed: %+v", j.Outputs)
	}
	if _, err := e.m.RetryImport(ctx, "no-such-job"); err == nil {
		t.Fatal("retry of an unknown job should fail")
	}

	none.Store(false)
	r, err := e.m.RetryImport(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if importFailed(r) || r.Stage != StageDone || len(r.Warnings) != 0 {
		t.Fatalf("after retry: stage=%s warnings=%v outputs=%+v", r.Stage, r.Warnings, r.Outputs)
	}
	if got, _ := filepath.Glob(filepath.Join(root, "Friends (1994)", "*.mkv")); len(got) != 2 {
		t.Fatalf("sonarr holds %d files, want 2", len(got))
	}
	for _, o := range r.Outputs {
		if _, err := os.Stat(o.Path); !os.IsNotExist(err) {
			t.Fatalf("%s should have left staging", o.Path)
		}
	}
	// The saved history now shows the import, and a second retry has
	// nothing left to do.
	if rec, ok := e.m.Record(j.ID); !ok || importFailed(rec) {
		t.Fatalf("record after retry: %+v", rec.Outputs)
	}
	if _, err := e.m.RetryImport(ctx, j.ID); err == nil {
		t.Fatal("second retry should have nothing to import")
	}
}

// An import cut short by shutdown is not a failure: it is marked pending
// and runs again when media-ripper starts.
func TestInterruptedImportResumesAtStart(t *testing.T) {
	old := heldWait
	heldWait = 30 * time.Second
	t.Cleanup(func() { heldWait = old })
	var none atomic.Bool
	none.Store(true)
	var commands atomic.Int32
	e, root := sonarrEnv(t, sonarrOpts{takeNothing: &none, commands: &commands})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.m.Run(ctx); close(done) }()
	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")

	// Stop while the import waits for Sonarr to list the files.
	deadline := time.Now().Add(10 * time.Second)
	for commands.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if commands.Load() == 0 {
		t.Fatal("import never started")
	}
	cancel()
	<-done
	j := e.waitDone(t)
	if got := e.st.PendingImports(); len(got) != 1 || got[0] != j.ID {
		t.Fatalf("pending = %v, want %s", got, j.ID)
	}
	if !importFailed(j) || j.Outputs[0].Import != "not imported: interrupted" {
		t.Fatalf("outputs = %+v", j.Outputs)
	}

	// The next start imports them.
	none.Store(false)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	e.m.resumeImports(ctx2)
	if got := e.st.PendingImports(); len(got) != 0 {
		t.Fatalf("still pending: %v", got)
	}
	if rec, ok := e.m.Record(j.ID); !ok || importFailed(rec) {
		t.Fatalf("after resume: %+v", rec.Outputs)
	}
	if got, _ := filepath.Glob(filepath.Join(root, "Friends (1994)", "*.mkv")); len(got) != 2 {
		t.Fatalf("sonarr holds %d files, want 2", len(got))
	}
}

// Files someone already imported by hand (as on S1 D3) are recognised:
// the retry sends no new import and only tidies staging.
func TestRetryWhenAppAlreadyHoldsTheFiles(t *testing.T) {
	var none atomic.Bool
	none.Store(true)
	var commands atomic.Int32
	e, root := sonarrEnv(t, sonarrOpts{takeNothing: &none, commands: &commands})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	if !importFailed(j) {
		t.Fatalf("import should have failed: %+v", j.Outputs)
	}
	sent := commands.Load()
	for _, o := range j.Outputs {
		copyFile(t, o.Path, filepath.Join(root, "Friends (1994)", filepath.Base(o.Path)))
	}
	r, err := e.m.RetryImport(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if importFailed(r) || len(r.Warnings) != 0 || commands.Load() != sent {
		t.Fatalf("outputs=%+v warnings=%v commands %d -> %d", r.Outputs, r.Warnings, sent, commands.Load())
	}
	for _, o := range r.Outputs {
		if _, err := os.Stat(o.Path); !os.IsNotExist(err) {
			t.Fatalf("%s should have left staging", o.Path)
		}
	}
}
