package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sourcequality/media-ripper/internal/drive"
	"github.com/sourcequality/media-ripper/internal/notify"
)

// A USB drive can vanish and come back on its own. The loop does not poll
// while a job runs, so a watcher notices it: a job that fails because of it
// runs again when the disc is back, an eject that failed is tried again, and
// a drive that stays away is reported.

// maxDriveRetries is how often one disc is run again after the drive
// dropped; a disc that keeps dropping the drive must not loop forever.
const maxDriveRetries = 3

// maxEjectTries limits eject attempts per disc.
const maxEjectTries = 10

var (
	driveWatchEvery = time.Second
	// driveLostAfter is how long a drive stays away before anyone is told.
	// Most resets are over in seconds.
	driveLostAfter = 30 * time.Second
	// driveFailGrace is how long a failed job waits for the watcher: the
	// read errors that fail it come just before the drive disappears.
	driveFailGrace  = 3 * time.Second
	ejectRetryEvery = 5 * time.Second
)

// driveGone reports whether a status error means the device is not there.
func driveGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.ENXIO)
}

func (r *runner) watchDrive(ctx context.Context, d drive.Drive) {
	t := time.NewTicker(driveWatchEvery)
	defer t.Stop()
	var lostAt, nextEject time.Time
	announced := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, checked, err := r.lookAtDrive(d)
		if err != nil && driveGone(err) {
			if lostAt.IsZero() {
				lostAt = time.Now()
				r.log.Warn("drive disappeared", "err", err)
				r.mu.Lock()
				job := r.job
				r.mu.Unlock()
				if job != nil {
					job.set(func(j *Job) { j.driveLost = true })
				}
			}
			if !announced && time.Since(lostAt) >= driveLostAfter && r.m.servedElsewhere(r) {
				announced = true // back under another name; nothing to tell anyone
				r.log.Info("drive is back under another name")
			}
			if !announced && time.Since(lostAt) >= driveLostAfter {
				announced = true
				r.notifyDrive(ctx, "drive_lost", fmt.Sprintf("`%s` has not answered since %s. It may need replugging.", r.path, lostAt.Format("15:04:05 MST")))
			}
			continue
		}
		if !lostAt.IsZero() {
			r.log.Info("drive is back", "after", time.Since(lostAt).Round(time.Second).String())
			if announced {
				r.notifyDrive(ctx, "drive_back", fmt.Sprintf("`%s` answers again.", r.path))
			}
			lostAt, announced = time.Time{}, false
		}
		if checked && err == nil && time.Now().After(nextEject) && r.retryEject(ctx, d, st) {
			nextEject = time.Now().Add(ejectRetryEvery)
		}
	}
}

// presence is a drive that can say it is there without a command to the
// hardware. Every second, for hours of ripping, that matters: this watcher
// exists because USB bridges are fragile.
type presence interface {
	Present() bool
}

// lookAtDrive reports the drive's state for the watcher. The tray is only
// asked about when an eject is waiting; checked says whether it was.
func (r *runner) lookAtDrive(d drive.Drive) (st drive.Status, checked bool, err error) {
	if p, ok := d.(presence); ok {
		if !p.Present() {
			return st, false, os.ErrNotExist
		}
		if !r.ejectWaiting() {
			return st, false, nil
		}
	}
	st, err = d.Status()
	return st, true, err
}

func (r *runner) ejectWaiting() bool {
	r.mu.Lock()
	job := r.job
	r.mu.Unlock()
	if job == nil {
		return false
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	return job.ejectPending
}

func (r *runner) notifyDrive(ctx context.Context, typ, summary string) {
	r.mu.Lock()
	model := r.model
	r.mu.Unlock()
	r.m.rt.Load().notifier.Send(ctx, notify.Event{Type: typ, Drive: r.path, DriveName: model, Summary: summary})
}

// retryEject tries an eject that failed once more and reports whether it
// tried.
func (r *runner) retryEject(ctx context.Context, d drive.Drive, st drive.Status) bool {
	r.mu.Lock()
	job := r.job
	r.mu.Unlock()
	if job == nil {
		return false
	}
	tries, attempt := 0, false
	job.set(func(j *Job) {
		if !j.ejectPending {
			return
		}
		if st == drive.TrayOpen || st == drive.NoDisc {
			j.ejectPending = false // someone took the disc out
			return
		}
		if st != drive.DiscOK {
			return // not ready yet: the flag stays for the next look
		}
		j.ejectPending, attempt = false, true // taken: nobody else ejects meanwhile
		j.ejectTries++
		tries = j.ejectTries
	})
	if !attempt {
		return false
	}
	if err := d.Eject(); err != nil {
		r.log.Warn("eject retry", "job", job.ID, "try", tries, "err", err)
		if tries < maxEjectTries {
			job.set(func(j *Job) { j.ejectPending = true })
		}
		return true
	}
	job.set(func(j *Job) { j.Ejected = true })
	job.logf("ejected after the drive came back")
	r.log.Info("ejected on retry", "job", job.ID)
	job.rt.notifier.Send(context.WithoutCancel(ctx), r.m.readyEvent(job))
	return true
}

// readyEvent says the tray is open, and how the disc ended if it has.
func (m *Manager) readyEvent(job *Job) notify.Event {
	snap := job.Snapshot()
	ev := m.event(job, "ready")
	switch snap.Stage {
	case StageFailed:
		ev.Summary = fmt.Sprintf("**%s** failed and the tray is open.", displayTitleSnap(snap))
	case StageSkipped:
		ev.Summary = fmt.Sprintf("**%s** was skipped and the tray is open.", displayTitleSnap(snap))
	case StageDone, StageReview, StageCancelled:
		ev.Summary = fmt.Sprintf("**%s** is finished and the tray is open.", displayTitleSnap(snap))
	}
	return ev
}

// pauseForDrive keeps a job that failed because the drive dropped, so it
// runs again when the disc is back, and reports whether it did. Past the
// limit the failure stands, and says why.
func (m *Manager) pauseForDrive(ctx context.Context, d drive.Drive, job *Job) bool {
	s := job.Snapshot()
	if s.Stage != StageFailed || s.Ejected || s.Fingerprint == "" || s.Contribute {
		return false
	}
	lost := func() bool {
		job.mu.Lock()
		defer job.mu.Unlock()
		return job.driveLost
	}
	for deadline := time.Now().Add(driveFailGrace); !lost() && time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(driveWatchEvery / 4):
		}
	}
	if !lost() {
		return false
	}
	m.mu.Lock()
	if m.driveRetries == nil {
		m.driveRetries = map[string]int{}
	}
	m.driveRetries[s.Fingerprint]++
	n := m.driveRetries[s.Fingerprint]
	if n > maxDriveRetries {
		delete(m.driveRetries, s.Fingerprint)
	}
	m.mu.Unlock()
	if n > maxDriveRetries {
		msg := fmt.Sprintf("the drive dropped out %d times on this disc, so it was given up on: %s", maxDriveRetries+1, s.Error)
		job.logf("%s", msg)
		job.set(func(j *Job) { j.Error = msg })
		return false
	}
	dir := workDir(job.rt.cfg, job)
	if _, err := os.Stat(filepath.Join(dir, resumeFile)); err == nil {
		if err := markInterrupted(dir, s); err != nil {
			m.log.Warn("save paused job", "job", job.ID, "err", err)
		}
	}
	m.log.Warn("drive dropped; the job runs again when the disc is back", "drive", job.Drive, "job", job.ID, "try", n, "err", s.Error)
	job.logf("the drive dropped (%s); running again when it is back, try %d of %d", s.Error, n, maxDriveRetries)
	job.set(func(j *Job) {
		j.Stage, j.Message, j.Error, j.FinishedAt, j.Elapsed = StageRipping, "waiting for the drive to come back", "", time.Time{}, ""
		j.retryDisc = true
	})
	m.forget(job) // the next attempt carries on under the same id
	ev := m.event(job, "interrupted")
	ev.Summary = "⏸ The drive reset. This disc carries on by itself when the drive is back."
	job.rt.notifier.Send(context.WithoutCancel(ctx), ev)
	_ = d.Lock(false)
	return true
}

// forget drops a job from the recent list.
func (m *Manager) forget(job *Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.recent[:0]
	for _, j := range m.recent {
		if j != job {
			out = append(out, j)
		}
	}
	m.recent = out
}

// clearDriveRetries forgets a disc's count once it has come through.
func (m *Manager) clearDriveRetries(fp string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.driveRetries, fp)
}

// savePausedJobs records what a restart needs the moment the service is told
// to stop. Stopping can then block for as long as the kernel needs to flush
// a copy to a slow share, or be killed, without losing the pause.
func (m *Manager) savePausedJobs() {
	m.mu.Lock()
	jobs := append([]*Job(nil), m.finishing...)
	runners := append([]*runner(nil), m.runners...)
	m.mu.Unlock()
	for _, r := range runners {
		r.mu.Lock()
		if r.job != nil {
			jobs = append(jobs, r.job)
		}
		r.mu.Unlock()
	}
	for _, job := range jobs {
		s := job.Snapshot()
		if s.Stage.Terminal() || job.rt == nil {
			continue
		}
		dir := workDir(job.rt.cfg, job)
		if _, err := os.Stat(filepath.Join(dir, resumeFile)); err != nil {
			continue
		}
		if err := markInterrupted(dir, s); err != nil {
			m.log.Warn("save paused job", "job", job.ID, "err", err)
		}
	}
}
