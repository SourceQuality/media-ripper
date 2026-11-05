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

	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/notify"
)

func matrix() *metadata.Identity {
	return &metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Year: 1999, TMDBID: 603, Runtime: 136 * time.Minute, Confidence: 1, Source: "test"}
}

// failedCopy runs a disc whose copy to the library fails, as when the share
// is not mounted, and returns the failed job and how to unblock the library.
func failedCopy(t *testing.T, e *env) (Job, func()) {
	t.Helper()
	out := e.cfg.Output.Path
	if err := os.RemoveAll(out); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, nil, 0o644); err != nil { // mkdir under it fails
		t.Fatal(err)
	}
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitDone(t)
	if j.Stage != StageFailed {
		t.Fatalf("stage = %s, want failed", j.Stage)
	}
	e.waitFinalized(t, j.ID)
	return j, func() {
		if err := os.Remove(out); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(out, 0o775); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) waitJobStage(t *testing.T, id string, want Stage) Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var j Job
	for time.Now().Before(deadline) {
		var ok bool
		if j, ok = e.m.Record(id); ok && j.Stage == want && !e.m.finishingDisc(j.Fingerprint) {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s stage = %s, want %s (err %q)", id, j.Stage, want, j.Error)
	return j
}

func assertDelivered(t *testing.T, e *env, j Job) {
	t.Helper()
	if len(j.Outputs) != 1 {
		t.Fatalf("outputs = %+v", j.Outputs)
	}
	if _, err := os.Stat(j.Outputs[0].Path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.RipDir(), "fp-matrix")); !os.IsNotExist(err) {
		t.Fatalf("workspace not removed: %v", err)
	}
}

// A copy that fails needs a retry, not the disc.
func TestRetryDeliveryFinishesWithoutDisc(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{matrix()})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go e.m.Run(ctx)
	j, unblock := failedCopy(t, e)
	e.waitEject(t, 1)
	ejects := e.drv.ejectCount()
	if !e.m.CanRetryDelivery(j) {
		t.Fatal("a complete rip should be deliverable")
	}
	unblock()
	if _, err := e.m.RetryDelivery(j.ID); err != nil {
		t.Fatal(err)
	}
	done := e.waitJobStage(t, j.ID, StageDone)
	assertDelivered(t, e, done)
	if done.Error != "" {
		t.Fatalf("old error kept: %q", done.Error)
	}
	if e.drv.ejectCount() != ejects {
		t.Fatalf("ejected again: %d, was %d", e.drv.ejectCount(), ejects)
	}
	if _, err := e.m.RetryDelivery(j.ID); err == nil {
		t.Fatal("a delivered job retried again")
	}
}

// After a restart the job is only in the history; that must be enough.
func TestRetryDeliveryAfterRestart(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{matrix()})
	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { _ = e.m.Run(ctx); close(stopped) }()
	j, unblock := failedCopy(t, e)
	stop()
	<-stopped

	unblock()
	fresh := New(Deps{Config: e.cfg, Store: e.st, Notifier: e.m.deps.Notifier, Logger: e.m.log})
	e.m = fresh
	ctx2, stop2 := context.WithCancel(context.Background())
	defer stop2()
	go fresh.Run(ctx2)
	// The first storage probe finds the library usable and picks it up.
	assertDelivered(t, e, e.waitJobStage(t, j.ID, StageDone))
}

// When the share comes back, the job finishes by itself.
func TestDeliveryRetriedWhenStorageRecovers(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{matrix()})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go e.m.Run(ctx)
	j, unblock := failedCopy(t, e)
	e.m.setStorage(StorageStatus{State: "error", Path: e.cfg.Output.Path, Error: "gone"})
	if st, _ := e.m.Record(j.ID); st.Stage != StageFailed {
		t.Fatalf("stage = %s while the share is down", st.Stage)
	}
	unblock()
	e.m.setStorage(StorageStatus{State: "ok", Path: e.cfg.Output.Path})
	assertDelivered(t, e, e.waitJobStage(t, j.ID, StageDone))
}

// A title that was never ripped does need the disc, and the error says so.
func TestRetryDeliveryNamesWhatIsMissing(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{matrix()})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go e.m.Run(ctx)
	j, unblock := failedCopy(t, e)
	defer unblock()
	ws := filepath.Join(e.cfg.RipDir(), "fp-matrix")
	st := readResume(t, ws)
	for _, tt := range st.Titles {
		if err := os.Remove(tt.Ripped); err != nil {
			t.Fatal(err)
		}
	}
	if e.m.CanRetryDelivery(j) {
		t.Fatal("offered a retry without the rip")
	}
	if _, err := e.m.RetryDelivery(j.ID); err == nil || !strings.Contains(err.Error(), "insert the disc") {
		t.Fatalf("err = %v", err)
	}
}

func TestFailedCopyOffersRetryAndAnnouncesEject(t *testing.T) {
	var mu sync.Mutex
	var events []notify.Event
	hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var ev notify.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}))
	defer hook.Close()
	e := setup(t, movieInfo, fakeProvider{matrix()})
	e.m.deps.Notifier = &notify.Notifier{WebhookURL: hook.URL}
	e.m.SetConfig(e.cfg)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go e.m.Run(ctx)
	_, unblock := failedCopy(t, e)
	defer unblock()
	deadline := time.Now().Add(5 * time.Second)
	var failed, ready *notify.Event
	for time.Now().Before(deadline) && (failed == nil || ready == nil) {
		mu.Lock()
		for i := range events {
			switch events[i].Type {
			case "failed":
				failed = &events[i]
			case "ready":
				ready = &events[i]
			}
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	if failed == nil || !failed.RetryDelivery {
		t.Fatalf("failed event = %+v", failed)
	}
	if ready == nil {
		t.Fatal("no ready event when the tray opened")
	}
}

// A disc that stays in until the end announces the eject with how it went.
func TestEjectAtTheEndSaysHowItWent(t *testing.T) {
	var mu sync.Mutex
	var ready []notify.Event
	hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var ev notify.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		if ev.Type == "ready" {
			mu.Lock()
			ready = append(ready, ev)
			mu.Unlock()
		}
	}))
	defer hook.Close()
	e := setup(t, movieInfo, fakeProvider{matrix()})
	e.cfg.Eject.AfterRip = false
	e.m.deps.Notifier = &notify.Notifier{WebhookURL: hook.URL}
	e.m.SetConfig(e.cfg)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go e.m.Run(ctx)
	_, unblock := failedCopy(t, e)
	defer unblock()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(ready)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ready) != 1 || !strings.Contains(ready[0].Summary, "failed and the tray is open") {
		t.Fatalf("ready = %+v", ready)
	}
}

func TestExpiringRipWarnsOnce(t *testing.T) {
	var mu sync.Mutex
	var events []notify.Event
	hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var ev notify.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}))
	defer hook.Close()
	e := setup(t, movieInfo, fakeProvider{matrix()})
	e.m.deps.Notifier = &notify.Notifier{WebhookURL: hook.URL}
	e.m.SetConfig(e.cfg)
	ws := filepath.Join(e.cfg.RipDir(), "fp-old")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	st := resumeState{Fingerprint: "fp-old", Label: "OLD", JobID: "j-old", Updated: time.Now(), Titles: map[int]*resumedTitle{0: {Ripped: filepath.Join(ws, "a.mkv")}}}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(ws, resumeFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	warned := map[string]bool{}
	maxAge := e.cfg.Output.ResumeMaxAge.D()
	e.m.warnExpiringRips(context.Background(), warned, st.Updated.Add(time.Hour)) // plenty of time left
	if len(warned) != 0 {
		t.Fatalf("warned early: %v", warned)
	}
	late := st.Updated.Add(maxAge - time.Hour)
	e.m.warnExpiringRips(context.Background(), warned, late)
	e.m.warnExpiringRips(context.Background(), warned, late)
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0].Type != "expiring" || !strings.Contains(events[0].Summary, "never reached the library") {
		t.Fatalf("events = %+v", events)
	}
}
