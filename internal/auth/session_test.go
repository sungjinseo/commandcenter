package auth

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
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

func newTestSessions() (*Sessions, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s := NewSessions()
	s.now = c.now
	return s, c
}

func TestSessionCreateAndUser(t *testing.T) {
	s, _ := newTestSessions()
	id := s.Create("admin")
	if len(id) != 64 {
		t.Fatalf("id length = %d, want 64 hex chars", len(id))
	}
	if u, ok := s.User(id); !ok || u != "admin" {
		t.Fatalf("User = %q, %v", u, ok)
	}
	if _, ok := s.User("nope"); ok {
		t.Fatal("unknown id accepted")
	}
	if id2 := s.Create("admin"); id2 == id {
		t.Fatal("ids must be unique")
	}
}

func TestSessionIdleExpiry(t *testing.T) {
	s, c := newTestSessions()
	id := s.Create("admin")
	c.advance(11 * time.Hour)
	if _, ok := s.User(id); !ok {
		t.Fatal("session used at 11h should be valid")
	}
	c.advance(11 * time.Hour) // 11h since last use
	if _, ok := s.User(id); !ok {
		t.Fatal("idle refresh did not extend the session")
	}
	c.advance(12*time.Hour + time.Second)
	if _, ok := s.User(id); ok {
		t.Fatal("session idle for >12h should expire")
	}
	if len(s.m) != 0 {
		t.Fatal("expired session not deleted")
	}
}

func TestSessionAbsoluteExpiry(t *testing.T) {
	s, c := newTestSessions()
	id := s.Create("admin")
	for i := 0; i < 15; i++ { // 165h, used every 11h so never idle
		c.advance(11 * time.Hour)
		if _, ok := s.User(id); !ok {
			t.Fatalf("expired early at step %d", i)
		}
	}
	c.advance(4 * time.Hour) // 169h > 7d, idle only 4h
	if _, ok := s.User(id); ok {
		t.Fatal("session older than 7d must expire despite use")
	}
}

func TestSessionDelete(t *testing.T) {
	s, _ := newTestSessions()
	id := s.Create("admin")
	s.Delete(id)
	if _, ok := s.User(id); ok {
		t.Fatal("deleted session still valid")
	}
	s.Delete("missing") // must not panic
}

func TestSessionDeleteUser(t *testing.T) {
	s, _ := newTestSessions()
	a1, a2, b := s.Create("a"), s.Create("a"), s.Create("b")
	s.DeleteUser("a")
	if _, ok := s.User(a1); ok {
		t.Fatal("a1 survived")
	}
	if _, ok := s.User(a2); ok {
		t.Fatal("a2 survived")
	}
	if _, ok := s.User(b); !ok {
		t.Fatal("b removed")
	}
}

func TestSessionEvictsOldest(t *testing.T) {
	s, c := newTestSessions()
	s.max = 3
	ids := make([]string, 4)
	for i := 0; i < 3; i++ {
		ids[i] = s.Create("u")
		c.advance(time.Minute)
	}
	s.User(ids[0]) // use must not change eviction order (by creation)
	ids[3] = s.Create("u")
	if _, ok := s.User(ids[0]); ok {
		t.Fatal("oldest session not evicted")
	}
	for _, id := range ids[1:] {
		if _, ok := s.User(id); !ok {
			t.Fatal("newer session evicted")
		}
	}
	if len(s.m) != 3 {
		t.Fatalf("store size = %d, want 3", len(s.m))
	}
}

func TestSessionConcurrent(t *testing.T) {
	s := NewSessions()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				id := s.Create("u")
				s.User(id)
				if i%3 == 0 {
					s.Delete(id)
				}
				s.DeleteUser("zzz")
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
	a.Sessions = NewSessions()
	id := a.Sessions.Create("admin")

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
	a.Sessions = NewSessions()

	if p, err := a.Authenticate(cookieReq(http.MethodGet, "deadbeef")); err != nil || !p.Anonymous() {
		t.Fatalf("unknown id: %+v, %v", p, err)
	}
	ghost := a.Sessions.Create("ghost") // not in config
	if p, err := a.Authenticate(cookieReq(http.MethodGet, ghost)); err != nil || !p.Anonymous() {
		t.Fatalf("removed user: %+v, %v", p, err)
	}
}

func TestAuthenticateHeaderBeatsCookie(t *testing.T) {
	a, _ := newAuth(t)
	a.Sessions = NewSessions()
	id := a.Sessions.Create("admin")

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

func TestSessionPerUserCap(t *testing.T) {
	s, c := newTestSessions()
	s.perUser = 3
	var a []string
	for i := 0; i < 3; i++ {
		a = append(a, s.Create("a"))
		c.advance(time.Minute)
	}
	b := s.Create("b")
	c.advance(time.Minute)
	a = append(a, s.Create("a")) // a already has 3: its oldest goes
	if _, ok := s.User(a[0]); ok {
		t.Fatal("a's oldest session not evicted")
	}
	for _, id := range a[1:] {
		if _, ok := s.User(id); !ok {
			t.Fatal("a's newer session evicted")
		}
	}
	if _, ok := s.User(b); !ok {
		t.Fatal("another user's session evicted")
	}
	if n := len(s.m); n != 4 {
		t.Fatalf("store size = %d, want 4", n)
	}
}

func TestSessionPrincipalVia(t *testing.T) {
	a, _ := newAuth(t)
	a.Sessions = NewSessions()
	p, err := a.Authenticate(cookieReq(http.MethodGet, a.Sessions.Create("ci")))
	if err != nil || p.Via != ViaSession {
		t.Fatalf("%+v, %v", p, err)
	}
}

func TestSessionSetIdle(t *testing.T) {
	s, c := newTestSessions()
	id := s.Create("admin")
	s.SetIdle(30 * time.Minute)
	c.advance(31 * time.Minute)
	if _, ok := s.User(id); ok {
		t.Fatal("session survived the shortened idle timeout")
	}
	id = s.Create("admin")
	s.SetIdle(0) // ignored
	s.SetIdle(24 * time.Hour)
	c.advance(20 * time.Hour)
	if _, ok := s.User(id); !ok {
		t.Fatal("lengthened idle timeout not applied")
	}
}
