package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/selector"
)

// fakeApp is a Radarr or Sonarr with a small catalogue and a library on
// disk (its paths are this machine's: no path map). Imports copy or move
// the files into the movie's or series' folder; removing an entry leaves
// its files where they are.
type fakeApp struct {
	t       *testing.T
	sonarr  bool
	root    string
	catalog []map[string]any

	mu      sync.Mutex
	items   map[int]map[string]any
	next    int
	removed []string
	modes   []string
}

func newFakeApp(t *testing.T, sonarr bool, root string, catalog ...map[string]any) (*fakeApp, string) {
	a := &fakeApp{t: t, sonarr: sonarr, root: root, catalog: catalog, items: map[int]map[string]any{}, next: 1}
	srv := httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(srv.Close)
	return a, srv.URL
}

func movie(title string, year, tmdb int) map[string]any {
	return map[string]any{"title": title, "year": year, "tmdbId": tmdb}
}

func series(title string, year, tvdb int) map[string]any {
	return map[string]any{"title": title, "year": year, "tvdbId": tvdb}
}

func (a *fakeApp) res() (string, string) {
	if a.sonarr {
		return "series", "tvdbId"
	}
	return "movie", "tmdbId"
}

// item returns the library entry for an id from the catalogue, if added.
func (a *fakeApp) item(catID int) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, key := a.res()
	for _, it := range a.items {
		if int(num(it[key])) == catID {
			return it
		}
	}
	return nil
}

// add puts a catalogue entry into the library, as if added by hand.
func (a *fakeApp) add(catID int, monitored bool) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, key := a.res()
	for _, c := range a.catalog {
		if int(num(c[key])) == catID {
			return a.addLocked(c, monitored)
		}
	}
	a.t.Fatalf("no catalogue entry %d", catID)
	return nil
}

func (a *fakeApp) addLocked(c map[string]any, monitored bool) map[string]any {
	it := map[string]any{}
	for k, v := range c {
		it[k] = v
	}
	it["id"] = a.next
	a.next++
	it["monitored"] = monitored
	it["path"] = filepath.Join(a.root, fmt.Sprintf("%s (%d)", it["title"], int(num(it["year"]))))
	_ = os.MkdirAll(it["path"].(string), 0o755)
	a.items[int(num(it["id"]))] = it
	return it
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

func filesUnder(dir string) []map[string]any {
	out := []map[string]any{}
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			out = append(out, map[string]any{"id": len(out) + 1, "path": p, "size": info.Size()})
		}
		return nil
	})
	return out
}

func (a *fakeApp) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	res, key := a.res()
	q := r.URL.Query()
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch p := r.URL.Path; {
	case p == "/api/v3/"+res+"/lookup":
		term := strings.ToLower(q.Get("term"))
		var out []map[string]any
		for _, c := range a.catalog {
			if id, ok := strings.CutPrefix(term, "tmdb:"); ok && !a.sonarr {
				if strconv.Itoa(int(num(c[key]))) == id {
					out = append(out, c)
				}
				continue
			}
			if id, ok := strings.CutPrefix(term, "tvdb:"); ok && a.sonarr {
				if strconv.Itoa(int(num(c[key]))) == id {
					out = append(out, c)
				}
				continue
			}
			if strings.Contains(strings.ToLower(c["title"].(string)), strings.Fields(term + " x")[0]) {
				out = append(out, c)
			}
		}
		if out == nil {
			out = []map[string]any{}
		}
		write(out)
	case p == "/api/v3/"+res && r.Method == http.MethodGet:
		a.mu.Lock()
		out := []map[string]any{}
		for _, it := range a.items {
			if v := q.Get(key); v == "" || strconv.Itoa(int(num(it[key]))) == v {
				out = append(out, it)
			}
		}
		a.mu.Unlock()
		write(out)
	case p == "/api/v3/"+res && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.mu.Lock()
		it := a.addLocked(body, body["monitored"] == true)
		a.mu.Unlock()
		write(it)
	case strings.HasPrefix(p, "/api/v3/"+res+"/") && r.Method == http.MethodDelete:
		if q.Get("deleteFiles") != "false" {
			a.t.Errorf("remove without deleteFiles=false: %s", r.URL)
		}
		id, _ := strconv.Atoi(strings.TrimPrefix(p, "/api/v3/"+res+"/"))
		a.mu.Lock()
		if it := a.items[id]; it != nil {
			a.removed = append(a.removed, it["title"].(string))
		}
		delete(a.items, id)
		a.mu.Unlock()
		w.WriteHeader(200)
	case p == "/api/v3/qualityprofile":
		write([]map[string]any{{"id": 1, "name": "Any"}})
	case p == "/api/v3/qualitydefinition":
		write([]map[string]any{{"quality": map[string]any{"id": 30, "name": "Remux-1080p"}}, {"quality": map[string]any{"id": 2, "name": "DVD"}}})
	case p == "/api/v3/episode":
		var eps []map[string]any
		for n := 1; n <= 20; n++ {
			eps = append(eps, map[string]any{"id": 100 + n, "seasonNumber": 1, "episodeNumber": n, "title": fmt.Sprintf("Episode %d", n)})
		}
		write(eps)
	case p == "/api/v3/moviefile" || p == "/api/v3/episodefile":
		idq := q.Get("movieId")
		if a.sonarr {
			idq = q.Get("seriesId")
		}
		id, _ := strconv.Atoi(idq)
		a.mu.Lock()
		it := a.items[id]
		a.mu.Unlock()
		if it == nil {
			write([]map[string]any{})
			return
		}
		write(filesUnder(it["path"].(string)))
	case p == "/api/v3/filesystem":
		entries, err := os.ReadDir(q.Get("path"))
		if err != nil {
			w.WriteHeader(404)
			return
		}
		out := map[string]any{"directories": []any{}, "files": []any{}}
		for _, e := range entries {
			if e.IsDir() {
				out["directories"] = append(out["directories"].([]any), map[string]any{"name": e.Name()})
			} else if info, err := e.Info(); err == nil {
				out["files"] = append(out["files"].([]any), map[string]any{"path": filepath.Join(q.Get("path"), e.Name()), "size": info.Size()})
			}
		}
		write(out)
	case p == "/api/v3/manualimport":
		var items []map[string]any
		target, _ := strconv.Atoi(q.Get("movieId"))
		for _, f := range filesUnder(q.Get("folder")) {
			it := map[string]any{"path": f["path"], "size": f["size"], "quality": map[string]any{}, "rejections": []any{}}
			id := target
			a.mu.Lock()
			for iid, li := range a.items {
				if id == 0 && (strings.Contains(f["path"].(string), li["title"].(string)) || len(a.items) == 1) {
					id = iid
				}
			}
			a.mu.Unlock()
			switch {
			case id == 0:
				it["rejections"] = []any{map[string]any{"reason": "Unknown"}}
			case a.sonarr:
				it["series"] = map[string]any{"id": id}
				it["episodes"] = []any{map[string]any{"id": 101}}
			default:
				it["movie"] = map[string]any{"id": id}
			}
			items = append(items, it)
		}
		write(items)
	case p == "/api/v3/command" && r.Method == http.MethodPost:
		var body struct {
			ImportMode string `json:"importMode"`
			Files      []struct {
				Path     string `json:"path"`
				MovieID  int    `json:"movieId"`
				SeriesID int    `json:"seriesId"`
			} `json:"files"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.mu.Lock()
		a.modes = append(a.modes, body.ImportMode)
		for _, f := range body.Files {
			id := f.MovieID
			if a.sonarr {
				id = f.SeriesID
			}
			it := a.items[id]
			if it == nil {
				a.t.Errorf("import into unknown %s %d", res, id)
				continue
			}
			dst := filepath.Join(it["path"].(string), filepath.Base(f.Path))
			if body.ImportMode == "move" {
				if err := os.Rename(f.Path, dst); err != nil {
					a.t.Errorf("move: %v", err)
				}
			} else {
				data, err := os.ReadFile(f.Path)
				if err != nil {
					a.t.Errorf("copy: %v", err)
					continue
				}
				_ = os.WriteFile(dst, data, 0o644)
			}
		}
		a.mu.Unlock()
		write(map[string]any{"id": 1, "status": "completed", "result": "successful"})
	default:
		w.WriteHeader(404)
	}
}

func (a *fakeApp) removedTitles() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.removed...)
}

// titleEnv has a Radarr and a Sonarr, both knowing a few titles.
func titleEnv(t *testing.T, info string, provider metadata.Provider) (*env, *fakeApp, *fakeApp) {
	e := setup(t, info, provider)
	radarr, rURL := newFakeApp(t, false, filepath.Join(e.out, "radarr"),
		movie("Friends Movie", 2001, 500), movie("Wrong Movie", 2000, 1), movie("Right Movie", 2001, 2))
	sonarr, sURL := newFakeApp(t, true, filepath.Join(e.out, "sonarr"), series("Friends", 1994, 79168))
	e.cfg.Arr.Radarr = config.ArrApp{Enabled: true, URL: rURL, APIKey: "k", RootFolder: radarr.root, AddMissing: true, ImportMode: "move"}
	e.cfg.Arr.Sonarr = config.ArrApp{Enabled: true, URL: sURL, APIKey: "k", RootFolder: sonarr.root, AddMissing: true, ImportMode: "move"}
	e.cfg.Arr.ImportPolicy = PolicyAlways
	e.m.SetConfig(e.cfg)
	return e, radarr, sonarr
}

func friendsTV() *metadata.Identity {
	return &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TVDBID: 79168, EpisodeRuntime: 22 * time.Minute, Confidence: 1, Source: "test"}
}

func friendsMovie(runtime time.Duration) *metadata.Identity {
	return &metadata.Identity{Kind: metadata.KindMovie, Title: "Friends Movie", Year: 2001, TMDBID: 500, Runtime: runtime, Confidence: 1, Source: "test"}
}

var toMovie = ReviewEdit{Kind: metadata.KindMovie, Title: "Friends Movie", Year: 2001, TMDBID: 500}
var toTV = ReviewEdit{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TVDBID: 79168, Season: 1}

// waitHistory waits for a history record that ok accepts.
func (e *env) waitHistory(t *testing.T, ok func(Job) bool) Job {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		h, _ := e.st.History(0)
		for _, raw := range h {
			var j Job
			if json.Unmarshal(raw, &j) == nil && ok(j) {
				return j
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no such job in the history")
	return Job{}
}

// waitRipping waits until a job is ripping its titles.
func (e *env) waitRipping(t *testing.T) Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, d := range e.m.Snapshot().Drives {
			if j := d.Job; j != nil && j.Stage == StageRipping && j.Selection != nil {
				return *j
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no job started ripping")
	return Job{}
}

func held(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, f := range filesUnder(dir) {
		out = append(out, filepath.Base(f["path"].(string)))
	}
	return out
}

func isMovieDone(j Job) bool {
	return j.Stage == StageDone && j.Identity != nil && j.Identity.Kind == metadata.KindMovie && j.Identity.Source == sourceManual
}

func isTVDone(j Job) bool {
	return j.Stage == StageDone && j.Identity != nil && j.Identity.Kind == metadata.KindTV && j.Identity.Source == sourceManual
}

func (e *env) assertMatch(t *testing.T, fp string, kind metadata.Kind, rerip bool) {
	t.Helper()
	dm, ok := e.st.DiscMatch(fp)
	if !ok || dm.Kind != string(kind) || dm.Rerip != rerip {
		t.Fatalf("remembered match = %+v (ok %v), want %s rerip=%v", dm, ok, kind, rerip)
	}
}

// Running, TV → movie: the feature was not among the episodes being
// ripped, and the disc is still in, so it is ripped again as the movie.
func TestCorrectRunningTVToMovieRipsAgain(t *testing.T) {
	e, radarr, _ := titleEnv(t, tvInfo, fakeProvider{friendsTV()})
	t.Setenv("FAKE_SLOW", "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends", "FRIENDS_S1_D1")
	j := e.waitRipping(t)
	if j.Identity.Kind != metadata.KindTV {
		t.Fatalf("identified as %s", j.Identity.Kind)
	}
	_, msg, err := e.m.CorrectTitle(ctx, j.ID, toMovie)
	if err != nil || !strings.HasPrefix(msg, "Ripping it again as Friends Movie (2001)") {
		t.Fatalf("correct: %q %v", msg, err)
	}
	done := e.waitHistory(t, isMovieDone)
	if len(done.Selection.Picks) != 1 || done.Selection.Picks[0].Title.ID != 0 {
		t.Fatalf("movie picks = %+v", done.Selection.Picks)
	}
	if it := radarr.item(500); it == nil || len(held(t, it["path"].(string))) != 1 {
		t.Fatalf("radarr holds %v", it)
	}
	e.assertMatch(t, "fp-friends", metadata.KindMovie, false)
}

// Running, movie → TV: the episodes were not ripped; ripped again as TV.
func TestCorrectRunningMovieToTVRipsAgain(t *testing.T) {
	e, _, sonarr := titleEnv(t, tvInfo, fakeProvider{friendsMovie(0)})
	t.Setenv("FAKE_SLOW", "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends", "FRIENDS_S1_D1")
	j := e.waitRipping(t)
	if j.Identity.Kind != metadata.KindMovie {
		t.Fatalf("identified as %s", j.Identity.Kind)
	}
	if _, msg, err := e.m.CorrectTitle(ctx, j.ID, toTV); err != nil || !strings.HasPrefix(msg, "Ripping it again as Friends S01") {
		t.Fatalf("correct: %q %v", msg, err)
	}
	done := e.waitHistory(t, isTVDone)
	var eps []int
	for _, p := range done.Selection.Picks {
		eps = append(eps, p.Episode)
	}
	if fmt.Sprint(eps) != "[1 2]" {
		t.Fatalf("episodes = %v", eps)
	}
	if it := sonarr.item(79168); it == nil || len(held(t, it["path"].(string))) != 2 {
		t.Fatalf("sonarr holds %v", it)
	}
	e.assertMatch(t, "fp-friends", metadata.KindTV, false)
}

// Running, same kind: the correction waits and the import goes to the
// right movie; the files are renamed once, before it.
func TestCorrectRunningMovieWaitsForCopy(t *testing.T) {
	wrong := &metadata.Identity{Kind: metadata.KindMovie, Title: "Wrong Movie", Year: 2000, TMDBID: 1, Runtime: 136 * time.Minute, Confidence: 1, Source: "test"}
	e, radarr, _ := titleEnv(t, movieInfo, fakeProvider{wrong})
	t.Setenv("FAKE_SLOW", "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitRipping(t)
	_, msg, err := e.m.CorrectTitle(ctx, j.ID, ReviewEdit{Kind: metadata.KindMovie, Title: "Right Movie", Year: 2001, TMDBID: 2})
	if err != nil || !strings.HasPrefix(msg, "Will be imported as Right Movie (2001)") {
		t.Fatalf("correct: %q %v", msg, err)
	}
	if s, _ := e.m.Job(j.ID); s.PendingTitle != "Right Movie (2001)" {
		t.Fatalf("pending = %q", s.PendingTitle)
	}
	done := e.waitHistory(t, func(j Job) bool { return j.Stage == StageDone })
	if done.Identity.TMDBID != 2 || done.PendingTitle != "" {
		t.Fatalf("done as %+v", done.Identity)
	}
	if radarr.item(1) != nil {
		t.Fatal("the wrong movie was added to Radarr")
	}
	if it := radarr.item(2); it == nil || len(held(t, it["path"].(string))) != 1 {
		t.Fatalf("right movie holds %v", it)
	}
	e.assertMatch(t, "fp-matrix", metadata.KindMovie, false)
}

// Review, TV → movie: the feature was not ripped; it is remembered and the
// disc ripped as the movie when it goes back in, though ripped before.
func TestCorrectReviewTVToMovie(t *testing.T) {
	e, radarr, _ := titleEnv(t, tvInfo, fakeProvider{friendsTV()})
	e.cfg.Arr.ImportPolicy = PolicyConfident
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	if j.Stage != StageReview {
		t.Fatalf("stage = %s", j.Stage)
	}
	_, msg, err := e.m.CorrectTitle(ctx, j.ID, toMovie)
	if err != nil || !strings.Contains(msg, "put the disc back in") {
		t.Fatalf("correct: %q %v", msg, err)
	}
	if len(e.m.Reviews()) != 0 {
		t.Fatal("still held for review")
	}
	e.assertMatch(t, "fp-friends", metadata.KindMovie, true)
	e.drv.insert("fp-friends", "FRIENDS_S1_D1")
	e.waitHistory(t, isMovieDone)
	if it := radarr.item(500); it == nil || len(held(t, it["path"].(string))) != 1 {
		t.Fatalf("radarr holds %v", it)
	}
	e.assertMatch(t, "fp-friends", metadata.KindMovie, false)
}

// Review, movie → TV: the episodes were not ripped; the same.
func TestCorrectReviewMovieToTV(t *testing.T) {
	e, _, sonarr := titleEnv(t, tvInfo, fakeProvider{friendsMovie(30 * time.Minute)}) // runtime off: held
	e.cfg.Arr.ImportPolicy = PolicyConfident
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	if j.Stage != StageReview {
		t.Fatalf("stage = %s", j.Stage)
	}
	if _, msg, err := e.m.CorrectTitle(ctx, j.ID, toTV); err != nil || !strings.Contains(msg, "put the disc back in") {
		t.Fatalf("correct: %q %v", msg, err)
	}
	e.assertMatch(t, "fp-friends", metadata.KindTV, true)
	e.drv.insert("fp-friends", "FRIENDS_S1_D1")
	e.waitHistory(t, isTVDone)
	if it := sonarr.item(79168); it == nil || len(held(t, it["path"].(string))) != 2 {
		t.Fatalf("sonarr holds %v", it)
	}
}

// Review, kind kept but the titles already there: approved as corrected.
func TestCorrectReviewWithTitlesAtHand(t *testing.T) {
	e, _, sonarr := titleEnv(t, tvInfo, fakeProvider{&metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, EpisodeRuntime: 22 * time.Minute, Confidence: 0.5, Source: "test"}})
	e.cfg.Arr.ImportPolicy = PolicyConfident
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	if j.Stage != StageReview {
		t.Fatalf("stage = %s", j.Stage)
	}
	got := e.m.DiscordAction(ctx, "mr:titlepick:"+j.ID+":1", "tv:79168")
	if !strings.HasPrefix(got, "✏️ Imported as Friends S01") {
		t.Fatalf("discord pick: %q", got)
	}
	if it := sonarr.item(79168); it == nil || len(held(t, it["path"].(string))) != 2 {
		t.Fatalf("sonarr holds %v", it)
	}
}

// Done and unidentified (LOGICAL_VOLUME_ID): named a movie, the longest
// title it kept is the feature and goes to Radarr. Named a TV show, the
// episodes were not ripped, so it waits for the disc.
func TestCorrectDoneUnidentified(t *testing.T) {
	e, radarr, _ := titleEnv(t, tvInfo, metadata.NoneProvider{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-unknown", "LOGICAL_VOLUME_ID")
	j := e.waitDone(t)
	e.waitHistory(t, func(h Job) bool { return h.ID == j.ID })
	if j.Stage != StageDone || j.Identity.Identified() {
		t.Fatalf("stage %s identity %+v", j.Stage, j.Identity)
	}
	if _, msg, err := e.m.CorrectTitle(ctx, j.ID, toTV); err != nil || !strings.Contains(msg, "Put the disc back in") {
		t.Fatalf("to TV: %q %v", msg, err)
	}
	e.assertMatch(t, "fp-unknown", metadata.KindTV, true)
	done, msg, err := e.m.CorrectTitle(ctx, j.ID, toMovie)
	if err != nil || msg != "Imported as Friends Movie (2001)" {
		t.Fatalf("to movie: %q %v", msg, err)
	}
	if done.Identity.Kind != metadata.KindMovie {
		t.Fatalf("identity %+v", done.Identity)
	}
	if it := radarr.item(500); it == nil || len(held(t, it["path"].(string))) != 1 {
		t.Fatalf("radarr holds %v", it)
	}
	e.assertMatch(t, "fp-unknown", metadata.KindMovie, false)
}

// Done and imported as TV, really a movie: Sonarr's series, made by
// media-ripper for this disc, is taken out (its files stay) and the disc is
// ripped again as the movie.
func TestCorrectDoneImportedTVToMovie(t *testing.T) {
	e, radarr, sonarr := titleEnv(t, tvInfo, fakeProvider{friendsTV()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	e.waitHistory(t, func(h Job) bool { return h.ID == j.ID })
	files := held(t, sonarr.item(79168)["path"].(string))
	if len(files) != 2 {
		t.Fatalf("sonarr holds %v", files)
	}
	_, msg, err := e.m.CorrectTitle(ctx, j.ID, toMovie)
	if err != nil || !strings.Contains(msg, "Friends was removed from Sonarr; its files stay") {
		t.Fatalf("correct: %q %v", msg, err)
	}
	if got := sonarr.removedTitles(); len(got) != 1 {
		t.Fatalf("removed %v", got)
	}
	if left := filesUnder(filepath.Join(sonarr.root, "Friends (1994)")); len(left) != 2 {
		t.Fatalf("the series' files went: %v", left)
	}
	e.drv.insert("fp-friends", "FRIENDS_S1_D1")
	e.waitHistory(t, isMovieDone)
	if it := radarr.item(500); it == nil || len(held(t, it["path"].(string))) != 1 {
		t.Fatalf("radarr holds %v", it)
	}
}

// A series that is not only this disc's is left alone.
func TestCorrectDoneImportedLeavesSomeoneElsesSeries(t *testing.T) {
	e, _, sonarr := titleEnv(t, tvInfo, fakeProvider{friendsTV()})
	sonarr.add(79168, true) // the person's own, monitored
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	e.waitHistory(t, func(h Job) bool { return h.ID == j.ID })
	_, msg, err := e.m.CorrectTitle(ctx, j.ID, toMovie)
	if err != nil || !strings.Contains(msg, "left alone (it is monitored)") {
		t.Fatalf("correct: %q %v", msg, err)
	}
	if got := sonarr.removedTitles(); len(got) != 0 {
		t.Fatalf("removed %v", got)
	}
}

// Done and imported as a movie, really TV: the same the other way.
func TestCorrectDoneImportedMovieToTV(t *testing.T) {
	e, radarr, _ := titleEnv(t, tvInfo, fakeProvider{friendsMovie(0)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	e.waitHistory(t, func(h Job) bool { return h.ID == j.ID })
	if it := radarr.item(500); it == nil || len(held(t, it["path"].(string))) != 1 {
		t.Fatalf("radarr holds %v", it)
	}
	_, msg, err := e.m.CorrectTitle(ctx, j.ID, toTV)
	if err != nil || !strings.Contains(msg, "Friends Movie was removed from Radarr") {
		t.Fatalf("correct: %q %v", msg, err)
	}
	e.assertMatch(t, "fp-friends", metadata.KindTV, true)
	e.drv.insert("fp-friends", "FRIENDS_S1_D1")
	e.waitHistory(t, isTVDone)
}

// Done and imported as the wrong movie: Radarr moves the file to the right
// one, and the wrong entry, which media-ripper made, goes.
func TestCorrectDoneImportedMovieMoves(t *testing.T) {
	wrong := &metadata.Identity{Kind: metadata.KindMovie, Title: "Wrong Movie", Year: 2000, TMDBID: 1, Runtime: 136 * time.Minute, Confidence: 1, Source: "test"}
	e, radarr, _ := titleEnv(t, movieInfo, fakeProvider{wrong})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitDone(t)
	e.waitHistory(t, func(h Job) bool { return h.ID == j.ID })
	if it := radarr.item(1); it == nil || len(held(t, it["path"].(string))) != 1 {
		t.Fatalf("wrong movie holds %v", it)
	}
	done, msg, err := e.m.CorrectTitle(ctx, j.ID, ReviewEdit{Kind: metadata.KindMovie, Title: "Right Movie", Year: 2001, TMDBID: 2})
	if err != nil || msg != "Moved to Right Movie (2001) in Radarr" {
		t.Fatalf("correct: %q %v", msg, err)
	}
	if radarr.item(1) != nil {
		t.Fatal("the wrong movie is still in Radarr")
	}
	if it := radarr.item(2); it == nil || len(held(t, it["path"].(string))) != 1 {
		t.Fatalf("right movie holds %v", it)
	}
	if left := filesUnder(filepath.Join(radarr.root, "Wrong Movie (2000)")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	if done.Identity.TMDBID != 2 {
		t.Fatalf("identity %+v", done.Identity)
	}
	if last := e.waitHistory(t, func(h Job) bool { return h.ID == j.ID && h.Identity.TMDBID == 2 }); last.Stage != StageDone {
		t.Fatalf("history %+v", last)
	}
	e.assertMatch(t, "fp-matrix", metadata.KindMovie, false)
}

// A wrong movie that is the person's own (monitored) is not touched.
func TestCorrectDoneImportedMovieLeavesMonitored(t *testing.T) {
	wrong := &metadata.Identity{Kind: metadata.KindMovie, Title: "Wrong Movie", Year: 2000, TMDBID: 1, Runtime: 136 * time.Minute, Confidence: 1, Source: "test"}
	e, radarr, _ := titleEnv(t, movieInfo, fakeProvider{wrong})
	radarr.add(1, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitDone(t)
	e.waitHistory(t, func(h Job) bool { return h.ID == j.ID })
	_, _, err := e.m.CorrectTitle(ctx, j.ID, ReviewEdit{Kind: metadata.KindMovie, Title: "Right Movie", Year: 2001, TMDBID: 2})
	if err == nil || !strings.Contains(err.Error(), "monitored") {
		t.Fatalf("err = %v", err)
	}
	if radarr.item(1) == nil || radarr.item(2) != nil {
		t.Fatal("Radarr was changed")
	}
}

// Running with the disc already out, kind switched to titles this rip does
// not take: saved, this copy is not imported, and the disc is asked for.
// The job has no saved scan: its titles are rebuilt from the summaries.
func TestCorrectRunningDiscOutSwitchesKind(t *testing.T) {
	for _, tc := range []struct {
		name  string
		from  *metadata.Identity
		picks []int
		to    ReviewEdit
		kind  metadata.Kind
	}{
		{"tv to movie", friendsTV(), []int{1, 2}, toMovie, metadata.KindMovie},
		{"movie to tv", friendsMovie(0), []int{0}, toTV, metadata.KindTV},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := titleEnv(t, tvInfo, fakeProvider{tc.from})
			dir := t.TempDir()
			titles := map[int]time.Duration{0: 88 * time.Minute, 1: 22*time.Minute + 10*time.Second, 2: 21*time.Minute + 50*time.Second, 3: 3 * time.Minute}
			job := &Job{mu: &sync.Mutex{}, rt: e.m.rt.Load(), ID: "j-" + strings.ReplaceAll(tc.name, " ", "-"), Fingerprint: "fp-out", Stage: StageDelivering, Ejected: true, Identity: tc.from, DiscType: "DVD disc"}
			for id := 0; id <= 3; id++ {
				job.Titles = append(job.Titles, TitleSummary{ID: id, Duration: titles[id], SizeBytes: 100000})
			}
			sel := &selector.Selection{Kind: tc.from.Kind}
			for i, id := range tc.picks {
				p := selector.Pick{Title: &makemkv.Title{ID: id, Duration: titles[id]}, Reason: "test"}
				if tc.from.Kind == metadata.KindTV {
					p.Season, p.Episode = 1, i+1
				}
				sel.Picks = append(sel.Picks, p)
				f := filepath.Join(dir, fmt.Sprintf("t%d.mkv", id))
				if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				job.Outputs = append(job.Outputs, Output{Path: f, Size: 1, TitleID: id})
			}
			job.Selection = sel
			e.m.remember(job)
			_, msg, err := e.m.CorrectTitle(context.Background(), job.ID, tc.to)
			if err != nil || !strings.Contains(msg, "the disc is already out") {
				t.Fatalf("correct: %q %v", msg, err)
			}
			if e.m.applyTitleChange(job) {
				t.Fatal("imported anyway")
			}
			s := job.Snapshot()
			if len(s.Warnings) != 1 || !strings.HasPrefix(s.Warnings[0], "not imported") || s.Identity.Kind != tc.from.Kind {
				t.Fatalf("warnings %v identity %+v", s.Warnings, s.Identity)
			}
			e.assertMatch(t, "fp-out", tc.kind, true)
			e.m.reripDone(job)
			e.assertMatch(t, "fp-out", tc.kind, true) // still wanted: this run did not rip it
		})
	}
}

// The Discord flow: a form for the kind, a menu from that app's search
// labelled with the kind, and the pick applied.
func TestDiscordTitleFlow(t *testing.T) {
	e, radarr, _ := titleEnv(t, tvInfo, metadata.NoneProvider{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-unknown", "LOGICAL_VOLUME_ID")
	j := e.waitDone(t)
	e.waitHistory(t, func(h Job) bool { return h.ID == j.ID })

	md := e.m.DiscordModal("mr:title:movie:" + j.ID)
	if md == nil || md.CustomID != "mr:titlesearch:movie:"+j.ID || len(md.Inputs) != 1 || md.Inputs[0].Value == "" {
		t.Fatalf("movie form = %+v", md)
	}
	if tv := e.m.DiscordModal("mr:title:tv:" + j.ID); tv == nil || len(tv.Inputs) != 2 || tv.Inputs[1].ID != "season" {
		t.Fatalf("tv form = %+v", tv)
	}
	menu := e.m.DiscordSearch(ctx, md.CustomID, map[string]string{"q": "friends"})
	if menu.CustomID != "mr:titlepick:"+j.ID+":0" || len(menu.Options) != 1 || menu.Options[0].Label != "Movie · Friends Movie (2001)" || menu.Options[0].Value != "movie:500" {
		t.Fatalf("movie menu = %+v", menu)
	}
	tvMenu := e.m.DiscordSearch(ctx, "mr:titlesearch:tv:"+j.ID, map[string]string{"q": "friends", "season": "1"})
	if len(tvMenu.Options) != 1 || tvMenu.Options[0].Label != "TV · Friends (1994)" || tvMenu.CustomID != "mr:titlepick:"+j.ID+":1" {
		t.Fatalf("tv menu = %+v", tvMenu)
	}
	if got := e.m.DiscordAction(ctx, menu.CustomID, "movie:500"); got != "✏️ Imported as Friends Movie (2001)" {
		t.Fatalf("pick: %q", got)
	}
	if it := radarr.item(500); it == nil || len(held(t, it["path"].(string))) != 1 {
		t.Fatalf("radarr holds %v", it)
	}
	// Nothing to correct on a failed disc: no form, and the button says why.
	if e.m.DiscordModal("mr:title:movie:nope") != nil {
		t.Fatal("form for an unknown job")
	}
	if got := e.m.DiscordAction(ctx, "mr:title:movie:nope"); got != "That disc is no longer known" {
		t.Fatalf("unknown: %q", got)
	}
}
