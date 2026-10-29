package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"commandcenter/internal/meta"
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

const (
	// maxPerUser caps the sessions one owner may hold: a login beyond it
	// ends the owner's least recently used one.
	maxPerUser = 20
	// defaultIdle is the idle timeout when no setting gives one.
	defaultIdle = 12 * time.Hour
	// sessionTouch is how often a session's last use is written: idle
	// time counts from a use at most this much earlier.
	sessionTouch = time.Minute
)

// Sessions are the UI sessions, stored in the database (meta's sessions
// table): every node of a cluster accepts a session any node started, and
// sessions survive a restart. Rows are keyed by the SHA-256 of the session
// id, so the raw ids are never stored. A session ends after the idle
// timeout without use and meta.SessionMaxAge after it started.
type Sessions struct {
	db   *meta.DB
	now  func() time.Time
	idle func() time.Duration
}

// NewSessions returns the sessions in db. idle gives the idle timeout on
// every check (the app passes the live setting, Authenticator.SessionIdle);
// nil, or a result of 0, is 12 hours.
func NewSessions(db *meta.DB, idle func() time.Duration) *Sessions {
	return &Sessions{db: db, now: time.Now, idle: idle}
}

func sessionKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// Idle returns the current idle timeout.
func (s *Sessions) Idle() time.Duration {
	if s.idle != nil {
		if d := s.idle(); d > 0 {
			return d
		}
	}
	return defaultIdle
}

// Create starts a session for owner and returns its id. The owner's expired
// sessions go, and beyond maxPerUser its least recently used ones.
func (s *Sessions) Create(ctx context.Context, owner string) (string, error) {
	id := randomHex(32)
	if err := s.db.CreateSession(ctx, sessionKey(id), owner, s.now(), s.Idle(), maxPerUser); err != nil {
		return "", err
	}
	return id, nil
}

// User returns the session's owner and records the use (at most once per
// sessionTouch). An expired session is deleted; a database error fails
// closed.
func (s *Sessions) User(ctx context.Context, id string) (string, bool) {
	if id == "" {
		return "", false
	}
	key, now := sessionKey(id), s.now()
	v, err := s.db.GetSession(ctx, key)
	if err != nil {
		return "", false
	}
	if now.Sub(v.LastSeen) > s.Idle() || now.Sub(v.Created) > meta.SessionMaxAge {
		s.db.DeleteSession(ctx, key)
		return "", false
	}
	if now.Sub(v.LastSeen) >= sessionTouch {
		s.db.TouchSession(ctx, key, now, sessionTouch) // a failed write only shortens the idle time
	}
	return v.Owner, true
}

// Delete ends the session; an unknown id is ignored.
func (s *Sessions) Delete(ctx context.Context, id string) error {
	return s.db.DeleteSession(ctx, sessionKey(id))
}

// DeleteUser ends the sessions of owners started before before, or all of
// them when before is zero.
func (s *Sessions) DeleteUser(ctx context.Context, before time.Time, owners ...string) error {
	return s.db.DeleteSessions(ctx, before, owners...)
}

// SessionIdle is the live idle timeout setting, for NewSessions.
func (a *Authenticator) SessionIdle() time.Duration {
	return time.Duration(a.snap.Load().settings.SessionIdleMinutes) * time.Minute
}
