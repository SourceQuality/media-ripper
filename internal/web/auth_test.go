package web

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sourcequality/media-ripper/internal/config"
	"github.com/sourcequality/media-ripper/internal/pipeline"
	"github.com/sourcequality/media-ripper/internal/store"
)

type testClient struct {
	t   *testing.T
	srv *httptest.Server
	c   *http.Client
}

func (c *testClient) do(method, path, body string, hdr ...string) (int, string, *http.Response) {
	req, _ := http.NewRequest(method, c.srv.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp
}

func authServer(t *testing.T) (*testClient, *pipeline.Manager, string) {
	cfg := config.Default()
	cfg.Output.Path = t.TempDir()
	cfg.Path = filepath.Join(t.TempDir(), "config.yaml")
	st, _ := store.Open(t.TempDir())
	m := pipeline.New(pipeline.Deps{Config: &cfg, Store: st})
	srv := httptest.NewServer((&Server{Manager: m, Store: st, Version: "test"}).Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &testClient{t: t, srv: srv, c: &http.Client{Jar: jar, CheckRedirect: noRedirect}}, m, cfg.Path
}

func TestSignInFlow(t *testing.T) {
	c, m, cfgPath := authServer(t)

	// Before the account exists nothing but the sign-in page works.
	if code, body, _ := c.do("GET", "/api/status", ""); code != 401 || !strings.Contains(body, `"setup": true`) {
		t.Fatalf("status before setup: %d %s", code, body)
	}
	if code, _, resp := c.do("GET", "/", ""); code != 302 || !strings.HasPrefix(resp.Header.Get("Location"), "/login.html") {
		t.Fatalf("page before setup: %d %s", code, resp.Header.Get("Location"))
	}
	if code, _, _ := c.do("GET", "/login.html", ""); code != 200 {
		t.Fatalf("login page: %d", code)
	}
	if code, _, _ := c.do("POST", "/api/auth/setup", `{"username":"jack","password":"short"}`); code != 400 {
		t.Fatalf("short password accepted: %d", code)
	}
	if code, body, _ := c.do("POST", "/api/auth/setup", `{"username":"jack","password":"correct horse battery"}`); code != 200 {
		t.Fatalf("setup: %d %s", code, body)
	}
	// Signed in by the setup; the hash, not the password, is in the file.
	if code, _, _ := c.do("GET", "/api/status", ""); code != 200 {
		t.Fatalf("status after setup: %d", code)
	}
	raw, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(raw), "correct horse") || !strings.Contains(string(raw), "pbkdf2-sha256$") {
		t.Fatalf("config file:\n%s", raw)
	}
	if code, _, _ := c.do("POST", "/api/auth/setup", `{"username":"evil","password":"another password"}`); code != 409 {
		t.Fatalf("second setup: %d", code)
	}
	// The hash never reaches the browser.
	if _, body, _ := c.do("GET", "/api/config", ""); strings.Contains(body, "pbkdf2") {
		t.Fatal("password hash in /api/config")
	}

	// Another site cannot make changes with the cookie.
	if code, _, _ := c.do("POST", "/api/notify/test", "", "Origin", "https://evil.example"); code != 403 {
		t.Fatalf("cross-site POST: %d", code)
	}

	// Sign out, then wrong and right passwords.
	c.do("POST", "/api/auth/logout", "")
	if code, _, _ := c.do("GET", "/api/status", ""); code != 401 {
		t.Fatalf("after logout: %d", code)
	}
	if code, _, _ := c.do("POST", "/api/auth/login", `{"username":"jack","password":"wrong password"}`); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	if code, _, _ := c.do("POST", "/api/auth/login", `{"username":"JACK","password":"correct horse battery"}`); code != 200 {
		t.Fatalf("login: %d", code)
	}

	// An API token works for scripts and Prometheus, and is shown once.
	_, body, _ := c.do("POST", "/api/auth/token", "")
	var tok struct{ Token string }
	_ = json.Unmarshal([]byte(body), &tok)
	if !strings.HasPrefix(tok.Token, "mr_") {
		t.Fatalf("token: %s", body)
	}
	bare := &testClient{t: t, srv: c.srv, c: &http.Client{}}
	if code, _, _ := bare.do("GET", "/metrics", ""); code != 401 {
		t.Fatalf("metrics without token: %d", code)
	}
	if code, body, _ := bare.do("GET", "/metrics", "", "Authorization", "Bearer "+tok.Token); code != 200 || !strings.Contains(body, "media_ripper_build_info") {
		t.Fatalf("metrics with token: %d", code)
	}
	if strings.Contains(m.Config().Auth.APITokenHash, tok.Token) {
		t.Fatal("token stored in the clear")
	}

	// Changing the password signs other browsers out.
	other := &testClient{t: t, srv: c.srv}
	jar, _ := cookiejar.New(nil)
	other.c = &http.Client{Jar: jar}
	other.do("POST", "/api/auth/login", `{"username":"jack","password":"correct horse battery"}`)
	if code, _, _ := c.do("POST", "/api/auth/password", `{"current":"correct horse battery","password":"a new long password"}`); code != 200 {
		t.Fatalf("change password: %d", code)
	}
	if code, _, _ := other.do("GET", "/api/status", ""); code != 401 {
		t.Fatalf("other browser still signed in: %d", code)
	}
	if code, _, _ := c.do("GET", "/api/status", ""); code != 200 {
		t.Fatalf("the browser that changed it stays signed in: %d", code)
	}
}

func TestGuessingIsSlowedDown(t *testing.T) {
	c, _, _ := authServer(t)
	c.do("POST", "/api/auth/setup", `{"username":"jack","password":"correct horse battery"}`)
	c.do("POST", "/api/auth/logout", "")
	var last int
	for i := 0; i < 6; i++ {
		last, _, _ = c.do("POST", "/api/auth/login", `{"username":"jack","password":"guess"}`)
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("6th guess: %d, want 429", last)
	}
	// Even the right password waits.
	if code, _, _ := c.do("POST", "/api/auth/login", `{"username":"jack","password":"correct horse battery"}`); code != http.StatusTooManyRequests {
		t.Fatalf("during the wait: %d", code)
	}
}

func TestBrowserHTTPS(t *testing.T) {
	tlsState := &tls.ConnectionState{}
	cases := []struct {
		name   string
		remote string
		tls    bool
		proto  string
		want   bool
	}{
		{"direct https", "10.5.3.20:5000", true, "", true},
		{"direct http", "10.5.3.20:5000", false, "", false},
		{"proxy says https", "127.0.0.1:4000", true, "https", true},
		{"Tailscale serve: http browser, TLS to us", "127.0.0.1:4000", true, "http", false},
		{"local proxy without the header", "127.0.0.1:4000", true, "", false},
		{"remote client cannot claim https", "10.5.3.20:5000", false, "https", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.tls {
			r.TLS = tlsState
		}
		if c.proto != "" {
			r.Header.Set("X-Forwarded-Proto", c.proto)
		}
		if got := browserHTTPS(r); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}
