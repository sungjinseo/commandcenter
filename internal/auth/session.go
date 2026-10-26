package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

const (
	// SessionCookie is the name of the UI session cookie under an http
	// baseURL.
	SessionCookie = "cc_session"
	// HostSessionCookie is its name under an https baseURL, where the cookie
	// is Secure. Browsers take a __Host- cookie only from the host itself
	// (Secure, Path=/, no Domain), so another host under the same domain
	// cannot plant or shadow it.
	HostSessionCookie = "__Host-" + SessionCookie
	// UIHeader must be "1" for a cookie to authenticate a state-changing request.
	UIHeader = "X-Command-Center-UI"
)

type session struct {
	user    string
	created time.Time
	last    time.Time
}

// Sessions is an in-memory session store. Sessions do not survive a restart.
// Entries are keyed by the SHA-256 of the session id, so the raw ids never sit
// in the map.
type Sessions struct {
	mu       sync.Mutex // not an RWMutex: User refreshes the idle time
	m        map[string]*session
	now      func() time.Time
	max      int
	perUser  int
	idle     time.Duration
	absolute time.Duration
}

// maxPerUser caps the sessions one user may hold.
const maxPerUser = 20

// NewSessions returns a store holding at most 1000 sessions (20 per user) with
// a 12h idle and 7d absolute lifetime.
func NewSessions() *Sessions {
	return &Sessions{
		m:        map[string]*session{},
		now:      time.Now,
		max:      1000,
		perUser:  maxPerUser,
		idle:     12 * time.Hour,
		absolute: 7 * 24 * time.Hour,
	}
}

func sessionKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// Create starts a session for user and returns its id, after making room
// (makeRoomLocked).
func (s *Sessions) Create(user string) string {
	id := randomHex(32)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.makeRoomLocked(user, now)
	s.m[sessionKey(id)] = &session{user: user, created: now, last: now}
	return id
}

// makeRoomLocked makes room for one more session of user, in one O(n) scan:
// expired sessions go; then, when user is at the per-user cap or the store
// is full, user's own least recently used session; and when the store is
// full and user holds none, the least recently used session of anyone. So
// at capacity a user's logins replace its own sessions, and end another
// user's (the idlest) only while it holds none; a session in use outlasts
// idle ones however old it is.
func (s *Sessions) makeRoomLocked(user string, now time.Time) {
	var ownKey, lruKey string
	var own, lru *session
	n := 0
	for k, v := range s.m {
		if s.expired(v, now) {
			delete(s.m, k)
			continue
		}
		if v.user == user {
			n++
			if own == nil || v.last.Before(own.last) {
				ownKey, own = k, v
			}
		}
		if lru == nil || v.last.Before(lru.last) {
			lruKey, lru = k, v
		}
	}
	switch {
	case own != nil && (n >= s.perUser || len(s.m) >= s.max):
		delete(s.m, ownKey)
	case lru != nil && len(s.m) >= s.max:
		delete(s.m, lruKey)
	}
}

func (s *Sessions) expired(v *session, now time.Time) bool {
	return now.Sub(v.last) > s.idle || now.Sub(v.created) > s.absolute
}

// User returns the session's user and refreshes its idle time. An expired
// session is deleted.
func (s *Sessions) User(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	key := sessionKey(id)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	if !ok {
		return "", false
	}
	if s.expired(v, now) {
		delete(s.m, key)
		return "", false
	}
	v.last = now
	return v.user, true
}

// Delete ends the session; an unknown id is ignored.
func (s *Sessions) Delete(id string) {
	s.mu.Lock()
	delete(s.m, sessionKey(id))
	s.mu.Unlock()
}

// DeleteUser ends every session of user.
func (s *Sessions) DeleteUser(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if v.user == user {
			delete(s.m, k)
		}
	}
}

// SetIdle changes the idle timeout; it applies to existing sessions too, as
// the check runs on each use (plan 12 Task 3). The absolute lifetime is not
// affected. A non-positive d is ignored.
func (s *Sessions) SetIdle(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	s.idle = d
	s.mu.Unlock()
}

// Idle returns the current idle timeout.
func (s *Sessions) Idle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idle
}
