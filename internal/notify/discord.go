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

	apiBase string // overridden in tests
}

func (d *Discord) enabled() bool {
	return d != nil && ((d.BotToken != "" && d.ChannelID != "") || d.WebhookURL != "")
}

func (d *Discord) send(ctx context.Context, client *http.Client, ev Event) error {
	payload := map[string]any{"embeds": []any{discordEmbed(ev)}, "allowed_mentions": map[string]any{"parse": []string{}}}
	var req *http.Request
	var err error
	if d.BotToken != "" && d.ChannelID != "" {
		base := d.apiBase
		if base == "" {
			base = "https://discord.com/api/v10"
		}
		body, _ := json.Marshal(payload)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, base+"/channels/"+url.PathEscape(d.ChannelID)+"/messages", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Authorization", "Bot "+d.BotToken)
		}
	} else {
		payload["username"] = "media-ripper"
		body, _ := json.Marshal(payload)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, d.WebhookURL, bytes.NewReader(body))
	}
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "media-ripper (https://github.com/sourcequality/media-ripper)")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("discord: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
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
