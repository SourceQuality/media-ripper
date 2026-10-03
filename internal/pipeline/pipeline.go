// Package pipeline runs the unattended disc workflow for every drive.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/drive"
	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/naming"
	"github.com/sourcequality/media-ripper/internal/notify"
	"github.com/sourcequality/media-ripper/internal/postprocess"
	"github.com/sourcequality/media-ripper/internal/selector"
	"github.com/sourcequality/media-ripper/internal/store"
)

// Deps are the collaborators the pipeline needs.
type Deps struct {
	Config   *config.Config
	MakeMKV  *makemkv.Client
	Metadata metadata.Provider
	Store    *store.Store
	Notifier *notify.Notifier
	Logger   *slog.Logger
	// OpenDrive lets tests substitute a fake drive.
	OpenDrive func(path string) (drive.Drive, error)
}

// Manager owns one runner per drive.
type Manager struct {
	deps    Deps
	cfg     *config.Config
	log     *slog.Logger
	runners []*runner
	mu      sync.Mutex
	recent  []*Job
	started time.Time
}

// New builds a manager. Drives are opened lazily by Run.
func New(deps Deps) *Manager {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.OpenDrive == nil {
		deps.OpenDrive = drive.Open
	}
	if deps.Metadata == nil {
		deps.Metadata = metadata.NoneProvider{}
	}
	return &Manager{deps: deps, cfg: deps.Config, log: deps.Logger, started: time.Now()}
}

// Run blocks until ctx is cancelled, watching every configured drive.
func (m *Manager) Run(ctx context.Context) error {
	paths := m.cfg.Drives
	if len(paths) == 0 {
		paths = drive.Discover()
	}
	if len(paths) == 0 {
		m.log.Warn("no optical drives found; waiting for one to appear")
	}
	if err := os.MkdirAll(m.cfg.RipDir(), 0o775); err != nil {
		return err
	}
	m.cleanWorkspace()

	var wg sync.WaitGroup
	seen := map[string]bool{}
	startRunner := func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		r := &runner{m: m, path: p, log: m.log.With("drive", p)}
		m.mu.Lock()
		m.runners = append(m.runners, r)
		m.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.loop(ctx)
		}()
	}
	for _, p := range paths {
		startRunner(p)
	}
	// Hot-plug: when no drives were configured, keep discovering.
	if len(m.cfg.Drives) == 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					for _, p := range drive.Discover() {
						if !seen[p] {
							m.log.Info("new drive", "drive", p)
							startRunner(p)
						}
					}
				}
			}
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// cleanWorkspace removes leftovers from rips interrupted by a crash or power
// loss. Finished files were already moved out, so anything here is partial.
func (m *Manager) cleanWorkspace() {
	entries, err := os.ReadDir(m.cfg.RipDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".keep") {
			continue
		}
		p := filepath.Join(m.cfg.RipDir(), e.Name())
		m.log.Info("removing stale workspace", "path", p)
		_ = os.RemoveAll(p)
	}
}

// DriveStatus is the per-drive view for the UI.
type DriveStatus struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Label     string `json:"label,omitempty"`
	Job       *Job   `json:"job,omitempty"`
	LastError string `json:"last_error,omitempty"`
	Ignored   bool   `json:"ignored,omitempty"` // disc present but already handled
}

// Snapshot is the whole system state for the UI.
type Snapshot struct {
	Drives  []DriveStatus `json:"drives"`
	Recent  []Job         `json:"recent"`
	Started time.Time     `json:"started"`
}

// Snapshot returns the current state.
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	runners := append([]*runner(nil), m.runners...)
	recent := append([]*Job(nil), m.recent...)
	m.mu.Unlock()
	s := Snapshot{Started: m.started, Drives: []DriveStatus{}, Recent: []Job{}}
	for _, r := range runners {
		s.Drives = append(s.Drives, r.status())
	}
	sort.Slice(s.Drives, func(i, j int) bool { return s.Drives[i].Path < s.Drives[j].Path })
	for i := len(recent) - 1; i >= 0; i-- {
		s.Recent = append(s.Recent, recent[i].Snapshot())
	}
	return s
}

func (m *Manager) remember(j *Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recent = append(m.recent, j)
	if len(m.recent) > 50 {
		m.recent = m.recent[len(m.recent)-50:]
	}
}

func (m *Manager) runner(path string) (*runner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.runners {
		if r.path == path {
			return r, nil
		}
	}
	return nil, fmt.Errorf("unknown drive %q", path)
}

// Eject opens the tray of a drive that is not ripping.
func (m *Manager) Eject(path string) error {
	r, err := m.runner(path)
	if err != nil {
		return err
	}
	if j := r.current(); j != nil && !j.Snapshot().Stage.Terminal() {
		return errors.New("drive is busy")
	}
	d, err := m.deps.OpenDrive(path)
	if err != nil {
		return err
	}
	return d.Eject()
}

// Rescan forces the inserted disc to be processed even if it was ripped
// before.
func (m *Manager) Rescan(path string) error {
	r, err := m.runner(path)
	if err != nil {
		return err
	}
	if j := r.current(); j != nil && !j.Snapshot().Stage.Terminal() {
		return errors.New("drive is busy")
	}
	r.force()
	return nil
}

// Cancel aborts a running job; the disc is ejected per configuration.
func (m *Manager) Cancel(jobID string) error {
	m.mu.Lock()
	runners := append([]*runner(nil), m.runners...)
	m.mu.Unlock()
	for _, r := range runners {
		if j := r.current(); j != nil && j.ID == jobID {
			j.mu.Lock()
			c := j.cancel
			j.mu.Unlock()
			if c != nil {
				c()
				return nil
			}
		}
	}
	return fmt.Errorf("no running job %q", jobID)
}

// Job finds a job by id among recent ones.
func (m *Manager) Job(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.recent {
		if j.ID == id {
			return j.Snapshot(), true
		}
	}
	return Job{}, false
}

// ---------------------------------------------------------------------------

type runner struct {
	m    *Manager
	path string
	log  *slog.Logger

	mu        sync.Mutex
	job       *Job
	handled   string // fingerprint already processed while the disc stays in
	forced    bool
	lastErr   string
	lastState drive.Status
	lastLabel string
	ignored   bool
}

func (r *runner) current() *Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.job
}

func (r *runner) force() {
	r.mu.Lock()
	r.forced = true
	r.handled = ""
	r.mu.Unlock()
}

func (r *runner) status() DriveStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := DriveStatus{Path: r.path, Status: r.lastState.String(), LastError: r.lastErr, Label: r.lastLabel, Ignored: r.ignored}
	if r.job != nil {
		j := r.job.Snapshot()
		s.Job = &j
	}
	return s
}

func (r *runner) loop(ctx context.Context) {
	cfg := r.m.cfg
	d, err := r.m.deps.OpenDrive(r.path)
	if err != nil {
		r.log.Error("cannot open drive", "err", err)
		r.mu.Lock()
		r.lastErr = err.Error()
		r.mu.Unlock()
		return
	}
	if cfg.Eject.CloseTrayOnStart {
		_ = d.CloseTray()
	}
	t := time.NewTicker(cfg.PollInterval)
	defer t.Stop()
	errCount := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := d.Status()
		if err != nil {
			errCount++
			if errCount == 1 || errCount%100 == 0 {
				r.log.Warn("drive status", "err", err)
			}
			r.mu.Lock()
			r.lastErr = err.Error()
			r.mu.Unlock()
			continue
		}
		errCount = 0
		r.mu.Lock()
		r.lastState = st
		r.lastErr = ""
		if st != drive.DiscOK {
			r.handled = ""
			r.lastLabel = ""
			r.ignored = false
			r.mu.Unlock()
			continue
		}
		handled, forced := r.handled, r.forced
		r.mu.Unlock()

		fp, label, err := d.Fingerprint()
		if err != nil {
			// Drive is still spinning up; try again next tick.
			continue
		}
		r.mu.Lock()
		r.lastLabel = label
		r.mu.Unlock()
		if fp == handled && !forced {
			continue
		}
		r.mu.Lock()
		r.handled = fp
		r.forced = false
		r.ignored = false
		r.mu.Unlock()

		if !forced && !cfg.Eject.ReripSameDisc {
			if rec, ok := r.m.deps.Store.Disc(fp); ok {
				r.log.Info("disc already ripped; ejecting", "label", label, "ripped_at", rec.RippedAt.Format(time.RFC3339))
				r.m.deps.Notifier.Send(ctx, notify.Event{Type: "skipped", Drive: r.path, Label: label, Title: rec.Title, Error: "already ripped"})
				r.mu.Lock()
				r.ignored = true
				r.mu.Unlock()
				if cfg.Eject.OnSuccess {
					_ = d.Eject()
				}
				continue
			}
		}

		job := newJob(r.path)
		job.Fingerprint = fp
		job.Label = label
		r.mu.Lock()
		r.job = job
		r.mu.Unlock()
		r.m.remember(job)
		r.m.runJob(ctx, d, job)
	}
}

// ---------------------------------------------------------------------------

// ScanOnly runs scan, identify and select for a drive without ripping. It is
// what `media-ripper scan` prints.
func (m *Manager) ScanOnly(ctx context.Context, path string) (*Job, error) {
	d, err := m.deps.OpenDrive(path)
	if err != nil {
		return nil, err
	}
	job := newJob(path)
	job.DryRun = true
	if fp, label, err := d.Fingerprint(); err == nil {
		job.Fingerprint, job.Label = fp, label
	}
	_, _, err = m.scanIdentifySelect(ctx, job)
	return job, err
}

func (m *Manager) runJob(ctx context.Context, d drive.Drive, job *Job) {
	cfg := m.cfg
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	job.set(func(j *Job) { j.cancel = cancel })
	log := m.log.With("drive", job.Drive, "job", job.ID)

	defer func() {
		if rec := recover(); rec != nil {
			log.Error("job panicked", "panic", rec)
			job.set(func(j *Job) { j.Error = fmt.Sprintf("internal error: %v", rec) })
			job.setStage(StageFailed, "internal error")
		}
		m.finish(ctx, d, job)
	}()

	_ = d.Lock(true)
	defer d.Lock(false)

	disc, sel, err := m.scanIdentifySelect(ctx, job)
	if err != nil {
		if errors.Is(err, selector.ErrNothingToRip) {
			job.setStage(StageSkipped, "nothing to rip")
			return
		}
		m.fail(job, err)
		return
	}

	workDir := filepath.Join(cfg.RipDir(), job.ID)
	if err := os.MkdirAll(workDir, 0o775); err != nil {
		m.fail(job, err)
		return
	}
	title := displayTitle(job)
	m.deps.Notifier.Send(ctx, notify.Event{Type: "started", Drive: job.Drive, Label: job.Label, Title: title})

	job.set(func(j *Job) { j.Total = len(sel.Picks) })
	for i, pick := range sel.Picks {
		if ctx.Err() != nil {
			job.setStage(StageCancelled, "cancelled")
			return
		}
		job.set(func(j *Job) { j.Current = i + 1 })
		file, err := m.ripPick(ctx, job, disc, pick, workDir, i, len(sel.Picks))
		if err != nil {
			m.fail(job, err)
			return
		}
		file, err = m.postProcess(ctx, job, pick, file)
		if err != nil {
			m.fail(job, err)
			return
		}
		if err := m.deliver(ctx, job, disc, pick, file); err != nil {
			m.fail(job, err)
			return
		}
	}

	job.setStage(StageDone, "done")
}

func (m *Manager) fail(job *Job, err error) {
	if errors.Is(err, context.Canceled) {
		job.setStage(StageCancelled, "cancelled")
		return
	}
	job.logf("error: %v", err)
	job.set(func(j *Job) { j.Error = err.Error() })
	job.setStage(StageFailed, "failed")
}

// finish runs after every job: history, state, notifications, cleanup, eject.
func (m *Manager) finish(ctx context.Context, d drive.Drive, job *Job) {
	cfg := m.cfg
	snap := job.Snapshot()
	log := m.log.With("drive", job.Drive, "job", job.ID)
	workDir := filepath.Join(cfg.RipDir(), job.ID)

	switch snap.Stage {
	case StageDone:
		var outs []string
		for _, o := range snap.Outputs {
			outs = append(outs, o.Path)
		}
		if err := m.deps.Store.MarkDisc(store.DiscRecord{Fingerprint: snap.Fingerprint, Label: snap.Label, Title: displayTitle(job), Outputs: outs}); err != nil {
			log.Warn("record disc", "err", err)
		}
		if snap.Selection != nil && snap.Selection.Kind == metadata.KindTV && snap.Selection.NextEpisode > 0 && snap.Identity != nil {
			if err := m.deps.Store.SetNextEpisode(seriesKey(snap.Identity), snap.Identity.Season, snap.Selection.NextEpisode, snap.Identity.Disc); err != nil {
				log.Warn("record series progress", "err", err)
			}
		}
		_ = os.RemoveAll(workDir)
		log.Info("done", "title", displayTitle(job), "files", len(outs), "elapsed", snap.Elapsed)
		m.deps.Notifier.Send(context.WithoutCancel(ctx), notify.Event{Type: "done", Drive: snap.Drive, Label: snap.Label, Title: displayTitle(job), Outputs: outs, Elapsed: snap.Elapsed})
	case StageFailed, StageCancelled:
		if !cfg.Output.KeepWorkspaceOnError {
			_ = os.RemoveAll(workDir)
		}
		log.Error("job "+string(snap.Stage), "title", displayTitle(job), "err", snap.Error)
		m.deps.Notifier.Send(context.WithoutCancel(ctx), notify.Event{Type: "failed", Drive: snap.Drive, Label: snap.Label, Title: displayTitle(job), Error: snap.Error, Elapsed: snap.Elapsed})
	case StageSkipped:
		_ = os.RemoveAll(workDir)
		log.Info("skipped", "label", snap.Label)
		m.deps.Notifier.Send(context.WithoutCancel(ctx), notify.Event{Type: "skipped", Drive: snap.Drive, Label: snap.Label, Title: displayTitle(job), Error: snap.Message})
	}
	if err := m.deps.Store.AppendHistory(historyRecord(snap)); err != nil {
		log.Warn("write history", "err", err)
	}

	_ = d.Lock(false)
	eject := (snap.Stage == StageDone || snap.Stage == StageSkipped) && cfg.Eject.OnSuccess ||
		(snap.Stage == StageFailed || snap.Stage == StageCancelled) && cfg.Eject.OnFailure
	if eject {
		if err := d.Eject(); err != nil {
			log.Warn("eject", "err", err)
		}
	}
}

type historyEntry struct {
	ID         string              `json:"id"`
	Drive      string              `json:"drive"`
	Label      string              `json:"label"`
	Title      string              `json:"title"`
	Kind       string              `json:"kind"`
	Stage      Stage               `json:"stage"`
	Error      string              `json:"error,omitempty"`
	Outputs    []Output            `json:"outputs,omitempty"`
	StartedAt  time.Time           `json:"started_at"`
	FinishedAt time.Time           `json:"finished_at"`
	Elapsed    string              `json:"elapsed"`
	Identity   *metadata.Identity  `json:"identity,omitempty"`
	Selection  *selector.Selection `json:"selection,omitempty"`
	Titles     []TitleSummary      `json:"titles,omitempty"`
}

func historyRecord(s Job) historyEntry {
	h := historyEntry{ID: s.ID, Drive: s.Drive, Label: s.Label, Title: displayTitleSnap(s), Stage: s.Stage, Error: s.Error, Outputs: s.Outputs,
		StartedAt: s.StartedAt, FinishedAt: s.FinishedAt, Elapsed: s.Elapsed, Identity: s.Identity, Selection: s.Selection, Titles: s.Titles}
	if s.Identity != nil {
		h.Kind = string(s.Identity.Kind)
	}
	return h
}

func displayTitle(j *Job) string { return displayTitleSnap(j.Snapshot()) }

func displayTitleSnap(s Job) string {
	if s.Identity != nil && s.Identity.Identified() {
		t := s.Identity.Title
		if s.Identity.Kind == metadata.KindTV && s.Identity.Season > 0 {
			t += fmt.Sprintf(" S%02d", s.Identity.Season)
			if s.Identity.Disc > 0 {
				t += fmt.Sprintf(" D%d", s.Identity.Disc)
			}
		} else if s.Identity.Year > 0 {
			t += fmt.Sprintf(" (%d)", s.Identity.Year)
		}
		return t
	}
	if s.Label != "" {
		return s.Label
	}
	return "unknown disc"
}

func seriesKey(id *metadata.Identity) string {
	if id.TMDBID > 0 {
		return fmt.Sprintf("tmdb:%d", id.TMDBID)
	}
	return id.Title
}

// scanIdentifySelect runs the three decision stages and fills the job.
func (m *Manager) scanIdentifySelect(ctx context.Context, job *Job) (*makemkv.Disc, *selector.Selection, error) {
	cfg := m.cfg
	log := m.log.With("drive", job.Drive, "job", job.ID)

	// 1. Scan.
	job.setStage(StageScanning, "scanning disc")
	var disc *makemkv.Disc
	var err error
	for attempt := 0; attempt <= cfg.MakeMKV.Retries; attempt++ {
		if attempt > 0 {
			job.logf("scan retry %d", attempt)
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
		disc, err = m.deps.MakeMKV.Info(ctx, job.Drive)
		if err == nil {
			break
		}
		job.logf("scan failed: %v", err)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("scan: %w", err)
	}
	label := disc.Label()
	if label == "" {
		label = job.Snapshot().Label
	}
	job.set(func(j *Job) {
		j.Label = label
		j.DiscType = disc.Type
		j.Titles = summarize(disc)
	})
	job.logf("%s, %d titles", disc.Type, len(disc.Titles))
	for _, t := range disc.Titles {
		job.logf("  title %d: %s, %d chapters, %d audio, %d subs, %s", t.ID, t.Duration.Round(time.Second), t.Chapters, t.AudioCount(), t.SubtitleCount(), t.Size)
	}

	// 2. Identify.
	job.setStage(StageIdentifying, "looking up "+label)
	hint := metadata.ParseLabel(applyOverride(cfg, label))
	job.logf("label %q → query %q year=%d season=%d disc=%d", label, hint.Query, hint.Year, hint.Season, hint.Disc)
	ictx, icancel := context.WithTimeout(ctx, maxDur(cfg.Metadata.Timeout*4, time.Minute))
	id, err := m.deps.Metadata.Identify(ictx, hint, selector.DiscHints(disc))
	icancel()
	if err != nil {
		log.Warn("identify", "err", err)
		job.logf("lookup failed: %v (continuing unidentified)", err)
	}
	if id == nil {
		id = &metadata.Identity{Kind: metadata.KindUnknown, Hint: hint}
	}
	if id.Season == 0 && hint.Season > 0 {
		id.Season = hint.Season
	}
	if id.Disc == 0 {
		id.Disc = hint.Disc
	}
	if id.Identified() {
		job.logf("identified: %s %s (%d) [%s %.2f]", id.Kind, id.Title, id.Year, id.Source, id.Confidence)
	} else {
		job.logf("not identified")
	}
	job.set(func(j *Job) { j.Identity = id })

	// 3. Select.
	job.setStage(StageSelecting, "choosing titles")
	opts := selector.Options{
		MovieRuntimeTolerance: cfg.Selection.MovieRuntimeTolerance,
		TVEpisodeTolerance:    cfg.Selection.TVEpisodeTolerance,
		MinMovieDuration:      cfg.Selection.MinMovieDuration,
		MinEpisodeDuration:    cfg.Selection.MinEpisodeDuration,
		UnidentifiedStrategy:  cfg.Selection.UnidentifiedStrategy,
		AllowDoubleEpisodes:   cfg.Selection.AllowDoubleEpisodes,
	}
	if id.Kind == metadata.KindTV {
		opts.NextEpisode = m.deps.Store.NextEpisode(seriesKey(id), id.Season)
	}
	sel, err := selector.Select(disc, id, opts)
	if sel != nil {
		job.set(func(j *Job) { j.Selection = sel })
		for _, p := range sel.Picks {
			if p.Episode > 0 {
				job.logf("pick title %d → S%02dE%02d %s (%s)", p.Title.ID, p.Season, p.Episode, p.EpisodeTitle, p.Reason)
			} else {
				job.logf("pick title %d (%s)", p.Title.ID, p.Reason)
			}
		}
		for _, s := range sel.Skipped {
			job.logf("skip title %d (%s): %s", s.TitleID, s.Duration.Round(time.Second), s.Reason)
		}
		for _, n := range sel.Notes {
			job.logf("note: %s", n)
		}
	}
	if err != nil {
		return disc, sel, err
	}
	return disc, sel, nil
}

func applyOverride(cfg *config.Config, label string) string {
	for k, v := range cfg.Metadata.LabelOverrides {
		if strings.EqualFold(strings.TrimSpace(k), strings.TrimSpace(label)) {
			return v
		}
	}
	return label
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func (m *Manager) ripPick(ctx context.Context, job *Job, disc *makemkv.Disc, pick selector.Pick, workDir string, idx, total int) (string, error) {
	cfg := m.cfg
	what := fmt.Sprintf("title %d", pick.Title.ID)
	if pick.Episode > 0 {
		what = fmt.Sprintf("S%02dE%02d", pick.Season, pick.Episode)
	}
	msg := fmt.Sprintf("ripping %s", what)
	if total > 1 {
		msg = fmt.Sprintf("ripping %d/%d: %s", idx+1, total, what)
	}
	job.setStage(StageRipping, msg)
	job.logf("rip title %d (%s)", pick.Title.ID, pick.Title.Duration.Round(time.Second))

	// Each title gets its own folder so MakeMKV's output name cannot collide.
	dir := filepath.Join(workDir, fmt.Sprintf("t%02d", pick.Title.ID))
	var lastErr error
	for attempt := 0; attempt <= cfg.MakeMKV.Retries; attempt++ {
		if attempt > 0 {
			job.logf("rip retry %d", attempt)
			_ = os.RemoveAll(dir)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(10 * time.Second):
			}
		}
		var lastPct float64 = -1
		file, err := m.deps.MakeMKV.Rip(ctx, job.Drive, pick.Title, dir, func(p makemkv.Progress) {
			job.set(func(j *Job) {
				if p.Percent >= 0 {
					j.Progress = p.Percent
					j.Overall = (float64(idx) + p.Percent/100) / float64(total) * 100
					if p.Remaining > 0 {
						j.ETA = p.Remaining.Round(time.Second).String()
					}
				}
				if p.Task != "" {
					j.Message = msg + " · " + p.Task
				}
			})
			if p.Percent >= 0 && p.Percent-lastPct >= 10 {
				lastPct = p.Percent
				m.log.Info("rip progress", "drive", job.Drive, "job", job.ID, "pct", int(p.Percent), "eta", p.Remaining.Round(time.Second))
			}
		})
		if err == nil {
			job.logf("ripped %s", filepath.Base(file))
			return file, nil
		}
		lastErr = err
		job.logf("rip failed: %v", err)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return "", fmt.Errorf("rip title %d: %w", pick.Title.ID, lastErr)
}

func (m *Manager) postProcess(ctx context.Context, job *Job, pick selector.Pick, file string) (string, error) {
	cfg := m.cfg
	if cfg.PostProcess.Mode == "none" {
		return file, nil
	}
	job.setStage(StagePostProcess, "remuxing")
	res, err := postprocess.Run(ctx, file, mkvTitle(job, pick), postprocess.Options{
		Mode:          cfg.PostProcess.Mode,
		Tool:          cfg.PostProcess.Tool,
		Languages:     cfg.Selection.Languages,
		SetTitle:      cfg.PostProcess.SetTitle,
		CustomCommand: cfg.PostProcess.CustomCommand,
		CustomExt:     cfg.PostProcess.CustomExt,
		Timeout:       cfg.PostProcess.Timeout,
		Logger:        m.log,
	})
	if err != nil {
		return "", fmt.Errorf("postprocess: %w", err)
	}
	if res.Skipped {
		job.logf("postprocess skipped: %s", res.Reason)
	} else {
		job.logf("postprocess with %s", res.Tool)
	}
	return res.Output, nil
}

func mkvTitle(job *Job, pick selector.Pick) string {
	s := job.Snapshot()
	if s.Identity == nil || !s.Identity.Identified() {
		return ""
	}
	if s.Identity.Kind == metadata.KindTV && pick.Episode > 0 {
		t := fmt.Sprintf("%s S%02dE%02d", s.Identity.Title, pick.Season, pick.Episode)
		if pick.EpisodeTitle != "" {
			t += " " + pick.EpisodeTitle
		}
		return t
	}
	if s.Identity.Year > 0 {
		return fmt.Sprintf("%s (%d)", s.Identity.Title, s.Identity.Year)
	}
	return s.Identity.Title
}

func (m *Manager) deliver(ctx context.Context, job *Job, disc *makemkv.Disc, pick selector.Pick, file string) error {
	cfg := m.cfg
	job.setStage(StageDelivering, "copying to library")
	s := job.Snapshot()
	vars := naming.Vars{Label: s.Label, TitleID: pick.Title.ID, Date: s.StartedAt}
	if v := pick.Title.Video(); v != nil {
		vars.Resolution = resolutionName(v.VideoSize)
	}
	if disc.IsBluray() {
		vars.Source = "bluray"
	} else {
		vars.Source = "dvd"
	}
	var tmpl, sub string
	switch {
	case s.Identity != nil && s.Identity.Kind == metadata.KindMovie && s.Identity.Identified():
		tmpl, sub = cfg.Output.MovieTemplate, cfg.Output.MoviesSubdir
		vars.Title, vars.Year = s.Identity.Title, s.Identity.Year
	case s.Identity != nil && s.Identity.Kind == metadata.KindTV && s.Identity.Identified():
		tmpl, sub = cfg.Output.TVTemplate, cfg.Output.TVSubdir
		vars.Series, vars.Title, vars.Year = s.Identity.Title, s.Identity.Title, s.Identity.Year
		vars.Season, vars.Episode, vars.EpisodeEnd, vars.EpisodeTitle = pick.Season, pick.Episode, pick.EpisodeEnd, pick.EpisodeTitle
	default:
		tmpl, sub = cfg.Output.UnknownTemplate, cfg.Output.UnidentifiedDir
		vars.Title = s.Label
		if vars.Label == "" {
			vars.Label = "disc"
		}
		if pick.Episode > 0 { // unidentified TV set
			tmpl = "{label} {date}/{label} - S{season:02}E{episode:02}.mkv"
			vars.Season, vars.Episode, vars.EpisodeEnd = pick.Season, pick.Episode, pick.EpisodeEnd
		}
	}
	rel, err := naming.Render(tmpl, vars)
	if err != nil {
		return err
	}
	if ext := filepath.Ext(file); ext != "" && !strings.EqualFold(filepath.Ext(rel), ext) {
		rel = strings.TrimSuffix(rel, filepath.Ext(rel)) + ext
	}
	dest := filepath.Join(cfg.Output.Path, sub, rel)
	if !cfg.Output.Overwrite {
		dest = uniquePath(dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), os.FileMode(cfg.Output.DirMode)); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dest), err)
	}
	size, err := moveFile(ctx, file, dest, os.FileMode(cfg.Output.FileMode))
	if err != nil {
		return fmt.Errorf("deliver %s: %w", dest, err)
	}
	job.logf("delivered %s (%.2f GB)", dest, float64(size)/1e9)
	job.set(func(j *Job) {
		j.Outputs = append(j.Outputs, Output{Path: dest, Size: size, TitleID: pick.Title.ID, Duration: pick.Title.Duration})
	})
	return nil
}

func resolutionName(videoSize string) string {
	// MakeMKV reports "1920x1080", "3840x2160", "720x480" ...
	_, h, ok := strings.Cut(videoSize, "x")
	if !ok {
		return ""
	}
	switch {
	case strings.HasPrefix(h, "2160"):
		return "2160p"
	case strings.HasPrefix(h, "1080"):
		return "1080p"
	case strings.HasPrefix(h, "720"):
		return "720p"
	case h == "576" || h == "480":
		return h + "p"
	}
	return ""
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 2; i < 1000; i++ {
		c := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(c); errors.Is(err, os.ErrNotExist) {
			return c
		}
	}
	return p
}

// moveFile renames when possible, otherwise copies through a temp name,
// verifies the size and removes the source. Returns the final size.
func moveFile(ctx context.Context, src, dst string, mode os.FileMode) (int64, error) {
	st, err := os.Stat(src)
	if err != nil {
		return 0, err
	}
	// Rename when the workspace and library share a filesystem. Any failure,
	// EXDEV or otherwise (network filesystems reject renames for odd reasons),
	// falls back to a verified copy.
	if err := os.Rename(src, dst); err == nil {
		_ = os.Chmod(dst, mode)
		return st.Size(), nil
	}
	tmp := dst + ".part"
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return 0, err
	}
	n, err := copyCtx(ctx, out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if n != st.Size() {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("copied %d of %d bytes", n, st.Size())
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	_ = os.Remove(src)
	return n, nil
}

func copyCtx(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 4<<20)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			total += int64(w)
			if werr != nil {
				return total, werr
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}
