// Package makemkv wraps makemkvcon in robot mode: scanning a disc for titles
// and ripping a title to MKV without re-encoding.
package makemkv

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Message is a MakeMKV log line.
type Message struct {
	Code int    `json:"code"`
	Text string `json:"text"`
}

// Stream is one audio/video/subtitle track inside a title.
type Stream struct {
	Index     int    `json:"index"`
	Type      string `json:"type"` // Video, Audio, Subtitles
	Name      string `json:"name,omitempty"`
	Lang      string `json:"lang,omitempty"`
	LangName  string `json:"lang_name,omitempty"`
	CodecID   string `json:"codec_id,omitempty"`
	Codec     string `json:"codec,omitempty"`
	CodecLong string `json:"codec_long,omitempty"`
	Bitrate   string `json:"bitrate,omitempty"`
	Channels  int    `json:"channels,omitempty"`
	VideoSize string `json:"video_size,omitempty"`
	Aspect    string `json:"aspect,omitempty"`
	FrameRate string `json:"frame_rate,omitempty"`
	Flags     int    `json:"flags,omitempty"`
}

// Title is one rippable title (a playlist on Blu-ray, a PGC on DVD).
type Title struct {
	ID            int           `json:"id"`
	Name          string        `json:"name,omitempty"`
	Chapters      int           `json:"chapters"`
	Duration      time.Duration `json:"duration"`
	Size          string        `json:"size,omitempty"`
	SizeBytes     int64         `json:"size_bytes"`
	AngleInfo     string        `json:"angle_info,omitempty"`
	SourceFile    string        `json:"source_file,omitempty"`
	SegmentsCount int           `json:"segments_count"`
	SegmentsMap   string        `json:"segments_map,omitempty"`
	Segments      []int         `json:"segments,omitempty"`
	OutputFile    string        `json:"output_file,omitempty"`
	OrderWeight   int           `json:"order_weight,omitempty"`
	Comment       string        `json:"comment,omitempty"`
	OriginalID    int           `json:"original_id,omitempty"`
	Streams       []Stream      `json:"streams,omitempty"`
}

// Video returns the first video stream, if any.
func (t *Title) Video() *Stream {
	for i := range t.Streams {
		if t.Streams[i].Type == "Video" {
			return &t.Streams[i]
		}
	}
	return nil
}

// AudioCount and SubtitleCount help the UI and selection heuristics.
func (t *Title) AudioCount() int    { return t.count("Audio") }
func (t *Title) SubtitleCount() int { return t.count("Subtitles") }

func (t *Title) count(kind string) int {
	n := 0
	for _, s := range t.Streams {
		if s.Type == kind {
			n++
		}
	}
	return n
}

// Disc is the result of a scan.
type Disc struct {
	Type       string    `json:"type"` // "Blu-ray disc", "DVD disc", ...
	Name       string    `json:"name,omitempty"`
	Volume     string    `json:"volume,omitempty"`
	MetaLang   string    `json:"meta_lang,omitempty"`
	TitleCount int       `json:"title_count"`
	Titles     []*Title  `json:"titles"`
	Messages   []Message `json:"-"`
}

// IsBluray reports whether MakeMKV identified a Blu-ray (including UHD).
func (d *Disc) IsBluray() bool { return strings.Contains(strings.ToLower(d.Type), "blu") }

// Label returns the best human-facing label MakeMKV knows about.
func (d *Disc) Label() string {
	if d.Name != "" {
		return d.Name
	}
	return d.Volume
}

// Progress is one progress sample during a rip.
type Progress struct {
	Current   int64
	Total     int64
	Max       int64
	Task      string
	Percent   float64
	Elapsed   time.Duration
	Remaining time.Duration
}

// Client runs makemkvcon.
type Client struct {
	Binary      string
	MinLength   int
	ScanTimeout time.Duration
	RipTimeout  time.Duration
	ExtraArgs   []string
	Logger      *slog.Logger
}

// Available reports whether the binary can be found.
func (c *Client) Available() error {
	_, err := exec.LookPath(c.Binary)
	return err
}

func (c *Client) baseArgs() []string {
	args := []string{"-r", "--progress=-same", "--messages=-stdout"}
	if c.MinLength > 0 {
		args = append(args, "--minlength="+strconv.Itoa(c.MinLength))
	}
	args = append(args, c.ExtraArgs...)
	return args
}

// Info scans the disc in device (e.g. /dev/sr0).
func (c *Client) Info(ctx context.Context, device string) (*Disc, error) {
	if c.ScanTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.ScanTimeout)
		defer cancel()
	}
	args := append(c.baseArgs(), "info", "dev:"+device)
	lines, err := c.run(ctx, args, nil)
	disc := parseInfo(lines)
	if err != nil {
		return disc, fmt.Errorf("makemkvcon info: %w%s", err, summarizeErrors(disc.Messages))
	}
	if disc.TitleCount == 0 && len(disc.Titles) == 0 {
		return disc, fmt.Errorf("no titles found%s", summarizeErrors(disc.Messages))
	}
	return disc, nil
}

// Rip saves one title from device into outDir and returns the written file.
func (c *Client) Rip(ctx context.Context, device string, title *Title, outDir string, onProgress func(Progress)) (string, error) {
	if c.RipTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.RipTimeout)
		defer cancel()
	}
	if err := os.MkdirAll(outDir, 0o775); err != nil {
		return "", err
	}
	before, _ := filepath.Glob(filepath.Join(outDir, "*.mkv"))
	seen := map[string]bool{}
	for _, f := range before {
		seen[f] = true
	}

	args := append(c.baseArgs(), "mkv", "dev:"+device, strconv.Itoa(title.ID), outDir)
	start := time.Now()
	var saved, failed int
	var msgs []Message
	_, err := c.run(ctx, args, func(key string, f []string) {
		switch key {
		case "PRGV":
			if len(f) >= 3 && onProgress != nil {
				p := Progress{Current: atoi64(f[0]), Total: atoi64(f[1]), Max: atoi64(f[2]), Elapsed: time.Since(start)}
				if p.Max > 0 {
					p.Percent = float64(p.Total) / float64(p.Max) * 100
					if p.Total > 0 {
						p.Remaining = time.Duration(float64(p.Elapsed) * float64(p.Max-p.Total) / float64(p.Total))
					}
				}
				onProgress(p)
			}
		case "PRGT", "PRGC":
			if len(f) >= 3 && onProgress != nil && key == "PRGC" {
				onProgress(Progress{Task: f[2], Elapsed: time.Since(start), Percent: -1})
			}
		case "MSG":
			if len(f) >= 4 {
				m := Message{Code: atoi(f[0]), Text: f[3]}
				msgs = append(msgs, m)
				switch m.Code {
				case 5004, 5005: // "Copy complete. N titles saved[, M failed]."
					if len(f) >= 6 {
						saved = atoi(f[5])
					}
					if len(f) >= 7 {
						failed = atoi(f[6])
					}
				}
			}
		}
	})
	if err != nil {
		return "", fmt.Errorf("makemkvcon mkv: %w%s", err, summarizeErrors(msgs))
	}
	if failed > 0 || (saved == 0 && !sawSuccess(msgs)) {
		return "", fmt.Errorf("makemkv reported %d saved, %d failed%s", saved, failed, summarizeErrors(msgs))
	}

	// Prefer the output name MakeMKV announced; fall back to whatever is new.
	if title.OutputFile != "" {
		p := filepath.Join(outDir, title.OutputFile)
		if st, err := os.Stat(p); err == nil && st.Size() > 0 {
			return p, nil
		}
	}
	after, _ := filepath.Glob(filepath.Join(outDir, "*.mkv"))
	var newest string
	var newestTime time.Time
	for _, f := range after {
		if seen[f] {
			continue
		}
		if st, err := os.Stat(f); err == nil && st.ModTime().After(newestTime) {
			newest, newestTime = f, st.ModTime()
		}
	}
	if newest == "" {
		return "", errors.New("makemkv finished but no output file was found")
	}
	return newest, nil
}

func sawSuccess(msgs []Message) bool {
	for _, m := range msgs {
		if m.Code == 5004 || m.Code == 5011 || strings.HasPrefix(m.Text, "Copy complete") {
			return true
		}
	}
	return false
}

func summarizeErrors(msgs []Message) string {
	var out []string
	for _, m := range msgs {
		t := strings.ToLower(m.Text)
		if strings.Contains(t, "fail") || strings.Contains(t, "error") || strings.Contains(t, "too old") || strings.Contains(t, "expired") || strings.Contains(t, "not registered") {
			out = append(out, m.Text)
		}
	}
	if len(out) == 0 {
		return ""
	}
	if len(out) > 5 {
		out = out[len(out)-5:]
	}
	return ": " + strings.Join(out, " | ")
}

// run executes makemkvcon, returning every stdout line. onLine, if set, is
// called with the parsed key and fields for each line as it arrives.
func (c *Client) run(ctx context.Context, args []string, onLine func(key string, fields []string)) ([]string, error) {
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	cmd.Stderr = cmd.Stdout
	cmd.WaitDelay = 10 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if c.Logger != nil {
		c.Logger.Debug("exec", "cmd", c.Binary, "args", strings.Join(args, " "))
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", c.Binary, err)
	}
	var lines []string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		lines = append(lines, line)
		if c.Logger != nil {
			c.Logger.Debug("makemkv", "line", line)
		}
		if onLine != nil {
			if key, rest, ok := strings.Cut(line, ":"); ok {
				onLine(key, splitRobot(rest))
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		_ = cmd.Wait()
		return lines, fmt.Errorf("read output: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return lines, ctx.Err()
		}
		return lines, err
	}
	return lines, nil
}

// WriteSettings writes ~/.MakeMKV/settings.conf so makemkvcon has its key and
// track selection without any interactive step. Existing unrelated keys are
// preserved.
func WriteSettings(dir, key, selection string) error {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".MakeMKV")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "settings.conf")
	existing := map[string]string{}
	var order []string
	if data, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			if _, dup := existing[k]; !dup {
				order = append(order, k)
			}
			existing[k] = strings.TrimSpace(v)
		}
	}
	set := func(k, v string) {
		if _, ok := existing[k]; !ok {
			order = append(order, k)
		}
		existing[k] = v
	}
	if key != "" {
		set("app_Key", strconv.Quote(key))
	}
	if selection != "" {
		set("app_DefaultSelectionString", strconv.Quote(selection))
	}
	set("app_ExpertMode", "1")
	set("app_UpdateEnable", "0")
	var b strings.Builder
	for _, k := range order {
		fmt.Fprintf(&b, "%s = %s\n", k, existing[k])
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// SelectionString builds MakeMKV's track selection rule. With no languages
// every audio and subtitle track is kept; otherwise only the listed languages
// plus tracks with no language tag.
func SelectionString(languages []string) string {
	if len(languages) == 0 {
		return "+sel:all,-sel:mvcvideo,=100:all,-10:favlang"
	}
	var langs []string
	for _, l := range languages {
		langs = append(langs, "lang="+l)
	}
	return "-sel:all,+sel:video,+sel:(" + strings.Join(langs, "|") + "|nolang),-sel:mvcvideo,=100:all,-10:favlang"
}

var versionRe = regexp.MustCompile(`v(\d+(?:\.\d+)+)`)

// Probe starts makemkvcon without touching a drive and reports its version.
// A beta that has expired or an invalid stored key is an error: MakeMKV
// would refuse every disc ("This application version is too old").
func (c *Client) Probe(ctx context.Context) (string, error) {
	var version string
	var problems []string
	_, err := c.run(ctx, []string{"-r", "--noscan", "info", "disc:9999"}, func(key string, f []string) {
		if key != "MSG" || len(f) < 4 {
			return
		}
		switch f[0] {
		case "1005": // "MakeMKV v2.0.0 linux(x64-release) started"
			if m := versionRe.FindStringSubmatch(f[3]); m != nil {
				version = m[1]
			}
		case "5020", "5021": // invalid stored key; version too old / expired
			problems = append(problems, strings.TrimSpace(f[3]))
		}
	})
	if version == "" && err != nil {
		return "", fmt.Errorf("run %s: %w", c.Binary, err)
	}
	if len(problems) > 0 {
		return version, fmt.Errorf("MakeMKV %s will not rip: %s Set makemkv.key to your registration key or the current beta key, or update MakeMKV", version, strings.Join(problems, " "))
	}
	// "disc:9999" does not exist, so a non-zero exit is expected here.
	return version, nil
}

// Backup copies the whole disc, decrypted, into outDir (a BDMV or VIDEO_TS
// folder tree that plays like the disc), reporting progress like Rip.
func (c *Client) Backup(ctx context.Context, device, outDir string, onProgress func(Progress)) error {
	if c.RipTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.RipTimeout)
		defer cancel()
	}
	if err := os.MkdirAll(outDir, 0o775); err != nil {
		return err
	}
	args := append(c.baseArgs(), "backup", "--decrypt", "dev:"+device, outDir)
	start := time.Now()
	var last []string // the final messages, to explain a failure
	_, err := c.run(ctx, args, func(key string, f []string) {
		switch key {
		case "PRGV":
			if len(f) >= 3 && onProgress != nil {
				p := Progress{Current: atoi64(f[0]), Total: atoi64(f[1]), Max: atoi64(f[2]), Elapsed: time.Since(start)}
				if p.Max > 0 {
					p.Percent = float64(p.Total) / float64(p.Max) * 100
				}
				onProgress(p)
			}
		case "PRGC":
			if len(f) >= 3 && onProgress != nil {
				onProgress(Progress{Task: f[2], Elapsed: time.Since(start), Percent: -1})
			}
		case "MSG":
			if len(f) >= 4 {
				last = append(last, f[3])
				if len(last) > 3 {
					last = last[1:]
				}
			}
		}
	})
	// The exit status is the verdict: MakeMKV's success message itself
	// says "... 0 failed".
	if err != nil {
		return fmt.Errorf("makemkvcon backup: %w: %s", err, strings.Join(last, "; "))
	}
	return nil
}
