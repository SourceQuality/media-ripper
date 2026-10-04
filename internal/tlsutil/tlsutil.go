// Package tlsutil gives the web UI HTTPS without any setup: a certificate
// made on this machine for its own names and addresses, renewed before it
// expires, and a listener that also answers plain HTTP with a redirect.
package tlsutil

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Lifetime stays under the 825 days Apple platforms accept for a server
// certificate; Renew is how early a new one is made.
const (
	Lifetime = 820 * 24 * time.Hour
	Renew    = 30 * 24 * time.Hour
)

// Names lists what the certificate should cover: the host name (and its
// .local form), localhost, every address of this machine, and extra.
func Names(extra []string) []string {
	set := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	if h, err := os.Hostname(); err == nil && h != "" {
		set[strings.ToLower(h)] = true
		set[strings.ToLower(h)+".local"] = true
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				set[ipn.IP.String()] = true
			}
		}
	}
	for _, e := range extra {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			set[e] = true
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SelfSigned returns a certificate for names kept in dir (cert.pem and
// key.pem), making a new one when there is none, it expires within Renew,
// or it does not cover every name.
func SelfSigned(dir string, names []string, now time.Time) (tls.Certificate, error) {
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if c, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil && covers(c, names, now) {
		return c, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "media-ripper " + host, Organization: []string{"media-ripper (self-signed)"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(Lifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := writeFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(certFile, keyFile)
}

func covers(c tls.Certificate, names []string, now time.Time) bool {
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil || now.Add(Renew).After(leaf.NotAfter) {
		return false
	}
	for _, n := range names {
		if leaf.VerifyHostname(n) != nil {
			return false
		}
	}
	return true
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Fingerprint is the certificate's SHA-256, as browsers show it, so a
// person can check the one they are asked to trust.
func Fingerprint(c tls.Certificate) string {
	if len(c.Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(c.Certificate[0])
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var b strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}

// Expiry returns when the certificate stops being valid.
func Expiry(c tls.Certificate) time.Time {
	if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil {
		return leaf.NotAfter
	}
	return time.Time{}
}

// Split serves TLS and plain HTTP on one port: connections that open with
// a TLS handshake go to the TLS listener, others to the plain one (which
// the caller answers with a redirect to https).
func Split(l net.Listener) (tlsL, plainL net.Listener) {
	s := &splitter{inner: l, tlsCh: make(chan net.Conn), plainCh: make(chan net.Conn), done: make(chan struct{})}
	go s.run()
	return &chanListener{s: s, ch: s.tlsCh}, &chanListener{s: s, ch: s.plainCh}
}

type splitter struct {
	inner          net.Listener
	tlsCh, plainCh chan net.Conn
	done           chan struct{}
	err            error
}

func (s *splitter) run() {
	defer close(s.done)
	for {
		c, err := s.inner.Accept()
		if err != nil {
			s.err = err
			return
		}
		go s.route(c)
	}
}

// route peeks at the first byte: 0x16 is a TLS handshake record.
func (s *splitter) route(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		return
	}
	pc := &peekedConn{Conn: c, r: br}
	ch := s.plainCh
	if first[0] == 0x16 {
		ch = s.tlsCh
	}
	select {
	case ch <- pc:
	case <-s.done:
		c.Close()
	}
}

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }

type chanListener struct {
	s  *splitter
	ch chan net.Conn
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.s.done:
		if l.s.err != nil {
			return nil, l.s.err
		}
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error   { return l.s.inner.Close() }
func (l *chanListener) Addr() net.Addr { return l.s.inner.Addr() }

// Load reads a certificate and key from files (web.tls: files).
func Load(certFile, keyFile string) (tls.Certificate, error) {
	if certFile == "" || keyFile == "" {
		return tls.Certificate{}, errors.New("web.tls is files but web.tls_cert or web.tls_key is empty")
	}
	c, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load certificate: %w", err)
	}
	return c, nil
}
