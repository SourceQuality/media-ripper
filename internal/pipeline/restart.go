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

	"github.com/sourcequality/media-ripper/internal/drive"
	"github.com/sourcequality/media-ripper/internal/makemkv"
)

// A restart is not a cancel: a job the service stops in the middle is
// paused. Its workspace is kept, and the next start carries it on under the
// same id: with the disc still in, the drive's runner resumes it (titles
// already ripped are reused); with the disc already ejected, the remaining
// copying and the import are finished from the workspace.

// jobFile is a paused job's snapshot, kept in its workspace.
const jobFile = "job.json"

// shuttingDown reports whether the service is stopping.
func (m *Manager) shuttingDown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runCtx != nil && m.runCtx.Err() != nil
}

// pausedByShutdown reports whether a job ended only because the service
// is stopping.
func (m *Manager) pausedByShutdown(s Job) bool {
	switch s.Stage {
	case StageDone, StageReview, StageSkipped:
		return false
	}
	return m.shuttingDown()
}

// pauseForRestart keeps what a job needs to carry on after the restart:
// no history record, no failure notice, and the disc stays in.
func (m *Manager) pauseForRestart(ctx context.Context, d drive.Drive, job *Job) {
	snap := job.Snapshot()
	dir := workDir(job.rt.cfg, job)
	if _, err := os.Stat(filepath.Join(dir, resumeFile)); err == nil {
		if err := markInterrupted(dir, snap); err != nil {
			m.log.Warn("save paused job", "job", job.ID, "err", err)
		}
	}
	m.saveInventory(job)
	m.log.Info("paused for a restart; carries on at the next start", "drive", job.Drive, "job", job.ID, "title", displayTitle(job))
	ev := m.event(job, "interrupted")
	if !snap.Ejected {
		snap.Stage = StageRipping // the title in progress starts over
	}
	ev.Items, ev.Summary = progressLines(snap)
	ev.Summary = "⏸ Paused: media-ripper is restarting. It carries on by itself; ripped titles are kept, the one in progress starts over."
	job.rt.notifier.Send(context.WithoutCancel(ctx), ev)
	_ = d.Lock(false)
}

// markInterrupted flags the workspace's resume record and keeps the job.
func markInterrupted(dir string, snap Job) error {
	path := filepath.Join(dir, resumeFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var st resumeState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	st.Interrupted = true
	if st.JobID == "" {
		st.JobID, st.Started = snap.ID, snap.StartedAt
	}
	if data, err = json.MarshalIndent(st, "", "  "); err != nil {
		return err
	}
	if err := atomicWriteFile(path, data); err != nil {
		return err
	}
	if data, err = json.Marshal(snap); err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(dir, jobFile), data)
}

func atomicWriteFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// finishEjected carries on, without the disc, the paused jobs whose disc
// had already been ripped and ejected.
func (m *Manager) finishEjected(ctx context.Context) {
	cfg := m.Config()
	entries, err := os.ReadDir(cfg.RipDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		dir := filepath.Join(cfg.RipDir(), e.Name())
		data, err := os.ReadFile(filepath.Join(dir, jobFile))
		if err != nil {
			continue
		}
		var snap Job
		if json.Unmarshal(data, &snap) != nil || !snap.Ejected || snap.Selection == nil {
			continue // the disc's runner resumes it when the disc is in
		}
		if ctx.Err() != nil {
			return
		}
		m.finishOffline(ctx, snap, dir)
	}
}

// finishOffline delivers the titles a paused job had ripped but not yet
// delivered, imports them and records the job, as if it had never stopped.
func (m *Manager) finishOffline(ctx context.Context, snap Job, dir string) {
	rt := m.rt.Load()
	job := new(Job)
	*job = snap
	job.mu, job.rt = &sync.Mutex{}, rt
	job.set(func(j *Job) {
		j.Outputs, j.Activities, j.Error = nil, nil, ""
		j.Stage = StageDelivering
	})
	rs := openResume(dir, job, true)
	if _, _, ok := rs.restarted(); !ok {
		return // not ours to carry on
	}
	if m.needsOCR(job) {
		job.logf("needs the disc for title-card recognition; insert it again to finish")
		return
	}
	m.setFinishing(job, true)
	defer m.setFinishing(job, false)
	m.remember(job)
	if err := rs.begin(job.ID, job.StartedAt); err != nil {
		m.log.Warn("resume state", "job", job.ID, "err", err)
	}
	m.log.Info("finishing a paused job; the disc was already ejected", "job", job.ID, "title", displayTitle(job))
	job.logf("carrying on after a restart; the disc was already ejected")
	stop := make(chan struct{})
	go m.liveProgress(ctx, job, stop)
	defer func() {
		close(stop)
		m.finish(ctx, ejectedDrive{path: job.Drive}, job)
	}()

	disc := &makemkv.Disc{Type: snap.DiscType}
	var missing []string
	for _, pick := range snap.Selection.Picks {
		if ctx.Err() != nil {
			job.setStage(StageCancelled, "cancelled")
			return
		}
		if out, ok := rs.delivered(pick.Title.ID); ok {
			job.set(func(j *Job) { j.Outputs = append(j.Outputs, out) })
			continue
		}
		file, ok := rs.ripped(pick.Title.ID)
		if !ok {
			missing = append(missing, pickName(pick))
			continue
		}
		file2, err := m.postProcess(ctx, job, pick, file)
		if err == nil {
			var out Output
			if out, err = m.deliver(ctx, job, disc, pick, file2); err == nil {
				out.RippedAs = filepath.Base(file)
				job.set(func(j *Job) {
					for k := range j.Outputs {
						if j.Outputs[k].Path == out.Path {
							j.Outputs[k].RippedAs = out.RippedAs
						}
					}
				})
				err = rs.markDelivered(pick.Title.ID, out)
			}
		}
		if err != nil {
			m.fail(job, err)
			return
		}
	}
	if len(missing) > 0 {
		m.fail(job, fmt.Errorf("%s not ripped before the restart; insert the disc again and rip it", strings.Join(missing, ", ")))
		return
	}
	if needsReview(job.Snapshot(), rt) {
		m.holdForReview(job)
		return
	}
	m.arrHandoff(ctx, job)
	job.setStage(StageDone, "done")
}

// setFinishing tracks the jobs finished without their disc, for the UI and
// so the same disc put back in meanwhile waits for it.
func (m *Manager) setFinishing(job *Job, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.finishing[:0]
	for _, j := range m.finishing {
		if j != job {
			out = append(out, j)
		}
	}
	if on {
		out = append(out, job)
	}
	m.finishing = out
}

// finishingDisc reports whether a disc's paused job is being finished.
func (m *Manager) finishingDisc(fp string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.finishing {
		if j.Fingerprint == fp {
			return true
		}
	}
	return false
}

// ejectedDrive stands in for the drive of a job whose disc is gone.
type ejectedDrive struct{ path string }

func (d ejectedDrive) Path() string                         { return d.path }
func (d ejectedDrive) Status() (drive.Status, error)        { return drive.TrayOpen, nil }
func (d ejectedDrive) Eject() error                         { return nil }
func (d ejectedDrive) CloseTray() error                     { return errors.New("not the job's drive") }
func (d ejectedDrive) Lock(bool) error                      { return nil }
func (d ejectedDrive) Fingerprint() (string, string, error) { return "", "", errors.New("no disc") }
