package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sourcequality/media-ripper/internal/arr"
	"github.com/sourcequality/media-ripper/internal/metadata"
)

// arrFor returns the app that should take over a job's files, if any.
func (rt *runtime) arrFor(id *metadata.Identity) *arr.Client {
	if id == nil || !id.Identified() {
		return nil
	}
	switch id.Kind {
	case metadata.KindMovie:
		return rt.radarr
	case metadata.KindTV:
		return rt.sonarr
	}
	return nil
}

// arrHandoff makes sure the title exists in Radarr/Sonarr and asks it to
// import the delivered files. Failures leave the files in staging and are
// recorded on the job as warnings; the rip itself has succeeded.
func (m *Manager) arrHandoff(ctx context.Context, job *Job) {
	s := job.Snapshot()
	client := job.rt.arrFor(s.Identity)
	if client == nil || len(s.Outputs) == 0 {
		return
	}
	job.setStage(StageHandoff, "importing into "+string(client.Kind))
	if err := m.doHandoff(ctx, job, client, s); err != nil {
		job.logf("%s: %v (files left in staging)", client.Kind, err)
		job.warn(fmt.Sprintf("%s import failed: %v", client.Kind, err))
		return
	}
	job.logf("%s import complete", client.Kind)
}

func (m *Manager) doHandoff(ctx context.Context, job *Job, client *arr.Client, s Job) error {
	cfg := job.rt.cfg
	id := s.Identity
	item, err := m.ensureInLibrary(ctx, job, client, id)
	if err != nil {
		return err
	}
	if item.Path != "" {
		job.logf("%s library path: %s", client.Kind, item.Path)
	}
	dir := commonDir(s.Outputs)
	if dir == "" {
		return errors.New("no delivered files")
	}
	if err := client.Import(ctx, dir, cfg.Arr.ImportTimeout.D()); err != nil {
		return err
	}
	// After a move import the staging folders are empty; tidy them up to
	// the staging root. A copy import leaves the files in place by design.
	if strings.EqualFold(client.Cfg.ImportMode, "move") {
		staging := filepath.Join(cfg.Output.Path, cfg.Arr.StagingSubdir)
		removeEmptyUpTo(dir, staging)
	}
	return nil
}

// ensureInLibrary finds the title in the app, adding it when allowed.
func (m *Manager) ensureInLibrary(ctx context.Context, job *Job, client *arr.Client, id *metadata.Identity) (arr.Item, error) {
	var term string
	switch {
	case client.Kind == arr.Radarr && id.TMDBID > 0:
		term = "tmdb:" + strconv.Itoa(id.TMDBID)
	case client.Kind == arr.Sonarr && id.TVDBID > 0:
		term = "tvdb:" + strconv.Itoa(id.TVDBID)
	default:
		term = id.Title
		if id.Year > 0 {
			term += " " + strconv.Itoa(id.Year)
		}
	}
	items, err := client.Lookup(ctx, term)
	if err != nil {
		return arr.Item{}, err
	}
	var pick *arr.Item
	for i := range items {
		it := &items[i]
		if client.Kind == arr.Radarr && id.TMDBID > 0 && it.TMDBID == id.TMDBID {
			pick = it
			break
		}
		if client.Kind == arr.Sonarr && id.TVDBID > 0 && it.TVDBID == id.TVDBID {
			pick = it
			break
		}
		if metadata.Similarity(id.Title, it.Title) >= 0.8 && (id.Year == 0 || it.Year == 0 || absInt(it.Year-id.Year) <= 1) {
			pick = it
			break
		}
	}
	if pick == nil {
		return arr.Item{}, fmt.Errorf("%q not found by %s lookup", term, client.Kind)
	}
	lib, ok, err := client.InLibrary(ctx, *pick)
	if err != nil {
		return arr.Item{}, err
	}
	if ok {
		return lib, nil
	}
	if !client.Cfg.AddMissing {
		return arr.Item{}, fmt.Errorf("%s is not in the %s library and add_missing is off", pick.Title, client.Kind)
	}
	job.logf("adding %s to %s", pick.Title, client.Kind)
	return client.Add(ctx, *pick)
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// commonDir is the deepest directory containing every output.
func commonDir(outs []Output) string {
	if len(outs) == 0 {
		return ""
	}
	dir := filepath.Dir(outs[0].Path)
	for _, o := range outs[1:] {
		d := filepath.Dir(o.Path)
		for !strings.HasPrefix(d+"/", dir+"/") {
			dir = filepath.Dir(dir)
			if dir == "/" || dir == "." {
				return dir
			}
		}
	}
	return dir
}

// removeEmptyUpTo removes dir and its empty parents, stopping at root.
func removeEmptyUpTo(dir, root string) {
	root = filepath.Clean(root)
	for d := filepath.Clean(dir); d != root && strings.HasPrefix(d, root+string(filepath.Separator)); d = filepath.Dir(d) {
		entries, err := os.ReadDir(d)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(d); err != nil {
			return
		}
	}
}
