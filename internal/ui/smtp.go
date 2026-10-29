package ui

import (
	"context"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
)

// SMTP settings (plan 12 Task 5). Reads need an admin; a change and the test
// send need an interactive admin plus the UI header. The password is a
// secret: it is accepted on PUT (empty keeps the stored one, clearPassword
// blanks it) and never returned, logged or put in an event.

// Mailer sends one message at once (mail.Sender).
type Mailer interface {
	SendNow(ctx context.Context, cfg meta.SMTP, to []string, subject, body string) error
}

const (
	smtpTestInterval = 10 * time.Second
	smtpTestTimeout  = 30 * time.Second
)

// smtpTestState rate-limits the test endpoint.
type smtpTestState struct {
	mu   sync.Mutex
	last time.Time
}

func (u *UI) smtpRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/settings/smtp", u.getSMTP)
	mux.HandleFunc("PUT /-/api/settings/smtp", u.putSMTP)
	mux.HandleFunc("POST /-/api/settings/smtp/test", u.testSMTP)
}

// smtpDTO is the response: everything but the password.
type smtpDTO struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Security    string `json:"security"`
	Username    string `json:"username"`
	From        string `json:"from"`
	Enabled     bool   `json:"enabled"`
	PasswordSet bool   `json:"passwordSet"`
}

func smtpJSON(c meta.SMTP) smtpDTO {
	return smtpDTO{Host: c.Host, Port: c.Port, Security: c.Security, Username: c.Username, From: c.From,
		Enabled: c.Enabled, PasswordSet: c.Password != ""}
}

type smtpRequest struct {
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Security      string `json:"security"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	ClearPassword bool   `json:"clearPassword"`
	From          string `json:"from"`
	Enabled       bool   `json:"enabled"`
}

func (u *UI) getSMTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, smtpJSON(u.Auth.Snapshot().Settings().SMTP))
}

// smtpReenterMessage is matched by settings.js (REENTER); keep them in step.
const smtpReenterMessage = "re-enter the password when changing the server or weakening TLS"

func (u *UI) putSMTP(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body smtpRequest
	if !decodeBody(w, r, &body) {
		return
	}
	// Merge into the stored value, not the snapshot, so the password that
	// is kept is the stored one.
	var cur meta.SMTP
	if _, err := u.DB.GetSettingJSON(r.Context(), meta.SettingSMTP, &cur); err != nil {
		u.fail(w, r, err)
		return
	}
	next := meta.SMTP{
		Host: strings.TrimSpace(body.Host), Port: body.Port, Security: strings.TrimSpace(body.Security),
		Username: body.Username, From: strings.TrimSpace(body.From), Enabled: body.Enabled,
		Password: cur.Password,
	}
	switch {
	case body.ClearPassword:
		next.Password = ""
	case body.Password != "":
		next.Password = body.Password
	}
	if next.Security == "" {
		next.Security = meta.SMTPNone
	}
	// The stored password never follows the configuration to another server
	// or onto an unencrypted connection: the admin must type it again.
	if body.Password == "" && !body.ClearPassword && cur.Password != "" && !smtpSameTrust(cur, next) {
		writeError(w, http.StatusBadRequest, smtpReenterMessage)
		return
	}
	if err := meta.ValidateSMTP(next); err != nil {
		var v validation
		v.add("%s", err.Error())
		u.writeAccessError(w, r, v.err())
		return
	}
	if err := u.DB.SetSettingJSON(r.Context(), meta.SettingSMTP, next); err != nil {
		u.fail(w, r, err)
		return
	}
	if err := u.reloadAccess(r.Context()); err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "settings.smtp", "", "", smtpChanged(cur, next))
	writeJSON(w, http.StatusOK, smtpJSON(next))
}

// smtpChanged names the keys that differ, never their values.
func smtpChanged(a, b meta.SMTP) string {
	var keys []string
	add := func(changed bool, k string) {
		if changed {
			keys = append(keys, k)
		}
	}
	add(a.Host != b.Host, "host")
	add(a.Port != b.Port, "port")
	add(a.Security != b.Security, "security")
	add(a.Username != b.Username, "username")
	add(a.Password != b.Password, "password")
	add(a.From != b.From, "from")
	add(a.Enabled != b.Enabled, "enabled")
	return strings.Join(keys, ", ")
}

// takeSMTPTest claims the test window: false while one is still open.
func (u *UI) takeSMTPTest() bool {
	u.smtpTest.mu.Lock()
	defer u.smtpTest.mu.Unlock()
	now := u.clock()
	if !u.smtpTest.last.IsZero() && now.Sub(u.smtpTest.last) < smtpTestInterval {
		return false
	}
	u.smtpTest.last = now
	return true
}

func (u *UI) testSMTP(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body struct {
		To string `json:"to"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	to := strings.TrimSpace(body.To)
	if _, err := mail.ParseAddress(to); err != nil {
		writeError(w, http.StatusBadRequest, "to must be a valid e-mail address")
		return
	}
	if u.Mail == nil {
		writeError(w, http.StatusNotImplemented, "e-mail is not available")
		return
	}
	// The saved configuration, read fresh: it carries the password.
	set, err := u.DB.LoadSettings(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	if !set.SMTP.Enabled {
		writeError(w, http.StatusBadRequest, "e-mail is disabled")
		return
	}
	if !u.takeSMTPTest() {
		writeError(w, http.StatusTooManyRequests, "please wait before testing again")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), smtpTestTimeout)
	defer cancel()
	subject := "[Command Center] Test message / 테스트 메시지"
	text := "This is a test message from Command Center.\nCommand Center에서 보낸 테스트 메시지입니다.\n"
	if err := u.Mail.SendNow(ctx, set.SMTP, []string{to}, subject, text); err != nil {
		msg := logsafe.URLs(err.Error())
		if runes := []rune(msg); len(runes) > 300 {
			msg = string(runes[:300])
		}
		u.emit(r, "settings.smtp.test", "", to, "failed: "+msg)
		writeError(w, http.StatusBadGateway, "could not send the test message: "+msg)
		return
	}
	u.emit(r, "settings.smtp.test", "", to, "sent")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// smtpSameTrust reports whether the stored password may stay with next:
// the same host and port, and still encrypted if it was (tls or starttls
// to none is weaker).
func smtpSameTrust(cur, next meta.SMTP) bool {
	if !strings.EqualFold(strings.TrimSpace(cur.Host), next.Host) || cur.Port != next.Port {
		return false
	}
	return !(cur.Security != meta.SMTPNone && cur.Security != "" && next.Security == meta.SMTPNone)
}
