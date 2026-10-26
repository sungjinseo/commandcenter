package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"commandcenter/internal/config"
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
