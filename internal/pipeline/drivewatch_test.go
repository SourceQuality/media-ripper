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

	"github.com/sourcequality/media-ripper/internal/drive"
	"github.com/sourcequality/media-ripper/internal/notify"
)

func (d *fakeDrive) setGone(gone bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gone = gone
}

// eventLog collects what the JSON webhook receives.
type eventLog struct {
	mu     sync.Mutex
	events []notify.Event
}

func (l *eventLog) count(typ string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, ev := range l.events {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

func (l *eventLog) wait(t *testing.T, typ string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l.count(typ) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s event", typ)
}

func listen(t *testing.T, e *env) *eventLog {
	t.Helper()
	l := &eventLog{}
	hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var ev notify.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		l.mu.Lock()
		l.events = append(l.events, ev)
		l.mu.Unlock()
	}))
	t.Cleanup(hook.Close)
	e.m.deps.Notifier = &notify.Notifier{WebhookURL: hook.URL}
	e.m.SetConfig(e.cfg)
	return l
}

// dropOnTitle makes the rip of title 2 fail with the drive vanishing, as a
// USB reset does, and returns the ripped titles so far.
func dropOnTitle(t *testing.T, e *env) func() string {
	t.Helper()
	ripLog := filepath.Join(t.TempDir(), "rips.log")
	t.Setenv("FAKE_LOG", ripLog)
	t.Setenv("FAKE_FAIL_TITLE", "2")
	stop := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() { close(stop); <-done })
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			if data, _ := os.ReadFile(ripLog); strings.HasSuffix(string(data), "2\n") {
				e.drv.setGone(true)
				return
			}
		}
	}()
	return func() string {
		data, _ := os.ReadFile(ripLog)
		return strings.Join(strings.Fields(string(data)), ",")
	}
}

func TestDriveDropRunsTheJobAgain(t *testing.T) {
	e := setup(t, tvInfo, fakeProvider{friends()})
	l := listen(t, e)
	rips := dropOnTitle(t, e)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	l.wait(t, "started")
	var id string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h, _ := e.st.History(0)
		if len(h) > 0 {
			t.Fatalf("a drive drop was recorded as the end of the job: %s", h[0])
		}
		if st, err := e.drv.Status(); err != nil && st == drive.NoInfo {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	for _, d := range e.m.Snapshot().Drives {
		if d.Job != nil {
			id = d.Job.ID
		}
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d := e.m.Snapshot().Drives[0]; d.Job != nil && d.Job.Message == "waiting for the drive to come back" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h, _ := e.st.History(0); len(h) != 0 {
		t.Fatalf("recorded while paused: %s", h[0])
	}

	t.Setenv("FAKE_FAIL_TITLE", "")
	e.drv.setGone(false)
	j := e.waitJobStage(t, id, StageDone)
	if len(j.Outputs) != 2 {
		t.Fatalf("outputs = %+v", j.Outputs)
	}
	// Title 1 survived the drop; only the one in progress ran again.
	if got := rips(); strings.Count(got, "1") != 1 || strings.Count(got, "2") != 2 {
		t.Fatalf("ripped: %s", got)
	}
	if l.count("failed") != 0 {
		t.Fatal("told someone it failed")
	}
	counted := func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		_, ok := e.m.driveRetries["fp-friends-1"]
		return ok
	}
	e.waitFinalized(t, id) // the count goes when the job is recorded
	for deadline := time.Now().Add(2 * time.Second); counted() && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if counted() {
		t.Fatal("count kept after the disc came through")
	}
}

func TestDriveDropGivesUpAfterTheLimit(t *testing.T) {
	e := setup(t, tvInfo, fakeProvider{friends()})
	l := listen(t, e)
	e.m.driveRetries = map[string]int{"fp-friends-1": maxDriveRetries}
	dropOnTitle(t, e)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	j, _ := e.m.Record(e.waitDone(t).ID) // once the failure is final
	if j.Stage != StageFailed || !strings.Contains(j.Error, "given up") {
		t.Fatalf("stage %s err %q", j.Stage, j.Error)
	}
	l.wait(t, "failed")
}

func TestEjectRetriedWhenTheDriveIsBack(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{matrix()})
	l := listen(t, e)
	e.drv.mu.Lock()
	e.drv.ejectErrs = 2 // after the rip, and at the end of the job
	e.drv.mu.Unlock()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	if j := e.waitDone(t); j.Stage != StageDone {
		t.Fatalf("stage = %s", j.Stage)
	}
	e.waitEject(t, 1)
	l.wait(t, "ready")
	time.Sleep(50 * time.Millisecond)
	if n := e.drv.ejectCount(); n != 1 {
		t.Fatalf("ejects = %d", n)
	}
	if n := l.count("ready"); n != 1 {
		t.Fatalf("ready events = %d", n)
	}
}

func TestDriveLostAndBackAreReported(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{matrix()})
	l := listen(t, e)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go e.m.Run(ctx)
	e.drv.setGone(true)
	l.wait(t, "drive_lost")
	if l.count("drive_back") != 0 {
		t.Fatal("back while gone")
	}
	e.drv.setGone(false)
	l.wait(t, "drive_back")
}

// A job waits for another's copy, says so, and can be cancelled meanwhile.
func TestOneCopyAtATime(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{matrix()})
	e.m.deliverSlot <- struct{}{} // another job is copying
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d := e.m.Snapshot().Drives; len(d) > 0 && d[0].Job != nil && d[0].Job.Message == "waiting for another copy to finish" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	d := e.m.Snapshot().Drives[0]
	if d.Job == nil || d.Job.Message != "waiting for another copy to finish" {
		t.Fatalf("job = %+v", d.Job)
	}
	<-e.m.deliverSlot
	if j := e.waitDone(t); j.Stage != StageDone {
		t.Fatalf("stage = %s err %s", j.Stage, j.Error)
	}
}

func TestWaitingForACopyCanBeCancelled(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{matrix()})
	e.m.deliverSlot <- struct{}{}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	deadline := time.Now().Add(5 * time.Second)
	var id string
	for time.Now().Before(deadline) && id == "" {
		if d := e.m.Snapshot().Drives; len(d) > 0 && d[0].Job != nil && d[0].Job.Message == "waiting for another copy to finish" {
			id = d[0].Job.ID
		}
		time.Sleep(5 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("never waited")
	}
	if err := e.m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	e.waitJobStage(t, id, StageCancelled)
}

func TestCopyLeavesLittleInTheCache(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "x"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := &syncingWriter{f: f}
	chunk := make([]byte, 4<<20)
	for i := 0; i < 17; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if w.dirty >= copySyncEvery || w.dirty != 4<<20 {
		t.Fatalf("dirty = %d", w.dirty)
	}
}

func TestStoppedCopyLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.mkv")
	if err := os.WriteFile(src, make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.Mkdir(filepath.Join(other, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(other, "sub", "b.mkv")
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		t.Fatal(err)
	}
	abandon(f, tmp)
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("part left: %v", err)
	}
}

// The pause is on disk as soon as the service is told to stop, before a slow
// share can hold anything up.
func TestPauseSavedBeforeStopping(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{matrix()})
	job := newJob("/dev/fake0", e.m.rt.Load())
	job.Fingerprint, job.Stage = "fp-matrix", StageDelivering
	ws := workDir(e.cfg, job)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	rs := openResume(ws, job, true)
	if err := rs.begin(job.ID, job.StartedAt); err != nil {
		t.Fatal(err)
	}
	e.m.mu.Lock()
	e.m.runners = []*runner{{m: e.m, job: job}}
	e.m.mu.Unlock()
	e.m.savePausedJobs()
	if st := readResume(t, ws); !st.Interrupted || st.JobID != job.ID {
		t.Fatalf("resume = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(ws, jobFile)); err != nil {
		t.Fatal(err)
	}
}
