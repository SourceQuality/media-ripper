package updates

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"v0.5.0", "v0.6.0", true},
		{"v0.5.0", "v0.5.1", true},
		{"v0.5.0", "v0.5.0", false},
		{"v0.5.0-3-gabc1234-dirty", "v0.5.0", false},
		{"v0.5.0-3-gabc1234", "v0.5.1", true},
		{"v0.10.0", "v0.9.9", false},
		{"dev", "v9.9.9", false},
		{"89f8be6", "v1.0.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.cur, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.cur, c.latest, got)
		}
	}
}

func TestWatcher(t *testing.T) {
	var auth string
	private := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/repos/SourceQuality/media-ripper/releases/latest" || (private && auth == "") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"v0.6.0","html_url":"https://github.com/SourceQuality/media-ripper/releases/tag/v0.6.0"}`))
	}))
	defer srv.Close()
	set := Settings{Check: true, Repo: "SourceQuality/media-ripper"}
	w := &Watcher{Current: "v0.5.0", APIBase: srv.URL, Settings: func() Settings { return set }}

	w.check(context.Background())
	if w.Available() != nil {
		t.Fatal("a private repo without a token cannot report a release")
	}
	set.Token = "ghp_x"
	w.check(context.Background())
	if r := w.Available(); r == nil || r.Tag != "v0.6.0" || auth != "Bearer ghp_x" {
		t.Fatalf("with token: %+v auth=%q", r, auth)
	}
	w.Current = "v0.6.0"
	if w.Available() != nil {
		t.Fatal("current build should not be offered itself")
	}
	set.Check = false
	w.latest = nil
	w.check(context.Background())
	if w.Available() != nil {
		t.Fatal("checking switched off")
	}
}
