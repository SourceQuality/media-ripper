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
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/metadata"
)

// fakeRadarr "imports" by moving whatever is under the scanned path into
// its own root folder, like the real thing does with importMode Move.
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
		case r.URL.Path == "/api/v3/command" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			p, _ := body["path"].(string)
			if !strings.HasPrefix(p, pathPrefix) {
				t.Errorf("import path %q not under %q", p, pathPrefix)
			}
			local := strings.Replace(p, pathPrefix, filepath.Dir(root), 1)
			entries, _ := os.ReadDir(local)
			_ = os.MkdirAll(filepath.Join(root, "The Matrix (1999)"), 0o755)
			for _, e := range entries {
				_ = os.Rename(filepath.Join(local, e.Name()), filepath.Join(root, "The Matrix (1999)", e.Name()))
			}
			_, _ = w.Write([]byte(`{"id":9,"status":"completed"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	return srv, &calls
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
