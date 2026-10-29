package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"commandcenter/internal/meta"
)

// User tokens. A principal's user token is a name code and a pass code that
// stand in for its user name and password as Basic credentials wherever
// Basic is accepted. The name code finds the stored row (it does not reveal
// the user name), the pass code is compared with its stored SHA-256 hash.
// The owner (Principal.Owner) is looked up on every use, so a disabled or
// deleted user's token fails at once, and a failed check counts in the
// sign-in throttle under the name code, like a password. The token is a
// token (ViaToken): it cannot mint tokens or change settings.

const (
	nameCodeBytes = 6  // 8 base64url characters
	passCodeBytes = 33 // 44 base64url characters
)

// UserTokenStore persists user tokens (implemented by *meta.DB). The
// Authenticator uses it when its TokenStore implements it too.
type UserTokenStore interface {
	PutUserToken(ctx context.Context, owner, nameCode, hash string, created time.Time) error
	UserTokenByNameCode(ctx context.Context, nameCode string) (meta.UserToken, string, error)
	TouchUserToken(ctx context.Context, owner string)
}

var (
	// ErrUserTokensDisabled refuses to issue a user token while the feature is off.
	ErrUserTokensDisabled = errors.New("user tokens are disabled")
	// ErrUserTokenRequired refuses a password sent to a repository endpoint
	// while user tokens are required. It wraps ErrBadCredentials (401).
	ErrUserTokenRequired = fmt.Errorf("%w: user tokens are required for repository access", ErrBadCredentials)
)

// userTokenShaped reports whether Basic credentials have the form of a user
// token: an 8-character name code and a 44-character pass code, base64url.
func userTokenShaped(name, pass string) bool {
	return len(name) == 8 && len(pass) == 44 && base64url(name) && base64url(pass)
}

func base64url(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func randomCode(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *Authenticator) userTokenStore() (UserTokenStore, bool) {
	st, ok := a.tokens.(UserTokenStore)
	return st, ok
}

// userTokensRequired reports whether repository endpoints refuse user name
// and password: user tokens are enabled and required.
func (s *Snapshot) userTokensRequired() bool {
	return s.settings.UserTokens.Enabled && s.settings.UserTokens.Required
}

// CreateUserToken issues the user token of owner (Principal.Owner),
// replacing the one it had, and returns its codes; only the pass code's hash
// is stored. The owner must be a principal now.
func (a *Authenticator) CreateUserToken(ctx context.Context, owner string) (nameCode, passCode string, err error) {
	st, ok := a.userTokenStore()
	if !ok {
		return "", "", errors.New("auth: the token store cannot hold user tokens")
	}
	if !a.snap.Load().settings.UserTokens.Enabled {
		return "", "", ErrUserTokensDisabled
	}
	if _, ok := a.lookup(ctx, owner); !ok {
		return "", "", fmt.Errorf("%w: %q", ErrUserDisabled, owner)
	}
	for range 5 { // a name code taken by another owner: draw again
		nameCode, passCode = randomCode(nameCodeBytes), randomCode(passCodeBytes)
		if err = st.PutUserToken(ctx, owner, nameCode, hashToken(passCode), a.clock()); !errors.Is(err, meta.ErrExists) {
			break
		}
	}
	if err != nil {
		return "", "", err
	}
	return nameCode, passCode, nil
}

// userTokenBasic authenticates Basic credentials shaped like a user token.
// handled is false when they are not one (the feature is off, or no token
// has the name code): the caller then checks them as a password.
func (a *Authenticator) userTokenBasic(ctx context.Context, name, pass string) (p Principal, handled bool, err error) {
	st, ok := a.userTokenStore()
	if !ok || !userTokenShaped(name, pass) || !a.snap.Load().settings.UserTokens.Enabled {
		return Principal{}, false, nil
	}
	t, hash, err := st.UserTokenByNameCode(ctx, name)
	if errors.Is(err, meta.ErrNotFound) {
		return Principal{}, false, nil
	}
	if err != nil {
		return Principal{}, true, err
	}
	// The check is a hash compare and a lookup, cheap enough to run in
	// full where the throttle only asks for a cached success: a valid token
	// then never queues behind its name code's failures.
	p, err = a.throttle(ctx, name, func(bool) (Principal, bool) {
		s := a.snap.Load()
		if subtle.ConstantTimeCompare([]byte(hashToken(pass)), []byte(hash)) != 1 || !s.settings.UserTokens.Enabled ||
			UserTokenExpired(s.settings.UserTokens, t.Created, a.clock()) {
			return Principal{}, false
		}
		return a.lookup(ctx, t.Owner)
	})
	if err != nil {
		return Principal{}, true, err
	}
	if a.clock().Sub(t.LastUsed) >= meta.TokenTouchInterval {
		st.TouchUserToken(ctx, t.Owner)
	}
	p.Via = ViaToken
	return p, true, nil
}

// UserTokenExpired reports whether a token created at created has expired
// under c at now.
func UserTokenExpired(c meta.UserTokens, created, now time.Time) bool {
	exp := c.Expires(created)
	return !exp.IsZero() && !now.Before(exp)
}

// CheckRepoPassword checks credentials a client sent to a repository
// endpoint outside the Authorization header (npm login): a user token's
// codes are accepted, and a password is refused while user tokens are
// required (ErrUserTokenRequired); otherwise it is CheckPassword.
func (a *Authenticator) CheckRepoPassword(ctx context.Context, user, pass string) (Principal, error) {
	if p, handled, err := a.userTokenBasic(ctx, user, pass); handled {
		return p, err
	}
	if a.snap.Load().userTokensRequired() {
		return Principal{}, ErrUserTokenRequired
	}
	return a.CheckPassword(ctx, user, pass)
}

// repoEndpoint reports whether path is repository content: /repository/
// or the Docker registry API.
func repoEndpoint(path string) bool {
	return strings.HasPrefix(path, "/repository/") || path == "/v2" || strings.HasPrefix(path, "/v2/")
}
