package makemkv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sampleInfo = `MSG:1005,0,1,"MakeMKV v1.17.7 linux(x64-release) started","%1 started","MakeMKV v1.17.7 linux(x64-release)"
MSG:3007,0,0,"Using direct disc access mode","Using direct disc access mode"
DRV:0,2,999,12,"BD-RE HL-DT-ST BD-RE  WH16NS40 1.05","THE_MATRIX","/dev/sr0"
TCOUNT:3
CINFO:1,6209,"Blu-ray disc"
CINFO:2,0,"The Matrix"
CINFO:28,0,"eng"
CINFO:32,0,"THE_MATRIX"
TINFO:0,2,0,"The Matrix"
TINFO:0,8,0,"24"
TINFO:0,9,0,"2:16:18"
TINFO:0,10,0,"32.5 GB"
TINFO:0,11,0,"34900000000"
TINFO:0,16,0,"00800.mpls"
TINFO:0,25,0,"3"
TINFO:0,26,0,"1,2,3"
TINFO:0,27,0,"The_Matrix_t00.mkv"
SINFO:0,0,1,6201,"Video"
SINFO:0,0,5,0,"V_MPEG4/ISO/AVC"
SINFO:0,0,19,0,"1920x1080"
SINFO:0,1,1,6202,"Audio"
SINFO:0,1,3,0,"eng"
SINFO:0,1,4,0,"English"
SINFO:0,1,6,0,"DTS-HD MA"
SINFO:0,1,14,0,"6"
SINFO:0,2,1,6203,"Subtitles"
SINFO:0,2,3,0,"eng"
TINFO:1,8,0,"2"
TINFO:1,9,0,"2:16:18"
TINFO:1,16,0,"00801.mpls"
TINFO:1,26,0,"3,1,2"
TINFO:1,27,0,"The_Matrix_t01.mkv"
TINFO:2,8,0,"1"
TINFO:2,9,0,"0:02:11"
TINFO:2,16,0,"00002.mpls"
TINFO:2,26,0,"50"
TINFO:2,27,0,"The_Matrix_t02.mkv"
`

func TestParseInfo(t *testing.T) {
	d := parseInfo(strings.Split(sampleInfo, "\n"))
	if d.Type != "Blu-ray disc" || !d.IsBluray() {
		t.Fatalf("type = %q", d.Type)
	}
	if d.Name != "The Matrix" || d.Volume != "THE_MATRIX" || d.Label() != "The Matrix" {
		t.Fatalf("name/volume = %q/%q", d.Name, d.Volume)
	}
	if d.TitleCount != 3 || len(d.Titles) != 3 {
		t.Fatalf("titles = %d/%d", d.TitleCount, len(d.Titles))
	}
	t0 := d.Titles[0]
	if t0.Duration != 2*time.Hour+16*time.Minute+18*time.Second {
		t.Fatalf("duration = %s", t0.Duration)
	}
	if t0.Chapters != 24 || t0.SizeBytes != 34900000000 || t0.SourceFile != "00800.mpls" || t0.OutputFile != "The_Matrix_t00.mkv" {
		t.Fatalf("title 0 = %+v", *t0)
	}
	if len(t0.Segments) != 3 || t0.Segments[2] != 3 {
		t.Fatalf("segments = %v", t0.Segments)
	}
	if len(t0.Streams) != 3 || t0.Streams[1].Lang != "eng" || t0.Streams[1].Channels != 6 || t0.Video().VideoSize != "1920x1080" {
		t.Fatalf("streams = %+v", t0.Streams)
	}
	if t0.AudioCount() != 1 || t0.SubtitleCount() != 1 {
		t.Fatalf("counts = %d/%d", t0.AudioCount(), t0.SubtitleCount())
	}
	if d.Titles[2].Duration != 2*time.Minute+11*time.Second {
		t.Fatalf("title 2 duration = %s", d.Titles[2].Duration)
	}
}

func TestSplitRobot(t *testing.T) {
	got := splitRobot(`5004,0,2,"Copy complete. 1 titles saved, 0 failed.","Copy complete. %1 titles saved, %2 failed.","1","0"`)
	want := []string{"5004", "0", "2", "Copy complete. 1 titles saved, 0 failed.", "Copy complete. %1 titles saved, %2 failed.", "1", "0"}
	if len(got) != len(want) {
		t.Fatalf("got %d fields: %q", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("field %d = %q want %q", i, got[i], want[i])
		}
	}
	got = splitRobot(`1,0,"He said \"hi\", ok"`)
	if got[2] != `He said "hi", ok` {
		t.Fatalf("escaped quote: %q", got[2])
	}
}

func TestParseSegments(t *testing.T) {
	cases := map[string][]int{
		"1,2,3":     {1, 2, 3},
		"(1,2),3":   {1, 2, 3},
		"100-103":   {100, 101, 102, 103},
		"":          nil,
		"7, 8 ,(9)": {7, 8, 9},
	}
	for in, want := range cases {
		got := parseSegments(in)
		if len(got) != len(want) {
			t.Fatalf("%q: got %v want %v", in, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%q: got %v want %v", in, got, want)
			}
		}
	}
}

func TestSelectionString(t *testing.T) {
	if s := SelectionString(nil); !strings.HasPrefix(s, "+sel:all") {
		t.Fatalf("all: %s", s)
	}
	if s := SelectionString([]string{"eng", "jpn"}); !strings.Contains(s, "lang=eng|lang=jpn|nolang") || !strings.Contains(s, "+sel:video") {
		t.Fatalf("langs: %s", s)
	}
}

func TestWriteSettings(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSettings(dir, "KEY-123", "+sel:all"); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettings(dir, "", ""); err != nil {
		t.Fatal(err)
	}
	data, _ := readFile(t, dir+"/settings.conf")
	if !strings.Contains(data, `app_Key = "KEY-123"`) || !strings.Contains(data, `app_DefaultSelectionString = "+sel:all"`) {
		t.Fatalf("settings:\n%s", data)
	}
}

func readFile(t *testing.T, p string) (string, error) {
	t.Helper()
	b, err := osReadFile(p)
	return string(b), err
}

func TestProbe(t *testing.T) {
	dir := t.TempDir()
	write := func(name, out string) *Client {
		bin := filepath.Join(dir, name)
		script := "#!/bin/sh\ncat <<'OUT'\n" + out + "\nOUT\nexit 1\n"
		if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return &Client{Binary: bin}
	}
	ok := write("ok", `MSG:1005,0,1,"MakeMKV v2.0.0 linux(x64-release) started","%1 started","MakeMKV v2.0.0 linux(x64-release)"
MSG:5010,0,0,"Failed to open disc","Failed to open disc"`)
	if v, err := ok.Probe(context.Background()); err != nil || v != "2.0.0" {
		t.Fatalf("ok: %q %v", v, err)
	}
	// What a test's dummy key in ~/.MakeMKV did to the real install.
	expired := write("expired", `MSG:1005,0,1,"MakeMKV v1.18.3 linux(x64-release) started","%1 started","MakeMKV v1.18.3 linux(x64-release)"
MSG:5020,516,0,"The stored activation key is invalid.","The stored activation key is invalid."
MSG:5021,131332,1,"This application version is too old.","This application version is too old.","http://www.makemkv.com/"`)
	v, err := expired.Probe(context.Background())
	if v != "1.18.3" || err == nil || !strings.Contains(err.Error(), "too old") || !strings.Contains(err.Error(), "makemkv.key") {
		t.Fatalf("expired: %q %v", v, err)
	}
	if _, err := (&Client{Binary: filepath.Join(dir, "missing")}).Probe(context.Background()); err == nil {
		t.Fatal("missing binary should fail")
	}
}
