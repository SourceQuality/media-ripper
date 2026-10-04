package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// importFailed reports whether a finished job left files that Radarr or
// Sonarr did not import, which a retry may still get in.
func importFailed(s Job) bool {
	if s.Stage != StageDone {
		return false
	}
	for _, o := range s.Outputs {
		if strings.HasPrefix(o.Import, "not imported") {
			return true
		}
	}
	return false
}

// RetryImport runs the Radarr/Sonarr import of a finished job again, for
// files that a failed or interrupted import left in staging.
func (m *Manager) RetryImport(ctx context.Context, id string) (Job, error) {
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
			if !strings.HasPrefix(w, string(client.Kind)+" ") {
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
	ev := m.event(job, "done")
	ev.Elapsed, ev.Error, ev.ImportFailed = s.Elapsed, strings.Join(s.Warnings, "; "), importFailed(s)
	for _, o := range s.Outputs {
		ev.Outputs = append(ev.Outputs, o.Path)
	}
	rt.notifier.Send(context.WithoutCancel(ctx), ev)
	return s, nil
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
		if j.ID == id {
			return j
		}
	}
	return nil
}
