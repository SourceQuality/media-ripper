package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type captured struct {
	mu   sync.Mutex
	reqs []*http.Request
	body []map[string]any
}

func discordServer(t *testing.T, status int) (*httptest.Server, *captured) {
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		c.mu.Lock()
		c.reqs = append(c.reqs, r)
		c.body = append(c.body, m)
		c.mu.Unlock()
		w.WriteHeader(status)
		if status >= 300 {
			_, _ = w.Write([]byte(`{"message": "Missing Access", "code": 50001}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func TestDiscordBotPostsEmbed(t *testing.T) {
	srv, got := discordServer(t, 200)
	n := &Notifier{Discord: &Discord{BotToken: "tok", ChannelID: "123", apiBase: srv.URL}}
	n.Send(context.Background(), Event{Type: "started", Title: "The Twilight Zone S01 D2", Label: "TWILIGHT_ZONE_SEASON1_DISC2",
		DriveName: "HL-DT-ST BD-RE BU40N", Match: "TheDiscDB: Season 1 Disc 2",
		Items: []string{"S01E08 Time Enough at Last", "S01E09 Perchance to Dream"}, Time: time.Unix(1700000000, 0)})
	if len(got.reqs) != 1 {
		t.Fatalf("requests = %d", len(got.reqs))
	}
	r := got.reqs[0]
	if r.URL.Path != "/channels/123/messages" || r.Header.Get("Authorization") != "Bot tok" {
		t.Fatalf("request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
	}
	embed := got.body[0]["embeds"].([]any)[0].(map[string]any)
	if embed["title"] != "Ripping: The Twilight Zone S01 D2" || embed["description"] != "TheDiscDB: Season 1 Disc 2" {
		t.Fatalf("embed = %v", embed)
	}
	fields := embed["fields"].([]any)
	first := fields[0].(map[string]any)
	if first["name"] != "Titles (2)" || !strings.Contains(first["value"].(string), "Perchance to Dream") {
		t.Fatalf("fields = %v", fields)
	}
	// Disc labels and titles must never ping anyone.
	if am := got.body[0]["allowed_mentions"].(map[string]any); len(am["parse"].([]any)) != 0 {
		t.Fatalf("allowed_mentions = %v", am)
	}
}

func TestDiscordWebhookAndTest(t *testing.T) {
	srv, got := discordServer(t, 204)
	n := &Notifier{Discord: &Discord{WebhookURL: srv.URL + "/api/webhooks/1/abc"}}
	if err := n.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.reqs[0].URL.Path != "/api/webhooks/1/abc" || got.body[0]["username"] != "media-ripper" {
		t.Fatalf("webhook request %s %v", got.reqs[0].URL.Path, got.body[0])
	}
	n.Send(context.Background(), Event{Type: "ready", Title: "The Twilight Zone S01 D2"})
	if title := got.body[1]["embeds"].([]any)[0].(map[string]any)["title"]; title != "Ready for the next disc" {
		t.Fatalf("ready title = %v", title)
	}
}

func TestDiscordErrorsReachTheTestButton(t *testing.T) {
	srv, _ := discordServer(t, 403)
	n := &Notifier{Discord: &Discord{BotToken: "tok", ChannelID: "123", apiBase: srv.URL}}
	if err := n.Test(context.Background()); err == nil || !strings.Contains(err.Error(), "Missing Access") {
		t.Fatalf("err = %v", err)
	}
	if err := (&Notifier{}).Test(context.Background()); err == nil {
		t.Fatal("test with nothing configured should say so")
	}
}

func TestDiscordInviteURL(t *testing.T) {
	u, err := url.Parse(DiscordInviteURL(""))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "discord.com" || q.Get("client_id") != DefaultDiscordApp || q.Get("scope") != "bot" || q.Get("permissions") != "19456" {
		t.Fatalf("invite = %s", u)
	}
	if q := mustQuery(t, DiscordInviteURL("42")); q.Get("client_id") != "42" {
		t.Fatalf("custom app = %v", q)
	}
}

func mustQuery(t *testing.T, s string) url.Values {
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestClipLongLists(t *testing.T) {
	var items []string
	for i := 0; i < 200; i++ {
		items = append(items, "S01E99 A rather long episode title for testing")
	}
	if got := clip(strings.Join(items, "\n")); len(got) > 1024 || !strings.HasSuffix(got, "…") {
		t.Fatalf("clip len %d", len(got))
	}
}
