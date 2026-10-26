package auth

import (
	"context"
	"errors"
	"slices"
	"testing"

	"commandcenter/internal/meta"
)

func (m memTokens) DeleteTokenByHash(_ context.Context, hash string) error {
	if _, ok := m[hash]; !ok {
		return meta.ErrNotFound
	}
	delete(m, hash)
	return nil
}

func (m memTokens) DeleteUserTokens(_ context.Context, owners ...string) (int64, error) {
	var n int64
	for h, u := range m {
		if slices.Contains(owners, u) {
			delete(m, h)
			n++
		}
	}
	return n, nil
}

// QA N-16: RevokeUserTokens ends the tokens of the stored user and of the
// LDAP user with the same id, and nobody else's.
func TestRevokeUserTokens(t *testing.T) {
	f := newLDAPFixture(t)
	ctx := context.Background()
	f.users = append(f.users, meta.User{ID: "alice", PasswordHash: hash(t, "pw"), Source: meta.SourceLocal,
		Status: meta.StatusActive, Roles: []string{}})
	f.apply()
	_, local, err := f.a.CreateToken(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.a.CheckPassword(ctx, "alice", "alice-pw") // the directory's alice
	if err != nil || !p.IsLDAP() {
		t.Fatalf("ldap login: %+v %v", p, err)
	}
	_, ldapTok, err := f.a.CreateTokenFor(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	_, other, err := f.a.CreateToken(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := f.a.RevokeUserTokens(ctx, "alice"); err != nil || n != 2 {
		t.Fatalf("revoked %d, %v; want 2", n, err)
	}
	for _, tok := range []string{local, ldapTok} {
		if _, err := f.a.Authenticate(req("Bearer " + tok)); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("revoked token still authenticates: %v", err)
		}
	}
	if _, err := f.a.Authenticate(req("Bearer " + other)); err != nil {
		t.Fatalf("another user's token was revoked: %v", err)
	}
}

func TestRevokeTokenOwnOnly(t *testing.T) {
	a, toks := newAuth(t)
	ctx := context.Background()
	_, ciTok, err := a.CreateToken(ctx, "ci")
	if err != nil {
		t.Fatal(err)
	}
	_, adminTok, err := a.CreateToken(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ci, err := a.Authenticate(req("Bearer " + ciTok))
	if err != nil {
		t.Fatal(err)
	}
	// Another user's token, even as seen by an admin-less caller: not found.
	if err := a.RevokeToken(ctx, adminTok, ci); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("other user's token: %v", err)
	}
	if _, err := a.Authenticate(req("Bearer " + adminTok)); err != nil {
		t.Fatalf("other user's token must survive: %v", err)
	}
	if err := a.RevokeToken(ctx, "cc_unknown", ci); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("unknown token: %v", err)
	}
	if err := a.RevokeToken(ctx, ciTok, ci); err != nil {
		t.Fatal(err)
	}
	if _, ok := toks[hashToken(ciTok)]; ok {
		t.Fatal("token row still stored")
	}
	if _, err := a.Authenticate(req("Bearer " + ciTok)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("revoked token still authenticates: %v", err)
	}
	if err := a.RevokeToken(ctx, ciTok, ci); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
}

func TestRevokeTokenLDAPOwner(t *testing.T) {
	f := newLDAPFixture(t)
	ctx := context.Background()
	p, err := f.a.CheckPassword(ctx, "alice", "alice-pw")
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := f.a.CreateTokenFor(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	// A stored user named "alice" is not the owner "ldap:alice".
	if err := f.a.RevokeToken(ctx, tok, Principal{User: "alice"}); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("local namesake: %v", err)
	}
	got, err := f.a.Authenticate(req("Bearer " + tok))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.a.RevokeToken(ctx, tok, got); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.Authenticate(req("Bearer " + tok)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("revoked LDAP token still authenticates: %v", err)
	}
}

// The app's token store must revoke, or npm logout silently answers 404.
var _ TokenRevoker = (*meta.DB)(nil)
