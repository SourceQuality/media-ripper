package pipeline

import (
	"archive/zip"
	"compress/flate"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sourcequality/media-ripper/internal/drive"
	"github.com/sourcequality/media-ripper/internal/store"
	"github.com/sourcequality/media-ripper/internal/udf"
)

// TheDiscDB adds a disc by having the browser read a folder with the
// disc's layout: the names, sizes and dates of the video streams (for the
// content hash) and the contents of the small metadata files (the AACS
// key file for the disc id; playlists, clip info and index for the scan).
// After the rip media-ripper keeps those small files, so the folder can
// be rebuilt without the disc: the streams become placeholders of the
// right size that are never read.

const (
	folderFileMax  = 16 << 20  // larger files are placeholders too
	folderTotalMax = 256 << 20 // contents kept per disc, at most
	folderReadWait = 5 * time.Minute
	folderSubdir   = "_thediscdb" // under output.path, for the NAS copy
)

// folderContent reports whether a file's contents belong in the folder:
// everything small except the audio/video streams.
func folderContent(f udf.File) bool {
	if f.Size > folderFileMax {
		return false
	}
	switch strings.ToLower(path.Ext(f.Path)) {
	case ".m2ts", ".ssif", ".vob", ".evo":
		return false
	}
	return true
}

type folderRequest struct {
	jobID string
	at    time.Time
}

// folderDir holds a job's kept files.
func (m *Manager) folderDir(jobID string) string {
	return filepath.Join(m.Config().StateDir(), "discfolders", filepath.Base(jobID))
}

// wantFolder reports whether a job's folder should be kept after its rip.
func wantFolder(s Job, mode string) bool {
	switch mode {
	case "off":
		return false
	case "always":
		return true
	}
	return !s.HashMatched
}

// readFolder reads a disc's small files from device and keeps them (and
// the file list, with dates) for job id.
func (m *Manager) readFolder(ctx context.Context, id, device string) (int, error) {
	read := m.deps.ReadDisc
	if read == nil {
		read = udf.ReadDevice
	}
	type result struct {
		files []udf.File
		data  map[string][]byte
		err   error
	}
	done := make(chan result, 1)
	go func() {
		files, data, err := read(device, folderContent, folderTotalMax)
		done <- result{files, data, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(folderReadWait):
		return 0, errors.New("reading the disc took too long")
	}
	if r.err != nil {
		return 0, r.err
	}
	hash, err := udf.ContentHash(r.files)
	if err != nil {
		return 0, err
	}
	dir := m.folderDir(id)
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	for p, b := range r.data {
		dst, ok := safeJoin(tmp, p)
		if !ok {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, err
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return 0, err
		}
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return 0, err
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return 0, err
	}
	inv := store.DiscInventory{ContentHash: hash}
	for _, f := range r.files {
		inv.Files = append(inv.Files, store.FileEntry{Path: f.Path, Size: f.Size, Modified: f.Modified})
	}
	if err := m.deps.Store.SaveInventory(id, inv); err != nil {
		return 0, err
	}
	if job := m.recentJob(id); job != nil {
		job.set(func(j *Job) { j.discFiles = r.files })
	}
	return len(r.data), nil
}

// keepFolder reads a ripped disc's small files before it is ejected.
func (m *Manager) keepFolder(ctx context.Context, job *Job, device string) {
	if !wantFolder(job.Snapshot(), job.rt.cfg.Metadata.TheDiscDB.DiscFolder) {
		return
	}
	job.set(func(j *Job) { j.Message = "keeping the disc's folder for TheDiscDB" })
	start := time.Now()
	n, err := m.readFolder(ctx, job.ID, device)
	if err != nil {
		job.logf("TheDiscDB folder not kept: %v", err)
		return
	}
	job.logf("kept the disc's folder for TheDiscDB (%d small files, %s)", n, time.Since(start).Round(time.Second))
}

// DiscFolder describes what can be offered for TheDiscDB's "Add a disc".
type DiscFolder struct {
	Kept    bool   `json:"kept"`              // the small files are kept
	Waiting bool   `json:"waiting,omitempty"` // read when the disc goes in
	Files   int    `json:"files,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"` // the disc's full size
	NAS     string `json:"nas,omitempty"`   // the copy on the library share
	Name    string `json:"name,omitempty"`
}

// DiscFolderStatus says whether a job's folder can be offered.
func (m *Manager) DiscFolderStatus(id string) (DiscFolder, error) {
	j, ok := m.Record(id)
	if !ok {
		return DiscFolder{}, errors.New("no such job")
	}
	st := DiscFolder{Name: folderName(j)}
	if _, err := os.Stat(m.folderDir(id)); err == nil {
		if inv, err := m.deps.Store.Inventory(id); err == nil {
			st.Kept, st.Files = true, len(inv.Files)
			for _, f := range inv.Files {
				st.Bytes += f.Size
			}
		}
	}
	if p := m.nasFolder(j); p != "" {
		if _, err := os.Stat(p); err == nil {
			st.NAS = p
		}
	}
	m.mu.Lock()
	for _, r := range m.folderWanted {
		st.Waiting = st.Waiting || r.jobID == id
	}
	m.mu.Unlock()
	return st, nil
}

// RequestDiscFolder reads the folder of an already ripped disc the next
// time it goes in (within an hour) instead of skipping it.
func (m *Manager) RequestDiscFolder(id string) error {
	j, ok := m.Record(id)
	if !ok || j.Fingerprint == "" {
		return errors.New("this disc was not recorded with a fingerprint")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.folderWanted == nil {
		m.folderWanted = map[string]folderRequest{}
	}
	m.folderWanted[j.Fingerprint] = folderRequest{jobID: id, at: time.Now()}
	return nil
}

// takeFolderRequest returns the job waiting for this disc, if any.
func (m *Manager) takeFolderRequest(fp string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.folderWanted[fp]
	if !ok {
		return "", false
	}
	delete(m.folderWanted, fp)
	if time.Since(r.at) > time.Hour {
		return "", false
	}
	return r.jobID, true
}

// readRequestedFolder serves a request for an inserted disc, then ejects.
func (r *runner) readRequestedFolder(ctx context.Context, d drive.Drive, id string) {
	r.log.Info("reading the disc's folder for TheDiscDB", "job", id)
	n, err := r.m.readFolder(ctx, id, r.path)
	if err != nil {
		r.log.Warn("TheDiscDB folder", "job", id, "err", err)
		r.mu.Lock()
		r.lastErr = "TheDiscDB folder: " + err.Error()
		r.mu.Unlock()
		return
	}
	r.log.Info("kept the disc's folder for TheDiscDB", "job", id, "files", n)
	if r.m.Config().Eject.OnSuccess {
		_ = d.Eject()
	}
}

func folderName(j Job) string {
	name := strings.TrimSpace(j.Label)
	if name == "" {
		name = j.ID
	}
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r < 32 || strings.ContainsRune(`*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	return strings.Trim(name, ". ")
}

func (m *Manager) nasFolder(j Job) string {
	return filepath.Join(m.Config().Output.Path, folderSubdir, folderName(j))
}

// folderFiles lists the folder's files in order with where their contents
// are kept ("" for a placeholder).
func (m *Manager) folderFiles(id string) (Job, []store.FileEntry, func(p string) string, error) {
	j, ok := m.Record(id)
	if !ok {
		return Job{}, nil, nil, errors.New("no such job")
	}
	dir := m.folderDir(id)
	if _, err := os.Stat(dir); err != nil {
		return Job{}, nil, nil, errors.New("the disc's folder was not kept; read it from the disc first")
	}
	inv, err := m.deps.Store.Inventory(id)
	if err != nil {
		return Job{}, nil, nil, err
	}
	files := append([]store.FileEntry(nil), inv.Files...)
	sort.Slice(files, func(a, b int) bool { return files[a].Path < files[b].Path })
	kept := func(p string) string {
		if src, ok := safeJoin(dir, p); ok {
			if st, err := os.Stat(src); err == nil && st.Mode().IsRegular() {
				return src
			}
		}
		return ""
	}
	return j, files, kept, nil
}

// WriteDiscFolderNAS puts the folder under output.path/_thediscdb, with
// the streams as sparse placeholders that take no space. It is written
// under a hidden name and renamed when complete, so a half-written folder
// (minutes on a busy share) is never the one picked on thediscdb.com.
func (m *Manager) WriteDiscFolderNAS(id string) (string, error) {
	j, files, kept, err := m.folderFiles(id)
	if err != nil {
		return "", err
	}
	final := m.nasFolder(j)
	root := filepath.Join(filepath.Dir(final), ".writing-"+filepath.Base(final))
	_ = os.RemoveAll(root)
	for _, f := range files {
		dst, ok := safeJoin(root, f.Path)
		if !ok {
			continue
		}
		if err := mkdirAll(filepath.Dir(dst), 0o775); err != nil {
			return "", err
		}
		if src := kept(f.Path); src != "" {
			data, err := os.ReadFile(src)
			if err != nil {
				return "", err
			}
			if err := os.WriteFile(dst, data, 0o664); err != nil {
				return "", err
			}
		} else {
			out, err := os.Create(dst)
			if err != nil {
				return "", err
			}
			err = out.Truncate(f.Size) // a hole: no space on the share
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return "", err
			}
		}
		if !f.Modified.IsZero() {
			_ = os.Chtimes(dst, f.Modified, f.Modified)
		}
	}
	_ = os.RemoveAll(final)
	if err := os.Rename(root, final); err != nil {
		return "", err
	}
	return final, nil
}

// RemoveDiscFolderNAS deletes the copy on the share.
func (m *Manager) RemoveDiscFolderNAS(id string) error {
	j, ok := m.Record(id)
	if !ok {
		return errors.New("no such job")
	}
	return os.RemoveAll(m.nasFolder(j))
}

// WriteDiscFolderZip streams the folder as a zip. The placeholders are
// zeros, which compress to almost nothing but unzip to the disc's size.
func (m *Manager) WriteDiscFolderZip(w io.Writer, id string) error {
	j, files, kept, err := m.folderFiles(id)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed)
	})
	root := folderName(j)
	zeros := make([]byte, 1<<20)
	for _, f := range files {
		if _, ok := safeJoin("/", f.Path); !ok {
			continue
		}
		hdr := &zip.FileHeader{Name: root + "/" + f.Path, Method: zip.Deflate, Modified: f.Modified}
		if f.Modified.IsZero() {
			hdr.Modified = j.StartedAt
		}
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if src := kept(f.Path); src != "" {
			in, err := os.Open(src)
			if err != nil {
				return err
			}
			_, err = io.Copy(fw, in)
			in.Close()
			if err != nil {
				return err
			}
			continue
		}
		for left := f.Size; left > 0; {
			n := min(left, int64(len(zeros)))
			if _, err := fw.Write(zeros[:n]); err != nil {
				return err
			}
			left -= n
		}
	}
	return zw.Close()
}

// safeJoin joins a disc path under root, refusing anything that could
// leave it.
func safeJoin(root, p string) (string, bool) {
	parts := strings.Split(p, "/")
	for _, c := range parts {
		if c == "" || c == "." || c == ".." || strings.ContainsAny(c, "\\\x00") {
			return "", false
		}
	}
	return filepath.Join(append([]string{root}, parts...)...), true
}
