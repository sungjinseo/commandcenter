package auth

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Password-guessing throttle (QA L-1). Every password check (Basic auth, UI
// login, npm login, the current password of a self-service change; local
// and LDAP realms alike) goes through throttle. After accountLimit failures
// of a login name with no success in between, the name is refused (429,
// Retry-After) for refuseFirst after its latest failure, twice as long after
// each further failure, up to refuseMax; a refused attempt checks nothing.
// A success forgets the name's failures, and so does an hour without one.
// Names are compared case-insensitively (directories match uid that way)
// and unknown names count like real ones, so a refusal does not reveal
// which exist. A name that is not PlainLoginName fails like a wrong
// password before any of this, unchecked and uncounted (logged at Debug
// level): a directory also ignores width, Unicode form and extra spaces
// when it matches a name (RFC 4518), so each such spelling of one entry
// would get a count of its own. Checks of one name run one at a time, so a
// burst of parallel guesses cannot all start before the first failures are
// counted. A password that a realm has cached as a recent success (a wrong
// one never is) needs no check: it does not wait for that turn, and the
// requests that waited behind the check that cached it (a build whose
// cached login expired) take it from the cache, in either realm order.
//
// A client address with addrLimit failures within addrWindow of its first
// one may make only one failed attempt per name: a password spray from one
// place is slowed, but an address is never refused as a whole. Behind a
// reverse proxy every client has the proxy's address, and a whole-address
// refusal would let one client lock everyone out. Refusals end on their own
// (nothing stays locked until an administrator acts), and tokens and
// existing sessions are never affected.
const (
	accountLimit = 5
	refuseFirst  = time.Minute
	refuseMax    = 15 * time.Minute
	failForget   = time.Hour // a name's failures are forgotten after this long without another
	addrLimit    = 20
	addrWindow   = 15 * time.Minute
	failKeysMax  = 10000 // tracked names and addresses together
)

// ThrottledError refuses a password check without making it. It wraps
// ErrBadCredentials, so a caller that does not know it fails closed.
type ThrottledError struct{ Wait time.Duration }

func (e *ThrottledError) Error() string { return "too many failed sign-ins" }
func (e *ThrottledError) Unwrap() error { return ErrBadCredentials }

// RetryAfter returns the whole seconds a throttled sign-in must wait and
// sets h's Retry-After to them; it returns 0 if err is not a ThrottledError.
func RetryAfter(h http.Header, err error) int {
	var te *ThrottledError
	if !errors.As(err, &te) {
		return 0
	}
	s := max(1, int((te.Wait+time.Second-1)/time.Second))
	h.Set("Retry-After", strconv.Itoa(s))
	return s
}

type clientAddrKey struct{}

// WithClientAddr records the client's address (http.Request.RemoteAddr)
// for the throttle. The server sets it before authenticating.
func WithClientAddr(ctx context.Context, remoteAddr string) context.Context {
	return context.WithValue(ctx, clientAddrKey{}, remoteAddr)
}

// throttleAddr is the IP of remoteAddr without its port; an IPv6 address
// counts as its /64, which one host usually holds whole.
func throttleAddr(remoteAddr string) string {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return remoteAddr // "" when the server set none
	}
	ip := ap.Addr().Unmap()
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}

type failRec struct {
	n  int
	at time.Time // a name's latest failure; an address's first failure in its window
}

// failGate serializes the checks of one name; refs counts its holder and
// waiters, and the gate is dropped when they are gone.
type failGate struct {
	ch   chan struct{}
	refs int
}

func nameKey(name string) string { return "u\x00" + strings.ToLower(name) }
func addrKey(addr string) string { return "a\x00" + addr }

// PlainLoginName reports whether sign-in accepts name: printable ASCII
// without leading, trailing or repeated spaces. For such a name a
// directory's case-insensitive match is plain ASCII case folding, so
// nameKey gives all the spellings it matches one count. The LDAP
// connection test applies it too.
func PlainLoginName(name string) bool {
	for i := 0; i < len(name); i++ {
		if name[i] < ' ' || name[i] > '~' {
			return false
		}
	}
	return strings.TrimSpace(name) == name && !strings.Contains(name, "  ")
}

// lapsed reports whether record r under key k no longer counts at now.
func lapsed(k string, r failRec, now time.Time) bool {
	if strings.HasPrefix(k, "a\x00") {
		return now.Sub(r.at) > addrWindow
	}
	return now.Sub(r.at) > failForget
}

// throttle checks name's password with check unless the name is being
// refused, and records the outcome. check(true) only answers a live cached
// success, which no wrong password has: it is asked before the name's gate,
// so a cached login never queues behind the checks of its name, and again
// at the gate, where the check ahead may just have cached it. Only then
// does check(false) check the password. An attempt whose caller went away
// is not counted.
func (a *Authenticator) throttle(ctx context.Context, name string, check func(cachedOnly bool) (Principal, bool)) (Principal, error) {
	remote, _ := ctx.Value(clientAddrKey{}).(string)
	addr, k := throttleAddr(remote), nameKey(name)
	if !PlainLoginName(name) {
		a.logger().Debug("sign-in refused: the name is not printable ASCII without leading, trailing or double spaces",
			"user", strconv.Quote(name), "remote", addr)
		return Principal{}, ErrBadCredentials
	}
	if wait := a.refusal(k, addr); wait > 0 {
		return Principal{}, &ThrottledError{Wait: wait}
	}
	p, ok := check(true)
	if !ok {
		unlock, err := a.lockName(ctx, k)
		if err != nil {
			return Principal{}, ErrBadCredentials
		}
		defer unlock()
		if wait := a.refusal(k, addr); wait > 0 {
			return Principal{}, &ThrottledError{Wait: wait}
		}
		if p, ok = check(true); !ok {
			p, ok = check(false)
		}
	}
	if !ok {
		if ctx.Err() == nil {
			a.noteFailure(name, k, addr)
		}
		return Principal{}, ErrBadCredentials
	}
	a.mu.Lock()
	delete(a.fails, k)
	a.mu.Unlock()
	return p, nil
}

// lockName waits, until ctx ends, for the checks of name key k to be ours.
func (a *Authenticator) lockName(ctx context.Context, k string) (unlock func(), err error) {
	a.mu.Lock()
	if a.gates == nil {
		a.gates = map[string]*failGate{}
	}
	g := a.gates[k]
	if g == nil {
		g = &failGate{ch: make(chan struct{}, 1)}
		a.gates[k] = g
	}
	g.refs++
	a.mu.Unlock()
	leave := func() {
		a.mu.Lock()
		if g.refs--; g.refs == 0 {
			delete(a.gates, k)
		}
		a.mu.Unlock()
	}
	select {
	case g.ch <- struct{}{}:
		return func() { <-g.ch; leave() }, nil
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
}

// refusal is how long attempts for name key k from addr are still refused.
func (a *Authenticator) refusal(k, addr string) time.Duration {
	now := a.clock()
	a.mu.Lock()
	defer a.mu.Unlock()
	limit := accountLimit
	if r, ok := a.fails[addrKey(addr)]; ok && addr != "" && !lapsed(addrKey(addr), r, now) && r.n >= addrLimit {
		limit = 1
	}
	r, ok := a.fails[k]
	if !ok || lapsed(k, r, now) || r.n < limit {
		return 0
	}
	return r.at.Add(min(refuseFirst<<min(r.n-limit, 10), refuseMax)).Sub(now)
}

// noteFailure counts a failed attempt of name (key k) from addr and logs
// when the name or the address reaches its limit.
func (a *Authenticator) noteFailure(name, k, addr string) {
	now := a.clock()
	a.mu.Lock()
	r := a.fails[k]
	if lapsed(k, r, now) {
		r.n = 0
	}
	r.n++
	r.at = now
	a.putFail(k, r, now)
	ar := a.fails[addrKey(addr)]
	if addr != "" {
		if lapsed(addrKey(addr), ar, now) {
			ar = failRec{at: now}
		}
		ar.n++
		a.putFail(addrKey(addr), ar, now)
	}
	a.mu.Unlock()
	if r.n == accountLimit {
		a.logger().Warn("sign-in throttled: password attempts for this name are refused for a while", "user", name, "failures", r.n, "remote", addr)
	}
	if addr != "" && ar.n == addrLimit {
		a.logger().Warn("sign-in throttled: this address may make one failed attempt per name for a while", "remote", addr, "failures", ar.n)
	}
}

// putFail stores r under k, with a.mu held. When the map is full it drops
// the lapsed records, then the one with the fewest failures (the oldest of
// those), so a flood of fresh names does not push out a refused one.
// Records are only added after a completed check, so filling the map costs
// a password compare per record.
func (a *Authenticator) putFail(k string, r failRec, now time.Time) {
	if a.fails == nil {
		a.fails = map[string]failRec{}
	}
	if _, ok := a.fails[k]; !ok && len(a.fails) >= failKeysMax {
		victim := ""
		for key, v := range a.fails {
			if lapsed(key, v, now) {
				delete(a.fails, key)
			} else if w, ok := a.fails[victim]; !ok || v.n < w.n || v.n == w.n && v.at.Before(w.at) {
				victim = key
			}
		}
		if len(a.fails) >= failKeysMax {
			delete(a.fails, victim)
		}
	}
	a.fails[k] = r
}
