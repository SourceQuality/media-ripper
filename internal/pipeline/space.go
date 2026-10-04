package pipeline

import (
	"fmt"
	"syscall"

	"github.com/sourcequality/media-ripper/internal/selector"
)

// spaceFunc reports the bytes an unprivileged user may still write on the
// filesystem holding path, and the filesystem's device id.
type spaceFunc func(path string) (free int64, dev uint64, err error)

// diskSpace is the real spaceFunc.
func diskSpace(path string) (free int64, dev uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	var s syscall.Stat_t
	if err := syscall.Stat(path, &s); err != nil {
		return 0, 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), uint64(s.Dev), nil
}

// spaceMargin covers container overhead and remux temp files.
const spaceMargin = 1.05

// checkSpace decides, before anything is read from the disc, whether the
// workspace and the library can take the titles still to rip and deliver.
// It returns an error to stop the job and a warning to log and carry on:
// titles are delivered while later ones rip, which frees the workspace as
// it goes, so only room for the largest title is strictly needed there.
// backupBytes is the size of a full-disc copy, written to the library.
func checkSpace(workDir, library string, sel *selector.Selection, rs *resume, backupBytes int64, diskSpace spaceFunc) (warning string, err error) {
	var toRip, largest, toDeliver int64
	toDeliver = int64(float64(backupBytes) * spaceMargin)
	for _, p := range sel.Picks {
		size := int64(float64(p.Title.SizeBytes) * spaceMargin)
		if _, ok := rs.delivered(p.Title.ID); ok {
			continue
		}
		toDeliver += size
		if _, ok := rs.ripped(p.Title.ID); ok {
			continue
		}
		toRip += size
		if size > largest {
			largest = size
		}
	}
	if toDeliver == 0 {
		return "", nil
	}
	wsFree, wsDev, err := diskSpace(workDir)
	if err != nil {
		return "", nil // cannot tell; the rip itself will fail loudly if full
	}
	if wsFree < largest {
		return "", fmt.Errorf("workspace %s has %s free; the largest title needs %s", workDir, gb(wsFree), gb(largest))
	}
	if wsFree < toRip {
		warning = fmt.Sprintf("workspace has %s free for %s of titles; relying on delivery freeing space while ripping", gb(wsFree), gb(toRip))
	}
	libFree, libDev, err := diskSpace(library)
	if err != nil || (libDev == wsDev && backupBytes == 0) {
		// Same filesystem: delivery renames, the workspace check covers it.
		return warning, nil
	}
	if libFree < toDeliver {
		return "", fmt.Errorf("library %s has %s free; this disc needs %s", library, gb(libFree), gb(toDeliver))
	}
	return warning, nil
}

func gb(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/1e9) }
