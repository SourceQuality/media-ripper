package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultDiscordApp is the media-ripper Discord application. Its invite
// link adds the bot to a server; posting as it needs its bot token.
const DefaultDiscordApp = "1556123331047202997"

// discordPermissions is View Channel (1<<10) + Send Messages (1<<11) +
// Embed Links (1<<14): all the bot needs to post its embeds.
const discordPermissions = 1<<10 | 1<<11 | 1<<14

// DiscordInviteURL is the link that adds application appID to a server
// with only the permissions media-ripper uses.
func DiscordInviteURL(appID string) string {
	if appID == "" {
		appID = DefaultDiscordApp
	}
	q := url.Values{"client_id": {appID}, "scope": {"bot"}, "permissions": {fmt.Sprint(discordPermissions)}}
	return "https://discord.com/oauth2/authorize?" + q.Encode()
}

// Discord posts events to a channel, as a bot (token + channel) or through
// a channel webhook. Either is enough; with both, the bot is used.
type Discord struct {
	BotToken   string
	ChannelID  string
	WebhookURL string
	// Buttons adds actions (approve, discard, cancel, eject) to bot
	// messages; a running discordbot.Bot handles the presses.
	Buttons bool

	mu       sync.Mutex
	messages map[string]string // job id -> its live "Ripping" message

	apiBase string // overridden in tests
}

func (d *Discord) enabled() bool {
	return d != nil && ((d.BotToken != "" && d.ChannelID != "") || d.WebhookURL != "")
}

func (d *Discord) send(ctx context.Context, client *http.Client, ev Event) error {
	payload := map[string]any{"embeds": []any{discordEmbed(ev)}, "allowed_mentions": map[string]any{"parse": []string{}}}
	bot := d.BotToken != "" && d.ChannelID != ""
	if bot && d.Buttons && !ev.Final {
		if c := components(ev); c != nil {
			payload["components"] = c
		}
	}
	// A disc's "Ripping" message is kept up to date in place.
	if ev.Type == "progress" {
		id := d.live(ev.JobID, "", ev.Final)
		if id == "" {
			return nil
		}
		if ev.Final {
			payload["components"] = []any{}
		}
		_, err := d.request(ctx, client, http.MethodPatch, id, payload)
		return err
	}
	id, err := d.request(ctx, client, http.MethodPost, "", payload)
	if err == nil && ev.Type == "started" && ev.JobID != "" && id != "" {
		d.live(ev.JobID, id, false)
	}
	return err
}

// live remembers (id set) or looks up a job's live message; final forgets
// it after this last edit.
func (d *Discord) live(jobID, id string, final bool) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.messages == nil {
		d.messages = map[string]string{}
	}
	if id != "" {
		d.messages[jobID] = id
		return id
	}
	got := d.messages[jobID]
	if final {
		delete(d.messages, jobID)
	}
	return got
}

// request posts a new message (messageID empty) or edits one, as the bot
// or through the webhook, and returns the message id.
func (d *Discord) request(ctx context.Context, client *http.Client, method, messageID string, payload map[string]any) (string, error) {
	var target string
	bot := d.BotToken != "" && d.ChannelID != ""
	if bot {
		base := d.apiBase
		if base == "" {
			base = "https://discord.com/api/v10"
		}
		target = base + "/channels/" + url.PathEscape(d.ChannelID) + "/messages"
		if messageID != "" {
			target += "/" + url.PathEscape(messageID)
		}
	} else {
		u, err := url.Parse(d.WebhookURL)
		if err != nil {
			return "", err
		}
		if messageID != "" {
			u.Path = strings.TrimSuffix(u.Path, "/") + "/messages/" + url.PathEscape(messageID)
		} else {
			payload["username"] = "media-ripper"
			q := u.Query()
			q.Set("wait", "true") // so Discord returns the message and its id
			u.RawQuery = q.Encode()
		}
		target = u.String()
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	if bot {
		req.Header.Set("Authorization", "Bot "+d.BotToken)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "media-ripper (https://github.com/sourcequality/media-ripper)")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("discord: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var msg struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(data, &msg)
	return msg.ID, nil
}

// Embed colours.
const (
	colorBlue   = 0x4f63ff
	colorGreen  = 0x15803d
	colorAmber  = 0xb45309
	colorRed    = 0xb91c1c
	colorGrey   = 0x6b7280
	maxEmbedTxt = 1000 // Discord caps a field value at 1024 characters
)

func discordEmbed(ev Event) map[string]any {
	name := ev.Title
	if name == "" {
		name = ev.Label
	}
	if name == "" {
		name = "Disc"
	}
	e := map[string]any{}
	var fields []map[string]any
	field := func(n, v string, inline bool) {
		if v != "" {
			fields = append(fields, map[string]any{"name": n, "value": clip(v), "inline": inline})
		}
	}
	switch ev.Type {
	case "started":
		e["title"], e["color"] = "Ripping: "+name, colorBlue
		e["description"] = ev.Match
		field(itemsHeading(ev.Items), strings.Join(ev.Items, "\n"), false)
	case "progress":
		e["title"], e["color"] = "Ripping: "+name, colorBlue
		if ev.Final {
			e["title"], e["color"] = name, colorGrey
		}
		desc := ev.Match
		if ev.Summary != "" {
			desc = strings.TrimSpace(desc + "\n**" + ev.Summary + "**")
		}
		e["description"] = desc
		field(itemsHeading(ev.Items), strings.Join(ev.Items, "\n"), false)
	case "ready":
		e["title"], e["color"] = "Ready for the next disc", colorGreen
		e["description"] = fmt.Sprintf("**%s** is ripped and the tray is open. Copying and import carry on in the background.", name)
	case "done":
		e["title"] = "Done: " + name
		e["color"] = colorGreen
		if len(ev.Warnings) > 0 {
			e["color"] = colorAmber
		}
		e["description"] = fmt.Sprintf("%d file(s) delivered in %s.", len(ev.Outputs), ev.Elapsed)
		field("Warnings", strings.Join(ev.Warnings, "\n"), false)
	case "review":
		e["title"], e["color"] = "Waiting for review: "+name, colorAmber
		e["description"] = ev.Match + "\nThe files are in staging. Approve or correct the titles in the web UI's Review section; nothing is imported until then."
		field(itemsHeading(ev.Items), strings.Join(ev.Items, "\n"), false)
	case "failed":
		e["title"], e["color"] = "Failed: "+name, colorRed
		e["description"] = ev.Error
	case "skipped":
		e["title"], e["color"] = "Skipped: "+name, colorGrey
		e["description"] = ev.Error
	case "storage":
		if ev.Match == "recovered" {
			e["title"], e["color"] = "Library storage is back", colorGreen
			e["description"] = fmt.Sprintf("`%s` answers again; delivery carries on.", ev.Title)
		} else {
			e["title"], e["color"] = "Library storage is "+ev.Match, colorRed
			e["description"] = fmt.Sprintf("`%s`: %s. Ripping continues; delivery waits for the share.", ev.Title, ev.Error)
		}
	case "test":
		e["title"], e["color"] = "media-ripper is connected", colorBlue
		e["description"] = "Notifications for this ripper will be posted here."
	default:
		e["title"], e["color"] = name, colorGrey
	}
	field("Drive", ev.DriveName, true)
	if ev.Label != "" && ev.Label != name {
		field("Disc label", "`"+ev.Label+"`", true)
	}
	if len(fields) > 0 {
		e["fields"] = fields
	}
	if !ev.Time.IsZero() {
		e["timestamp"] = ev.Time.UTC().Format(time.RFC3339)
	}
	return e
}

func itemsHeading(items []string) string {
	if len(items) == 1 {
		return "Title"
	}
	return fmt.Sprintf("Titles (%d)", len(items))
}

func clip(s string) string {
	if len(s) <= maxEmbedTxt {
		return s
	}
	cut := strings.LastIndex(s[:maxEmbedTxt], "\n")
	if cut < 0 {
		cut = maxEmbedTxt
	}
	return s[:cut] + "\n…"
}

// components are the buttons for an event. Their custom ids are read by
// the bot: "mr:<action>:<job id or drive>".
func components(ev Event) []any {
	button := func(style int, label, id string) map[string]any {
		return map[string]any{"type": 2, "style": style, "label": label, "custom_id": id}
	}
	var row []any
	switch {
	case ev.Type == "review" && ev.JobID != "":
		row = []any{button(3, "Approve & import", "mr:approve:"+ev.JobID), button(4, "Discard", "mr:discard:"+ev.JobID)}
	case ev.Type == "started" && ev.JobID != "":
		row = []any{button(2, "Cancel", "mr:cancel:"+ev.JobID)}
	case ev.Type == "failed" && ev.Drive != "":
		row = []any{button(2, "Eject", "mr:eject:"+strings.TrimPrefix(ev.Drive, "/dev/"))}
	}
	if row == nil {
		return nil
	}
	return []any{map[string]any{"type": 1, "components": row}}
}
