package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/metadata"
)

// flaky stands in front of a fake app and answers 503 while fail says so.
func flaky(t *testing.T, target *httptest.Server, fail func(r *http.Request) bool) *httptest.Server {
	t.Helper()
	u, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail(r) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func radarrEnv(t *testing.T, fail func(r *http.Request) bool) (*env, string) {
	t.Helper()
	e := setup(t, movieInfo, fakeProvider{&metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Year: 1999, TMDBID: 603, Runtime: 136 * time.Minute, Confidence: 1, Source: "test"}})
	root := filepath.Join(e.out, "radarr-movies")
	real, _ := fakeRadarr(t, root, "/remote/library")
	t.Cleanup(real.Close)
	front := flaky(t, real, fail)
	e.cfg.Arr.Radarr = config.ArrApp{Enabled: true, URL: front.URL, APIKey: "k", RootFolder: root, AddMissing: true, ImportMode: "move",
		PathMap: map[string]string{e.out: "/remote/library"}}
	e.m.SetConfig(e.cfg)
	return e, root
}

// A busy app that times out on the first add does not need a person: the
// import is tried again within the same job.
func TestImportRetriedWhenAppIsBusy(t *testing.T) {
	var adds atomic.Int32
	e, root := radarrEnv(t, func(r *http.Request) bool {
		return r.Method == http.MethodPost && r.URL.Path == "/api/v3/movie" && adds.Add(1) == 1
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitDone(t)
	if importFailed(j) || len(j.Warnings) != 0 {
		t.Fatalf("import should have been retried: warnings=%v outputs=%+v", j.Warnings, j.Outputs)
	}
	if _, err := os.Stat(filepath.Join(root, "The Matrix (1999)", "The Matrix (1999).mkv")); err != nil {
		t.Fatalf("radarr did not receive the file: %v", err)
	}
	if adds.Load() < 2 {
		t.Fatalf("adds = %d", adds.Load())
	}
}

// An app that stays down past the quick retries is retried later in the
// background, and only on schedule.
func TestFailedImportRetriedInBackground(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	e, root := radarrEnv(t, func(*http.Request) bool { return down.Load() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitDone(t)
	e.waitFinalized(t, j.ID)
	if !importFailed(j) || !transientFailure(j) {
		t.Fatalf("import should have failed for a passing reason: %+v", j.Outputs)
	}
	tried := map[string]int{}
	down.Store(false)
	e.m.retryFailedImports(ctx, tried, j.FinishedAt.Add(time.Minute)) // not due yet
	if tried[j.ID] != 0 {
		t.Fatalf("retried before it was due: %v", tried)
	}
	e.m.retryFailedImports(ctx, tried, j.FinishedAt.Add(importRetryAfter[0]+time.Minute))
	r, ok := e.m.Record(j.ID)
	if !ok || importFailed(r) {
		t.Fatalf("after background retry: %+v", r.Outputs)
	}
	if _, err := os.Stat(filepath.Join(root, "The Matrix (1999)", "The Matrix (1999).mkv")); err != nil {
		t.Fatalf("radarr did not receive the file: %v", err)
	}
	// Done: nothing more to retry.
	e.m.retryFailedImports(ctx, tried, time.Now().Add(24*time.Hour))
	if tried[j.ID] != 1 {
		t.Fatalf("tries = %d", tried[j.ID])
	}
}

func TestTransientImport(t *testing.T) {
	for msg, want := range map[string]bool{
		`radarr: Post "https://radarr-rips.d.apjh.xyz/api/v3/movie": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`: true,
		"radarr: dial tcp 10.5.6.26:443: connect: connection refused":                                                                            true,
		"radarr: POST /movie: http 503: busy":                                                                                                    true,
		"radarr matched none of the files: Airplane! (1980).mkv (Not an upgrade for existing movie file.)":                                       false,
		"radarr: invalid api key": false,
	} {
		if got := transientImport(msg); got != want {
			t.Errorf("transientImport(%q) = %v", msg, got)
		}
	}
	if transientFailure(Job{Outputs: []Output{{Import: "not imported: http 503"}, {Import: "not imported: Not an upgrade"}}}) {
		t.Error("a refusal among the failures must not be retried")
	}
}
