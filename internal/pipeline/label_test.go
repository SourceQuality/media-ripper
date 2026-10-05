package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/metadata"
)

// matrixWithPlaylists is movieInfo with each title's playlist, so it can
// be previewed.
var matrixWithPlaylists = movieInfo + `TINFO:0,16,0,"00800.mpls"
TINFO:1,16,0,"00801.mpls"
TINFO:2,16,0,"00005.mpls"
`

// fakeBlurayFFmpeg answers like ffmpeg reading bluray:, recording its arguments.
const fakeBlurayFFmpeg = `#!/bin/sh
[ -n "$FAKE_FFMPEG_LOG" ] && echo "$* $LIBAACS_PATH" >> "$FAKE_FFMPEG_LOG"
case "$*" in *mp4*) [ -n "$FAKE_FFMPEG_SLOW" ] && exec sleep "$FAKE_FFMPEG_SLOW" ;; esac
# libmmbd's helper: started by ffmpeg, keeps its output open, outlives it.
[ -n "$FAKE_FFMPEG_HELPER" ] && sleep 317 &
case "$*" in
  *mjpeg*) for last; do :; done; printf 'JPEGDATA' > "$last" ;;
  *) printf 'MP4DATA' ;;
esac
`

func manualEnv(t *testing.T, info string) *env {
	e := setup(t, info, fakeProvider{matrixID()})
	ff := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(ff, []byte(fakeBlurayFFmpeg), 0o755); err != nil {
		t.Fatal(err)
	}
	e.m.deps.FFmpeg = ff
	e.cfg.Mode = "manual"
	e.cfg.Metadata.TheDiscDB.DiscFolder = "off"
	e.m.SetConfig(e.cfg)
	return e
}

func (e *env) waitStage(t *testing.T, stage Stage) Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, d := range e.m.Snapshot().Drives {
			if d.Job != nil && d.Job.Stage == stage {
				return *d.Job
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never reached %s", stage)
	return Job{}
}

func labelOf(j Job, id int) TitleLabel {
	for _, l := range j.Labels {
		if l.TitleID == id {
			return l
		}
	}
	return TitleLabel{}
}

// Manual mode: the disc waits with a guessed label for every title, stills
// and previews come from the disc, and the person's labels decide the rip
// and name the extras for TheDiscDB.
func TestManualModeLabelThenRip(t *testing.T) {
	e := manualEnv(t, matrixWithPlaylists)
	ffLog := filepath.Join(t.TempDir(), "ff")
	t.Setenv("FAKE_FFMPEG_LOG", ffLog)
	ripLog := filepath.Join(t.TempDir(), "rips")
	t.Setenv("FAKE_LOG", ripLog)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitStage(t, StageLabelling)

	if l := labelOf(j, 0); l.Kind != LabelMain || !l.Rip || l.Guess == "" {
		t.Fatalf("title 0 guessed %+v", l)
	}
	if l := labelOf(j, 1); l.Kind != LabelSkip || l.Rip {
		t.Fatalf("title 1 (as long as the feature) guessed %+v", l)
	}
	if l := labelOf(j, 2); l.Kind != LabelTrailer {
		t.Fatalf("title 2 (2:11) guessed %+v", l)
	}
	if data, _ := os.ReadFile(ripLog); len(data) != 0 {
		t.Fatal("ripped before being labelled")
	}

	// Stills and previews, read from the disc through libmmbd.
	img, err := e.m.Thumbnail(ctx, j.ID, 2)
	if err != nil || string(img) != "JPEGDATA" {
		t.Fatalf("thumbnail %q %v", img, err)
	}
	var vid bytes.Buffer
	if err := e.m.Preview(ctx, j.ID, 2, 30*time.Second, &vid); err != nil || vid.String() != "MP4DATA" {
		t.Fatalf("preview %q %v", vid.String(), err)
	}
	calls, _ := os.ReadFile(ffLog)
	if !strings.Contains(string(calls), "-playlist 5 -ss 30 -i bluray:/dev/fake0") || !strings.Contains(string(calls), "libmmbd") {
		t.Fatalf("ffmpeg calls:\n%s", calls)
	}

	// Nothing to rip is refused; an unknown title too.
	if err := e.m.SubmitLabels(j.ID, LabelDecision{Action: "rip", Labels: []TitleLabel{{TitleID: 0, Kind: LabelSkip}}}); err == nil {
		t.Fatal("rip with nothing to rip accepted")
	}
	if err := e.m.SubmitLabels(j.ID, LabelDecision{Action: "rip", Labels: []TitleLabel{{TitleID: 9, Kind: LabelMain, Rip: true}}}); err == nil {
		t.Fatal("unknown title accepted")
	}
	labels := []TitleLabel{
		{TitleID: 0, Kind: LabelMain, Rip: true},
		{TitleID: 1, Kind: LabelExtra, Category: "behind-the-scenes", Name: "Behind the Matrix", Rip: true},
		{TitleID: 2, Kind: LabelTrailer, Name: "Theatrical trailer"}, // named, not ripped
	}
	if err := e.m.SubmitLabels(j.ID, LabelDecision{Action: "rip", Labels: labels}); err != nil {
		t.Fatal(err)
	}
	done := e.waitDone(t)
	if done.Stage != StageDone || len(done.Outputs) != 2 {
		t.Fatalf("ripped: stage=%s outputs=%+v err=%s", done.Stage, done.Outputs, done.Error)
	}
	if data, _ := os.ReadFile(ripLog); strings.Join(strings.Fields(string(data)), ",") != "0,1" {
		t.Fatalf("rips = %q, want titles 0 and 1", data)
	}
	// The extra sits next to the movie, where Plex and Jellyfin look.
	movieDir := filepath.Join(e.out, "Movies", "The Matrix (1999)")
	if _, err := os.Stat(filepath.Join(movieDir, "Behind The Scenes", "Behind the Matrix.mkv")); err != nil {
		t.Fatalf("extra not next to the movie: %v (outputs %+v)", err, done.Outputs)
	}
	text, err := e.m.ContributionText(done.ID)
	if err != nil || !strings.Contains(text, "Extra: Behind the Matrix") || !strings.Contains(text, "Trailer: Theatrical trailer") {
		t.Fatalf("title mapping:\n%s", text)
	}
	// Remembered: the next time this disc goes in, the labels are offered.
	dm, ok := e.st.DiscMatch("fp-matrix")
	var saved []TitleLabel
	if !ok || json.Unmarshal(dm.Labels, &saved) != nil || len(saved) != 3 || saved[1].Name != "Behind the Matrix" {
		t.Fatalf("remembered labels: %s", dm.Labels)
	}
	if _, err := e.m.Thumbnail(ctx, done.ID, 2); err == nil {
		t.Fatal("previews outlive the labelling")
	}
}

// Keeping it for TheDiscDB, ejecting, or taking the disc out are the other
// ways out of labelling.
func TestManualModeOtherDecisions(t *testing.T) {
	e := manualEnv(t, matrixWithPlaylists)
	fakeDisc(e) // the folder reader, for TheDiscDB only
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)

	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitStage(t, StageLabelling)
	if err := e.m.SubmitLabels(j.ID, LabelDecision{Action: "contribute", Labels: []TitleLabel{{TitleID: 2, Kind: LabelTrailer, Name: "Trailer"}}}); err != nil {
		t.Fatal(err)
	}
	e.waitEject(t, 1)
	e.waitFinalized(t, j.ID)
	if r, _ := e.m.Record(j.ID); !r.Contribute || r.Stage != StageDone || len(r.Labels) != 1 {
		t.Fatalf("contribute: %+v", r)
	}

	e.drv.insert("fp-matrix", "THE_MATRIX")
	j2 := e.waitStage(t, StageLabelling)
	if err := e.m.SubmitLabels(j2.ID, LabelDecision{Action: "eject"}); err != nil {
		t.Fatal(err)
	}
	e.waitEject(t, 2)
	e.waitFinalized(t, j2.ID)
	if r, _ := e.m.Record(j2.ID); r.Stage != StageSkipped {
		t.Fatalf("eject: %+v", r)
	}

	// Taking the disc out while it waits.
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j3 := e.waitStage(t, StageLabelling)
	_ = e.drv.Eject()
	e.waitFinalized(t, j3.ID)
	if r, _ := e.m.Record(j3.ID); r.Stage != StageSkipped || !strings.Contains(r.Message+r.Error, "taken out") {
		t.Fatalf("taken out: stage=%s message=%q", r.Stage, r.Message)
	}
	if _, ripped := e.st.Disc("fp-matrix"); ripped {
		t.Fatal("recorded as ripped without a rip")
	}
}

// In auto mode, "label first" holds one disc for labelling.
func TestLabelFirstInAutoMode(t *testing.T) {
	e := manualEnv(t, matrixWithPlaylists)
	e.cfg.Mode = "auto"
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	for len(e.m.Snapshot().Drives) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if err := e.m.NextDisc("/dev/fake0", "label"); err != nil {
		t.Fatal(err)
	}
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitStage(t, StageLabelling)
	if !j.Manual {
		t.Fatal("not marked manual")
	}
	if err := e.m.NextDisc("/dev/fake0", "bogus"); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func matrixID() *metadata.Identity {
	return &metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Year: 1999, TMDBID: 603, Runtime: 136 * time.Minute, Confidence: 1, Source: "test"}
}

// A decision stops a preview still reading the disc before ripping.
func TestDecisionStopsPreview(t *testing.T) {
	e := manualEnv(t, matrixWithPlaylists)
	t.Setenv("FAKE_FFMPEG_SLOW", "30")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitStage(t, StageLabelling)
	previewDone := make(chan error, 1)
	go func() { previewDone <- e.m.Preview(context.Background(), j.ID, 0, 0, &bytes.Buffer{}) }()
	time.Sleep(300 * time.Millisecond) // the preview is reading
	start := time.Now()
	if err := e.m.SubmitLabels(j.ID, LabelDecision{Action: "rip", Labels: []TitleLabel{{TitleID: 0, Kind: LabelMain, Rip: true}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-previewDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the preview kept reading the disc")
	}
	if d := e.waitDone(t); d.Stage != StageDone || time.Since(start) > 20*time.Second {
		t.Fatalf("rip after the preview: %s in %s", d.Stage, time.Since(start))
	}
	// And no new preview starts once the decision is made.
	if err := e.m.Preview(context.Background(), j.ID, 0, 0, &bytes.Buffer{}); err == nil {
		t.Fatal("preview after the decision")
	}
}

// With Radarr, extras are kept out of the import (it would take one for
// the movie) and placed in its library folder afterwards.
func TestExtrasPlacedAfterRadarrImport(t *testing.T) {
	e := manualEnv(t, matrixWithPlaylists)
	radarrRoot := filepath.Join(e.out, "radarr-movies")
	srv, calls := fakeRadarr(t, radarrRoot, "/remote/library")
	defer srv.Close()
	e.cfg.Arr.Radarr = config.ArrApp{Enabled: true, URL: srv.URL, APIKey: "k", RootFolder: radarrRoot, AddMissing: true, ImportMode: "move",
		PathMap: map[string]string{e.out: "/remote/library"}}
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitStage(t, StageLabelling)
	if err := e.m.SubmitLabels(j.ID, LabelDecision{Action: "rip", Labels: []TitleLabel{
		{TitleID: 0, Kind: LabelMain, Rip: true},
		{TitleID: 2, Kind: LabelTrailer, Name: "Theatrical trailer", Rip: true},
	}}); err != nil {
		t.Fatal(err)
	}
	done := e.waitDone(t)
	if done.Stage != StageDone || len(done.Warnings) != 0 || importFailed(done) {
		t.Fatalf("stage=%s warnings=%v outputs=%+v", done.Stage, done.Warnings, done.Outputs)
	}
	trailer := filepath.Join(radarrRoot, "The Matrix (1999)", "Trailers", "Theatrical trailer.mkv")
	if _, err := os.Stat(trailer); err != nil {
		t.Fatalf("trailer not in the library folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(radarrRoot, "The Matrix (1999)", "The Matrix (1999).mkv")); err != nil {
		t.Fatalf("movie not imported: %v", err)
	}
	for _, o := range done.Outputs {
		if o.Extra != "" && (o.Path != trailer || o.Import != "") {
			t.Fatalf("extra output %+v", o)
		}
	}
	// Radarr was only ever given the movie.
	if n := strings.Count(strings.Join(*calls, " "), "POST /api/v3/command"); n != 1 {
		t.Fatalf("%d import commands", n)
	}
	if left, _ := filepath.Glob(filepath.Join(e.out, "_incoming", "_extras", "*")); len(left) != 0 {
		t.Fatalf("extras staging left: %v", left)
	}
}

// Only extras: the disc's film is already in the library.
func TestRipOnlyExtras(t *testing.T) {
	e := manualEnv(t, matrixWithPlaylists)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitStage(t, StageLabelling)
	if err := e.m.SubmitLabels(j.ID, LabelDecision{Action: "rip", Labels: []TitleLabel{
		{TitleID: 0, Kind: LabelMain},
		{TitleID: 2, Kind: LabelExtra, Category: "interview", Name: "Keanu", Rip: true},
	}}); err != nil {
		t.Fatal(err)
	}
	done := e.waitDone(t)
	want := filepath.Join(e.out, "Movies", "The Matrix (1999)", "Interviews", "Keanu.mkv")
	if done.Stage != StageDone || len(done.Outputs) != 1 || done.Outputs[0].Path != want {
		t.Fatalf("stage=%s outputs=%+v err=%s", done.Stage, done.Outputs, done.Error)
	}
	if err := e.m.SubmitLabels("nope", LabelDecision{Action: "rip"}); err == nil {
		t.Fatal("labels for a disc not waiting")
	}
}

// The second disc of a release: only its extras are ripped, into the
// folder of the movie Radarr already has, with no import.
func TestExtrasOnlyWithRadarr(t *testing.T) {
	e := manualEnv(t, matrixWithPlaylists)
	radarrRoot := filepath.Join(e.out, "radarr-movies")
	srv, calls := fakeRadarr(t, radarrRoot, "/remote/library")
	defer srv.Close()
	e.cfg.Arr.Radarr = config.ArrApp{Enabled: true, URL: srv.URL, APIKey: "k", RootFolder: radarrRoot, AddMissing: true, ImportMode: "move",
		PathMap: map[string]string{e.out: "/remote/library"}}
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitStage(t, StageLabelling)
	if err := e.m.SubmitLabels(j.ID, LabelDecision{Action: "rip", Labels: []TitleLabel{
		{TitleID: 0, Kind: LabelMain},
		{TitleID: 2, Kind: LabelExtra, Name: "The Making of", Rip: true},
	}}); err != nil {
		t.Fatal(err)
	}
	done := e.waitDone(t)
	want := filepath.Join(radarrRoot, "The Matrix (1999)", "Featurettes", "The Making of.mkv")
	if done.Stage != StageDone || len(done.Warnings) != 0 || len(done.Outputs) != 1 || done.Outputs[0].Path != want {
		t.Fatalf("stage=%s warnings=%v outputs=%+v", done.Stage, done.Warnings, done.Outputs)
	}
	if strings.Contains(strings.Join(*calls, " "), "POST /api/v3/command") {
		t.Fatal("an import was sent for extras only")
	}
}

// libmmbd's helper inherits ffmpeg's output and outlives it: reads must
// still finish, and the helper must not keep the disc busy (seen on The
// Thing: the decision then waited forever before ripping).
func TestLingeringHelperDoesNotHangReads(t *testing.T) {
	e := manualEnv(t, matrixWithPlaylists)
	t.Setenv("FAKE_FFMPEG_HELPER", "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitStage(t, StageLabelling)
	start := time.Now()
	img, err := e.m.Thumbnail(ctx, j.ID, 0)
	if err != nil || string(img) != "JPEGDATA" || time.Since(start) > 3*time.Second {
		t.Fatalf("thumbnail %q %v after %s", img, err, time.Since(start))
	}
	start = time.Now()
	var vid bytes.Buffer
	if err := e.m.Preview(ctx, j.ID, 0, 0, &vid); err != nil || vid.String() != "MP4DATA" || time.Since(start) > 3*time.Second {
		t.Fatalf("preview %q %v after %s", vid.String(), err, time.Since(start))
	}
	if err := e.m.SubmitLabels(j.ID, LabelDecision{Action: "rip", Labels: []TitleLabel{{TitleID: 0, Kind: LabelMain, Rip: true}}}); err != nil {
		t.Fatal(err)
	}
	if d := e.waitDone(t); d.Stage != StageDone {
		t.Fatalf("rip after a lingering helper: %s %s", d.Stage, d.Error)
	}
	// Every helper is killed with its ffmpeg's process group.
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-x", "-f", "sleep 317").Output()
		if len(strings.TrimSpace(string(out))) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helpers still running: %s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// An extra that cannot be placed (the library folder belongs to Radarr's
// user) is marked "not placed" and placed by a retry once it can be.
func TestExtraNotPlacedThenRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	e := manualEnv(t, matrixWithPlaylists)
	radarrRoot := filepath.Join(e.out, "radarr-movies")
	srv, _ := fakeRadarr(t, radarrRoot, "/remote/library")
	defer srv.Close()
	e.cfg.Arr.Radarr = config.ArrApp{Enabled: true, URL: srv.URL, APIKey: "k", RootFolder: radarrRoot, AddMissing: true, ImportMode: "move",
		PathMap: map[string]string{e.out: "/remote/library"}}
	e.m.SetConfig(e.cfg)
	movieDir := filepath.Join(radarrRoot, "The Matrix (1999)")
	if err := os.MkdirAll(movieDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(movieDir, 0o555) // as Radarr's folder is to media-ripper
	t.Cleanup(func() { _ = os.Chmod(movieDir, 0o755) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitStage(t, StageLabelling)
	if err := e.m.SubmitLabels(j.ID, LabelDecision{Action: "rip", Labels: []TitleLabel{
		{TitleID: 0, Kind: LabelMain},
		{TitleID: 2, Kind: LabelTrailer, Name: "Teaser", Rip: true},
	}}); err != nil {
		t.Fatal(err)
	}
	done := e.waitDone(t)
	if !importFailed(done) || !strings.HasPrefix(done.Outputs[0].Import, "not placed: media-ripper may not write") || len(done.Warnings) != 1 {
		t.Fatalf("outputs=%+v warnings=%v", done.Outputs, done.Warnings)
	}
	if _, err := os.Stat(done.Outputs[0].Path); err != nil {
		t.Fatalf("extra lost: %v", err)
	}

	_ = os.Chmod(movieDir, 0o755) // the group was given
	r, err := e.m.RetryImport(ctx, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(movieDir, "Trailers", "Teaser.mkv")
	if importFailed(r) || r.Outputs[0].Path != want || len(r.Warnings) != 0 {
		t.Fatalf("after retry: outputs=%+v warnings=%v", r.Outputs, r.Warnings)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatal(err)
	}
}
