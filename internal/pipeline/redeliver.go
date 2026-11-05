package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sourcequality/media-ripper/internal/notify"
)

// A copy that fails leaves a finished rip in the workspace, so nothing more
// is needed from the disc. Such a job is finished from there: by a person
// (Retry delivery), or by itself when the library storage answers.

// expiryWarning is how long before a kept rip's removal a notice goes out.
const expiryWarning = 24 * time.Hour

var errNothingToDeliver = errors.New("nothing is waiting to be delivered for this disc")

// redeliverable returns the workspace a failed job can be finished from.
func (m *Manager) redeliverable(s Job) (string, error) {
	if s.Stage != StageFailed || s.Selection == nil || len(s.Selection.Picks) == 0 || s.Contribute || s.DryRun {
		return "", errNothingToDeliver
	}
	dir := workDir(m.Config(), &s)
	if _, err := os.Stat(filepath.Join(dir, resumeFile)); err != nil {
		return "", fmt.Errorf("%w: the rip is no longer kept", errNothingToDeliver)
	}
	rs := openResume(dir, &s, true)
	if id, _, _ := rs.restarted(); id != s.ID {
		return "", fmt.Errorf("%w: a later attempt took over its rip", errNothingToDeliver)
	}
	var missing []string
	for _, pick := range s.Selection.Picks {
		if _, ok := rs.delivered(pick.Title.ID); ok {
			continue
		}
		if _, ok := rs.ripped(pick.Title.ID); !ok {
			missing = append(missing, pickName(pick))
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("%s not ripped; insert the disc again and rip it", strings.Join(missing, ", "))
	}
	return dir, nil
}

// CanRetryDelivery reports whether a failed job can be finished without
// its disc right now, for the UI.
func (m *Manager) CanRetryDelivery(s Job) bool {
	if _, err := m.redeliverable(s); err != nil {
		return false
	}
	return !m.finishingDisc(s.Fingerprint)
}

// RetryDelivery finishes a failed job from its workspace in the background;
// copying can take hours. The outcome arrives like any job's.
func (m *Manager) RetryDelivery(id string) (Job, error) {
	m.mu.Lock()
	ctx := m.runCtx
	m.mu.Unlock()
	if ctx == nil {
		return Job{}, errors.New("media-ripper is not running")
	}
	job, rs, snap, err := m.claimRedelivery(id)
	if err != nil {
		return Job{}, err
	}
	m.running.Add(1)
	go func() {
		defer m.running.Done()
		m.completeOffline(ctx, job, rs, snap, "delivering again from the kept rip; the disc is not needed")
	}()
	return job.Snapshot(), nil
}

// claimRedelivery checks a job and marks its disc as being finished, so a
// second press or the same disc put back in waits for this attempt.
func (m *Manager) claimRedelivery(id string) (*Job, *resume, Job, error) {
	s, ok := m.Record(id)
	if !ok {
		return nil, nil, Job{}, errors.New("no such job")
	}
	dir, err := m.redeliverable(s)
	if err != nil {
		return nil, nil, Job{}, err
	}
	rt := m.rt.Load()
	job := m.recentJob(id)
	known := job != nil
	if !known {
		job = new(Job)
		*job = s
		job.mu = &sync.Mutex{}
	}
	job.set(func(j *Job) { j.rt, j.Ejected = rt, true })
	if m.needsOCR(job) {
		return nil, nil, Job{}, errors.New("needs the disc for title-card recognition; insert it again to finish")
	}
	m.mu.Lock()
	for _, j := range m.finishing {
		if j.Fingerprint == s.Fingerprint {
			m.mu.Unlock()
			return nil, nil, Job{}, errors.New("this disc is already being delivered")
		}
	}
	m.finishing = append(m.finishing, job)
	m.mu.Unlock()
	if !known {
		m.remember(job)
	}
	job.set(func(j *Job) {
		j.Outputs, j.Activities, j.Error = nil, nil, ""
		j.Stage = StageDelivering
	})
	return job, openResume(dir, job, true), job.Snapshot(), nil
}

// retryFailedDeliveries finishes, one at a time, the failed jobs whose rip
// is complete. It runs when the library storage becomes usable, which is
// when a copy that could not reach it can succeed.
func (m *Manager) retryFailedDeliveries(ctx context.Context) {
	if !m.redelivering.CompareAndSwap(false, true) {
		return
	}
	defer m.redelivering.Store(false)
	entries, err := os.ReadDir(m.Config().RipDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		st, ok := readResumeState(filepath.Join(m.Config().RipDir(), e.Name()))
		if !ok || st.JobID == "" || st.Interrupted {
			continue // an interrupted job is the restart's to carry on
		}
		job, rs, snap, err := m.claimRedelivery(st.JobID)
		if err != nil {
			continue
		}
		m.log.Info("library storage is usable; delivering a kept rip again", "job", snap.ID, "title", displayTitleSnap(snap))
		m.completeOffline(ctx, job, rs, snap, "the library is reachable again; delivering the kept rip")
	}
}

func readResumeState(dir string) (resumeState, bool) {
	var st resumeState
	data, err := os.ReadFile(filepath.Join(dir, resumeFile))
	if err != nil || json.Unmarshal(data, &st) != nil {
		return st, false
	}
	return st, true
}

// undelivered reports whether a resume record holds a rip that never
// reached the library.
func undelivered(st resumeState) bool {
	for _, t := range st.Titles {
		if t != nil && t.Ripped != "" {
			return true
		}
	}
	return false
}

// watchKeptRips warns before a rip that never reached the library expires.
func (m *Manager) watchKeptRips(ctx context.Context) {
	warned := map[string]bool{}
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		m.warnExpiringRips(ctx, warned, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (m *Manager) warnExpiringRips(ctx context.Context, warned map[string]bool, now time.Time) {
	cfg := m.Config()
	if !cfg.Output.Resume {
		return
	}
	entries, err := os.ReadDir(cfg.RipDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		st, ok := readResumeState(filepath.Join(cfg.RipDir(), e.Name()))
		if !ok || warned[e.Name()] || !undelivered(st) {
			continue
		}
		left := st.Updated.Add(cfg.Output.ResumeMaxAge.D()).Sub(now)
		if left > expiryWarning {
			continue
		}
		warned[e.Name()] = true
		m.log.Warn("kept rip expires soon", "label", st.Label, "job", st.JobID, "expires", st.Updated.Add(cfg.Output.ResumeMaxAge.D()).Format(time.RFC3339))
		ev := notify.Event{Type: "expiring", JobID: st.JobID, Label: st.Label,
			Summary: fmt.Sprintf("This rip never reached the library. It is removed at the first start after %s.", st.Updated.Add(cfg.Output.ResumeMaxAge.D()).Format("Jan 2 15:04 MST"))}
		if s, ok := m.Record(st.JobID); ok {
			ev.Title, ev.RetryDelivery = displayTitleSnap(s), m.CanRetryDelivery(s)
		}
		m.rt.Load().notifier.Send(context.WithoutCancel(ctx), ev)
	}
}
