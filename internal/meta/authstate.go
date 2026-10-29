package meta

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Auth state in the database, so that every node knows it and it survives a
// restart (HA design 2.4, 2.5): UI sessions, pending SSO logins, the SAML
// assertions already used, the sign-in failures of each name and the
// identities SSO realms asserted. No transaction that checks an invariant
// reads these tables, so their writes skip the write lock
// (execFree, or a write through QueryRowContext, as acquireLease).

const (
	// SessionMaxAge is a UI session's absolute lifetime, and how long an SSO
	// identity is trusted after the login that asserted it.
	SessionMaxAge = 7 * 24 * time.Hour
	// LoginStateTTL is how long a started SSO login may take.
	LoginStateTTL = 10 * time.Minute
	// AuthFailureForget is how long a name's sign-in failures count
	// without another one.
	AuthFailureForget = time.Hour

	authPruneEvery = time.Minute
)

// loginStatesMax is how many logins may be pending; a start beyond them is
// refused. A variable for the tests.
var loginStatesMax = 10000

// Session is a UI session row, keyed by the hash of its id.
type Session struct {
	Owner             string
	Created, LastSeen time.Time
}

// CreateSession stores a session of owner, then deletes the owner's
// expired sessions (idle, or older than SessionMaxAge) and, beyond keep,
// its least recently used ones.
func (d *DB) CreateSession(ctx context.Context, key, owner string, now time.Time, idle time.Duration, keep int) error {
	if _, err := d.sql.execFree(ctx, `INSERT INTO sessions(key, owner, created_at, last_seen) VALUES(?, ?, ?, ?)`,
		key, owner, toMs(now), toMs(now)); err != nil {
		return err
	}
	_, err := d.sql.execFree(ctx, `DELETE FROM sessions WHERE owner = ? AND key <> ? AND (created_at < ? OR last_seen < ? OR key NOT IN
  (SELECT key FROM sessions WHERE owner = ? AND key <> ? ORDER BY last_seen DESC, created_at DESC, key LIMIT ?))`,
		owner, key, toMs(now.Add(-SessionMaxAge)), toMs(now.Add(-idle)), owner, key, keep-1)
	return err
}

// GetSession returns the session stored under key, or ErrNotFound.
func (d *DB) GetSession(ctx context.Context, key string) (Session, error) {
	var s Session
	var created, last int64
	err := d.sql.QueryRowContext(ctx, `SELECT owner, created_at, last_seen FROM sessions WHERE key = ?`, key).Scan(&s.Owner, &created, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	s.Created, s.LastSeen = fromMs(created), fromMs(last)
	return s, err
}

// TouchSession records a use of the session at now unless one since
// now-every is recorded, so busy browsers do not write on every request.
func (d *DB) TouchSession(ctx context.Context, key string, now time.Time, every time.Duration) error {
	_, err := d.sql.execFree(ctx, `UPDATE sessions SET last_seen = ? WHERE key = ? AND last_seen < ?`, toMs(now), key, toMs(now.Add(-every)))
	return err
}

// DeleteSession deletes the session stored under key, if any.
func (d *DB) DeleteSession(ctx context.Context, key string) error {
	_, err := d.sql.execFree(ctx, `DELETE FROM sessions WHERE key = ?`, key)
	return err
}

// DeleteSessions deletes the sessions of owners created before before, or
// all of them when before is zero.
func (d *DB) DeleteSessions(ctx context.Context, before time.Time, owners ...string) error {
	if len(owners) == 0 {
		return nil
	}
	q := `DELETE FROM sessions WHERE owner IN (?` + strings.Repeat(", ?", len(owners)-1) + `)`
	args := make([]any, 0, len(owners)+1)
	for _, o := range owners {
		args = append(args, o)
	}
	if !before.IsZero() {
		q += ` AND created_at < ?`
		args = append(args, toMs(before))
	}
	_, err := d.sql.execFree(ctx, q, args...)
	return err
}

// LoginState is a pending SSO login: OIDC's nonce and PKCE verifier, or
// SAML's AuthnRequest ID as Nonce, and the UI location to return to.
type LoginState struct{ Nonce, Verifier, Next string }

// PutLoginState stores the pending login of realm under state. It reports
// false, storing nothing, while loginStatesMax unexpired logins are pending.
func (d *DB) PutLoginState(ctx context.Context, realm, state string, st LoginState, now time.Time) (bool, error) {
	for pruned := false; ; pruned = true {
		var n int
		if err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM login_states`).Scan(&n); err != nil {
			return false, err
		}
		if n < loginStatesMax {
			break
		}
		if pruned {
			return false, nil
		}
		if _, err := d.sql.execFree(ctx, `DELETE FROM login_states WHERE created_at < ?`, toMs(now.Add(-LoginStateTTL))); err != nil {
			return false, err
		}
	}
	_, err := d.sql.execFree(ctx, `INSERT INTO login_states(state, realm, nonce, verifier, next, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
		state, realm, st.Nonce, st.Verifier, st.Next, toMs(now))
	return err == nil, err
}

// TakeLoginState deletes and returns the pending login of realm under
// state in one statement, so of any number of nodes one gets it. A missing
// or expired one is ErrNotFound.
func (d *DB) TakeLoginState(ctx context.Context, realm, state string, now time.Time) (LoginState, error) {
	var st LoginState
	var created int64
	err := d.sql.QueryRowContext(ctx, `DELETE FROM login_states WHERE state = ? AND realm = ? RETURNING nonce, verifier, next, created_at`,
		state, realm).Scan(&st.Nonce, &st.Verifier, &st.Next, &created)
	if errors.Is(err, sql.ErrNoRows) || err == nil && now.Sub(fromMs(created)) > LoginStateTTL {
		return LoginState{}, ErrNotFound
	}
	return st, err
}

// FirstAssertion records the SAML assertion id until exp and reports
// whether it is new: false for one any node recorded that has not expired
// (a replay). The id is stored as its SHA-256.
func (d *DB) FirstAssertion(ctx context.Context, id string, exp, now time.Time) (bool, error) {
	sum := sha256.Sum256([]byte(id))
	res, err := d.sql.execFree(ctx, `INSERT INTO saml_replay(id, expires_at) VALUES(?, ?)
ON CONFLICT(id) DO UPDATE SET expires_at = excluded.expires_at WHERE saml_replay.expires_at <= ?`,
		hex.EncodeToString(sum[:]), toMs(exp), toMs(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// AuthFailures returns the sign-in failures counted for name (a lower-case
// login name) and the time of the latest; 0 when there are none.
func (d *DB) AuthFailures(ctx context.Context, name string) (int, time.Time, error) {
	var n int
	var last int64
	err := d.sql.QueryRowContext(ctx, `SELECT fails, last_fail FROM auth_failures WHERE key = ?`, name).Scan(&n, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, nil
	}
	return n, fromMs(last), err
}

// AddAuthFailure counts a failure of name at now, starting again from one
// when the latest is older than AuthFailureForget, and returns the count.
func (d *DB) AddAuthFailure(ctx context.Context, name string, now time.Time) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, `INSERT INTO auth_failures(key, fails, last_fail) VALUES(?, 1, ?)
ON CONFLICT(key) DO UPDATE SET fails = CASE WHEN auth_failures.last_fail < ? THEN 1 ELSE auth_failures.fails + 1 END,
  last_fail = excluded.last_fail
RETURNING fails`, name, toMs(now), toMs(now.Add(-AuthFailureForget))).Scan(&n)
	return n, err
}

// ClearAuthFailures forgets the failures of name.
func (d *DB) ClearAuthFailures(ctx context.Context, name string) error {
	_, err := d.sql.execFree(ctx, `DELETE FROM auth_failures WHERE key = ?`, name)
	return err
}

// Identity is what an SSO realm asserted about user ID: Data is the
// caller's JSON, Conf a hash of the realm configuration it was asserted
// under (a row of another configuration is never returned) and At the
// login, or directory read, that asserted it.
type Identity struct {
	ID, Conf, Data string
	At             time.Time
}

// PutIdentity stores realm's identity id.ID, replacing an older one.
func (d *DB) PutIdentity(ctx context.Context, realm string, id Identity) error {
	_, err := d.sql.execFree(ctx, `INSERT INTO sso_identities(realm, id, conf, data, checked_at) VALUES(?, ?, ?, ?, ?)
ON CONFLICT(realm, id) DO UPDATE SET conf = excluded.conf, data = excluded.data, checked_at = excluded.checked_at`,
		realm, id.ID, id.Conf, id.Data, toMs(id.At))
	return err
}

// GetIdentity returns realm's identity id stored under configuration conf,
// or ErrNotFound.
func (d *DB) GetIdentity(ctx context.Context, realm, id, conf string) (Identity, error) {
	out := Identity{ID: id, Conf: conf}
	var at int64
	err := d.sql.QueryRowContext(ctx, `SELECT data, checked_at FROM sso_identities WHERE realm = ? AND id = ? AND conf = ?`,
		realm, id, conf).Scan(&out.Data, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrNotFound
	}
	out.At = fromMs(at)
	return out, err
}

// ListIdentities returns realm's identities stored under conf and asserted
// after since, ordered by id.
func (d *DB) ListIdentities(ctx context.Context, realm, conf string, since time.Time) ([]Identity, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, data, checked_at FROM sso_identities WHERE realm = ? AND conf = ? AND checked_at > ? ORDER BY id`,
		realm, conf, toMs(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		x := Identity{Conf: conf}
		var at int64
		if err := rows.Scan(&x.ID, &x.Data, &at); err != nil {
			return nil, err
		}
		x.At = fromMs(at)
		out = append(out, x)
	}
	return out, rows.Err()
}

// DeleteIdentity deletes realm's identity id; realm "" deletes it in every
// realm.
func (d *DB) DeleteIdentity(ctx context.Context, realm, id string) error {
	if realm == "" {
		_, err := d.sql.execFree(ctx, `DELETE FROM sso_identities WHERE id = ?`, id)
		return err
	}
	_, err := d.sql.execFree(ctx, `DELETE FROM sso_identities WHERE realm = ? AND id = ?`, realm, id)
	return err
}

// pruneAuthState deletes the auth state that can no longer be used: the
// scheduler lease holder runs it (beat).
func (d *DB) pruneAuthState(ctx context.Context, now time.Time) error {
	for _, p := range []struct {
		q  string
		at time.Time
	}{
		{`DELETE FROM sessions WHERE created_at < ?`, now.Add(-SessionMaxAge)}, // idle ones go when used, or with their owner's next login
		{`DELETE FROM login_states WHERE created_at < ?`, now.Add(-LoginStateTTL)},
		{`DELETE FROM saml_replay WHERE expires_at <= ?`, now},
		{`DELETE FROM auth_failures WHERE last_fail < ?`, now.Add(-AuthFailureForget)},
		{`DELETE FROM sso_identities WHERE checked_at < ?`, now.Add(-SessionMaxAge)},
	} {
		if _, err := d.sql.execFree(ctx, p.q, toMs(p.at)); err != nil {
			return err
		}
	}
	return nil
}

// settingTokenKey holds the key that signs the Docker and Conan bearer
// tokens, sealed like the other secrets; settingKeyCheck the HMAC of the
// secret key (keyCheck). Neither is a general setting: LoadSettings, and
// older builds, ignore them.
const (
	settingTokenKey = "registry.tokenKey"
	settingKeyCheck = "secretKey.check"
)

// TokenKey returns the 32-byte key that signs the Docker and Conan bearer
// tokens, the same on every node: the first call on a database stores a
// random one (one node's insert wins), sealed with the secret key.
func (d *DB) TokenKey(ctx context.Context) ([]byte, error) {
	var v string
	found, err := getSettingJSON(ctx, d.sql, settingTokenKey, &v)
	if err == nil && !found {
		b := make([]byte, 32)
		rand.Read(b)
		var sealed string
		if sealed, err = d.box.seal(base64.StdEncoding.EncodeToString(b)); err != nil {
			return nil, err
		}
		js, _ := json.Marshal(sealed)
		if _, err = d.sql.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO NOTHING`, settingTokenKey, string(js)); err != nil {
			return nil, err
		}
		found, err = getSettingJSON(ctx, d.sql, settingTokenKey, &v)
	}
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("the registry token key was not stored")
	}
	plain, err := d.box.open(v)
	if err != nil {
		return nil, fmt.Errorf("setting %q: %w", settingTokenKey, err)
	}
	key, err := base64.StdEncoding.DecodeString(plain)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("setting %q does not hold 32 bytes in base64", settingTokenKey)
	}
	return key, nil
}

// keyCheck is the check value of a secret key: it identifies the key
// without giving it away.
func keyCheck(key []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("commandcenter key check v1"))
	return hex.EncodeToString(m.Sum(nil))
}

// CheckSecretKey makes every node of the database use one secret key. The
// first start with a key stores its check value (keyCheck); a later start
// is refused with another key, or with none, even while no secret is
// sealed yet, as nodes would otherwise seal with different keys.
func (d *DB) CheckSecretKey(ctx context.Context) error {
	var stored string
	found, err := getSettingJSON(ctx, d.sql, settingKeyCheck, &stored)
	if err == nil && !found && d.box.aead != nil {
		js, _ := json.Marshal(d.box.check)
		if _, err = d.sql.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO NOTHING`, settingKeyCheck, string(js)); err == nil {
			found, err = getSettingJSON(ctx, d.sql, settingKeyCheck, &stored)
		}
	}
	switch {
	case err != nil:
		return err
	case !found:
		return nil
	case d.box.aead == nil:
		return errors.New("this database was started with a secret key, and no secret key is set: set " +
			"COMMANDCENTER_SECRET_KEY or secretKeyFile to that key on every node")
	case !hmac.Equal([]byte(stored), []byte(d.box.check)):
		return errors.New("the secret key is another key than the one this database was started with (settings key " +
			settingKeyCheck + "): every node must use the same key; to change it, stop every node and run \"commandcenter secret-key rotate\"")
	}
	return nil
}
