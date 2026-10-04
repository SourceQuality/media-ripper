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
	if u := DiscordInviteURL(""); u != "" {
		t.Fatalf("invite without an application = %q", u)
	}
	u, err := url.Parse(DiscordInviteURL("42"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "discord.com" || q.Get("client_id") != "42" || q.Get("scope") != "bot" || q.Get("permissions") != "19456" {
		t.Fatalf("invite = %s", u)
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

func TestButtonsOnBotMessages(t *testing.T) {
	srv, got := discordServer(t, 200)
	n := &Notifier{Discord: &Discord{BotToken: "tok", ChannelID: "1", Buttons: true, apiBase: srv.URL}}
	n.Send(context.Background(), Event{Type: "review", JobID: "20261004-010000-001", Title: "The Twilight Zone S01 D3"})
	n.Send(context.Background(), Event{Type: "started", JobID: "j2", Title: "x"})
	n.Send(context.Background(), Event{Type: "failed", Drive: "/dev/sr0", Title: "x"})
	n.Send(context.Background(), Event{Type: "done", JobID: "j3", Title: "x"})
	n.Send(context.Background(), Event{Type: "done", JobID: "j4", Title: "x", ImportFailed: true})
	ids := func(i int) []string {
		var out []string
		rows, _ := got.body[i]["components"].([]any)
		for _, r := range rows {
			for _, c := range r.(map[string]any)["components"].([]any) {
				out = append(out, c.(map[string]any)["custom_id"].(string))
			}
		}
		return out
	}
	if g := strings.Join(ids(0), ","); g != "mr:approve:20261004-010000-001,mr:discard:20261004-010000-001" {
		t.Fatalf("review buttons = %s", g)
	}
	if g := strings.Join(ids(1), ","); g != "mr:cancel:j2" {
		t.Fatalf("started buttons = %s", g)
	}
	if g := strings.Join(ids(2), ","); g != "mr:eject:sr0" {
		t.Fatalf("failed buttons = %s", g)
	}
	if len(ids(3)) != 0 {
		t.Fatal("done messages have no buttons")
	}
	if g := strings.Join(ids(4), ","); g != "mr:retry:j4" {
		t.Fatalf("failed import buttons = %s", g)
	}
	// A webhook message cannot carry working buttons.
	hook, hookGot := discordServer(t, 204)
	(&Notifier{Discord: &Discord{WebhookURL: hook.URL, Buttons: true}}).Send(context.Background(), Event{Type: "review", JobID: "x", Title: "x"})
	if _, ok := hookGot.body[0]["components"]; ok {
		t.Fatal("webhook message with buttons")
	}
}

// The "Ripping" message is edited in place with progress, and the last
// edit removes the buttons; the bot and the webhook both work.
func TestLiveProgressMessage(t *testing.T) {
	for _, mode := range []string{"bot", "webhook"} {
		var mu sync.Mutex
		var reqs []string
		var bodies []map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			data, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(data, &m)
			mu.Lock()
			reqs = append(reqs, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
			bodies = append(bodies, m)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"msg42"}`))
		}))
		d := &Discord{Buttons: true, apiBase: srv.URL}
		if mode == "bot" {
			d.BotToken, d.ChannelID = "tok", "chan"
		} else {
			d.WebhookURL = srv.URL + "/api/webhooks/1/abc"
		}
		n := &Notifier{Discord: d}
		ctx := context.Background()
		n.Send(ctx, Event{Type: "progress", JobID: "j1", Items: []string{"x"}}) // nothing to edit yet
		n.Send(ctx, Event{Type: "started", JobID: "j1", Title: "The Twilight Zone S01 D3"})
		n.Send(ctx, Event{Type: "progress", JobID: "j1", Title: "The Twilight Zone S01 D3", Summary: "Ripping 2 of 7 · 1 delivered",
			Items: []string{"✅ S01E16 The Hitch-Hiker · delivered", "▶️ S01E17 The Fever · ripping 40% · 20.1 MB/s · 3m0s left"}})
		n.Send(ctx, Event{Type: "progress", JobID: "j1", Final: true, Summary: "Done"})
		n.Send(ctx, Event{Type: "progress", JobID: "j1", Summary: "after the end"}) // forgotten: no edit
		srv.Close()

		wantEdit := "PATCH /channels/chan/messages/msg42?"
		wantPost := "POST /channels/chan/messages?"
		if mode == "webhook" {
			wantEdit, wantPost = "PATCH /api/webhooks/1/abc/messages/msg42?", "POST /api/webhooks/1/abc?wait=true"
		}
		if len(reqs) != 3 || reqs[0] != wantPost || reqs[1] != wantEdit || reqs[2] != wantEdit {
			t.Fatalf("%s: requests = %v", mode, reqs)
		}
		embed := bodies[1]["embeds"].([]any)[0].(map[string]any)
		desc, _ := embed["description"].(string)
		field := embed["fields"].([]any)[0].(map[string]any)
		if !strings.Contains(desc, "Ripping 2 of 7") || !strings.Contains(field["value"].(string), "ripping 40%") {
			t.Fatalf("%s: progress embed = %v", mode, embed)
		}
		if mode == "bot" {
			if c, _ := bodies[2]["components"].([]any); c == nil || len(c) != 0 {
				t.Fatalf("final edit keeps buttons: %v", bodies[2]["components"])
			}
		}
	}
}
