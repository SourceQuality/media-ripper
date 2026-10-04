package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/metadata"
)

// backupDisc makes the full decrypted copy while the disc is still in the
// drive. It is written under a .part name and renamed when complete, so a
// half copy never looks finished.
func (m *Manager) backupDisc(ctx context.Context, job *Job) error {
	cfg := job.rt.cfg
	s := job.Snapshot()
	root := cfg.BackupDir()
	dest := uniquePath(filepath.Join(root, backupName(s)))
	part := dest + ".part"
	_ = os.RemoveAll(part) // left by an interrupted attempt
	if err := mkdirAll(root, os.FileMode(cfg.Output.DirMode)); err != nil {
		return err
	}
	var total int64
	job.mu.Lock()
	for _, f := range job.discFiles {
		total += f.Size
	}
	job.mu.Unlock()
	job.setStage(StageRipping, "backing up the disc")
	job.logf("full-disc backup to %s", dest)
	var rate meter
	saving := false
	err := job.rt.mk.Backup(ctx, job.Drive, part, func(p makemkv.Progress) {
		if p.Task != "" {
			saving = true
			rate.reset()
		}
		if saving && p.Percent >= 0 {
			if total > 0 {
				job.track(&rate, "backup", "whole disc", int64(float64(total)*p.Percent/100), total)
			} else {
				job.track(&rate, "backup", "whole disc", int64(p.Percent*1e6), 100e6)
			}
		}
	})
	job.untrack("backup")
	if err != nil {
		_ = os.RemoveAll(part)
		return err
	}
	if err := os.Rename(part, dest); err != nil {
		return fmt.Errorf("finish backup: %w", err)
	}
	size := dirSize(dest)
	job.set(func(j *Job) { j.Backup = &Output{Path: dest, Size: size, TitleID: -1} })
	job.logf("backup complete: %s (%.1f GB)", dest, float64(size)/1e9)
	return nil
}

// backupName is a folder name people recognise: "The Twilight Zone (1959)
// S01 D3", "The Thing (1982)", or the label and date.
func backupName(s Job) string {
	id := s.Identity
	name := s.Label
	if id != nil && id.Identified() {
		name = id.Title
		if id.Year > 0 {
			name += fmt.Sprintf(" (%d)", id.Year)
		}
		if id.Kind == metadata.KindTV {
			name += fmt.Sprintf(" S%02d", id.Season)
			if id.Disc > 0 {
				name += fmt.Sprintf(" D%d", id.Disc)
			}
		}
	}
	if name == "" {
		name = "disc " + s.StartedAt.Format("2006-01-02 1504")
	}
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) || r < 32 {
			return '_'
		}
		return r
	}, name)
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n
}
