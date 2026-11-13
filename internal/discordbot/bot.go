// Package discordbot receives button presses on media-ripper's Discord
// messages. It holds the bot's Gateway connection, an outbound WebSocket,
// so Discord delivers interactions without the ripper being reachable from
// the internet.
package discordbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Interaction is a button press, a pick from a menu (Values) or a form sent
// (Type 5, Fields).
type Interaction struct {
	// Type is Discord's: 3 for a button or menu, 5 for a submitted form.
	Type int
	// Values are the options picked in a menu.
	Values []string
	// Fields are a submitted form's inputs, by input id.
	Fields map[string]string
	// Ephemeral: the pressed message is only shown to the person who
	// pressed; it cannot be edited through the channel.
	Ephemeral bool
	// ChannelID and MessageID locate the pressed message; an action that
	// outlives the interaction token (15 minutes) reports through them.
	ChannelID, MessageID string
	ID                   string // interaction id, for the reply
	Token                string // interaction token, for the reply and follow-up edits
	CustomID             string // what the button does, e.g. "mr:approve:<job>"
	UserID               string
	UserName             string
}

// Handler acts on a press and returns the text to put on the message.
// It may take minutes (an import); the press is acknowledged before.
type Handler func(ctx context.Context, in Interaction) string

// Modal is a form Discord shows in answer to a button.
type Modal struct {
	CustomID string // returned with the submitted form
	Title    string // at most 45 characters
	Inputs   []TextInput
}

// TextInput is one line of a Modal.
type TextInput struct {
	ID, Label, Value, Placeholder string
	Required                      bool
}

// Menu answers a submitted form: a text, and a menu to pick from when there
// are Options (at most 25 are shown).
type Menu struct {
	Content     string
	CustomID    string
	Placeholder string
	Options     []Option
}

// Option is one choice in a Menu.
type Option struct {
	Label, Value, Description string
}

// Bot keeps the Gateway connection up and dispatches presses.
type Bot struct {
	Token   string
	AppID   string   // learned from READY when empty
	Allowed []string // user ids allowed to press; empty = anyone in the channel
	Handle  Handler
	// Modal, when set, may answer a button with a form instead of running
	// Handle (nil: run Handle).
	Modal func(in Interaction) *Modal
	// Submit answers a submitted form, privately to the person who sent it.
	Submit func(ctx context.Context, in Interaction) Menu
	Logger *slog.Logger
	HTTP   *http.Client

	GatewayURL string // tests
	APIBase    string // tests

	mu    sync.Mutex
	seq   *int64
	appID string
}

const (
	gatewayDefault = "wss://gateway.discord.gg/?v=10&encoding=json"
	apiDefault     = "https://discord.com/api/v10"
)

// Run connects and reconnects until ctx ends.
func (b *Bot) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := b.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		b.log().Warn("discord gateway disconnected; reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 2*time.Minute {
			backoff *= 2
		}
	}
}

type frame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// session is one Gateway connection: hello, identify, heartbeats, events.
func (b *Bot) session(ctx context.Context) error {
	url := b.GatewayURL
	if url == "" {
		url = gatewayDefault
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4 << 20)

	var hello struct {
		HeartbeatInterval int64 `json:"heartbeat_interval"`
	}
	f, err := read(ctx, conn)
	if err != nil {
		return err
	}
	if f.Op != 10 || json.Unmarshal(f.D, &hello) != nil || hello.HeartbeatInterval <= 0 {
		return fmt.Errorf("expected hello, got op %d", f.Op)
	}
	// Interactions arrive without any intents.
	identify := map[string]any{"token": b.Token, "intents": 0,
		"properties": map[string]string{"os": "linux", "browser": "media-ripper", "device": "media-ripper"}}
	if err := send(ctx, conn, 2, identify); err != nil {
		return err
	}

	var writeMu sync.Mutex
	errc := make(chan error, 1)
	go func() {
		interval := time.Duration(hello.HeartbeatInterval) * time.Millisecond
		wait := time.Duration(rand.Int63n(int64(interval))) // jitter the first beat, as Discord asks
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = interval
			b.mu.Lock()
			seq := b.seq
			b.mu.Unlock()
			writeMu.Lock()
			err := send(ctx, conn, 1, seq)
			writeMu.Unlock()
			if err != nil {
				errc <- err
				cancel() // unblock the read below
				return
			}
		}
	}()

	for {
		select {
		case err := <-errc:
			return err
		default:
		}
		f, err := read(ctx, conn)
		if err != nil {
			return err
		}
		if f.S != nil {
			b.mu.Lock()
			b.seq = f.S
			b.mu.Unlock()
		}
		switch f.Op {
		case 0:
			b.dispatch(ctx, f)
		case 1: // the server asks for a heartbeat now
			b.mu.Lock()
			seq := b.seq
			b.mu.Unlock()
			writeMu.Lock()
			err := send(ctx, conn, 1, seq)
			writeMu.Unlock()
			if err != nil {
				return err
			}
		case 7:
			return fmt.Errorf("discord asked to reconnect")
		case 9:
			return fmt.Errorf("discord invalidated the session")
		}
	}
}

func (b *Bot) dispatch(ctx context.Context, f frame) {
	switch f.T {
	case "READY":
		var r struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
			Application struct {
				ID string `json:"id"`
			} `json:"application"`
		}
		if json.Unmarshal(f.D, &r) == nil {
			b.mu.Lock()
			b.appID = r.Application.ID
			b.mu.Unlock()
			b.log().Info("discord bot connected", "as", r.User.Username)
		}
	case "INTERACTION_CREATE":
		var d struct {
			ID     string `json:"id"`
			Token  string `json:"token"`
			Type   int    `json:"type"`
			Member *struct {
				User user `json:"user"`
			} `json:"member"`
			User      *user  `json:"user"`
			ChannelID string `json:"channel_id"`
			Message   struct {
				ID    string `json:"id"`
				Flags int    `json:"flags"`
			} `json:"message"`
			Data struct {
				CustomID   string   `json:"custom_id"`
				Values     []string `json:"values"`
				Components []struct {
					Components []struct {
						CustomID string `json:"custom_id"`
						Value    string `json:"value"`
					} `json:"components"`
				} `json:"components"`
			} `json:"data"`
		}
		if json.Unmarshal(f.D, &d) != nil || (d.Type != 3 && d.Type != 5) || !strings.HasPrefix(d.Data.CustomID, "mr:") {
			return
		}
		in := Interaction{Type: d.Type, ID: d.ID, Token: d.Token, CustomID: d.Data.CustomID, Values: d.Data.Values,
			ChannelID: d.ChannelID, MessageID: d.Message.ID, Ephemeral: d.Message.Flags&64 != 0}
		if d.Type == 5 {
			in.Fields = map[string]string{}
			for _, row := range d.Data.Components {
				for _, c := range row.Components {
					in.Fields[c.CustomID] = c.Value
				}
			}
		}
		if d.Member != nil {
			in.UserID, in.UserName = d.Member.User.ID, d.Member.User.Username
		} else if d.User != nil {
			in.UserID, in.UserName = d.User.ID, d.User.Username
		}
		go b.answer(context.WithoutCancel(ctx), in)
	}
}

type user struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// answer shows the press right away, runs the action, then reports its
// result on the message. Taking the buttons off in the acknowledgement means
// a slow action never looks ignored and cannot be started twice.
func (b *Bot) answer(ctx context.Context, in Interaction) {
	if len(b.Allowed) > 0 && !contains(b.Allowed, in.UserID) {
		_ = b.post(ctx, fmt.Sprintf("/interactions/%s/%s/callback", in.ID, in.Token),
			map[string]any{"type": 4, "data": map[string]any{"content": "You are not allowed to do that here.", "flags": 64}})
		return
	}
	who := in.UserName
	if who == "" {
		who = "someone"
	}
	none := map[string]any{"parse": []string{}}
	callback := fmt.Sprintf("/interactions/%s/%s/callback", in.ID, in.Token)
	if in.Type == 5 {
		b.answerForm(ctx, in, callback, none)
		return
	}
	if b.Modal != nil {
		if md := b.Modal(in); md != nil {
			// 9: show a form; the message and its buttons stay as they are.
			if err := b.post(ctx, callback, map[string]any{"type": 9, "data": modalData(md)}); err != nil {
				b.log().Warn("discord: open form", "err", err)
			}
			return
		}
	}
	// 7: update the pressed message as the response.
	working := map[string]any{"type": 7, "data": map[string]any{"content": fmt.Sprintf("⏳ Working on it — %s", who), "components": []any{}, "allowed_mentions": none}}
	if err := b.post(ctx, callback, working); err != nil {
		b.log().Warn("discord: acknowledge", "err", err)
		return
	}
	result := b.Handle(ctx, in)
	edit := map[string]any{"content": fmt.Sprintf("%s — %s", result, who), "components": []any{}, "allowed_mentions": none}
	err := b.request(ctx, http.MethodPatch, b.originalPath(in), edit)
	if err != nil && !in.Ephemeral && in.ChannelID != "" && in.MessageID != "" {
		// The interaction token lasts 15 minutes; the bot's own message
		// can still be edited after that.
		err = b.request(ctx, http.MethodPatch, fmt.Sprintf("/channels/%s/messages/%s", in.ChannelID, in.MessageID), edit)
	}
	if err != nil {
		b.log().Warn("discord: update message", "err", err)
	}
}

// answerForm acknowledges a submitted form with a private "thinking"
// message, then replaces it with the handler's answer and menu.
func (b *Bot) answerForm(ctx context.Context, in Interaction, callback string, none map[string]any) {
	if b.Submit == nil {
		return
	}
	// 5: deferred reply; flags 64: only the person who sent the form sees it.
	if err := b.post(ctx, callback, map[string]any{"type": 5, "data": map[string]any{"flags": 64}}); err != nil {
		b.log().Warn("discord: acknowledge form", "err", err)
		return
	}
	menu := b.Submit(ctx, in)
	reply := map[string]any{"content": menu.Content, "components": []any{}, "allowed_mentions": none}
	if len(menu.Options) > 0 {
		var opts []any
		for i, o := range menu.Options {
			if i == 25 {
				break
			}
			opt := map[string]any{"label": clip(o.Label, 100), "value": clip(o.Value, 100)}
			if o.Description != "" {
				opt["description"] = clip(o.Description, 100)
			}
			opts = append(opts, opt)
		}
		sel := map[string]any{"type": 3, "custom_id": menu.CustomID, "options": opts}
		if menu.Placeholder != "" {
			sel["placeholder"] = clip(menu.Placeholder, 150)
		}
		reply["components"] = []any{map[string]any{"type": 1, "components": []any{sel}}}
	}
	if err := b.request(ctx, http.MethodPatch, b.originalPath(in), reply); err != nil {
		b.log().Warn("discord: answer form", "err", err)
	}
}

func modalData(md *Modal) map[string]any {
	var rows []any
	for i, t := range md.Inputs {
		if i == 5 {
			break
		}
		in := map[string]any{"type": 4, "custom_id": t.ID, "label": clip(t.Label, 45), "style": 1, "required": t.Required, "max_length": 100}
		if t.Value != "" {
			in["value"] = clip(t.Value, 100)
		}
		if t.Placeholder != "" {
			in["placeholder"] = clip(t.Placeholder, 100)
		}
		rows = append(rows, map[string]any{"type": 1, "components": []any{in}})
	}
	return map[string]any{"custom_id": md.CustomID, "title": clip(md.Title, 45), "components": rows}
}

// originalPath is the interaction's own message: the pressed one for a
// button or menu, the reply for a form.
func (b *Bot) originalPath(in Interaction) string {
	b.mu.Lock()
	app := b.AppID
	if app == "" {
		app = b.appID
	}
	b.mu.Unlock()
	return fmt.Sprintf("/webhooks/%s/%s/messages/@original", app, in.Token)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (b *Bot) post(ctx context.Context, path string, body any) error {
	return b.request(ctx, http.MethodPost, path, body)
}

func (b *Bot) request(ctx context.Context, method, path string, body any) error {
	base := b.APIBase
	if base == "" {
		base = apiDefault
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bot "+b.Token)
	client := b.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func read(ctx context.Context, conn *websocket.Conn) (frame, error) {
	var f frame
	_, data, err := conn.Read(ctx)
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(data, &f)
}

func send(ctx context.Context, conn *websocket.Conn, op int, d any) error {
	data, err := json.Marshal(map[string]any{"op": op, "d": d})
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (b *Bot) log() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}
