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
	"sync/atomic"
	"time"

	"github.com/sourcequality/media-ripper/internal/arr"
	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/discdb"
	"github.com/sourcequality/media-ripper/internal/drive"
	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/naming"
	"github.com/sourcequality/media-ripper/internal/notify"
	"github.com/sourcequality/media-ripper/internal/postprocess"
	"github.com/sourcequality/media-ripper/internal/selector"
	"github.com/sourcequality/media-ripper/internal/store"
	"github.com/sourcequality/media-ripper/internal/udf"
)

// Deps are the collaborators the pipeline needs. MakeMKV, Metadata and
// Notifier are optional overrides (used by tests); when nil they are built
// from the configuration and rebuilt whenever it changes.
type Deps struct {
	Config   *config.Config
	MakeMKV  *makemkv.Client
	Metadata metadata.Provider
	Store    *store.Store
	Notifier *notify.Notifier
	Logger   *slog.Logger
	// OpenDrive lets tests substitute a fake drive.
	OpenDrive func(path string) (drive.Drive, error)
	// OCRTools overrides the ffmpeg and tesseract binaries (tests).
	OCRTools []string
	// Catalog replaces TheDiscDB (tests).
	Catalog Catalog
	// DiscFiles lists a disc's files and sizes from its UDF filesystem
	// (tests replace the reader).
	DiscFiles func(device string) ([]udf.File, error)
}

// runtime is one immutable configuration generation with the components
// built from it. Jobs capture the generation they started under.
type runtime struct {
	cfg      *config.Config
	mk       *makemkv.Client
	provider metadata.Provider
	notifier *notify.Notifier
	radarr   *arr.Client
	sonarr   *arr.Client
	catalog  Catalog  // nil when TheDiscDB is off
	ocrTools []string // test hook: [ffmpeg, tesseract]
}

// Catalog finds the catalogued disc that matches a scan (TheDiscDB).
type Catalog interface {
	Find(ctx context.Context, kind discdb.Kind, title string, year int, scan []discdb.ScanTitle) (*discdb.Match, error)
	FindByHash(ctx context.Context, hash string, scan []discdb.ScanTitle) (*discdb.HashMatch, error)
	ReleaseDiscs(ctx context.Context, kind discdb.Kind, title string, year int, release string) ([]discdb.DiscInfo, error)
}

// Manager owns one runner per drive.
type Manager struct {
	deps    Deps
	rt      atomic.Pointer[runtime]
	log     *slog.Logger
	runners []*runner
	mu      sync.Mutex
	recent  []*Job
	started time.Time
	storage storageMonitor
}

// New builds a manager. Drives are opened lazily by Run.
func New(deps Deps) *Manager {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.OpenDrive == nil {
		deps.OpenDrive = drive.Open
	}
	m := &Manager{deps: deps, log: deps.Logger, started: time.Now()}
	m.SetConfig(deps.Config)
	return m
}

// Config returns the active configuration. Treat it as read-only.
func (m *Manager) Config() *config.Config { return m.rt.Load().cfg }

// SetConfig swaps in a new configuration. Jobs already running finish under
// the old one. It returns the keys that need a restart to take effect.
func (m *Manager) SetConfig(cfg *config.Config) []string {
	var restart []string
	if prev := m.rt.Load(); prev != nil {
		restart = prev.cfg.NeedsRestart(cfg)
	}
	m.rt.Store(m.build(cfg))
	if cfg.MakeMKV.WriteSettings {
		if err := makemkv.WriteSettings(cfg.MakeMKV.SettingsDir, cfg.MakeMKV.Key, makemkv.SelectionString(cfg.Selection.Languages)); err != nil {
			m.log.Warn("write makemkv settings", "err", err)
		}
	}
	return restart
}

// provider assembles the identification chain for the configured mode.
func (m *Manager) provider(cfg *config.Config, rt *runtime) metadata.Provider {
	var chain metadata.Chain
	mode := cfg.Metadata.Provider
	if mode == "none" {
		return metadata.NoneProvider{}
	}
	if (mode == "auto" || mode == "tmdb") && cfg.Metadata.TMDBAPIKey != "" {
		t := metadata.NewTMDB(cfg.Metadata.TMDBAPIKey, cfg.Metadata.Language, cfg.Metadata.Timeout.D())
		t.Logger = m.log
		chain = append(chain, t)
	}
	if (mode == "auto" || mode == "arr") && (rt.radarr != nil || rt.sonarr != nil) {
		p := &metadata.ArrProvider{Logger: m.log}
		if rt.radarr != nil {
			p.Radarr = arr.LookupAdapter{Client: rt.radarr}
		}
		if rt.sonarr != nil {
			p.Sonarr = arr.LookupAdapter{Client: rt.sonarr}
		}
		chain = append(chain, p)
	}
	if len(chain) == 0 {
		switch mode {
		case "tmdb":
			m.log.Warn("metadata.tmdb_api_key not set; discs will not be identified")
		case "arr":
			m.log.Warn("no radarr or sonarr configured; discs will not be identified")
		default:
			m.log.Warn("no metadata source configured (tmdb key or radarr/sonarr); discs will not be identified")
		}
		return metadata.NoneProvider{}
	}
	return chain
}

func (m *Manager) build(cfg *config.Config) *runtime {
	rt := &runtime{cfg: cfg, mk: m.deps.MakeMKV, provider: m.deps.Metadata, notifier: m.deps.Notifier, ocrTools: m.deps.OCRTools}
	if rt.mk == nil {
		rt.mk = &makemkv.Client{
			Binary:      cfg.MakeMKV.Binary,
			MinLength:   cfg.MakeMKV.MinLength,
			ScanTimeout: cfg.MakeMKV.ScanTimeout.D(),
			RipTimeout:  cfg.MakeMKV.Timeout.D(),
			ExtraArgs:   cfg.MakeMKV.ExtraArgs,
			Logger:      m.log,
		}
	}
	rt.radarr = arr.New(arr.Radarr, cfg.Arr.Radarr, cfg.Metadata.Timeout.D()*3, m.log)
	rt.sonarr = arr.New(arr.Sonarr, cfg.Arr.Sonarr, cfg.Metadata.Timeout.D()*3, m.log)
	if rt.provider == nil {
		rt.provider = m.provider(cfg, rt)
	}
	switch {
	case m.deps.Catalog != nil:
		rt.catalog = m.deps.Catalog
	case cfg.Metadata.TheDiscDB.Enabled:
		c := discdb.New(cfg.Metadata.TheDiscDB.Repo, filepath.Join(cfg.StateDir(), "thediscdb"))
		c.Logger, c.API = m.log, cfg.Metadata.TheDiscDB.API
		rt.catalog = c
	}
	if rt.notifier == nil {
		rt.notifier = &notify.Notifier{WebhookURL: cfg.Notify.WebhookURL, NtfyURL: cfg.Notify.NtfyURL, NtfyToken: cfg.Notify.NtfyToken, Logger: m.log,
			Discord: &notify.Discord{BotToken: cfg.Notify.Discord.BotToken, ChannelID: cfg.Notify.Discord.ChannelID, WebhookURL: cfg.Notify.Discord.WebhookURL,
				Buttons: cfg.Notify.Discord.Buttons}}
	}
	return rt
}

// Run blocks until ctx is cancelled, watching every configured drive.
func (m *Manager) Run(ctx context.Context) error {
	cfg := m.Config()
	paths := cfg.Drives
	if len(paths) == 0 {
		paths = drive.Discover()
	}
	if len(paths) == 0 {
		m.log.Warn("no optical drives found; waiting for one to appear")
	}
	if err := os.MkdirAll(cfg.RipDir(), 0o775); err != nil {
		return err
	}
	m.cleanWorkspace()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		m.watchStorage(ctx)
	}()
	go func() {
		defer wg.Done()
		m.backfillBoxSets(ctx)
	}()
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
	if len(cfg.Drives) == 0 {
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
	ripDir := m.Config().RipDir()
	entries, err := os.ReadDir(ripDir)
	if err != nil {
		return
	}
	cfg := m.Config()
	now := time.Now()
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".keep") {
			continue
		}
		p := filepath.Join(ripDir, e.Name())
		if !staleWorkspace(p, cfg.Output.Resume, cfg.Output.ResumeMaxAge.D(), now) {
			m.log.Info("keeping workspace for resume", "path", p)
			continue
		}
		m.log.Info("removing stale workspace", "path", p)
		_ = os.RemoveAll(p)
	}
}

// DriveStatus is the per-drive view for the UI.
type DriveStatus struct {
	Path      string `json:"path"`
	Model     string `json:"model,omitempty"` // e.g. "HL-DT-ST BD-RE BU40N"
	Status    string `json:"status"`
	Label     string `json:"label,omitempty"`
	Job       *Job   `json:"job,omitempty"`
	LastError string `json:"last_error,omitempty"`
	Ignored   bool   `json:"ignored,omitempty"` // disc present but already handled
}

// Snapshot is the whole system state for the UI.
type Snapshot struct {
	Drives  []DriveStatus  `json:"drives"`
	Recent  []Job          `json:"recent"`
	Started time.Time      `json:"started"`
	Storage *StorageStatus `json:"storage,omitempty"`
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
	if st := m.storage.get(); st.State != "" {
		s.Storage = &st
	}
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
	// drv is read live while a job runs: the loop does not poll then, and
	// the job ejects part-way through.
	drv   drive.Drive
	model string
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
	s := DriveStatus{Path: r.path, Model: r.model, Status: r.lastState.String(), LastError: r.lastErr, Label: r.lastLabel, Ignored: r.ignored}
	if r.job != nil && r.drv != nil {
		if st, err := r.drv.Status(); err == nil {
			s.Status = st.String()
		}
	}
	if r.job != nil {
		j := r.job.Snapshot()
		s.Job = &j
	}
	return s
}

func (r *runner) loop(ctx context.Context) {
	d, err := r.m.deps.OpenDrive(r.path)
	if err != nil {
		r.log.Error("cannot open drive", "err", err)
		r.mu.Lock()
		r.lastErr = err.Error()
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	r.drv = d
	r.model = drive.Model(r.path)
	r.mu.Unlock()
	if r.m.Config().Eject.CloseTrayOnStart {
		_ = d.CloseTray()
	}
	errCount := 0
	for {
		rt := r.m.rt.Load()
		cfg := rt.cfg
		select {
		case <-ctx.Done():
			return
		case <-time.After(cfg.PollInterval.D()):
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
				rt.notifier.Send(ctx, notify.Event{Type: "skipped", Drive: r.path, Label: label, Title: rec.Title, Error: "already ripped"})
				r.mu.Lock()
				r.ignored = true
				if cfg.Eject.OnSuccess {
					_ = d.Eject()
					// The tray is open; whatever comes next is a new insertion.
					r.handled = ""
				}
				r.mu.Unlock()
				continue
			}
		}

		job := newJob(r.path, rt)
		job.Fingerprint = fp
		job.Label = label
		r.mu.Lock()
		r.job = job
		r.mu.Unlock()
		r.m.remember(job)
		if r.m.runJob(ctx, d, job) {
			// We ejected, so the next disc-ok is a fresh insertion even if it
			// arrives before we observe the open tray.
			r.mu.Lock()
			r.handled = ""
			r.mu.Unlock()
		}
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
	job := newJob(path, m.rt.Load())
	job.DryRun = true
	if fp, label, err := d.Fingerprint(); err == nil {
		job.Fingerprint, job.Label = fp, label
	}
	_, _, err = m.scanIdentifySelect(ctx, job)
	return job, err
}

// runJob drives one disc through the pipeline and reports whether the disc
// was ejected afterwards.
func (m *Manager) runJob(ctx context.Context, d drive.Drive, job *Job) (ejected bool) {
	cfg := job.rt.cfg
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
		ejected = m.finish(ctx, d, job)
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

	workDir := workDir(cfg, job)
	if !cfg.Output.Resume {
		_ = os.RemoveAll(workDir)
	}
	if err := os.MkdirAll(workDir, 0o775); err != nil {
		m.fail(job, err)
		return
	}
	rs := openResume(workDir, job, cfg.Output.Resume)
	if rs.resumable() {
		job.logf("resuming: reusing titles ripped or delivered by an earlier attempt")
	}
	backupOnly := cfg.Output.Backup == "only"
	var backupBytes int64
	if cfg.Output.Backup != "off" {
		job.mu.Lock()
		for _, f := range job.discFiles {
			backupBytes += f.Size
		}
		job.mu.Unlock()
	}
	spaceSel := sel
	if backupOnly {
		spaceSel = &selector.Selection{}
	}
	if warning, err := checkSpace(workDir, cfg.Output.Path, spaceSel, rs, backupBytes, diskSpace); err != nil {
		m.fail(job, fmt.Errorf("not enough space: %w", err))
		return
	} else if warning != "" {
		job.logf("%s", warning)
	}
	job.rt.notifier.Send(ctx, m.event(job, "started"))

	// Rip everything before anything else needs the disc so it can leave the
	// drive as early as possible. When the disc is already identified, each
	// title is remuxed and delivered in the background while the next one
	// rips: the drive, the local disk and the network are separate limits.
	// OCR needs the ripped files before anything is named, so it keeps the
	// strict order.
	job.set(func(j *Job) { j.Total = len(sel.Picks) })
	files := make([]string, len(sel.Picks))
	// done marks picks an earlier attempt already delivered.
	done := make([]bool, len(sel.Picks))
	finishPick := func(i int) error {
		file, err := m.postProcess(ctx, job, sel.Picks[i], files[i])
		if err != nil {
			return err
		}
		out, err := m.deliver(ctx, job, disc, sel.Picks[i], file)
		if err != nil {
			return err
		}
		out.RippedAs = filepath.Base(files[i])
		job.set(func(j *Job) {
			for k := range j.Outputs {
				if j.Outputs[k].Path == out.Path {
					j.Outputs[k].RippedAs = out.RippedAs
				}
			}
		})
		if err := rs.markDelivered(sel.Picks[i].Title.ID, out); err != nil {
			log.Warn("resume state", "err", err)
		}
		return nil
	}

	var (
		work  chan int
		wg    sync.WaitGroup
		bgMu  sync.Mutex
		bgErr error
	)
	bgFailed := func() error {
		bgMu.Lock()
		defer bgMu.Unlock()
		return bgErr
	}
	stopWorker := func() {
		if work != nil {
			close(work)
			wg.Wait()
			work = nil
		}
	}
	// Runs before the deferred finish, which removes the workspace.
	defer stopWorker()
	if !m.needsOCR(job) {
		work = make(chan int, len(sel.Picks))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if ctx.Err() != nil || bgFailed() != nil {
					continue
				}
				if err := finishPick(i); err != nil {
					bgMu.Lock()
					bgErr = err
					bgMu.Unlock()
				}
			}
		}()
	}

	job.set(func(j *Job) { j.ripping = true })
	for i, pick := range sel.Picks {
		if backupOnly {
			break
		}
		if ctx.Err() != nil {
			job.setStage(StageCancelled, "cancelled")
			return
		}
		if err := bgFailed(); err != nil {
			break
		}
		job.set(func(j *Job) { j.Current = i + 1 })
		if out, ok := rs.delivered(pick.Title.ID); ok {
			job.logf("%s already delivered to %s", pickName(pick), out.Path)
			job.set(func(j *Job) { j.Outputs = append(j.Outputs, out) })
			done[i] = true
			continue
		}
		if file, ok := rs.ripped(pick.Title.ID); ok {
			job.logf("%s already ripped; reusing %s", pickName(pick), filepath.Base(file))
			files[i] = file
		} else {
			file, err := m.ripPick(ctx, job, disc, pick, workDir, i, len(sel.Picks))
			if err != nil {
				m.fail(job, err)
				return
			}
			files[i] = file
			if err := rs.markRipped(pick.Title.ID, file); err != nil {
				log.Warn("resume state", "err", err)
			}
		}
		if work != nil {
			work <- i
		}
	}
	if cfg.Output.Backup != "off" && bgFailed() == nil && ctx.Err() == nil {
		if err := m.backupDisc(ctx, job); err != nil {
			if backupOnly || ctx.Err() != nil {
				m.fail(job, err)
				return
			}
			job.warn("full-disc backup failed: " + err.Error())
			job.logf("full-disc backup failed: %v", err)
		}
	}
	job.set(func(j *Job) { j.ripping = false })
	if bgFailed() == nil && cfg.Eject.AfterRip && cfg.Eject.OnSuccess {
		_ = d.Lock(false)
		if err := d.Eject(); err != nil {
			log.Warn("eject", "err", err)
		} else {
			job.set(func(j *Job) { j.Ejected = true })
			job.logf("ejected")
			log.Info("ejected", "stage", "ripped")
			job.rt.notifier.Send(ctx, m.event(job, "ready"))
		}
	}

	if backupOnly {
		stopWorker()
		job.setStage(StageDone, "done")
		return false
	}
	if work != nil {
		stopWorker()
		if err := bgFailed(); err != nil {
			m.fail(job, err)
			return
		}
		if ctx.Err() != nil {
			job.setStage(StageCancelled, "cancelled")
			return
		}
	} else {
		m.ocrIdentify(ctx, job, disc, sel, files)
		for i := range sel.Picks {
			if ctx.Err() != nil {
				job.setStage(StageCancelled, "cancelled")
				return
			}
			if done[i] {
				continue
			}
			job.set(func(j *Job) { j.Current = i + 1 })
			if err := finishPick(i); err != nil {
				m.fail(job, err)
				return
			}
		}
	}
	if needsReview(job.Snapshot(), job.rt) {
		m.holdForReview(job)
		return false
	}
	m.arrHandoff(ctx, job)

	job.setStage(StageDone, "done")
	return false // the deferred finish sets the real value
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
// It reports whether the disc was ejected.
func (m *Manager) finish(ctx context.Context, d drive.Drive, job *Job) bool {
	cfg := job.rt.cfg
	snap := job.Snapshot()
	log := m.log.With("drive", job.Drive, "job", job.ID)
	workDir := workDir(cfg, job)

	switch snap.Stage {
	case StageDone, StageReview:
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
		m.markBoxSet(snap)
		m.saveInventory(job)
		log.Info("done", "title", displayTitle(job), "files", len(outs), "elapsed", snap.Elapsed, "warnings", len(snap.Warnings))
		ev := m.event(job, "done")
		if snap.Stage == StageReview {
			ev.Type = "review"
		}
		ev.Outputs, ev.Elapsed, ev.Error = outs, snap.Elapsed, strings.Join(snap.Warnings, "; ")
		job.rt.notifier.Send(context.WithoutCancel(ctx), ev)
	case StageFailed, StageCancelled:
		if !cfg.Output.KeepWorkspaceOnError && !cfg.Output.Resume {
			_ = os.RemoveAll(workDir)
		}
		log.Error("job "+string(snap.Stage), "title", displayTitle(job), "err", snap.Error)
		ev := m.event(job, "failed")
		ev.Error, ev.Elapsed = snap.Error, snap.Elapsed
		job.rt.notifier.Send(context.WithoutCancel(ctx), ev)
	case StageSkipped:
		_ = os.RemoveAll(workDir)
		log.Info("skipped", "label", snap.Label)
		ev := m.event(job, "skipped")
		ev.Error = snap.Message
		job.rt.notifier.Send(context.WithoutCancel(ctx), ev)
	}
	if err := m.deps.Store.AppendHistory(historyRecord(snap)); err != nil {
		log.Warn("write history", "err", err)
	}

	_ = d.Lock(false)
	if snap.Ejected {
		return true
	}
	// A cancel is a person at the web UI, usually about to change settings
	// and rescan, so the disc stays in. Failures eject for the next disc.
	eject := (snap.Stage == StageDone || snap.Stage == StageReview || snap.Stage == StageSkipped) && cfg.Eject.OnSuccess ||
		snap.Stage == StageFailed && cfg.Eject.OnFailure
	if eject {
		if err := d.Eject(); err != nil {
			log.Warn("eject", "err", err)
			return false
		}
		log.Info("ejected", "stage", snap.Stage)
		if snap.Stage == StageDone || snap.Stage == StageReview {
			job.rt.notifier.Send(context.WithoutCancel(ctx), m.event(job, "ready"))
		}
	}
	return eject
}

type historyEntry struct {
	ID           string              `json:"id"`
	Drive        string              `json:"drive"`
	Label        string              `json:"label"`
	Title        string              `json:"title"`
	Kind         string              `json:"kind"`
	Stage        Stage               `json:"stage"`
	Error        string              `json:"error,omitempty"`
	Outputs      []Output            `json:"outputs,omitempty"`
	StartedAt    time.Time           `json:"started_at"`
	FinishedAt   time.Time           `json:"finished_at"`
	Elapsed      string              `json:"elapsed"`
	Identity     *metadata.Identity  `json:"identity,omitempty"`
	Selection    *selector.Selection `json:"selection,omitempty"`
	Titles       []TitleSummary      `json:"titles,omitempty"`
	Catalog      string              `json:"catalog,omitempty"`
	Matched      int                 `json:"catalog_matched,omitempty"`
	Compared     int                 `json:"catalog_compared,omitempty"`
	Hash         string              `json:"content_hash,omitempty"`
	HashOK       bool                `json:"hash_matched,omitempty"`
	Verification string              `json:"verification,omitempty"`
	Warnings     []string            `json:"warnings,omitempty"`
}

func historyRecord(s Job) historyEntry {
	h := historyEntry{ID: s.ID, Drive: s.Drive, Label: s.Label, Title: displayTitleSnap(s), Stage: s.Stage, Error: s.Error, Outputs: s.Outputs,
		StartedAt: s.StartedAt, FinishedAt: s.FinishedAt, Elapsed: s.Elapsed, Identity: s.Identity, Selection: s.Selection, Titles: s.Titles,
		Catalog: s.Catalog, Matched: s.CatalogMatched, Compared: s.CatalogCompared, Hash: s.ContentHash, HashOK: s.HashMatched,
		Verification: s.Verification, Warnings: s.Warnings}
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
	cfg := job.rt.cfg
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
		disc, err = job.rt.mk.Info(ctx, job.Drive)
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

	m.hashDisc(job)

	// 2. Identify.
	job.setStage(StageIdentifying, "looking up "+label)
	hint := metadata.ParseLabel(applyOverride(cfg, label))
	job.logf("label %q → query %q year=%d season=%d disc=%d", label, hint.Query, hint.Year, hint.Season, hint.Disc)
	var id *metadata.Identity
	confirmed, isConfirmed := m.deps.Store.DiscMatch(job.Fingerprint)
	if isConfirmed {
		id = confirmedIdentity(confirmed, hint)
		job.logf("this disc was confirmed by a person on %s", confirmed.ConfirmedAt.Format("2 Jan 2006"))
	} else {
		ictx, icancel := context.WithTimeout(ctx, maxDur(cfg.Metadata.Timeout.D()*4, time.Minute))
		id, err = job.rt.provider.Identify(ictx, hint, selector.DiscHints(disc))
		icancel()
		if err != nil {
			log.Warn("identify", "err", err)
			job.logf("lookup failed: %v (continuing unidentified)", err)
		}
	}
	if id == nil {
		id = &metadata.Identity{Kind: metadata.KindUnknown, Hint: hint}
	}
	// A label that says nothing ("BD_ROM"): the disc's content still can.
	var byHash *discdb.Match
	if !id.Identified() {
		if hid, hm := m.identifyByHash(ctx, job, disc, hint); hid != nil {
			id, byHash = hid, hm
		}
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
		MovieRuntimeTolerance: cfg.Selection.MovieRuntimeTolerance.D(),
		TVEpisodeTolerance:    cfg.Selection.TVEpisodeTolerance,
		MinMovieDuration:      cfg.Selection.MinMovieDuration.D(),
		MinEpisodeDuration:    cfg.Selection.MinEpisodeDuration.D(),
		UnidentifiedStrategy:  cfg.Selection.UnidentifiedStrategy,
		AllowDoubleEpisodes:   cfg.Selection.AllowDoubleEpisodes,
	}
	if id.Kind == metadata.KindTV {
		opts.NextEpisode = m.deps.Store.NextEpisode(seriesKey(id), id.Season)
	}
	var sel *selector.Selection
	if isConfirmed && len(confirmed.Episodes) > 0 {
		sel, err = selector.SelectFromCatalog(disc, id, confirmedEntries(confirmed), "Confirmed")
	} else if entries, source := m.catalogEntries(ctx, job, disc, id, byHash); entries != nil {
		sel, err = selector.SelectFromCatalog(disc, id, entries, source)
		adoptCatalogSeason(job, id, sel)
	} else {
		sel, err = selector.Select(disc, id, opts)
	}
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
	cfg := job.rt.cfg
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
	// An interrupted earlier attempt may have left a partial file here.
	_ = os.RemoveAll(dir)
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
		var plog progressLog
		// MakeMKV runs a short analysis phase before "Saving to MKV file";
		// only the save moves the title's bytes, so only it is metered.
		var rate meter
		saving := false
		file, err := job.rt.mk.Rip(ctx, job.Drive, pick.Title, dir, func(p makemkv.Progress) {
			if p.Task != "" {
				saving = strings.Contains(strings.ToLower(p.Task), "sav")
				rate.reset()
			}
			var eta time.Duration
			if saving && p.Percent >= 0 && pick.Title.SizeBytes > 0 {
				done := int64(float64(pick.Title.SizeBytes) * p.Percent / 100)
				_, eta = job.track(&rate, "rip", pickName(pick), done, pick.Title.SizeBytes)
			}
			job.set(func(j *Job) {
				if p.Percent >= 0 {
					j.Progress = p.Percent
					j.Overall = (float64(idx) + p.Percent/100) / float64(total) * 100
					switch {
					case eta > 0:
						j.ETA = eta.String()
					case p.Remaining > 0:
						j.ETA = p.Remaining.Round(time.Second).String()
					}
				}
				if p.Task != "" {
					j.Message = msg + " · " + p.Task
				}
			})
			if plog.due(p) {
				m.log.Info("rip progress", "drive", job.Drive, "job", job.ID, "pct", int(p.Percent), "eta", p.Remaining.Round(time.Second))
			}
		})
		job.untrack("rip")
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

// progressLog throttles rip progress logging to one line per 10 points.
// MakeMKV reports each phase of a rip (analysis, then saving) from 0 to 100,
// so the threshold starts over when a new phase begins or the percentage
// drops; otherwise the analysis phase reaching 100 silences the whole save.
type progressLog struct {
	last    float64
	started bool
}

func (l *progressLog) due(p makemkv.Progress) bool {
	if p.Task != "" {
		l.started = false
	}
	if p.Percent < 0 {
		return false
	}
	if !l.started || p.Percent < l.last || p.Percent-l.last >= 10 {
		l.last, l.started = p.Percent, true
		return true
	}
	return false
}

// pickName names a pick in progress messages: "S01E03" for an episode,
// "title 2" otherwise.
func pickName(p selector.Pick) string {
	if p.Episode > 0 {
		return fmt.Sprintf("S%02dE%02d", p.Season, p.Episode)
	}
	return fmt.Sprintf("title %d", p.Title.ID)
}

func (m *Manager) postProcess(ctx context.Context, job *Job, pick selector.Pick, file string) (string, error) {
	cfg := job.rt.cfg
	if cfg.PostProcess.Mode == "none" {
		return file, nil
	}
	job.stepStage(StagePostProcess, "remuxing "+pickName(pick))
	var rate meter
	defer job.untrack("remux")
	res, err := postprocess.Run(ctx, file, mkvTitle(job, pick), postprocess.Options{
		Progress: func(done, total int64) {
			job.track(&rate, "remux", pickName(pick), done, total)
		},
		Mode:          cfg.PostProcess.Mode,
		Tool:          cfg.PostProcess.Tool,
		Languages:     cfg.Selection.Languages,
		SetTitle:      cfg.PostProcess.SetTitle,
		CustomCommand: cfg.PostProcess.CustomCommand,
		CustomExt:     cfg.PostProcess.CustomExt,
		Timeout:       cfg.PostProcess.Timeout.D(),
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

func (m *Manager) deliver(ctx context.Context, job *Job, disc *makemkv.Disc, pick selector.Pick, file string) (Output, error) {
	cfg := job.rt.cfg
	job.stepStage(StageDelivering, "copying "+pickName(pick)+" to library")
	dest, err := destPath(cfg, job.rt, job.Snapshot(), pick, filepath.Ext(file), disc.IsBluray())
	if err != nil {
		return Output{}, err
	}
	if !cfg.Output.Overwrite {
		dest = uniquePath(dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), os.FileMode(cfg.Output.DirMode)); err != nil {
		return Output{}, fmt.Errorf("create %s: %w", filepath.Dir(dest), err)
	}
	var rate meter
	size, err := moveFile(ctx, file, dest, os.FileMode(cfg.Output.FileMode), func(done, total int64) {
		job.track(&rate, "copy", pickName(pick), done, total)
	})
	job.untrack("copy")
	if err != nil {
		return Output{}, fmt.Errorf("deliver %s: %w", dest, err)
	}
	job.logf("delivered %s (%.2f GB)", dest, float64(size)/1e9)
	out := Output{Path: dest, Size: size, TitleID: pick.Title.ID, Duration: pick.Title.Duration}
	job.set(func(j *Job) { j.Outputs = append(j.Outputs, out) })
	return out, nil
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
func moveFile(ctx context.Context, src, dst string, mode os.FileMode, progress func(done, total int64)) (int64, error) {
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
	n, err := copyCtx(ctx, out, in, func(done int64) {
		if progress != nil {
			progress(done, st.Size())
		}
	})
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

// copyCtx copies until EOF or cancellation, reporting the running total
// about twice a second.
func copyCtx(ctx context.Context, dst io.Writer, src io.Reader, progress func(done int64)) (int64, error) {
	buf := make([]byte, 4<<20)
	var total int64
	var last time.Time
	for {
		if progress != nil && time.Since(last) >= 500*time.Millisecond {
			progress(total)
			last = time.Now()
		}
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

// event fills in what every notification about a job carries: the title,
// the drive, how the disc was matched and what is being ripped.
func (m *Manager) event(job *Job, typ string) notify.Event {
	s := job.Snapshot()
	ev := notify.Event{Type: typ, JobID: s.ID, Drive: s.Drive, DriveName: drive.Model(s.Drive), Label: s.Label, Title: displayTitleSnap(s), Warnings: s.Warnings}
	ev.Match = verification(s)
	if s.Selection != nil {
		for _, p := range s.Selection.Picks {
			switch {
			case p.Episode > 0 && p.EpisodeEnd > p.Episode:
				ev.Items = append(ev.Items, strings.TrimSpace(fmt.Sprintf("S%02dE%02d-E%02d %s", p.Season, p.Episode, p.EpisodeEnd, p.EpisodeTitle)))
			case p.Episode > 0:
				ev.Items = append(ev.Items, strings.TrimSpace(fmt.Sprintf("S%02dE%02d %s", p.Season, p.Episode, p.EpisodeTitle)))
			default:
				ev.Items = append(ev.Items, fmt.Sprintf("Title %d (%s)", p.Title.ID, p.Title.Duration.Round(time.Minute)))
			}
		}
	}
	return ev
}

// TestNotify sends a test message to every configured target.
func (m *Manager) TestNotify(ctx context.Context) error {
	return m.rt.Load().notifier.Test(ctx)
}

// verification says, for people, how far the titles of a disc can be
// trusted: checked title by title against TheDiscDB, or only chosen by
// length after the disc was identified, or not identified at all.
func verification(s Job) string {
	id := s.Identity
	if id == nil {
		return ""
	}
	if id.Source == sourceManual {
		return "✅ Confirmed by you"
	}
	if !id.Identified() {
		label := s.Label
		if label == "" {
			label = "none"
		}
		return fmt.Sprintf("⚠️ Not identified (label %s); ripping by length", label)
	}
	if s.Catalog != "" {
		catalog := strings.TrimSuffix(strings.TrimPrefix(s.Catalog, "TheDiscDB ("), ")")
		if s.HashMatched {
			return fmt.Sprintf("✅ Verified by TheDiscDB: this exact disc (content hash), %s", catalog)
		}
		if s.CatalogMatched > 0 && s.CatalogMatched == s.CatalogCompared {
			return fmt.Sprintf("✅ Verified by TheDiscDB: all %d titles match %s", s.CatalogMatched, catalog)
		}
		return fmt.Sprintf("☑️ TheDiscDB: %d of %d titles match %s", s.CatalogMatched, s.CatalogCompared, catalog)
	}
	name := id.Title
	if id.Year > 0 {
		name = fmt.Sprintf("%s (%d)", id.Title, id.Year)
	}
	how := "titles chosen by length"
	if s.Selection != nil && len(s.Selection.Picks) > 0 && s.Selection.Picks[0].Episode > 0 {
		p := s.Selection.Picks[0]
		how = fmt.Sprintf("episodes numbered in disc order from S%02dE%02d", p.Season, p.Episode)
	}
	return fmt.Sprintf("⚠️ Not verified: found %s via %s; %s", name, sourceName(id.Source), how)
}

func sourceName(src string) string {
	switch strings.ToLower(src) {
	case "sonarr":
		return "Sonarr"
	case "radarr":
		return "Radarr"
	case "tmdb":
		return "TMDB"
	case "ocr":
		return "the title card"
	case "":
		return "lookup"
	}
	return src
}

// destPath is where a title is delivered: the library layout from the
// naming templates, or the Radarr/Sonarr staging folder when an app will
// import it.
func destPath(cfg *config.Config, rt *runtime, s Job, pick selector.Pick, ext string, bluray bool) (string, error) {
	vars := naming.Vars{Label: s.Label, TitleID: pick.Title.ID, Date: s.StartedAt}
	if v := pick.Title.Video(); v != nil {
		vars.Resolution = resolutionName(v.VideoSize)
	}
	if bluray {
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
		return "", err
	}
	if ext != "" && !strings.EqualFold(filepath.Ext(rel), ext) {
		rel = strings.TrimSuffix(rel, filepath.Ext(rel)) + ext
	}
	if rt.arrFor(s.Identity) != nil {
		// Radarr/Sonarr will move it into their own library layout.
		sub = cfg.Arr.StagingSubdir
	}
	return filepath.Join(cfg.Output.Path, sub, rel), nil
}

// hashDisc computes the disc's TheDiscDB content hash from its file
// inventory, read straight from the device's UDF filesystem. It runs after
// MakeMKV's scan so the two never read the drive at once; a failure only
// means the hash is not available.
func (m *Manager) hashDisc(job *Job) {
	list := m.deps.DiscFiles
	if list == nil {
		list = udf.ListDevice
	}
	type result struct {
		files []udf.File
		hash  string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		files, err := list(job.Drive)
		if err != nil {
			done <- result{err: err}
			return
		}
		h, err := udf.ContentHash(files)
		done <- result{files, h, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			job.logf("content hash: %v", r.err)
			return
		}
		// The inventory is kept for the disc manifest (TheDiscDB export).
		job.set(func(j *Job) { j.ContentHash, j.discFiles = r.hash, r.files })
		job.logf("content hash %s (%d files on the disc)", r.hash, len(r.files))
	case <-time.After(time.Minute):
		job.logf("content hash: the disc's file list took over a minute; skipped")
	}
}
