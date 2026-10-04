// Package notify sends short completion and failure notices.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Notifier posts events to the configured endpoints. Both are optional.
type Notifier struct {
	WebhookURL string
	NtfyURL    string
	NtfyToken  string
	Discord    *Discord
	Client     *http.Client
	Logger     *slog.Logger
}

// Event is what gets sent.
type Event struct {
	Type  string `json:"type"` // started | ready | done | failed | skipped | review | storage | test
	JobID string `json:"job_id,omitempty"`
	Drive string `json:"drive"`
	// DriveName is the drive model, e.g. "HL-DT-ST BD-RE BU40N".
	DriveName string `json:"drive_name,omitempty"`
	// Match says how the disc was identified, e.g. "TheDiscDB: …".
	Match string `json:"match,omitempty"`
	// Items are the titles being ripped, e.g. "S01E08 Time Enough at Last".
	Items []string `json:"items,omitempty"`
	// Summary is the overall state for progress updates ("Ripping 3 of 8").
	Summary string `json:"summary,omitempty"`
	// Final marks the last progress update of a job: buttons go away.
	Final    bool      `json:"final,omitempty"`
	Warnings []string  `json:"warnings,omitempty"`
	Label    string    `json:"label,omitempty"`
	Title    string    `json:"title,omitempty"`
	Outputs  []string  `json:"outputs,omitempty"`
	Error    string    `json:"error,omitempty"`
	Elapsed  string    `json:"elapsed,omitempty"`
	Time     time.Time `json:"time"`
}

// Send delivers the event; failures are logged, never fatal.
func (n *Notifier) Send(ctx context.Context, ev Event) {
	_ = n.send(ctx, ev)
}

// Test sends a test event and reports the first failure, for the
// settings page.
func (n *Notifier) Test(ctx context.Context) error {
	if n == nil || (n.WebhookURL == "" && n.NtfyURL == "" && !n.Discord.enabled()) {
		return errors.New("no notification target is configured")
	}
	return n.send(ctx, Event{Type: "test", Title: "Test"})
}

func (n *Notifier) send(ctx context.Context, ev Event) error {
	if n == nil || (n.WebhookURL == "" && n.NtfyURL == "" && !n.Discord.enabled()) {
		return nil
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var errs []error
	report := func(target string, err error) {
		if err == nil {
			return
		}
		errs = append(errs, fmt.Errorf("%s: %w", target, err))
		if n.Logger != nil {
			n.Logger.Warn("notify failed", "target", target, "err", err)
		}
	}
	if ev.Type == "progress" {
		// Progress only updates Discord's live message in place; for the
		// webhook and ntfy it would be a flood.
		if n.Discord.enabled() {
			report("discord", n.Discord.send(ctx, client, ev))
		}
		return errors.Join(errs...)
	}
	if n.WebhookURL != "" {
		body, _ := json.Marshal(ev)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.WebhookURL, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			err = n.do(client, req)
		}
		report("webhook", err)
	}
	if n.NtfyURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.NtfyURL, strings.NewReader(ntfyBody(ev)))
		if err == nil {
			req.Header.Set("Title", ntfyTitle(ev))
			req.Header.Set("Tags", ntfyTag(ev))
			if ev.Type == "failed" {
				req.Header.Set("Priority", "high")
			}
			if n.NtfyToken != "" {
				req.Header.Set("Authorization", "Bearer "+n.NtfyToken)
			}
			err = n.do(client, req)
		}
		report("ntfy", err)
	}
	if n.Discord.enabled() {
		report("discord", n.Discord.send(ctx, client, ev))
	}
	return errors.Join(errs...)
}

func (n *Notifier) do(client *http.Client, req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("rejected: %s", resp.Status)
	}
	return nil
}

func ntfyTitle(ev Event) string {
	name := ev.Title
	if name == "" {
		name = ev.Label
	}
	switch ev.Type {
	case "done":
		return "Ripped: " + name
	case "failed":
		return "Failed: " + name
	case "started":
		return "Ripping: " + name
	case "skipped":
		return "Skipped: " + name
	case "ready":
		return "Ready for the next disc"
	case "review":
		return "Waiting for review: " + name
	case "storage":
		if ev.Match == "recovered" {
			return "Library storage is back"
		}
		return "Library storage is " + ev.Match
	case "test":
		return "media-ripper test"
	}
	return name
}

func ntfyBody(ev Event) string {
	switch ev.Type {
	case "done":
		if len(ev.Outputs) == 1 {
			return ev.Outputs[0]
		}
		return fmt.Sprintf("%d files, %s", len(ev.Outputs), ev.Elapsed)
	case "failed":
		return ev.Error
	}
	return ev.Drive
}

func ntfyTag(ev Event) string {
	switch ev.Type {
	case "done":
		return "white_check_mark"
	case "failed":
		return "x"
	case "started":
		return "cd"
	}
	return "information_source"
}
