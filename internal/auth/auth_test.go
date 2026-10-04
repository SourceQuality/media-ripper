package auth

import (
	"strings"
	"testing"
	"time"
)

func TestPasswords(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$600000$") || strings.Contains(h, "correct horse") {
		t.Fatalf("hash = %s", h)
	}
	if !CheckPassword(h, "correct horse") || CheckPassword(h, "correct horsE") || CheckPassword(h, "") {
		t.Fatal("check")
	}
	if h2, _ := HashPassword("correct horse"); h2 == h {
		t.Fatal("salt must differ")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short passwords are refused")
	}
	for _, bad := range []string{"", "plain", "pbkdf2-sha256$x$a$b", "pbkdf2-sha256$99999999999$a$b", "md5$1$a$b"} {
		if CheckPassword(bad, "anything") {
			t.Fatalf("malformed hash %q accepted", bad)
		}
	}
}

func TestTokens(t *testing.T) {
	tok, hash, err := NewToken()
	if err != nil || !strings.HasPrefix(tok, "mr_") || strings.Contains(hash, tok) {
		t.Fatalf("token %q hash %q %v", tok, hash, err)
	}
	if !CheckToken(hash, tok) || CheckToken(hash, tok+"x") || CheckToken("", tok) || CheckToken(hash, "") {
		t.Fatal("check")
	}
}

func TestSessions(t *testing.T) {
	s := &Sessions{Key: []byte("0123456789abcdef0123456789abcdef"), TTL: time.Hour}
	now := time.Unix(1_800_000_000, 0)
	v := s.Issue("admin", "v1", now)
	if u, ok := s.Verify(v, "v1", now.Add(30*time.Minute)); !ok || u != "admin" {
		t.Fatalf("valid cookie: %q %v", u, ok)
	}
	if _, ok := s.Verify(v, "v1", now.Add(2*time.Hour)); ok {
		t.Fatal("expired cookie accepted")
	}
	if _, ok := s.Verify(v, "v2", now); ok {
		t.Fatal("cookie survived a password change")
	}
	other := &Sessions{Key: []byte("another key, another key, anothe"), TTL: time.Hour}
	if _, ok := other.Verify(v, "v1", now); ok {
		t.Fatal("cookie signed with another key accepted")
	}
	// Tampering with the user name breaks the signature.
	enc, sig, _ := strings.Cut(v, ".")
	_ = enc
	forged := "YWRtaW58OTk5OTk5OTk5OXx2MQ." + sig // admin|9999999999|v1
	if _, ok := s.Verify(forged, "v1", now); ok {
		t.Fatal("forged expiry accepted")
	}
}

func TestLimiter(t *testing.T) {
	l := &Limiter{Free: 5}
	now := time.Unix(0, 0)
	for i := 0; i < 4; i++ {
		if err := l.Allow("1.2.3.4", now); err != nil {
			t.Fatalf("attempt %d refused", i)
		}
		l.Result("1.2.3.4", false, now)
	}
	l.Result("1.2.3.4", false, now) // fifth failure
	if l.Allow("1.2.3.4", now.Add(10*time.Second)) == nil {
		t.Fatal("guessing not slowed down")
	}
	if l.Allow("5.6.7.8", now) != nil {
		t.Fatal("other clients are not affected")
	}
	if l.Allow("1.2.3.4", now.Add(16*time.Second)) != nil {
		t.Fatal("the wait ends")
	}
	l.Result("1.2.3.4", false, now.Add(16*time.Second)) // sixth: 30 s
	if l.Allow("1.2.3.4", now.Add(40*time.Second)) == nil {
		t.Fatal("the wait doubles")
	}
	l.Result("1.2.3.4", true, now.Add(50*time.Second))
	if l.Allow("1.2.3.4", now.Add(50*time.Second)) != nil {
		t.Fatal("success resets")
	}
}
