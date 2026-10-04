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
	StageHandoff     Stage = "handoff"
	StageDelivering  Stage = "delivering"
	StageDone        Stage = "done"
	StageFailed      Stage = "failed"
	StageSkipped     Stage = "skipped"
	StageCancelled   Stage = "cancelled"
	// StageReview: delivered to staging, waiting for a person to confirm
	// the titles before Radarr/Sonarr import them. The drive is free.
	StageReview Stage = "review"
)

// Terminal reports whether the stage is final.
func (s Stage) Terminal() bool {
	switch s {
	case StageDone, StageFailed, StageSkipped, StageCancelled, StageReview:
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
	// Import is what Radarr/Sonarr did with the file: "imported", or
	// "not imported: <reason>". Empty when no app takes the files.
	Import string `json:"import,omitempty"`
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
	Warnings  []string            `json:"warnings,omitempty"`
	Ejected   bool                `json:"ejected,omitempty"`
	Log       []LogLine           `json:"log,omitempty"`
	Current   int                 `json:"current,omitempty"` // 1-based pick in progress
	Total     int                 `json:"total,omitempty"`   // number of picks
	// Catalog names the catalogue entry the titles were taken from, and
	// how many scanned titles matched it exactly out of those compared.
	Catalog         string `json:"catalog,omitempty"`
	CatalogMatched  int    `json:"catalog_matched,omitempty"`
	CatalogCompared int    `json:"catalog_compared,omitempty"`
	// Verification says how far the titles can be trusted, for people.
	Verification string `json:"verification,omitempty"`
	// Activities are the steps moving data right now. A rip and the
	// delivery of an earlier title can run at the same time.
	Activities []Activity `json:"activities,omitempty"`

	cancel func()
	// ripping is set while titles are still being read from the disc and
	// earlier ones are remuxed and delivered alongside; their steps go to
	// the job log so the stage keeps showing the rip.
	ripping bool
	rt      *runtime
}

// Activity is one data-moving step in progress, with its throughput.
type Activity struct {
	Kind       string  `json:"kind"` // rip | remux | copy
	Item       string  `json:"item"` // "S01E03", "title 2"
	Done       int64   `json:"done"`
	Total      int64   `json:"total,omitempty"` // 0 when the final size is unknown
	Percent    float64 `json:"percent"`         // -1 when unknown
	Speed      float64 `json:"speed"`           // bytes per second over the last few seconds
	ETASeconds int64   `json:"eta_seconds,omitempty"`
}

// track reports progress of one activity; done and total are bytes. It
// returns the speed and ETA so callers can mirror them elsewhere.
func (j *Job) track(m *meter, kind, item string, done, total int64) (float64, time.Duration) {
	speed, eta := m.add(done, total)
	a := Activity{Kind: kind, Item: item, Done: done, Total: total, Percent: -1, Speed: speed, ETASeconds: int64(eta / time.Second)}
	if total > 0 {
		a.Percent = float64(done) / float64(total) * 100
		if a.Percent > 100 {
			a.Percent = 100
		}
	}
	j.set(func(j *Job) {
		for i := range j.Activities {
			if j.Activities[i].Kind == kind {
				j.Activities[i] = a
				return
			}
		}
		j.Activities = append(j.Activities, a)
	})
	return speed, eta
}

// untrack removes a finished activity.
func (j *Job) untrack(kind string) {
	j.set(func(j *Job) {
		out := j.Activities[:0]
		for _, a := range j.Activities {
			if a.Kind != kind {
				out = append(out, a)
			}
		}
		j.Activities = out
	})
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

func (j *Job) warn(msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Warnings = append(j.Warnings, msg)
}

func (j *Job) set(fn func(j *Job)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	fn(j)
}

// stepStage sets the stage for a remux or delivery step, or only logs it
// while a rip of a later title is still running.
func (j *Job) stepStage(s Stage, msg string) {
	j.mu.Lock()
	ripping := j.ripping
	j.mu.Unlock()
	if ripping {
		j.logf("%s", msg)
		return
	}
	j.setStage(s, msg)
}

func (j *Job) setStage(s Stage, msg string) {
	j.set(func(j *Job) {
		j.Stage = s
		j.Message = msg
		j.Progress = -1
		j.ETA = ""
		if s.Terminal() {
			j.Activities = nil
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
		Identity: j.Identity, Selection: j.Selection, Error: j.Error, Current: j.Current, Total: j.Total, Ejected: j.Ejected, Catalog: j.Catalog,
		CatalogMatched: j.CatalogMatched, CatalogCompared: j.CatalogCompared,
	}
	c.Warnings = append([]string(nil), j.Warnings...)
	c.Activities = append([]Activity(nil), j.Activities...)
	c.Verification = verification(c)
	if !c.Stage.Terminal() {
		c.Elapsed = time.Since(j.StartedAt).Round(time.Second).String()
	}
	c.Titles = append([]TitleSummary(nil), j.Titles...)
	c.Outputs = append([]Output(nil), j.Outputs...)
	c.Log = append([]LogLine(nil), j.Log...)
	c.mu = nil
	return c
}
