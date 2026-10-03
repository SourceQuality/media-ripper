package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/metadata"
)

const fakeFFmpeg = `#!/bin/sh
out=""
for a in "$@"; do out="$a"; done
case "$out" in *h_*) : > "$(dirname "$out")/h_00001.png"; : > "$(dirname "$out")/h_00002.png";; esac
exit 0
`

const fakeTesseract = `#!/bin/sh
printf 'level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n'
printf '1\t1\t0\t0\t0\t0\t0\t0\t1280\t720\t-1\t\n'
printf '5\t1\t1\t1\t1\t1\t400\t300\t200\t80\t91\tTHE\n'
printf '5\t1\t1\t1\t1\t2\t620\t300\t300\t80\t95\tMATRIX\n'
exit 0
`

// labelAware identifies only when asked about "The Matrix", so the junk
// label fails and OCR must do the work.
type labelAware struct{}

func (labelAware) Identify(_ context.Context, h metadata.Hint, _ metadata.DiscHints) (*metadata.Identity, error) {
	if h.Query == "The Matrix" {
		return &metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Year: 1999, TMDBID: 603, Runtime: 136 * time.Minute, Confidence: 1, Source: "test", Hint: h}, nil
	}
	return &metadata.Identity{Kind: metadata.KindUnknown, Hint: h}, nil
}

func TestOCRIdentifiesUnlabelledDisc(t *testing.T) {
	e := setup(t, strings.ReplaceAll(movieInfo, "THE_MATRIX", "BD_ROM"), labelAware{})
	dir := t.TempDir()
	ff, ts := filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "tesseract")
	_ = os.WriteFile(ff, []byte(fakeFFmpeg), 0o755)
	_ = os.WriteFile(ts, []byte(fakeTesseract), 0o755)
	e.cfg.Metadata.OCR.Enabled = true
	e.m.deps.OCRTools = []string{ff, ts}
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-blank", "BD_ROM")
	j := e.waitDone(t)
	if j.Stage != StageDone {
		t.Fatalf("job = %s err=%s log=%v", j.Stage, j.Error, j.Log)
	}
	if j.Identity == nil || j.Identity.Title != "The Matrix" || j.Identity.Source != "ocr+test" {
		t.Fatalf("identity = %+v", j.Identity)
	}
	want := filepath.Join(e.out, "Movies", "The Matrix (1999)", "The Matrix (1999).mkv")
	if len(j.Outputs) != 1 || j.Outputs[0].Path != want {
		t.Fatalf("outputs = %+v", j.Outputs)
	}
	if !j.Ejected {
		t.Fatal("disc should have been ejected before OCR")
	}
}

func TestOCRSkippedWhenToolsMissing(t *testing.T) {
	e := setup(t, strings.ReplaceAll(movieInfo, "THE_MATRIX", "BD_ROM"), labelAware{})
	e.cfg.Metadata.OCR.Enabled = true
	e.m.deps.OCRTools = []string{"/nonexistent/ffmpeg", "/nonexistent/tesseract"}
	e.m.SetConfig(e.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-blank", "BD_ROM")
	j := e.waitDone(t)
	if j.Stage != StageDone || !strings.Contains(j.Outputs[0].Path, "_unidentified") {
		t.Fatalf("job = %+v", j)
	}
	found := false
	for _, l := range j.Log {
		if strings.Contains(l.Message, "ocr skipped") {
			found = true
		}
	}
	if !found {
		t.Fatalf("log = %v", j.Log)
	}
}
