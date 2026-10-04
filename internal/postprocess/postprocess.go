// Package postprocess optionally remuxes the MakeMKV output. Stream copy
// only: bit rates never change unless the user supplies a custom command.
package postprocess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Options controls the pass.
type Options struct {
	Mode          string   // none | remux | custom
	Tool          string   // auto | mkvmerge | ffmpeg
	Languages     []string // keep only these audio/subtitle languages; empty = all
	SetTitle      bool
	CustomCommand []string
	CustomExt     string
	Timeout       time.Duration
	Logger        *slog.Logger
}

// Result describes what happened.
type Result struct {
	Output  string
	Tool    string
	Skipped bool
	Reason  string
}

// Run post-processes input and returns the file to deliver. When nothing needs
// doing the input is returned untouched.
func Run(ctx context.Context, input, title string, opts Options) (Result, error) {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	switch opts.Mode {
	case "", "none":
		return Result{Output: input, Skipped: true, Reason: "disabled"}, nil
	case "custom":
		return runCustom(ctx, input, title, opts)
	}
	tool := opts.Tool
	if tool == "" || tool == "auto" {
		switch {
		case has("mkvmerge"):
			tool = "mkvmerge"
		case has("ffmpeg"):
			tool = "ffmpeg"
		default:
			return Result{Output: input, Skipped: true, Reason: "no mkvmerge or ffmpeg"}, nil
		}
	}
	if !has(tool) {
		return Result{Output: input, Skipped: true, Reason: tool + " not installed"}, nil
	}
	if len(opts.Languages) == 0 && (!opts.SetTitle || title == "") {
		return Result{Output: input, Skipped: true, Reason: "nothing to change"}, nil
	}
	// Metadata-only change: mkvpropedit is instant and in place.
	if len(opts.Languages) == 0 && tool == "mkvmerge" && has("mkvpropedit") {
		cmd := exec.CommandContext(ctx, "mkvpropedit", input, "--edit", "info", "--set", "title="+title)
		if out, err := cmd.CombinedOutput(); err != nil {
			return Result{}, fmt.Errorf("mkvpropedit: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return Result{Output: input, Tool: "mkvpropedit"}, nil
	}
	output := strings.TrimSuffix(input, filepath.Ext(input)) + ".remux.mkv"
	var err error
	switch tool {
	case "mkvmerge":
		err = runMkvmerge(ctx, input, output, title, opts)
	case "ffmpeg":
		err = runFFmpeg(ctx, input, output, title, opts)
	default:
		return Result{}, fmt.Errorf("unknown tool %q", tool)
	}
	if err != nil {
		_ = os.Remove(output)
		return Result{}, err
	}
	if st, e := os.Stat(output); e != nil || st.Size() == 0 {
		_ = os.Remove(output)
		return Result{}, errors.New(tool + " produced no output")
	}
	_ = os.Remove(input)
	return Result{Output: output, Tool: tool}, nil
}

func has(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

type track struct {
	ID   int
	Type string
	Lang string
}

func identifyMkvmerge(ctx context.Context, input string) ([]track, error) {
	out, err := exec.CommandContext(ctx, "mkvmerge", "-J", input).Output()
	if err != nil {
		return nil, fmt.Errorf("mkvmerge -J: %w", err)
	}
	var doc struct {
		Tracks []struct {
			ID         int    `json:"id"`
			Type       string `json:"type"`
			Properties struct {
				Language     string `json:"language"`
				LanguageIETF string `json:"language_ietf"`
			} `json:"properties"`
		} `json:"tracks"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	var tracks []track
	for _, t := range doc.Tracks {
		lang := t.Properties.Language
		if lang == "" {
			lang = t.Properties.LanguageIETF
		}
		tracks = append(tracks, track{ID: t.ID, Type: t.Type, Lang: lang})
	}
	return tracks, nil
}

func identifyFFprobe(ctx context.Context, input string) ([]track, error) {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_streams", "-of", "json", input).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	var doc struct {
		Streams []struct {
			Index     int    `json:"index"`
			CodecType string `json:"codec_type"`
			Tags      struct {
				Language string `json:"language"`
			} `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	var tracks []track
	for _, s := range doc.Streams {
		tracks = append(tracks, track{ID: s.Index, Type: s.CodecType, Lang: s.Tags.Language})
	}
	return tracks, nil
}

// keep decides whether a track of a language survives the filter. Video and
// untagged tracks always stay.
func keep(t track, langs []string) bool {
	if t.Type == "video" {
		return true
	}
	l := strings.ToLower(t.Lang)
	if l == "" || l == "und" {
		return true
	}
	for _, want := range langs {
		if l == want || strings.HasPrefix(l, want+"-") || alias(l) == alias(want) {
			return true
		}
	}
	return false
}

// alias maps ISO 639-1 to 639-2 for the common cases users type.
func alias(l string) string {
	switch l {
	case "en", "eng":
		return "eng"
	case "de", "ger", "deu":
		return "deu"
	case "fr", "fre", "fra":
		return "fra"
	case "es", "spa":
		return "spa"
	case "it", "ita":
		return "ita"
	case "ja", "jpn":
		return "jpn"
	case "pt", "por":
		return "por"
	case "nl", "dut", "nld":
		return "nld"
	case "zh", "chi", "zho":
		return "zho"
	case "ko", "kor":
		return "kor"
	case "ru", "rus":
		return "rus"
	case "sv", "swe":
		return "swe"
	case "da", "dan":
		return "dan"
	case "no", "nor":
		return "nor"
	case "fi", "fin":
		return "fin"
	case "pl", "pol":
		return "pol"
	}
	return l
}

func runMkvmerge(ctx context.Context, input, output, title string, opts Options) error {
	args := []string{"-o", output}
	if opts.SetTitle && title != "" {
		args = append(args, "--title", title)
	}
	if len(opts.Languages) > 0 {
		tracks, err := identifyMkvmerge(ctx, input)
		if err != nil {
			return err
		}
		var audio, subs []string
		for _, t := range tracks {
			if !keep(t, opts.Languages) {
				continue
			}
			switch t.Type {
			case "audio":
				audio = append(audio, strconv.Itoa(t.ID))
			case "subtitles":
				subs = append(subs, strconv.Itoa(t.ID))
			}
		}
		if len(audio) == 0 {
			// Never ship a silent file: keep every audio track instead.
			if opts.Logger != nil {
				opts.Logger.Warn("language filter would remove all audio; keeping all", "file", input)
			}
		} else {
			args = append(args, "--audio-tracks", strings.Join(audio, ","))
		}
		if len(subs) == 0 {
			args = append(args, "--no-subtitles")
		} else {
			args = append(args, "--subtitle-tracks", strings.Join(subs, ","))
		}
	}
	args = append(args, input)
	cmd := exec.CommandContext(ctx, "mkvmerge", args...)
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Exit status 1 means warnings only.
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			if opts.Logger != nil {
				opts.Logger.Warn("mkvmerge warnings", "output", tail(string(out)))
			}
			return nil
		}
		return fmt.Errorf("mkvmerge: %w: %s", err, tail(string(out)))
	}
	return nil
}

func runFFmpeg(ctx context.Context, input, output, title string, opts Options) error {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-i", input, "-map_chapters", "0", "-map_metadata", "0"}
	if len(opts.Languages) > 0 {
		tracks, err := identifyFFprobe(ctx, input)
		if err != nil {
			return err
		}
		hasAudio := false
		for _, t := range tracks {
			if t.Type == "audio" && keep(t, opts.Languages) {
				hasAudio = true
			}
		}
		for _, t := range tracks {
			switch t.Type {
			case "video":
				args = append(args, "-map", "0:"+strconv.Itoa(t.ID))
			case "audio":
				if !hasAudio || keep(t, opts.Languages) {
					args = append(args, "-map", "0:"+strconv.Itoa(t.ID))
				}
			case "subtitle":
				if keep(t, opts.Languages) {
					args = append(args, "-map", "0:"+strconv.Itoa(t.ID))
				}
			}
		}
	} else {
		args = append(args, "-map", "0")
	}
	args = append(args, "-c", "copy")
	if opts.SetTitle && title != "" {
		args = append(args, "-metadata", "title="+title)
	}
	args = append(args, output)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.WaitDelay = 10 * time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, tail(string(out)))
	}
	return nil
}

func runCustom(ctx context.Context, input, title string, opts Options) (Result, error) {
	if len(opts.CustomCommand) == 0 {
		return Result{}, errors.New("custom command not configured")
	}
	ext := strings.TrimPrefix(opts.CustomExt, ".")
	if ext == "" {
		ext = "mkv"
	}
	output := strings.TrimSuffix(input, filepath.Ext(input)) + ".out." + ext
	args := make([]string, 0, len(opts.CustomCommand))
	for _, a := range opts.CustomCommand {
		a = strings.ReplaceAll(a, "{input}", input)
		a = strings.ReplaceAll(a, "{output}", output)
		a = strings.ReplaceAll(a, "{title}", title)
		args = append(args, a)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(output)
		return Result{}, fmt.Errorf("custom command: %w: %s", err, tail(string(out)))
	}
	if st, e := os.Stat(output); e != nil || st.Size() == 0 {
		return Result{}, errors.New("custom command produced no output")
	}
	_ = os.Remove(input)
	return Result{Output: output, Tool: args[0]}, nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	lines := strings.Split(s, "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return strings.Join(lines, " | ")
}
