package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

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
// recorded on the job as warnings; the rip itself has succeeded. An import
// cut short by shutdown is marked pending and runs again at the next start.
func (m *Manager) arrHandoff(ctx context.Context, job *Job) {
	s := job.Snapshot()
	client := job.rt.arrFor(s.Identity)
	if client == nil || len(s.Outputs) == 0 {
		return
	}
	if len(mainOutputs(s.Outputs)) == 0 {
		// Only extras: they go next to what is already in the library.
		if item, err := m.ensureInLibrary(ctx, job, client, s.Identity); err == nil {
			m.placeExtras(job, client, item)
		} else {
			markPlaced(job, client.Kind, fmt.Sprintf("not placed: %v", err))
		}
		return
	}
	job.setStage(StageHandoff, "importing into "+string(client.Kind))
	// Marked before the import starts, so even a crash leaves it to retry.
	if err := m.deps.Store.SetImportPending(job.ID, true); err != nil {
		m.log.Warn("mark import pending", "job", job.ID, "err", err)
	}
	err := m.doHandoff(ctx, job, client, s)
	if err != nil && ctx.Err() != nil {
		job.logf("%s import interrupted; it runs again when media-ripper starts", client.Kind)
		job.warn(fmt.Sprintf("%s import interrupted; it runs again when media-ripper starts", client.Kind))
		markImports(job, func(o Output) string {
			if o.Import == "imported" {
				return o.Import
			}
			return "not imported: interrupted"
		})
		return
	}
	_ = m.deps.Store.SetImportPending(job.ID, false)
	if err != nil {
		job.logf("%s: %v (files left in staging)", client.Kind, err)
		job.warn(fmt.Sprintf("%s import failed: %v", client.Kind, err))
		markImports(job, func(o Output) string {
			if o.Import == "imported" {
				return o.Import
			}
			return "not imported: " + err.Error()
		})
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
	// Extras are not Radarr's or Sonarr's to import; they follow below.
	outs := mainOutputs(s.Outputs)
	s.Outputs = outs
	dir := commonDir(outs)
	if dir == "" {
		return errors.New("no delivered files")
	}
	// Files the app already holds (an earlier import that was reported
	// as failed, or one done by hand) need no new import.
	already := map[int64]bool{}
	if item.ID != 0 {
		if sizes, err := client.Holds(ctx, item.ID); err == nil {
			already = sizes
		}
	}
	allThere := true
	for _, o := range s.Outputs {
		allThere = allThere && already[o.Size]
	}
	res := arr.ImportResult{Rejected: map[string][]string{}, ItemID: item.ID}
	if allThere {
		job.logf("%s already has every file", client.Kind)
	} else {
		if res, err = client.Import(ctx, dir, arr.ImportOptions{Wait: cfg.Arr.ImportTimeout.D(), Quality: importQuality(client.Kind, s)}); err != nil {
			return err
		}
		job.logf("%s was given %d file(s)", client.Kind, res.Imported)
		// A file it already holds is rejected as "not an upgrade"; that
		// one is in, not left out.
		for _, o := range s.Outputs {
			if already[o.Size] {
				delete(res.Rejected, filepath.Base(o.Path))
			}
		}
		if len(res.Rejected) > 0 {
			job.warn(fmt.Sprintf("%s did not take %s", client.Kind, res.DescribeRejected()))
		}
	}
	itemID := res.ItemID
	if itemID == 0 {
		itemID = item.ID
	}
	// The command can report success for files it then failed to take
	// (seen when it could not delete them from staging), so ask the app
	// what it holds now.
	var expect []Output
	for _, o := range s.Outputs {
		if _, rejected := res.Rejected[filepath.Base(o.Path)]; !rejected {
			expect = append(expect, o)
		}
	}
	held, err := confirmHeld(ctx, client, itemID, expect)
	if err != nil {
		return err
	}
	move := strings.EqualFold(client.Cfg.ImportMode, "move")
	var missing []string
	for _, o := range expect {
		if !held[o.Path] {
			missing = append(missing, filepath.Base(o.Path))
		} else if move {
			// The app copied it; finish the move here, as the owner of
			// the staged file.
			if err := os.Remove(o.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				job.logf("remove %s from staging: %v", o.Path, err)
			}
		}
	}
	markImports(job, func(o Output) string {
		if reasons, ok := res.Rejected[filepath.Base(o.Path)]; ok {
			return "not imported: " + strings.Join(reasons, "; ")
		}
		if !held[o.Path] {
			return fmt.Sprintf("not imported: %s did not keep it (see its log)", client.Kind)
		}
		return "imported"
	})
	if move {
		// Tidy the emptied staging folders up to the staging root. A copy
		// import leaves the files in place by design.
		removeEmptyUpTo(dir, filepath.Join(cfg.Output.Path, cfg.Arr.StagingSubdir))
	}
	if len(missing) > 0 {
		return fmt.Errorf("imported %d of %d files; not taken: %s", len(s.Outputs)-len(res.Rejected)-len(missing), len(s.Outputs), strings.Join(missing, ", "))
	}
	m.placeExtras(job, client, item)
	return nil
}

// markPlaced records why the job's extras stayed in staging. The warning
// starts with the app's name, so a successful retry clears it.
func markPlaced(job *Job, kind arr.Kind, why string) {
	job.warn(fmt.Sprintf("%s: extras left in staging: %s", kind, strings.TrimPrefix(why, "not placed: ")))
	job.set(func(j *Job) {
		for i := range j.Outputs {
			if j.Outputs[i].Extra != "" {
				j.Outputs[i].Import = why
			}
		}
	})
}

func firstValue(m map[string]string) string {
	for _, v := range m {
		return v
	}
	return ""
}

// mainOutputs are the outputs Radarr/Sonarr import: all but extras.
func mainOutputs(outs []Output) []Output {
	var main []Output
	for _, o := range outs {
		if o.Extra == "" {
			main = append(main, o)
		}
	}
	return main
}

// placeExtras moves the disc's extras from their staging folder into the
// library folder of the movie or show Radarr/Sonarr imported, where Plex
// and Jellyfin find them ("…/Featurettes/The Making of.mkv").
func (m *Manager) placeExtras(job *Job, client *arr.Client, item arr.Item) {
	s := job.Snapshot()
	var extras []Output
	for _, o := range s.Outputs {
		if o.Extra != "" {
			extras = append(extras, o)
		}
	}
	if len(extras) == 0 {
		return
	}
	if item.Path == "" {
		markPlaced(job, client.Kind, fmt.Sprintf("not placed: %s did not say where the movie's folder is", client.Kind))
		return
	}
	lib := client.LocalPath(item.Path)
	cfg := job.rt.cfg
	staging := filepath.Join(cfg.Output.Path, cfg.Arr.StagingSubdir, extrasSubdir, filepath.Base(s.ID))
	// New folders and files take the library folder's group, so Radarr or
	// Sonarr (often another user) can still manage them.
	gid := -1
	if st, err := os.Stat(lib); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			gid = int(sys.Gid)
		}
	}
	moved := map[string]string{}
	failed := map[string]string{}
	for _, o := range extras {
		if _, err := os.Stat(o.Path); err != nil {
			continue // placed before
		}
		dst := filepath.Join(lib, o.Extra, filepath.Base(o.Path))
		if !cfg.Output.Overwrite {
			dst = uniquePath(dst)
		}
		err := mkdirAll(filepath.Dir(dst), os.FileMode(cfg.Output.DirMode))
		if err == nil {
			_, err = moveFile(context.Background(), o.Path, dst, os.FileMode(cfg.Output.FileMode), nil)
		}
		if err != nil {
			why := err.Error()
			if errors.Is(err, os.ErrPermission) {
				why = fmt.Sprintf("media-ripper may not write in %s's folder %s (give the media-ripper user that folder's group, e.g. sudo usermod -aG <group> media-ripper, then restart it)", client.Kind, lib)
			}
			failed[o.Path] = "not placed: " + why
			continue
		}
		if gid >= 0 {
			_ = os.Lchown(filepath.Dir(dst), -1, gid)
			_ = os.Lchown(dst, -1, gid)
		}
		moved[o.Path] = dst
		job.logf("placed %s in %s", filepath.Base(dst), filepath.Dir(dst))
	}
	if len(failed) > 0 {
		job.warn(fmt.Sprintf("%s: %d extra(s) left in staging: %s", client.Kind, len(failed), strings.TrimPrefix(firstValue(failed), "not placed: ")))
	}
	job.set(func(j *Job) {
		for i := range j.Outputs {
			if dst, ok := moved[j.Outputs[i].Path]; ok {
				j.Outputs[i].Path, j.Outputs[i].Import = dst, ""
			} else if why, ok := failed[j.Outputs[i].Path]; ok {
				j.Outputs[i].Import = why
			}
		}
	})
	removeEmptyTree(staging) // an extra that could not be moved stays
}

// removeEmptyTree removes dir and the folders under it that hold no file.
func removeEmptyTree(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			removeEmptyTree(filepath.Join(dir, e.Name()))
		}
	}
	_ = os.Remove(dir) // fails, harmlessly, while anything is left in it
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
	added, err := client.Add(ctx, *pick)
	if err != nil {
		return arr.Item{}, err
	}
	if err := client.WaitReady(ctx, added, arrReadyWait); err != nil {
		return arr.Item{}, err
	}
	return added, nil
}

// arrReadyWait bounds how long a newly added series may take to load.
var arrReadyWait = 2 * time.Minute

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

// importQuality names the quality of a MakeMKV rip in the app's terms: a
// Blu-ray rip is a remux (Sonarr "Bluray-1080p Remux", Radarr
// "Remux-1080p"), a DVD rip is "DVD". Empty lets the app guess.
func importQuality(kind arr.Kind, s Job) string {
	if strings.HasPrefix(strings.ToLower(s.DiscType), "dvd") {
		return "DVD"
	}
	if !strings.Contains(strings.ToLower(s.DiscType), "blu-ray") || s.Selection == nil || len(s.Selection.Picks) == 0 {
		return ""
	}
	res := ""
	pick := s.Selection.Picks[0]
	for _, p := range s.Selection.Picks {
		if p.Extra == "" { // the feature's quality, not a bonus clip's
			pick = p
			break
		}
	}
	if v := pick.Title.Video(); v != nil {
		res = resolutionName(v.VideoSize)
	}
	if res != "1080p" && res != "2160p" {
		return ""
	}
	if kind == arr.Sonarr {
		return "Bluray-" + res + " Remux"
	}
	return "Remux-" + res
}

// markImports records what the app did with each delivered file.
func markImports(job *Job, status func(o Output) string) {
	job.set(func(j *Job) {
		for i := range j.Outputs {
			if j.Outputs[i].Extra == "" { // extras are placed, not imported
				j.Outputs[i].Import = status(j.Outputs[i])
			}
		}
	})
}

// heldWait is how long an imported file may take to show up in the app's
// file list after its command completed.
var (
	heldWait = 75 * time.Second
	heldPoll = 2 * time.Second
)

// confirmHeld reports, by path, which outputs the app now holds: a file of
// the same size among the series' or movie's files.
func confirmHeld(ctx context.Context, client *arr.Client, itemID int, outs []Output) (map[string]bool, error) {
	held := map[string]bool{}
	if len(outs) == 0 {
		return held, nil
	}
	if itemID == 0 {
		return nil, fmt.Errorf("%s did not say which title the files belong to", client.Kind)
	}
	deadline := time.Now().Add(heldWait)
	for {
		sizes, err := client.Holds(ctx, itemID)
		if err != nil {
			return nil, err
		}
		all := true
		for _, o := range outs {
			held[o.Path] = sizes[o.Size]
			all = all && held[o.Path]
		}
		if all || time.Now().After(deadline) {
			return held, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(heldPoll):
		}
	}
}
