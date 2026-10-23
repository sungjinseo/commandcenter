package mail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"log/slog"
	"math/big"
	"mime"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo/repotest"
)

type captured struct {
	from, rcpt []string
	data       string
	auth       string
}

// fakeSMTP is a minimal in-process SMTP server.
type fakeSMTP struct {
	ln       net.Listener
	mu       sync.Mutex
	got      []captured
	tlsConf  *tls.Config // non-nil: offers STARTTLS (or serves implicit TLS when implicit)
	implicit bool
}

func newFake(t *testing.T, tc *tls.Config, implicit bool) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if tc != nil && implicit {
		ln = tls.NewListener(ln, tc)
	}
	f := &fakeSMTP{ln: ln, tlsConf: tc, implicit: implicit}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve(c net.Conn) {
	defer func() { c.Close() }()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	say := func(s string) { io.WriteString(c, s+"\r\n") }
	say("220 fake ESMTP")
	var cur captured
	secure := f.implicit
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			io.WriteString(c, "250-fake\r\n")
			if f.tlsConf != nil && !secure {
				io.WriteString(c, "250-STARTTLS\r\n")
			}
			io.WriteString(c, "250 AUTH PLAIN\r\n")
		case up == "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, f.tlsConf)
			if err := tc.Handshake(); err != nil {
				return
			}
			c = tc
			secure = true
			r = bufio.NewReader(c)
		case strings.HasPrefix(up, "AUTH PLAIN"):
			b, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(line[len("AUTH PLAIN"):]))
			cur.auth = strings.ReplaceAll(string(b), "\x00", "|")
			say("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			cur.from = append(cur.from, line[len("MAIL FROM:"):])
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			cur.rcpt = append(cur.rcpt, line[len("RCPT TO:"):])
			say("250 ok")
		case up == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			cur.data = b.String()
			f.mu.Lock()
			f.got = append(f.got, cur)
			f.mu.Unlock()
			cur = captured{}
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (f *fakeSMTP) messages() []captured {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]captured(nil), f.got...)
}

func testSender(sink *repotest.Sink) *Sender {
	return New(slog.New(slog.DiscardHandler), sink)
}

func TestSendNowPlain(t *testing.T) {
	f := newFake(t, nil, false)
	s := testSender(&repotest.Sink{})
	defer s.Close()
	cfg := meta.SMTP{Host: "127.0.0.1", Port: f.port(), Security: "none", From: "Command Center <cc@example.com>", Enabled: true,
		Username: "u", Password: "s3cret"}
	err := s.SendNow(context.Background(), cfg, []string{"a@example.com", "Bob <b@example.com>"}, "Task failed: 정리\r\nBcc: evil@x.com", "line1\nline2 한글")
	if err != nil {
		t.Fatal(err)
	}
	m := f.messages()
	if len(m) != 1 {
		t.Fatalf("messages: %d", len(m))
	}
	g := m[0]
	if len(g.rcpt) != 2 || !strings.Contains(g.rcpt[0], "<a@example.com>") || !strings.Contains(g.rcpt[1], "<b@example.com>") {
		t.Fatalf("rcpt %v", g.rcpt)
	}
	if len(g.from) != 1 || !strings.Contains(g.from[0], "<cc@example.com>") {
		t.Fatalf("from %v", g.from)
	}
	if g.auth != "|u|s3cret" {
		t.Fatalf("auth %q", g.auth)
	}
	msg, err := mail.ReadMessage(strings.NewReader(g.data))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get("Bcc") != "" {
		t.Fatalf("header injection: %v", msg.Header)
	}
	subj, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || !strings.HasPrefix(subj, "Task failed: 정리") {
		t.Fatalf("subject %q %v", subj, err)
	}
	if !strings.Contains(msg.Header.Get("From"), "cc@example.com") || !strings.Contains(msg.Header.Get("To"), "a@example.com") ||
		msg.Header.Get("Date") == "" || !strings.HasPrefix(msg.Header.Get("Content-Type"), "text/plain; charset=utf-8") ||
		msg.Header.Get("MIME-Version") != "1.0" {
		t.Fatalf("headers: %v", msg.Header)
	}
	raw, _ := io.ReadAll(msg.Body)
	dec, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
	if err != nil || string(dec) != "line1\r\nline2 한글" {
		t.Fatalf("body %q %v", dec, err)
	}
}

func selfSigned(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, pool
}

func TestSendNowTLSModes(t *testing.T) {
	srvConf, pool := selfSigned(t)
	for _, mode := range []string{"starttls", "tls"} {
		t.Run(mode, func(t *testing.T) {
			f := newFake(t, srvConf, mode == "tls")
			s := testSender(&repotest.Sink{})
			defer s.Close()
			s.rootCAs = pool
			cfg := meta.SMTP{Host: "127.0.0.1", Port: f.port(), Security: mode, From: "cc@example.com", Enabled: true, Username: "u", Password: "p"}
			if err := s.SendNow(context.Background(), cfg, []string{"a@example.com"}, "hi", "body"); err != nil {
				t.Fatal(err)
			}
			if n := len(f.messages()); n != 1 {
				t.Fatalf("messages: %d", n)
			}
			// Verified TLS: without the test root the certificate is refused.
			s.rootCAs = nil
			if err := s.SendNow(context.Background(), cfg, []string{"a@example.com"}, "hi", "body"); err == nil {
				t.Fatal("untrusted certificate accepted")
			}
		})
	}
}

func TestSendNowErrors(t *testing.T) {
	s := testSender(&repotest.Sink{})
	defer s.Close()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens
	cfg := meta.SMTP{Host: "127.0.0.1", Port: port, Security: "none", From: "cc@example.com", Enabled: true}
	if err := s.SendNow(context.Background(), cfg, []string{"a@example.com"}, "x", "y"); err == nil {
		t.Fatal("connection failure not reported")
	}
	if err := s.SendNow(context.Background(), cfg, []string{"not an address"}, "x", "y"); err == nil {
		t.Fatal("bad recipient accepted")
	}
	if err := s.SendNow(context.Background(), cfg, nil, "x", "y"); err == nil {
		t.Fatal("no recipients accepted")
	}
}

func TestSendAsyncDeliversAndNeverBlocks(t *testing.T) {
	f := newFake(t, nil, false)
	sink := &repotest.Sink{}
	s := testSender(sink)
	cfg := meta.SMTP{Host: "127.0.0.1", Port: f.port(), Security: "none", From: "cc@example.com", Enabled: true}
	s.SendAsync(cfg, []string{"a@example.com"}, "async", "b")
	s.Close() // drains
	if n := len(f.messages()); n != 1 {
		t.Fatalf("delivered %d", n)
	}
	if len(sink.Events()) != 0 {
		t.Fatalf("unexpected events: %s", sink)
	}
	// After Close, SendAsync neither blocks nor panics.
	s.SendAsync(cfg, []string{"a@example.com"}, "late", "b")
	if s.Dropped() != 1 {
		t.Fatalf("dropped %d", s.Dropped())
	}
}

func TestSendAsyncFailureRecordsEvent(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	sink := &repotest.Sink{}
	s := testSender(sink)
	cfg := meta.SMTP{Host: "127.0.0.1", Port: port, Security: "none", From: "cc@example.com", Enabled: true, Password: "topsecret"}
	start := time.Now()
	s.SendAsync(cfg, []string{"a@example.com"}, "Task failed: nightly", "b")
	if time.Since(start) > time.Second {
		t.Fatal("SendAsync blocked")
	}
	s.Close()
	evs := sink.Events()
	if len(evs) != 1 || evs[0].Kind != "email.failed" || !strings.Contains(evs[0].Subject, "Task failed: nightly") {
		t.Fatalf("events: %+v", evs)
	}
	if strings.Contains(evs[0].Detail, "topsecret") || evs[0].Detail == "" {
		t.Fatalf("detail %q", evs[0].Detail)
	}
}

func TestSendAsyncFullQueueDrops(t *testing.T) {
	// A server that accepts but never answers keeps the worker busy; the queue
	// then fills and further sends are dropped without blocking.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	s := testSender(&repotest.Sink{})
	s.timeout = 200 * time.Millisecond
	cfg := meta.SMTP{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Security: "none", From: "cc@example.com", Enabled: true}
	start := time.Now()
	for i := 0; i < QueueSize+50; i++ {
		s.SendAsync(cfg, []string{"a@example.com"}, "s"+strconv.Itoa(i), "b")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("SendAsync blocked on a full queue")
	}
	if s.Dropped() == 0 {
		t.Fatal("nothing dropped")
	}
	closeTimeout = 100 * time.Millisecond
	s.Close()
}
