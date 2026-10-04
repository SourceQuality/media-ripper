package pipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/metadata"
	"github.com/sourcequality/media-ripper/internal/udf"
)

var discDate = time.Date(2016, 10, 3, 9, 30, 0, 0, time.UTC)

// fakeDisc is what the UDF reader returns for The Matrix in these tests.
func fakeDisc(e *env) (reads *int) {
	reads = new(int)
	files := []udf.File{
		{Path: "dummy_f.dat", Size: 1, Modified: time.Date(1754, 3, 1, 0, 0, 0, 0, time.UTC)}, // as on The Thing
		{Path: "AACS/Unit_Key_RO.inf", Size: 5, Modified: discDate},
		{Path: "BDMV/index.bdmv", Size: 4, Modified: discDate},
		{Path: "BDMV/PLAYLIST/00800.mpls", Size: 3, Modified: discDate},
		{Path: "BDMV/STREAM/00001.m2ts", Size: 3 << 20, Modified: discDate},
	}
	contents := map[string]string{"AACS/Unit_Key_RO.inf": "aacsk", "BDMV/index.bdmv": "INDX", "BDMV/PLAYLIST/00800.mpls": "MPL"}
	e.m.deps.DiscFiles = func(string) ([]udf.File, error) { return files, nil }
	e.m.deps.ReadDisc = func(_ string, want func(udf.File) bool, limit int64) ([]udf.File, map[string][]byte, error) {
		*reads++
		data := map[string][]byte{}
		for _, f := range files {
			if want(f) && f.Size <= limit {
				data[f.Path] = []byte(contents[f.Path])
			}
		}
		return files, data, nil
	}
	return reads
}

func matrixEnv(t *testing.T) *env {
	return setup(t, movieInfo, fakeProvider{&metadata.Identity{Kind: metadata.KindMovie, Title: "The Matrix", Year: 1999, TMDBID: 603, Runtime: 136 * time.Minute, Confidence: 1, Source: "test"}})
}

// A disc TheDiscDB does not know keeps its small files after the rip, and
// the folder thediscdb.com asks for can be had on the share or as a zip.
func TestDiscFolderKeptAndExported(t *testing.T) {
	e := matrixEnv(t)
	reads := fakeDisc(e)
	argsLog := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_ARGS", argsLog)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitDone(t)
	if *reads != 1 {
		t.Fatalf("disc read %d times for its folder", *reads)
	}
	st, err := e.m.DiscFolderStatus(j.ID)
	if err != nil || !st.Kept || !st.Log || st.Files != 5 || st.Name != "THE_MATRIX" {
		t.Fatalf("status = %+v, %v", st, err)
	}
	// The MakeMKV log is TheDiscDB's own command: every title, as printed.
	if log, err := e.m.ScanLog(j.ID); err != nil || string(log) != movieInfo && string(log) != movieInfo+"\n" {
		t.Fatalf("log = %q, %v", log, err)
	}
	if args, _ := os.ReadFile(argsLog); !strings.Contains(string(args), "--minlength=0 --robot info dev:/dev/fake0") {
		t.Fatalf("makemkvcon runs:\n%s", args)
	}

	// On the share: real small files, the stream as a hole of its size.
	root, err := e.m.WriteDiscFolderNAS(j.ID)
	if err != nil || root != filepath.Join(e.out, "_thediscdb", "THE_MATRIX") {
		t.Fatalf("nas folder %q: %v", root, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "AACS", "Unit_Key_RO.inf")); string(b) != "aacsk" {
		t.Fatalf("AACS key file = %q", b)
	}
	if dummy, _ := os.Stat(filepath.Join(root, "dummy_f.dat")); dummy == nil || !dummy.ModTime().Equal(discDate) {
		t.Fatalf("a 1754 date must become the disc's latest: %v", dummy)
	}
	m2ts, err := os.Stat(filepath.Join(root, "BDMV", "STREAM", "00001.m2ts"))
	if err != nil || m2ts.Size() != 3<<20 || !m2ts.ModTime().Equal(discDate) {
		t.Fatalf("placeholder: %v %v", m2ts, err)
	}
	if blocks := m2ts.Sys().(*syscall.Stat_t).Blocks; blocks*512 >= 3<<20 {
		t.Fatalf("placeholder takes %d bytes on disk", blocks*512)
	}
	if st, _ := e.m.DiscFolderStatus(j.ID); st.NAS != root {
		t.Fatalf("status NAS = %q", st.NAS)
	}
	if left, _ := filepath.Glob(filepath.Join(e.out, "_thediscdb", ".writing-*")); len(left) != 0 {
		t.Fatalf("temporary folder left: %v", left)
	}
	// Written again over an existing copy.
	if again, err := e.m.WriteDiscFolderNAS(j.ID); err != nil || again != root {
		t.Fatalf("rewrite: %q %v", again, err)
	}
	if err := e.m.RemoveDiscFolderNAS(j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("NAS copy not removed")
	}

	// As a zip: the same layout under the disc's name, dates kept.
	var buf bytes.Buffer
	if err := e.m.WriteDiscFolderZip(&buf, j.ID); err != nil {
		t.Fatal(err)
	}
	if buf.Len() > 64<<10 {
		t.Fatalf("zip is %d bytes; the zeros should compress away", buf.Len())
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		if !f.Modified.Equal(discDate) {
			t.Fatalf("%s modified %v", f.Name, f.Modified)
		}
		if strings.HasSuffix(f.Name, ".m2ts") {
			got[f.Name] = "placeholder"
			if len(data) != 3<<20 || bytes.Count(data, []byte{0}) != len(data) {
				t.Fatalf("placeholder in zip: %d bytes", len(data))
			}
			continue
		}
		got[f.Name] = string(data)
	}
	if got["THE_MATRIX/AACS/Unit_Key_RO.inf"] != "aacsk" || got["THE_MATRIX/BDMV/PLAYLIST/00800.mpls"] != "MPL" || len(got) != 5 {
		t.Fatalf("zip entries = %v", got)
	}
}

// A disc ripped without its folder kept can be put back in to read it:
// it is read and ejected, not ripped again.
func TestDiscFolderReadOnReinsert(t *testing.T) {
	e := matrixEnv(t)
	e.cfg.Metadata.TheDiscDB.DiscFolder = "off"
	e.m.SetConfig(e.cfg)
	reads := fakeDisc(e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.drv.insert("fp-matrix", "THE_MATRIX")
	j := e.waitDone(t)
	e.waitEject(t, 1)
	if *reads != 0 {
		t.Fatal("folder read with disc_folder off")
	}
	if st, _ := e.m.DiscFolderStatus(j.ID); st.Kept || st.Log {
		t.Fatal("folder or log kept with disc_folder off")
	}
	// A waiting request can be cancelled.
	if err := e.m.RequestDiscFolder(j.ID); err != nil {
		t.Fatal(err)
	}
	e.m.CancelDiscFolder(j.ID)
	if st, _ := e.m.DiscFolderStatus(j.ID); st.Waiting {
		t.Fatalf("cancelled request still waiting: %+v", st)
	}
	if err := e.m.RequestDiscFolder(j.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.m.DiscFolderStatus(j.ID); !st.Waiting {
		t.Fatalf("status = %+v", st)
	}
	// Hold the read so its progress can be seen.
	read, release := e.m.deps.ReadDisc, make(chan struct{})
	e.m.deps.ReadDisc = func(dev string, want func(udf.File) bool, limit int64) ([]udf.File, map[string][]byte, error) {
		<-release
		return read(dev, want, limit)
	}
	e.drv.insert("fp-matrix", "THE_MATRIX")
	deadline := time.Now().Add(5 * time.Second)
	for st, _ := e.m.DiscFolderStatus(j.ID); !st.Reading; st, _ = e.m.DiscFolderStatus(j.ID) {
		if time.Now().After(deadline) {
			t.Fatalf("never shown as reading: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Clicking again while it reads does not queue another read.
	_ = e.m.RequestDiscFolder(j.ID)
	if st, _ := e.m.DiscFolderStatus(j.ID); st.Waiting || !st.Reading {
		t.Fatalf("second click while reading: %+v", st)
	}
	close(release)
	e.waitEject(t, 2)
	if st, _ := e.m.DiscFolderStatus(j.ID); !st.Kept || !st.Log || st.Waiting || st.Reading {
		t.Fatalf("after reinsert: %+v", st)
	}
	// Nothing left to read: a request is a no-op.
	_ = e.m.RequestDiscFolder(j.ID)
	if st, _ := e.m.DiscFolderStatus(j.ID); st.Waiting {
		t.Fatalf("request with everything kept: %+v", st)
	}
	if n := len(e.m.Snapshot().Recent); n != 1 {
		t.Fatalf("%d jobs: the disc was ripped again", n)
	}
}

func TestWantFolderAndSafeJoin(t *testing.T) {
	if !wantFolder(Job{}, "unmatched") || wantFolder(Job{HashMatched: true}, "unmatched") || wantFolder(Job{}, "off") || !wantFolder(Job{HashMatched: true}, "always") {
		t.Fatal("wantFolder")
	}
	for _, bad := range []string{"../x", "a/../../x", "/abs", "a//b", "a\\b"} {
		if _, ok := safeJoin("/root", bad); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
	if p, ok := safeJoin("/root", "BDMV/index.bdmv"); !ok || p != "/root/BDMV/index.bdmv" {
		t.Fatalf("%q %v", p, ok)
	}
}
