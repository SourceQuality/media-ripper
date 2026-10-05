package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sourcequality/media-ripper/internal/odm"
	"github.com/sourcequality/media-ripper/internal/store"
)

// saveInventory keeps the disc's file inventory with the finished job.
func (m *Manager) saveInventory(job *Job) {
	job.mu.Lock()
	files, hash, id := job.discFiles, job.ContentHash, job.ID
	job.mu.Unlock()
	if len(files) == 0 {
		return
	}
	inv := store.DiscInventory{ContentHash: hash}
	for _, f := range files {
		inv.Files = append(inv.Files, store.FileEntry{Path: f.Path, Size: f.Size, Modified: f.Modified})
	}
	if err := m.deps.Store.SaveInventory(id, inv); err != nil {
		m.log.Warn("save disc inventory", "job", id, "err", err)
	}
}

// Record returns a job by id: running or recent in memory, else from the
// saved history.
func (m *Manager) Record(id string) (Job, bool) {
	if j, ok := m.Job(id); ok {
		return j, true
	}
	h, err := m.deps.Store.History(0)
	if err != nil {
		return Job{}, false
	}
	for _, raw := range h {
		var j Job
		if json.Unmarshal(raw, &j) == nil && j.ID == id {
			return j, true
		}
	}
	return Job{}, false
}

// DiscFiles is the disc's file inventory as read for its content hash:
// from the job while it runs, from the saved inventory afterwards. Nothing
// is read from the disc again.
func (m *Manager) DiscFiles(id string) (store.DiscInventory, error) {
	if job := m.recentJob(id); job != nil {
		job.mu.Lock()
		files, hash := job.discFiles, job.ContentHash
		job.mu.Unlock()
		if len(files) > 0 {
			inv := store.DiscInventory{ContentHash: hash}
			for _, f := range files {
				inv.Files = append(inv.Files, store.FileEntry{Path: f.Path, Size: f.Size, Modified: f.Modified})
			}
			return inv, nil
		}
	}
	inv, err := m.deps.Store.Inventory(id)
	if err != nil || len(inv.Files) == 0 {
		return store.DiscInventory{}, errors.New("no file inventory was read from this disc (it is read at the start of a rip; needs v0.6.0 or later)")
	}
	return inv, nil
}

// DiscManifest builds the Optical Disc Manifest of a job, for contributing
// the disc to TheDiscDB. It works while the disc is still ripping.
func (m *Manager) DiscManifest(id, version string) (*odm.Manifest, error) {
	j, ok := m.Record(id)
	if !ok {
		return nil, errors.New("no such job")
	}
	inv, err := m.DiscFiles(id)
	if err != nil {
		return nil, err
	}
	files := make([]odm.File, 0, len(inv.Files))
	for _, f := range inv.Files {
		files = append(files, odm.File{Path: f.Path, SizeBytes: f.Size})
	}
	format := "unknown"
	switch dt := strings.ToLower(j.DiscType); {
	case strings.Contains(dt, "dvd"):
		format = "dvd"
	case strings.Contains(dt, "blu-ray"):
		format = "blu-ray"
		for _, t := range j.Titles {
			if strings.HasSuffix(t.Video, "2160") || strings.Contains(t.Video, "x2160") {
				format = "uhd-blu-ray"
			}
		}
	}
	var titles []odm.Title
	for _, t := range j.Titles {
		src, ok := odm.BlurayTitleSource(t.Source)
		if !ok {
			continue
		}
		titles = append(titles, odm.Title{Source: src, DurationSeconds: t.Duration.Seconds(), SizeBytes: t.SizeBytes, ChapterCount: t.Chapters})
	}
	captured := j.StartedAt
	if captured.IsZero() {
		captured = time.Now()
	}
	return odm.New(version, format, j.Label, inv.ContentHash, files, titles, captured)
}

// ContributionText lists what each title of a finished disc turned out to
// be, as confirmed by TheDiscDB, a person or an import, for entering on
// thediscdb.com/contribute next to the disc manifest.
func (m *Manager) ContributionText(id string) (string, error) {
	j, ok := m.Record(id)
	if !ok {
		return "", errors.New("no such job")
	}
	var b strings.Builder
	name := j.Label
	if j.Identity != nil && j.Identity.Identified() {
		name = j.Identity.Title
		if j.Identity.Year > 0 {
			name += fmt.Sprintf(" (%d)", j.Identity.Year)
		}
	}
	fmt.Fprintf(&b, "%s — disc label %s\n", name, j.Label)
	if j.ContentHash != "" {
		fmt.Fprintf(&b, "Content hash: %s\n", j.ContentHash)
	}
	if j.Verification != "" {
		fmt.Fprintf(&b, "%s\n", j.Verification)
	}
	b.WriteString("\n")
	picks := map[int]string{}
	if j.Selection != nil {
		for _, p := range j.Selection.Picks {
			switch {
			case p.Episode > 0:
				picks[p.Title.ID] = strings.TrimSpace(fmt.Sprintf("Episode S%02dE%02d %s", p.Season, p.Episode, p.EpisodeTitle))
			default:
				picks[p.Title.ID] = "Main movie"
			}
		}
		for _, s := range j.Selection.Skipped {
			if _, ok := picks[s.TitleID]; !ok {
				picks[s.TitleID] = "not ripped: " + s.Reason
			}
		}
	}
	// A person's labels name every title, extras and trailers included.
	for _, l := range j.Labels {
		switch l.Kind {
		case LabelMain:
			picks[l.TitleID] = strings.TrimSpace("Main movie " + l.Name)
		case LabelEpisode:
			picks[l.TitleID] = strings.TrimSpace(fmt.Sprintf("Episode S%02dE%02d %s", l.Season, l.Episode, l.Name))
		case LabelExtra:
			picks[l.TitleID] = "Extra: " + l.Name
		case LabelTrailer:
			picks[l.TitleID] = "Trailer: " + l.Name
		case LabelSkip:
			picks[l.TitleID] = "nothing worth keeping"
		}
	}
	for _, t := range j.Titles {
		what := picks[t.ID]
		if what == "" {
			what = "not ripped"
		}
		fmt.Fprintf(&b, "%-12s %8s  %-8s  %2d ch  %s\n", t.Source, fmtClock(t.Duration), t.Size, t.Chapters, what)
	}
	return b.String(), nil
}

func fmtClock(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	return fmt.Sprintf("%d:%02d:%02d", s/3600, s%3600/60, s%60)
}
