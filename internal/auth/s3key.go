package auth

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"commandcenter/internal/meta"
)

// S3 access keys (format/s3). A principal may hold up to MaxS3KeysPerUser
// pairs of an access key id and a secret. The secret is stored sealed, as
// a SigV4 signature is an HMAC under it; it is answered once, on creation.
// A request signed with a key acts as the key's owner, looked up on every
// use like a user token's (a disabled or deleted owner fails at once), and
// is a token (ViaToken): it reads and writes repositories, nothing else.

// MaxS3KeysPerUser bounds the keys of one principal.
const MaxS3KeysPerUser = 10

// s3KeyTTL is how long a node keeps a key it read: a key deleted on
// another node stops working here within it.
const s3KeyTTL = 30 * time.Second

var (
	ErrTooManyS3Keys = fmt.Errorf("a user may hold at most %d S3 access keys", MaxS3KeysPerUser)
	// ErrS3KeyUnknown is an access key id no stored key has (S3's
	// InvalidAccessKeyId).
	ErrS3KeyUnknown = errors.New("unknown S3 access key")
	// ErrS3KeyExpired is a key past its expiry.
	ErrS3KeyExpired = errors.New("the S3 access key has expired")
)

// S3KeyStore persists S3 keys (implemented by *meta.DB).
type S3KeyStore interface {
	PutS3Key(ctx context.Context, k meta.S3Key, secret string) error
	S3KeyByID(ctx context.Context, id string) (meta.S3Key, string, error)
	ListS3Keys(ctx context.Context, owner string) ([]meta.S3Key, error)
	DeleteS3Key(ctx context.Context, id string) (meta.S3Key, error)
	TouchS3Key(ctx context.Context, id string, at time.Time)
}

type s3KeyEntry struct {
	key    meta.S3Key
	secret string
	read   time.Time
}

// s3Keys is the per-node cache of keys read.
type s3Keys struct {
	mu sync.Mutex
	m  map[string]s3KeyEntry
}

func (a *Authenticator) s3KeyStore() (S3KeyStore, bool) {
	st, ok := a.tokens.(S3KeyStore)
	return st, ok
}

var s3IDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// CreateS3Key issues a key to owner (Principal.Owner), who must be a
// principal now, and returns it with its secret. A zero expires never
// expires.
func (a *Authenticator) CreateS3Key(ctx context.Context, owner, name string, expires time.Time) (meta.S3Key, string, error) {
	st, ok := a.s3KeyStore()
	if !ok {
		return meta.S3Key{}, "", errors.New("auth: the token store cannot hold S3 keys")
	}
	if _, ok := a.lookup(ctx, owner); !ok {
		return meta.S3Key{}, "", fmt.Errorf("%w: %q", ErrUserDisabled, owner)
	}
	have, err := st.ListS3Keys(ctx, owner)
	if err != nil {
		return meta.S3Key{}, "", err
	}
	if len(have) >= MaxS3KeysPerUser {
		return meta.S3Key{}, "", ErrTooManyS3Keys
	}
	now := a.clock().UTC().Truncate(time.Millisecond)
	for range 5 { // an id clash is all but impossible: draw again
		id := make([]byte, 11)
		sec := make([]byte, 30)
		rand.Read(id)
		rand.Read(sec)
		k := meta.S3Key{AccessKeyID: "CCK" + s3IDEncoding.EncodeToString(id)[:17], Owner: owner, Name: name,
			Created: now, Expires: expires.UTC().Truncate(time.Millisecond)}
		secret := base64.StdEncoding.EncodeToString(sec)
		if err = st.PutS3Key(ctx, k, secret); !errors.Is(err, meta.ErrExists) {
			if err != nil {
				return meta.S3Key{}, "", err
			}
			return k, secret, nil
		}
	}
	return meta.S3Key{}, "", err
}

// ListS3Keys returns owner's keys, or every key for owner "".
func (a *Authenticator) ListS3Keys(ctx context.Context, owner string) ([]meta.S3Key, error) {
	st, ok := a.s3KeyStore()
	if !ok {
		return []meta.S3Key{}, nil
	}
	return st.ListS3Keys(ctx, owner)
}

// DeleteS3Key removes a key and forgets it on this node at once (other
// nodes within s3KeyTTL); meta.ErrNotFound if there is none.
func (a *Authenticator) DeleteS3Key(ctx context.Context, id string) (meta.S3Key, error) {
	st, ok := a.s3KeyStore()
	if !ok {
		return meta.S3Key{}, meta.ErrNotFound
	}
	k, err := st.DeleteS3Key(ctx, id)
	a.s3.mu.Lock()
	delete(a.s3.m, id)
	a.s3.mu.Unlock()
	return k, err
}

// s3Key returns the key with id and its secret, from the cache while it
// is fresh.
func (a *Authenticator) s3Key(ctx context.Context, id string) (meta.S3Key, string, error) {
	now := a.clock()
	a.s3.mu.Lock()
	e, ok := a.s3.m[id]
	a.s3.mu.Unlock()
	if ok && now.Sub(e.read) < s3KeyTTL {
		return e.key, e.secret, nil
	}
	st, ok := a.s3KeyStore()
	if !ok {
		return meta.S3Key{}, "", ErrS3KeyUnknown
	}
	k, secret, err := st.S3KeyByID(ctx, id)
	if errors.Is(err, meta.ErrNotFound) {
		return meta.S3Key{}, "", ErrS3KeyUnknown
	}
	if err != nil {
		return meta.S3Key{}, "", err
	}
	a.s3.mu.Lock()
	if a.s3.m == nil || len(a.s3.m) > 10000 {
		a.s3.m = map[string]s3KeyEntry{}
	}
	a.s3.m[id] = s3KeyEntry{key: k, secret: secret, read: now}
	a.s3.mu.Unlock()
	return k, secret, nil
}

// S3Check authenticates a request signed with the access key id: verify
// checks the signature with the key's secret. The check runs under the
// sign-in throttle of the id, like a password. It returns ErrS3KeyUnknown
// for an id no key has, ErrS3KeyExpired, verify's error when the signature
// fails, ErrBadCredentials when the owner is no longer a principal, or a
// *ThrottledError.
func (a *Authenticator) S3Check(ctx context.Context, id string, verify func(secret string) error) (Principal, error) {
	k, secret, err := a.s3Key(ctx, id)
	if err != nil {
		return Principal{}, err
	}
	if !k.Expires.IsZero() && !a.clock().Before(k.Expires) {
		return Principal{}, ErrS3KeyExpired
	}
	var verr error
	p, err := a.throttle(ctx, id, func(bool) (Principal, bool) {
		if verr = verify(secret); verr != nil {
			return Principal{}, false
		}
		return a.lookup(ctx, k.Owner)
	})
	if err != nil {
		if verr != nil && errors.Is(err, ErrBadCredentials) {
			return Principal{}, verr
		}
		return Principal{}, err
	}
	if st, ok := a.s3KeyStore(); ok && a.clock().Sub(k.LastUsed) >= meta.TokenTouchInterval {
		at := a.clock()
		st.TouchS3Key(ctx, id, at)
		a.s3.mu.Lock()
		if e, ok := a.s3.m[id]; ok {
			e.key.LastUsed = at
			a.s3.m[id] = e
		}
		a.s3.mu.Unlock()
	}
	p.Via = ViaToken
	return p, nil
}
