package web

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sourcequality/media-ripper/internal/auth"
	"github.com/sourcequality/media-ripper/internal/config"
)

const sessionCookie = "mr_session"

// publicPaths work without signing in: the sign-in page and what it needs.
var publicPaths = map[string]bool{
	"/login.html": true, "/login.js": true, "/style.css": true, "/theme.js": true, "/healthz": true,
	"/api/auth/status": true, "/api/auth/login": true, "/api/auth/setup": true,
}

type authState struct {
	once     sync.Once
	sessions *auth.Sessions
	err      error
	limiter  auth.Limiter
}

func (s *Server) sessions() (*auth.Sessions, error) {
	s.auth.once.Do(func() {
		key, err := s.Store.Secret("session.key", 32)
		s.auth.sessions, s.auth.err = &auth.Sessions{Key: key, TTL: 30 * 24 * time.Hour}, err
		s.auth.limiter.Free = 5
	})
	return s.auth.sessions, s.auth.err
}

// user returns who the request is signed in as: by session cookie, or
// "api" for a valid API token.
func (s *Server) user(r *http.Request, a config.Auth) (string, bool) {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && auth.CheckToken(a.APITokenHash, strings.TrimSpace(tok)) {
		return "api", true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || a.PasswordHash == "" {
		return "", false
	}
	sess, err := s.sessions()
	if err != nil {
		return "", false
	}
	u, ok := sess.Verify(c.Value, auth.PasswordVersion(a.PasswordHash), time.Now())
	return u, ok && u == a.Username
}

// guard lets a request through when sign-in is off, the path is public,
// or the request is signed in. Changes made with a cookie must come from
// this site (SameSite=Strict already stops other sites from sending it;
// the Origin check covers older browsers).
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := s.Manager.Config().Auth
		if !a.Enabled || publicPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		u, ok := s.user(r, a)
		if ok {
			if u != "api" && r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "request from another site"})
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/metrics" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required", "setup": a.PasswordHash == ""})
			return
		}
		http.Redirect(w, r, "/login.html?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
	})
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return true // not a browser form or fetch; SameSite covers cookies
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	for _, h := range []string{r.Host, r.Header.Get("X-Forwarded-Host")} {
		if h != "" && strings.EqualFold(u.Host, h) {
			return true
		}
	}
	return false
}

// client identifies who is guessing passwords. Behind a local reverse
// proxy (Tailscale serve) the real address is in X-Forwarded-For.
func client(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if f := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); f != "" {
			return f
		}
	}
	return host
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	a := s.Manager.Config().Auth
	u, ok := s.user(r, a)
	writeJSON(w, http.StatusOK, map[string]any{"enabled": a.Enabled, "setup": a.Enabled && a.PasswordHash == "", "signed_in": ok, "user": u})
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Current  string `json:"current"`
}

func readCredentials(w http.ResponseWriter, r *http.Request) (credentials, bool) {
	var c credentials
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&c); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return c, false
	}
	c.Username = strings.TrimSpace(c.Username)
	return c, true
}

// authSetup creates the account, once, while none exists.
func (s *Server) authSetup(w http.ResponseWriter, r *http.Request) {
	c, ok := readCredentials(w, r)
	if !ok {
		return
	}
	if s.Manager.Config().Auth.PasswordHash != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the account already exists; sign in"})
		return
	}
	if c.Username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "choose a user name"})
		return
	}
	hash, err := auth.HashPassword(c.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.updateConfig(func(cfg *config.Config) { cfg.Auth.Username, cfg.Auth.PasswordHash = c.Username, hash }); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.signIn(w, r, c.Username, hash)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	c, ok := readCredentials(w, r)
	if !ok {
		return
	}
	who, now := client(r), time.Now()
	if _, err := s.sessions(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.auth.limiter.Allow(who, now); err != nil {
		writeErr(w, http.StatusTooManyRequests, err)
		return
	}
	a := s.Manager.Config().Auth
	// The password is always checked, so a wrong user name takes as long
	// as a wrong password and does not reveal which names exist.
	passOK := auth.CheckPassword(a.PasswordHash, c.Password)
	good := a.PasswordHash != "" && strings.EqualFold(c.Username, a.Username) && passOK
	s.auth.limiter.Result(who, good, now)
	if !good {
		if s.Logger != nil {
			s.Logger.Warn("failed sign-in", "user", c.Username, "from", who)
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong user name or password"})
		return
	}
	s.signIn(w, r, a.Username, a.PasswordHash)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) signIn(w http.ResponseWriter, r *http.Request, user, hash string) {
	sess, err := s.sessions()
	if err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: sess.Issue(user, auth.PasswordVersion(hash), time.Now()), Path: "/",
		MaxAge: int(sess.TTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"})
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// authPassword changes the password; every other browser is signed out.
func (s *Server) authPassword(w http.ResponseWriter, r *http.Request) {
	c, ok := readCredentials(w, r)
	if !ok {
		return
	}
	a := s.Manager.Config().Auth
	if !auth.CheckPassword(a.PasswordHash, c.Current) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "the current password is wrong"})
		return
	}
	hash, err := auth.HashPassword(c.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.updateConfig(func(cfg *config.Config) { cfg.Auth.PasswordHash = hash }); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.signIn(w, r, a.Username, hash)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// authToken creates (POST) or revokes (DELETE) the API token. The token
// is only ever shown in this response.
func (s *Server) authToken(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		if err := s.updateConfig(func(cfg *config.Config) { cfg.Auth.APITokenHash = "" }); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	tok, hash, err := auth.NewToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.updateConfig(func(cfg *config.Config) { cfg.Auth.APITokenHash = hash }); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

// updateConfig changes the running configuration and saves it.
func (s *Server) updateConfig(change func(*config.Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := *s.Manager.Config()
	change(&next)
	if err := next.Validate(); err != nil {
		return err
	}
	path := next.Path
	if path == "" {
		path = config.DefaultPath()
	}
	if err := next.Save(path); err != nil {
		return err
	}
	next.Path = path
	s.Manager.SetConfig(&next)
	if s.Logger != nil {
		s.Logger.Info("sign-in settings changed")
	}
	return nil
}
