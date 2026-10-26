package ui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo/repotest"
)

type fakeMail struct {
	mu   sync.Mutex
	got  []string
	cfg  meta.SMTP
	err  error
	subj string
	body string
}

func (f *fakeMail) SendNow(_ context.Context, cfg meta.SMTP, to []string, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, strings.Join(to, ","))
	f.cfg, f.subj, f.body = cfg, subject, body
	return f.err
}

func (f *fakeMail) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.got) }

const smtpBody = `{"host":"smtp.example.com","port":587,"security":"starttls","username":"mailer","password":"hunter2-secret","from":"cc@example.com","enabled":true}`

func TestSMTPGetDefault(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.admin(t, "GET", "/-/api/settings/smtp", "")
	wantStatus(t, r, http.StatusOK)
	m := r.json(t)
	if m["enabled"] != false || m["passwordSet"] != false || m["host"] != "" {
		t.Fatalf("default: %s", r.body)
	}
	if _, ok := m["password"]; ok {
		t.Fatalf("password key present: %s", r.body)
	}
}

func TestSMTPGates(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	e.ui.Mail = &fakeMail{}
	wantStatus(t, e.req(t, "GET", "/-/api/settings/smtp", "", "", nil, false), http.StatusUnauthorized)
	wantStatus(t, e.get(t, "/-/api/settings/smtp", "ci"), http.StatusForbidden)
	for _, p := range []struct{ method, path, body string }{
		{"PUT", "/-/api/settings/smtp", smtpBody},
		{"POST", "/-/api/settings/smtp/test", `{"to":"a@example.com"}`},
	} {
		wantStatus(t, e.req(t, p.method, p.path, p.body, "admin", nil, false), http.StatusForbidden) // no UI header
		wantStatus(t, e.req(t, p.method, p.path, p.body, "ci", nil, true), http.StatusForbidden)
		wantStatus(t, e.req(t, p.method, p.path, p.body, "", nil, true), http.StatusUnauthorized)
		_, tok := e.newToken(t, "admin", "")
		r, _ := http.NewRequest(p.method, e.ts.URL+p.path, strings.NewReader(p.body))
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Header.Set("X-Command-Center-UI", "1")
		res, err := noRedirect.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("token %s %s: %d", p.method, p.path, res.StatusCode)
		}
	}
	if e.admin(t, "GET", "/-/api/settings/smtp", "").json(t)["enabled"] != false {
		t.Fatal("a rejected PUT changed the settings")
	}
	// A token may read.
	_, tok := e.newToken(t, "admin", "")
	if st := e.bearer(t, "/-/api/settings/smtp", tok); st != http.StatusOK {
		t.Fatalf("token read: %d", st)
	}
}

func TestSMTPPutSecretHandling(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	sink := &repotest.Sink{}
	e.ui.Events = sink
	r := e.admin(t, "PUT", "/-/api/settings/smtp", smtpBody)
	wantStatus(t, r, http.StatusOK)
	if strings.Contains(r.body, "hunter2") || r.json(t)["passwordSet"] != true || r.json(t)["host"] != "smtp.example.com" {
		t.Fatalf("put: %s", r.body)
	}
	stored := func() meta.SMTP {
		s, err := e.db.LoadSettings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return s.SMTP
	}
	if stored().Password != "hunter2-secret" {
		t.Fatal("password not stored")
	}
	g := e.admin(t, "GET", "/-/api/settings/smtp", "")
	if strings.Contains(g.body, "hunter2") || g.json(t)["passwordSet"] != true || g.json(t)["username"] != "mailer" {
		t.Fatalf("get: %s", g.body)
	}
	// The general settings and other APIs never carry it either.
	if body := e.admin(t, "GET", "/-/api/settings", "").body; strings.Contains(body, "hunter2") || strings.Contains(body, "smtp") {
		t.Fatalf("general settings: %s", body)
	}

	// Empty password keeps the stored one.
	r = e.admin(t, "PUT", "/-/api/settings/smtp", `{"host":"SMTP.example.com","port":587,"security":"starttls","username":"mailer2","password":"","from":"cc@example.com","enabled":true}`)
	wantStatus(t, r, http.StatusOK)
	if got := stored(); got.Password != "hunter2-secret" || got.Username != "mailer2" || got.Port != 587 {
		t.Fatalf("kept: %+v", got)
	}
	// A new password replaces it.
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings/smtp", `{"host":"smtp2.example.com","port":25,"security":"none","password":"new-pw-777","from":"cc@example.com","enabled":true}`), http.StatusOK)
	if stored().Password != "new-pw-777" {
		t.Fatalf("replaced: %+v", stored())
	}
	// clearPassword blanks it.
	r = e.admin(t, "PUT", "/-/api/settings/smtp", `{"host":"smtp2.example.com","port":25,"security":"none","clearPassword":true,"from":"cc@example.com","enabled":true}`)
	wantStatus(t, r, http.StatusOK)
	if stored().Password != "" || r.json(t)["passwordSet"] != false {
		t.Fatalf("cleared: %+v %s", stored(), r.body)
	}
	// The live snapshot follows (GET reads it).
	if e.admin(t, "GET", "/-/api/settings/smtp", "").json(t)["host"] != "smtp2.example.com" {
		t.Fatal("snapshot not reloaded")
	}
	// Events never carry a password.
	evs := sink.Events()
	if len(evs) != 4 {
		t.Fatalf("events: %v", evs)
	}
	for _, ev := range evs {
		if ev.Kind != "settings.smtp" || strings.Contains(ev.Detail+ev.Subject, "hunter2") || strings.Contains(ev.Detail+ev.Subject, "new-pw-777") {
			t.Fatalf("event: %+v", ev)
		}
	}
	if !strings.Contains(evs[0].Detail, "host") || !strings.Contains(evs[0].Detail, "password") {
		t.Fatalf("detail should name changed keys: %q", evs[0].Detail)
	}
	if strings.Contains(evs[1].Detail, "password") {
		t.Fatalf("unchanged password listed: %q", evs[1].Detail)
	}
}

func TestSMTPPutValidation(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	for _, body := range []string{
		`{"host":"","port":587,"security":"starttls","from":"cc@example.com","enabled":true}`,
		`{"host":"h","port":0,"security":"starttls","from":"cc@example.com","enabled":true}`,
		`{"host":"h","port":587,"security":"ssl","from":"cc@example.com","enabled":true}`,
		`{"host":"h","port":587,"security":"tls","from":"nope","enabled":true}`,
		// QA N-17: no CR/LF, control character or space in the host, enabled or not
		`{"host":"smtp.example.com\r\nRCPT TO:<x@q.r>","port":587,"security":"starttls","from":"cc@example.com","enabled":true}`,
		`{"host":"smtp.example.com\nx","enabled":false}`,
		`{"host":"smtp example.com","enabled":false}`,
	} {
		r := e.admin(t, "PUT", "/-/api/settings/smtp", body)
		wantStatus(t, r, http.StatusBadRequest)
		m := r.json(t)
		if m["error"] == nil || m["errors"] == nil || strings.Contains(r.body, "hunter2") {
			t.Fatalf("body %s", r.body)
		}
	}
	// A disabled, partial config is fine.
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings/smtp", `{"host":"h","enabled":false}`), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings/smtp", `{"bogus":1}`), http.StatusBadRequest)
}

func TestSMTPTestEndpoint(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	fm := &fakeMail{}
	e.ui.Mail = fm
	sink := &repotest.Sink{}
	e.ui.Events = sink
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	e.ui.now = func() time.Time { return now }

	// Disabled: 400, nothing sent, and the rate-limit window is not consumed.
	r := e.admin(t, "POST", "/-/api/settings/smtp/test", `{"to":"a@example.com"}`)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "disabled") || fm.count() != 0 {
		t.Fatalf("disabled: %s", r.body)
	}
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings/smtp", smtpBody), http.StatusOK)
	wantStatus(t, e.admin(t, "POST", "/-/api/settings/smtp/test", `{"to":"not an address"}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "POST", "/-/api/settings/smtp/test", `{}`), http.StatusBadRequest)

	r = e.admin(t, "POST", "/-/api/settings/smtp/test", `{"to":"a@example.com"}`)
	wantStatus(t, r, http.StatusOK)
	if r.json(t)["ok"] != true || fm.count() != 1 {
		t.Fatalf("test: %s", r.body)
	}
	fm.mu.Lock()
	if fm.cfg.Password != "hunter2-secret" || fm.cfg.Host != "smtp.example.com" {
		t.Fatalf("test used cfg %+v, want the saved one with its password", fm.cfg)
	}
	fm.mu.Unlock()

	// Within 10 s: 429.
	now = now.Add(9 * time.Second)
	r = e.admin(t, "POST", "/-/api/settings/smtp/test", `{"to":"a@example.com"}`)
	wantStatus(t, r, http.StatusTooManyRequests)
	if r.json(t)["error"] != "please wait before testing again" || fm.count() != 1 {
		t.Fatalf("limited: %s", r.body)
	}
	// After the window: allowed; a failing send gives 502 with a non-secret summary.
	now = now.Add(2 * time.Second)
	fm.err = errors.New("connect: dial tcp 10.0.0.1:587: connection refused")
	r = e.admin(t, "POST", "/-/api/settings/smtp/test", `{"to":"a@example.com"}`)
	wantStatus(t, r, http.StatusBadGateway)
	if !strings.Contains(r.body, "connection refused") || strings.Contains(r.body, "hunter2") {
		t.Fatalf("failure: %s", r.body)
	}
	kinds := ""
	for _, ev := range sink.Events() {
		kinds += ev.Kind + ","
		if strings.Contains(ev.Detail, "hunter2") {
			t.Fatalf("secret in event %+v", ev)
		}
	}
	if !strings.Contains(kinds, "settings.smtp.test") {
		t.Fatalf("events: %s", kinds)
	}
}

func TestSMTPTestNoMailer(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings/smtp", smtpBody), http.StatusOK)
	wantStatus(t, e.admin(t, "POST", "/-/api/settings/smtp/test", `{"to":"a@example.com"}`), http.StatusNotImplemented)
}

func TestTaskNotifyEmailAPI(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	r := e.admin(t, "POST", "/-/api/tasks", `{"name":"N","type":"blobstore.compact","schedule":"manual","notifyEmail":["ops@example.com"]}`)
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	if got, _ := m["notifyEmail"].([]any); len(got) != 1 || got[0] != "ops@example.com" {
		t.Fatalf("create: %s", r.body)
	}
	id := m["id"].(string)
	wantStatus(t, e.admin(t, "POST", "/-/api/tasks", `{"name":"B","type":"blobstore.compact","schedule":"manual","notifyEmail":["bad"]}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "PUT", "/-/api/tasks/"+id, `{"notifyEmail":["bad"]}`), http.StatusBadRequest)
	r = e.admin(t, "PUT", "/-/api/tasks/"+id, `{"name":"N2"}`)
	wantStatus(t, r, http.StatusOK)
	if got, _ := r.json(t)["notifyEmail"].([]any); len(got) != 1 {
		t.Fatalf("kept: %s", r.body)
	}
	r = e.admin(t, "PUT", "/-/api/tasks/"+id, `{"notifyEmail":[]}`)
	wantStatus(t, r, http.StatusOK)
	if got, ok := r.json(t)["notifyEmail"].([]any); !ok || len(got) != 0 {
		t.Fatalf("cleared: %s", r.body)
	}
	// The seeded task (backward compat) lists [] rather than null.
	list := e.admin(t, "GET", "/-/api/tasks", "")
	if strings.Contains(list.body, `"notifyEmail":null`) || !strings.Contains(list.body, `"notifyEmail":[]`) {
		t.Fatalf("list: %s", list.body)
	}
}

// The stored password never follows the configuration to another server or
// over a weaker transport without being re-entered (I2).
func TestSMTPPasswordNotCarriedToAnotherServer(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings/smtp", smtpBody), http.StatusOK)
	const msg = "re-enter the password when changing the server or weakening TLS"
	for name, body := range map[string]string{
		"host":          `{"host":"evil.example.net","port":587,"security":"starttls","from":"cc@example.com","enabled":true}`,
		"port":          `{"host":"smtp.example.com","port":2525,"security":"starttls","from":"cc@example.com","enabled":true}`,
		"starttls→none": `{"host":"smtp.example.com","port":587,"security":"none","from":"cc@example.com","enabled":true}`,
	} {
		r := e.admin(t, "PUT", "/-/api/settings/smtp", body)
		if r.status != http.StatusBadRequest || r.json(t)["error"] != msg {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	s, _ := e.db.LoadSettings(context.Background())
	if s.SMTP.Host != "smtp.example.com" || s.SMTP.Password != "hunter2-secret" {
		t.Fatalf("changed: %v", s.SMTP)
	}
	// starttls -> tls is not weaker; a re-entered or cleared password may move.
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings/smtp", `{"host":"smtp.example.com","port":587,"security":"tls","from":"cc@example.com","enabled":true}`), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings/smtp", `{"host":"smtp.example.com","port":587,"security":"none","from":"cc@example.com","enabled":true}`), http.StatusBadRequest) // tls -> none
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings/smtp", `{"host":"other.example.com","port":25,"security":"none","password":"typed-again","from":"cc@example.com","enabled":true}`), http.StatusOK)
	wantStatus(t, e.admin(t, "PUT", "/-/api/settings/smtp", `{"host":"third.example.com","port":25,"security":"none","clearPassword":true,"from":"cc@example.com","enabled":true}`), http.StatusOK)
}
