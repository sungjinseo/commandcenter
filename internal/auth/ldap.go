package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"commandcenter/internal/ldap"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
)

// LDAP login (plan 12 Task 6, Decision 4).
//
// An LDAP principal has no stored record of its own. A successful login
// fills an in-memory entry (id, name, e-mail, groups); sessions and tokens
// of the user ("ldap:<id>") resolve through it. An entry older than
// ldapRecheck is re-checked with a service-bind search before it is used:
// a user gone from the directory is denied, and so is everyone while the
// directory cannot be reached (fail closed; local admins are unaffected).
// Roles are computed on every use from the live snapshot: the group
// mappings, the default roles and an optional stored override row
// (source=ldap, same id) whose roles are added and which denies the user
// when disabled.

// Directory is the LDAP client (ldap.Client); tests may substitute it.
type Directory interface {
	Authenticate(ctx context.Context, cfg meta.LDAP, name, password string) (ldap.User, error)
	Find(ctx context.Context, cfg meta.LDAP, id string) (ldap.User, error)
}

const (
	// ldapOwnerPrefix marks an LDAP principal in session keys and token owners.
	ldapOwnerPrefix = "ldap:"
	sourceLDAP      = "ldap"

	ldapRecheck      = time.Minute      // an entry is re-checked when older
	ldapRetry        = 10 * time.Second // after a failed re-check, deny without dialling for this long
	ldapLoginBackoff = 15 * time.Second // after an outage on the login path, skip the ldap realm for this long
	ldapDialFailures = 3                // consecutive dial-phase failures that start the back-off
	maxLoginName     = 256              // bytes; longer names are refused before any realm
	maxLoginPassword = 1024             // bytes; longer passwords are refused before any realm
	ldapMax          = 1000             // cached LDAP users and cached LDAP password successes, each
)

// ldapSlots bounds concurrent directory operations.
var ldapSlots = make(chan struct{}, 8)

type ldapEntry struct {
	// busy is a one-slot lock that serialises the re-check of one id; unlike
	// a mutex, waiting for it ends with the caller's context.
	busy chan struct{}

	// guarded by busy
	user      ldap.User
	checkedAt time.Time
	failedAt  time.Time
}

func newLDAPEntry(u ldap.User, checkedAt time.Time) *ldapEntry {
	return &ldapEntry{busy: make(chan struct{}, 1), user: u, checkedAt: checkedAt}
}

type ldapPassHit struct {
	id  string
	exp time.Time
}

// IsLDAP reports whether p signed in through the ldap realm.
func (p Principal) IsLDAP() bool { return p.source == sourceLDAP }

// Owner is the key of p's sessions and tokens: the user id, or "ldap:<id>"
// for an LDAP principal, so a local and an LDAP user with the same id
// never share them.
func (p Principal) Owner() string {
	if p.IsLDAP() {
		return ldapOwnerPrefix + p.User
	}
	return p.User
}

// Roles returns a copy of p's role ids.
func (p Principal) Roles() []string { return append([]string{}, p.roles...) }

func (a *Authenticator) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *Authenticator) dir() Directory {
	if a.LDAP != nil {
		return a.LDAP
	}
	return defaultDirectory
}

var defaultDirectory = &ldap.Client{}

func (a *Authenticator) logger() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.New(slog.DiscardHandler)
}

// acquireLDAP takes a directory slot until ctx ends.
func acquireLDAP(ctx context.Context) (func(), error) {
	select {
	case ldapSlots <- struct{}{}:
		return func() { <-ldapSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ldapUsable reports whether the ldap realm is on and configured.
func ldapUsable(s *Snapshot) bool {
	return s.realms[meta.RealmLDAP] && s.settings.LDAP.Complete()
}

// ldapPassKey is the cache key of an LDAP password success: never the
// plaintext, always the realm.
func ldapPassKey(name, pass string) string {
	sum := sha256.Sum256([]byte("ldap\x00" + name + "\x00" + pass))
	return "ldap\x00" + hex.EncodeToString(sum[:])
}

// clearLDAP drops every cached LDAP user and password success.
func (a *Authenticator) clearLDAP() {
	a.ldapMu.Lock()
	a.ldapUsers = map[string]*ldapEntry{}
	a.ldapPass = map[string]ldapPassHit{}
	a.ldapDown = time.Time{}
	a.ldapFails = 0
	a.ldapMu.Unlock()
}

// dropLDAP forgets one LDAP user.
func (a *Authenticator) dropLDAP(id string) {
	a.ldapMu.Lock()
	delete(a.ldapUsers, id)
	for k, v := range a.ldapPass {
		if v.id == id {
			delete(a.ldapPass, k)
		}
	}
	a.ldapMu.Unlock()
}

// ldapPassword is the ldap realm's password check; with cachedOnly it only
// answers a live cached success.
func (a *Authenticator) ldapPassword(ctx context.Context, s *Snapshot, name, pass string, cachedOnly bool) (Principal, bool) {
	if name == "" || pass == "" || len(name) > maxLoginName || len(pass) > maxLoginPassword || !ldapUsable(s) {
		return Principal{}, false // never an unauthenticated bind
	}
	key := ldapPassKey(name, pass)
	now := a.clock()
	a.ldapMu.Lock()
	hit, ok := a.ldapPass[key]
	a.ldapMu.Unlock()
	if ok && now.Before(hit.exp) {
		if p, ok := a.ldapLookup(ctx, hit.id); ok {
			p.Via = ViaPassword
			return p, true
		}
		return Principal{}, false
	}
	if cachedOnly {
		return Principal{}, false
	}
	a.ldapMu.Lock()
	down := now.Before(a.ldapDown)
	a.ldapMu.Unlock()
	if down {
		return Principal{}, false // the directory failed recently: the next realm decides
	}
	release, err := acquireLDAP(ctx)
	if err != nil {
		return Principal{}, false
	}
	u, err := a.dir().Authenticate(ctx, s.settings.LDAP, name, pass)
	release()
	if err != nil {
		a.noteLDAPResult(ctx, err)
		return Principal{}, false
	}
	a.noteLDAPResult(ctx, nil)
	if !meta.ValidUserID(u.ID) {
		a.logger().Warn("ldap login refused: the user id is not valid", "attr", s.settings.LDAP.UserIDAttr)
		return Principal{}, false
	}
	e := newLDAPEntry(u, now)
	p, ok := a.ldapPrincipal(s, e.user)
	if !ok {
		return Principal{}, false
	}
	a.ldapMu.Lock()
	if a.ldapUsers == nil || len(a.ldapUsers) >= ldapMax {
		a.ldapUsers = map[string]*ldapEntry{}
	}
	if a.ldapPass == nil || len(a.ldapPass) >= ldapMax {
		a.ldapPass = map[string]ldapPassHit{}
	}
	a.ldapUsers[u.ID] = e
	a.ldapPass[key] = ldapPassHit{id: u.ID, exp: now.Add(cacheTTL)}
	a.ldapMu.Unlock()
	p.Via = ViaPassword
	return p, true
}

// ldapPrincipal builds the principal of directory user u under s. A
// disabled override row denies it.
func (a *Authenticator) ldapPrincipal(s *Snapshot, u ldap.User) (Principal, bool) {
	roles, _ := ldap.MapRoles(s.settings.LDAP, u.Groups)
	if row, ok := s.users[u.ID]; ok && row.Source == meta.SourceLDAP {
		if row.Status != meta.StatusActive {
			return Principal{}, false
		}
		for _, r := range row.Roles {
			if !hasRole(roles, r) {
				roles = append(roles, r)
			}
		}
	}
	return Principal{User: u.ID, Admin: hasRole(roles, meta.AdminRole), roles: roles, source: sourceLDAP}, true
}

// ldapLookup returns the principal of LDAP user id, re-checking it against
// the directory when its entry is older than ldapRecheck (or missing, as
// after a restart). It fails closed, also when ctx ends while another
// request re-checks the same id.
func (a *Authenticator) ldapLookup(ctx context.Context, id string) (Principal, bool) {
	s := a.snap.Load()
	if !ldapUsable(s) || !meta.ValidUserID(id) {
		return Principal{}, false
	}
	a.ldapMu.Lock()
	if a.ldapUsers == nil || len(a.ldapUsers) >= ldapMax {
		a.ldapUsers = map[string]*ldapEntry{}
	}
	e := a.ldapUsers[id]
	if e == nil {
		e = newLDAPEntry(ldap.User{ID: id}, time.Time{})
		a.ldapUsers[id] = e
	}
	a.ldapMu.Unlock()

	select {
	case e.busy <- struct{}{}:
	case <-ctx.Done():
		return Principal{}, false
	}
	defer func() { <-e.busy }()
	now := a.clock()
	if e.checkedAt.IsZero() || now.Sub(e.checkedAt) >= ldapRecheck {
		if !e.failedAt.IsZero() && now.Sub(e.failedAt) < ldapRetry {
			return Principal{}, false
		}
		u, err := a.findLDAP(ctx, s.settings.LDAP, id)
		// Stamp with the time the attempt ended: after a directory that hung
		// past ldapRetry, the requests queued behind this one must still be
		// denied without dialling.
		now = a.clock()
		switch {
		case errors.Is(err, ldap.ErrNoSuchUser) || errors.Is(err, ldap.ErrAmbiguous):
			a.dropLDAP(id)
			return Principal{}, false
		case err != nil && ctx.Err() != nil:
			return Principal{}, false // the caller left: not a directory failure (as isOutage)
		case err != nil:
			e.failedAt = now
			a.logger().Warn("ldap re-check failed: denying until the directory answers", "user", id, "err", logsafe.Err(err))
			return Principal{}, false
		}
		e.user, e.checkedAt, e.failedAt = u, now, time.Time{}
	}
	return a.ldapPrincipal(s, e.user)
}

func (a *Authenticator) findLDAP(ctx context.Context, cfg meta.LDAP, id string) (ldap.User, error) {
	release, err := acquireLDAP(ctx)
	if err != nil {
		return ldap.User{}, err
	}
	defer release()
	return a.dir().Find(ctx, cfg, id)
}

// ldapChanged reports whether the LDAP configuration differs between snapshots.
func ldapChanged(old, cur *Snapshot) bool {
	if old == nil {
		return true
	}
	return !reflect.DeepEqual(old.settings.LDAP, cur.settings.LDAP)
}

// LDAPUser is a recently seen LDAP principal, for the user list.
type LDAPUser struct {
	ID, Name, Email string
	Roles           []string
}

// LDAPUsers returns the cached LDAP users that are currently allowed, with
// their roles under the live snapshot, ordered by id. It never contacts
// the directory.
func (a *Authenticator) LDAPUsers() []LDAPUser {
	s := a.snap.Load()
	if !ldapUsable(s) {
		return nil
	}
	a.ldapMu.Lock()
	entries := make([]*ldapEntry, 0, len(a.ldapUsers))
	for _, e := range a.ldapUsers {
		entries = append(entries, e)
	}
	a.ldapMu.Unlock()
	var out []LDAPUser
	for _, e := range entries {
		select {
		case e.busy <- struct{}{}:
		default:
			continue // being re-checked
		}
		u, checked := e.user, !e.checkedAt.IsZero()
		<-e.busy
		if !checked {
			continue
		}
		if p, ok := a.ldapPrincipal(s, u); ok {
			out = append(out, LDAPUser{ID: u.ID, Name: u.Name, Email: u.Email, Roles: p.roles})
		}
	}
	sortLDAPUsers(out)
	return out
}

func sortLDAPUsers(us []LDAPUser) {
	for i := 1; i < len(us); i++ {
		for j := i; j > 0 && us[j].ID < us[j-1].ID; j-- {
			us[j], us[j-1] = us[j-1], us[j]
		}
	}
}

// CreateTokenFor issues an API token for p itself: a local user's token is
// owned by its id (as CreateToken), an LDAP user's by "ldap:<id>" after
// confirming the user is still allowed.
func (a *Authenticator) CreateTokenFor(ctx context.Context, p Principal) (id, token string, err error) {
	if p.Anonymous() {
		return "", "", errors.New("anonymous caller")
	}
	if !p.IsLDAP() {
		return a.CreateToken(ctx, p.User)
	}
	if _, ok := a.ldapLookup(ctx, p.User); !ok {
		return "", "", fmt.Errorf("%w: %q", ErrUserDisabled, p.User)
	}
	return a.storeToken(ctx, p.Owner())
}

// splitOwner splits a token owner or session key into an LDAP id.
func splitOwner(owner string) (ldapID string, isLDAP bool) {
	return strings.CutPrefix(owner, ldapOwnerPrefix)
}

// isOutage reports whether a login error means the directory could not be
// reached: only a dial-phase failure (*ldap.DialError: connect, TLS, network
// I/O during the service bind). Everything after the service bind (search,
// user bind, parse or cap errors, a notice of disconnection) is a
// per-request failure a caller can provoke, so it never counts. A caller
// that went away is not an outage either.
func isOutage(ctx context.Context, err error) bool {
	var de *ldap.DialError
	return ctx.Err() == nil && errors.As(err, &de)
}

// noteLDAPResult counts dial-phase failures of login attempts: after
// ldapDialFailures in a row (within ldapLoginBackoff of the first) the
// login path skips the ldap realm for ldapLoginBackoff. Any other outcome
// (a success, or any answer after the service bind) resets the count: the
// directory is reachable, and those outcomes are the caller's to provoke.
func (a *Authenticator) noteLDAPResult(ctx context.Context, err error) {
	now := a.clock()
	a.ldapMu.Lock()
	defer a.ldapMu.Unlock()
	if err == nil || !isOutage(ctx, err) {
		a.ldapFails = 0
		return
	}
	if a.ldapFails == 0 || now.Sub(a.ldapFailsSince) > ldapLoginBackoff {
		a.ldapFails, a.ldapFailsSince = 0, now
	}
	a.ldapFails++
	if a.ldapFails >= ldapDialFailures {
		a.ldapFails = 0
		a.ldapDown = now.Add(ldapLoginBackoff)
		a.logger().Warn("ldap is unreachable: skipping the ldap realm for logins", "for", ldapLoginBackoff.String(), "err", logsafe.Err(err))
	}
}
