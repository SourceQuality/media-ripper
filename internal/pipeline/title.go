package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sourcequality/media-ripper/internal/arr"
	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/discordbot"
	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/selector"
)

// A person can correct a disc's title by picking it from a Radarr (movie) or
// Sonarr (TV) search, in the web UI or on Discord, at any point.
//
// Keeping the kind (movie stays movie, TV stays TV) keeps the titles: a
// movie's feature, a TV disc's titles as episodes. Switching the kind, or
// naming an unidentified disc, chooses the titles again with the selector's
// own rules (the feature for a movie, episode-length titles in playback order
// for TV). When those are titles that were ripped, the files are renamed and
// imported under the right title. When they were not, the corrected match is
// remembered and the disc ripped again: right away if it is still in the
// drive (titles already ripped are reused), or when it goes back in.
//
//   - While it is ripped or copied, the correction waits on the job and is
//     applied once the copying is done, before the import.
//   - Held for review, it is approved with the correction.
//   - Done, files Radarr/Sonarr did not take are imported again under the
//     right title; a movie Radarr took is moved to the right movie, but only
//     when the wrong entry is one media-ripper made for this disc. Imported
//     TV episodes stay with Sonarr unless the kind switches.

// titleMu runs one correction at a time.
var titleMu sync.Mutex

var errTVImported = errors.New("this disc's episodes are already in Sonarr; correct them there (Wanted or the series' Manual Import). media-ripper changes an imported TV disc only to a movie")

// validEdit checks a correction before anything is done with it.
func validEdit(e *ReviewEdit) error {
	e.Title = strings.TrimSpace(e.Title)
	if e.Kind != metadata.KindMovie && e.Kind != metadata.KindTV {
		return errors.New("kind must be movie or tv")
	}
	if e.Title == "" {
		return errors.New("title is required")
	}
	if e.Kind == metadata.KindTV && e.Season <= 0 {
		return errors.New("season is required for TV")
	}
	if e.Kind == metadata.KindMovie {
		e.Season, e.TVDBID = 0, 0
	}
	return nil
}

// editLabel names a correction for people: "The Matrix (1999)", "Friends S02".
func editLabel(e ReviewEdit) string {
	if e.Kind == metadata.KindTV {
		return fmt.Sprintf("%s S%02d", e.Title, e.Season)
	}
	if e.Year > 0 {
		return fmt.Sprintf("%s (%d)", e.Title, e.Year)
	}
	return e.Title
}

func kindName(k metadata.Kind) string {
	if k == metadata.KindTV {
		return "TV show"
	}
	return "movie"
}

// CorrectTitle applies a person's correction to a job, whatever state it is
// in, and says what was done.
func (m *Manager) CorrectTitle(ctx context.Context, id string, e ReviewEdit) (Job, string, error) {
	if err := validEdit(&e); err != nil {
		return Job{}, "", err
	}
	titleMu.Lock()
	defer titleMu.Unlock()
	if raw, ok := m.deps.Store.Review(id); ok {
		var snap Job
		if err := json.Unmarshal(raw, &snap); err != nil {
			return Job{}, "", err
		}
		return m.correctReview(ctx, snap, e)
	}
	if job := m.recentJob(id); job != nil && !job.Snapshot().Stage.Terminal() {
		return m.correctRunning(job, e)
	}
	s, ok := m.Record(id)
	if !ok {
		return Job{}, "", errors.New("no such job")
	}
	if err := m.titleProblem(s); err != nil {
		return Job{}, "", err
	}
	if s.Stage == StageReview {
		return Job{}, "", errors.New("it is held for review; correct it there")
	}
	return m.correctDone(ctx, s, e)
}

// correctRunning keeps a correction on a running job, applied once its
// copying is done. A kind switch that needs titles this rip does not take
// rips the disc again with the corrected match while it is still in.
func (m *Manager) correctRunning(job *Job, e ReviewEdit) (Job, string, error) {
	s := job.Snapshot()
	if err := m.titleProblem(s); err != nil {
		return Job{}, "", err
	}
	plan, err := m.planSwitch(s, e)
	if err != nil {
		return Job{}, "", err
	}
	var missing []string
	if plan != nil {
		avail := stagedTitles(s)
		if s.Selection != nil {
			for _, p := range s.Selection.Picks {
				avail[p.Title.ID] = true
			}
		}
		if missing = plan.missing(avail); len(missing) > 0 {
			if r := m.runnerOf(job); r != nil && !s.Ejected {
				if err := m.saveMatch(s, plan, true); err != nil {
					return Job{}, "", err
				}
				job.logf("a person changed the title to %s, which needs %s; ripping the disc again", editLabel(e), strings.Join(missing, ", "))
				m.ripAgain(r, job)
				return s, fmt.Sprintf("Ripping it again as %s (it needs %s); titles already ripped are reused", editLabel(e), strings.Join(missing, ", ")), nil
			}
		}
	}
	var msg string
	job.set(func(j *Job) {
		if j.titleFixed {
			err = errors.New("it is being imported right now; correct it once that is done")
			return
		}
		c := e
		j.titleChange, j.PendingTitle = &c, editLabel(e)
		msg = fmt.Sprintf("Will be imported as %s once the copying is done", editLabel(e))
		if len(missing) > 0 {
			// Applied at the end, where it is remembered and the disc asked for.
			msg = fmt.Sprintf("Saved as %s, but it needs %s, which this rip does not take and the disc is already out: when this copy is done, put the disc back in and it is ripped as %s (this copy is not imported)", editLabel(e), strings.Join(missing, ", "), editLabel(e))
		}
	})
	if err != nil {
		return Job{}, "", err
	}
	job.logf("a person changed the title to %s; it is applied once the copying is done", editLabel(e))
	return job.Snapshot(), msg, nil
}

// correctReview approves a held disc with a correction.
func (m *Manager) correctReview(ctx context.Context, snap Job, e ReviewEdit) (Job, string, error) {
	if err := m.titleProblem(snap); err != nil {
		return Job{}, "", err
	}
	plan, err := m.planSwitch(snap, e)
	if err != nil {
		return Job{}, "", err
	}
	if plan != nil {
		if missing := plan.missing(stagedTitles(snap)); len(missing) > 0 {
			if err := m.saveMatch(snap, plan, true); err != nil {
				return Job{}, "", err
			}
			_ = m.deps.Store.DeleteReview(snap.ID)
			return snap, fmt.Sprintf("Saved as %s, but it needs %s, which were not ripped: put the disc back in and it is ripped as %s. The files held for review stay in staging; nothing was imported", editLabel(e), strings.Join(missing, ", "), editLabel(e)), nil
		}
		j, err := m.approveReview(ctx, snap.ID, func(job *Job) error { setPlan(job, plan); return nil })
		if err != nil {
			return Job{}, "", err
		}
		return j, importedMessage(j, e), nil
	}
	edit := m.fillEpisodes(snap, e)
	j, err := m.ApproveReview(ctx, snap.ID, &edit)
	if err != nil {
		return Job{}, "", err
	}
	return j, importedMessage(j, e), nil
}

// correctDone corrects a finished disc.
func (m *Manager) correctDone(ctx context.Context, s Job, e ReviewEdit) (Job, string, error) {
	plan, err := m.planSwitch(s, e)
	if err != nil {
		return Job{}, "", err
	}
	imported := len(importedOutputs(s)) > 0
	if plan != nil {
		if missing := plan.missing(stagedTitles(s)); len(missing) > 0 || imported {
			if err := m.saveMatch(s, plan, true); err != nil {
				return Job{}, "", err
			}
			msg := fmt.Sprintf("Saved as %s. Put the disc back in: it is ripped again as %s", editLabel(e), editLabel(e))
			if len(missing) > 0 {
				msg += " (it needs " + strings.Join(missing, ", ") + ", which were not ripped)"
			}
			if imported {
				msg += ". " + m.releaseWrongEntry(ctx, s)
			}
			return s, msg, nil
		}
		return m.reimportStaged(ctx, s, e, plan)
	}
	if !imported {
		return m.reimportStaged(ctx, s, e, nil)
	}
	if s.Identity == nil || s.Identity.Kind != metadata.KindMovie {
		return Job{}, "", errTVImported
	}
	if len(mainOutputs(s.Outputs)) > 1 {
		return Job{}, "", errors.New("it went to Radarr as more than one file; correct it in Radarr")
	}
	return m.moveImportedMovie(ctx, s, e)
}

func importedMessage(j Job, e ReviewEdit) string {
	if len(j.Warnings) > 0 {
		return fmt.Sprintf("Changed to %s, with warnings: %s", editLabel(e), strings.Join(j.Warnings, "; "))
	}
	return "Imported as " + editLabel(e)
}

// importedOutputs are the job's files Radarr/Sonarr took.
func importedOutputs(s Job) []Output {
	var out []Output
	for _, o := range mainOutputs(s.Outputs) {
		if o.Import == "imported" {
			out = append(out, o)
		}
	}
	return out
}

// stagedTitles are the titles whose delivered files are still where
// media-ripper put them (not taken by Radarr/Sonarr).
func stagedTitles(s Job) map[int]bool {
	out := map[int]bool{}
	for _, o := range mainOutputs(s.Outputs) {
		if o.Import == "imported" {
			continue
		}
		if _, err := os.Stat(o.Path); err == nil {
			out[o.TitleID] = true
		}
	}
	return out
}

// titleProblem says why a job's title cannot be corrected, or nil.
func (m *Manager) titleProblem(s Job) error {
	rt := m.rt.Load()
	if rt.radarr == nil && rt.sonarr == nil {
		return errors.New("Radarr and Sonarr are not set up, so there is nothing to search")
	}
	if s.Contribute || s.DryRun {
		return errors.New("this disc was not ripped")
	}
	switch s.Stage {
	case StageDone:
		if len(mainOutputs(s.Outputs)) == 0 {
			return errors.New("nothing was delivered")
		}
	case StageFailed, StageCancelled, StageSkipped:
		return fmt.Errorf("the disc was %s, so there is nothing to correct", s.Stage)
	}
	return nil
}

// titlePlan is a job's titles chosen again for a correction.
type titlePlan struct {
	id  *metadata.Identity
	sel *selector.Selection
}

// missing names the plan's titles that are not among have.
func (p *titlePlan) missing(have map[int]bool) []string {
	var out []string
	for _, pk := range p.sel.Picks {
		if !have[pk.Title.ID] {
			out = append(out, fmt.Sprintf("title %d", pk.Title.ID))
		}
	}
	return out
}

// planSwitch chooses a job's titles again when a correction switches its
// kind or names an unidentified disc, with the selector's rules. nil: the
// kind stays, and so do the titles.
func (m *Manager) planSwitch(s Job, e ReviewEdit) (*titlePlan, error) {
	if len(e.Episodes) > 0 {
		return nil, nil // a person mapped the titles by hand
	}
	if s.Identity != nil && s.Identity.Identified() && s.Identity.Kind == e.Kind {
		return nil, nil
	}
	disc := m.discOf(s)
	if disc == nil || len(disc.Titles) == 0 {
		return nil, errors.New("the disc's titles are not known, so they cannot be chosen again; correct it in the web page's review instead")
	}
	id := correctedIdentity(s, e)
	sel, err := selector.Select(disc, id, m.selectOptions(m.Config(), id))
	if err != nil || sel == nil || len(sel.Picks) == 0 {
		return nil, fmt.Errorf("no title on the disc fits a %s", kindName(e.Kind))
	}
	for i := range sel.Picks {
		sel.Picks[i].Reason = fmt.Sprintf("chosen as a %s by a person: %s", kindName(e.Kind), sel.Picks[i].Reason)
	}
	return &titlePlan{id: id, sel: sel}, nil
}

func correctedIdentity(s Job, e ReviewEdit) *metadata.Identity {
	id := &metadata.Identity{Kind: e.Kind, Title: e.Title, Year: e.Year, TMDBID: e.TMDBID, TVDBID: e.TVDBID, Season: e.Season, Confidence: 1, Source: sourceManual}
	if s.Identity != nil {
		id.Hint, id.Disc = s.Identity.Hint, s.Identity.Disc
	}
	return id
}

// setPlan puts a plan's identity and titles on the job.
func setPlan(job *Job, p *titlePlan) {
	job.set(func(j *Job) {
		j.Identity, j.Selection = p.id, p.sel
		j.Catalog, j.CatalogMatched, j.CatalogCompared = "", 0, 0
	})
}

// discOf is the disc as scanned for a job: the saved scan, or, for jobs from
// before scans were kept, the titles as summarised.
func (m *Manager) discOf(s Job) *makemkv.Disc {
	if data, err := m.deps.Store.Scan(s.ID); err == nil {
		var d makemkv.Disc
		if json.Unmarshal(data, &d) == nil && len(d.Titles) > 0 {
			return &d
		}
	}
	if len(s.Titles) == 0 {
		return nil
	}
	full := map[int]*makemkv.Title{}
	if s.Selection != nil {
		for _, p := range s.Selection.Picks {
			full[p.Title.ID] = p.Title
		}
	}
	d := &makemkv.Disc{Type: s.DiscType}
	for _, t := range s.Titles {
		if f := full[t.ID]; f != nil {
			d.Titles = append(d.Titles, f)
			continue
		}
		d.Titles = append(d.Titles, &makemkv.Title{ID: t.ID, Duration: t.Duration, Chapters: t.Chapters, Size: t.Size, SizeBytes: t.SizeBytes, SourceFile: t.Source})
	}
	return d
}

// selectOptions are the selection rules from the configuration; for a TV
// disc numbering starts where its season stopped.
func (m *Manager) selectOptions(cfg *config.Config, id *metadata.Identity) selector.Options {
	opts := selector.Options{
		MovieRuntimeTolerance: cfg.Selection.MovieRuntimeTolerance.D(),
		TVEpisodeTolerance:    cfg.Selection.TVEpisodeTolerance,
		MinMovieDuration:      cfg.Selection.MinMovieDuration.D(),
		MinEpisodeDuration:    cfg.Selection.MinEpisodeDuration.D(),
		UnidentifiedStrategy:  cfg.Selection.UnidentifiedStrategy,
		AllowDoubleEpisodes:   cfg.Selection.AllowDoubleEpisodes,
	}
	if id != nil && id.Kind == metadata.KindTV {
		opts.NextEpisode = m.deps.Store.NextEpisode(seriesKey(id), id.Season)
	}
	return opts
}

// saveMatch remembers a correction for the disc; rerip asks for the disc to
// be ripped again when it goes back in.
func (m *Manager) saveMatch(s Job, p *titlePlan, rerip bool) error {
	if s.Fingerprint == "" {
		return errors.New("the disc has no fingerprint to remember it by")
	}
	dm := matchOf(Job{Identity: p.id, Selection: p.sel})
	dm.Rerip = rerip
	return m.deps.Store.SetDiscMatch(s.Fingerprint, dm)
}

// reripWanted says a disc was corrected to titles it has not given yet, so
// it is ripped again though it was ripped before.
func (m *Manager) reripWanted(fp string) bool {
	dm, ok := m.deps.Store.DiscMatch(fp)
	return ok && dm.Rerip
}

// reripDone clears that once the disc has been ripped as corrected.
func (m *Manager) reripDone(job *Job) {
	s := job.Snapshot()
	var left bool
	job.set(func(j *Job) { left = j.reripLeft })
	dm, ok := m.deps.Store.DiscMatch(s.Fingerprint)
	if !ok || !dm.Rerip || left || s.Identity == nil || s.Identity.Source != sourceManual {
		return
	}
	dm.Rerip = false
	if err := m.deps.Store.SetDiscMatch(s.Fingerprint, dm); err != nil {
		m.log.Warn("remember match", "err", err)
	}
}

// runnerOf is the drive runner a job is running on.
func (m *Manager) runnerOf(job *Job) *runner {
	m.mu.Lock()
	runners := append([]*runner(nil), m.runners...)
	m.mu.Unlock()
	for _, r := range runners {
		if r.current() == job {
			return r
		}
	}
	return nil
}

// ripAgainWait bounds how long a cancelled job may take to wind down
// before its disc is scanned again.
var ripAgainWait = 2 * time.Minute

// ripAgain cancels a job and has its disc, still in the drive, scanned
// again: the remembered match then decides its titles, and titles the
// cancelled run ripped are reused from its workspace.
func (m *Manager) ripAgain(r *runner, job *Job) {
	job.mu.Lock()
	cancel := job.cancel
	job.restarting = true
	job.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.running.Add(1)
	go func() {
		defer m.running.Done()
		for deadline := time.Now().Add(ripAgainWait); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if m.Rescan(r.path) == nil {
				return
			}
		}
		m.log.Warn("disc not scanned again after a title change", "drive", r.path, "job", job.ID)
	}()
}

// releaseWrongEntry takes the wrong movie or series out of Radarr/Sonarr
// after a kind switch, when media-ripper made it for this disc: unmonitored,
// holding only this disc's files. Its files stay on disk. It says what it
// did, for people.
func (m *Manager) releaseWrongEntry(ctx context.Context, s Job) string {
	rt := m.rt.Load()
	client := rt.arrFor(s.Identity)
	if client == nil || s.Identity == nil {
		return ""
	}
	app := "Radarr"
	if client.Kind == arr.Sonarr {
		app = "Sonarr"
	}
	item, ok, err := client.InLibrary(ctx, arr.Item{TMDBID: s.Identity.TMDBID, TVDBID: s.Identity.TVDBID})
	if err != nil || !ok {
		return fmt.Sprintf("%s's %s was not found, so nothing was changed there", app, s.Identity.Title)
	}
	leave := func(why string) string {
		return fmt.Sprintf("%s's %s was left alone (%s); remove it there if it is wrong", app, item.Title, why)
	}
	if item.Monitored {
		return leave("it is monitored")
	}
	held, err := client.Holds(ctx, item.ID)
	if err != nil {
		return leave("its files could not be listed")
	}
	ours := map[int64]bool{}
	for _, o := range s.Outputs {
		ours[o.Size] = true
	}
	for size := range held {
		if !ours[size] {
			return leave("it holds files from elsewhere")
		}
	}
	if err := client.Remove(ctx, item.ID); err != nil {
		return leave(err.Error())
	}
	m.log.Info("removed a wrongly identified entry", "app", app, "title", item.Title, "job", s.ID)
	return fmt.Sprintf("%s was removed from %s; its files stay in %s", item.Title, app, item.Path)
}

// fillEpisodes completes a correction that keeps the kind and only names
// the title: every title the job kept stays, a movie's as the feature, a TV
// disc's as the same episodes when the series and season stay, else
// numbered in disc order from where that season stopped. Extras stay.
func (m *Manager) fillEpisodes(s Job, e ReviewEdit) ReviewEdit {
	if len(e.Episodes) > 0 || s.Selection == nil {
		return e
	}
	e.KeepExtras = true
	e.Episodes = map[int]int{}
	var main []selector.Pick
	for _, p := range s.Selection.Picks {
		if p.Extra == "" {
			main = append(main, p)
		}
	}
	if e.Kind == metadata.KindMovie {
		for _, p := range main {
			e.Episodes[p.Title.ID] = 0
		}
		return e
	}
	if id := s.Identity; id != nil && id.Kind == metadata.KindTV && id.Season == e.Season && sameSeries(id, e) {
		keep := true
		for _, p := range main {
			keep = keep && p.Episode > 0
		}
		if keep {
			for _, p := range main {
				e.Episodes[p.Title.ID] = p.Episode
			}
			return e
		}
	}
	next := m.deps.Store.NextEpisode(seriesKey(&metadata.Identity{Title: e.Title, TMDBID: e.TMDBID}), e.Season)
	if next <= 0 {
		next = 1
	}
	for _, p := range main {
		e.Episodes[p.Title.ID] = next
		next++
	}
	return e
}

func sameSeries(id *metadata.Identity, e ReviewEdit) bool {
	switch {
	case e.TVDBID > 0 && id.TVDBID > 0:
		return e.TVDBID == id.TVDBID
	case e.TMDBID > 0 && id.TMDBID > 0:
		return e.TMDBID == id.TMDBID
	}
	return strings.EqualFold(e.Title, id.Title)
}

// applyTitleChange applies a correction made while the job ran, once its
// copying is done and before the import, and reports whether the import
// should go ahead. From here on the titles are final: a later correction is
// a re-import. A job ripped again after a correction gets the files it
// reused from the earlier run renamed to the corrected title.
func (m *Manager) applyTitleChange(job *Job) bool {
	var e *ReviewEdit
	job.set(func(j *Job) { e, j.titleChange, j.titleFixed, j.PendingTitle = j.titleChange, nil, true, "" })
	before := job.Snapshot()
	if e == nil {
		if before.Identity != nil && before.Identity.Source == sourceManual && before.Selection != nil && len(before.Outputs) > 0 {
			if err := m.restage(job, true); err != nil {
				job.logf("renaming to the confirmed title: %v", err)
			}
		}
		return true
	}
	plan, err := m.planSwitch(before, *e)
	if err != nil {
		job.warn(fmt.Sprintf("title not changed to %s: %v", editLabel(*e), err))
		return true
	}
	if plan != nil {
		if missing := plan.missing(stagedTitles(before)); len(missing) > 0 {
			if err := m.saveMatch(before, plan, true); err != nil {
				job.warn(fmt.Sprintf("title not changed to %s: %v", editLabel(*e), err))
				return true
			}
			job.set(func(j *Job) { j.reripLeft = true })
			job.warn(fmt.Sprintf("not imported: the title was changed to %s, which needs %s that were not ripped; put the disc back in to rip it again (these files stay in staging)", editLabel(*e), strings.Join(missing, ", ")))
			return false
		}
		setPlan(job, plan)
	} else {
		edit := m.fillEpisodes(before, *e)
		if err := applyEdit(job, &edit); err != nil {
			job.warn(fmt.Sprintf("title not changed to %s: %v", editLabel(*e), err))
			return true
		}
	}
	setNextEpisode(job)
	if err := m.restage(job, true); err != nil {
		job.set(func(j *Job) {
			j.Identity, j.Selection, j.Outputs = before.Identity, before.Selection, before.Outputs
			j.Catalog, j.CatalogMatched, j.CatalogCompared = before.Catalog, before.CatalogMatched, before.CatalogCompared
			j.Warnings = before.Warnings
		})
		job.warn(fmt.Sprintf("title not changed to %s: %v", editLabel(*e), err))
		return true
	}
	job.logf("title changed to %s by a person", editLabel(*e))
	s := job.Snapshot()
	if err := m.deps.Store.SetDiscMatch(s.Fingerprint, matchOf(s)); err != nil {
		m.log.Warn("remember match", "err", err)
	}
	return true
}

// setNextEpisode points a corrected TV disc's season at the episode after
// its last one, for the next disc of the season.
func setNextEpisode(job *Job) {
	job.set(func(j *Job) {
		if j.Selection == nil || j.Identity == nil {
			return
		}
		sel := *j.Selection
		sel.NextEpisode = 0
		if j.Identity.Kind == metadata.KindTV {
			for _, p := range sel.Picks {
				if p.Extra == "" && p.Episode >= sel.NextEpisode {
					sel.NextEpisode = p.Episode + 1
				}
				if p.EpisodeEnd >= sel.NextEpisode {
					sel.NextEpisode = p.EpisodeEnd + 1
				}
			}
		}
		j.Selection = &sel
	})
}

// reimportStaged corrects a finished disc whose files Radarr/Sonarr did not
// take (or that went to no app): they are renamed in staging and imported
// under the right title.
func (m *Manager) reimportStaged(ctx context.Context, s Job, e ReviewEdit, plan *titlePlan) (Job, string, error) {
	rt := m.rt.Load()
	job := new(Job)
	*job = s
	job.mu, job.rt = &sync.Mutex{}, rt
	if plan != nil {
		setPlan(job, plan)
	} else {
		edit := m.fillEpisodes(s, e)
		if err := applyEdit(job, &edit); err != nil {
			return Job{}, "", err
		}
	}
	setNextEpisode(job)
	if err := m.restage(job, false); err != nil {
		return Job{}, "", err
	}
	job.logf("title changed to %s by a person", editLabel(e))
	m.arrHandoff(ctx, job)
	job.setStage(StageDone, "done")
	out := job.Snapshot()
	if err := m.deps.Store.SetDiscMatch(out.Fingerprint, matchOf(out)); err != nil {
		m.log.Warn("remember match", "err", err)
	}
	if out.Selection != nil && out.Identity.Kind == metadata.KindTV && out.Selection.NextEpisode > 0 {
		if out.Selection.NextEpisode > m.deps.Store.NextEpisode(seriesKey(out.Identity), out.Identity.Season) {
			_ = m.deps.Store.SetNextEpisode(seriesKey(out.Identity), out.Identity.Season, out.Selection.NextEpisode, out.Identity.Disc)
		}
	}
	if err := m.deps.Store.AppendHistory(historyRecord(out)); err != nil {
		m.log.Warn("write history", "err", err)
	}
	m.refreshRecent(out)
	ev := m.event(job, "done")
	ev.Elapsed, ev.Error, ev.ImportFailed = out.Elapsed, strings.Join(out.Warnings, "; "), importFailed(out)
	for _, o := range out.Outputs {
		ev.Outputs = append(ev.Outputs, o.Path)
	}
	rt.notifier.Send(context.WithoutCancel(ctx), ev)
	if importFailed(out) {
		return out, "", fmt.Errorf("changed to %s, but the import failed: %s", editLabel(e), strings.Join(out.Warnings, "; "))
	}
	return out, importedMessage(out, e), nil
}

// moveImportedMovie moves a movie Radarr already took to the right movie.
// Only an entry media-ripper made for this disc is touched: unmonitored,
// holding just this file, its folder holding nothing but the file and this
// disc's extras. That entry is removed (its file stays), then Radarr moves
// the file into the right movie; if Radarr does not take it, the old entry
// is added back, which finds the file where it was.
func (m *Manager) moveImportedMovie(ctx context.Context, s Job, e ReviewEdit) (Job, string, error) {
	rt := m.rt.Load()
	client := rt.radarr
	if client == nil {
		return Job{}, "", errors.New("Radarr is not set up")
	}
	out := mainOutputs(s.Outputs)[0]
	if s.Identity.TMDBID == 0 {
		return Job{}, "", errors.New("media-ripper does not know which Radarr movie it went to; correct it in Radarr")
	}
	if e.TMDBID > 0 && e.TMDBID == s.Identity.TMDBID {
		return Job{}, "", errors.New("that is the title it already has")
	}
	leave := func(why string) (Job, string, error) {
		return Job{}, "", fmt.Errorf("%s, so nothing was moved; correct it in Radarr", why)
	}
	old, ok, err := client.InLibrary(ctx, arr.Item{TMDBID: s.Identity.TMDBID})
	if err != nil {
		return Job{}, "", err
	}
	if !ok {
		return leave(fmt.Sprintf("Radarr no longer has %s", s.Identity.Title))
	}
	if old.Monitored {
		return leave(fmt.Sprintf("Radarr's %s is monitored, so it may not be only this disc's", old.Title))
	}
	files, err := client.MovieFiles(ctx, old.ID)
	if err != nil {
		return Job{}, "", err
	}
	if len(files) != 1 || files[0].Size != out.Size {
		return leave(fmt.Sprintf("Radarr's %s holds other files than this disc's", old.Title))
	}
	folder, err := client.ListFolder(ctx, old.Path)
	if err != nil {
		return leave(fmt.Sprintf("Radarr could not show what is in %s (%v)", old.Path, err))
	}
	extras := map[string]bool{}
	for _, o := range s.Outputs {
		if o.Extra != "" {
			extras[strings.SplitN(filepath.ToSlash(o.Extra), "/", 2)[0]] = true
		}
	}
	for _, d := range folder.Dirs {
		if !extras[d] {
			return leave(fmt.Sprintf("%s holds a folder %q that is not this disc's", old.Path, d))
		}
	}
	if len(folder.Files) != 1 || folder.Files[0].Size != out.Size {
		return leave(fmt.Sprintf("%s holds other files than this disc's", old.Path))
	}

	job := new(Job)
	*job = s
	job.mu, job.rt = &sync.Mutex{}, rt
	newID := correctedIdentity(s, e)
	if e.TMDBID > 0 {
		if have, ok, err := client.InLibrary(ctx, arr.Item{TMDBID: e.TMDBID}); err != nil {
			return Job{}, "", err
		} else if ok {
			if f, err := client.MovieFiles(ctx, have.ID); err != nil {
				return Job{}, "", err
			} else if len(f) > 0 {
				return leave(fmt.Sprintf("Radarr already has a file for %s", have.Title))
			}
		}
	}
	target, err := m.ensureInLibrary(ctx, job, client, newID)
	if err != nil {
		return Job{}, "", err
	}
	if target.ID == old.ID {
		return Job{}, "", errors.New("that is the title it already has")
	}
	if err := client.Remove(ctx, old.ID); err != nil {
		return Job{}, "", err
	}
	job.logf("removed %s from Radarr (its file stays) to move the file to %s", old.Title, target.Title)
	_, err = client.ImportInto(ctx, old.Path, target.ID, out.Size, arr.ImportOptions{Wait: rt.cfg.Arr.ImportTimeout.D(), Quality: importQuality(arr.Radarr, s)})
	if err == nil {
		var held map[string]bool
		held, err = confirmHeld(ctx, client, target.ID, []Output{out})
		if err == nil && !held[out.Path] {
			err = errors.New("Radarr did not keep it (see its log)")
		}
	}
	if err != nil {
		why := fmt.Sprintf("Radarr did not move the file to %s: %v", target.Title, err)
		if rerr := m.restoreMovie(ctx, client, old); rerr != nil {
			job.logf("%s; putting %s back failed too: %v", why, old.Title, rerr)
			return Job{}, "", fmt.Errorf("%s. Putting %s back failed too (%v): its file is still in %s; add it in Radarr", why, old.Title, rerr, old.Path)
		}
		job.logf("%s; %s was put back", why, old.Title)
		return Job{}, "", fmt.Errorf("%s; %s was put back as it was", why, old.Title)
	}
	if fresh, ok, err := client.InLibrary(ctx, target); err == nil && ok {
		target = fresh
	}
	m.moveExtras(job, client, old, target)
	job.set(func(j *Job) {
		j.Identity = newID
		j.Catalog, j.CatalogMatched, j.CatalogCompared = "", 0, 0
	})
	job.logf("moved to %s in Radarr by a person", target.Title)
	done := job.Snapshot()
	if err := m.deps.Store.SetDiscMatch(done.Fingerprint, matchOf(done)); err != nil {
		m.log.Warn("remember match", "err", err)
	}
	if err := m.deps.Store.AppendHistory(historyRecord(done)); err != nil {
		m.log.Warn("write history", "err", err)
	}
	m.refreshRecent(done)
	msg := fmt.Sprintf("Moved to %s in Radarr", editLabel(e))
	if len(done.Warnings) > len(s.Warnings) {
		msg += "; " + done.Warnings[len(done.Warnings)-1]
	}
	return done, msg, nil
}

// refreshRecent carries a corrected job's outcome over to its copy kept in
// memory, which lookups prefer to the history.
func (m *Manager) refreshRecent(s Job) {
	j := m.recentJob(s.ID)
	if j == nil {
		return
	}
	j.set(func(x *Job) {
		x.Stage, x.Identity, x.Selection, x.Outputs, x.Warnings = s.Stage, s.Identity, s.Selection, s.Outputs, s.Warnings
		x.Catalog, x.CatalogMatched, x.CatalogCompared, x.PendingTitle = s.Catalog, s.CatalogMatched, s.CatalogCompared, ""
	})
}

// restoreMovie adds a removed movie back; Radarr finds its file in its
// folder when it scans the new entry.
func (m *Manager) restoreMovie(ctx context.Context, client *arr.Client, old arr.Item) error {
	items, err := client.Lookup(ctx, "tmdb:"+strconv.Itoa(old.TMDBID))
	if err != nil {
		return err
	}
	for _, it := range items {
		if it.TMDBID == old.TMDBID {
			_, err := client.Add(ctx, it)
			return err
		}
	}
	return fmt.Errorf("Radarr's lookup no longer finds tmdb:%d", old.TMDBID)
}

// moveExtras follows a moved movie with the disc's extras that
// media-ripper placed in its old folder.
func (m *Manager) moveExtras(job *Job, client *arr.Client, old, target arr.Item) {
	s := job.Snapshot()
	if target.Path == "" {
		return
	}
	from, to := client.LocalPath(old.Path), client.LocalPath(target.Path)
	cfg := job.rt.cfg
	var left []string
	moved := map[string]string{}
	for _, o := range s.Outputs {
		if o.Extra == "" || !strings.HasPrefix(o.Path, from+"/") {
			continue
		}
		dst := filepath.Join(to, strings.TrimPrefix(o.Path, from+"/"))
		err := mkdirAll(filepath.Dir(dst), os.FileMode(cfg.Output.DirMode))
		if err == nil {
			_, err = moveFile(context.Background(), o.Path, dst, os.FileMode(cfg.Output.FileMode), nil)
		}
		if err != nil {
			left = append(left, filepath.Base(o.Path))
			continue
		}
		moved[o.Path] = dst
	}
	job.set(func(j *Job) {
		for i := range j.Outputs {
			if dst, ok := moved[j.Outputs[i].Path]; ok {
				j.Outputs[i].Path = dst
			}
		}
	})
	removeEmptyTree(from)
	if len(left) > 0 {
		job.warn(fmt.Sprintf("radarr: extras left in %s: %s", old.Path, strings.Join(left, ", ")))
	}
}

// titleJob finds a job for a correction: held for review, live, or done.
func (m *Manager) titleJob(id string) (Job, bool) {
	if raw, ok := m.deps.Store.Review(id); ok {
		var s Job
		if json.Unmarshal(raw, &s) == nil {
			return s, true
		}
	}
	return m.Record(id)
}

// titleButtonArg splits a title button's argument, "<kind>:<job id>".
func titleButtonArg(arg string) (metadata.Kind, string, bool) {
	kind, id, ok := strings.Cut(arg, ":")
	if !ok || (kind != string(metadata.KindMovie) && kind != string(metadata.KindTV)) {
		return "", "", false
	}
	return metadata.Kind(kind), id, true
}

// DiscordModal answers "It's a movie" / "It's a TV show" with a search form
// for that kind, or nil when the disc's title cannot be corrected
// (DiscordAction then says why).
func (m *Manager) DiscordModal(customID string) *discordbot.Modal {
	arg, ok := strings.CutPrefix(customID, "mr:title:")
	if !ok {
		return nil
	}
	kind, id, ok := titleButtonArg(arg)
	if !ok {
		return nil
	}
	s, ok := m.titleJob(id)
	if !ok || m.titleProblem(s) != nil || !m.searchable(kind) {
		return nil
	}
	q := s.Label
	season := "1"
	if s.Identity != nil && s.Identity.Title != "" {
		q = s.Identity.Title
		if s.Identity.Kind == metadata.KindTV && s.Identity.Season > 0 {
			season = strconv.Itoa(s.Identity.Season)
		}
	}
	md := &discordbot.Modal{CustomID: "mr:titlesearch:" + arg, Title: "Find the movie", Inputs: []discordbot.TextInput{
		{ID: "q", Label: "Movie title to search for", Value: q, Required: true},
	}}
	if kind == metadata.KindTV {
		md.Title = "Find the TV show"
		md.Inputs = []discordbot.TextInput{
			{ID: "q", Label: "TV show to search for", Value: q, Required: true},
			{ID: "season", Label: "Season", Value: season, Required: true},
		}
	}
	return md
}

// searchable says whether the app for a kind is set up.
func (m *Manager) searchable(kind metadata.Kind) bool {
	rt := m.rt.Load()
	if kind == metadata.KindTV {
		return rt.sonarr != nil
	}
	return rt.radarr != nil
}

// titleUnavailable says why a disc offers no title form.
func (m *Manager) titleUnavailable(arg string) string {
	kind, id, ok := titleButtonArg(arg)
	if !ok {
		return "Unknown button"
	}
	s, ok := m.titleJob(id)
	if !ok {
		return "That disc is no longer known"
	}
	if err := m.titleProblem(s); err != nil {
		return "Can't change the title: " + err.Error()
	}
	if !m.searchable(kind) {
		if kind == metadata.KindTV {
			return "Can't make it a TV show: Sonarr is not set up"
		}
		return "Can't make it a movie: Radarr is not set up"
	}
	return "Press the button again to open the search"
}

// DiscordSearch answers the search form with a menu of matches from the
// app for the chosen kind: Radarr for a movie, Sonarr for a TV show.
func (m *Manager) DiscordSearch(ctx context.Context, customID string, fields map[string]string) discordbot.Menu {
	arg, ok := strings.CutPrefix(customID, "mr:titlesearch:")
	if !ok {
		return discordbot.Menu{Content: "Unknown form"}
	}
	kind, id, ok := titleButtonArg(arg)
	if !ok {
		return discordbot.Menu{Content: "Unknown form"}
	}
	s, ok := m.titleJob(id)
	if !ok {
		return discordbot.Menu{Content: "That disc is no longer known"}
	}
	q := strings.TrimSpace(fields["q"])
	if q == "" {
		return discordbot.Menu{Content: "Type a title to search for"}
	}
	season := 0
	if kind == metadata.KindTV {
		season, _ = strconv.Atoi(strings.TrimSpace(fields["season"]))
		if season <= 0 {
			return discordbot.Menu{Content: "Type the season as a number, e.g. 1, and search again."}
		}
	}
	res, err := m.Lookup(ctx, kind, q)
	if err != nil {
		return discordbot.Menu{Content: "Search failed: " + err.Error()}
	}
	what := "Movie"
	if kind == metadata.KindTV {
		what = "TV"
	}
	var opts []discordbot.Option
	for _, r := range res {
		n := r.TMDBID
		if kind == metadata.KindTV {
			n = r.TVDBID
		}
		if n == 0 {
			continue
		}
		label := what + " · " + r.Title
		if r.Year > 0 {
			label += fmt.Sprintf(" (%d)", r.Year)
		}
		opts = append(opts, discordbot.Option{Label: label, Value: fmt.Sprintf("%s:%d", kind, n)})
	}
	if len(opts) == 0 {
		return discordbot.Menu{Content: fmt.Sprintf("No %s found for “%s”. Press the button again to search for something else.", kindName(kind), q)}
	}
	content := fmt.Sprintf("Which %s is **%s**?", kindName(kind), displayTitleSnap(s))
	if kind == metadata.KindTV {
		content += fmt.Sprintf(" It goes in as season %d, episodes in playback order from where that season stopped; set them one by one on the web page if that is wrong.", season)
	}
	return discordbot.Menu{Content: content, CustomID: fmt.Sprintf("mr:titlepick:%s:%d", id, season), Placeholder: "The right " + kindName(kind), Options: opts}
}

// discordPickTitle applies the title picked from DiscordSearch's menu.
// arg is "<job id>:<season>", value "movie:<tmdb id>" or "tv:<tvdb id>".
func (m *Manager) discordPickTitle(ctx context.Context, arg, value string) string {
	i := strings.LastIndex(arg, ":")
	if i < 0 {
		return "Unknown menu"
	}
	id := arg[:i]
	season, _ := strconv.Atoi(arg[i+1:])
	kind, num, ok := strings.Cut(value, ":")
	n, err := strconv.Atoi(num)
	if !ok || err != nil || n <= 0 {
		return "Unknown choice"
	}
	e := ReviewEdit{Kind: metadata.Kind(kind)}
	term := "tmdb:" + num
	if e.Kind == metadata.KindTV {
		term, e.Season = "tvdb:"+num, season
	}
	res, err := m.Lookup(ctx, e.Kind, term)
	if err != nil {
		return "Could not change the title: " + err.Error()
	}
	for _, r := range res {
		if (e.Kind == metadata.KindMovie && r.TMDBID == n) || (e.Kind == metadata.KindTV && r.TVDBID == n) {
			e.Title, e.Year, e.TMDBID, e.TVDBID = r.Title, r.Year, r.TMDBID, r.TVDBID
			break
		}
	}
	if e.Title == "" {
		return "Could not change the title: the search no longer finds it"
	}
	_, msg, err := m.CorrectTitle(ctx, id, e)
	if err != nil {
		return "Could not change the title: " + err.Error()
	}
	return "✏️ " + msg
}
