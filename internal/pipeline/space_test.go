package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sourcequality/media-ripper/internal/makemkv"
	"github.com/sourcequality/media-ripper/internal/notify"
	"github.com/sourcequality/media-ripper/internal/selector"
)

func TestCheckSpace(t *testing.T) {
	type fs struct {
		free int64
		dev  uint64
	}
	const gb = int64(1e9)
	sel := &selector.Selection{Picks: []selector.Pick{
		{Title: &makemkv.Title{ID: 1, SizeBytes: 5 * gb}},
		{Title: &makemkv.Title{ID: 2, SizeBytes: 5 * gb}},
		{Title: &makemkv.Title{ID: 3, SizeBytes: 30 * gb}},
	}}
	cases := []struct {
		name      string
		ws, lib   fs
		wantErr   string
		wantWarn  bool
		delivered bool
	}{
		{"plenty", fs{500 * gb, 1}, fs{5000 * gb, 2}, "", false, false},
		{"workspace cannot hold the largest title", fs{20 * gb, 1}, fs{5000 * gb, 2}, "workspace", false, false},
		{"room for the largest only", fs{35 * gb, 1}, fs{5000 * gb, 2}, "", true, false},
		{"library too small", fs{500 * gb, 1}, fs{10 * gb, 2}, "library", false, false},
		{"library on the same filesystem is not counted twice", fs{500 * gb, 1}, fs{10 * gb, 1}, "", false, false},
		{"titles already delivered need nothing", fs{1 * gb, 1}, fs{1 * gb, 2}, "", false, true},
	}
	for _, c := range cases {
		space := func(path string) (int64, uint64, error) {
			if path == "/lib" {
				return c.lib.free, c.lib.dev, nil
			}
			return c.ws.free, c.ws.dev, nil
		}
		dir := t.TempDir()
		rs := openResume(dir, &Job{Fingerprint: "fp"}, true)
		if c.delivered {
			for _, p := range sel.Picks {
				f := filepath.Join(dir, "out.mkv")
				_ = os.WriteFile(f, []byte("x"), 0o644)
				_ = rs.markDelivered(p.Title.ID, Output{Path: f, Size: 1})
			}
		}
		warn, err := checkSpace(dir, "/lib", sel, rs, 0, space)
		switch {
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.wantErr)
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected err %v", c.name, err)
		case (warn != "") != c.wantWarn:
			t.Errorf("%s: warning = %q", c.name, warn)
		}
	}
}

func TestStorageMonitor(t *testing.T) {
	if st := probeStorage(t.TempDir()); st.State != "ok" || st.FreeBytes <= 0 {
		t.Fatalf("writable dir: %+v", st)
	}
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o644)
	if st := probeStorage(filepath.Join(file, "lib")); st.State != "error" || st.Error == "" {
		t.Fatalf("path under a file: %+v", st)
	}

	// Changes between usable and unusable are announced once each.
	var mu sync.Mutex
	var events []notify.Event
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev notify.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}))
	defer hook.Close()
	e := setup(t, movieInfo, nil)
	e.m.deps.Notifier = &notify.Notifier{WebhookURL: hook.URL}
	e.m.SetConfig(e.cfg)
	for _, state := range []string{"ok", "slow", "unreachable", "unreachable", "ok"} {
		e.m.setStorage(StorageStatus{Path: "/mnt/Media/Rips", State: state, Error: "timeout"})
	}
	mu.Lock()
	got := append([]notify.Event(nil), events...)
	mu.Unlock()
	if len(got) != 2 || got[0].Match != "unreachable" || got[1].Match != "recovered" {
		t.Fatalf("events = %+v", got)
	}

	// A probe still stuck from the last round is reported, not stacked.
	e.m.storage.inFlight = true
	e.m.checkStorage(context.Background())
	if st := e.m.storage.get(); st.State != "unreachable" {
		t.Fatalf("stuck probe: %+v", st)
	}
}

func TestCheckSpaceCountsTheBackup(t *testing.T) {
	space := func(path string) (int64, uint64, error) {
		if path == "/lib" {
			return 40e9, 2, nil
		}
		return 500e9, 1, nil
	}
	sel := &selector.Selection{Picks: []selector.Pick{{Title: &makemkv.Title{ID: 1, SizeBytes: 5e9}}}}
	rs := openResume(t.TempDir(), &Job{Fingerprint: "fp"}, true)
	if _, err := checkSpace(t.TempDir(), "/lib", sel, rs, 0, space); err != nil {
		t.Fatalf("titles alone fit: %v", err)
	}
	if _, err := checkSpace(t.TempDir(), "/lib", sel, rs, 45e9, space); err == nil || !strings.Contains(err.Error(), "library") {
		t.Fatalf("a 45 GB backup does not fit in 40 GB: %v", err)
	}
}
