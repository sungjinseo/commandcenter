package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/meta"
)

type sent struct {
	cfg           meta.SMTP
	to            []string
	subject, body string
}

type fakeMailer struct {
	mu  sync.Mutex
	got []sent
}

func (f *fakeMailer) SendAsync(cfg meta.SMTP, to []string, subject, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, sent{cfg, to, subject, body})
}

func (f *fakeMailer) all() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.got...)
}

func enableSMTP(t *testing.T, h *harness, lang string) {
	t.Helper()
	ctx := context.Background()
	if err := h.db.SetSettingJSON(ctx, meta.SettingSMTP, meta.SMTP{Host: "smtp.example.com", Port: 25, Security: "none",
		From: "cc@example.com", Enabled: true, Password: "hunter2-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetSettingJSON(ctx, meta.SettingDefaultLanguage, lang); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyEmailValidationAndRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tk := h.create(t, Def{Name: "a", Type: TypeCompact, Schedule: "manual"})
	if tk.NotifyEmail == nil || len(tk.NotifyEmail) != 0 {
		t.Fatalf("default notifyEmail = %#v, want []", tk.NotifyEmail)
	}
	if _, err := h.s.Create(ctx, Def{Name: "b", Type: TypeCompact, Schedule: "manual", NotifyEmail: []string{"ok@example.com", "nope"}}); err == nil {
		t.Fatal("bad address accepted")
	}
	many := make([]string, MaxNotifyEmail+1)
	for i := range many {
		many[i] = fmt.Sprintf("u%d@example.com", i)
	}
	if _, err := h.s.Create(ctx, Def{Name: "b", Type: TypeCompact, Schedule: "manual", NotifyEmail: many}); err == nil {
		t.Fatal("too many addresses accepted")
	}
	tk2 := h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "manual", NotifyEmail: []string{" ops@example.com ", "Dev <dev@example.com>"}})
	if got := strings.Join(tk2.NotifyEmail, ","); got != "ops@example.com,Dev <dev@example.com>" {
		t.Fatalf("stored %q", got)
	}
	// An update that omits notifyEmail keeps it; [] clears it; a bad one is refused.
	up, err := h.s.Update(ctx, tk2.ID, Def{Name: "c2"})
	if err != nil || len(up.NotifyEmail) != 2 {
		t.Fatalf("omitted: %v %v", up.NotifyEmail, err)
	}
	if _, err := h.s.Update(ctx, tk2.ID, Def{NotifyEmail: []string{"bad"}}); err == nil {
		t.Fatal("bad address accepted on update")
	}
	up, err = h.s.Update(ctx, tk2.ID, Def{NotifyEmail: []string{}})
	if err != nil || len(up.NotifyEmail) != 0 {
		t.Fatalf("cleared: %v %v", up.NotifyEmail, err)
	}
	up, err = h.s.Update(ctx, tk2.ID, Def{NotifyEmail: []string{"x@example.com"}})
	if err != nil || len(up.NotifyEmail) != 1 || h.get(t, tk2.ID).NotifyEmail[0] != "x@example.com" {
		t.Fatalf("set: %v %v", up.NotifyEmail, err)
	}
}

func TestFailedRunSendsOneMail(t *testing.T) {
	for _, lang := range []string{"en", "ko"} {
		t.Run(lang, func(t *testing.T) {
			h := newHarness(t)
			enableSMTP(t, h, lang)
			m := &fakeMailer{}
			h.s.Mail = m
			h.s.GC = func(context.Context, string) (int, int64, error) { return 0, 0, errors.New("disk on fire") }
			tk := h.create(t, Def{Name: "Nightly compact", Type: TypeCompact, Schedule: "manual", NotifyEmail: []string{"ops@example.com"}})
			out, err := h.s.RunNow(tk.ID, "admin")
			if err != nil || out.Result != ResultFailed {
				t.Fatalf("run: %+v %v", out, err)
			}
			got := m.all()
			if len(got) != 1 {
				t.Fatalf("mails: %d", len(got))
			}
			g := got[0]
			if len(g.to) != 1 || g.to[0] != "ops@example.com" || g.cfg.Host != "smtp.example.com" {
				t.Fatalf("to %v cfg %+v", g.to, g.cfg)
			}
			if !strings.Contains(g.subject, "Nightly compact") || !strings.Contains(g.body, "Nightly compact") ||
				!strings.Contains(g.body, "disk on fire") || !strings.Contains(g.body, "compact failed") ||
				!strings.Contains(g.body, "2026-10-20") {
				t.Fatalf("subject %q body %q", g.subject, g.body)
			}
			if lang == "ko" && !strings.Contains(g.subject, "실패") {
				t.Fatalf("ko subject %q", g.subject)
			}
			if lang == "en" && !strings.Contains(g.subject, "failed") {
				t.Fatalf("en subject %q", g.subject)
			}
			if strings.Contains(g.body, "hunter2-secret") || strings.Contains(g.subject, "hunter2-secret") {
				t.Fatal("password in body")
			}
		})
	}
}

func TestMailBodyKeepsLast20LogLines(t *testing.T) {
	lines := make([]string, 50)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%02d", i)
	}
	body := failureBody("en", "T", time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC), "msg", strings.Join(lines, "\n")+"\n")
	if strings.Contains(body, "line-29") || !strings.Contains(body, "line-30") || !strings.Contains(body, "line-49") {
		t.Fatalf("body %q", body)
	}
}

func TestNoMailForOKStoppedDisabledOrNoRecipients(t *testing.T) {
	h := newHarness(t)
	m := &fakeMailer{}
	h.s.Mail = m
	notify := []string{"ops@example.com"}

	// OK run.
	enableSMTP(t, h, "en")
	ok := h.create(t, Def{Name: "ok", Type: TypeCompact, Schedule: "manual", NotifyEmail: notify})
	if out, _ := h.s.RunNow(ok.ID, "admin"); out.Result != ResultOK {
		t.Fatalf("%+v", out)
	}

	// Stopped run.
	started, release := make(chan struct{}), make(chan struct{})
	h.s.GC = func(ctx context.Context, _ string) (int, int64, error) {
		close(started)
		<-ctx.Done()
		<-release
		return 0, 0, ctx.Err()
	}
	stopped := h.create(t, Def{Name: "stopped", Type: TypeCompact, Schedule: "manual", NotifyEmail: notify})
	if err := h.s.RunAsync(stopped.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	<-started
	h.s.StopRun(stopped.ID)
	close(release)
	waitIdle(t, h.s)

	// Failed, but no recipients / smtp disabled.
	h.s.GC = func(context.Context, string) (int, int64, error) { return 0, 0, errors.New("boom") }
	norcpt := h.create(t, Def{Name: "norcpt", Type: TypeCompact, Schedule: "manual"})
	if out, _ := h.s.RunNow(norcpt.ID, "admin"); out.Result != ResultFailed {
		t.Fatalf("%+v", out)
	}
	if err := h.db.SetSettingJSON(context.Background(), meta.SettingSMTP, meta.SMTP{Host: "h", Port: 25, Security: "none", From: "a@b.com"}); err != nil {
		t.Fatal(err)
	}
	dis := h.create(t, Def{Name: "dis", Type: TypeCompact, Schedule: "manual", NotifyEmail: notify})
	if out, _ := h.s.RunNow(dis.ID, "admin"); out.Result != ResultFailed {
		t.Fatalf("%+v", out)
	}
	if got := m.all(); len(got) != 0 {
		t.Fatalf("unexpected mail: %+v", got)
	}
	// A nil mailer is tolerated.
	h.s.Mail = nil
	enableSMTP(t, h, "en")
	if out, _ := h.s.RunNow(dis.ID, "admin"); out.Result != ResultFailed {
		t.Fatalf("%+v", out)
	}
}

func waitIdle(t *testing.T, s *Service) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if s.Running() == "" {
			// execute holds runMu a moment longer than running is set
			if s.runMu.TryLock() {
				s.runMu.Unlock()
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run did not finish")
}

type blockingMailer struct{ release chan struct{} }

func (b *blockingMailer) SendAsync(meta.SMTP, []string, string, string) { <-b.release }

func TestNoRecipientsNeverTouchesMailer(t *testing.T) {
	// A task without recipients must not even touch the mailer.
	h := newHarness(t)
	h.s.Mail = &blockingMailer{release: make(chan struct{})}
	h.s.GC = func(context.Context, string) (int, int64, error) { return 0, 0, errors.New("boom") }
	enableSMTP(t, h, "en")
	tk := h.create(t, Def{Name: "x", Type: TypeCompact, Schedule: "manual"})
	done := make(chan struct{})
	go func() { h.s.RunNow(tk.ID, "admin"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run blocked")
	}
}
