// Package ocr identifies a ripped file by reading its title card and end
// credits with tesseract, then confirming a metadata match by runtime.
package ocr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sourcequality/media-ripper/internal/metadata"
)

// Options controls frame sampling and recognition.
type Options struct {
	FFmpeg        string
	Tesseract     string
	Languages     string
	HeadMinutes   int
	TailMinutes   int
	FramesPerMin  int
	MaxCandidates int
	Workers       int
	Timeout       time.Duration
	Logger        *slog.Logger
}

func (o *Options) defaults() {
	if o.FFmpeg == "" {
		o.FFmpeg = "ffmpeg"
	}
	if o.Tesseract == "" {
		o.Tesseract = "tesseract"
	}
	if o.Languages == "" {
		o.Languages = "eng"
	}
	if o.HeadMinutes <= 0 {
		o.HeadMinutes = 8
	}
	if o.TailMinutes < 0 {
		o.TailMinutes = 0
	}
	if o.FramesPerMin <= 0 {
		o.FramesPerMin = 20
	}
	if o.MaxCandidates <= 0 {
		o.MaxCandidates = 8
	}
	if o.Workers <= 0 {
		o.Workers = runtime.NumCPU()
		if o.Workers > 4 {
			o.Workers = 4
		}
	}
	if o.Timeout <= 0 {
		o.Timeout = 20 * time.Minute
	}
}

// Candidate is a piece of on-screen text that might be the title.
type Candidate struct {
	Text   string  `json:"text"`
	Frames int     `json:"frames"` // frames it appeared in
	Height float64 `json:"height"` // tallest occurrence, as a fraction of frame height
	Score  float64 `json:"score"`
}

// Available reports whether both tools can be found.
func Available(opts Options) error {
	opts.defaults()
	for _, bin := range []string{opts.FFmpeg, opts.Tesseract} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found", bin)
		}
	}
	return nil
}

// Extract samples frames from the start and end of file and returns the
// most prominent recognised text, best first.
func Extract(ctx context.Context, file string, opts Options) ([]Candidate, error) {
	opts.defaults()
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "media-ripper-ocr-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	fps := fmt.Sprintf("%d/60", opts.FramesPerMin)
	vf := "fps=" + fps + ",scale=1280:-2,format=gray"
	if err := run(ctx, opts.FFmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-t", strconv.Itoa(opts.HeadMinutes*60), "-i", file, "-vf", vf, "-an", filepath.Join(dir, "h_%05d.png")); err != nil {
		return nil, fmt.Errorf("extract head frames: %w", err)
	}
	if opts.TailMinutes > 0 {
		if err := run(ctx, opts.FFmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-sseof", strconv.Itoa(-opts.TailMinutes*60), "-i", file, "-vf", vf, "-an", filepath.Join(dir, "t_%05d.png")); err != nil {
			// The tail is a bonus; a seek failure should not sink the head.
			if opts.Logger != nil {
				opts.Logger.Warn("ocr: tail frames", "err", err)
			}
		}
	}
	frames, _ := filepath.Glob(filepath.Join(dir, "*.png"))
	if len(frames) == 0 {
		return nil, errors.New("no frames extracted")
	}

	type lineHit struct {
		text   string
		height float64
	}
	results := make([][]lineHit, len(frames))
	var wg sync.WaitGroup
	sem := make(chan struct{}, opts.Workers)
	for i, f := range frames {
		wg.Add(1)
		go func(i int, f string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			out, err := exec.CommandContext(ctx, opts.Tesseract, f, "stdout", "--psm", "11", "-l", opts.Languages, "tsv").Output()
			if err != nil {
				return
			}
			for _, l := range parseTSV(string(out)) {
				results[i] = append(results[i], lineHit{l.text, l.height})
			}
		}(i, f)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	agg := map[string]*Candidate{}
	for _, hits := range results {
		seen := map[string]bool{}
		for _, h := range hits {
			key := normalize(h.text)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			c, ok := agg[key]
			if !ok {
				c = &Candidate{Text: metadata.TitleCase(key)}
				agg[key] = c
			}
			c.Frames++
			if h.height > c.Height {
				c.Height = h.height
			}
		}
	}
	var out []Candidate
	for _, c := range agg {
		// Big text that stays on screen is a title card; tiny text that
		// flickers past is dialogue or credits.
		c.Score = float64(c.Frames) * (1 + 25*c.Height)
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Text < out[j].Text
	})
	if len(out) > opts.MaxCandidates {
		out = out[:opts.MaxCandidates]
	}
	return out, nil
}

func run(ctx context.Context, bin string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return fmt.Errorf("%s: %w: %s", bin, err, msg)
	}
	return nil
}

type tsvLine struct {
	text   string
	height float64
}

// parseTSV turns tesseract's TSV output into lines with an average
// confidence filter and the line height as a fraction of the page.
func parseTSV(s string) []tsvLine {
	rows := strings.Split(s, "\n")
	if len(rows) < 2 {
		return nil
	}
	type acc struct {
		words []string
		conf  float64
		n     int
		h     int
	}
	lines := map[string]*acc{}
	var order []string
	pageH := 0
	for _, row := range rows[1:] {
		f := strings.Split(strings.TrimRight(row, "\r"), "\t")
		if len(f) < 12 {
			continue
		}
		level, _ := strconv.Atoi(f[0])
		height, _ := strconv.Atoi(f[9])
		if level == 1 && height > pageH {
			pageH = height
		}
		if level != 5 {
			continue
		}
		conf, _ := strconv.ParseFloat(f[10], 64)
		word := strings.TrimSpace(f[11])
		if word == "" || conf < 0 {
			continue
		}
		key := f[2] + ":" + f[3] + ":" + f[4]
		a, ok := lines[key]
		if !ok {
			a = &acc{}
			lines[key] = a
			order = append(order, key)
		}
		a.words = append(a.words, word)
		a.conf += conf
		a.n++
		if height > a.h {
			a.h = height
		}
	}
	var out []tsvLine
	for _, k := range order {
		a := lines[k]
		if a.n == 0 || a.conf/float64(a.n) < 60 {
			continue
		}
		text := strings.Join(a.words, " ")
		if letters(text) < 3 {
			continue
		}
		h := 0.0
		if pageH > 0 {
			h = float64(a.h) / float64(pageH)
		}
		out = append(out, tsvLine{text: text, height: h})
	}
	return out
}

var reJunk = regexp.MustCompile(`[^\p{L}\p{N}' &:!-]+`)

func letters(s string) int {
	n := 0
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127 {
			n++
		}
	}
	return n
}

func normalize(s string) string {
	s = reJunk.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(strings.ToLower(s)), " ")
	s = strings.Trim(s, " -:'&!")
	if letters(s) < 3 || len(s) > 60 {
		return ""
	}
	return s
}

// Match is what Identify found.
type Match struct {
	Identity   *metadata.Identity
	Candidate  Candidate
	Candidates []Candidate
}

// Identify runs Extract and tries each candidate against the provider. A
// match is accepted only when the title similarity is high and the known
// runtime agrees with the ripped title's duration.
func Identify(ctx context.Context, file string, titleDuration time.Duration, tolerance time.Duration, provider metadata.Provider, disc metadata.DiscHints, opts Options) (*Match, error) {
	cands, err := Extract(ctx, file, opts)
	if err != nil {
		return nil, err
	}
	m := &Match{Candidates: cands}
	if tolerance <= 0 {
		tolerance = 8 * time.Minute
	}
	for _, c := range cands {
		hint := metadata.ParseLabel(c.Text)
		if hint.Junk || letters(hint.Query) < 3 {
			continue
		}
		id, err := provider.Identify(ctx, hint, disc)
		if err != nil || !id.Identified() || id.Confidence < 0.8 {
			continue
		}
		if runtimeAgrees(id, titleDuration, tolerance) {
			id.Source = "ocr+" + id.Source
			m.Identity = id
			m.Candidate = c
			return m, nil
		}
		if opts.Logger != nil {
			opts.Logger.Info("ocr: title matched but runtime did not", "text", c.Text, "title", id.Title, "runtime", id.Runtime, "ripped", titleDuration)
		}
	}
	return m, nil
}

func runtimeAgrees(id *metadata.Identity, d, tol time.Duration) bool {
	if d <= 0 {
		return false
	}
	switch id.Kind {
	case metadata.KindMovie:
		if id.Runtime <= 0 {
			return false
		}
		return absDur(id.Runtime-d) <= tol
	case metadata.KindTV:
		if id.EpisodeRuntime <= 0 {
			return false
		}
		r := float64(id.EpisodeRuntime)
		x := float64(d)
		return (x >= r*0.65 && x <= r*1.35) || (x >= r*1.7 && x <= r*2.35)
	}
	return false
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
