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

// Interaction is a button press.
type Interaction struct {
	ID       string // interaction id, for the reply
	Token    string // interaction token, for the reply and follow-up edits
	CustomID string // what the button does, e.g. "mr:approve:<job>"
	UserID   string
	UserName string
}

// Handler acts on a press and returns the text to put on the message.
// It may take minutes (an import); the press is acknowledged before.
type Handler func(ctx context.Context, in Interaction) string

// Bot keeps the Gateway connection up and dispatches presses.
type Bot struct {
	Token   string
	AppID   string   // learned from READY when empty
	Allowed []string // user ids allowed to press; empty = anyone in the channel
	Handle  Handler
	Logger  *slog.Logger
	HTTP    *http.Client

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
			User *user `json:"user"`
			Data struct {
				CustomID string `json:"custom_id"`
			} `json:"data"`
		}
		if json.Unmarshal(f.D, &d) != nil || d.Type != 3 || !strings.HasPrefix(d.Data.CustomID, "mr:") {
			return
		}
		in := Interaction{ID: d.ID, Token: d.Token, CustomID: d.Data.CustomID}
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

// answer acknowledges within Discord's 3 seconds, runs the action, then
// rewrites the message: the result replaces the buttons.
func (b *Bot) answer(ctx context.Context, in Interaction) {
	if len(b.Allowed) > 0 && !contains(b.Allowed, in.UserID) {
		_ = b.post(ctx, fmt.Sprintf("/interactions/%s/%s/callback", in.ID, in.Token),
			map[string]any{"type": 4, "data": map[string]any{"content": "You are not allowed to do that here.", "flags": 64}})
		return
	}
	// 6: deferred update; the message keeps showing until edited.
	if err := b.post(ctx, fmt.Sprintf("/interactions/%s/%s/callback", in.ID, in.Token), map[string]any{"type": 6}); err != nil {
		b.log().Warn("discord: acknowledge", "err", err)
		return
	}
	result := b.Handle(ctx, in)
	who := in.UserName
	if who == "" {
		who = "someone"
	}
	b.mu.Lock()
	app := b.AppID
	if app == "" {
		app = b.appID
	}
	b.mu.Unlock()
	edit := map[string]any{"content": fmt.Sprintf("%s — %s", result, who), "components": []any{}, "allowed_mentions": map[string]any{"parse": []string{}}}
	if err := b.request(ctx, http.MethodPatch, fmt.Sprintf("/webhooks/%s/%s/messages/@original", app, in.Token), edit); err != nil {
		b.log().Warn("discord: update message", "err", err)
	}
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
