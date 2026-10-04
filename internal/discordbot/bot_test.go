package discordbot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type rest struct {
	mu     sync.Mutex
	calls  []string
	bodies []map[string]any
}

func fakeDiscord(t *testing.T, press func(conn *websocket.Conn, ctx context.Context)) (gateway, api *httptest.Server, r *rest) {
	r = &rest{}
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		data, _ := io.ReadAll(req.Body)
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		r.mu.Lock()
		r.calls = append(r.calls, req.Method+" "+req.URL.Path+" auth="+req.Header.Get("Authorization"))
		r.bodies = append(r.bodies, m)
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	gateway = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := req.Context()
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"op":10,"d":{"heartbeat_interval":45000}}`))
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var ident struct {
			Op int `json:"op"`
			D  struct {
				Token   string `json:"token"`
				Intents int    `json:"intents"`
			} `json:"d"`
		}
		_ = json.Unmarshal(data, &ident)
		if ident.Op != 2 || ident.D.Token != "bot-token" {
			t.Errorf("identify = %s", data)
		}
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"op":0,"s":1,"t":"READY","d":{"user":{"username":"media-ripper"},"application":{"id":"1556123331047202997"}}}`))
		press(conn, ctx)
		<-ctx.Done()
	}))
	t.Cleanup(func() { gateway.Close(); api.Close() })
	return gateway, api, r
}

func pressEvent(user, customID string) string {
	return `{"op":0,"s":2,"t":"INTERACTION_CREATE","d":{"id":"int1","token":"tok1","type":3,"member":{"user":{"id":"` + user + `","username":"jack"}},"data":{"custom_id":"` + customID + `"}}}`
}

func waitCalls(r *rest, n int) []string {
	for i := 0; i < 200; i++ {
		r.mu.Lock()
		got := append([]string(nil), r.calls...)
		r.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestButtonPressIsAcknowledgedHandledAndShown(t *testing.T) {
	gw, api, r := fakeDiscord(t, func(conn *websocket.Conn, ctx context.Context) {
		_ = conn.Write(ctx, websocket.MessageText, []byte(pressEvent("u1", "mr:approve:job1")))
	})
	var got Interaction
	b := &Bot{Token: "bot-token", GatewayURL: "ws" + strings.TrimPrefix(gw.URL, "http"), APIBase: api.URL,
		Handle: func(_ context.Context, in Interaction) string { got = in; return "Imported 8 episodes" }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	calls := waitCalls(r, 2)
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
	if calls[0] != "POST /interactions/int1/tok1/callback auth=Bot bot-token" || r.bodies[0]["type"] != float64(6) {
		t.Fatalf("acknowledge = %s %v", calls[0], r.bodies[0])
	}
	if calls[1] != "PATCH /webhooks/1556123331047202997/tok1/messages/@original auth=Bot bot-token" {
		t.Fatalf("edit = %s", calls[1])
	}
	if c := r.bodies[1]["content"]; c != "Imported 8 episodes — jack" {
		t.Fatalf("edited content = %v", c)
	}
	if got.CustomID != "mr:approve:job1" || got.UserID != "u1" {
		t.Fatalf("handler got %+v", got)
	}
}

func TestOnlyAllowedUsersMayPress(t *testing.T) {
	gw, api, r := fakeDiscord(t, func(conn *websocket.Conn, ctx context.Context) {
		_ = conn.Write(ctx, websocket.MessageText, []byte(pressEvent("stranger", "mr:eject:sr0")))
	})
	handled := false
	b := &Bot{Token: "bot-token", Allowed: []string{"u1"}, GatewayURL: "ws" + strings.TrimPrefix(gw.URL, "http"), APIBase: api.URL,
		Handle: func(context.Context, Interaction) string { handled = true; return "" }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)
	calls := waitCalls(r, 1)
	time.Sleep(50 * time.Millisecond)
	if handled || len(calls) != 1 || r.bodies[0]["type"] != float64(4) {
		t.Fatalf("handled=%v calls=%v body=%v", handled, calls, r.bodies)
	}
	if data, _ := r.bodies[0]["data"].(map[string]any); data["flags"] != float64(64) {
		t.Fatalf("refusal should be visible only to the presser: %v", r.bodies[0])
	}
}
