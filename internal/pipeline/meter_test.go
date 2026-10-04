package pipeline

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestMeterSpeedAndETA(t *testing.T) {
	now := time.Unix(0, 0)
	m := &meter{now: func() time.Time { return now }}
	const mb = 1_000_000
	const total = 100 * mb

	if s, e := m.add(0, total); s != 0 || e != 0 {
		t.Fatalf("first reading: speed=%v eta=%v", s, e)
	}
	// 20 MB/s for 5 s.
	for i := 1; i <= 5; i++ {
		now = now.Add(time.Second)
		m.add(int64(i)*20*mb, total)
	}
	s, e := m.add(100*mb, total)
	if s < 19*mb || s > 21*mb {
		t.Fatalf("speed = %.1f MB/s, want ~20", s/mb)
	}
	if e != 0 {
		t.Fatalf("eta at 100%% = %v", e)
	}

	// Halfway at a steady 20 MB/s: 50 MB left is 2.5 s.
	m.reset()
	now = now.Add(time.Second)
	m.add(0, total)
	now = now.Add(time.Second)
	m.add(20*mb, total)
	now = now.Add(time.Second)
	if _, e := m.add(40*mb, total); e < 2*time.Second || e > 4*time.Second {
		t.Fatalf("eta = %v, want ~3s", e)
	}
}

// The speed follows a change within the window instead of averaging the
// whole run, and a reading that goes backwards starts over.
func TestMeterWindowAndRestart(t *testing.T) {
	now := time.Unix(0, 0)
	m := &meter{now: func() time.Time { return now }}
	const mb = 1_000_000
	var done int64
	for i := 0; i < 30; i++ { // 30 s at 50 MB/s
		now = now.Add(time.Second)
		done += 50 * mb
		m.add(done, 0)
	}
	var s float64
	for i := 0; i < 15; i++ { // then 15 s at 10 MB/s
		now = now.Add(time.Second)
		done += 10 * mb
		s, _ = m.add(done, 0)
	}
	if s < 9*mb || s > 11*mb {
		t.Fatalf("speed after slowdown = %.1f MB/s, want ~10", s/mb)
	}
	now = now.Add(time.Second)
	if s, _ := m.add(1*mb, 0); s != 0 {
		t.Fatalf("speed right after restart = %.1f, want 0", s/mb)
	}
}

// copyCtx reports a running total and the job keeps one entry per kind,
// dropped when the step ends.
func TestCopyProgressAndActivities(t *testing.T) {
	src := strings.NewReader(strings.Repeat("x", 12<<20))
	var seen []int64
	var dst bytes.Buffer
	n, err := copyCtx(context.Background(), &dst, slowReader{src}, func(done int64) { seen = append(seen, done) })
	if err != nil || n != 12<<20 {
		t.Fatalf("copied %d: %v", n, err)
	}
	if len(seen) < 2 || seen[len(seen)-1] <= seen[0] {
		t.Fatalf("progress readings = %v", seen)
	}

	j := newJob("/dev/fake0", nil)
	var m1, m2 meter
	j.track(&m1, "rip", "S01E02", 10, 100)
	j.track(&m2, "copy", "S01E01", 50, 200)
	j.track(&m1, "rip", "S01E02", 40, 100)
	s := j.Snapshot()
	if len(s.Activities) != 2 || s.Activities[0].Kind != "rip" || s.Activities[0].Percent != 40 || s.Activities[1].Item != "S01E01" {
		t.Fatalf("activities = %+v", s.Activities)
	}
	j.untrack("rip")
	if s := j.Snapshot(); len(s.Activities) != 1 || s.Activities[0].Kind != "copy" {
		t.Fatalf("after untrack = %+v", s.Activities)
	}
	j.setStage(StageDone, "done")
	if s := j.Snapshot(); len(s.Activities) != 0 {
		t.Fatalf("activities kept after the job ended: %+v", s.Activities)
	}
}

// slowReader returns small chunks with a pause so progress fires more than
// once.
type slowReader struct{ r io.Reader }

func (s slowReader) Read(p []byte) (int, error) {
	time.Sleep(100 * time.Millisecond)
	if len(p) > 1<<20 {
		p = p[:1<<20]
	}
	return s.r.Read(p)
}
