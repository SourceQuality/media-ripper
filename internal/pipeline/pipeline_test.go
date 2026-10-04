package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/drive"
	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/notify"
	"github.com/sourcequality/media-ripper/internal/store"
)

// fakeDrive simulates a tray: tests insert a disc, the pipeline ejects it.
type fakeDrive struct {
	mu      sync.Mutex
	path    string
	status  drive.Status
	fp      string
	label   string
	ejected int
	locked  bool
}

func (d *fakeDrive) Path() string { return d.path }
func (d *fakeDrive) Status() (drive.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status, nil
}
func (d *fakeDrive) Eject() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ejected++
	d.status = drive.TrayOpen
	return nil
}
func (d *fakeDrive) CloseTray() error { return nil }
func (d *fakeDrive) Lock(l bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.locked = l
	return nil
}
func (d *fakeDrive) Fingerprint() (string, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fp, d.label, nil
}
func (d *fakeDrive) insert(fp, label string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fp, d.label, d.status = fp, label, drive.DiscOK
}
func (d *fakeDrive) ejectCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ejected
}

// fakeMakemkv is a shell script standing in for makemkvcon. It answers
// "info" with canned robot output and "mkv" by writing a file.
const fakeMakemkv = `#!/bin/sh
# args: -r --progress=-same --messages=-stdout --minlength=N info|mkv dev:/dev/x [title] [outdir]
cmd=""
for a in "$@"; do
  case "$a" in
    info|mkv) cmd=$a ;;
  esac
done
if [ "$cmd" = info ]; then
  cat "$FAKE_INFO"
  exit 0
fi
# mkv: last two args are title id and outdir
title=$(eval echo \${$(($#-1))})
outdir=$(eval echo \${$#})
[ -n "$FAKE_LOG" ] && echo "$title" >> "$FAKE_LOG"
if [ "$title" = "$FAKE_FAIL_TITLE" ]; then
  echo 'MSG:5003,0,0,"Read error","Read error"'
  exit 1
fi
mkdir -p "$outdir"
echo 'PRGC:5017,0,"Saving to MKV file"'
echo 'PRGV:0,0,65536'
echo 'PRGV:32768,32768,65536'
[ -n "$FAKE_SLOW" ] && sleep "$FAKE_SLOW"
head -c 100000 /dev/zero > "$outdir/Disc_t0${title}.mkv"
echo 'PRGV:65536,65536,65536'
echo 'MSG:5004,0,2,"Copy complete. 1 titles saved, 0 failed.","Copy complete. %1 titles saved, %2 failed.","1","0"'
exit 0
`

const movieInfo = `TCOUNT:3
CINFO:1,6209,"Blu-ray disc"
CINFO:2,0,"THE_MATRIX"
CINFO:32,0,"THE_MATRIX"
TINFO:0,8,0,"24"
TINFO:0,9,0,"2:16:18"
TINFO:0,10,0,"32.5 GB"
TINFO:0,26,0,"1,2,3"
TINFO:0,27,0,"Disc_t00.mkv"
SINFO:0,0,1,6201,"Video"
SINFO:0,0,19,0,"1920x1080"
SINFO:0,1,1,6202,"Audio"
SINFO:0,1,3,0,"eng"
TINFO:1,8,0,"2"
TINFO:1,9,0,"2:16:18"
TINFO:1,26,0,"3,2,1"
TINFO:1,27,0,"Disc_t01.mkv"
TINFO:2,8,0,"1"
TINFO:2,9,0,"0:02:11"
TINFO:2,26,0,"9"
TINFO:2,27,0,"Disc_t02.mkv"
`

const tvInfo = `TCOUNT:4
CINFO:1,6206,"DVD disc"
CINFO:32,0,"FRIENDS_S1_D1"
TINFO:0,8,0,"8"
TINFO:0,9,0,"1:28:00"
TINFO:0,26,0,"1,2,3,4"
TINFO:0,27,0,"Disc_t00.mkv"
TINFO:1,8,0,"4"
TINFO:1,9,0,"0:22:10"
TINFO:1,26,0,"1,2"
TINFO:1,27,0,"Disc_t01.mkv"
TINFO:2,8,0,"4"
TINFO:2,9,0,"0:21:50"
TINFO:2,26,0,"3,4"
TINFO:2,27,0,"Disc_t02.mkv"
TINFO:3,8,0,"1"
TINFO:3,9,0,"0:03:00"
TINFO:3,26,0,"9"
TINFO:3,27,0,"Disc_t03.mkv"
`

type fakeProvider struct{ id *metadata.Identity }

func (p fakeProvider) Identify(_ context.Context, h metadata.Hint, _ metadata.DiscHints) (*metadata.Identity, error) {
	id := *p.id
	id.Hint = h
	if id.Season == 0 {
		id.Season = h.Season
	}
	id.Disc = h.Disc
	return &id, nil
}

type env struct {
	cfg  *config.Config
	st   *store.Store
	drv  *fakeDrive
	m    *Manager
	info string
	out  string
}

func setup(t *testing.T, infoText string, provider metadata.Provider) *env {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "makemkvcon")
	if err := os.WriteFile(bin, []byte(fakeMakemkv), 0o755); err != nil {
		t.Fatal(err)
	}
	info := filepath.Join(root, "info.txt")
	if err := os.WriteFile(info, []byte(infoText), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_INFO", info)

	cfg := config.Default()
	cfg.Output.Path = filepath.Join(root, "library")
	cfg.Workspace = filepath.Join(root, "work")
	cfg.Drives = []string{"/dev/fake0"}
	cfg.MakeMKV.Binary = bin
	cfg.MakeMKV.Retries = 0
	cfg.PostProcess.Mode = "none"
	cfg.Metadata.Provider = "none"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.PollInterval = config.Duration(20 * time.Millisecond) // below the validated minimum, fine for tests
	st, err := store.Open(cfg.StateDir())
	if err != nil {
		t.Fatal(err)
	}
	drv := &fakeDrive{path: "/dev/fake0", status: drive.NoDisc}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	m := New(Deps{
		Config:    &cfg,
		MakeMKV:   &makemkv.Client{Binary: bin, Logger: log},
		Metadata:  provider,
		Store:     st,
		Notifier:  &notify.Notifier{},
		Logger:    log,
		OpenDrive: func(string) (drive.Drive, error) { return drv, nil },
	})
	return &env{cfg: &cfg, st: st, drv: drv, m: m, info: info, out: cfg.Output.Path}
}

func (e *env) waitDone(t *testing.T) Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snap := e.m.Snapshot()
		for _, j := range snap.Recent {
			if j.Stage.Terminal() {
				return j
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job did not finish: %+v", e.m.Snapshot())
	return Job{}
}

func (e *env) waitEject(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.drv.ejectCount() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d ejects, got %d", n, e.drv.ejectCount())
}

func TestMovieEndToEnd(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{&metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Year: 1999, TMDBID: 603, Runtime: 136 * time.Minute, Confidence: 1, Source: "test"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)

	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitDone(t)
	if j.Stage != StageDone {
		t.Fatalf("stage = %s err=%s log=%v", j.Stage, j.Error, j.Log)
	}
	want := filepath.Join(e.out, "Movies", "The Matrix (1999)", "The Matrix (1999).mkv")
	if len(j.Outputs) != 1 || j.Outputs[0].Path != want {
		t.Fatalf("outputs = %+v", j.Outputs)
	}
	if st, err := os.Stat(want); err != nil || st.Size() != 100000 {
		t.Fatalf("delivered file: %v", err)
	}
	if j.Selection.Picks[0].Title.ID != 0 {
		t.Fatalf("picked title %d", j.Selection.Picks[0].Title.ID)
	}
	e.waitEject(t, 1)
	if _, ok := e.st.Disc("fp-matrix"); !ok {
		t.Fatal("disc not recorded")
	}
	if entries, _ := os.ReadDir(e.cfg.RipDir()); len(entries) != 0 {
		t.Fatalf("workspace not cleaned: %v", entries)
	}
	h, _ := e.st.History(10)
	if len(h) != 1 {
		t.Fatalf("history = %d", len(h))
	}
	var rec map[string]any
	_ = json.Unmarshal(h[0], &rec)
	if rec["title"] != "The Matrix (1999)" || rec["stage"] != "done" {
		t.Fatalf("history record = %v", rec)
	}

	// Same disc re-inserted after eject: skipped and ejected again, no new job.
	e.drv.insert("fp-matrix", "THE_MATRIX")
	e.waitEject(t, 2)
	if n := len(e.m.Snapshot().Recent); n != 1 {
		t.Fatalf("re-ripped an already ripped disc: %d jobs", n)
	}

	// Rescan forces it.
	e.drv.insert("fp-matrix", "THE_MATRIX")
	if err := e.m.Rescan("/dev/fake0"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(e.m.Snapshot().Recent) < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	snap := e.m.Snapshot()
	if len(snap.Recent) != 2 {
		t.Fatalf("rescan did not start a job")
	}
	for time.Now().Before(deadline) && !snap.Recent[0].Stage.Terminal() {
		time.Sleep(20 * time.Millisecond)
		snap = e.m.Snapshot()
	}
	if snap.Recent[0].Stage != StageDone || !strings.HasSuffix(snap.Recent[0].Outputs[0].Path, "The Matrix (1999) (2).mkv") {
		t.Fatalf("rescan job: %+v", snap.Recent[0])
	}
}

func TestTVEndToEndWithEpisodeProgress(t *testing.T) {
	id := &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TMDBID: 1668, EpisodeRuntime: 22 * time.Minute, Confidence: 1, Source: "test",
		Episodes: []metadata.Episode{{Number: 1, Title: "Pilot"}, {Number: 2, Title: "The One with the Sonogram"}, {Number: 3, Title: "Three"}, {Number: 4, Title: "Four"}}}
	e := setup(t, tvInfo, fakeProvider{id})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)

	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	if j.Stage != StageDone {
		t.Fatalf("stage = %s err=%s", j.Stage, j.Error)
	}
	if len(j.Outputs) != 2 {
		t.Fatalf("outputs = %+v", j.Outputs)
	}
	want1 := filepath.Join(e.out, "TV Shows", "Friends (1994)", "Season 01", "Friends - S01E01 - Pilot.mkv")
	want2 := filepath.Join(e.out, "TV Shows", "Friends (1994)", "Season 01", "Friends - S01E02 - The One with the Sonogram.mkv")
	if j.Outputs[0].Path != want1 || j.Outputs[1].Path != want2 {
		t.Fatalf("paths = %s, %s", j.Outputs[0].Path, j.Outputs[1].Path)
	}
	if next := e.st.NextEpisode("tmdb:1668", 1); next != 3 {
		t.Fatalf("next episode = %d", next)
	}

	// Disc 2 continues at episode 3.
	if err := os.WriteFile(e.info, []byte(strings.Replace(tvInfo, "FRIENDS_S1_D1", "FRIENDS_S1_D2", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	e.drv.insert("fp-friends-2", "FRIENDS_S1_D2")
	deadline := time.Now().Add(10 * time.Second)
	var j2 Job
	for time.Now().Before(deadline) {
		snap := e.m.Snapshot()
		if len(snap.Recent) == 2 && snap.Recent[0].Stage.Terminal() {
			j2 = snap.Recent[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if j2.Stage != StageDone || len(j2.Outputs) != 2 || !strings.HasSuffix(j2.Outputs[0].Path, "Friends - S01E03 - Three.mkv") {
		t.Fatalf("disc 2: %+v", j2)
	}
}

func TestUnidentifiedDisc(t *testing.T) {
	e := setup(t, strings.ReplaceAll(movieInfo, "THE_MATRIX", "BD_ROM"), metadata.NoneProvider{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-unknown", "BD_ROM")
	j := e.waitDone(t)
	if j.Stage != StageDone || len(j.Outputs) != 1 {
		t.Fatalf("job = %+v", j)
	}
	if !strings.Contains(j.Outputs[0].Path, filepath.Join("_unidentified", "BD_ROM ")) || !strings.HasSuffix(j.Outputs[0].Path, "BD_ROM - t00.mkv") {
		t.Fatalf("path = %s", j.Outputs[0].Path)
	}
}

func TestScanFailureEjectsAndRecords(t *testing.T) {
	e := setup(t, "MSG:5010,0,1,\"Failed to open disc\",\"Failed to open disc\"\n", metadata.NoneProvider{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-bad", "BAD")
	j := e.waitDone(t)
	if j.Stage != StageFailed || !strings.Contains(j.Error, "Failed to open disc") {
		t.Fatalf("job = %+v", j)
	}
	e.waitEject(t, 1)
	if _, ok := e.st.Disc("fp-bad"); ok {
		t.Fatal("failed disc must not be recorded as ripped")
	}
}

func TestScanOnlyDryRun(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{&metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Year: 1999, Runtime: 136 * time.Minute}})
	e.drv.insert("fp", "THE_MATRIX")
	job, err := e.m.ScanOnly(context.Background(), "/dev/fake0")
	if err != nil {
		t.Fatal(err)
	}
	s := job.Snapshot()
	if !s.DryRun || s.Selection == nil || len(s.Selection.Picks) != 1 || len(s.Outputs) != 0 {
		t.Fatalf("dry run = %+v", s)
	}
	if e.drv.ejectCount() != 0 {
		t.Fatal("dry run must not eject")
	}
	found := false
	for _, l := range s.Log {
		if strings.Contains(l.Message, "pick title 0") {
			found = true
		}
	}
	if !found {
		t.Fatalf("log = %v", s.Log)
	}
}

func TestMoveFileAcrossCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.mkv")
	if err := os.WriteFile(src, []byte(strings.Repeat("x", 5000)), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "sub", "b.mkv")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	n, err := moveFile(context.Background(), src, dst, 0o644)
	if err != nil || n != 5000 {
		t.Fatalf("move: %d %v", n, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("source should be gone")
	}
	if _, err := os.Stat(fmt.Sprintf("%s.part", dst)); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
}

func TestProgressLogRestartsPerPhase(t *testing.T) {
	var l progressLog
	steps := []struct {
		p    makemkv.Progress
		want bool
	}{
		{makemkv.Progress{Task: "Analyzing seamless segments", Percent: -1}, false},
		{makemkv.Progress{Percent: 0}, true},
		{makemkv.Progress{Percent: 5}, false},
		{makemkv.Progress{Percent: 100}, true},
		{makemkv.Progress{Task: "Saving to MKV file", Percent: -1}, false},
		{makemkv.Progress{Percent: 0.4}, true},
		{makemkv.Progress{Percent: 9}, false},
		{makemkv.Progress{Percent: 10.5}, true},
		{makemkv.Progress{Percent: 25}, true},
		{makemkv.Progress{Percent: 3}, true}, // drop without a task line
	}
	for i, s := range steps {
		if got := l.due(s.p); got != s.want {
			t.Errorf("step %d (%+v): due = %v, want %v", i, s.p, got, s.want)
		}
	}
}

// A cancel from the web UI leaves the disc in so it can be rescanned after a
// settings change; only failures eject.
func TestCancelKeepsDiscIn(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{&metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Year: 1999, Runtime: 136 * time.Minute, Confidence: 1, Source: "test"}})
	t.Setenv("FAKE_SLOW", "2")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)

	e.drv.insert("fp-matrix", "THE_MATRIX")
	deadline := time.Now().Add(5 * time.Second)
	var id string
	for time.Now().Before(deadline) && id == "" {
		for _, d := range e.m.Snapshot().Drives {
			if d.Job != nil && d.Job.Stage == StageRipping {
				id = d.Job.ID
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if id == "" {
		t.Fatalf("job never started ripping: %+v", e.m.Snapshot())
	}
	if err := e.m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	if j := e.waitDone(t); j.Stage != StageCancelled {
		t.Fatalf("stage = %s", j.Stage)
	}
	time.Sleep(200 * time.Millisecond)
	if n := e.drv.ejectCount(); n != 0 {
		t.Fatalf("cancel ejected the disc (%d ejects)", n)
	}
}

// An identified disc's first episode is delivered while the second still rips.
func TestDeliversWhileRipping(t *testing.T) {
	id := &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TMDBID: 1668, EpisodeRuntime: 22 * time.Minute, Confidence: 1, Source: "test",
		Episodes: []metadata.Episode{{Number: 1, Title: "Pilot"}, {Number: 2, Title: "The One with the Sonogram"}}}
	e := setup(t, tvInfo, fakeProvider{id})
	t.Setenv("FAKE_SLOW", "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)

	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	first := filepath.Join(e.out, "TV Shows", "Friends (1994)", "Season 01", "Friends - S01E01 - Pilot.mkv")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(first); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var during Job
	for _, d := range e.m.Snapshot().Drives {
		if d.Job != nil {
			during = *d.Job
		}
	}
	if during.Stage != StageRipping || during.Current != 2 {
		t.Fatalf("episode 1 not delivered during the second rip: stage=%s current=%d", during.Stage, during.Current)
	}
	j := e.waitDone(t)
	if j.Stage != StageDone || len(j.Outputs) != 2 {
		t.Fatalf("stage = %s outputs = %+v err=%s", j.Stage, j.Outputs, j.Error)
	}
	if next := e.st.NextEpisode("tmdb:1668", 1); next != 3 {
		t.Fatalf("next episode = %d", next)
	}
	if entries, _ := os.ReadDir(e.cfg.RipDir()); len(entries) != 0 {
		t.Fatalf("workspace not cleaned: %v", entries)
	}
}

// A delivery that fails in the background fails the job and does not
// advance the season, even though ripping had moved on.
func TestBackgroundDeliveryFailure(t *testing.T) {
	id := &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TMDBID: 1668, EpisodeRuntime: 22 * time.Minute, Confidence: 1, Source: "test"}
	e := setup(t, tvInfo, fakeProvider{id})
	t.Setenv("FAKE_SLOW", "1")
	// A file where the show folder should go makes every delivery fail.
	if err := os.MkdirAll(filepath.Join(e.out, "TV Shows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.out, "TV Shows", "Friends (1994)"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)

	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	if j.Stage != StageFailed || j.Error == "" {
		t.Fatalf("stage = %s err=%q", j.Stage, j.Error)
	}
	if next := e.st.NextEpisode("tmdb:1668", 1); next > 1 {
		t.Fatalf("season advanced to %d after a failed disc", next)
	}
	if _, ok := e.st.Disc("fp-friends-1"); ok {
		t.Fatal("failed disc recorded as ripped")
	}
}

// A disc that fails part-way is resumed on reinsert: titles already
// delivered are neither ripped nor delivered again, so no "(2)" copies.
func TestResumeAfterFailedRip(t *testing.T) {
	id := &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TMDBID: 1668, EpisodeRuntime: 22 * time.Minute, Confidence: 1, Source: "test",
		Episodes: []metadata.Episode{{Number: 1, Title: "Pilot"}, {Number: 2, Title: "The One with the Sonogram"}}}
	e := setup(t, tvInfo, fakeProvider{id})
	ripLog := filepath.Join(t.TempDir(), "rips.log")
	t.Setenv("FAKE_LOG", ripLog)
	t.Setenv("FAKE_FAIL_TITLE", "2")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)

	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	if j := e.waitDone(t); j.Stage != StageFailed {
		t.Fatalf("first attempt: stage = %s", j.Stage)
	}
	ep1 := filepath.Join(e.out, "TV Shows", "Friends (1994)", "Season 01", "Friends - S01E01 - Pilot.mkv")
	if _, err := os.Stat(ep1); err != nil {
		t.Fatalf("episode 1 should be delivered before the failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.RipDir(), "fp-friends-1", resumeFile)); err != nil {
		t.Fatalf("workspace not kept for resume: %v", err)
	}

	// The read error clears (a cleaned disc, a better drive); reinsert.
	t.Setenv("FAKE_FAIL_TITLE", "")
	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	deadline := time.Now().Add(10 * time.Second)
	var j Job
	for time.Now().Before(deadline) {
		snap := e.m.Snapshot()
		if len(snap.Recent) == 2 && snap.Recent[0].Stage.Terminal() {
			j = snap.Recent[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if j.Stage != StageDone || len(j.Outputs) != 2 {
		t.Fatalf("resumed job: stage=%s outputs=%+v err=%s", j.Stage, j.Outputs, j.Error)
	}
	for _, o := range j.Outputs {
		if strings.Contains(o.Path, "(2)") {
			t.Fatalf("duplicate delivery: %s", o.Path)
		}
	}
	data, _ := os.ReadFile(ripLog)
	if got := strings.Fields(string(data)); strings.Join(got, ",") != "1,2,2" {
		t.Fatalf("rips = %v, want title 1 once and title 2 twice", got)
	}
	if next := e.st.NextEpisode("tmdb:1668", 1); next != 3 {
		t.Fatalf("next episode = %d", next)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.RipDir(), "fp-friends-1")); !os.IsNotExist(err) {
		t.Fatalf("workspace not removed after success: %v", err)
	}
}

// With resume off, a failed disc starts over.
func TestResumeDisabled(t *testing.T) {
	id := &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TMDBID: 1668, EpisodeRuntime: 22 * time.Minute, Confidence: 1, Source: "test"}
	e := setup(t, tvInfo, fakeProvider{id})
	e.cfg.Output.Resume = false
	ripLog := filepath.Join(t.TempDir(), "rips.log")
	t.Setenv("FAKE_LOG", ripLog)
	t.Setenv("FAKE_FAIL_TITLE", "2")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)

	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	if j := e.waitDone(t); j.Stage != StageFailed {
		t.Fatalf("stage = %s", j.Stage)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.RipDir(), "fp-friends-1")); !os.IsNotExist(err) {
		t.Fatalf("workspace kept with resume off: %v", err)
	}
}

func TestStaleWorkspace(t *testing.T) {
	now := time.Now()
	write := func(updated time.Time) string {
		dir := t.TempDir()
		data, _ := json.Marshal(resumeState{Fingerprint: "fp", Updated: updated, Titles: map[int]*resumedTitle{1: {Ripped: "x"}}})
		if err := os.WriteFile(filepath.Join(dir, resumeFile), data, 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	fresh, old := write(now.Add(-time.Hour)), write(now.Add(-100*time.Hour))
	cases := []struct {
		name   string
		dir    string
		resume bool
		stale  bool
	}{
		{"recent record kept", fresh, true, false},
		{"old record removed", old, true, true},
		{"no record removed", t.TempDir(), true, true},
		{"resume off removes all", fresh, false, true},
	}
	for _, c := range cases {
		if got := staleWorkspace(c.dir, c.resume, 72*time.Hour, now); got != c.stale {
			t.Errorf("%s: stale = %v, want %v", c.name, got, c.stale)
		}
	}
}

// While a job runs the loop does not poll, so the drive status must be read
// live; after an eject part-way through the job it reads tray-open.
func TestDriveStatusLiveDuringJob(t *testing.T) {
	id := &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TMDBID: 1668, EpisodeRuntime: 22 * time.Minute, Confidence: 1, Source: "test"}
	e := setup(t, tvInfo, fakeProvider{id})
	e.cfg.PostProcess.Mode = "custom"
	e.cfg.PostProcess.CustomCommand = []string{"sh", "-c", "sleep 1; cp \"$1\" \"$2\"", "sh", "{input}", "{output}"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)

	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, d := range e.m.Snapshot().Drives {
			if d.Job != nil && d.Job.Ejected && !d.Job.Stage.Terminal() {
				if d.Status != drive.TrayOpen.String() {
					t.Fatalf("drive status = %s after eject, want tray-open", d.Status)
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("never saw the job between eject and finish")
}
