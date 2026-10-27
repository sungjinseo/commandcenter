package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

type memTokens map[string]string // hash -> user

func (m memTokens) CreateToken(_ context.Context, _, user, hash string, _ time.Time) error {
	m[hash] = user
	return nil
}

func (m memTokens) TokenUser(_ context.Context, hash string) (string, time.Time, error) {
	if u, ok := m[hash]; ok {
		var last time.Time
		if ms, err := strconv.ParseInt(m["\x00last:"+hash], 10, 64); err == nil {
			last = time.UnixMilli(ms)
		}
		return u, last, nil
	}
	return "", time.Time{}, meta.ErrNotFound
}

func hash(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func newAuth(t *testing.T) (*Authenticator, memTokens) {
	f := false
	cfg := &config.Config{
		AnonymousRead: true,
		Users: []config.User{
			{Name: "admin", PasswordHash: hash(t, "adminpw"), Roles: []string{"admin"}},
			{Name: "ci", PasswordHash: hash(t, "cipw"), Roles: []string{"deployer"}},
			{Name: "reader", PasswordHash: hash(t, "readpw")},
		},
		Roles:        map[string]config.Role{"deployer": {Write: []string{"releases"}}},
		Repositories: []config.Repository{{Name: "releases"}, {Name: "private", AnonymousRead: &f}},
	}
	tok := memTokens{}
	return New(cfg, tok), tok
}

func req(header string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	return r
}

func TestAuthenticateBasic(t *testing.T) {
	a, _ := newAuth(t)
	r := req("")
	r.SetBasicAuth("ci", "cipw")
	for i := 0; i < 2; i++ { // second call uses the cache
		p, err := a.Authenticate(r)
		if err != nil || p.User != "ci" || p.Admin {
			t.Fatalf("p=%+v err=%v", p, err)
		}
	}
	r.SetBasicAuth("ci", "wrong")
	if _, err := a.Authenticate(r); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("err = %v", err)
	}
	r.SetBasicAuth("ghost", "x")
	if _, err := a.Authenticate(r); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("err = %v", err)
	}
}

func TestAuthenticateAnonymousAndUnknownScheme(t *testing.T) {
	a, _ := newAuth(t)
	p, err := a.Authenticate(req(""))
	if err != nil || !p.Anonymous() {
		t.Fatalf("p=%+v err=%v", p, err)
	}
	if _, err := a.Authenticate(req("Digest abc")); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("err = %v", err)
	}
}

func TestTokens(t *testing.T) {
	a, _ := newAuth(t)
	_, tok, err := a.CreateToken(context.Background(), "ci")
	if err != nil || !strings.HasPrefix(tok, "cc_") {
		t.Fatalf("tok=%q err=%v", tok, err)
	}
	for _, h := range []string{"Bearer " + tok, tok} { // npm uses Bearer, cargo sends the raw token
		p, err := a.Authenticate(req(h))
		if err != nil || p.User != "ci" {
			t.Fatalf("header %q: p=%+v err=%v", h, p, err)
		}
	}
	if _, err := a.Authenticate(req("Bearer cc_nope")); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := a.CreateToken(context.Background(), "ghost"); err == nil {
		t.Fatal("token for unknown user should fail")
	}
}

func TestCan(t *testing.T) {
	a, _ := newAuth(t)
	anon := Principal{}
	admin := lookup(t, a, "admin")
	ci := lookup(t, a, "ci")
	reader := lookup(t, a, "reader")
	cases := []struct {
		p    Principal
		repo string
		acc  Access
		want bool
	}{
		{anon, "releases", Read, true},
		{anon, "private", Read, false},
		{anon, "releases", Add, false},
		{anon, "private", Public, true},
		{reader, "private", Read, true},
		{reader, "releases", Add, false},
		{ci, "releases", Add, true},
		{ci, "releases", Edit, true}, // imported write grants every action
		{ci, "releases", Delete, true},
		{ci, "private", Add, false},
		{admin, "private", Delete, true},
	}
	for i, c := range cases {
		if got := a.Can(c.p, c.repo, c.acc); got != c.want {
			t.Errorf("case %d: Can(%q, %q, %v) = %v", i, c.p.User, c.repo, c.acc, got)
		}
	}
}

func TestContext(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{User: "u"})
	if FromContext(ctx).User != "u" || !FromContext(context.Background()).Anonymous() {
		t.Fatal("context round trip failed")
	}
}

func TestCheckPassword(t *testing.T) {
	a, _ := newAuth(t)
	if p, err := a.CheckPassword(context.Background(), "ci", "cipw"); err != nil || p.User != "ci" {
		t.Fatalf("p=%+v err=%v", p, err)
	}
	if _, err := a.CheckPassword(context.Background(), "ci", "nope"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownUserCostsABcryptCompare(t *testing.T) {
	a, _ := newAuth(t)
	start := time.Now()
	if _, err := a.CheckPassword(context.Background(), "ghost", "x"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d < 5*time.Millisecond {
		t.Fatalf("unknown user answered in %v; it must cost a bcrypt compare", d)
	}
	if cap(bcryptSlots) != 4 {
		t.Fatalf("bcrypt slots = %d, want 4", cap(bcryptSlots))
	}
}

// QA L-2: only a bcrypt hash is a password; a placeholder is not.
func TestHashUsable(t *testing.T) {
	h := hash(t, "pw")
	for _, ok := range []string{h, "$2y$" + strings.TrimPrefix(h, "$2a$")} { // $2y$: htpasswd -B
		if !HashUsable(ok) {
			t.Errorf("%q: not usable", ok)
		}
	}
	for _, bad := range []string{"", "x", "<commandcenter hash-password 출력값>", "$2a$10$tooshort", "$2a$99$" + strings.Repeat("a", 53)} {
		if HashUsable(bad) {
			t.Errorf("%q: usable", bad)
		}
	}
}

func TestCheckPasswordHonorsContextWhileQueued(t *testing.T) {
	a, _ := newAuth(t)
	for i := 0; i < cap(bcryptSlots); i++ {
		bcryptSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(bcryptSlots); i++ {
			<-bcryptSlots
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	for _, user := range []string{"ci", "ghost"} {
		start := time.Now()
		p, err := a.CheckPassword(ctx, user, "cipw")
		if !errors.Is(err, ErrBadCredentials) || !p.Anonymous() {
			t.Fatalf("%s: p=%+v err=%v, want ErrBadCredentials", user, p, err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("%s: returned after %v", user, d)
		}
	}
}

func TestAuthenticateVia(t *testing.T) {
	a, _ := newAuth(t)
	_, tok, _ := a.CreateToken(context.Background(), "ci")

	r := req("")
	r.SetBasicAuth("ci", "cipw")
	if p, err := a.Authenticate(r); err != nil || p.Via != ViaPassword {
		t.Fatalf("basic: p=%+v err=%v", p, err)
	}
	if p, err := a.Authenticate(r); err != nil || p.Via != ViaPassword { // cached path
		t.Fatalf("cached basic: p=%+v err=%v", p, err)
	}
	for _, h := range []string{"Bearer " + tok, tok} {
		if p, err := a.Authenticate(req(h)); err != nil || p.Via != ViaToken {
			t.Fatalf("%q: p=%+v err=%v", h, p, err)
		}
	}
	if p, _ := a.Authenticate(req("")); p.Via != ViaNone {
		t.Fatalf("anonymous Via = %q", p.Via)
	}
	if p, ok := a.Lookup("ci"); !ok || p.Via != ViaNone {
		t.Fatalf("Lookup Via = %q", p.Via)
	}
}

func TestBasicAcceptsOwnTokenAsPassword(t *testing.T) {
	a, _ := newAuth(t)
	_, tok, _ := a.CreateToken(context.Background(), "ci")
	r := req("")
	r.SetBasicAuth("ci", tok)
	p, err := a.Authenticate(r)
	if err != nil || p.User != "ci" || p.Via != ViaToken {
		t.Fatalf("p=%+v err=%v", p, err)
	}
	r.SetBasicAuth("admin", tok) // another user's name
	if _, err := a.Authenticate(r); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("other user: err = %v", err)
	}
	r.SetBasicAuth("ci", "cc_nope")
	if _, err := a.Authenticate(r); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("unknown token: err = %v", err)
	}
	// CheckPassword itself never accepts a token.
	if _, err := a.CheckPassword(context.Background(), "ci", tok); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("CheckPassword accepted a token: %v", err)
	}
}

func TestAnonymousReadFollowsLookup(t *testing.T) {
	yes, no := true, false
	var mu sync.Mutex
	live := map[string]*bool{"r": &yes}
	a := New(&config.Config{AnonymousRead: false}, memTokens{})
	a.AnonymousRead = func(name string) *bool {
		mu.Lock()
		defer mu.Unlock()
		return live[name]
	}
	anon := Principal{}
	if !a.Can(anon, "r", Read) || a.Can(anon, "other", Read) {
		t.Fatal("initial lookup not honoured")
	}
	mu.Lock()
	live["r"], live["other"] = &no, &yes
	mu.Unlock()
	if a.Can(anon, "r", Read) || !a.Can(anon, "other", Read) {
		t.Fatal("a changed lookup was not followed without rebuilding the Authenticator")
	}
}

func TestNewUsesConfigRepositoriesByDefault(t *testing.T) {
	no := false
	a := New(&config.Config{AnonymousRead: true, Repositories: []config.Repository{{Name: "p", AnonymousRead: &no}}}, memTokens{})
	if a.Can(Principal{}, "p", Read) || !a.Can(Principal{}, "q", Read) {
		t.Fatal("default lookup from cfg.Repositories")
	}
}

func (m memTokens) TouchToken(_ context.Context, hash string) {
	m["\x00touched:"+hash] = "1"
	n, _ := strconv.Atoi(m["\x00count:"+hash])
	m["\x00count:"+hash] = strconv.Itoa(n + 1)
	m["\x00last:"+hash] = strconv.FormatInt(time.Now().UnixMilli(), 10)
}

func (m memTokens) touchCount(tok string) int {
	n, _ := strconv.Atoi(m["\x00count:"+hashToken(tok)])
	return n
}

func (m memTokens) touched(tok string) bool { return m["\x00touched:"+hashToken(tok)] != "" }

func TestBasicTokenWrongUserDoesNotTouch(t *testing.T) {
	a, store := newAuth(t)
	_, tok, err := a.CreateToken(context.Background(), "ci")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth("admin", tok) // admin guesses ci's token
	if _, err := a.Authenticate(r); err == nil {
		t.Fatal("a token must not authenticate another user")
	}
	if store.touched(tok) {
		t.Fatal("a wrong-user attempt must not record a use")
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth("ci", tok)
	if p, err := a.Authenticate(r); err != nil || p.User != "ci" {
		t.Fatalf("owner: %v %v", p, err)
	}
	if !store.touched(tok) {
		t.Fatal("the owner's use must be recorded")
	}
}

func TestTokenUseIsTouchedAtMostOncePerInterval(t *testing.T) {
	a, store := newAuth(t)
	_, tok, err := a.CreateToken(context.Background(), "ci")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest("GET", "/", nil)
		r.SetBasicAuth("ci", tok)
		if _, err := a.Authenticate(r); err != nil {
			t.Fatal(err)
		}
	}
	if n := store.touchCount(tok); n != 1 {
		t.Fatalf("10 authentications within the interval touched %d times, want 1", n)
	}
}

// TestLegacyTokens: tokens issued before the rename start with rl_ and must
// keep working through every path a cc_ token does.
func TestLegacyTokens(t *testing.T) {
	a, store := newAuth(t)
	const legacy = "rl_0123456789abcdef"
	store[hashToken(legacy)] = "ci"
	for _, h := range []string{"Bearer " + legacy, legacy} {
		p, err := a.Authenticate(req(h))
		if err != nil || p.User != "ci" || p.Via != ViaToken {
			t.Fatalf("header %q: p=%+v err=%v", h, p, err)
		}
	}
	r := req("")
	r.SetBasicAuth("ci", legacy)
	p, err := a.Authenticate(r)
	if err != nil || p.User != "ci" || p.Via != ViaToken {
		t.Fatalf("basic: p=%+v err=%v", p, err)
	}
	r.SetBasicAuth("admin", legacy) // not the owner
	if _, err := a.Authenticate(r); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("other user: err = %v", err)
	}
	r.SetBasicAuth("ci", "rl_nope")
	if _, err := a.Authenticate(r); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("unknown legacy token: err = %v", err)
	}
	_, tok, _ := a.CreateToken(context.Background(), "ci")
	if !strings.HasPrefix(tok, "cc_") {
		t.Fatalf("new token %q must start with cc_", tok)
	}
}

// QA L-4: the username __token__ stands for "whoever owns this token".
func TestBasicTokenUserPlaceholder(t *testing.T) {
	a, _ := newAuth(t)
	_, tok, _ := a.CreateToken(context.Background(), "ci")
	r := req("")
	r.SetBasicAuth("__token__", tok)
	p, err := a.Authenticate(r)
	if err != nil || p.User != "ci" || p.Via != ViaToken {
		t.Fatalf("p=%+v err=%v", p, err)
	}
	r.SetBasicAuth("__token__", "cc_nope")
	if _, err := a.Authenticate(r); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("unknown token: err = %v", err)
	}
}
