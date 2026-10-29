package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// testDB is a metadata database of the test's own (metatest: SQLite, or
// PostgreSQL with COMMANDCENTER_TEST_POSTGRES_URL).
func testDB(t *testing.T) *meta.DB {
	t.Helper()
	db, err := meta.Open(metatest.Target(t, filepath.Join(t.TempDir(), "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestSessions(t *testing.T) (*Sessions, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s := NewSessions(testDB(t), nil)
	s.now = c.now
	return s, c
}

// create starts a session, failing the test on an error.
func (s *Sessions) create(t *testing.T, owner string) string {
	t.Helper()
	id, err := s.Create(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (s *Sessions) user(id string) (string, bool) { return s.User(context.Background(), id) }

// stored reports whether the session's row exists, without using it.
func (s *Sessions) stored(t *testing.T, id string) bool {
	t.Helper()
	_, err := s.db.GetSession(context.Background(), sessionKey(id))
	if err != nil && !errors.Is(err, meta.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func TestSessionCreateAndUser(t *testing.T) {
	s, _ := newTestSessions(t)
	id := s.create(t, "admin")
	if len(id) != 64 {
		t.Fatalf("id length = %d, want 64 hex chars", len(id))
	}
	if u, ok := s.user(id); !ok || u != "admin" {
		t.Fatalf("User = %q, %v", u, ok)
	}
	if _, ok := s.user("nope"); ok {
		t.Fatal("unknown id accepted")
	}
	if id2 := s.create(t, "admin"); id2 == id {
		t.Fatal("ids must be unique")
	}
	// Only the hash of the id is stored.
	if _, err := s.db.GetSession(context.Background(), id); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("the raw id is a key: %v", err)
	}
}

func TestSessionIdleExpiry(t *testing.T) {
	s, c := newTestSessions(t)
	id := s.create(t, "admin")
	c.advance(11 * time.Hour)
	if _, ok := s.user(id); !ok {
		t.Fatal("session used at 11h should be valid")
	}
	c.advance(11 * time.Hour) // 11h since last use
	if _, ok := s.user(id); !ok {
		t.Fatal("idle refresh did not extend the session")
	}
	c.advance(30 * time.Second) // a use within sessionTouch is not written...
	s.user(id)
	c.advance(12*time.Hour - 29*time.Second) // ...so the idle time counts from the last written one
	if _, ok := s.user(id); ok {
		t.Fatal("session idle for >12h should expire")
	}
	if s.stored(t, id) {
		t.Fatal("expired session not deleted")
	}
}

func TestSessionAbsoluteExpiry(t *testing.T) {
	s, c := newTestSessions(t)
	id := s.create(t, "admin")
	for i := 0; i < 15; i++ { // 165h, used every 11h so never idle
		c.advance(11 * time.Hour)
		if _, ok := s.user(id); !ok {
			t.Fatalf("expired early at step %d", i)
		}
	}
	c.advance(4 * time.Hour) // 169h > 7d, idle only 4h
	if _, ok := s.user(id); ok {
		t.Fatal("session older than 7d must expire despite use")
	}
}

func TestSessionDelete(t *testing.T) {
	s, _ := newTestSessions(t)
	id := s.create(t, "admin")
	if err := s.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.user(id); ok {
		t.Fatal("deleted session still valid")
	}
	if err := s.Delete(context.Background(), "missing"); err != nil {
		t.Fatal(err)
	}
}

// DeleteUser ends an owner's sessions, all of them or those started before
// a time (the moment a user changed, which another node may learn late).
func TestSessionDeleteUser(t *testing.T) {
	s, c := newTestSessions(t)
	ctx := context.Background()
	a1, a2, b, l := s.create(t, "a"), s.create(t, "a"), s.create(t, "b"), s.create(t, "ldap:a")
	if err := s.DeleteUser(ctx, time.Time{}, "a", "ldap:a"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a1, a2, l} {
		if _, ok := s.user(id); ok {
			t.Fatal("a session of a survived")
		}
	}
	if _, ok := s.user(b); !ok {
		t.Fatal("b removed")
	}
	old := s.create(t, "a")
	c.advance(time.Second)
	changed := c.now()
	c.advance(time.Second)
	fresh := s.create(t, "a")
	if err := s.DeleteUser(ctx, changed, "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.user(old); ok {
		t.Fatal("a session started before the change survived")
	}
	if _, ok := s.user(fresh); !ok {
		t.Fatal("a session started after the change was ended")
	}
}

// A login beyond maxPerUser ends the owner's least recently used session,
// never another owner's; the owner's expired sessions go at its next login.
func TestSessionPerUserCap(t *testing.T) {
	s, c := newTestSessions(t)
	var a []string
	for i := 0; i < maxPerUser; i++ {
		a = append(a, s.create(t, "a"))
		c.advance(time.Minute)
	}
	b := s.create(t, "b")
	c.advance(time.Minute)
	s.user(a[0])                    // the oldest, but now the most recently used
	a = append(a, s.create(t, "a")) // a already has maxPerUser: its least recently used (a[1]) goes
	if s.stored(t, a[1]) {
		t.Fatal("a's least recently used session not evicted")
	}
	for i, id := range a {
		if i != 1 && !s.stored(t, id) {
			t.Fatalf("a's more recently used session %d evicted", i)
		}
	}
	if !s.stored(t, b) {
		t.Fatal("another user's session evicted")
	}
	c.advance(13 * time.Hour) // every session idle
	last := s.create(t, "a")
	for i, id := range a {
		if s.stored(t, id) {
			t.Fatalf("a's expired session %d survived its next login", i)
		}
	}
	if !s.stored(t, last) || !s.stored(t, b) {
		t.Fatal("the new session, or another owner's, went")
	}
}

func TestSessionConcurrent(t *testing.T) {
	s := NewSessions(testDB(t), nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				id, err := s.Create(ctx, "u")
				if err != nil {
					t.Error(err)
					return
				}
				s.User(ctx, id)
				if i%3 == 0 {
					s.Delete(ctx, id)
				}
				s.DeleteUser(ctx, time.Time{}, "zzz")
			}
		}()
	}
	wg.Wait()
}

func cookieReq(method, id string) *http.Request {
	r := httptest.NewRequest(method, "/x", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: id})
	return r
}

func TestAuthenticateCookie(t *testing.T) {
	a, _ := newAuth(t)
	a.Sessions = NewSessions(testDB(t), nil)
	id := a.Sessions.create(t, "admin")

	for _, m := range []string{http.MethodGet, http.MethodHead} {
		p, err := a.Authenticate(cookieReq(m, id))
		if err != nil || p.User != "admin" || !p.Admin {
			t.Fatalf("%s: %+v, %v", m, p, err)
		}
	}

	p, err := a.Authenticate(cookieReq(http.MethodPost, id))
	if err != nil || !p.Anonymous() {
		t.Fatalf("POST without header: %+v, %v", p, err)
	}

	r := cookieReq(http.MethodPost, id)
	r.Header.Set(UIHeader, "1")
	if p, err = a.Authenticate(r); err != nil || p.User != "admin" {
		t.Fatalf("POST with header: %+v, %v", p, err)
	}

	r = cookieReq(http.MethodDelete, id)
	r.Header.Set(UIHeader, "0")
	if p, err = a.Authenticate(r); err != nil || !p.Anonymous() {
		t.Fatalf("DELETE with wrong header value: %+v, %v", p, err)
	}
}

func TestAuthenticateCookieUnknownOrRemoved(t *testing.T) {
	a, _ := newAuth(t)
	a.Sessions = NewSessions(testDB(t), nil)

	if p, err := a.Authenticate(cookieReq(http.MethodGet, "deadbeef")); err != nil || !p.Anonymous() {
		t.Fatalf("unknown id: %+v, %v", p, err)
	}
	ghost := a.Sessions.create(t, "ghost") // not in config
	if p, err := a.Authenticate(cookieReq(http.MethodGet, ghost)); err != nil || !p.Anonymous() {
		t.Fatalf("removed user: %+v, %v", p, err)
	}
}

func TestAuthenticateHeaderBeatsCookie(t *testing.T) {
	a, _ := newAuth(t)
	a.Sessions = NewSessions(testDB(t), nil)
	id := a.Sessions.create(t, "admin")

	r := cookieReq(http.MethodGet, id)
	r.SetBasicAuth("ci", "cipw")
	p, err := a.Authenticate(r)
	if err != nil || p.User != "ci" {
		t.Fatalf("got %+v, %v; want ci", p, err)
	}
	r = cookieReq(http.MethodGet, id)
	r.SetBasicAuth("ci", "wrong")
	if _, err = a.Authenticate(r); err == nil {
		t.Fatal("bad Authorization must not fall back to the cookie")
	}
}

// The session cookie is __Host-cc_session under an https baseURL, which
// another host under the same domain cannot set, and cc_session under http;
// only the live scheme's name authenticates.
func TestAuthenticateCookieName(t *testing.T) {
	for _, tc := range []struct{ baseURL, name, other string }{
		{"http://repo.example", SessionCookie, HostSessionCookie},
		{"https://repo.example", HostSessionCookie, SessionCookie},
	} {
		a := New(&config.Config{BaseURL: tc.baseURL, Users: []config.User{{Name: "ci", PasswordHash: hash(t, "cipw")}}}, memTokens{})
		a.Sessions = NewSessions(testDB(t), nil)
		id := a.Sessions.create(t, "ci")
		if got := a.SessionCookieName(); got != tc.name {
			t.Fatalf("%s: cookie name %q, want %q", tc.baseURL, got, tc.name)
		}
		for name, want := range map[string]string{tc.name: "ci", tc.other: ""} {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			r.AddCookie(&http.Cookie{Name: name, Value: id})
			if p, err := a.Authenticate(r); err != nil || p.User != want {
				t.Errorf("%s, cookie %s: %+v %v, want user %q", tc.baseURL, name, p, err, want)
			}
		}
	}
}

// A request may carry several cookies of the session name (on http another
// host under the same domain can add some): unknown ids are skipped, so a
// planted value neither shadows the user's session nor locks the user out,
// and live sessions of two users are refused rather than one picked.
func TestAuthenticateSeveralCookies(t *testing.T) {
	a, _ := newAuth(t)
	a.Sessions = NewSessions(testDB(t), nil)
	ci, ci2, other := a.Sessions.create(t, "ci"), a.Sessions.create(t, "ci"), a.Sessions.create(t, "reader")
	for _, tc := range []struct {
		ids  []string
		want string
	}{
		{[]string{"planted", ci}, "ci"},
		{[]string{ci, "planted"}, "ci"},
		{[]string{ci, ci}, "ci"},
		{[]string{ci, ci2}, "ci"},
		{[]string{other, ci}, ""},
		{[]string{ci, other}, ""},
		{[]string{"planted", "junk"}, ""},
	} {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		for _, id := range tc.ids {
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: id})
		}
		if p, err := a.Authenticate(r); err != nil || p.User != tc.want {
			t.Errorf("%d cookies: %+v %v, want user %q", len(tc.ids), p, err, tc.want)
		}
	}
}

func TestAuthenticateNilSessionsIgnoresCookie(t *testing.T) {
	a, _ := newAuth(t)
	if p, err := a.Authenticate(cookieReq(http.MethodGet, "x")); err != nil || !p.Anonymous() {
		t.Fatalf("%+v, %v", p, err)
	}
}

func TestUserExistsAndLookup(t *testing.T) {
	a, _ := newAuth(t)
	if !a.UserExists("ci") || a.UserExists("nobody") {
		t.Fatal("UserExists wrong")
	}
	p, ok := a.Lookup("ci")
	if !ok || p.User != "ci" || !a.Can(p, "releases", Add) {
		t.Fatalf("Lookup ci = %+v, %v", p, ok)
	}
	if _, ok := a.Lookup("nobody"); ok {
		t.Fatal("Lookup nobody ok")
	}
}

func TestSessionPrincipalVia(t *testing.T) {
	a, _ := newAuth(t)
	a.Sessions = NewSessions(testDB(t), nil)
	p, err := a.Authenticate(cookieReq(http.MethodGet, a.Sessions.create(t, "ci")))
	if err != nil || p.Via != ViaSession {
		t.Fatalf("%+v, %v", p, err)
	}
}

// The idle timeout is asked on every use (the app passes the live
// setting), so a change applies to existing sessions too.
func TestSessionIdleFollowsSetting(t *testing.T) {
	s, c := newTestSessions(t)
	idle := 30 * time.Minute
	s.idle = func() time.Duration { return idle }
	id := s.create(t, "admin")
	c.advance(31 * time.Minute)
	if _, ok := s.user(id); ok {
		t.Fatal("session survived the shortened idle timeout")
	}
	id = s.create(t, "admin")
	idle = 24 * time.Hour
	c.advance(20 * time.Hour)
	if _, ok := s.user(id); !ok {
		t.Fatal("lengthened idle timeout not applied")
	}
	idle = 0
	if s.Idle() != defaultIdle {
		t.Fatalf("idle %v without a setting", s.Idle())
	}
}
