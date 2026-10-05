package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Previews of a disc being labelled are read straight from the disc by
// ffmpeg through libbluray, which MakeMKV's libmmbd decrypts for (the same
// way video players use it). One read at a time per manager: the drive
// has one head.

var (
	previewOnce sync.Once
	previewErr  error
)

// previewEnv points libbluray at MakeMKV's libmmbd for decryption.
func previewEnv() []string {
	return append(os.Environ(), "LIBAACS_PATH=libmmbd", "LIBBDPLUS_PATH=libmmbd")
}

func (m *Manager) ffmpeg() string {
	if m.deps.FFmpeg != "" {
		return m.deps.FFmpeg
	}
	return "ffmpeg"
}

// PreviewsAvailable reports why previews cannot be made, if they cannot:
// ffmpeg needs Blu-ray (libbluray) support.
func (m *Manager) PreviewsAvailable() error {
	if m.deps.FFmpeg != "" {
		return nil // tests
	}
	previewOnce.Do(func() {
		out, err := exec.Command(m.ffmpeg(), "-hide_banner", "-protocols").Output()
		switch {
		case err != nil:
			previewErr = fmt.Errorf("ffmpeg not found: %w", err)
		case !bytes.Contains(out, []byte("bluray")):
			previewErr = errors.New("this ffmpeg was built without Blu-ray (libbluray) support")
		}
	})
	return previewErr
}

// playlistOf returns the playlist number of a title from its source
// ("00800.mpls" → 800). Titles that are a lone clip have none.
func playlistOf(source string) (int, bool) {
	name, ok := strings.CutSuffix(strings.ToLower(source), ".mpls")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(name)
	return n, err == nil
}

// previewTitle finds a title of a disc being labelled.
func (m *Manager) previewTitle(id string, titleID int) (*Job, TitleSummary, int, error) {
	job := m.recentJob(id)
	if job == nil || job.Snapshot().Stage != StageLabelling {
		return nil, TitleSummary{}, 0, errors.New("previews are made while a disc waits to be labelled")
	}
	for _, t := range job.Snapshot().Titles {
		if t.ID != titleID {
			continue
		}
		pl, ok := playlistOf(t.Source)
		if !ok {
			return nil, TitleSummary{}, 0, errors.New("this title is a lone clip, outside any playlist: no preview")
		}
		return job, t, pl, nil
	}
	return nil, TitleSummary{}, 0, errors.New("no such title")
}

func (m *Manager) thumbFile(id string, titleID int) string {
	return filepath.Join(m.Config().StateDir(), "previews", filepath.Base(id), strconv.Itoa(titleID)+".jpg")
}

// Thumbnail is a still from a title of a disc being labelled, a third of
// the way in; made once and kept until the job ends.
func (m *Manager) Thumbnail(ctx context.Context, id string, titleID int) ([]byte, error) {
	if b, err := os.ReadFile(m.thumbFile(id, titleID)); err == nil {
		return b, nil
	}
	if err := m.PreviewsAvailable(); err != nil {
		return nil, err
	}
	job, t, pl, err := m.previewTitle(id, titleID)
	if err != nil {
		return nil, err
	}
	ctx, done := m.trackPreview(ctx)
	defer done()
	m.discRead.Lock()
	defer m.discRead.Unlock()
	if b, err := os.ReadFile(m.thumbFile(id, titleID)); err == nil {
		return b, nil // made while this waited
	}
	if job.Snapshot().Stage != StageLabelling {
		return nil, errors.New("the disc is no longer waiting to be labelled")
	}
	at := max(t.Duration/3, time.Second)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// Keyframes only: a frame decoded without its reference is grey.
	cmd := exec.CommandContext(ctx, m.ffmpeg(), "-hide_banner", "-loglevel", "error", "-skip_frame", "nokey",
		"-playlist", strconv.Itoa(pl), "-ss", strconv.FormatFloat(at.Seconds(), 'f', 0, 64), "-i", "bluray:"+job.Drive,
		"-frames:v", "1", "-vf", "scale=480:-2", "-f", "image2", "-c:v", "mjpeg", "pipe:1")
	cmd.Env = previewEnv()
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return nil, fmt.Errorf("ffmpeg: %v %s", err, lastLine(errb.String()))
	}
	p := m.thumbFile(id, titleID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
		_ = atomicWriteFile(p, out)
	}
	return out, nil
}

// makeThumbnails prepares a still of every title while the disc waits.
func (m *Manager) makeThumbnails(ctx context.Context, job *Job) {
	if m.PreviewsAvailable() != nil {
		return
	}
	for _, t := range job.Snapshot().Titles {
		if ctx.Err() != nil || job.Snapshot().Stage != StageLabelling {
			return
		}
		if _, ok := playlistOf(t.Source); !ok {
			continue
		}
		if _, err := m.Thumbnail(ctx, job.ID, t.ID); err != nil {
			job.logf("preview of title %d: %v", t.ID, err)
		}
	}
}

// trackPreview makes a disc read cancellable by stopPreviews.
func (m *Manager) trackPreview(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	if m.previews == nil {
		m.previews = map[*context.CancelFunc]bool{}
	}
	m.previews[&cancel] = true
	m.mu.Unlock()
	return ctx, func() {
		m.mu.Lock()
		delete(m.previews, &cancel)
		m.mu.Unlock()
		cancel()
	}
}

// stopPreviews ends every preview read of the disc and waits until none
// runs, so ripping has the drive to itself.
func (m *Manager) stopPreviews() {
	m.mu.Lock()
	for c := range m.previews {
		(*c)()
	}
	m.mu.Unlock()
	m.discRead.Lock()
	m.discRead.Unlock() //nolint:staticcheck // waiting for the reader
}

// is4K reports whether a title is UHD, too heavy to convert live here.
func is4K(t TitleSummary) bool {
	return strings.Contains(t.Video, "2160") || strings.Contains(t.Video, "3840")
}

// Preview streams a title of a disc being labelled, from start, as a
// small browser-playable MP4 (H.264, AAC), converted live.
func (m *Manager) Preview(ctx context.Context, id string, titleID int, start time.Duration, w io.Writer) error {
	if err := m.PreviewsAvailable(); err != nil {
		return err
	}
	job, t, pl, err := m.previewTitle(id, titleID)
	if err != nil {
		return err
	}
	if is4K(t) {
		return errors.New("4K titles are too heavy to convert live on this machine; use the stills")
	}
	ctx, done := m.trackPreview(ctx)
	defer done()
	m.discRead.Lock()
	defer m.discRead.Unlock()
	if job.Snapshot().Stage != StageLabelling {
		return errors.New("the disc is no longer waiting to be labelled")
	}
	cmd := exec.CommandContext(ctx, m.ffmpeg(), "-hide_banner", "-loglevel", "error",
		"-playlist", strconv.Itoa(pl), "-ss", strconv.FormatFloat(start.Seconds(), 'f', 0, 64), "-i", "bluray:"+job.Drive,
		"-t", "600", "-map", "0:v:0", "-map", "0:a:0?", "-vf", "scale=-2:540",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "28", "-g", "48",
		"-c:a", "aac", "-b:a", "128k", "-ac", "2",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "pipe:1")
	cmd.Env = previewEnv()
	cmd.Stdout = w
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("ffmpeg: %v %s", err, lastLine(errb.String()))
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
