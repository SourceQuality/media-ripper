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
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"op":0,"s":1,"t":"READY","d":{"user":{"username":"media-ripper"},"application":{"id":"123456789012345678"}}}`))
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
	if calls[0] != "POST /interactions/int1/tok1/callback auth=Bot bot-token" || r.bodies[0]["type"] != float64(7) {
		t.Fatalf("acknowledge = %s %v", calls[0], r.bodies[0])
	}
	// The press shows at once and the buttons go, so it cannot run twice.
	ack := r.bodies[0]["data"].(map[string]any)
	if c, _ := ack["components"].([]any); c == nil || len(c) != 0 || !strings.Contains(ack["content"].(string), "Working") {
		t.Fatalf("acknowledge data = %v", ack)
	}
	if calls[1] != "PATCH /webhooks/123456789012345678/tok1/messages/@original auth=Bot bot-token" {
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

// An action can outlive the interaction token (15 minutes): its result then
// goes through the bot's own message.
// recorder is a Discord API that records what it was sent.
type recorder struct {
	mu     sync.Mutex
	calls  []string
	bodies []map[string]any
}

func newRecorder(t *testing.T) (*recorder, string) {
	rec := &recorder{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		rec.mu.Lock()
		rec.calls = append(rec.calls, r.Method+" "+r.URL.Path)
		rec.bodies = append(rec.bodies, m)
		rec.mu.Unlock()
	}))
	t.Cleanup(api.Close)
	return rec, api.URL
}

// A button can open a form instead of acting; the message stays as it is.
func TestButtonOpensForm(t *testing.T) {
	rec, url := newRecorder(t)
	handled := false
	b := &Bot{Token: "t", AppID: "app", APIBase: url,
		Handle: func(context.Context, Interaction) string { handled = true; return "x" },
		Modal: func(in Interaction) *Modal {
			if in.CustomID != "mr:title:movie:j1" {
				return nil
			}
			return &Modal{CustomID: "mr:titlesearch:movie:j1", Title: "Find the movie", Inputs: []TextInput{{ID: "q", Label: "Movie title", Value: "Friends", Required: true}}}
		}}
	b.answer(context.Background(), Interaction{Type: 3, ID: "i", Token: "tok", CustomID: "mr:title:movie:j1", UserName: "jack"})
	if handled || len(rec.calls) != 1 || rec.bodies[0]["type"] != float64(9) {
		t.Fatalf("handled=%v calls=%v bodies=%v", handled, rec.calls, rec.bodies)
	}
	data := rec.bodies[0]["data"].(map[string]any)
	input := data["components"].([]any)[0].(map[string]any)["components"].([]any)[0].(map[string]any)
	if data["custom_id"] != "mr:titlesearch:movie:j1" || input["type"] != float64(4) || input["custom_id"] != "q" || input["value"] != "Friends" {
		t.Fatalf("form = %v", data)
	}
	// Another button still acts.
	b.answer(context.Background(), Interaction{Type: 3, ID: "i2", Token: "tok2", CustomID: "mr:approve:j1"})
	if !handled {
		t.Fatal("approve not handled")
	}
}

// A sent form is answered privately with a menu of what it found.
func TestFormAnsweredWithMenu(t *testing.T) {
	rec, url := newRecorder(t)
	var got Interaction
	b := &Bot{Token: "t", AppID: "app", APIBase: url, Handle: func(context.Context, Interaction) string { return "x" },
		Submit: func(_ context.Context, in Interaction) Menu {
			got = in
			return Menu{Content: "Which movie?", CustomID: "mr:titlepick:j1:0", Options: []Option{{Label: "Movie · Friends Movie (2001)", Value: "movie:500"}}}
		}}
	b.answer(context.Background(), Interaction{Type: 5, ID: "i", Token: "tok", CustomID: "mr:titlesearch:movie:j1", Fields: map[string]string{"q": "friends"}})
	if got.Fields["q"] != "friends" {
		t.Fatalf("form fields = %v", got.Fields)
	}
	if len(rec.calls) != 2 || rec.bodies[0]["type"] != float64(5) || rec.bodies[0]["data"].(map[string]any)["flags"] != float64(64) {
		t.Fatalf("acknowledge = %v %v", rec.calls, rec.bodies)
	}
	if rec.calls[1] != "PATCH /webhooks/app/tok/messages/@original" || rec.bodies[1]["content"] != "Which movie?" {
		t.Fatalf("reply = %v %v", rec.calls[1], rec.bodies[1])
	}
	sel := rec.bodies[1]["components"].([]any)[0].(map[string]any)["components"].([]any)[0].(map[string]any)
	opt := sel["options"].([]any)[0].(map[string]any)
	if sel["type"] != float64(3) || sel["custom_id"] != "mr:titlepick:j1:0" || opt["label"] != "Movie · Friends Movie (2001)" || opt["value"] != "movie:500" {
		t.Fatalf("menu = %v", sel)
	}
}

// A pick from a private menu is handled like a press; the private message
// cannot be edited through the channel, so that is not tried.
func TestMenuPickHandled(t *testing.T) {
	rec, url := newRecorder(t)
	var got Interaction
	b := &Bot{Token: "t", AppID: "app", APIBase: url, Handle: func(_ context.Context, in Interaction) string { got = in; return "✏️ Imported" }}
	b.answer(context.Background(), Interaction{Type: 3, ID: "i", Token: "tok", CustomID: "mr:titlepick:j1:0", Values: []string{"movie:500"}, Ephemeral: true, ChannelID: "c", MessageID: "m", UserName: "jack"})
	if len(got.Values) != 1 || got.Values[0] != "movie:500" {
		t.Fatalf("values = %v", got.Values)
	}
	if len(rec.calls) != 2 || rec.bodies[0]["type"] != float64(7) || rec.bodies[1]["content"] != "✏️ Imported — jack" {
		t.Fatalf("calls = %v bodies = %v", rec.calls, rec.bodies)
	}
}

// Only allowed users may send a form, as for buttons.
func TestOnlyAllowedUsersMaySendForms(t *testing.T) {
	rec, url := newRecorder(t)
	searched := false
	b := &Bot{Token: "t", APIBase: url, Allowed: []string{"u1"}, Handle: func(context.Context, Interaction) string { return "x" },
		Submit: func(context.Context, Interaction) Menu { searched = true; return Menu{} }}
	b.answer(context.Background(), Interaction{Type: 5, ID: "i", Token: "tok", UserID: "u2", CustomID: "mr:titlesearch:movie:j1"})
	if searched || len(rec.calls) != 1 || rec.bodies[0]["type"] != float64(4) {
		t.Fatalf("searched=%v calls=%v", searched, rec.calls)
	}
}

// Discord's form and menu events arrive with their inputs and picks.
func TestDispatchReadsFormsAndMenus(t *testing.T) {
	var mu sync.Mutex
	var got []Interaction
	_, url := newRecorder(t)
	b := &Bot{Token: "t", APIBase: url,
		Handle: func(_ context.Context, in Interaction) string {
			mu.Lock()
			got = append(got, in)
			mu.Unlock()
			return "x"
		},
		Submit: func(_ context.Context, in Interaction) Menu {
			mu.Lock()
			got = append(got, in)
			mu.Unlock()
			return Menu{}
		}}
	form := `{"id":"i","token":"t","type":5,"user":{"id":"u"},"data":{"custom_id":"mr:titlesearch:tv:j1","components":[{"type":1,"components":[{"type":4,"custom_id":"q","value":"friends"}]},{"type":1,"components":[{"type":4,"custom_id":"season","value":"2"}]}]}}`
	pick := `{"id":"i2","token":"t2","type":3,"user":{"id":"u"},"message":{"id":"m","flags":64},"data":{"custom_id":"mr:titlepick:j1:2","component_type":3,"values":["tv:79168"]}}`
	b.dispatch(context.Background(), frame{T: "INTERACTION_CREATE", D: json.RawMessage(form)})
	b.dispatch(context.Background(), frame{T: "INTERACTION_CREATE", D: json.RawMessage(pick)})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("got %d interactions", len(got))
	}
	var formIn, pickIn Interaction
	for _, in := range got {
		if in.Type == 5 {
			formIn = in
		} else {
			pickIn = in
		}
	}
	if formIn.Fields["q"] != "friends" || formIn.Fields["season"] != "2" {
		t.Fatalf("form = %+v", formIn)
	}
	if len(pickIn.Values) != 1 || pickIn.Values[0] != "tv:79168" || !pickIn.Ephemeral {
		t.Fatalf("pick = %+v", pickIn)
	}
}

func TestResultReportedAfterTokenExpires(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/webhooks/") {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer api.Close()
	b := &Bot{Token: "t", AppID: "app", APIBase: api.URL, Handle: func(context.Context, Interaction) string { return "Imported" }}
	b.answer(context.Background(), Interaction{ID: "i", Token: "tok", UserName: "jack", ChannelID: "c1", MessageID: "m1"})
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 || calls[2] != "PATCH /channels/c1/messages/m1" {
		t.Fatalf("calls = %v", calls)
	}
}
