package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sourcequality/media-ripper/internal/drive"
	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/selector"
)

// Manual mode: a scanned disc stays in the drive while a person labels
// what each title is, with previews, then chooses to rip it, keep it for
// TheDiscDB only, or eject it.

// Title kinds a person can give.
const (
	LabelMain    = "main"    // the feature (Name: its cut, e.g. "Extended")
	LabelEpisode = "episode" // Season/Episode
	LabelExtra   = "extra"   // Name: what it is
	LabelTrailer = "trailer" // Name: what it advertises
	LabelSkip    = "skip"    // nothing worth keeping
)

// TitleLabel is what one title of a disc is.
type TitleLabel struct {
	TitleID  int           `json:"title_id"`
	Duration time.Duration `json:"duration"`
	Kind     string        `json:"kind"`
	Name     string        `json:"name,omitempty"`
	// Category of an extra: featurette (default), behind-the-scenes,
	// deleted-scene, interview, scene, short or other.
	Category string `json:"category,omitempty"`
	Season   int    `json:"season,omitempty"`
	Episode  int           `json:"episode,omitempty"`
	Rip      bool          `json:"rip"`
	// Guess says where an offered label came from; empty once a person
	// has labelled the title.
	Guess string `json:"guess,omitempty"`
}

// LabelDecision is what a person decided for a disc being labelled.
type LabelDecision struct {
	Action string       `json:"action"` // rip | contribute | eject
	Labels []TitleLabel `json:"labels"`
	// Identity corrects what the disc is; nil keeps the identification.
	Identity *ReviewEdit `json:"identity,omitempty"`
	reason   string
}

// extraFolders are the library folders Plex and Jellyfin read extras from.
var extraFolders = map[string]string{
	"featurette": "Featurettes", "behind-the-scenes": "Behind The Scenes", "deleted-scene": "Deleted Scenes",
	"interview": "Interviews", "scene": "Scenes", "short": "Shorts", "other": "Other",
}

// extraFolder is where a ripped extra or trailer goes.
func extraFolder(l TitleLabel) string {
	if l.Kind == LabelTrailer {
		return "Trailers"
	}
	if f, ok := extraFolders[l.Category]; ok {
		return f
	}
	return "Featurettes"
}

// trailerMax is the longest a title is offered as a trailer.
const trailerMax = 3*time.Minute + 30*time.Second

// guessLabels offers a label for every title: the selection's picks as
// they are, the rest by length, or what a person said for this disc last
// time.
func (m *Manager) guessLabels(job *Job, disc *makemkv.Disc, sel *selector.Selection) []TitleLabel {
	s := job.Snapshot()
	picks := map[int]selector.Pick{}
	var main time.Duration
	if sel != nil {
		for _, p := range sel.Picks {
			picks[p.Title.ID] = p
			if p.Title.Duration > main {
				main = p.Title.Duration
			}
		}
	}
	saved := map[int]TitleLabel{}
	if dm, ok := m.deps.Store.DiscMatch(s.Fingerprint); ok && len(dm.Labels) > 0 {
		var ls []TitleLabel
		if json.Unmarshal(dm.Labels, &ls) == nil {
			for _, l := range ls {
				saved[l.TitleID] = l
			}
		}
	}
	var out []TitleLabel
	for _, t := range disc.Titles {
		l := TitleLabel{TitleID: t.ID, Duration: t.Duration}
		if prev, ok := saved[t.ID]; ok && absDur(prev.Duration-t.Duration) < 2*time.Second {
			prev.Guess = "as you labelled it last time"
			out = append(out, prev)
			continue
		}
		p, picked := picks[t.ID]
		switch {
		case picked && p.Episode > 0:
			l.Kind, l.Season, l.Episode, l.Name, l.Rip, l.Guess = LabelEpisode, p.Season, p.Episode, p.EpisodeTitle, true, p.Reason
		case picked:
			l.Kind, l.Rip, l.Guess = LabelMain, true, p.Reason
		case main > 0 && t.Duration > main*9/10:
			l.Kind, l.Guess = LabelSkip, "as long as the feature: another cut, or the same film again"
		case t.Duration <= trailerMax:
			l.Kind, l.Guess = LabelTrailer, "trailer length"
		default:
			l.Kind, l.Guess = LabelExtra, "not part of the rip"
		}
		out = append(out, l)
	}
	return out
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// waitForLabels holds the scanned disc until a person decides. Taking the
// disc out counts as ejecting it. It reports false when the job is
// cancelled or the service stops.
func (m *Manager) waitForLabels(ctx context.Context, d drive.Drive, job *Job, disc *makemkv.Disc, sel *selector.Selection) (LabelDecision, bool) {
	labels := m.guessLabels(job, disc, sel) // reads the job: not under its lock
	job.set(func(j *Job) { j.Labels = labels })
	job.setStage(StageLabelling, "waiting for you to label the titles")
	_ = d.Lock(false) // the tray opens if you change your mind
	job.rt.notifier.Send(ctx, m.event(job, "label"))
	ch := make(chan LabelDecision, 1)
	m.mu.Lock()
	if m.labelWait == nil {
		m.labelWait = map[string]chan LabelDecision{}
	}
	m.labelWait[job.ID] = ch
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.labelWait, job.ID)
		m.mu.Unlock()
	}()
	thumbs, stopThumbs := context.WithCancel(ctx)
	go m.makeThumbnails(thumbs, job)
	// Whatever is decided, the disc is free of previews afterwards.
	defer m.stopPreviews()
	defer stopThumbs()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case dec := <-ch:
			// Out of labelling first: no new preview starts after this.
			job.setStage(StageSelecting, "carrying out your decision")
			return dec, true
		case <-ctx.Done():
			return LabelDecision{}, false
		case <-tick.C:
			if st, err := d.Status(); err == nil && (st == drive.TrayOpen || st == drive.NoDisc) {
				return LabelDecision{Action: "eject", reason: "the disc was taken out before it was labelled"}, true
			}
		}
	}
}

// SubmitLabels hands a person's decision to a disc waiting to be labelled.
func (m *Manager) SubmitLabels(id string, dec LabelDecision) error {
	m.mu.Lock()
	ch := m.labelWait[id]
	m.mu.Unlock()
	job := m.recentJob(id)
	if ch == nil || job == nil {
		return errors.New("this disc is not waiting to be labelled")
	}
	if err := checkLabels(job.Snapshot(), &dec); err != nil {
		return err
	}
	select {
	case ch <- dec:
		return nil
	default:
		return errors.New("a decision for this disc is already being carried out")
	}
}

func checkLabels(s Job, dec *LabelDecision) error {
	switch dec.Action {
	case "rip", "contribute", "eject":
	default:
		return fmt.Errorf("unknown action %q", dec.Action)
	}
	known := map[int]bool{}
	for _, l := range s.Labels {
		known[l.TitleID] = true
	}
	ripping := 0
	for i := range dec.Labels {
		l := &dec.Labels[i]
		if !known[l.TitleID] {
			return fmt.Errorf("title %d is not on this disc", l.TitleID)
		}
		switch l.Kind {
		case LabelMain, LabelTrailer:
		case LabelSkip:
			l.Rip = false
		case LabelExtra:
			if _, ok := extraFolders[l.Category]; l.Category != "" && !ok {
				return fmt.Errorf("title %d: unknown extra category %q", l.TitleID, l.Category)
			}
		case LabelEpisode:
			if l.Rip && (l.Season <= 0 || l.Episode <= 0) {
				return fmt.Errorf("title %d needs a season and episode number", l.TitleID)
			}
		default:
			return fmt.Errorf("title %d: unknown kind %q", l.TitleID, l.Kind)
		}
		l.Name, l.Guess = strings.TrimSpace(l.Name), ""
		if l.Rip {
			ripping++
		}
	}
	if dec.Action == "rip" && ripping == 0 {
		return errors.New("choose at least one title to rip, or keep the disc for TheDiscDB only")
	}
	return nil
}

// applyLabels turns a person's labels into the disc's selection, and
// remembers them for the next time this disc goes in.
func (m *Manager) applyLabels(job *Job, disc *makemkv.Disc, dec LabelDecision) (*selector.Selection, error) {
	s := job.Snapshot()
	edit := dec.Identity
	if edit == nil {
		if s.Identity == nil || !s.Identity.Identified() {
			return nil, errors.New("say what the disc is before ripping it")
		}
		id := s.Identity
		edit = &ReviewEdit{Kind: id.Kind, Title: id.Title, Year: id.Year, TMDBID: id.TMDBID, TVDBID: id.TVDBID, Season: id.Season}
	}
	byID := map[int]*makemkv.Title{}
	for _, t := range disc.Titles {
		byID[t.ID] = t
	}
	base := selector.Selection{Kind: edit.Kind}
	edit.Episodes = map[int]int{}
	next := 0
	var extras []selector.Pick
	for _, l := range dec.Labels {
		t := byID[l.TitleID]
		if t == nil {
			continue
		}
		if !l.Rip {
			why := "left out by a person"
			if l.Name != "" {
				why = fmt.Sprintf("labelled %s: %s", l.Kind, l.Name)
			}
			base.Skipped = append(base.Skipped, selector.Skipped{TitleID: t.ID, Duration: t.Duration, Reason: why})
			continue
		}
		if l.Kind == LabelExtra || l.Kind == LabelTrailer {
			extras = append(extras, selector.Pick{Title: t, Reason: "labelled by a person", Extra: extraFolder(l), ExtraName: l.Name})
			continue
		}
		base.Picks = append(base.Picks, selector.Pick{Title: t, Reason: "labelled by a person"})
		edit.Episodes[t.ID] = l.Episode
		if l.Kind == LabelEpisode {
			if edit.Season <= 0 {
				edit.Season = l.Season
			}
			next = max(next, l.Episode+1)
		}
	}
	if len(base.Picks) == 0 && len(extras) == 0 {
		return nil, errors.New("no title to rip")
	}
	if edit.Kind == "" {
		edit.Kind = metadata.KindMovie
		if next > 0 {
			edit.Kind = metadata.KindTV
		}
	}
	if len(base.Picks) > 0 {
		job.set(func(j *Job) { j.Selection = &base })
		if err := applyEdit(job, edit); err != nil {
			return nil, err
		}
	} else { // only extras: the disc's identity, as given
		if strings.TrimSpace(edit.Title) == "" {
			return nil, errors.New("say what the disc is before ripping its extras")
		}
		id := metadata.Identity{Kind: edit.Kind, Title: edit.Title, Year: edit.Year, TMDBID: edit.TMDBID, TVDBID: edit.TVDBID, Season: edit.Season, Confidence: 1, Source: sourceManual}
		if s.Identity != nil {
			id.Disc, id.Hint = s.Identity.Disc, s.Identity.Hint
		}
		job.set(func(j *Job) { j.Identity, j.Selection = &id, &base })
	}
	job.set(func(j *Job) {
		j.Labels = dec.Labels
		if next > 0 {
			j.Selection.NextEpisode = next
		}
		sort.SliceStable(j.Selection.Picks, func(a, b int) bool { return j.Selection.Picks[a].Episode < j.Selection.Picks[b].Episode })
		// The feature and episodes first; extras after them.
		j.Selection.Picks = append(j.Selection.Picks, extras...)
	})
	s = job.Snapshot()
	if err := m.deps.Store.SetDiscMatch(s.Fingerprint, matchOf(s)); err != nil {
		m.log.Warn("remember labels", "err", err)
	}
	job.logf("labelled by a person: ripping %d title(s)", len(s.Selection.Picks))
	return s.Selection, nil
}
