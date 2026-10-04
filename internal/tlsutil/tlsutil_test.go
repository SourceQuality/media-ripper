package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSelfSigned(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	names := []string{"localhost", "127.0.0.1", "jacktheripper-arm", "jacktheripper-arm.ts.brello.cloud", "100.64.0.5"}
	c1, err := SelfSigned(dir, names, now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(c1.Certificate[0])
	for _, n := range names {
		if err := leaf.VerifyHostname(n); err != nil {
			t.Errorf("does not cover %s: %v", n, err)
		}
	}
	if leaf.NotAfter.Sub(now) > 825*24*time.Hour {
		t.Fatalf("lifetime %v exceeds what Apple accepts", leaf.NotAfter.Sub(now))
	}
	// Reused while valid and covering.
	c2, _ := SelfSigned(dir, names, now.Add(time.Hour))
	if Fingerprint(c2) != Fingerprint(c1) {
		t.Fatal("certificate remade without reason")
	}
	// Remade for a new name, and when about to expire.
	c3, _ := SelfSigned(dir, append(names, "ripper.example"), now)
	if Fingerprint(c3) == Fingerprint(c1) {
		t.Fatal("new name not covered")
	}
	c4, _ := SelfSigned(dir, names, now.Add(Lifetime-Renew+time.Hour))
	if Fingerprint(c4) == Fingerprint(c3) {
		t.Fatal("not renewed before expiry")
	}
	if !strings.Contains(Fingerprint(c1), ":") || len(Fingerprint(c1)) != 95 {
		t.Fatalf("fingerprint %q", Fingerprint(c1))
	}
}

// One port: TLS works, plain HTTP is redirected to https.
func TestSplitServesBoth(t *testing.T) {
	cert, err := SelfSigned(t.TempDir(), []string{"localhost", "127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	tlsL, plainL := Split(l)
	defer l.Close()
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "secure") })
	go (&http.Server{Handler: app}).Serve(tls.NewListener(tlsL, &tls.Config{Certificates: []tls.Certificate{cert}}))
	go (&http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+r.Host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})}).Serve(plainL)
	addr := l.Addr().String()

	plain := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := plain.Get("http://" + addr + "/history.html")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 308 || resp.Header.Get("Location") != "https://"+addr+"/history.html" {
		t.Fatalf("plain: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	pool := x509.NewCertPool()
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	pool.AddCert(leaf)
	secure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err = secure.Get("https://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "secure" || resp.TLS == nil {
		t.Fatalf("tls: %q", body)
	}
}
