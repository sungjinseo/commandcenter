package repo

import (
	"context"
	"net/http/httptest"
	"testing"

	"commandcenter/internal/auth"
	"commandcenter/internal/ldap"
	"commandcenter/internal/meta"
)

type accessSource struct{ data meta.AccessData }

func (s accessSource) LoadAccess(context.Context) (meta.AccessData, error) { return s.data, nil }

type fakeDirectory struct{}

func (fakeDirectory) Authenticate(_ context.Context, _ meta.LDAP, name, _ string) (ldap.User, error) {
	return ldap.User{ID: name}, nil
}

func (fakeDirectory) Find(_ context.Context, _ meta.LDAP, id string) (ldap.User, error) {
	return ldap.User{ID: id}, nil
}

type eventRec struct{ kind, actor string }

type recordingSink struct{ got []eventRec }

func (s *recordingSink) Emit(kind, actor, repo, subject, detail string) {
	s.got = append(s.got, eventRec{kind, actor})
}

// Store.Emit records the principal's Owner (final review): "ldap:<id>" for an
// LDAP user, the plain id for a stored user, "" for anonymous.
func TestEmitUsesPrincipalOwner(t *testing.T) {
	set := meta.DefaultSettings()
	set.Realms = []meta.Realm{{ID: meta.RealmLDAP, Enabled: true}}
	set.LDAP = meta.DefaultLDAP()
	set.LDAP.URL, set.LDAP.UserBaseDN = "ldap://ldap.example", "ou=people,dc=example"
	a, err := auth.Open(context.Background(), accessSource{meta.AccessData{Settings: set}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.LDAP = fakeDirectory{}
	p, err := a.CheckPassword(context.Background(), "alice", "secret")
	if err != nil || !p.IsLDAP() {
		t.Fatalf("ldap principal: %+v %v", p, err)
	}
	sink := &recordingSink{}
	s := &Store{Events: sink}
	r := httptest.NewRequest("PUT", "/repository/x/y", nil)
	s.Emit(r.WithContext(auth.WithPrincipal(r.Context(), p)), "asset.upload", "x", "y", "")
	s.Emit(r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{User: "bob"})), "asset.upload", "x", "y", "")
	s.Emit(r, "asset.upload", "x", "y", "")
	want := []eventRec{{"asset.upload", "ldap:alice"}, {"asset.upload", "bob"}, {"asset.upload", ""}}
	if len(sink.got) != 3 || sink.got[0] != want[0] || sink.got[1] != want[1] || sink.got[2] != want[2] {
		t.Fatalf("actors = %+v, want %+v", sink.got, want)
	}
}
