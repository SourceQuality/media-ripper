package arr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/config"
)

func fakeRadarr(t *testing.T) (*httptest.Server, *[]string) {
	var calls []string
	added := false
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "k" {
			w.WriteHeader(401)
			return
		}
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/lookup":
			_, _ = w.Write([]byte(`[{"title":"The Matrix","year":1999,"tmdbId":603,"runtime":136,"titleSlug":"the-matrix-603","images":[]},{"title":"The Matrix Reloaded","year":2003,"tmdbId":604,"runtime":138}]`))
		case r.URL.Path == "/api/v3/movie" && r.Method == http.MethodGet:
			if added {
				_, _ = w.Write([]byte(`[{"id":7,"title":"The Matrix","tmdbId":603,"path":"/movies/The Matrix (1999)"}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case r.URL.Path == "/api/v3/movie" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["qualityProfileId"] != float64(4) || body["rootFolderPath"] != "/movies" || body["monitored"] != false || body["titleSlug"] != "the-matrix-603" {
				t.Errorf("add body: %v", body)
			}
			added = true
			_, _ = w.Write([]byte(`{"id":7,"title":"The Matrix","tmdbId":603,"path":"/movies/The Matrix (1999)"}`))
		case r.URL.Path == "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[{"id":1,"name":"Any"},{"id":4,"name":"HD-1080p"}]`))
		case r.URL.Path == "/api/v3/command" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "DownloadedMoviesScan" || body["path"] != "/data/incoming/The Matrix (1999)" || body["importMode"] != "Move" {
				t.Errorf("command body: %v", body)
			}
			_, _ = w.Write([]byte(`{"id":55,"status":"queued"}`))
		case r.URL.Path == "/api/v3/command/55":
			polls++
			st := "started"
			if polls >= 2 {
				st = "completed"
			}
			_, _ = w.Write([]byte(`{"id":55,"status":"` + st + `"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	return srv, &calls
}

func TestRadarrFlow(t *testing.T) {
	srv, calls := fakeRadarr(t)
	defer srv.Close()
	c := New(Radarr, config.ArrApp{Enabled: true, URL: srv.URL + "/", APIKey: "k", RootFolder: "/movies", QualityProfile: "HD-1080p", ImportMode: "move",
		PathMap: map[string]string{"/mnt/media/_incoming": "/data/incoming", "/mnt": "/wrong"}}, 5*time.Second, nil)
	c.Poll = 10 * time.Millisecond
	ctx := context.Background()
	items, err := c.Lookup(ctx, "The Matrix")
	if err != nil || len(items) != 2 || items[0].TMDBID != 603 || items[0].Runtime != 136*time.Minute {
		t.Fatalf("lookup: %v %+v", err, items)
	}
	if _, ok, err := c.InLibrary(ctx, items[0]); err != nil || ok {
		t.Fatalf("should not be in library: %v %v", ok, err)
	}
	added, err := c.Add(ctx, items[0])
	if err != nil || added.ID != 7 {
		t.Fatalf("add: %v %+v", err, added)
	}
	if lib, ok, _ := c.InLibrary(ctx, items[0]); !ok || lib.Path == "" {
		t.Fatal("should be in library after add")
	}
	if p := c.RemotePath("/mnt/media/_incoming/The Matrix (1999)"); p != "/data/incoming/The Matrix (1999)" {
		t.Fatalf("remote path: %s", p)
	}
	if p := c.RemotePath("/elsewhere/x"); p != "/elsewhere/x" {
		t.Fatalf("unmapped path: %s", p)
	}
	if err := c.Import(ctx, "/mnt/media/_incoming/The Matrix (1999)", time.Minute); err != nil {
		t.Fatalf("import: %v", err)
	}
	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "GET /api/v3/command/55") {
		t.Fatalf("did not poll command: %s", joined)
	}
}

func TestBadKey(t *testing.T) {
	srv, _ := fakeRadarr(t)
	defer srv.Close()
	c := New(Radarr, config.ArrApp{Enabled: true, URL: srv.URL, APIKey: "wrong", ImportMode: "move"}, time.Second, nil)
	if _, err := c.Lookup(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("got %v", err)
	}
	if New(Radarr, config.ArrApp{Enabled: false}, time.Second, nil) != nil {
		t.Fatal("disabled app should be nil")
	}
}

func TestSonarrEpisodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/series/lookup":
			_, _ = w.Write([]byte(`[{"title":"Friends","year":1994,"tvdbId":79168,"runtime":22,"seasons":[{"seasonNumber":0},{"seasonNumber":1,"statistics":{"totalEpisodeCount":24}}]}]`))
		case "/api/v3/episode":
			if r.URL.Query().Get("seriesId") != "3" {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`[{"seasonNumber":1,"episodeNumber":2,"title":"Two","runtime":22},{"seasonNumber":1,"episodeNumber":1,"title":"Pilot","runtime":23}]`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := New(Sonarr, config.ArrApp{Enabled: true, URL: srv.URL, APIKey: "k", ImportMode: "copy"}, time.Second, nil)
	items, err := c.Lookup(context.Background(), "Friends")
	if err != nil || len(items) != 1 || items[0].TVDBID != 79168 || len(items[0].Seasons) != 2 || items[0].Seasons[1].EpisodeCount != 24 {
		t.Fatalf("lookup: %v %+v", err, items)
	}
	eps, err := c.Episodes(context.Background(), 3)
	if err != nil || len(eps) != 2 || eps[0].Number != 1 || eps[0].Title != "Pilot" {
		t.Fatalf("episodes: %v %+v", err, eps)
	}
}
