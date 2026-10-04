package udf

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func sorted(fs []File) []File {
	out := append([]File(nil), fs...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// bdTree mimics a Blu-ray layout, with enough streams that STREAM spans
// several blocks and, in the 2.50 layout, crosses the metadata run boundary.
func bdTree() (*tnode, []File) {
	var want []File
	stream := dir("STREAM")
	for i := 0; i < 60; i++ {
		name := fmt.Sprintf("%05d.m2ts", i)
		size := int64(i)*6144000 + 6144
		stream.kids = append(stream.kids, file(name, size))
		want = append(want, File{Path: "BDMV/STREAM/" + name, Size: size})
	}
	stream.kids = append(stream.kids, file("00800.m2ts", 34359738368), file("00001.ssif", 1000))
	want = append(want, File{Path: "BDMV/STREAM/00800.m2ts", Size: 34359738368}, File{Path: "BDMV/STREAM/00001.ssif", Size: 1000})
	cert := dir("CERTIFICATE", &tnode{name: "Ünïcødé ☃.txt", size: 77, wide: true})
	want = append(want, File{Path: "CERTIFICATE/Ünïcødé ☃.txt", Size: 77})
	root := dir("",
		dir("BDMV",
			file("index.bdmv", 100),
			stream,
			dir("BACKUP", dir("PLAYLIST", file("00000.mpls", 300))),
			dir("CLIPINF"),
		),
		cert,
		&tnode{name: "Latin1-é", size: 5}, // 8-bit CS0 above ASCII
	)
	want = append(want,
		File{Path: "BDMV/index.bdmv", Size: 100},
		File{Path: "BDMV/BACKUP/PLAYLIST/00000.mpls", Size: 300},
		File{Path: "Latin1-é", Size: 5},
	)
	return root, sorted(want)
}

var buildCases = []struct {
	name string
	opts buildOpts
}{
	{"udf102-short", buildOpts{}},
	{"udf102-long", buildOpts{dirAD: adLong}},
	{"udf102-embedded", buildOpts{dirAD: adEmbedded}},
	{"udf102-aed", buildOpts{aed: true}},
	{"udf102-long-aed", buildOpts{dirAD: adLong, aed: true}},
	{"udf102-indirect", buildOpts{indirect: true}},
	{"udf102-reserve-vds", buildOpts{badMainVDS: true}},
	{"udf250-short", buildOpts{metadata: true}},
	{"udf250-long", buildOpts{metadata: true, dirAD: adLong}},
	{"udf250-embedded", buildOpts{metadata: true, dirAD: adEmbedded}},
	{"udf250-aed", buildOpts{metadata: true, aed: true}},
	{"udf250-mirror", buildOpts{metadata: true, badMainMD: true}},
}

func TestListBuilt(t *testing.T) {
	for _, tc := range buildCases {
		t.Run(tc.name, func(t *testing.T) {
			root, want := bdTree()
			img, _ := buildImage(root, tc.opts)
			got, err := List(bytes.NewReader(img))
			if err != nil {
				t.Fatal(err)
			}
			if got = sorted(got); !reflect.DeepEqual(untimed(got), want) {
				t.Fatalf("got %v\nwant %v", got, want)
			}
		})
	}
}

func TestListDirectoryCycle(t *testing.T) {
	root := dir("", dir("A", &tnode{name: "back", kids: []*tnode{}, loop: true}))
	img, _ := buildImage(root, buildOpts{})
	if _, err := List(bytes.NewReader(img)); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v, want directory cycle", err)
	}
}

func TestListDevice(t *testing.T) {
	root, want := bdTree()
	img, _ := buildImage(root, buildOpts{metadata: true})
	path := filepath.Join(t.TempDir(), "disc.img")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ListDevice(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(untimed(sorted(got)), want) {
		t.Fatalf("got %v", got)
	}
}

func gunzip(t testing.TB, name string) []byte {
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The fixtures were made by mkudffs and populated with sparse files through a
// Linux loop mount, so they check the reader against an independent writer.
func TestListFixtures(t *testing.T) {
	bd := []File{
		{Path: "BDMV/BACKUP/PLAYLIST/00000.mpls", Size: 300},
		{Path: "BDMV/PLAYLIST/00000.mpls", Size: 300},
		{Path: "BDMV/STREAM/00001.ssif", Size: 1000},
		{Path: "BDMV/STREAM/00800.m2ts", Size: 34359738368},
		{Path: "BDMV/index.bdmv", Size: 100},
		{Path: "CERTIFICATE/Ünïcødé ☃.txt", Size: 77},
	}
	for i := 0; i <= 40; i++ {
		bd = append(bd, File{Path: fmt.Sprintf("BDMV/STREAM/%05d.m2ts", i), Size: int64(i)*6144000 + 6144})
	}
	cases := []struct {
		file string
		want []File
		hash string
	}{
		{"dvd-udf102.img.gz", []File{
			{Path: "JACKET_P/J00___5L.MP2", Size: 4096},
			{Path: "VIDEO_TS/VIDEO_TS.BUP", Size: 12288},
			{Path: "VIDEO_TS/VIDEO_TS.IFO", Size: 12288},
			{Path: "VIDEO_TS/VIDEO_TS.VOB", Size: 180224},
			{Path: "VIDEO_TS/VTS_01_0.BUP", Size: 71680},
			{Path: "VIDEO_TS/VTS_01_0.IFO", Size: 71680},
			{Path: "VIDEO_TS/VTS_01_0.VOB", Size: 30720},
			{Path: "VIDEO_TS/VTS_01_1.VOB", Size: 1073709056},
			{Path: "VIDEO_TS/VTS_01_2.VOB", Size: 523866112},
		}, "C9FBE5EDC461F713B8C35D7410610F26"},
		// UDF 2.01 written by Linux: Extended File Entries, data in ICB.
		{"bd-udf201.img.gz", sorted(bd), "8A71758DC00CEAC071D0E1AE1EF683B3"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			got, err := List(bytes.NewReader(gunzip(t, tc.file)))
			if err != nil {
				t.Fatal(err)
			}
			if got = sorted(got); !reflect.DeepEqual(untimed(got), tc.want) {
				t.Fatalf("got %v\nwant %v", got, tc.want)
			}
			// mkudffs and the kernel record when each file was written.
			for _, f := range got {
				if f.Modified.Year() < 2020 {
					t.Fatalf("%s: modified %v", f.Path, f.Modified)
				}
			}
			// Expected hashes computed independently (Python struct + hashlib).
			h, err := ContentHash(got)
			if err != nil || h != tc.hash {
				t.Fatalf("ContentHash = %q, %v; want %q", h, err, tc.hash)
			}
		})
	}
}

func TestContentHash(t *testing.T) {
	// MD5(int64le(3) || int64le(1)): 00001 sorts before 00002 regardless of
	// case, and .ssif, BACKUP and non-stream files are ignored.
	files := []File{
		{Path: "BDMV/STREAM/00002.M2TS", Size: 1},
		{Path: "bdmv/stream/00001.m2ts", Size: 3},
		{Path: "BDMV/STREAM/00001.ssif", Size: 99},
		{Path: "BDMV/BACKUP/STREAM/00000.m2ts", Size: 99},
		{Path: "BDMV/index.bdmv", Size: 99},
	}
	h, err := ContentHash(files)
	if err != nil || h != "7CBAFD9B44746822AEDD9051BD398AEC" {
		t.Fatalf("ContentHash = %q, %v", h, err)
	}

	// A DVD hashes only VIDEO_TS, even if a BDMV directory is present:
	// MD5(int64le(3) || int64le(3) || int64le(1)).
	dvd := []File{{Path: "VIDEO_TS/VTS_01_0.IFO", Size: 3}, {Path: "VIDEO_TS/VIDEO_TS.ifo", Size: 3}, {Path: "VIDEO_TS/VTS_01_0.VOB", Size: 1},
		{Path: "VIDEO_TS/notes.txt", Size: 9}, {Path: "BDMV/STREAM/00001.m2ts", Size: 9}}
	if h, err := ContentHash(dvd); err != nil || h != "BDBFCB611D5945E1698F49B3D54458C7" {
		t.Fatalf("DVD hash = %q, %v", h, err)
	}

	if _, err := ContentHash([]File{{Path: "BDMV/index.bdmv", Size: 1}}); err == nil {
		t.Fatal("expected an error with no hashable files")
	}
	if _, err := ContentHash(nil); err == nil {
		t.Fatal("expected an error for an empty list")
	}
}

// Every truncation that cuts into used structures must fail cleanly.
func TestListTruncated(t *testing.T) {
	for _, tc := range buildCases {
		root, _ := bdTree()
		img, maxSec := buildImage(root, tc.opts)
		for n := 0; n <= maxSec*sectorSize; n += 1531 {
			if _, err := List(bytes.NewReader(img[:n])); err == nil {
				t.Fatalf("%s: truncated to %d bytes: no error", tc.name, n)
			}
		}
	}
}

// Random corruption must never panic or hang; errors are fine.
func TestListCorrupted(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, tc := range buildCases {
		root, _ := bdTree()
		orig, maxSec := buildImage(root, tc.opts)
		for i := 0; i < 150; i++ {
			img := append([]byte(nil), orig[:(maxSec+1)*sectorSize]...)
			for j := 0; j < 1+rng.Intn(20); j++ {
				img[rng.Intn(len(img))] = byte(rng.Intn(256))
			}
			// Hitting only the metadata-bearing sectors makes damage likelier
			// to reach the parser.
			for j := 0; j < rng.Intn(4); j++ {
				s := 256 + rng.Intn(maxSec-255)
				img[s*sectorSize+rng.Intn(sectorSize)] = byte(rng.Intn(256))
			}
			_, _ = List(bytes.NewReader(img))
		}
	}
	if _, err := List(bytes.NewReader(nil)); err == nil {
		t.Fatal("empty input: no error")
	}
	garbage := make([]byte, 600*sectorSize)
	rng.Read(garbage)
	if _, err := List(bytes.NewReader(garbage)); err == nil {
		t.Fatal("garbage input: no error")
	}
}

// FuzzList overwrites bytes of a valid 2.50 image: whole images are too large
// for the fuzzer to mutate usefully.
func FuzzList(f *testing.F) {
	root, _ := bdTree()
	base, maxSec := buildImage(root, buildOpts{metadata: true, dirAD: adEmbedded})
	base = base[:(maxSec+1)*sectorSize]
	f.Add(uint32(256*sectorSize), []byte{0})
	f.Add(uint32((partStart+metaRuns[0].start)*sectorSize+400), []byte{0xff, 0xff})
	f.Add(uint32(mainVDS*sectorSize+440), []byte{2, 64})
	f.Fuzz(func(t *testing.T, off uint32, patch []byte) {
		img := append([]byte(nil), base...)
		copy(img[int(off)%len(img):], patch)
		_, _ = List(bytes.NewReader(img))
	})
}

// untimed drops the timestamps, which depend on when an image was made.
func untimed(files []File) []File {
	out := make([]File, len(files))
	for i, f := range files {
		out[i] = File{Path: f.Path, Size: f.Size}
	}
	return out
}

// Read returns the contents of the files asked for, in the same pass,
// within the byte limit.
func TestReadContents(t *testing.T) {
	img := gunzip(t, "bd-udf201.img.gz")
	files, data, err := Read(bytes.NewReader(img), func(f File) bool { return !strings.HasSuffix(f.Path, ".m2ts") }, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 47 {
		t.Fatalf("listed %d files", len(files))
	}
	got := map[string]int{}
	for p, b := range data {
		got[p] = len(b)
	}
	want := map[string]int{"BDMV/index.bdmv": 100, "BDMV/PLAYLIST/00000.mpls": 300, "BDMV/BACKUP/PLAYLIST/00000.mpls": 300, "BDMV/STREAM/00001.ssif": 1000, "CERTIFICATE/Ünïcødé ☃.txt": 77}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("read %v, want %v", got, want)
	}
	// The limit is respected: nothing fits in 50 bytes.
	if _, data, _ := Read(bytes.NewReader(img), func(File) bool { return true }, 50); len(data) != 0 {
		t.Fatalf("read past the limit: %d files", len(data))
	}
}
