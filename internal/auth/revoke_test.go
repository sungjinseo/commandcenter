package auth

import (
	"context"
	"errors"
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
