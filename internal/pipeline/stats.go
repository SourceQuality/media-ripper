package pipeline

import (
	"encoding/json"
	"time"
)

// Stats summarises everything ripped so far, from the saved history.
type Stats struct {
	Discs         map[Stage]int `json:"discs"`         // by final stage
	Titles        int           `json:"titles"`        // titles delivered
	Bytes         int64         `json:"bytes"`         // bytes delivered
	VideoSeconds  float64       `json:"video_seconds"` // running time delivered
	BusySeconds   float64       `json:"busy_seconds"`  // job time of finished discs
	Imported      int           `json:"imported"`      // titles Radarr/Sonarr took
	Verified      int           `json:"verified"`      // discs verified by TheDiscDB or a person
	FirstAt       time.Time     `json:"first_at,omitempty"`
	ReviewPending int           `json:"review_pending"`
}

// Stats reads the history. It is cheap enough to compute per request at
// the scale of a home disc collection.
func (m *Manager) Stats() Stats {
	st := Stats{Discs: map[Stage]int{}}
	hist, _ := m.deps.Store.History(0)
	for _, raw := range hist {
		var j Job
		if json.Unmarshal(raw, &j) != nil {
			continue
		}
		st.Discs[j.Stage]++
		if st.FirstAt.IsZero() || (!j.StartedAt.IsZero() && j.StartedAt.Before(st.FirstAt)) {
			st.FirstAt = j.StartedAt
		}
		if j.Stage != StageDone && j.Stage != StageReview {
			continue
		}
		if d, err := time.ParseDuration(j.Elapsed); err == nil {
			st.BusySeconds += d.Seconds()
		}
		if j.HashMatched || (j.CatalogMatched > 0 && j.CatalogMatched == j.CatalogCompared) || (j.Identity != nil && j.Identity.Source == sourceManual) {
			st.Verified++
		}
		for _, o := range j.Outputs {
			st.Titles++
			st.Bytes += o.Size
			st.VideoSeconds += o.Duration.Seconds()
			if o.Import == "imported" {
				st.Imported++
			}
		}
	}
	st.ReviewPending = len(m.deps.Store.Reviews())
	return st
}
