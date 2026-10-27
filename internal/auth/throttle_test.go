package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/ldap"
	"commandcenter/internal/meta"
)

// throttleAuth has users u0..u4 (password "pw") and a fake clock.
func throttleAuth(t *testing.T) (*Authenticator, *time.Time) {
	t.Helper()
	cfg := &config.Config{}
	for i := 0; i < 5; i++ {
		cfg.Users = append(cfg.Users, config.User{Name: fmt.Sprintf("u%d", i), PasswordHash: hash(t, "pw")})
	}
	a := New(cfg, memTokens{})
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	return a, &now
}

func refusedFor(err error) time.Duration {
	var te *ThrottledError
	if errors.As(err, &te) {
		return te.Wait
	}
	return 0
}

// QA L-1: after accountLimit failures a name is refused without a check,
// for refuseFirst after its latest failure and then twice as long per
// further failure; a success forgets the failures, and so does failForget.
func TestThrottleRefusesAndBacksOff(t *testing.T) {
	a, now := throttleAuth(t)
	ctx := context.Background()
	fail := func() {
		t.Helper()
		if _, err := a.CheckPassword(ctx, "u0", "wrong"); !errors.Is(err, ErrBadCredentials) || refusedFor(err) != 0 {
			t.Fatalf("failed attempt: %v", err)
		}
	}
	for i := 0; i < accountLimit; i++ {
		fail()
		*now = now.Add(time.Second)
	}
	_, err := a.CheckPassword(ctx, "u0", "pw")
	if d := refusedFor(err); d <= refuseFirst-5*time.Second || d > refuseFirst || !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("right password while refused: wait %v, err %v", d, err)
	}
	// Any spelling of the name is the same name.
	if _, err := a.CheckPassword(ctx, "U0", "pw"); refusedFor(err) == 0 {
		t.Fatalf("upper-case name not refused: %v", err)
	}
	*now = now.Add(refuseFirst)
	fail() // the refusal is over; the next failure doubles it
	if _, err := a.CheckPassword(ctx, "u0", "pw"); refusedFor(err) != 2*refuseFirst {
		t.Fatalf("second refusal: %v", err)
	}
	*now = now.Add(2 * refuseFirst)
	if p, err := a.CheckPassword(ctx, "u0", "pw"); err != nil || p.User != "u0" {
		t.Fatalf("after the refusal: %+v %v", p, err)
	}
	// The success forgot the failures: accountLimit more are allowed.
	for i := 0; i < accountLimit; i++ {
		fail()
	}
	if _, err := a.CheckPassword(ctx, "u0", "pw"); refusedFor(err) == 0 {
		t.Fatalf("not refused again: %v", err)
	}
	// Half an hour later the count still stands: the next failure doubles.
	*now = now.Add(30 * time.Minute)
	fail()
	if _, err := a.CheckPassword(ctx, "u0", "pw"); refusedFor(err) != 2*refuseFirst {
		t.Fatalf("after half an hour: %v", err)
	}
	// Failures more than failForget apart start over, and refusals are capped.
	*now = now.Add(failForget + time.Second)
	if p, err := a.CheckPassword(ctx, "u0", "pw"); err != nil || p.User != "u0" {
		t.Fatalf("after failWindow: %v", err)
	}
	for i := 0; i < accountLimit+10; i++ {
		a.CheckPassword(ctx, "u0", "wrong")
		*now = now.Add(refuseMax)
	}
	*now = now.Add(-refuseMax)
	if d := refusedFor(func() error { _, err := a.CheckPassword(ctx, "u0", "pw"); return err }()); d != refuseMax {
		t.Fatalf("capped refusal = %v, want %v", d, refuseMax)
	}
	// Other names are not affected.
	if _, err := a.CheckPassword(ctx, "u1", "pw"); err != nil {
		t.Fatal(err)
	}
}

// Unknown names are refused like real ones, so a refusal reveals nothing.
func TestThrottleUnknownName(t *testing.T) {
	a, _ := throttleAuth(t)
	for i := 0; i < accountLimit; i++ {
		if _, err := a.CheckPassword(context.Background(), "ghost", "x"); refusedFor(err) != 0 {
			t.Fatalf("attempt %d refused early", i+1)
		}
	}
	if _, err := a.CheckPassword(context.Background(), "ghost", "x"); refusedFor(err) == 0 {
		t.Fatalf("unknown name not refused: %v", err)
	}
}

// Checks of one name run one at a time: a burst of parallel guesses gets
// accountLimit checks, not one per request ...
func TestThrottleConcurrentBurst(t *testing.T) {
	a, _ := throttleAuth(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	checked := 0
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.CheckPassword(context.Background(), "u0", "wrong"); refusedFor(err) == 0 {
				mu.Lock()
				checked++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if checked != accountLimit {
		t.Fatalf("%d of 30 parallel attempts were checked, want %d", checked, accountLimit)
	}
}

// ... while parallel requests with the right password all succeed, even
// after earlier failures (a build whose cached login has just expired).
func TestThrottleParallelRightPassword(t *testing.T) {
	a, _ := throttleAuth(t)
	for i := 0; i < accountLimit-1; i++ {
		a.CheckPassword(context.Background(), "u0", "wrong")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.CheckPassword(context.Background(), "u0", "pw")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a parallel login with the right password failed: %v", err)
		}
	}
}

// An attempt whose caller went away is not counted.
func TestThrottleCancelledAttempts(t *testing.T) {
	a, _ := throttleAuth(t)
	for i := 0; i < cap(bcryptSlots); i++ {
		bcryptSlots <- struct{}{} // every compare waits until its caller leaves
	}
	held := true
	release := func() {
		for i := 0; held && i < cap(bcryptSlots); i++ {
			<-bcryptSlots
		}
		held = false
	}
	defer release() // even when the test fails: other tests need the slots
	for i := 0; i < 2*accountLimit; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := a.CheckPassword(ctx, "u0", "wrong")
		cancel()
		if !errors.Is(err, ErrBadCredentials) || refusedFor(err) != 0 {
			t.Fatalf("cancelled attempt %d: %v", i+1, err)
		}
	}
	release()
	if _, err := a.CheckPassword(context.Background(), "u0", "pw"); err != nil {
		t.Fatalf("cancelled attempts were counted: %v", err)
	}
	if len(a.gates) != 0 {
		t.Fatalf("%d gates left behind", len(a.gates))
	}
}

// An address with addrLimit failures gets one failed attempt per name, but
// a name without failures still signs in from it, and other addresses keep
// the normal limit.
func TestThrottleBusyAddress(t *testing.T) {
	a, _ := throttleAuth(t)
	from := func(addr string) context.Context {
		return WithClientAddr(context.Background(), addr+":40000")
	}
	for i := 0; i < addrLimit; i++ {
		a.CheckPassword(from("192.0.2.1"), fmt.Sprintf("u%d", i/accountLimit), "wrong")
	}
	if _, err := a.CheckPassword(from("192.0.2.1"), "u4", "pw"); err != nil {
		t.Fatalf("a name without failures was refused: %v", err)
	}
	a.CheckPassword(from("192.0.2.1"), "u4", "wrong")
	if _, err := a.CheckPassword(from("192.0.2.1"), "u4", "pw"); refusedFor(err) == 0 {
		t.Fatalf("second attempt from the busy address: %v", err)
	}
	if _, err := a.CheckPassword(from("198.51.100.7"), "u4", "pw"); err != nil {
		t.Fatalf("another address: %v", err)
	}
}

// A password reset (OnUserChanged) ends the refusal.
func TestThrottleEndsOnUserChanged(t *testing.T) {
	a, _ := throttleAuth(t)
	for i := 0; i < accountLimit; i++ {
		a.CheckPassword(context.Background(), "u0", "wrong")
	}
	a.OnUserChanged("u0")
	if _, err := a.CheckPassword(context.Background(), "u0", "pw"); err != nil {
		t.Fatalf("still refused after OnUserChanged: %v", err)
	}
}

// Every password check is throttled: Basic auth, LDAP and the current
// password of a self-service change share the name's count; a refused LDAP
// attempt never reaches the directory.
func TestThrottleAllPasswordPaths(t *testing.T) {
	a, _ := throttleAuth(t)
	r := req("")
	r.SetBasicAuth("u0", "wrong")
	for i := 0; i < accountLimit; i++ {
		if _, err := a.Authenticate(r); refusedFor(err) != 0 {
			t.Fatalf("basic attempt %d refused early", i+1)
		}
	}
	r.SetBasicAuth("u0", "pw")
	if _, err := a.Authenticate(r); refusedFor(err) == 0 {
		t.Fatalf("basic auth not refused: %v", err)
	}
	if _, err := a.CheckLocalPassword(context.Background(), "u0", "pw"); refusedFor(err) == 0 {
		t.Fatalf("current-password check not refused: %v", err)
	}

	f := newLDAPFixture(t)
	for i := 0; i < accountLimit; i++ {
		f.a.CheckPassword(context.Background(), "alice", "wrong")
	}
	binds := f.srv.Binds()
	if _, err := f.a.CheckPassword(context.Background(), "alice", "alice-pw"); refusedFor(err) == 0 {
		t.Fatalf("ldap login not refused: %v", err)
	}
	if f.srv.Binds() != binds {
		t.Fatal("a refused attempt reached the directory")
	}
	f.advance(refuseFirst)
	if p, err := f.a.CheckPassword(context.Background(), "alice", "alice-pw"); err != nil || !p.IsLDAP() {
		t.Fatalf("ldap login after the refusal: %+v %v", p, err)
	}
}

// spaceFoldDir models how a real directory matches the uid in the login
// filter: caseIgnoreMatch with RFC 4518 insignificant-space handling
// (OpenLDAP, 389-ds), so " alice", "alice " and "ALICE" all find
// uid=alice. It counts the logins it is asked for and the password binds
// made against each entry.
type spaceFoldDir struct {
	mu     sync.Mutex
	logins int
	binds  map[string]int
}

func (d *spaceFoldDir) entry(name string) (ldap.User, bool) {
	id := strings.ToLower(strings.Join(strings.Fields(name), " "))
	if id != "alice" {
		return ldap.User{}, false
	}
	return ldap.User{DN: aliceDN, ID: "alice"}, true
}

func (d *spaceFoldDir) Authenticate(_ context.Context, _ meta.LDAP, name, pw string) (ldap.User, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logins++
	u, ok := d.entry(name)
	if !ok {
		return ldap.User{}, ldap.ErrNoSuchUser
	}
	d.binds[u.ID]++
	if pw != "alice-pw" {
		return ldap.User{}, ldap.ErrInvalidCredentials
	}
	return u, nil
}

func (d *spaceFoldDir) Find(_ context.Context, _ meta.LDAP, id string) (ldap.User, error) {
	if u, ok := d.entry(id); ok && u.ID == id {
		return u, nil
	}
	return ldap.User{}, ldap.ErrNoSuchUser
}

// A directory finds one entry under many spellings of its name, and each
// spelling would be a fresh count of accountLimit guesses. So only a
// printable ASCII name without leading, trailing or repeated spaces
// reaches a realm; any other fails like a wrong password, unchecked and
// uncounted, and logged once at Debug level (never with the password).
func TestThrottleLDAPNameSpellings(t *testing.T) {
	f := newLDAPFixture(t)
	d := &spaceFoldDir{binds: map[string]int{}}
	f.a.LDAP = d
	var logs strings.Builder
	f.a.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		name := strings.Repeat(" ", i) + "alice" // the same directory entry every time
		if _, err := f.a.CheckPassword(ctx, name, fmt.Sprintf("wrong-%d", i)); !errors.Is(err, ErrBadCredentials) || refusedFor(err) != 0 {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	p, err := f.a.CheckPassword(ctx, strings.Repeat(" ", 31)+"alice", "alice-pw")
	if n := d.binds["alice"]; n != 1 || !errors.Is(err, ErrBadCredentials) || refusedFor(err) != 0 {
		t.Fatalf("the directory checked %d password guesses against uid=alice, and a spelled-out variant got %+v %v", n, p, err)
	}
	logins := d.logins
	for _, name := range []string{"alice ", "\talice", "a  lice", "ali\u00a0ce", "ａｌｉｃｅ", "김철수", "alice\x00", "alice\x7f"} {
		if _, err := f.a.CheckPassword(ctx, name, "alice-pw"); !errors.Is(err, ErrBadCredentials) || refusedFor(err) != 0 {
			t.Errorf("%q: %v", name, err)
		}
	}
	if d.logins != logins {
		t.Fatalf("%d refused spellings reached the directory", d.logins-logins)
	}
	if n := strings.Count(logs.String(), `level=DEBUG msg="sign-in refused`); n != 29+1+8 || strings.Contains(logs.String(), "-pw") || strings.Contains(logs.String(), "wrong") {
		t.Fatalf("%d of 38 refusals logged at Debug level, or a password logged:\n%s", n, logs.String())
	}
	for _, name := range []string{"Alice Smith", "alice@example.org", "a.b_c-d~"} {
		if !PlainLoginName(name) {
			t.Errorf("%q refused", name)
		}
	}
	if p, err := f.a.CheckPassword(ctx, "ALICE", "alice-pw"); err != nil || p.User != "alice" {
		t.Fatalf("upper-case login: %+v %v", p, err)
	}
}

// anyDir accepts the names l0..l7, each with the password "pw".
type anyDir struct{}

func (anyDir) Authenticate(_ context.Context, _ meta.LDAP, name, pw string) (ldap.User, error) {
	if len(name) != 2 || name[0] != 'l' || name[1] < '0' || name[1] > '7' {
		return ldap.User{}, ldap.ErrNoSuchUser
	}
	if pw != "pw" {
		return ldap.User{}, ldap.ErrInvalidCredentials
	}
	return ldap.User{DN: "uid=" + name + ",ou=people,dc=example,dc=org", ID: name}, nil
}

func (anyDir) Find(_ context.Context, _ meta.LDAP, id string) (ldap.User, error) {
	return ldap.User{DN: "uid=" + id + ",ou=people,dc=example,dc=org", ID: id}, nil
}

// With the default realm order (local before ldap) every request of an
// LDAP user passes the local realm, whose compare for a name it does not
// know no cache covers; behind the per-name gate one user's parallel
// requests paid those compares one at a time. A password the ldap realm
// has cached skips it: with every bcrypt slot taken, the parallel requests
// still pass at once, while a wrong password still waits for its compare.
func TestThrottleLDAPParallelRightPassword(t *testing.T) {
	f := newLDAPFixture(t) // realms: local, token, ldap
	f.a.LDAP = anyDir{}
	if _, err := f.a.CheckPassword(context.Background(), "l0", "pw"); err != nil { // fills the LDAP caches
		t.Fatal(err)
	}
	for i := 0; i < cap(bcryptSlots); i++ {
		bcryptSlots <- struct{}{} // every compare now waits until its caller leaves
	}
	defer func() {
		for i := 0; i < cap(bcryptSlots); i++ {
			<-bcryptSlots
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, err := f.a.CheckPassword(ctx, "l0", "pw"); err != nil || p.User != "l0" || ctx.Err() != nil {
				t.Errorf("a parallel request with the right password waited for a compare: %+v %v (%v)", p, err, ctx.Err())
			}
		}()
	}
	wg.Wait()
	short, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	f.a.CheckPassword(short, "l0", "wrong")
	if short.Err() == nil {
		t.Fatal("a wrong password skipped the local realm's compare")
	}
}

// slowNoUserDir is a directory that knows none of the local users and
// takes slowNoUserDelay to say so (service bind plus search, as over
// LDAPS). It counts the logins it is asked for.
type slowNoUserDir struct{ logins atomic.Int64 }

const slowNoUserDelay = 30 * time.Millisecond

func (d *slowNoUserDir) Authenticate(ctx context.Context, _ meta.LDAP, _, _ string) (ldap.User, error) {
	d.logins.Add(1)
	select {
	case <-time.After(slowNoUserDelay):
	case <-ctx.Done():
	}
	return ldap.User{}, ldap.ErrNoSuchUser
}

func (d *slowNoUserDir) Find(context.Context, meta.LDAP, string) (ldap.User, error) {
	return ldap.User{}, ldap.ErrNoSuchUser
}

// Re-review rr-auth-2: with the ldap realm ordered before local (the
// Realms screen allows it, and the order is the priority), every check of
// a LOCAL user asks the directory first, which caches no "no such user".
// The per-name gate ran those directory calls one at a time, so one CI
// user's parallel requests (a Maven build) took N directory round trips
// where N different users take one.
func TestThrottleLDAPFirstParallelLocalUser(t *testing.T) {
	f := newLDAPFixture(t)
	f.a.LDAP = &slowNoUserDir{}
	f.settings.Realms = []meta.Realm{{ID: meta.RealmLDAP, Enabled: true}, {ID: meta.RealmLocal, Enabled: true}, {ID: meta.RealmToken, Enabled: true}}
	f.apply()
	ctx := context.Background()
	users := []string{"admin", "adder", "editor", "npmdel", "browser", "plain"}
	for _, u := range users { // warm the local success cache: later checks need no bcrypt
		if _, err := f.a.CheckPassword(ctx, u, "pw"); err != nil {
			t.Fatal(err)
		}
	}
	run := func(name func(i int) string) time.Duration {
		var wg sync.WaitGroup
		start := time.Now()
		for i := range users {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := f.a.CheckPassword(ctx, name(i), "pw"); err != nil {
					t.Errorf("%s: %v", name(i), err)
				}
			}()
		}
		wg.Wait()
		return time.Since(start)
	}
	distinct := run(func(i int) string { return users[i] })
	one := run(func(int) string { return "plain" })
	// Both runs now take well under one directory round trip; the floor
	// keeps scheduler noise in two such short runs from failing the ratio.
	if one > 3*distinct && one > 3*slowNoUserDelay {
		t.Fatalf("%d parallel right-password requests: %d local users %v, one local user %v (ldap realm first)",
			len(users), len(users), distinct.Round(time.Millisecond), one.Round(time.Millisecond))
	}
}

// With ldap ordered before local, parallel right-password requests of a
// local user whose cached login has expired make one directory call, not
// one each in turn: the requests that waited at the name's gate take the
// first one's cached success. A cached success does not wait for the gate
// at all, and it forgets the name's failures as a checked one does, while
// a wrong password still waits for its turn.
func TestThrottleLDAPFirstOneDirectoryCall(t *testing.T) {
	f := ldapFirst(t)
	d := &slowNoUserDir{}
	f.a.LDAP = d
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, err := f.a.CheckPassword(ctx, "plain", "pw"); err != nil || p.User != "plain" {
				t.Errorf("a parallel right password: %+v %v", p, err)
			}
		}()
	}
	wg.Wait()
	if n := d.logins.Load(); n > 1 {
		t.Fatalf("8 parallel right-password requests of a local user made %d directory calls, want at most 1", n)
	}
	unlock, err := f.a.lockName(ctx, nameKey("plain")) // a slow check of the name in progress
	if err != nil {
		t.Fatal(err)
	}
	waiting, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if p, err := f.a.CheckPassword(waiting, "plain", "pw"); err != nil || p.User != "plain" {
		t.Fatalf("a cached right password waited for the name's gate: %+v %v", p, err)
	}
	short, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	if _, err := f.a.CheckPassword(short, "plain", "wrong"); !errors.Is(err, ErrBadCredentials) || short.Err() == nil {
		t.Fatalf("a wrong password did not wait for the gate: %v", err)
	}
	unlock()
	for round := 1; round <= 2; round++ {
		for i := 0; i < accountLimit-1; i++ {
			f.a.CheckPassword(ctx, "plain", "wrong")
		}
		if _, err := f.a.CheckPassword(ctx, "plain", "pw"); err != nil {
			t.Fatalf("round %d: the cached success did not forget the failures: %v", round, err)
		}
	}
}

// A revoked or mistyped token in the Basic password is not a password
// guess: CI retrying it must not get its owner throttled.
func TestThrottleIgnoresStaleTokens(t *testing.T) {
	a, _ := throttleAuth(t)
	r := req("")
	r.SetBasicAuth("u0", tokenPrefix+fmt.Sprintf("%064x", 42))
	for i := 0; i < 2*accountLimit; i++ {
		if _, err := a.Authenticate(r); !errors.Is(err, ErrBadCredentials) || refusedFor(err) != 0 {
			t.Fatalf("stale token: %v", err)
		}
	}
	if _, err := a.CheckPassword(context.Background(), "u0", "pw"); err != nil {
		t.Fatalf("owner throttled by a stale token: %v", err)
	}
	if !tokenShaped(legacyTokenPrefix+fmt.Sprintf("%064x", 1)) || tokenShaped("cc_short") || tokenShaped("cc_"+fmt.Sprintf("%063x", 1)+"g") {
		t.Fatal("tokenShaped")
	}
}

// The tracked records stay bounded. Lapsed ones go first, then those with
// the fewest failures, so a flood of fresh names does not end a refusal.
func TestThrottleBounded(t *testing.T) {
	a, now := throttleAuth(t)
	fail := func(name string) { a.noteFailure(name, nameKey(name), "") }
	for i := 0; i < failKeysMax-1; i++ {
		fail(fmt.Sprintf("old%d", i))
	}
	*now = now.Add(failForget + time.Second)
	for i := 0; i < accountLimit; i++ {
		fail("victim")
	}
	fail("new")
	if n := len(a.fails); n != 2 {
		t.Fatalf("%d records, want the 2 live ones", n)
	}
	*now = now.Add(time.Second) // the refused record is now the oldest
	for i := 0; i < failKeysMax+5; i++ {
		fail(fmt.Sprintf("live%d", i))
	}
	if n := len(a.fails); n > failKeysMax {
		t.Fatalf("%d records, cap %d", n, failKeysMax)
	}
	if a.refusal(nameKey("victim"), "") <= 0 {
		t.Fatal("a flood of fresh names ended the refusal")
	}
}

func TestThrottleAddrAndRetryAfter(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.1:1234":          "192.0.2.1",
		"[2001:db8:1:2:3::4]:80":  "2001:db8:1:2::/64",
		"[::ffff:192.0.2.9]:5555": "192.0.2.9",
		"@":                       "@",
	} {
		if got := throttleAddr(in); got != want {
			t.Errorf("throttleAddr(%q) = %q, want %q", in, got, want)
		}
	}
	h := http.Header{}
	if s := RetryAfter(h, &ThrottledError{Wait: 1500 * time.Millisecond}); s != 2 || h.Get("Retry-After") != "2" {
		t.Fatalf("RetryAfter = %d, header %q", s, h.Get("Retry-After"))
	}
	if s := RetryAfter(http.Header{}, ErrBadCredentials); s != 0 {
		t.Fatalf("RetryAfter of a plain failure = %d", s)
	}
}
