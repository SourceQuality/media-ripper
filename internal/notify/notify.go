// Package notify sends short completion and failure notices.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
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
	Client     *http.Client
	Logger     *slog.Logger
}

// Event is what gets sent.
type Event struct {
	Type    string    `json:"type"` // started | done | failed | skipped
	Drive   string    `json:"drive"`
	Label   string    `json:"label,omitempty"`
	Title   string    `json:"title,omitempty"`
	Outputs []string  `json:"outputs,omitempty"`
	Error   string    `json:"error,omitempty"`
	Elapsed string    `json:"elapsed,omitempty"`
	Time    time.Time `json:"time"`
}

// Send delivers the event; failures are logged, never fatal.
func (n *Notifier) Send(ctx context.Context, ev Event) {
	if n == nil || (n.WebhookURL == "" && n.NtfyURL == "") {
		return
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
	if n.WebhookURL != "" {
		body, _ := json.Marshal(ev)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.WebhookURL, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			n.do(client, req, "webhook")
		}
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
			n.do(client, req, "ntfy")
		}
	}
}

func (n *Notifier) do(client *http.Client, req *http.Request, what string) {
	resp, err := client.Do(req)
	if err != nil {
		if n.Logger != nil {
			n.Logger.Warn("notify failed", "target", what, "err", err)
		}
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 && n.Logger != nil {
		n.Logger.Warn("notify rejected", "target", what, "status", resp.StatusCode)
	}
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
