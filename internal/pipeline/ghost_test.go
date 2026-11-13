package pipeline

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sourcequality/media-ripper/internal/drive"
)

// hotDrive is a fake drive that can vanish, as a USB drive does on reset.
type hotDrive struct{ *fakeDrive }

func (d hotDrive) Present() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.gone
}

type hotplug struct {
	mu     sync.Mutex
	nodes  []string
	drives map[string]hotDrive
	ports  map[string]string
}

func (h *hotplug) add(node, port string) hotDrive {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := hotDrive{&fakeDrive{path: node, status: drive.NoDisc}}
	h.drives[node], h.ports[node] = d, port
	h.nodes = append(h.nodes, node)
	return d
}

func (h *hotplug) remove(node string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.drives[node].mu.Lock()
	h.drives[node].gone = true
	h.drives[node].mu.Unlock()
	var keep []string
	for _, n := range h.nodes {
		if n != node {
			keep = append(keep, n)
		}
	}
	h.nodes = keep
}

func hotplugEnv(t *testing.T) (*env, *hotplug) {
	e := setup(t, movieInfo, nil)
	h := &hotplug{drives: map[string]hotDrive{}, ports: map[string]string{}}
	e.m.deps.Discover = func() []string {
		h.mu.Lock()
		defer h.mu.Unlock()
		return append([]string(nil), h.nodes...)
	}
	e.m.deps.Port = func(p string) string {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.ports[p]
	}
	e.m.deps.OpenDrive = func(p string) (drive.Drive, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.drives[p], nil
	}
	e.cfg.Drives = nil
	e.m.SetConfig(e.cfg)
	return e, h
}

func (e *env) waitDrives(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		got = got[:0]
		for _, d := range e.m.Snapshot().Drives {
			got = append(got, d.Path)
		}
		sort.Strings(got)
		if strings.Join(got, ",") == strings.Join(want, ",") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("drives = %v, want %v", got, want)
}

// The drive resets while something holds /dev/sr0 open and comes back as
// /dev/sr1 on the same port: one drive, not a live one and a ghost.
func TestDriveBackUnderAnotherNameReplacesGhost(t *testing.T) {
	e, h := hotplugEnv(t)
	h.add("/dev/sr0", "usb2/2-1.3")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.waitDrives(t, "/dev/sr0")

	h.remove("/dev/sr0")
	h.add("/dev/sr1", "usb2/2-1.3")
	e.waitDrives(t, "/dev/sr1")

	// The name can be handed out again later, to whatever comes next.
	h.remove("/dev/sr1")
	h.add("/dev/sr0", "usb2/2-1.3")
	e.waitDrives(t, "/dev/sr0")
}

// A drive that goes away on its own port is not anyone's ghost: it stays
// listed (and is reported as lost) until it comes back.
func TestDriveOnAnotherPortIsNotAGhost(t *testing.T) {
	e, h := hotplugEnv(t)
	h.add("/dev/sr0", "usb2/2-1.3")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.m.Run(ctx)
	e.waitDrives(t, "/dev/sr0")

	h.remove("/dev/sr0")
	h.add("/dev/sr1", "usb2/2-1.4")
	e.waitDrives(t, "/dev/sr0", "/dev/sr1")
	time.Sleep(10 * hotplugEvery)
	e.waitDrives(t, "/dev/sr0", "/dev/sr1")
}
