package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/discdb"
	"github.com/sourcequality/media-ripper/internal/drive"
	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/notify"
	"github.com/sourcequality/media-ripper/internal/selector"
	"github.com/sourcequality/media-ripper/internal/store"
	"github.com/sourcequality/media-ripper/internal/udf"
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
    info|mkv|backup) cmd=$a ;;
  esac
done
if [ "$cmd" = info ]; then
  cat "$FAKE_INFO"
  exit 0
fi
if [ "$cmd" = backup ]; then
  outdir=$(eval echo \${$#})
  echo 'PRGC:5018,0,"Backing up disc"'
  echo 'PRGV:0,0,65536'
  if [ -n "$FAKE_BACKUP_FAIL" ]; then
    echo 'MSG:2003,0,1,"Error reading sector","Error reading sector"'
    exit 1
  fi
  mkdir -p "$outdir/BDMV/STREAM"
  head -c 300000 /dev/zero > "$outdir/BDMV/STREAM/00001.m2ts"
  echo 'PRGV:65536,65536,65536'
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
	cfg.Metadata.TheDiscDB.Enabled = false // never reach GitHub from tests
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
		// No real disc to read; tests that need a hash set their own.
		DiscFiles: func(string) ([]udf.File, error) { return nil, errors.New("no disc filesystem in tests") },
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
				e.waitFinalized(t, j.ID)
				return j
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job did not finish: %+v", e.m.Snapshot())
	return Job{}
}

// waitFinalized waits for a finished job's history record. The stage turns
// terminal before finish() records the disc, the season progress and the
// history and cleans the workspace; tests that check those must wait.
func (e *env) waitFinalized(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h, _ := e.st.History(0)
		for _, raw := range h {
			var rec struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &rec) == nil && rec.ID == id {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never reached the history", id)
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
	n, err := moveFile(context.Background(), src, dst, 0o644, nil)
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
	e.waitFinalized(t, j.ID)
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

// A remux in progress shows up as an activity with its output growing.
func TestRemuxActivity(t *testing.T) {
	id := &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TMDBID: 1668, EpisodeRuntime: 22 * time.Minute, Confidence: 1, Source: "test"}
	e := setup(t, tvInfo, fakeProvider{id})
	e.cfg.PostProcess.Mode = "custom"
	e.cfg.PostProcess.CustomCommand = []string{"sh", "-c", "head -c 50000 \"$1\" > \"$2\"; sleep 2; cat \"$1\" >> \"$2\"", "sh", "{input}", "{output}"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)

	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, d := range e.m.Snapshot().Drives {
			if d.Job == nil {
				continue
			}
			for _, a := range d.Job.Activities {
				if a.Kind == "remux" && a.Done > 0 {
					if a.Total != 0 || a.Item != "S01E01" {
						t.Fatalf("custom command activity = %+v (total should be unknown)", a)
					}
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no remux activity seen")
}

type fakeCatalog struct {
	match  *discdb.Match
	err    error
	calls  int
	byHash map[string]*discdb.HashMatch
	discs  []discdb.DiscInfo
}

func (f *fakeCatalog) ReleaseDiscs(context.Context, discdb.Kind, string, int, string) ([]discdb.DiscInfo, error) {
	return f.discs, nil
}

func (f *fakeCatalog) FindByHash(_ context.Context, hash string, scan []discdb.ScanTitle) (*discdb.HashMatch, error) {
	return f.byHash[hash], nil
}

func (f *fakeCatalog) Find(_ context.Context, kind discdb.Kind, title string, year int, scan []discdb.ScanTitle) (*discdb.Match, error) {
	f.calls++
	return f.match, f.err
}

// A catalogued disc is numbered by the catalogue, not the season counter:
// disc 2 inserted first is still E05, and its extra is skipped by name.
func TestCatalogNumbersOutOfOrderDisc(t *testing.T) {
	id := &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TMDBID: 1668, Season: 1, EpisodeRuntime: 22 * time.Minute, Confidence: 1, Source: "test"}
	e := setup(t, tvInfo, fakeProvider{id})
	cat := &fakeCatalog{match: &discdb.Match{Release: "season-1-dvd", Disc: discdb.Disc{Name: "Disc 2"}, Matched: 2, Compared: 2, ByID: map[int]discdb.Title{
		1: {Item: &discdb.Item{Type: "Episode", Title: "The One with the East German Laundry Detergent", Season: 1, Episode: 5}},
		2: {Item: &discdb.Item{Type: "Extra", Title: "Gag reel"}},
	}}}
	e.m.deps.Catalog = cat
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)

	e.drv.insert("fp-friends-2", "FRIENDS_S1_D2")
	j := e.waitDone(t)
	if j.Stage != StageDone || len(j.Outputs) != 1 {
		t.Fatalf("stage=%s outputs=%+v log=%v", j.Stage, j.Outputs, j.Log)
	}
	want := filepath.Join(e.out, "TV Shows", "Friends (1994)", "Season 01", "Friends - S01E05 - The One with the East German Laundry Detergent.mkv")
	if j.Outputs[0].Path != want {
		t.Fatalf("delivered %s", j.Outputs[0].Path)
	}
	if next := e.st.NextEpisode("tmdb:1668", 1); next != 6 {
		t.Fatalf("next episode = %d", next)
	}
	skippedExtra := false
	for _, s := range j.Selection.Skipped {
		if s.TitleID == 2 && strings.Contains(s.Reason, "Gag reel") {
			skippedExtra = true
		}
	}
	if !skippedExtra {
		t.Fatalf("extra not skipped by name: %+v", j.Selection.Skipped)
	}
}

// A catalogue that fails or knows nothing leaves the usual rules in charge.
func TestCatalogFallback(t *testing.T) {
	for _, cat := range []*fakeCatalog{{err: errors.New("github down")}, {}} {
		id := &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TMDBID: 1668, Season: 1, EpisodeRuntime: 22 * time.Minute, Confidence: 1, Source: "test"}
		e := setup(t, tvInfo, fakeProvider{id})
		e.m.deps.Catalog = cat
		e.m.SetConfig(e.cfg)
		ctx, cancel := context.WithCancel(context.Background())
		go e.m.Run(ctx)
		e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
		j := e.waitDone(t)
		cancel()
		if j.Stage != StageDone || len(j.Outputs) != 2 || cat.calls != 1 {
			t.Fatalf("err=%v: stage=%s outputs=%d calls=%d", cat.err, j.Stage, len(j.Outputs), cat.calls)
		}
	}
}

// A job announces itself, says the tray is free after the rip, then
// reports completion; each event carries the match and the titles.
func TestJobNotifications(t *testing.T) {
	var mu sync.Mutex
	var events []notify.Event
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev notify.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}))
	defer hook.Close()
	id := &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TMDBID: 1668, Season: 1, EpisodeRuntime: 22 * time.Minute, Confidence: 1, Source: "sonarr",
		Episodes: []metadata.Episode{{Number: 1, Title: "Pilot"}, {Number: 2, Title: "Two"}}}
	e := setup(t, tvInfo, fakeProvider{id})
	e.m.deps.Notifier = &notify.Notifier{WebhookURL: hook.URL}
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	if j := e.waitDone(t); j.Stage != StageDone {
		t.Fatalf("stage = %s", j.Stage)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(events)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	var types []string
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "started,ready,done" {
		t.Fatalf("events = %v", types)
	}
	start := events[0]
	if start.Match != "⚠️ Not verified: found Friends (1994) via Sonarr; episodes numbered in disc order from S01E01" || len(start.Items) != 2 || start.Items[0] != "S01E01 Pilot" || start.Title != "Friends S01 D1" {
		t.Fatalf("started event = %+v", start)
	}
}

func TestVerification(t *testing.T) {
	tz := &metadata.Identity{Kind: metadata.KindTV, Title: "The Twilight Zone", Year: 1959, Season: 1, Source: "sonarr", Confidence: 1}
	ep := &selector.Selection{Picks: []selector.Pick{{Title: &makemkv.Title{}, Season: 1, Episode: 16}}}
	cases := []struct {
		name string
		job  Job
		want string
	}{
		{"all titles match the catalogue",
			Job{Identity: tz, Catalog: "TheDiscDB (The Complete Series Blu-ray 2021, Season 1 Disc 4)", CatalogMatched: 8, CatalogCompared: 8},
			"✅ Verified by TheDiscDB: all 8 titles match The Complete Series Blu-ray 2021, Season 1 Disc 4"},
		{"most titles match",
			Job{Identity: tz, Catalog: "TheDiscDB (X, Disc 1)", CatalogMatched: 7, CatalogCompared: 9},
			"☑️ TheDiscDB: 7 of 9 titles match X, Disc 1"},
		{"identified, numbered by order",
			Job{Identity: tz, Selection: ep},
			"⚠️ Not verified: found The Twilight Zone (1959) via Sonarr; episodes numbered in disc order from S01E16"},
		{"movie by length",
			Job{Identity: &metadata.Identity{Kind: metadata.KindMovie, Title: "The Thing", Year: 1982, Source: "radarr", Confidence: 1}},
			"⚠️ Not verified: found The Thing (1982) via Radarr; titles chosen by length"},
		{"not identified",
			Job{Identity: &metadata.Identity{Kind: metadata.KindUnknown}, Label: "BD_ROM"},
			"⚠️ Not identified (label BD_ROM); ripping by length"},
	}
	for _, c := range cases {
		if got := verification(c.job); got != c.want {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
	}
}

// A disc whose label says nothing is identified by its content hash, and
// counts as verified: it is that exact catalogued disc.
func TestHashIdentifiesJunkLabel(t *testing.T) {
	e := setup(t, strings.ReplaceAll(movieInfo, "THE_MATRIX", "BD_ROM"), metadata.NoneProvider{})
	files := []udf.File{{Path: "BDMV/STREAM/00001.m2ts", Size: 123}}
	hash, _ := udf.ContentHash(files)
	e.m.deps.DiscFiles = func(string) ([]udf.File, error) { return files, nil }
	e.m.deps.Catalog = &fakeCatalog{byHash: map[string]*discdb.HashMatch{hash: {
		Kind: discdb.Movie, Title: "The Matrix", Year: 1999, TMDBID: 603,
		Match: discdb.Match{Release: "1999-blu-ray", Disc: discdb.Disc{Name: "Disc 1", ContentHash: hash}, Matched: 2, Compared: 3,
			ByID: map[int]discdb.Title{0: {Item: &discdb.Item{Type: "MainMovie"}}, 1: {Item: &discdb.Item{Type: "Extra", Title: "Decoy cut"}}}},
	}}}
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-junk", "BD_ROM")
	j := e.waitDone(t)
	if j.Stage != StageDone || j.Identity.Title != "The Matrix" || j.Identity.Source != "thediscdb" {
		t.Fatalf("stage=%s identity=%+v", j.Stage, j.Identity)
	}
	if len(j.Selection.Picks) != 1 || j.Selection.Picks[0].Title.ID != 0 {
		t.Fatalf("picks = %+v", j.Selection.Picks)
	}
	if !j.HashMatched || !strings.Contains(j.Verification, "this exact disc") {
		t.Fatalf("verification = %q hash=%v", j.Verification, j.HashMatched)
	}
	want := filepath.Join(e.out, "Movies", "The Matrix (1999)", "The Matrix (1999).mkv")
	if len(j.Outputs) != 1 || j.Outputs[0].Path != want {
		t.Fatalf("outputs = %+v", j.Outputs)
	}
}

// A title match whose catalogued content hash equals the disc's is the
// strongest verification, even when not every title compared.
func TestHashConfirmsTitleMatch(t *testing.T) {
	id := &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, Season: 1, EpisodeRuntime: 22 * time.Minute, Confidence: 1, Source: "sonarr"}
	e := setup(t, tvInfo, fakeProvider{id})
	files := []udf.File{{Path: "VIDEO_TS/VTS_01_1.VOB", Size: 1 << 30}}
	hash, _ := udf.ContentHash(files)
	e.m.deps.DiscFiles = func(string) ([]udf.File, error) { return files, nil }
	e.m.deps.Catalog = &fakeCatalog{match: &discdb.Match{Release: "season-1-dvd", Disc: discdb.Disc{Name: "Disc 1", ContentHash: strings.ToLower(hash)}, Matched: 2, Compared: 4, ByID: map[int]discdb.Title{
		1: {Item: &discdb.Item{Type: "Episode", Title: "Pilot", Season: 1, Episode: 1}},
		2: {Item: &discdb.Item{Type: "Episode", Title: "Two", Season: 1, Episode: 2}},
	}}}
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends-1", "FRIENDS_S1_D1")
	j := e.waitDone(t)
	if !j.HashMatched || !strings.HasPrefix(j.Verification, "✅ Verified by TheDiscDB: this exact disc") {
		t.Fatalf("verification = %q", j.Verification)
	}
	if needsReview(j, e.m.rt.Load()) {
		t.Fatal("a hash-verified disc should not wait for review")
	}
}

// A catalogued disc counts towards its release; the view lists the discs
// still missing.
func TestBoxSetProgress(t *testing.T) {
	id := &metadata.Identity{Kind: metadata.KindTV, Title: "Friends", Year: 1994, TMDBID: 1668, Season: 1, EpisodeRuntime: 22 * time.Minute, Confidence: 1, Source: "sonarr"}
	e := setup(t, tvInfo, fakeProvider{id})
	e.m.deps.Catalog = &fakeCatalog{
		match: &discdb.Match{Release: "the-complete-series-dvd-2004", Disc: discdb.Disc{Index: 2, Name: "Season 1 Disc 2", Slug: "S01D02"}, Matched: 2, Compared: 2, ByID: map[int]discdb.Title{
			1: {Item: &discdb.Item{Type: "Episode", Season: 1, Episode: 5}}, 2: {Item: &discdb.Item{Type: "Episode", Season: 1, Episode: 6}}}},
		discs: []discdb.DiscInfo{{Index: 1, Name: "Season 1 Disc 1", Slug: "S01D01"}, {Index: 2, Name: "Season 1 Disc 2", Slug: "S01D02"}, {Index: 3, Name: "Season 2 Disc 1", Slug: "S02D01"}},
	}
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-friends-2", "FRIENDS_S1_D2")
	if j := e.waitDone(t); j.Stage != StageDone {
		t.Fatalf("stage = %s", j.Stage)
	}
	sets := e.m.BoxSets(ctx)
	if len(sets) != 1 {
		t.Fatalf("sets = %+v", sets)
	}
	b := sets[0]
	if b.Title != "Friends" || b.ReleaseName != "The Complete Series DVD 2004" || b.Ripped != 1 || b.Total != 3 || len(b.Groups) != 2 {
		t.Fatalf("box set = %+v", b)
	}
	s1 := b.Groups[0]
	if s1.Name != "Season 1" || len(s1.Discs) != 2 || s1.Discs[0].Ripped || !s1.Discs[1].Ripped || s1.Discs[1].Short != "D2" {
		t.Fatalf("season 1 = %+v", s1)
	}
}

func TestBoxSetBackfillFromHistory(t *testing.T) {
	e := setup(t, tvInfo, nil)
	e.m.deps.Catalog = &fakeCatalog{match: &discdb.Match{Release: "the-complete-series-blu-ray-2021", Disc: discdb.Disc{Index: 4, Name: "Season 1 Disc 4", Slug: "S01D04"}, Matched: 2, Compared: 2}}
	e.m.SetConfig(e.cfg)
	_ = e.st.AppendHistory(map[string]any{"id": "old-1", "stage": "done", "finished_at": time.Now(),
		"identity":  map[string]any{"kind": "tv", "title": "The Twilight Zone", "year": 1959, "confidence": 1, "source": "sonarr"},
		"selection": map[string]any{"picks": []any{map[string]any{"title": map[string]any{"id": 6, "source_file": "00000.mpls", "size_bytes": 1}, "reason": "TheDiscDB: S01E23"}}}})
	_ = e.st.AppendHistory(map[string]any{"id": "old-2", "stage": "done", "identity": map[string]any{"kind": "tv", "title": "Friends", "confidence": 1},
		"selection": map[string]any{"picks": []any{map[string]any{"title": map[string]any{"id": 1}, "reason": "22:10 fits episode length"}}}})
	e.m.backfillBoxSets(context.Background())
	sets := e.st.BoxSets()
	if len(sets) != 1 || sets[0].Title != "The Twilight Zone" || sets[0].Ripped["S01D04"].JobID != "old-1" {
		t.Fatalf("backfilled = %+v", sets)
	}
	e.m.backfillBoxSets(context.Background()) // once only
	if len(e.st.BoxSets()) != 1 {
		t.Fatal("ran twice")
	}
}

// A finished disc keeps its file inventory, so it can be exported as a
// disc manifest with the title mapping for TheDiscDB.
func TestDiscManifestExport(t *testing.T) {
	e := setup(t, movieInfo, fakeProvider{&metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Year: 1999, TMDBID: 603, Runtime: 136 * time.Minute, Confidence: 1, Source: "test"}})
	files := []udf.File{{Path: "BDMV/STREAM/00001.m2ts", Size: 32_500_000_000}, {Path: "BDMV/PLAYLIST/00800.mpls", Size: 298}, {Path: "BDMV/index.bdmv", Size: 120}}
	e.m.deps.DiscFiles = func(string) ([]udf.File, error) { return files, nil }
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitDone(t)
	if j.Outputs[0].RippedAs != "Disc_t00.mkv" {
		t.Fatalf("ripped as %q", j.Outputs[0].RippedAs)
	}
	m, err := e.m.DiscManifest(j.ID, "v0.6.0")
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := udf.ContentHash(files)
	if m.Disc.Format != "blu-ray" || m.Disc.Name != "THE_MATRIX" || len(m.Disc.Files) != 3 || m.Disc.Identifiers[0].Value != hash {
		t.Fatalf("manifest = %+v", m.Disc)
	}
	text, err := e.m.ContributionText(j.ID)
	if err != nil || !strings.Contains(text, "The Matrix (1999)") || !strings.Contains(text, "Main movie") || !strings.Contains(text, hash) {
		t.Fatalf("contribution:\n%s", text)
	}
	if _, err := e.m.DiscManifest("nope", "v"); err == nil {
		t.Fatal("unknown job")
	}
}

func TestFullDiscBackup(t *testing.T) {
	matrix := &metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Year: 1999, TMDBID: 603, Runtime: 136 * time.Minute, Confidence: 1, Source: "test"}
	for _, c := range []struct {
		mode, fail string
		outputs    int
		backup     bool
		warning    bool
	}{
		{"also", "", 1, true, false},
		{"only", "", 0, true, false},
		{"also", "1", 1, false, true},
	} {
		e := setup(t, movieInfo, fakeProvider{matrix})
		e.cfg.Output.Backup = c.mode
		e.m.SetConfig(e.cfg)
		t.Setenv("FAKE_BACKUP_FAIL", c.fail)
		ctx, cancel := context.WithCancel(context.Background())
		go e.m.Run(ctx)
		e.drv.insert("fp-matrix", "THE_MATRIX")
		j := e.waitDone(t)
		cancel()
		name := c.mode + " fail=" + c.fail
		if j.Stage != StageDone || len(j.Outputs) != c.outputs || (j.Backup != nil) != c.backup || (len(j.Warnings) > 0) != c.warning {
			t.Fatalf("%s: stage=%s outputs=%d backup=%+v warnings=%v err=%s", name, j.Stage, len(j.Outputs), j.Backup, j.Warnings, j.Error)
		}
		if c.backup {
			want := filepath.Join(e.out, "_backups", "The Matrix (1999)")
			if j.Backup.Path != want || j.Backup.Size != 300000 {
				t.Fatalf("%s: backup = %+v", name, j.Backup)
			}
			if _, err := os.Stat(filepath.Join(want, "BDMV", "STREAM", "00001.m2ts")); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if leftovers, _ := filepath.Glob(filepath.Join(e.out, "_backups", "*.part")); len(leftovers) != 0 {
			t.Fatalf("%s: partial backup left behind: %v", name, leftovers)
		}
	}
}
