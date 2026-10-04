package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/ocr"
	"github.com/sourcequality/media-ripper/internal/selector"
)

// ocrIdentify is the second chance for a disc whose label told us nothing:
// read the title card of the longest ripped file and confirm by runtime.
// needsOCR reports whether ocrIdentify will look at the ripped files before
// they are named, which rules out delivering titles while others still rip.
func (m *Manager) needsOCR(job *Job) bool {
	cfg := job.rt.cfg
	s := job.Snapshot()
	if !cfg.Metadata.OCR.Enabled || (s.Identity != nil && s.Identity.Identified()) {
		return false
	}
	_, none := job.rt.provider.(metadata.NoneProvider)
	return !none
}

func (m *Manager) ocrIdentify(ctx context.Context, job *Job, disc *makemkv.Disc, sel *selector.Selection, files []string) {
	cfg := job.rt.cfg
	s := job.Snapshot()
	if !cfg.Metadata.OCR.Enabled || (s.Identity != nil && s.Identity.Identified()) || len(files) == 0 {
		return
	}
	if _, ok := job.rt.provider.(metadata.NoneProvider); ok {
		return
	}
	opts := ocr.Options{
		Tesseract:     cfg.Metadata.OCR.Tesseract,
		Languages:     cfg.Metadata.OCR.Languages,
		HeadMinutes:   cfg.Metadata.OCR.HeadMinutes,
		TailMinutes:   cfg.Metadata.OCR.TailMinutes,
		FramesPerMin:  cfg.Metadata.OCR.FramesPerMin,
		MaxCandidates: cfg.Metadata.OCR.MaxCandidates,
		Timeout:       cfg.Metadata.OCR.Timeout.D(),
		Logger:        m.log,
	}
	if job.rt.ocrTools != nil {
		opts.FFmpeg, opts.Tesseract = job.rt.ocrTools[0], job.rt.ocrTools[1]
	}
	if err := ocr.Available(opts); err != nil {
		job.logf("ocr skipped: %v", err)
		return
	}
	// Longest pick is the feature or a full episode: best odds of a title card.
	best := 0
	for i, p := range sel.Picks {
		if p.Title.Duration > sel.Picks[best].Title.Duration {
			best = i
		}
	}
	job.setStage(StageIdentifying, "reading title card")
	start := time.Now()
	match, err := ocr.Identify(ctx, files[best], sel.Picks[best].Title.Duration, cfg.Selection.MovieRuntimeTolerance.D(), job.rt.provider, selector.DiscHints(disc), opts)
	if err != nil {
		job.logf("ocr failed: %v", err)
		return
	}
	for i, c := range match.Candidates {
		if i >= 5 {
			break
		}
		job.logf("ocr text: %q (%d frames)", c.Text, c.Frames)
	}
	if match.Identity == nil {
		job.logf("ocr: no confirmed match (%s)", time.Since(start).Round(time.Second))
		return
	}
	id := match.Identity
	if id.Season == 0 && s.Identity != nil {
		id.Season = s.Identity.Hint.Season
	}
	if id.Kind == metadata.KindTV && id.Season == 0 {
		id.Season = 1
	}
	job.logf("ocr identified: %s %s (%d) from %q (%s)", id.Kind, id.Title, id.Year, match.Candidate.Text, time.Since(start).Round(time.Second))
	job.set(func(j *Job) { j.Identity = id })

	// Picks were made without an identity; give TV episodes their numbers
	// and titles now that the series is known.
	if id.Kind == metadata.KindTV {
		next := m.deps.Store.NextEpisode(seriesKey(id), id.Season)
		for i := range sel.Picks {
			p := &sel.Picks[i]
			p.Season = id.Season
			if p.Episode == 0 {
				p.Episode = next
				next++
			} else {
				p.Episode += next - 1
				if p.EpisodeEnd > 0 {
					p.EpisodeEnd += next - 1
				}
				next = p.Episode + 1
				if p.EpisodeEnd > p.Episode {
					next = p.EpisodeEnd + 1
				}
			}
			for _, e := range id.Episodes {
				if e.Number == p.Episode {
					p.EpisodeTitle = e.Title
				}
			}
			p.Reason += fmt.Sprintf("; numbered after ocr as S%02dE%02d", p.Season, p.Episode)
		}
		sel.NextEpisode = next
		sel.Kind = metadata.KindTV
	} else {
		sel.Kind = metadata.KindMovie
	}
	job.set(func(j *Job) { j.Selection = sel })
}
