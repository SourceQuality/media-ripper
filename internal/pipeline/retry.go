package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// importFailed reports whether a finished job left files that Radarr or
// Sonarr did not import, which a retry may still get in.
func importFailed(s Job) bool {
	if s.Stage != StageDone {
		return false
	}
	for _, o := range s.Outputs {
		if strings.HasPrefix(o.Import, "not imported") || strings.HasPrefix(o.Import, "not placed") {
			return true
		}
	}
	return false
}

// RetryImport runs the Radarr/Sonarr import of a finished job again, for
// files that a failed or interrupted import left in staging.
func (m *Manager) RetryImport(ctx context.Context, id string) (Job, error) {
	return m.retryImport(ctx, id, true)
}

// retryImport runs a finished disc's import again. A failure is announced
// only when announce is set: background retries stay quiet until they work.
func (m *Manager) retryImport(ctx context.Context, id string, announce bool) (Job, error) {
	reviewMu.Lock() // imports and approvals share the staging area
	defer reviewMu.Unlock()
	rt := m.rt.Load()
	job := m.recentJob(id)
	if job == nil {
		snap, ok := m.Record(id)
		if !ok {
			return Job{}, errors.New("no such job")
		}
		job = new(Job)
		*job = snap
		job.mu = &sync.Mutex{}
	}
	s := job.Snapshot()
	if !importFailed(s) {
		return Job{}, errors.New("nothing is waiting to be imported for this disc")
	}
	client := rt.arrFor(s.Identity)
	if client == nil {
		return Job{}, errors.New("Radarr/Sonarr is not set up for this disc")
	}
	for _, o := range s.Outputs {
		if o.Import != "imported" {
			if _, err := os.Stat(o.Path); err != nil {
				return Job{}, fmt.Errorf("%s is no longer in staging", o.Path)
			}
		}
	}
	job.set(func(j *Job) {
		j.rt = rt
		if j.DiscType == "" && hasBlurayTitles(*j) {
			j.DiscType = "Blu-ray disc" // records from before v0.13 did not keep it
		}
		// The outcome of this attempt replaces the last one's.
		var keep []string
		for _, w := range j.Warnings {
			if !strings.HasPrefix(w, string(client.Kind)+" ") && !strings.HasPrefix(w, string(client.Kind)+":") {
				keep = append(keep, w)
			}
		}
		j.Warnings = keep
	})
	job.logf("import retried")
	m.arrHandoff(ctx, job)
	job.setStage(StageDone, "done")

	s = job.Snapshot()
	if err := m.deps.Store.AppendHistory(historyRecord(s)); err != nil {
		m.log.Warn("write history", "err", err)
	}
	if !announce && importFailed(s) {
		return s, nil
	}
	ev := m.event(job, "done")
	ev.Elapsed, ev.Error, ev.ImportFailed = s.Elapsed, strings.Join(s.Warnings, "; "), importFailed(s)
	for _, o := range s.Outputs {
		ev.Outputs = append(ev.Outputs, o.Path)
	}
	rt.notifier.Send(context.WithoutCancel(ctx), ev)
	return s, nil
}

// An import that failed because the app did not answer is tried again in
// the background, a few times, further apart each time; nobody has to press
// Retry import for an app that was only busy.
var (
	importRetryAfter = []time.Duration{10 * time.Minute, time.Hour, 6 * time.Hour}
	importRetryEvery = 5 * time.Minute
)

func (m *Manager) watchFailedImports(ctx context.Context) {
	tried := map[string]int{}
	t := time.NewTicker(importRetryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.retryFailedImports(ctx, tried, time.Now())
	}
}

// retryFailedImports retries the imports that are due; tried counts the
// attempts per job.
func (m *Manager) retryFailedImports(ctx context.Context, tried map[string]int, now time.Time) {
	h, err := m.deps.Store.History(0)
	if err != nil {
		return
	}
	latest := map[string]Job{}
	var order []string
	for _, raw := range h {
		var j Job
		if json.Unmarshal(raw, &j) != nil {
			continue
		}
		if _, seen := latest[j.ID]; !seen {
			order = append(order, j.ID)
		}
		latest[j.ID] = j
	}
	for _, id := range order {
		j := latest[id]
		n := tried[id]
		if ctx.Err() != nil || !importFailed(j) || !transientFailure(j) || n >= len(importRetryAfter) || now.Sub(j.FinishedAt) < importRetryAfter[n] {
			continue
		}
		if live := m.recentJob(id); live != nil && !live.Snapshot().Stage.Terminal() {
			continue
		}
		tried[id] = n + 1
		r, err := m.retryImport(ctx, id, false)
		switch {
		case err != nil:
			m.log.Warn("retry import", "job", id, "try", n+1, "err", err)
		case importFailed(r):
			m.log.Info("import still failing; trying again later", "job", id, "try", n+1)
		default:
			m.log.Info("import retried in the background", "job", id, "title", displayTitleSnap(r))
		}
	}
}

// transientFailure reports whether a job's failed imports all failed for a
// passing reason.
func transientFailure(j Job) bool {
	any := false
	for _, o := range j.Outputs {
		if strings.HasPrefix(o.Import, "not imported") {
			if !transientImport(o.Import) {
				return false
			}
			any = true
		}
	}
	return any
}

func hasBlurayTitles(j Job) bool {
	for _, t := range j.Titles {
		if strings.HasSuffix(t.Source, ".mpls") || strings.HasSuffix(t.Source, ".m2ts") {
			return true
		}
	}
	return false
}

// resumeImports finishes the imports that a restart cut short.
func (m *Manager) resumeImports(ctx context.Context) {
	for _, id := range m.deps.Store.PendingImports() {
		if ctx.Err() != nil {
			return
		}
		if _, ok := m.Record(id); !ok {
			// Stopped before the job was recorded (a crash mid-import):
			// nothing to go on; the files stay in staging.
			_ = m.deps.Store.SetImportPending(id, false)
			continue
		}
		m.log.Info("resuming interrupted import", "job", id)
		if _, err := m.RetryImport(ctx, id); err != nil {
			m.log.Warn("resume import", "job", id, "err", err)
			_ = m.deps.Store.SetImportPending(id, false)
		}
	}
}

// recentJob is the live job with this id, if it is still in memory.
func (m *Manager) recentJob(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.recent {
		if j.currentID() == id {
			return j
		}
	}
	return nil
}
