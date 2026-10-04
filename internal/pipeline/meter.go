package pipeline

import (
	"sync"
	"time"
)

// speedWindow is how far back the meter looks when computing a speed. Long
// enough to smooth out NFS and MakeMKV bursts, short enough to follow a
// real change (a slower disc layer, a busy network).
const speedWindow = 10 * time.Second

// meter turns a stream of "bytes done so far" readings into a speed and an
// ETA. The zero value is ready to use.
type meter struct {
	mu      sync.Mutex
	samples []sample
	now     func() time.Time // for tests
}

type sample struct {
	at   time.Time
	done int64
}

func (m *meter) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// add records a reading and returns the speed in bytes per second over the
// window (0 until two readings are far enough apart) and the time left to
// reach total (0 when unknown).
func (m *meter) add(done, total int64) (speed float64, eta time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	// A reading that goes backwards is a new phase or a restarted copy.
	if n := len(m.samples); n > 0 && done < m.samples[n-1].done {
		m.samples = m.samples[:0]
	}
	m.samples = append(m.samples, sample{at: now, done: done})
	cut := 0
	for cut < len(m.samples)-2 && now.Sub(m.samples[cut+1].at) >= speedWindow {
		cut++
	}
	m.samples = m.samples[cut:]
	first := m.samples[0]
	dt := now.Sub(first.at).Seconds()
	if dt < 1 {
		return 0, 0
	}
	speed = float64(done-first.done) / dt
	if speed > 0 && total > done {
		eta = time.Duration(float64(total-done) / speed * float64(time.Second)).Round(time.Second)
	}
	return speed, eta
}

// reset forgets earlier readings, for a new phase of the same step.
func (m *meter) reset() {
	m.mu.Lock()
	m.samples = m.samples[:0]
	m.mu.Unlock()
}
