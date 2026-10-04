package pipeline

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/sourcequality/media-ripper/internal/notify"
)

// StorageStatus is the health of the library folder, which is often a
// network share: delivery stalls quietly when it goes away or crawls.
type StorageStatus struct {
	Path      string    `json:"path"`
	State     string    `json:"state"` // ok | slow | unreachable | error
	LatencyMS int64     `json:"latency_ms"`
	FreeBytes int64     `json:"free_bytes,omitempty"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// OK reports whether delivery can be expected to work.
func (s StorageStatus) OK() bool { return s.State == "ok" || s.State == "slow" }

var (
	storageInterval = time.Minute
	storageTimeout  = 20 * time.Second
	storageSlow     = 5 * time.Second
)

// storageMonitor probes the library folder by writing and removing a small
// file. A probe stuck on an unresponsive NFS server is never stacked: the
// next round reports it as unreachable until it returns.
type storageMonitor struct {
	mu       sync.Mutex
	status   StorageStatus
	inFlight bool
}

func (s *storageMonitor) get() StorageStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (m *Manager) watchStorage(ctx context.Context) {
	t := time.NewTicker(storageInterval)
	defer t.Stop()
	for {
		m.checkStorage(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) checkStorage(ctx context.Context) {
	cfg := m.Config()
	path := cfg.Output.Path
	st := &m.storage
	st.mu.Lock()
	if st.inFlight {
		st.mu.Unlock()
		m.setStorage(StorageStatus{Path: path, State: "unreachable", Error: fmt.Sprintf("no answer for over %s", storageInterval), CheckedAt: time.Now()})
		return
	}
	st.inFlight = true
	st.mu.Unlock()

	done := make(chan StorageStatus, 1)
	go func() {
		res := probeStorage(path)
		st.mu.Lock()
		st.inFlight = false
		st.mu.Unlock()
		done <- res
	}()
	select {
	case res := <-done:
		m.setStorage(res)
	case <-time.After(storageTimeout):
		m.setStorage(StorageStatus{Path: path, State: "unreachable", Error: fmt.Sprintf("no answer within %s", storageTimeout), CheckedAt: time.Now()})
	case <-ctx.Done():
	}
}

func probeStorage(path string) StorageStatus {
	start := time.Now()
	res := StorageStatus{Path: path, CheckedAt: start}
	err := func() error {
		if err := os.MkdirAll(path, 0o775); err != nil {
			return err
		}
		f, err := os.CreateTemp(path, ".media-ripper-probe-*")
		if err != nil {
			return err
		}
		name := f.Name()
		_, werr := f.Write([]byte("ok"))
		serr := f.Sync()
		cerr := f.Close()
		rerr := os.Remove(name)
		for _, e := range []error{werr, serr, cerr, rerr} {
			if e != nil {
				return e
			}
		}
		return nil
	}()
	res.LatencyMS = time.Since(start).Milliseconds()
	switch {
	case err != nil:
		res.State, res.Error = "error", err.Error()
	case time.Since(start) > storageSlow:
		res.State = "slow"
	default:
		res.State = "ok"
	}
	if free, _, err := diskSpace(path); err == nil {
		res.FreeBytes = free
	}
	return res
}

// setStorage records a probe result and announces changes between usable
// and unusable, so a share that drops out mid-evening is noticed.
func (m *Manager) setStorage(next StorageStatus) {
	st := &m.storage
	st.mu.Lock()
	prev := st.status
	st.status = next
	st.mu.Unlock()
	if prev.State == "" {
		if !next.OK() {
			m.log.Warn("library storage", "path", next.Path, "state", next.State, "err", next.Error)
		}
		return
	}
	if prev.OK() == next.OK() {
		return
	}
	ev := notify.Event{Type: "storage", Title: next.Path, Error: next.Error}
	if next.OK() {
		m.log.Info("library storage recovered", "path", next.Path, "latency_ms", next.LatencyMS)
		ev.Match = "recovered"
	} else {
		m.log.Warn("library storage", "path", next.Path, "state", next.State, "err", next.Error)
		ev.Match = next.State
	}
	m.rt.Load().notifier.Send(context.Background(), ev)
}
