package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
	// The tray opening gets a message of its own, so it notifies.
	n.Send(context.Background(), Event{Type: "ready", Title: "The Twilight Zone S01 D2", Summary: "**The Twilight Zone S01 D2** is finished and the tray is open."})
	if len(got.body) != 2 || got.reqs[1].Method != http.MethodPost {
		t.Fatalf("ready messages: %v", got.body[1:])
	}
	embed := got.body[1]["embeds"].([]any)[0].(map[string]any)
	if embed["title"] != "Ready for the next disc" || !strings.Contains(embed["description"].(string), "is finished") {
		t.Fatalf("ready embed = %v", embed)
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
	n.Send(context.Background(), Event{Type: "failed", JobID: "j5", Drive: "/dev/sr0", Title: "x", RetryDelivery: true})
	n.Send(context.Background(), Event{Type: "expiring", JobID: "j6", Title: "x", RetryDelivery: true})
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
	if g := strings.Join(ids(5), ","); g != "mr:redeliver:j5,mr:eject:sr0" {
		t.Fatalf("failed copy buttons = %s", g)
	}
	if g := strings.Join(ids(6), ","); g != "mr:redeliver:j6" {
		t.Fatalf("expiring buttons = %s", g)
	}
	// A webhook message cannot carry working buttons.
	hook, hookGot := discordServer(t, 204)
	(&Notifier{Discord: &Discord{WebhookURL: hook.URL, Buttons: true}}).Send(context.Background(), Event{Type: "review", JobID: "x", Title: "x"})
	if _, ok := hookGot.body[0]["components"]; ok {
		t.Fatal("webhook message with buttons")
	}
}

// One message per disc: the "Ripping" message is edited in place with
// progress, a restart pause and the outcome, which removes the buttons;
// the bot and the webhook both work.
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
		n.Send(ctx, Event{Type: "interrupted", JobID: "j1", Title: "The Twilight Zone S01 D3", Summary: "⏸ Paused"})
		n.Send(ctx, Event{Type: "started", JobID: "j1", Title: "The Twilight Zone S01 D3"}) // resumed: same message
		n.Send(ctx, Event{Type: "ready", JobID: "j1", Title: "The Twilight Zone S01 D3"})   // its own message
		n.Send(ctx, Event{Type: "done", JobID: "j1", Title: "The Twilight Zone S01 D3", Items: []string{"✅ S01E16 · imported"}})
		n.Send(ctx, Event{Type: "progress", JobID: "j1", Summary: "after the end"}) // forgotten: no edit
		srv.Close()

		wantEdit := "PATCH /channels/chan/messages/msg42?"
		wantPost := "POST /channels/chan/messages?"
		if mode == "webhook" {
			wantEdit, wantPost = "PATCH /api/webhooks/1/abc/messages/msg42?", "POST /api/webhooks/1/abc?wait=true"
		}
		if len(reqs) != 6 || reqs[0] != wantPost || reqs[4] != wantPost {
			t.Fatalf("%s: requests = %v", mode, reqs)
		}
		for i, r := range reqs[1:] {
			if i+1 != 4 && r != wantEdit { // [4] is the ready message
				t.Fatalf("%s: requests = %v", mode, reqs)
			}
		}
		if title := bodies[2]["embeds"].([]any)[0].(map[string]any)["title"]; title != "Paused: The Twilight Zone S01 D3" {
			t.Fatalf("%s: pause title = %v", mode, title)
		}
		if title := bodies[5]["embeds"].([]any)[0].(map[string]any)["title"]; title != "✅ Done: The Twilight Zone S01 D3" {
			t.Fatalf("%s: done title = %v", mode, title)
		}
		embed := bodies[1]["embeds"].([]any)[0].(map[string]any)
		desc, _ := embed["description"].(string)
		field := embed["fields"].([]any)[0].(map[string]any)
		if !strings.Contains(desc, "Ripping 2 of 7") || !strings.Contains(field["value"].(string), "ripping 40%") {
			t.Fatalf("%s: progress embed = %v", mode, embed)
		}
		if mode == "bot" {
			if c, _ := bodies[5]["components"].([]any); c == nil || len(c) != 0 {
				t.Fatalf("final edit keeps buttons: %v", bodies[5]["components"])
			}
		}
	}
}

// The live message survives a restart (and a settings change): the ids
// are kept in a file shared by every Discord built from the config.
func TestLiveMessagesPersist(t *testing.T) {
	file := filepath.Join(t.TempDir(), "discord-messages.json")
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.Method)
		_, _ = w.Write([]byte(`{"id":"msg7"}`))
	}))
	defer srv.Close()
	first := &Notifier{Discord: &Discord{BotToken: "tok", ChannelID: "c", apiBase: srv.URL, Live: &LiveMessages{File: file}}}
	first.Send(context.Background(), Event{Type: "started", JobID: "j9", Title: "x"})
	// After the restart: a new process, a new Discord.
	again := &Notifier{Discord: &Discord{BotToken: "tok", ChannelID: "c", apiBase: srv.URL, Live: &LiveMessages{File: file}}}
	again.Send(context.Background(), Event{Type: "started", JobID: "j9", Title: "x"})
	again.Send(context.Background(), Event{Type: "done", JobID: "j9", Title: "x"})
	if strings.Join(reqs, ",") != "POST,PATCH,PATCH" {
		t.Fatalf("requests = %v", reqs)
	}
	if data, _ := os.ReadFile(file); strings.Contains(string(data), "j9") {
		t.Fatalf("finished job still remembered: %s", data)
	}
}

// A disc held for labelling: its "label" message is the one the rip then
// keeps up to date.
func TestLabelMessageBecomesTheLiveOne(t *testing.T) {
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.Method)
		_, _ = w.Write([]byte(`{"id":"m1"}`))
	}))
	defer srv.Close()
	n := &Notifier{Discord: &Discord{BotToken: "tok", ChannelID: "c", apiBase: srv.URL}}
	ctx := context.Background()
	n.Send(ctx, Event{Type: "label", JobID: "j", Title: "The Thing (1982)"})
	n.Send(ctx, Event{Type: "started", JobID: "j", Title: "The Thing (1982)"})
	n.Send(ctx, Event{Type: "done", JobID: "j", Title: "The Thing (1982)"})
	if strings.Join(reqs, ",") != "POST,PATCH,PATCH" {
		t.Fatalf("requests = %v", reqs)
	}
}

func TestOwnerGetsTheDetails(t *testing.T) {
	srv, got := discordServer(t, 200)
	n := &Notifier{Discord: &Discord{BotToken: "tok", ChannelID: "friend", apiBase: srv.URL}, Owner: &Discord{BotToken: "tok", ChannelID: "owner", apiBase: srv.URL}}
	ctx := context.Background()
	send := func(ev Event) map[string]string {
		before := len(got.body)
		n.Send(ctx, ev)
		out := map[string]string{}
		for i := before; i < len(got.body); i++ {
			embed := got.body[i]["embeds"].([]any)[0].(map[string]any)
			desc, _ := embed["description"].(string)
			out[strings.Split(got.reqs[i].URL.Path, "/")[2]] = desc
		}
		return out
	}
	for _, typ := range []string{"storage", "expiring", "drive_lost", "drive_back"} {
		if m := send(Event{Type: typ, Title: "x", Summary: "s"}); len(m) != 1 || m["owner"] == "" && typ != "storage" {
			t.Fatalf("%s went to %v", typ, m)
		}
	}
	m := send(Event{Type: "failed", Drive: "/dev/sr0", Title: "x", Error: "makemkvcon mkv: exit status 11"})
	if !strings.Contains(m["owner"], "exit status 11") || m["friend"] != "Put this disc back in to try again." {
		t.Fatalf("failed = %v", m)
	}
	m = send(Event{Type: "failed", Drive: "/dev/sr0", JobID: "j", Title: "x", Error: "mkdir: denied", RetryDelivery: true})
	if !strings.Contains(m["friend"], "Nothing for you to do") || strings.Contains(m["friend"], "mkdir") {
		t.Fatalf("failed copy = %v", m)
	}
	if m := send(Event{Type: "ready", Title: "x"}); len(m) != 1 || m["friend"] == "" {
		t.Fatalf("ready went to %v", m)
	}
	if m := send(Event{Type: "done", Title: "x", Outputs: []string{"a"}}); len(m) != 1 || m["friend"] == "" {
		t.Fatalf("done went to %v", m)
	}
	if m := send(Event{Type: "done", Title: "x", Warnings: []string{"radarr: no"}, Error: "radarr: no", ImportFailed: true}); len(m) != 2 {
		t.Fatalf("done with warnings went to %v", m)
	}
}

// With no owner target nothing changes: one channel gets everything.
func TestNoOwnerKeepsOneChannel(t *testing.T) {
	srv, got := discordServer(t, 200)
	n := &Notifier{Discord: &Discord{BotToken: "tok", ChannelID: "friend", apiBase: srv.URL}, Owner: &Discord{BotToken: "tok", apiBase: srv.URL}}
	n.Send(context.Background(), Event{Type: "failed", Drive: "/dev/sr0", Title: "x", Error: "exit status 11"})
	n.Send(context.Background(), Event{Type: "drive_lost", Summary: "gone"})
	if len(got.body) != 2 {
		t.Fatalf("messages = %d", len(got.body))
	}
	for _, r := range got.reqs {
		if !strings.Contains(r.URL.Path, "friend") {
			t.Fatalf("posted to %s", r.URL.Path)
		}
	}
	if d := got.body[0]["embeds"].([]any)[0].(map[string]any)["description"]; d != "exit status 11" {
		t.Fatalf("failure text changed: %v", d)
	}
}
