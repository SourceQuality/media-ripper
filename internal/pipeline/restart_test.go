package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/metadata"
)

func friends() *metadata.Identity {
	return &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TMDBID: 1668, EpisodeRuntime: 22 * time.Minute, Confidence: 1, Source: "test",
		Episodes: []metadata.Episode{{Number: 1, Title: "Pilot"}, {Number: 2, Title: "The One with the Sonogram"}}}
}

func readResume(t *testing.T, dir string) resumeState {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, resumeFile))
	if err != nil {
		t.Fatal(err)
	}
	var st resumeState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// A restart mid-rip is a pause, not a cancel: nothing is recorded, the
// workspace is kept, and the next start carries on under the same job id,
// reusing the titles already ripped. Only the title in progress is ripped
// again.
func TestRestartMidRipCarriesOnUnderSameID(t *testing.T) {
	e := setup(t, tvInfo, fakeProvider{friends()})
	ripLog := filepath.Join(t.TempDir(), "rips.log")
	t.Setenv("FAKE_LOG", ripLog)
	t.Setenv("FAKE_SLOW", "1")
	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { _ = e.m.Run(ctx); close(stopped) }()
	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")

	// Stop the service while title 2 rips.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, _ := os.ReadFile(ripLog); strings.Join(strings.Fields(string(data)), ",") == "1,2" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	first := e.m.Snapshot().Drives[0].Job
	if first == nil {
		t.Fatal("no job running")
	}
	stop()
	<-stopped
	if h, _ := e.st.History(0); len(h) != 0 {
		t.Fatalf("a restart was recorded: %s", h)
	}
	ws := filepath.Join(e.cfg.RipDir(), "fp-friends-1")
	if st := readResume(t, ws); !st.Interrupted || st.JobID != first.ID {
		t.Fatalf("resume state = %+v, want interrupted %s", st, first.ID)
	}
	if e.drv.ejectCount() != 0 {
		t.Fatal("a restart ejected the disc")
	}

	// The service starts again with the disc still in.
	t.Setenv("FAKE_SLOW", "")
	e.m = New(e.m.deps)
	ctx2, stop2 := context.WithCancel(context.Background())
	defer stop2()
	go e.m.Run(ctx2)
	j := e.waitDone(t)
	if j.ID != first.ID || j.Stage != StageDone || len(j.Outputs) != 2 {
		t.Fatalf("carried on as %s (want %s): stage=%s outputs=%d err=%s", j.ID, first.ID, j.Stage, len(j.Outputs), j.Error)
	}
	if !j.StartedAt.Equal(first.StartedAt) {
		t.Fatalf("started %v, want the first attempt's %v", j.StartedAt, first.StartedAt)
	}
	if data, _ := os.ReadFile(ripLog); strings.Join(strings.Fields(string(data)), ",") != "1,2,2" {
		t.Fatalf("rips = %q, want title 1 once and title 2 again", data)
	}
	if h, _ := e.st.History(0); len(h) != 1 {
		t.Fatalf("history has %d records, want 1", len(h))
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatalf("workspace not removed after success: %v", err)
	}
}

// A restart early on, before any title is ripped, keeps the workspace too:
// it is no longer mistaken for a stale one.
func TestRestartBeforeFirstTitleKeepsWorkspace(t *testing.T) {
	e := setup(t, tvInfo, fakeProvider{friends()})
	ripLog := filepath.Join(t.TempDir(), "rips.log")
	t.Setenv("FAKE_LOG", ripLog)
	t.Setenv("FAKE_SLOW", "2")
	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { _ = e.m.Run(ctx); close(stopped) }()
	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, _ := os.ReadFile(ripLog); len(data) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	<-stopped
	e.m = New(e.m.deps)
	e.m.cleanWorkspace()
	if st := readResume(t, filepath.Join(e.cfg.RipDir(), "fp-friends-1")); !st.Interrupted {
		t.Fatalf("resume state = %+v", st)
	}
}

// A restart after the disc was ripped and ejected (while copying) is
// finished at the next start without the disc.
func TestPausedAfterEjectFinishesWithoutDisc(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{&metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Year: 1999, TMDBID: 603, Runtime: 136 * time.Minute, Confidence: 1, Source: "test"}})
	e.drv.insert("fp-matrix", "THE_MATRIX")
	job, err := e.m.ScanOnly(context.Background(), "/dev/fake0")
	if err != nil || job.Selection == nil || len(job.Selection.Picks) != 1 {
		t.Fatalf("scan: %v %+v", err, job)
	}
	_ = e.drv.Eject()

	// What the service left behind: the title ripped into the workspace,
	// the job paused after the eject.
	ws := filepath.Join(e.cfg.RipDir(), "fp-matrix")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	ripped := filepath.Join(ws, "Disc_t00.mkv")
	if err := os.WriteFile(ripped, []byte("movie"), 0o644); err != nil {
		t.Fatal(err)
	}
	job.ID, job.Fingerprint, job.Ejected, job.DryRun = "20261004-120000-001", "fp-matrix", true, false
	pick := job.Selection.Picks[0]
	rs := openResume(ws, job, true)
	if err := rs.begin(job.ID, job.StartedAt); err != nil {
		t.Fatal(err)
	}
	if err := rs.markRipped(pick.Title.ID, ripped); err != nil {
		t.Fatal(err)
	}
	if err := markInterrupted(ws, job.Snapshot()); err != nil {
		t.Fatal(err)
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go e.m.Run(ctx)
	e.waitFinalized(t, job.ID)
	j, ok := e.m.Record(job.ID)
	if !ok || j.Stage != StageDone || len(j.Outputs) != 1 {
		t.Fatalf("finished: ok=%v stage=%s outputs=%+v err=%s", ok, j.Stage, j.Outputs, j.Error)
	}
	if data, err := os.ReadFile(j.Outputs[0].Path); err != nil || string(data) != "movie" {
		t.Fatalf("delivered file: %q %v", data, err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatalf("workspace not removed: %v", err)
	}
	if e.drv.ejectCount() != 1 {
		t.Fatalf("ejects = %d", e.drv.ejectCount())
	}
}
