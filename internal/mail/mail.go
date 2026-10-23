// Package mail sends e-mail over SMTP (plan 12 Decision 6). SendNow is the
// synchronous send; SendAsync queues a message for one background goroutine
// and never blocks its caller, like the activity event recorder.
package mail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"commandcenter/internal/events"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

const (
	// QueueSize is the capacity of the queue between SendAsync and the worker.
	QueueSize = 256
	// SendTimeout bounds one send, connect to quit.
	SendTimeout = 30 * time.Second

	warnEvery = time.Minute
	maxDetail = 300
)

// closeTimeout bounds how long Close waits for the worker; a var for tests.
var closeTimeout = 10 * time.Second

type job struct {
	cfg           meta.SMTP
	to            []string
	subject, body string
}

// Sender sends mail. The zero value is not usable: use New.
type Sender struct {
	log    *slog.Logger
	events repo.EventSink // failed sends are recorded here; may be nil

	timeout time.Duration  // per send; tests shorten it
	rootCAs *x509.CertPool // nil: the system roots; tests trust their own CA

	mu     sync.RWMutex // guards closed and sends on ch
	closed bool
	ch     chan job
	done   chan struct{}

	dropped  atomic.Int64
	lastWarn atomic.Int64 // unix nanos of the last queue-full warning
	abort    atomic.Bool  // Close gave up waiting: the worker discards the rest
}

// New starts a Sender; Close stops it.
func New(log *slog.Logger, ev repo.EventSink) *Sender {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Sender{log: log, events: ev, timeout: SendTimeout, ch: make(chan job, QueueSize), done: make(chan struct{})}
	go s.run()
	return s
}

// Dropped is the number of messages lost: the queue was full or the sender closed.
func (s *Sender) Dropped() int64 { return s.dropped.Load() }

// SendAsync queues a message and returns at once. When the queue is full or
// the sender is closed the message is dropped and counted.
func (s *Sender) SendAsync(cfg meta.SMTP, to []string, subject, body string) {
	j := job{cfg: cfg, to: append([]string(nil), to...), subject: subject, body: body}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		s.dropped.Add(1)
		return
	}
	select {
	case s.ch <- j:
	default:
		n := s.dropped.Add(1)
		now := time.Now().UnixNano()
		if last := s.lastWarn.Load(); now-last >= int64(warnEvery) && s.lastWarn.CompareAndSwap(last, now) {
			s.log.Warn("e-mail queue is full: messages are being dropped", "dropped", n)
		}
	}
}

// Close stops accepting messages, sends what is queued and waits for the
// worker (bounded). It is safe to call more than once.
func (s *Sender) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
	s.mu.Unlock()
	select {
	case <-s.done:
	case <-time.After(closeTimeout):
		s.abort.Store(true)
		s.log.Warn("e-mail was not fully sent before the shutdown deadline")
	}
}

func (s *Sender) run() {
	defer close(s.done)
	for j := range s.ch {
		if s.abort.Load() {
			s.dropped.Add(1)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		err := s.SendNow(ctx, j.cfg, j.to, j.subject, j.body)
		cancel()
		if err == nil {
			continue
		}
		msg := clip(logsafe.URLs(err.Error()), maxDetail)
		s.log.Warn("could not send e-mail", "subject", clip(oneLine(j.subject), 200), "err", msg)
		if s.events != nil {
			s.events.Emit("email.failed", events.System, "", clip(oneLine(j.subject), 200), msg)
		}
	}
}

// SendNow sends one message with the SMTP server cfg describes and returns
// the first error. TLS is always verified. It does not check cfg.Enabled.
func (s *Sender) SendNow(ctx context.Context, cfg meta.SMTP, to []string, subject, body string) (err error) {
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return errors.New("invalid from address")
	}
	rcpts := make([]*mail.Address, 0, len(to))
	for _, a := range to {
		r, err := mail.ParseAddress(a)
		if err != nil {
			return fmt.Errorf("invalid recipient address %q", clip(a, 100))
		}
		rcpts = append(rcpts, r)
	}
	if len(rcpts) == 0 {
		return errors.New("no recipients")
	}
	switch cfg.Security {
	case meta.SMTPNone, meta.SMTPStartTLS, meta.SMTPTLS:
	default:
		return fmt.Errorf("unknown security mode %q", clip(cfg.Security, 20))
	}
	if cfg.Host == "" || cfg.Port < 1 || cfg.Port > 65535 {
		return errors.New("smtp host or port is not set")
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	tlsConf := &tls.Config{ServerName: cfg.Host, RootCAs: s.rootCAs, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	d := net.Dialer{}
	if cfg.Security == meta.SMTPTLS {
		td := tls.Dialer{NetDialer: &d, Config: tlsConf}
		conn, err = td.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	// Closing the connection unblocks a stuck read or write on cancellation.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("greeting: %w", err)
	}
	defer c.Close()
	if cfg.Security == meta.SMTPStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("the server does not offer STARTTLS")
		}
		if err := c.StartTLS(tlsConf); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r.Address); err != nil {
			return fmt.Errorf("rcpt to: %w", err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write(buildMessage(from, rcpts, subject, body, time.Now())); err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("data: %w", err)
	}
	c.Quit()
	return nil
}

// buildMessage composes an RFC 5322 text/plain message. The subject is one
// encoded line (no header injection) and the body is base64, so any text,
// dot-lines included, survives.
func buildMessage(from *mail.Address, to []*mail.Address, subject, body string, now time.Time) []byte {
	var b strings.Builder
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", from.String())
	list := make([]string, len(to))
	for i, a := range to {
		list[i] = a.String()
	}
	h("To", strings.Join(list, ", "))
	h("Subject", mime.QEncoding.Encode("utf-8", oneLine(subject)))
	h("Date", now.Format(time.RFC1123Z))
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "base64")
	b.WriteString("\r\n")
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(body))
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return []byte(b.String())
}

// oneLine replaces line breaks and other control characters with spaces.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}
