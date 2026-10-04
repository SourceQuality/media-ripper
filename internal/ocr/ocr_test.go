package ocr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/metadata"
)

// fake ffmpeg: writes 6 frames; fake tesseract: returns TSV depending on
// frame name so "THE MATRIX" is big and frequent, dialogue is small/rare.
const fakeFFmpeg = `#!/bin/sh
out=""
for a in "$@"; do out="$a"; done
dir=$(dirname "$out")
case "$out" in
  *h_*) for i in 1 2 3 4; do : > "$dir/h_0000$i.png"; done ;;
  *t_*) for i in 1 2; do : > "$dir/t_0000$i.png"; done ;;
esac
exit 0
`

const fakeTesseract = `#!/bin/sh
f="$1"
printf 'level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n'
printf '1\t1\t0\t0\t0\t0\t0\t0\t1280\t720\t-1\t\n'
case "$f" in
  *h_00001*|*h_00002*)
    printf '5\t1\t1\t1\t1\t1\t400\t300\t200\t80\t91\tTHE\n'
    printf '5\t1\t1\t1\t1\t2\t620\t300\t300\t80\t95\tMATRIX\n'
    printf '5\t1\t2\t1\t1\t1\t10\t690\t60\t12\t88\tWhat\n'
    printf '5\t1\t2\t1\t1\t2\t80\t690\t40\t12\t85\tis\n'
    ;;
  *h_00003*)
    printf '5\t1\t1\t1\t1\t1\t10\t690\t80\t12\t40\tgarbage\n'
    printf '5\t1\t2\t1\t1\t1\t400\t300\t400\t60\t90\tWARNER\n'
    printf '5\t1\t2\t1\t1\t2\t820\t300\t200\t60\t90\tBROS\n'
    ;;
  *t_*)
    printf '5\t1\t1\t1\t1\t1\t500\t200\t200\t40\t92\tDirected\n'
    printf '5\t1\t1\t1\t1\t2\t720\t200\t60\t40\t92\tby\n'
    ;;
esac
exit 0
`

type fakeProvider struct{ calls []string }

func (p *fakeProvider) Identify(_ context.Context, h metadata.Hint, _ metadata.DiscHints) (*metadata.Identity, error) {
	p.calls = append(p.calls, h.Query)
	if h.Query == "The Matrix" {
		return &metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Year: 1999, Runtime: 136 * time.Minute, Confidence: 1, Source: "test", Hint: h}, nil
	}
	if h.Query == "Warner Bros" {
		return &metadata.Identity{Kind: metadata.KindMovie, Title: "Warner Bros", Year: 2000, Runtime: 20 * time.Minute, Confidence: 1, Source: "test", Hint: h}, nil
	}
	return &metadata.Identity{Kind: metadata.KindUnknown, Hint: h}, nil
}

func tools(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	ff := filepath.Join(dir, "ffmpeg")
	ts := filepath.Join(dir, "tesseract")
	if err := os.WriteFile(ff, []byte(fakeFFmpeg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ts, []byte(fakeTesseract), 0o755); err != nil {
		t.Fatal(err)
	}
	return Options{FFmpeg: ff, Tesseract: ts, HeadMinutes: 1, TailMinutes: 1, FramesPerMin: 4}
}

func TestExtractRanksTitleCard(t *testing.T) {
	cands, err := Extract(context.Background(), "/nonexistent.mkv", tools(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) == 0 || cands[0].Text != "The Matrix" || cands[0].Frames != 2 {
		t.Fatalf("candidates: %+v", cands)
	}
	for _, c := range cands {
		if c.Text == "Garbage" {
			t.Fatalf("low confidence text kept: %+v", cands)
		}
	}
}

func TestIdentifyConfirmsRuntime(t *testing.T) {
	p := &fakeProvider{}
	m, err := Identify(context.Background(), "/x.mkv", 136*time.Minute, 8*time.Minute, p, metadata.DiscHints{}, tools(t))
	if err != nil {
		t.Fatal(err)
	}
	if m.Identity == nil || m.Identity.Title != "The Matrix" || m.Identity.Source != "ocr+test" || m.Candidate.Text != "The Matrix" {
		t.Fatalf("match: %+v", m)
	}
	// A 20-minute rip cannot be The Matrix; "Warner Bros" matches by text
	// but its fake runtime agrees, so it is what gets accepted.
	p = &fakeProvider{}
	m, _ = Identify(context.Background(), "/x.mkv", 20*time.Minute, 8*time.Minute, p, metadata.DiscHints{}, tools(t))
	if m.Identity == nil || m.Identity.Title != "Warner Bros" {
		t.Fatalf("runtime gate: %+v calls=%v", m.Identity, p.calls)
	}
	// Nothing agrees: no identity, no error.
	m, err = Identify(context.Background(), "/x.mkv", 60*time.Minute, 8*time.Minute, &fakeProvider{}, metadata.DiscHints{}, tools(t))
	if err != nil || m.Identity != nil {
		t.Fatalf("expected no match: %+v %v", m, err)
	}
}

func TestParseTSV(t *testing.T) {
	lines := parseTSV("level\tpage\tb\tp\tl\tw\tleft\ttop\twidth\theight\tconf\ttext\n" +
		"1\t1\t0\t0\t0\t0\t0\t0\t100\t200\t-1\t\n" +
		"5\t1\t1\t1\t1\t1\t0\t0\t10\t20\t90\tHello\n" +
		"5\t1\t1\t1\t1\t2\t0\t0\t10\t20\t80\tWorld\n" +
		"5\t1\t1\t1\t2\t1\t0\t0\t10\t5\t10\tnoise\n")
	if len(lines) != 1 || lines[0].text != "Hello World" || lines[0].height != 0.1 {
		t.Fatalf("got %+v", lines)
	}
}
