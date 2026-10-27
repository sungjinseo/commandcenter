// Package auth authenticates requests and checks repository permissions.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

// Access is the kind of permission a request needs (plan 12 Decision 2).
type Access int

const (
	Read   Access = iota // GET/HEAD of content and metadata
	Browse               // UI listing and search; read implies it
	Add                  // a new upload or publish
	Edit                 // overwrite, npm dist-tags and deprecate, Cargo yank/unyank
	Delete               // removing content
	Public               // no permission needed (e.g. npm login endpoints)
)

// IsWrite reports whether a is one of the write kinds (Add, Edit, Delete),
// which a group applies to its writeMember.
func (a Access) IsWrite() bool { return a == Add || a == Edit || a == Delete }

func (a Access) String() string {
	switch a {
	case Read:
		return "read"
	case Browse:
		return "browse"
	case Add:
		return "add"
	case Edit:
		return "edit"
	case Delete:
		return "delete"
	case Public:
		return "public"
	}
	return fmt.Sprintf("Access(%d)", int(a))
}

var accessBits = map[Access]actionSet{Read: actRead, Browse: actBrowse | actRead, Add: actAdd, Edit: actEdit, Delete: actDelete}

var ErrBadCredentials = errors.New("bad credentials")

// ErrUserDisabled reports an operation refused because the user is disabled.
var ErrUserDisabled = errors.New("user is disabled")

// bcryptSlots bounds concurrent bcrypt comparisons so password floods cannot
// take every CPU.
var bcryptSlots = make(chan struct{}, 4)

// dummyHash is compared for unknown users, so a wrong user name costs as much
// as a wrong password and user names cannot be probed by timing.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("commandcenter-unknown-user"), bcrypt.DefaultCost)

// compareHash waits for a bcrypt slot until ctx ends; a cancelled wait returns
// ctx's error, never nil.
func compareHash(ctx context.Context, hash []byte, pass string) error {
	select {
	case bcryptSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-bcryptSlots }()
	return bcrypt.CompareHashAndPassword(hash, []byte(pass))
}

// Via records how a Principal was authenticated.
type Via string

const (
	ViaNone     Via = ""         // anonymous
	ViaPassword Via = "password" // Basic auth with the real password
	ViaToken    Via = "token"    // Bearer, raw token, or token as Basic password
	ViaSession  Via = "session"  // UI session cookie
)

// tokenPrefix starts every newly issued API token; legacyTokenPrefix marks
// tokens issued before the rename to Command Center, which stay valid.
const (
	tokenPrefix       = "cc_"
	legacyTokenPrefix = "rl_"
)

// Principal is the authenticated caller. The zero value is anonymous.
// Its roles are resolved against the live snapshot on every Can, so a role
// edit applies at once.
type Principal struct {
	User   string
	Admin  bool
	Via    Via
	roles  []string // role ids; shared with an immutable snapshot, never modified
	source string   // "" for a stored user, sourceLDAP for a directory user
}

func (p Principal) Anonymous() bool { return p.User == "" }

// TokenStore persists API tokens (implemented by *meta.DB).
type TokenStore interface {
	CreateToken(ctx context.Context, id, user, hash string, created time.Time) error
	TokenUser(ctx context.Context, hash string) (user string, lastUsed time.Time, err error)
	// TouchToken records a successful use; it must not be called for failed attempts.
	TouchToken(ctx context.Context, hash string)
}

// Source loads the stored users, roles and settings in one consistent read
// (implemented by *meta.DB).
type Source interface {
	LoadAccess(ctx context.Context) (meta.AccessData, error)
}

type Authenticator struct {
	tokens TokenStore

	// Source feeds Reload; nil for an Authenticator built from a config by New.
	Source Source

	// Sessions enables cookie authentication for the UI; nil disables it.
	Sessions *Sessions

	// AnonymousRead returns a repository's anonymousRead override, or nil to
	// inherit the global setting. It is asked on every check, so it can follow
	// a live registry. New sets a lookup over cfg.Repositories; the app
	// replaces it with the registry's before serving.
	AnonymousRead func(name string) *bool

	// RepoFormat returns a repository's format ("" if unknown), for
	// "format:<fmt>" privileges. Like AnonymousRead it follows the registry.
	RepoFormat func(name string) string

	snap     atomic.Pointer[Snapshot]
	reloadMu sync.Mutex // orders Reloads, so an older load never wins

	mu    sync.Mutex
	cache map[string]time.Time // user + "\x00" + sha256(hash, password) -> expiry
	fails map[string]failRec   // sign-in throttle (throttle.go)
	gates map[string]*failGate // names being checked (throttle.go)

	// LDAP is the directory client; nil uses ldap.Client.
	LDAP Directory
	// Log receives redacted LDAP failures; nil discards them.
	Log *slog.Logger

	now            func() time.Time // test hook; nil is time.Now
	ldapMu         sync.Mutex
	ldapUsers      map[string]*ldapEntry  // LDAP id -> entry
	ldapPass       map[string]ldapPassHit // ldapPassKey -> cached success
	ldapDown       time.Time              // the login path skips the ldap realm until then (outage back-off)
	ldapFails      int                    // consecutive dial-phase login failures
	ldapFailsSince time.Time              // when the first of them happened
}

const (
	cacheTTL = time.Minute
	cacheMax = 1000
)

// New builds an Authenticator from the config file's users, roles and
// settings, mapped as the one-time import maps them. Tests and tools use it;
// the app uses Open over the database.
func New(cfg *config.Config, tokens TokenStore) *Authenticator {
	a := &Authenticator{tokens: tokens, cache: map[string]time.Time{}}
	a.setSnapshot(NewSnapshot(meta.UsersFromConfig(cfg), meta.RolesFromConfig(cfg), meta.SettingsFromConfig(cfg)))
	anon := map[string]*bool{}
	formats := map[string]string{}
	for _, r := range cfg.Repositories {
		formats[r.Name] = r.Format
		if r.AnonymousRead != nil {
			v := *r.AnonymousRead
			anon[r.Name] = &v
		}
	}
	a.AnonymousRead = func(name string) *bool { return anon[name] }
	a.RepoFormat = func(name string) string { return formats[name] }
	return a
}

// Open builds an Authenticator over src and loads its first snapshot.
func Open(ctx context.Context, src Source, tokens TokenStore) (*Authenticator, error) {
	a := &Authenticator{tokens: tokens, Source: src, cache: map[string]time.Time{}}
	if err := a.Reload(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// Reload rebuilds the snapshot from Source and publishes it. The API layer
// calls it after every write to users, roles, realms or settings, and the
// app every few seconds for changes another process wrote ("commandcenter
// user reset-password", QA N-2). Whoever made the change, a user whose
// password changed, or who was disabled or removed, since the last snapshot
// loses its sessions and cached logins (OnUserChanged), as an API change
// does. This runs under reloadMu: the API creates the new session of its
// own password change only after its own Reload, so it is never ended here.
func (a *Authenticator) Reload(ctx context.Context) error {
	if a.Source == nil {
		return errors.New("auth: no source to reload from")
	}
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	data, err := a.Source.LoadAccess(ctx)
	if err != nil {
		return err
	}
	old, s := a.snap.Load(), NewSnapshot(data.Users, data.Roles, data.Settings)
	a.setSnapshot(s)
	if old == nil {
		return nil
	}
	for id, was := range old.users {
		if u, ok := s.users[id]; !ok || u.PasswordHash != was.PasswordHash ||
			(was.Status == meta.StatusActive && u.Status != meta.StatusActive) {
			a.OnUserChanged(id)
		}
	}
	return nil
}

// setSnapshot publishes s. A changed LDAP configuration drops the cached
// LDAP users and password successes, so they are looked up again under it.
func (a *Authenticator) setSnapshot(s *Snapshot) {
	if old := a.snap.Swap(s); old != nil && ldapChanged(old, s) {
		a.clearLDAP()
	}
}

// Snapshot returns the live snapshot. It is immutable.
func (a *Authenticator) Snapshot() *Snapshot { return a.snap.Load() }

// OnUserChanged invalidates what was derived from user id: its UI sessions
// and its cached password successes, and the same for the LDAP user with
// that id (whose override row may have changed). Tokens are checked against
// the snapshot on every use, so no token principal is cached. It also ends
// a sign-in refusal of the name, so a password reset lets the user in. Call
// it after Reload when a password changed or the user was disabled or
// deleted (Decision 1).
func (a *Authenticator) OnUserChanged(id string) {
	if a.Sessions != nil {
		a.Sessions.DeleteUser(id)
		a.Sessions.DeleteUser(ldapOwnerPrefix + id)
	}
	a.dropLDAP(id)
	prefix := id + "\x00"
	a.mu.Lock()
	for k := range a.cache {
		if strings.HasPrefix(k, prefix) {
			delete(a.cache, k)
		}
	}
	delete(a.fails, nameKey(id))
	a.mu.Unlock()
}

// TokenUser is the Basic username that accepts a token of any owner.
const TokenUser = "__token__"

// Authenticate identifies the caller. A request without credentials is anonymous.
func (a *Authenticator) Authenticate(r *http.Request) (Principal, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return a.cookiePrincipal(r), nil
	}
	if user, pass, ok := r.BasicAuth(); ok {
		// A token may stand in for the password (Maven, pip, Go CI), but only
		// for its owner. Anything else falls through to the password check,
		// which a token string can never pass.
		if user != "" && (strings.HasPrefix(pass, tokenPrefix) || strings.HasPrefix(pass, legacyTokenPrefix)) {
			owner := user
			if user == TokenUser {
				owner = "" // "__token__" (twine, uv) stands for the token's owner
			}
			p, err := a.checkToken(r.Context(), pass, owner)
			if err == nil {
				return p, nil
			}
			if err != nil && !errors.Is(err, ErrBadCredentials) {
				return Principal{}, err
			}
			if tokenShaped(pass) {
				// A revoked or mistyped token, not a password guess: CI
				// retrying one must not get its owner's sign-ins throttled.
				return Principal{}, ErrBadCredentials
			}
		}
		return a.CheckPassword(r.Context(), user, pass)
	}
	scheme, tok, found := strings.Cut(h, " ")
	switch {
	case found && strings.EqualFold(scheme, "Bearer"):
		return a.checkToken(r.Context(), strings.TrimSpace(tok), "")
	case !found:
		return a.checkToken(r.Context(), h, "") // cargo sends the raw token
	}
	return Principal{}, ErrBadCredentials
}

// SessionCookieName is the UI session cookie's name under the live baseURL:
// HostSessionCookie when it is https (the cookie is then Secure), else
// SessionCookie. Only this name authenticates.
func (a *Authenticator) SessionCookieName() string {
	if strings.HasPrefix(a.snap.Load().settings.BaseURL, "https://") {
		return HostSessionCookie
	}
	return SessionCookie
}

// cookiePrincipal authenticates by session cookie. Anything doubtful (no
// store, no cookie, unknown session, removed or disabled user) is anonymous.
// Unsafe methods need the X-Command-Center-UI header, which a cross-site form
// cannot set. On http another host under the same domain can add cookies of
// the name (with a Domain or a longer Path, sent first), so all of them are
// tried: unknown ids are skipped, so a planted value neither shadows the
// user's session nor locks the user out, and live sessions of two users are
// refused rather than one picked (signing in again ends them all). A browser
// sends at most one __Host- cookie of the name.
func (a *Authenticator) cookiePrincipal(r *http.Request) Principal {
	if a.Sessions == nil {
		return Principal{}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(UIHeader) != "1" {
		return Principal{}
	}
	user := ""
	for _, c := range r.CookiesNamed(a.SessionCookieName()) {
		u, ok := a.Sessions.User(c.Value)
		if !ok {
			continue
		}
		if user != "" && u != user {
			return Principal{}
		}
		user = u
	}
	if user == "" {
		return Principal{}
	}
	p, ok := a.lookup(r.Context(), user)
	if !ok {
		return Principal{}
	}
	p.Via = ViaSession
	return p
}

// UserExists reports whether name is a stored user, enabled or not.
func (a *Authenticator) UserExists(name string) bool {
	_, ok := a.snap.Load().users[name]
	return ok
}

// Lookup returns the principal of a session key (Principal.Owner): an
// active stored user, or "ldap:<id>" for an LDAP user, re-checked against
// the directory at most once a minute (failing closed).
func (a *Authenticator) Lookup(name string) (Principal, bool) {
	return a.lookup(context.Background(), name)
}

func (a *Authenticator) lookup(ctx context.Context, name string) (Principal, bool) {
	if id, ok := splitOwner(name); ok {
		return a.ldapLookup(ctx, id)
	}
	u, ok := a.snap.Load().users[name]
	if !ok || u.Status != meta.StatusActive || u.Source == meta.SourceLDAP {
		return Principal{}, false // an ldap-source row is an override, not a principal
	}
	return principal(u), true
}

// CheckPassword verifies a user's password through the enabled password
// realms (local, ldap) in the order of the realm list; the first that
// accepts wins. Both realms cache successes for a minute, and a cached
// success is answered first, in the same order, without checking a
// password; disabled users and users without a local password fail the
// local realm. A realm that errors (LDAP unreachable) is logged and the
// next one is tried. If ctx ends while waiting for a bcrypt slot, the check
// fails with ErrBadCredentials. A name with too many recent failures is
// refused with a *ThrottledError, and one that is not PlainLoginName
// fails, before any realm is asked (throttle.go).
func (a *Authenticator) CheckPassword(ctx context.Context, user, pass string) (Principal, error) {
	if len(user) > maxLoginName || len(pass) > maxLoginPassword {
		// Never sent to any realm: no stored id or password is this long,
		// and an oversized value must not reach (and upset) the directory.
		return Principal{}, ErrBadCredentials
	}
	return a.throttle(ctx, user, func(cachedOnly bool) (Principal, bool) { return a.checkRealms(ctx, user, pass, cachedOnly) })
}

// checkRealms asks the enabled password realms in order. With cachedOnly a
// realm only answers a live cached success, and no password is checked.
func (a *Authenticator) checkRealms(ctx context.Context, user, pass string, cachedOnly bool) (Principal, bool) {
	s := a.snap.Load()
	compared := false
	for _, r := range s.settings.Realms {
		if !r.Enabled {
			continue
		}
		switch r.ID {
		case meta.RealmLocal:
			compared = true
			if p, ok := a.localPassword(ctx, s, user, pass, cachedOnly); ok {
				return p, true
			}
		case meta.RealmLDAP:
			if p, ok := a.ldapPassword(ctx, s, user, pass, cachedOnly); ok {
				return p, true
			}
		}
	}
	if !compared && !cachedOnly {
		compareHash(ctx, dummyHash, pass) // the same cost whichever realms are on
	}
	return Principal{}, false
}

// CheckLocalPassword verifies a stored local user's password through the
// local realm only (for proving the current password before changing it:
// a same-named LDAP user's password must not do). It is throttled like
// CheckPassword.
func (a *Authenticator) CheckLocalPassword(ctx context.Context, user, pass string) (Principal, error) {
	return a.throttle(ctx, user, func(cachedOnly bool) (Principal, bool) {
		return a.localPassword(ctx, a.snap.Load(), user, pass, cachedOnly)
	})
}

// localPassword is the local realm's check; with cachedOnly it only answers
// a live cached success.
func (a *Authenticator) localPassword(ctx context.Context, s *Snapshot, user, pass string, cachedOnly bool) (Principal, bool) {
	u, ok := s.users[user]
	if !ok || !s.canPasswordLogin(u) {
		if !cachedOnly {
			compareHash(ctx, dummyHash, pass) // only hides which names exist
		}
		return Principal{}, false
	}
	// The key covers the stored hash, so a success never outlives a password
	// change, even before OnUserChanged clears it.
	sum := sha256.Sum256([]byte(u.PasswordHash + "\x00" + pass))
	key := user + "\x00" + hex.EncodeToString(sum[:])
	now := a.clock()

	a.mu.Lock()
	exp, hit := a.cache[key]
	a.mu.Unlock()
	if hit && now.Before(exp) {
		return passwordPrincipal(u), true
	}
	if cachedOnly || compareHash(ctx, []byte(u.PasswordHash), pass) != nil {
		return Principal{}, false
	}
	a.mu.Lock()
	if len(a.cache) >= cacheMax {
		a.cache = map[string]time.Time{}
	}
	a.cache[key] = now.Add(cacheTTL)
	a.mu.Unlock()
	return passwordPrincipal(u), true
}

func passwordPrincipal(u meta.User) Principal {
	p := principal(u)
	p.Via = ViaPassword
	return p
}

// checkToken authenticates a token. A non-empty owner restricts it to that user
// (Basic auth); a mismatch fails without recording a use. The token realm must
// be enabled and the owner active.
func (a *Authenticator) checkToken(ctx context.Context, tok, owner string) (Principal, error) {
	if tok == "" || !a.snap.Load().RealmEnabled(meta.RealmToken) {
		return Principal{}, ErrBadCredentials
	}
	hash := hashToken(tok)
	user, lastUsed, err := a.tokens.TokenUser(ctx, hash)
	if errors.Is(err, meta.ErrNotFound) {
		return Principal{}, ErrBadCredentials
	}
	if err != nil {
		return Principal{}, err
	}
	s := a.snap.Load()
	if !s.RealmEnabled(meta.RealmToken) {
		return Principal{}, ErrBadCredentials
	}
	var p Principal
	if id, isLDAP := splitOwner(user); isLDAP {
		// An LDAP user's token: the ldap realm must be on too, and the user
		// still in the directory (re-checked at most once a minute).
		if owner != "" && id != owner {
			return Principal{}, ErrBadCredentials
		}
		lp, ok := a.ldapLookup(ctx, id)
		if !ok {
			return Principal{}, ErrBadCredentials
		}
		p = lp
	} else {
		u, ok := s.users[user]
		if !ok || u.Status != meta.StatusActive || u.Source == meta.SourceLDAP {
			return Principal{}, ErrBadCredentials
		}
		if owner != "" && u.ID != owner {
			return Principal{}, ErrBadCredentials
		}
		p = principal(u)
	}
	if time.Since(lastUsed) >= meta.TokenTouchInterval {
		a.tokens.TouchToken(ctx, hash)
	}
	p.Via = ViaToken
	return p, nil
}

// principal builds the principal of u; u.Roles belongs to an immutable snapshot.
func principal(u meta.User) Principal {
	return Principal{User: u.ID, Admin: hasRole(u.Roles, meta.AdminRole), roles: u.Roles}
}

// Can reports whether p may perform acc on the named repository.
//
// Read and Browse: anonymous read (the repository's override, else the global
// setting) admits everyone; otherwise a signed-in user needs authenticatedRead
// or a read privilege (Browse also accepts a browse privilege). Add, Edit and
// Delete need the matching privilege. Admins may do everything.
func (a *Authenticator) Can(p Principal, repo string, acc Access) bool {
	if acc == Public {
		return true
	}
	if p.Admin {
		return true
	}
	s := a.snap.Load()
	switch acc {
	case Read, Browse:
		if a.anonymousRead(s, repo) {
			return true
		}
		if p.Anonymous() {
			return false
		}
		if s.settings.AuthenticatedRead {
			return true
		}
	case Add, Edit, Delete:
		if p.Anonymous() {
			return false
		}
	default:
		return false
	}
	format := ""
	if a.RepoFormat != nil {
		format = a.RepoFormat(repo)
	}
	return s.has(p.roles, repo, format, accessBits[acc])
}

func (a *Authenticator) anonymousRead(s *Snapshot, repo string) bool {
	if a.AnonymousRead != nil {
		if v := a.AnonymousRead(repo); v != nil {
			return *v
		}
	}
	return s.settings.AnonymousRead
}

// CreateToken issues a new API token for an active user and returns its id
// and value. Only the hash is stored.
func (a *Authenticator) CreateToken(ctx context.Context, user string) (id, token string, err error) {
	u, ok := a.snap.Load().users[user]
	if !ok {
		return "", "", fmt.Errorf("unknown user %q", user)
	}
	if u.Status != meta.StatusActive {
		return "", "", fmt.Errorf("%w: %q", ErrUserDisabled, user)
	}
	if u.Source == meta.SourceLDAP {
		// An override row is not a principal: an LDAP user's own tokens
		// come from CreateTokenFor and are owned by "ldap:<id>".
		return "", "", fmt.Errorf("%w: %q is an LDAP user", ErrLDAPUser, user)
	}
	return a.storeToken(ctx, user)
}

// TokenRevoker deletes tokens (implemented by *meta.DB): one by its hash, or
// every token of some owners. It is separate from TokenStore so stores that
// never revoke need not implement it.
type TokenRevoker interface {
	DeleteTokenByHash(ctx context.Context, hash string) error
	DeleteUserTokens(ctx context.Context, owners ...string) (int64, error)
}

// RevokeUserTokens deletes every API token of user id: the stored user's and
// those of the LDAP user with that id ("ldap:<id>"), as OnUserChanged ends
// both users' sessions. It returns how many it deleted (QA N-16).
func (a *Authenticator) RevokeUserTokens(ctx context.Context, id string) (int64, error) {
	rv, ok := a.tokens.(TokenRevoker)
	if !ok {
		return 0, errors.New("auth: the token store cannot revoke tokens")
	}
	return rv.DeleteUserTokens(ctx, id, ldapOwnerPrefix+id)
}

// RevokeToken deletes token when p owns it (npm logout). A token that does not
// exist or belongs to anyone else is meta.ErrNotFound, so the answer does not
// reveal other users' tokens.
func (a *Authenticator) RevokeToken(ctx context.Context, token string, p Principal) error {
	rv, ok := a.tokens.(TokenRevoker)
	if !ok || token == "" || p.Anonymous() {
		return meta.ErrNotFound
	}
	hash := hashToken(token)
	owner, _, err := a.tokens.TokenUser(ctx, hash)
	if err != nil {
		return err
	}
	if owner != p.Owner() {
		return meta.ErrNotFound
	}
	return rv.DeleteTokenByHash(ctx, hash)
}

// ErrLDAPUser reports a token requested for an LDAP override row.
var ErrLDAPUser = errors.New("tokens of LDAP users are created by the user")

func (a *Authenticator) storeToken(ctx context.Context, owner string) (id, token string, err error) {
	id, token = randomHex(8), tokenPrefix+randomHex(32)
	if err := a.tokens.CreateToken(ctx, id, owner, hashToken(token), time.Now()); err != nil {
		return "", "", err
	}
	return id, token, nil
}

// Password rules shared by the API and the offline reset (final review I4).
const (
	MinPasswordRunes = 8
	MaxPasswordBytes = 72 // bcrypt ignores (or rejects) anything longer
)

// CheckNewPassword applies the password rules to a new password.
func CheckNewPassword(pw string) error {
	switch {
	case utf8.RuneCountInString(pw) < MinPasswordRunes:
		return fmt.Errorf("password must be at least %d characters", MinPasswordRunes)
	case len(pw) > MaxPasswordBytes:
		return fmt.Errorf("password must be at most %d bytes", MaxPasswordBytes)
	case strings.TrimSpace(pw) == "":
		return errors.New("password must not be only spaces")
	}
	return nil
}

// HashPassword returns a bcrypt hash for use in the config file.
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

// HashUsable reports whether hash is a bcrypt hash, the only kind a password
// can match. A placeholder such as the config example's "<commandcenter
// hash-password ...>" is not: its user cannot sign in with a password, so
// the API shows no password and the lockout checks do not count it as an
// administrator who can sign in (QA L-2).
func HashUsable(hash string) bool {
	_, err := bcrypt.Cost([]byte(hash))
	return err == nil
}

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func FromContext(ctx context.Context) Principal {
	p, _ := ctx.Value(ctxKey{}).(Principal)
	return p
}

// tokenShaped reports whether s has the form of an issued token: a token
// prefix and 64 hex digits (storeToken).
func tokenShaped(s string) bool {
	if len(s) != len(tokenPrefix)+64 || (!strings.HasPrefix(s, tokenPrefix) && !strings.HasPrefix(s, legacyTokenPrefix)) {
		return false
	}
	_, err := hex.DecodeString(s[len(tokenPrefix):])
	return err == nil
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

// Permits reports whether the principal in ctx may perform acc on repo. A nil
// Authenticator permits everything: handlers built without one (unit tests)
// rely on the server's check alone.
func (a *Authenticator) Permits(ctx context.Context, repo string, acc Access) bool {
	if a == nil {
		return true
	}
	return a.Can(FromContext(ctx), repo, acc)
}
