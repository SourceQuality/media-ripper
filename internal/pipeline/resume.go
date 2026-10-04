package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sourcequality/media-ripper/internal/config"
)

// resumeFile records, per disc, which titles are ripped into the workspace
// and which are already delivered. It lives in the disc's workspace.
const resumeFile = "resume.json"

type resumeState struct {
	Fingerprint string                `json:"fingerprint"`
	Label       string                `json:"label,omitempty"`
	Updated     time.Time             `json:"updated"`
	Titles      map[int]*resumedTitle `json:"titles"`
	// JobID and Started identify the attempt; one cut short by a restart
	// (Interrupted) is carried on under the same id.
	JobID       string    `json:"job_id,omitempty"`
	Started     time.Time `json:"started,omitempty"`
	Interrupted bool      `json:"interrupted,omitempty"`
}

type resumedTitle struct {
	// Ripped is the MakeMKV output in the workspace, once the rip succeeded.
	Ripped string `json:"ripped,omitempty"`
	// Delivered is where the title went in the library or staging folder.
	Delivered *Output `json:"delivered,omitempty"`
}

// resume tracks one job's progress on disk so a later attempt on the same
// disc can pick up where this one stopped. Safe for concurrent use by the
// ripper and the delivery worker.
type resume struct {
	mu    sync.Mutex
	dir   string
	state resumeState
}

// workDir is the job's workspace: one folder per disc so a retry finds the
// titles an earlier attempt ripped.
func workDir(cfg *config.Config, job *Job) string {
	name := job.Fingerprint
	if name == "" || strings.ContainsAny(name, `/\.`) {
		name = job.ID
	}
	return filepath.Join(cfg.RipDir(), name)
}

// openResume loads the record in dir, or starts a fresh one when there is
// none, it belongs to another disc, or resume is off.
func openResume(dir string, job *Job, enabled bool) *resume {
	r := &resume{dir: dir, state: resumeState{Fingerprint: job.Fingerprint, Label: job.Label, Titles: map[int]*resumedTitle{}}}
	if !enabled {
		return r
	}
	data, err := os.ReadFile(filepath.Join(dir, resumeFile))
	if err != nil {
		return r
	}
	var st resumeState
	if json.Unmarshal(data, &st) != nil || st.Fingerprint != job.Fingerprint || st.Titles == nil {
		return r
	}
	r.state = st
	return r
}

// resumable reports whether anything usable was carried over.
func (r *resume) resumable() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.state.Titles) > 0
}

// delivered returns the earlier delivery of a title if the file is still
// there at its recorded size.
func (r *resume) delivered(titleID int) (Output, bool) {
	r.mu.Lock()
	t := r.state.Titles[titleID]
	r.mu.Unlock()
	if t == nil || t.Delivered == nil {
		return Output{}, false
	}
	st, err := os.Stat(t.Delivered.Path)
	if err != nil || st.Size() != t.Delivered.Size {
		return Output{}, false
	}
	return *t.Delivered, true
}

// ripped returns an earlier rip of a title if the file is still there.
func (r *resume) ripped(titleID int) (string, bool) {
	r.mu.Lock()
	t := r.state.Titles[titleID]
	r.mu.Unlock()
	if t == nil || t.Ripped == "" {
		return "", false
	}
	if st, err := os.Stat(t.Ripped); err != nil || st.Size() == 0 {
		return "", false
	}
	return t.Ripped, true
}

func (r *resume) markRipped(titleID int, file string) error {
	return r.update(titleID, func(t *resumedTitle) { t.Ripped = file })
}

func (r *resume) markDelivered(titleID int, out Output) error {
	return r.update(titleID, func(t *resumedTitle) { t.Ripped, t.Delivered = "", &out })
}

// restarted returns the id and start of an attempt that a restart cut
// short, which this attempt continues.
func (r *resume) restarted() (string, time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.JobID, r.state.Started, r.state.Interrupted && r.state.JobID != ""
}

// begin records the attempt now running, so the workspace is kept for it
// from the start (not only once a title is ripped).
func (r *resume) begin(jobID string, started time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.JobID, r.state.Started, r.state.Interrupted = jobID, started, false
	return r.saveLocked()
}

func (r *resume) update(titleID int, fn func(t *resumedTitle)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.state.Titles[titleID]
	if t == nil {
		t = &resumedTitle{}
		r.state.Titles[titleID] = t
	}
	fn(t)
	return r.saveLocked()
}

func (r *resume) saveLocked() error {
	r.state.Updated = time.Now()
	data, err := json.MarshalIndent(r.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(r.dir, resumeFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write resume state: %w", err)
	}
	return os.Rename(tmp, filepath.Join(r.dir, resumeFile))
}

// staleWorkspace reports whether a workspace left from an earlier run should
// be removed at startup: it has no resume record, or one older than maxAge.
func staleWorkspace(dir string, resumeOn bool, maxAge time.Duration, now time.Time) bool {
	if !resumeOn {
		return true
	}
	data, err := os.ReadFile(filepath.Join(dir, resumeFile))
	if errors.Is(err, os.ErrNotExist) || err != nil {
		return true
	}
	var st resumeState
	if json.Unmarshal(data, &st) != nil {
		return true
	}
	return now.Sub(st.Updated) > maxAge
}
