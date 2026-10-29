package auth

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
)

type userTokenFixture struct {
	*accessFixture
	a     *Authenticator
	db    *meta.DB
	clock time.Time
}

func newUserTokenFixture(t *testing.T) *userTokenFixture {
	t.Helper()
	db, err := meta.Open(metatest.Target(t, filepath.Join(t.TempDir(), "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &userTokenFixture{accessFixture: newFixture(t), db: db, clock: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	f.settings.UserTokens = meta.UserTokens{Enabled: true, ExpirationDays: 30}
	f.a = New(&config.Config{}, db)
	f.a.now = func() time.Time { return f.clock }
	f.apply()
	return f
}

func (f *userTokenFixture) apply() { f.a.setSnapshot(NewSnapshot(f.users, f.roles, f.settings)) }

func (f *userTokenFixture) basic(path, user, pass string) (Principal, error) {
	r := httptest.NewRequest("GET", path, nil)
	r.SetBasicAuth(user, pass)
	return f.a.Authenticate(r)
}

func TestUserTokenBasicAuth(t *testing.T) {
	f := newUserTokenFixture(t)
	ctx := context.Background()
	name, pass, err := f.a.CreateUserToken(ctx, "adder")
	if err != nil || len(name) != 8 || len(pass) != 44 || !userTokenShaped(name, pass) {
		t.Fatalf("create: %q %q %v", name, pass, err)
	}
	p, err := f.basic("/repository/releases/x", name, pass)
	if err != nil || p.User != "adder" || p.Via != ViaToken || p.Owner() != "adder" {
		t.Fatalf("codes: %+v %v", p, err)
	}
	if got, _ := f.db.GetUserToken(ctx, "adder"); got.LastUsed.IsZero() || got.NameCode != name {
		t.Fatalf("stored %+v", got)
	}
	// A wrong pass code, and an unknown name code (then a password check),
	// fail; the user's password still works.
	if _, err := f.basic("/repository/releases/x", name, wrongCode(pass)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong pass code: %v", err)
	}
	if _, err := f.basic("/repository/releases/x", "AAAAAAAA", pass); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("unknown name code: %v", err)
	}
	if p, err := f.basic("/repository/releases/x", "adder", "pw"); err != nil || p.Via != ViaPassword {
		t.Fatalf("password: %+v %v", p, err)
	}

	// Expiry follows the live setting and the creation time.
	f.clock = f.clock.Add(30 * 24 * time.Hour)
	if _, err := f.basic("/repository/releases/x", name, pass); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("expired token: %v", err)
	}
	f.settings.UserTokens.ExpirationDays = 0
	f.apply()
	if _, err := f.basic("/repository/releases/x", name, pass); err != nil {
		t.Fatalf("never expiring: %v", err)
	}

	// A reset replaces the codes.
	name2, pass2, err := f.a.CreateUserToken(ctx, "adder")
	if err != nil || name2 == name {
		t.Fatalf("reset: %q %v", name2, err)
	}
	if _, err := f.basic("/repository/releases/x", name, pass); err == nil {
		t.Fatal("old codes still work")
	}
	if _, err := f.basic("/repository/releases/x", name2, pass2); err != nil {
		t.Fatalf("new codes: %v", err)
	}

	// A disabled user's token fails at once, and the feature turned off
	// stops every token.
	f.users[1].Status = meta.StatusDisabled
	f.apply()
	if _, err := f.basic("/repository/releases/x", name2, pass2); err == nil {
		t.Fatal("disabled user's token works")
	}
	f.users[1].Status = meta.StatusActive
	f.settings.UserTokens.Enabled = false
	f.apply()
	if _, err := f.basic("/repository/releases/x", name2, pass2); err == nil {
		t.Fatal("token works with the feature off")
	}
	if _, _, err := f.a.CreateUserToken(ctx, "adder"); !errors.Is(err, ErrUserTokensDisabled) {
		t.Fatalf("create while off: %v", err)
	}
	f.settings.UserTokens.Enabled = true
	f.apply()
	if _, _, err := f.a.CreateUserToken(ctx, "off"); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("create for a disabled user: %v", err)
	}
	if _, _, err := f.a.CreateUserToken(ctx, "ldap:nobody"); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("create for an unknown principal: %v", err)
	}
}

func TestUserTokenRequired(t *testing.T) {
	f := newUserTokenFixture(t)
	ctx := context.Background()
	name, pass, err := f.a.CreateUserToken(ctx, "adder")
	if err != nil {
		t.Fatal(err)
	}
	f.settings.UserTokens.Required = true
	f.apply()
	for _, path := range []string{"/repository/releases/x", "/v2/", "/v2/app/manifests/1"} {
		if _, err := f.basic(path, "adder", "pw"); !errors.Is(err, ErrUserTokenRequired) || !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("%s password: %v", path, err)
		}
		if p, err := f.basic(path, name, pass); err != nil || p.User != "adder" {
			t.Fatalf("%s codes: %+v %v", path, p, err)
		}
	}
	// Everyone, admins too; the API and the UI login are unaffected.
	if _, err := f.basic("/repository/releases/x", "admin", "pw"); !errors.Is(err, ErrUserTokenRequired) {
		t.Fatalf("admin password: %v", err)
	}
	if p, err := f.basic("/service/rest/v1/repositories", "adder", "pw"); err != nil || p.Via != ViaPassword {
		t.Fatalf("API password: %+v %v", p, err)
	}
	if _, err := f.a.CheckPassword(ctx, "adder", "pw"); err != nil {
		t.Fatalf("UI login: %v", err)
	}
	// npm login: codes yes, password no.
	if _, err := f.a.CheckRepoPassword(ctx, "adder", "pw"); !errors.Is(err, ErrUserTokenRequired) {
		t.Fatalf("npm login password: %v", err)
	}
	if p, err := f.a.CheckRepoPassword(ctx, name, pass); err != nil || p.User != "adder" {
		t.Fatalf("npm login codes: %+v %v", p, err)
	}
	// Required only applies while enabled.
	f.settings.UserTokens.Enabled = false
	f.apply()
	if _, err := f.basic("/repository/releases/x", "adder", "pw"); err != nil {
		t.Fatalf("required while disabled: %v", err)
	}
}

func TestUserTokenThrottled(t *testing.T) {
	f := newUserTokenFixture(t)
	name, pass, err := f.a.CreateUserToken(context.Background(), "adder")
	if err != nil {
		t.Fatal(err)
	}
	bad := wrongCode(pass)
	for range accountLimit {
		if _, err := f.basic("/repository/releases/x", name, bad); !errors.Is(err, ErrBadCredentials) {
			t.Fatal(err)
		}
	}
	var te *ThrottledError
	if _, err := f.basic("/repository/releases/x", name, pass); !errors.As(err, &te) {
		t.Fatalf("after %d failures: %v", accountLimit, err)
	}
	// The owner's password sign-in is a name of its own.
	if _, err := f.basic("/repository/releases/x", "adder", "pw"); err != nil {
		t.Fatalf("password: %v", err)
	}
	f.clock = f.clock.Add(refuseFirst + time.Second)
	if _, err := f.basic("/repository/releases/x", name, pass); err != nil {
		t.Fatalf("after the refusal: %v", err)
	}
}

// wrongCode changes the last character of a code.
func wrongCode(s string) string {
	if s[len(s)-1] == 'A' {
		return s[:len(s)-1] + "B"
	}
	return s[:len(s)-1] + "A"
}
