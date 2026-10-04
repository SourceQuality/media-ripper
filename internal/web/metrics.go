package web

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/sourcequality/media-ripper/internal/pipeline"
)

// metrics serves Prometheus text format, written by hand to keep the
// binary free of dependencies.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	gauge := func(name, help, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	q := func(v string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v) }

	gauge("media_ripper_build_info", "Version of the running media-ripper.", "gauge")
	fmt.Fprintf(&b, "media_ripper_build_info{version=\"%s\"} 1\n", q(s.Version))

	st := s.Manager.Stats()
	gauge("media_ripper_discs_total", "Discs finished, by final stage.", "counter")
	stages := make([]string, 0, len(st.Discs))
	for k := range st.Discs {
		stages = append(stages, string(k))
	}
	sort.Strings(stages)
	for _, k := range stages {
		fmt.Fprintf(&b, "media_ripper_discs_total{stage=\"%s\"} %d\n", q(k), st.Discs[stageOf(k)])
	}
	gauge("media_ripper_titles_delivered_total", "Titles delivered to the library or staging.", "counter")
	fmt.Fprintf(&b, "media_ripper_titles_delivered_total %d\n", st.Titles)
	gauge("media_ripper_titles_imported_total", "Titles Radarr or Sonarr imported.", "counter")
	fmt.Fprintf(&b, "media_ripper_titles_imported_total %d\n", st.Imported)
	gauge("media_ripper_delivered_bytes_total", "Bytes delivered.", "counter")
	fmt.Fprintf(&b, "media_ripper_delivered_bytes_total %d\n", st.Bytes)
	gauge("media_ripper_delivered_video_seconds_total", "Running time of the titles delivered.", "counter")
	fmt.Fprintf(&b, "media_ripper_delivered_video_seconds_total %.0f\n", st.VideoSeconds)
	gauge("media_ripper_discs_verified_total", "Discs verified by TheDiscDB or confirmed by a person.", "counter")
	fmt.Fprintf(&b, "media_ripper_discs_verified_total %d\n", st.Verified)
	gauge("media_ripper_reviews_pending", "Discs waiting for a person before import.", "gauge")
	fmt.Fprintf(&b, "media_ripper_reviews_pending %d\n", st.ReviewPending)

	snap := s.Manager.Snapshot()
	gauge("media_ripper_drive_busy", "1 while a drive has a job running.", "gauge")
	gauge("media_ripper_activity_bytes_per_second", "Current speed of each running rip, remux or copy.", "gauge")
	for _, d := range snap.Drives {
		busy := 0
		if d.Job != nil && !d.Job.Stage.Terminal() {
			busy = 1
			for _, a := range d.Job.Activities {
				fmt.Fprintf(&b, "media_ripper_activity_bytes_per_second{drive=\"%s\",kind=\"%s\"} %.0f\n", q(d.Path), q(a.Kind), a.Speed)
			}
		}
		fmt.Fprintf(&b, "media_ripper_drive_busy{drive=\"%s\",model=\"%s\",status=\"%s\"} %d\n", q(d.Path), q(d.Model), q(d.Status), busy)
	}
	if st := snap.Storage; st != nil {
		ok := 0
		if st.OK() {
			ok = 1
		}
		gauge("media_ripper_storage_up", "1 when the library folder answers.", "gauge")
		fmt.Fprintf(&b, "media_ripper_storage_up{path=\"%s\",state=\"%s\"} %d\n", q(st.Path), q(st.State), ok)
		gauge("media_ripper_storage_probe_seconds", "Time to write and remove a test file in the library.", "gauge")
		fmt.Fprintf(&b, "media_ripper_storage_probe_seconds{path=\"%s\"} %.3f\n", q(st.Path), float64(st.LatencyMS)/1000)
		gauge("media_ripper_storage_free_bytes", "Free space in the library.", "gauge")
		fmt.Fprintf(&b, "media_ripper_storage_free_bytes{path=\"%s\"} %d\n", q(st.Path), st.FreeBytes)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Manager.Stats())
}

func stageOf(s string) pipeline.Stage { return pipeline.Stage(s) }
