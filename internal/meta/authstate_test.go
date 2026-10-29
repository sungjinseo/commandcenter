package meta

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/meta/metatest"
)

func count(t *testing.T, db *DB, table string) int {
	t.Helper()
	var n int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A pending login is taken once, by its realm, before it expires; while
// loginStatesMax are pending, a start is refused once the expired ones are
// gone.
func TestLoginStates(t *testing.T) {
	db := openTest(t)
	now := time.UnixMilli(1_700_000_000_000)
	want := LoginState{Nonce: "n", Verifier: "v", Next: "/next"}
	if ok, err := db.PutLoginState(ctx, "oidc", "st", want, now); !ok || err != nil {
		t.Fatalf("put: %v %v", ok, err)
	}
	if _, err := db.TakeLoginState(ctx, "saml", "st", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another realm: %v", err)
	}
	if got, err := db.TakeLoginState(ctx, "oidc", "st", now.Add(LoginStateTTL)); got != want || err != nil {
		t.Fatalf("take: %+v %v", got, err)
	}
	if _, err := db.TakeLoginState(ctx, "oidc", "st", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second take: %v", err)
	}
	db.PutLoginState(ctx, "oidc", "late", want, now)
	if _, err := db.TakeLoginState(ctx, "oidc", "late", now.Add(LoginStateTTL+time.Millisecond)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired: %v", err)
	}

	max := loginStatesMax
	loginStatesMax = 2
	t.Cleanup(func() { loginStatesMax = max })
	db.PutLoginState(ctx, "oidc", "old", want, now.Add(-LoginStateTTL-time.Second))
	db.PutLoginState(ctx, "oidc", "s2", want, now)
	if ok, err := db.PutLoginState(ctx, "oidc", "s3", want, now); !ok || err != nil { // "old" pruned
		t.Fatalf("put beside an expired one: %v %v", ok, err)
	}
	if ok, err := db.PutLoginState(ctx, "oidc", "s4", want, now); ok || err != nil {
		t.Fatalf("put beyond the cap: %v %v", ok, err)
	}
	if _, err := db.TakeLoginState(ctx, "oidc", "s4", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refused login stored: %v", err)
	}
}

// Of nodes taking one pending login at once, one gets it.
func TestTakeLoginStateOnce(t *testing.T) {
	a, b, _ := twoNodes(t)
	now := time.Now()
	for i := range 5 {
		st := "st" + string(rune('a'+i))
		a.PutLoginState(ctx, "saml", st, LoginState{Nonce: "n"}, now)
		var won atomic.Int32
		var wg sync.WaitGroup
		for j := range 8 {
			wg.Go(func() {
				db := a
				if j%2 == 1 {
					db = b
				}
				if _, err := db.TakeLoginState(ctx, "saml", st, now); err == nil {
					won.Add(1)
				} else if !errors.Is(err, ErrNotFound) {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if won.Load() != 1 {
			t.Fatalf("%d nodes took %s", won.Load(), st)
		}
	}
}

// An assertion id is new once on any node until it expires.
func TestFirstAssertion(t *testing.T) {
	a, b, _ := twoNodes(t)
	now := time.Now()
	exp := now.Add(time.Minute)
	var first atomic.Int32
	var wg sync.WaitGroup
	for j := range 10 {
		wg.Go(func() {
			db := a
			if j%2 == 1 {
				db = b
			}
			ok, err := db.FirstAssertion(ctx, "id-1", exp, now)
			if err != nil {
				t.Error(err)
			}
			if ok {
				first.Add(1)
			}
		})
	}
	wg.Wait()
	if first.Load() != 1 {
		t.Fatalf("%d nodes saw id-1 first", first.Load())
	}
	if ok, err := b.FirstAssertion(ctx, "id-2", exp, now); !ok || err != nil {
		t.Fatalf("another id: %v %v", ok, err)
	}
	if ok, err := a.FirstAssertion(ctx, "id-1", exp.Add(time.Hour), exp); !ok || err != nil {
		t.Fatalf("after it expired: %v %v", ok, err)
	}
	if ok, _ := b.FirstAssertion(ctx, "id-1", exp.Add(time.Hour), exp); ok {
		t.Fatal("replay of the renewed id")
	}
}

// Failures count on every node, start again after AuthFailureForget and
// clear.
func TestAuthFailures(t *testing.T) {
	a, b, _ := twoNodes(t)
	now := time.UnixMilli(1_700_000_000_000)
	if n, last, err := a.AuthFailures(ctx, "alice"); n != 0 || !last.IsZero() || err != nil {
		t.Fatalf("none: %d %v %v", n, last, err)
	}
	var wg sync.WaitGroup
	for j := range 10 {
		wg.Go(func() {
			db := a
			if j%2 == 1 {
				db = b
			}
			if _, err := db.AddAuthFailure(ctx, "alice", now); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n, last, err := b.AuthFailures(ctx, "alice"); n != 10 || !last.Equal(now) || err != nil {
		t.Fatalf("after 10: %d %v %v", n, last, err)
	}
	if n, _ := a.AddAuthFailure(ctx, "alice", now.Add(AuthFailureForget)); n != 11 {
		t.Fatalf("within the hour: %d", n)
	}
	if n, _ := a.AddAuthFailure(ctx, "alice", now.Add(2*AuthFailureForget+time.Millisecond)); n != 1 {
		t.Fatalf("after an hour without one: %d", n)
	}
	a.AddAuthFailure(ctx, "bob", now)
	if err := b.ClearAuthFailures(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := a.AuthFailures(ctx, "alice"); n != 0 {
		t.Fatalf("cleared: %d", n)
	}
	if n, _, _ := a.AuthFailures(ctx, "bob"); n != 1 {
		t.Fatalf("bob: %d", n)
	}
}

// An identity is returned only under the configuration it was stored
// with.
func TestIdentities(t *testing.T) {
	db := openTest(t)
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	alice := Identity{ID: "alice", Conf: "c1", Data: `{"name":"Alice"}`, At: t0}
	if err := db.PutIdentity(ctx, "oidc", alice); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetIdentity(ctx, "oidc", "alice", "c1"); got != alice || err != nil {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := db.GetIdentity(ctx, "oidc", "alice", "c2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another configuration: %v", err)
	}
	alice.Conf, alice.At = "c2", t0.Add(time.Second)
	db.PutIdentity(ctx, "oidc", alice)
	if _, err := db.GetIdentity(ctx, "oidc", "alice", "c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replaced: %v", err)
	}
	db.PutIdentity(ctx, "oidc", Identity{ID: "bob", Conf: "c2", Data: `{}`, At: t0})
	db.PutIdentity(ctx, "saml", Identity{ID: "alice", Conf: "c2", Data: `{}`, At: t0})
	if got, err := db.ListIdentities(ctx, "oidc", "c2", t0); len(got) != 1 || got[0] != alice || err != nil {
		t.Fatalf("list since t0: %+v %v", got, err)
	}
	if got, _ := db.ListIdentities(ctx, "oidc", "c2", t0.Add(-time.Millisecond)); len(got) != 2 || got[0].ID != "alice" || got[1].ID != "bob" {
		t.Fatalf("list: %+v", got)
	}
	db.DeleteIdentity(ctx, "saml", "alice")
	if _, err := db.GetIdentity(ctx, "oidc", "alice", "c2"); err != nil {
		t.Fatalf("deleted in saml, gone in oidc: %v", err)
	}
	db.PutIdentity(ctx, "saml", Identity{ID: "alice", Conf: "c2", Data: `{}`, At: t0})
	db.DeleteIdentity(ctx, "", "alice")
	if n := count(t, db, "sso_identities"); n != 1 {
		t.Fatalf("after deleting alice everywhere: %d rows", n)
	}
}

// Pruning deletes the state that can no longer be used, and only that.
func TestPruneAuthState(t *testing.T) {
	db := openTest(t)
	now := time.Now()
	old := now.Add(-SessionMaxAge - time.Minute)
	db.CreateSession(ctx, "s-old", "a", old, 30*24*time.Hour, 20)
	db.CreateSession(ctx, "s-new", "b", now, 30*24*time.Hour, 20)
	db.PutLoginState(ctx, "oidc", "old", LoginState{}, now.Add(-LoginStateTTL-time.Second))
	db.PutLoginState(ctx, "oidc", "new", LoginState{}, now)
	db.FirstAssertion(ctx, "old", now.Add(-time.Second), now.Add(-time.Hour))
	db.FirstAssertion(ctx, "new", now.Add(time.Hour), now)
	db.AddAuthFailure(ctx, "old", now.Add(-AuthFailureForget-time.Second))
	db.AddAuthFailure(ctx, "new", now)
	db.PutIdentity(ctx, "oidc", Identity{ID: "old", Data: `{}`, At: old})
	db.PutIdentity(ctx, "oidc", Identity{ID: "new", Data: `{}`, At: now})
	if err := db.pruneAuthState(ctx, now); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"sessions", "login_states", "saml_replay", "auth_failures", "sso_identities"} {
		if n := count(t, db, table); n != 1 {
			t.Errorf("%s: %d rows, want the new one", table, n)
		}
	}
	if _, err := db.GetSession(ctx, "s-new"); err != nil {
		t.Fatalf("new session: %v", err)
	}
}

func openKey(t *testing.T, target string, key byte) *DB { // key 0: none
	t.Helper()
	db, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if key != 0 {
		if err := db.SetSecretKey(bytes.Repeat([]byte{key}, 32)); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// Every node gets the one token key, sealed with the secret key and
// sealed again by a rotation.
func TestTokenKey(t *testing.T) {
	a, b, _ := twoNodes(t)
	keys := make([][]byte, 8)
	var wg sync.WaitGroup
	for j := range keys {
		wg.Go(func() {
			db := a
			if j%2 == 1 {
				db = b
			}
			k, err := db.TokenKey(ctx)
			if err != nil {
				t.Error(err)
			}
			keys[j] = k
		})
	}
	wg.Wait()
	for _, k := range keys {
		if len(k) != 32 || !bytes.Equal(k, keys[0]) {
			t.Fatalf("keys differ: %x", keys)
		}
	}

	target := metatest.Target(t, filepath.Join(t.TempDir(), "sealed.db"))
	k1 := openKey(t, target, 1)
	want, err := k1.TokenKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if raw := rawRow(t, k1, `SELECT value FROM settings WHERE key = ?`, settingTokenKey); !strings.Contains(raw, sealedPrefix) {
		t.Fatalf("not sealed: %s", raw)
	}
	if _, err := openKey(t, target, 0).TokenKey(ctx); err == nil {
		t.Fatal("opened without the key")
	}
	if _, _, err := k1.RotateSecretKey(ctx, bytes.Repeat([]byte{2}, 32)); err != nil {
		t.Fatal(err)
	}
	if got, err := openKey(t, target, 2).TokenKey(ctx); !bytes.Equal(got, want) || err != nil {
		t.Fatalf("after rotation: %x %v", got, err)
	}
}

// On PostgreSQL every start uses the key of the first start with one; a
// rotation changes it.
func TestCheckSecretKey(t *testing.T) {
	target := metatest.Target(t, filepath.Join(t.TempDir(), "meta.db"))
	if !metatest.Postgres() {
		if err := openKey(t, target, 1).CheckSecretKey(ctx); err != nil {
			t.Fatal(err)
		}
		if err := openKey(t, target, 2).CheckSecretKey(ctx); err != nil {
			t.Fatalf("SQLite checks no key: %v", err)
		}
		return
	}
	check := func(key byte, want string) *DB {
		t.Helper()
		db := openKey(t, target, key)
		err := db.CheckSecretKey(ctx)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Fatalf("key %d: %v, want %q", key, err, want)
		}
		return db
	}
	check(0, "") // nothing to check yet
	check(1, "")
	check(2, "another key")
	check(0, "no secret key is set")
	a := check(1, "")
	if _, _, err := a.RotateSecretKey(ctx, bytes.Repeat([]byte{2}, 32)); err != nil {
		t.Fatal(err)
	}
	check(2, "")
	check(1, "another key")
}
