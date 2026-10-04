package pipeline

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// progressEvery is how often the live Discord message is edited: well
// inside Discord's rate limits, often enough to follow a rip.
var progressEvery = 15 * time.Second

// liveProgress keeps the disc's "Ripping" message up to date until stop is
// closed. Updates that would say nothing new are not sent.
func (m *Manager) liveProgress(ctx context.Context, job *Job, stop <-chan struct{}) {
	t := time.NewTicker(progressEvery)
	defer t.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-t.C:
		}
		ev := m.event(job, "progress")
		ev.Items, ev.Summary = progressLines(job.Snapshot())
		key := ev.Summary + strings.Join(ev.Items, "\n")
		if key == last {
			continue
		}
		last = key
		job.rt.notifier.Send(ctx, ev)
	}
}

// progressLines describes each title of the disc in progress, one line
// each, and the disc as a whole: the same states as the web UI's drive card.
func progressLines(s Job) ([]string, string) {
	if s.Selection == nil {
		return nil, ""
	}
	outs := map[int]Output{}
	for _, o := range s.Outputs {
		outs[o.TitleID] = o
	}
	acts := map[string]Activity{}
	for _, a := range s.Activities {
		acts[a.Kind+":"+a.Item] = a
	}
	type row struct {
		order  int
		season int
		ep     int
		line   string
	}
	var rows []row
	ripDone := s.Stage != StageRipping
	for i, p := range s.Selection.Picks {
		item := pickName(p)
		label := "Title " + fmt.Sprint(p.Title.ID)
		if p.Episode > 0 {
			label = strings.TrimSpace(fmt.Sprintf("%s %s", item, p.EpisodeTitle))
		} else if s.Identity != nil && s.Identity.Title != "" {
			label = "Main movie"
		}
		var state string
		switch o, ok := outs[p.Title.ID]; {
		case ok && o.Import != "" && o.Import != "imported":
			state = "⚠️ " + label + " · " + o.Import
		case ok && o.Import == "imported":
			state = "✅ " + label + " · imported"
		case ok:
			state = "✅ " + label + " · delivered"
		default:
			if a, live := firstActivity(acts, item); live {
				verb := map[string]string{"rip": "ripping", "remux": "remuxing", "copy": "copying"}[a.Kind]
				parts := []string{verb}
				if a.Percent >= 0 {
					parts[0] = fmt.Sprintf("%s %d%%", verb, int(a.Percent))
				}
				if a.Speed > 0 {
					parts = append(parts, fmt.Sprintf("%.1f MB/s", a.Speed/1e6))
				}
				if a.ETASeconds > 0 {
					parts = append(parts, (time.Duration(a.ETASeconds)*time.Second).String()+" left")
				}
				state = "▶️ " + label + " · " + strings.Join(parts, " · ")
			} else if ripDone || i < s.Current-1 {
				state = "☑️ " + label + " · ripped, waiting to copy"
			} else {
				state = "⏳ " + label
			}
		}
		rows = append(rows, row{i, p.Season, p.Episode, state})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].season != rows[j].season {
			return rows[i].season < rows[j].season
		}
		return rows[i].ep < rows[j].ep
	})
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = r.line
	}
	return lines, progressSummary(s)
}

func firstActivity(acts map[string]Activity, item string) (Activity, bool) {
	for _, k := range []string{"copy", "remux", "rip"} {
		if a, ok := acts[k+":"+item]; ok {
			return a, true
		}
	}
	return Activity{}, false
}

// progressSummary is the disc's overall state in a few words.
func progressSummary(s Job) string {
	total := 0
	if s.Selection != nil {
		total = len(s.Selection.Picks)
	}
	done := len(s.Outputs)
	switch s.Stage {
	case StageRipping:
		for _, a := range s.Activities {
			if a.Kind == "backup" {
				return fmt.Sprintf("Backing up the whole disc · %d of %d delivered", done, total)
			}
		}
		return fmt.Sprintf("Ripping %d of %d · %d delivered", max(s.Current, 1), total, done)
	case StagePostProcess, StageDelivering:
		return fmt.Sprintf("Disc ripped, tray open · copying to the library · %d of %d delivered", done, total)
	case StageHandoff:
		return "Importing into Radarr/Sonarr"
	case StageDone:
		return fmt.Sprintf("Done · %d of %d delivered in %s", done, total, s.Elapsed)
	case StageReview:
		return "Waiting for review in the web UI"
	case StageFailed:
		return "Failed: " + s.Error
	case StageCancelled:
		return "Cancelled"
	}
	return string(s.Stage)
}
