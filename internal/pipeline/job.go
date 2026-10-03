package pipeline

import (
	"fmt"
	"sync"
	"time"

	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/selector"
)

// Stage is where a job is in the pipeline.
type Stage string

const (
	StageQueued      Stage = "queued"
	StageScanning    Stage = "scanning"
	StageIdentifying Stage = "identifying"
	StageSelecting   Stage = "selecting"
	StageRipping     Stage = "ripping"
	StagePostProcess Stage = "postprocessing"
	StageDelivering  Stage = "delivering"
	StageDone        Stage = "done"
	StageFailed      Stage = "failed"
	StageSkipped     Stage = "skipped"
	StageCancelled   Stage = "cancelled"
)

// Terminal reports whether the stage is final.
func (s Stage) Terminal() bool {
	switch s {
	case StageDone, StageFailed, StageSkipped, StageCancelled:
		return true
	}
	return false
}

// LogLine is one entry in the job log shown in the UI.
type LogLine struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

// Output is one delivered file.
type Output struct {
	Path     string        `json:"path"`
	Size     int64         `json:"size"`
	TitleID  int           `json:"title_id"`
	Duration time.Duration `json:"duration"`
}

// Job tracks one disc from detection to eject. All exported fields are
// snapshotted under the mutex; use Snapshot() to read from other goroutines.
type Job struct {
	mu *sync.Mutex

	ID          string    `json:"id"`
	Drive       string    `json:"drive"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Label       string    `json:"label"`
	DiscType    string    `json:"disc_type,omitempty"`
	Stage       Stage     `json:"stage"`
	Message     string    `json:"message,omitempty"`
	Progress    float64   `json:"progress"` // 0..100 for the current step, -1 when indeterminate
	Overall     float64   `json:"overall"`  // 0..100 across all picks
	ETA         string    `json:"eta,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
	Elapsed     string    `json:"elapsed,omitempty"`
	DryRun      bool      `json:"dry_run,omitempty"`

	Identity  *metadata.Identity  `json:"identity,omitempty"`
	Selection *selector.Selection `json:"selection,omitempty"`
	Titles    []TitleSummary      `json:"titles,omitempty"`
	Outputs   []Output            `json:"outputs,omitempty"`
	Error     string              `json:"error,omitempty"`
	Log       []LogLine           `json:"log,omitempty"`
	Current   int                 `json:"current,omitempty"` // 1-based pick in progress
	Total     int                 `json:"total,omitempty"`   // number of picks

	cancel func()
	rt     *runtime
}

// TitleSummary is a compact view of a scanned title for the UI and history.
type TitleSummary struct {
	ID        int           `json:"id"`
	Duration  time.Duration `json:"duration"`
	Chapters  int           `json:"chapters"`
	Size      string        `json:"size,omitempty"`
	Audio     int           `json:"audio"`
	Subtitles int           `json:"subtitles"`
	Video     string        `json:"video,omitempty"`
	Source    string        `json:"source,omitempty"`
}

func summarize(d *makemkv.Disc) []TitleSummary {
	var out []TitleSummary
	for _, t := range d.Titles {
		s := TitleSummary{ID: t.ID, Duration: t.Duration, Chapters: t.Chapters, Size: t.Size, Audio: t.AudioCount(), Subtitles: t.SubtitleCount(), Source: t.SourceFile}
		if v := t.Video(); v != nil {
			s.Video = v.VideoSize
		}
		out = append(out, s)
	}
	return out
}

func newJob(drive string, rt *runtime) *Job {
	now := time.Now()
	return &Job{
		mu:        &sync.Mutex{},
		rt:        rt,
		ID:        now.Format("20060102-150405") + "-" + fmt.Sprintf("%03d", now.Nanosecond()/1e6),
		Drive:     drive,
		Stage:     StageQueued,
		StartedAt: now,
		Progress:  -1,
	}
}

const maxLog = 300

func (j *Job) logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Log = append(j.Log, LogLine{Time: time.Now(), Message: fmt.Sprintf(format, args...)})
	if len(j.Log) > maxLog {
		j.Log = j.Log[len(j.Log)-maxLog:]
	}
}

func (j *Job) set(fn func(j *Job)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	fn(j)
}

func (j *Job) setStage(s Stage, msg string) {
	j.set(func(j *Job) {
		j.Stage = s
		j.Message = msg
		j.Progress = -1
		j.ETA = ""
		if s.Terminal() {
			j.FinishedAt = time.Now()
			j.Elapsed = j.FinishedAt.Sub(j.StartedAt).Round(time.Second).String()
		}
	})
}

// Snapshot returns a copy safe to serialise.
func (j *Job) Snapshot() Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	c := Job{
		ID: j.ID, Drive: j.Drive, Fingerprint: j.Fingerprint, Label: j.Label, DiscType: j.DiscType,
		Stage: j.Stage, Message: j.Message, Progress: j.Progress, Overall: j.Overall, ETA: j.ETA,
		StartedAt: j.StartedAt, FinishedAt: j.FinishedAt, Elapsed: j.Elapsed, DryRun: j.DryRun,
		Identity: j.Identity, Selection: j.Selection, Error: j.Error, Current: j.Current, Total: j.Total,
	}
	if !c.Stage.Terminal() {
		c.Elapsed = time.Since(j.StartedAt).Round(time.Second).String()
	}
	c.Titles = append([]TitleSummary(nil), j.Titles...)
	c.Outputs = append([]Output(nil), j.Outputs...)
	c.Log = append([]LogLine(nil), j.Log...)
	c.mu = nil
	return c
}
