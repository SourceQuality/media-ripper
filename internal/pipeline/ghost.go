package pipeline

import "time"

// A USB drive that resets is usually back within seconds, but if something
// (MakeMKV, mid-read) still holds its old node open, the kernel gives it the
// next free one: /dev/sr0 comes back as /dev/sr1. Discovery starts a runner
// for the new node; the old one, watching a node that will not return, is a
// ghost and is retired once it is idle.

// hotplugEvery is how often drives are discovered when none are configured.
var hotplugEvery = 15 * time.Second

// present reports whether a runner's device node exists.
func (r *runner) present() bool {
	r.mu.Lock()
	d := r.drv
	r.mu.Unlock()
	p, ok := d.(presence)
	return !ok || p.Present()
}

// idle reports whether a runner has no job under way: none, finished, or one
// paused until its disc is back, which the drive's new runner takes up.
func (r *runner) idle() bool {
	job := r.current()
	if job == nil {
		return true
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	return job.Stage.Terminal() || job.retryDisc
}

// servedElsewhere reports whether another runner watches a present drive on
// this runner's port.
func (m *Manager) servedElsewhere(r *runner) bool {
	if r.port == "" {
		return false
	}
	m.mu.Lock()
	others := append([]*runner(nil), m.runners...)
	m.mu.Unlock()
	for _, o := range others {
		if o != r && o.port == r.port && o.present() {
			return true
		}
	}
	return false
}

// ghostRunners are the idle runners whose drive is gone and back under
// another name.
func (m *Manager) ghostRunners() []*runner {
	m.mu.Lock()
	all := append([]*runner(nil), m.runners...)
	m.mu.Unlock()
	var out []*runner
	for _, r := range all {
		if r.stop != nil && !r.present() && r.idle() && m.servedElsewhere(r) {
			out = append(out, r)
		}
	}
	return out
}

func (m *Manager) dropRunner(r *runner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.runners[:0]
	for _, o := range m.runners {
		if o != r {
			out = append(out, o)
		}
	}
	m.runners = out
}
