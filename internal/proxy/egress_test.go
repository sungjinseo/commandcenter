package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/repo"
)

func TestClassOf(t *testing.T) {
	for addr, want := range map[string]int{
		"8.8.8.8": classPublic, "203.0.113.7": classPublic, "2606:4700::1111": classPublic,
		"10.1.2.3": classPrivate, "172.16.0.1": classPrivate, "192.168.1.1": classPrivate,
		"100.100.100.200": classPrivate, "fd12::1": classPrivate,
		"127.0.0.1": classLoopback, "127.8.9.10": classLoopback, "::1": classLoopback, "::ffff:127.0.0.1": classLoopback,
		"169.254.169.254": classLinkLocal, "fe80::1%eth0": classLinkLocal,
		"0.0.0.0": classUnspecified, "0.1.2.3": classUnspecified, "::": classUnspecified,
		"224.0.0.1": classMulticast, "ff02::1": classMulticast,
		// NAT64 connects to the IPv4 address in the last 32 bits.
		"64:ff9b::a00:5": classPrivate, "64:ff9b::7f00:1": classLoopback, "64:ff9b::a9fe:a9fe": classLinkLocal,
		"64:ff9b::a00:5%eth0": classPrivate, "64:ff9b::808:808": classPublic,
		"64:ff9b:1::a00:5": classPrivate, "64:ff9b:1:2::c0a8:1": classPrivate, "64:ff9b:1::808:808": classPublic,
	} {
		if got := classOf(netip.MustParseAddr(addr)); got != want {
			t.Errorf("classOf(%s) = %b, want %b", addr, got, want)
		}
	}
}

// A public remote reaches public addresses only; an internal one also its
// private neighbours; loopback, link-local and the rest only when the remote
// itself is there.
func TestAllowedFor(t *testing.T) {
	for _, c := range []struct {
		remote      string
		ok, refused []string
	}{
		{"93.184.216.34", []string{"151.101.0.1"}, []string{"10.0.0.6", "fd00::6", "100.64.0.1", "127.0.0.1", "169.254.169.254", "0.0.0.0", "224.0.0.1"}},
		{"10.0.0.5", []string{"10.0.0.6", "192.168.0.9", "fd00::6", "151.101.0.1"}, []string{"127.0.0.1", "::1", "169.254.169.254", "::"}},
		{"127.0.0.1", []string{"127.0.0.2", "::1", "10.0.0.6", "151.101.0.1"}, []string{"169.254.169.254", "0.0.0.0", "ff02::1"}},
	} {
		allowed := allowedFor(classOf(netip.MustParseAddr(c.remote)))
		for _, a := range c.ok {
			if err := checkAddr(netip.MustParseAddr(a), allowed); err != nil {
				t.Errorf("remote %s: %s refused: %v", c.remote, a, err)
			}
		}
		for _, a := range c.refused {
			if err := checkAddr(netip.MustParseAddr(a), allowed); !errors.Is(err, errEgress) {
				t.Errorf("remote %s: %s allowed", c.remote, a)
			}
		}
	}
}

// resolvesTo is a lookup that finds every host name at addr; an address
// literal resolves to itself, as with the real resolver.
func resolvesTo(addr string) func(context.Context, string, string) ([]netip.Addr, error) {
	return func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if ip, err := netip.ParseAddr(host); err == nil {
			return []netip.Addr{ip}, nil
		}
		return []netip.Addr{netip.MustParseAddr(addr)}, nil
	}
}

// internalServer is a loopback service that a fetch reaches only when the
// policy allows; it counts its hits.
func internalServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := new(atomic.Int32)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "internal")
	}))
	t.Cleanup(ts.Close)
	return ts, hits
}

func proxyRepo(name, remote string) *repo.Repository {
	cfg := &config.Config{Repositories: []config.Repository{{Name: name, Format: "maven", Type: "proxy", Remote: remote,
		MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)}}}
	return repo.Build(cfg)[name]
}

// A public remote's index (a PyPI file URL, Cargo's dl template) cannot send
// the server to an internal address, and a remote name that resolved to a
// public address when classified but to loopback when dialed (DNS rebinding)
// is refused at connect time.
func TestEgressPublicRemoteCannotReachInternalAddresses(t *testing.T) {
	internal, hits := internalServer(t)
	f, _, _ := setup(t, &remote{})
	f.Client = newClient(nil, resolvesTo("203.0.113.10"))

	index := proxyRepo("py", "https://index.example/")
	if _, err := f.FetchTarget(ctx, index, Target{Key: "x.whl", URL: internal.URL + "/x.whl"}, Content); !errors.Is(err, ErrRemote) {
		t.Fatalf("index-supplied internal URL: err = %v, want ErrRemote", err)
	}
	_, port, _ := net.SplitHostPort(internal.Listener.Addr().String())
	rebound := proxyRepo("rebound", "http://localhost:"+port+"/")
	if _, err := f.Fetch(ctx, rebound, "x.jar", Content); !errors.Is(err, ErrRemote) {
		t.Fatalf("rebound remote: err = %v, want ErrRemote", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("internal server reached %d times", hits.Load())
	}
	if _, err := f.Store.Get(ctx, "py", "x.whl"); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("refused download was cached: %v", err)
	}

	// The same fetch for a remote that does resolve to loopback goes through.
	f.Client = newClient(nil, resolvesTo("127.0.0.1"))
	if _, err := f.Fetch(ctx, rebound, "x.jar", Content); err != nil || hits.Load() != 1 {
		t.Fatalf("loopback remote: err = %v, hits = %d", err, hits.Load())
	}
}

// A public remote's redirect to an internal host is refused when the target
// is dialed, here with the remote itself behind a forward proxy (HTTP_PROXY
// with NO_PROXY covering the internal host): the proxy's own loopback address
// is the admin's choice and is not refused. A connection to the internal host
// that an internal remote left in the pool is not reused for the redirect.
func TestEgressPublicRemoteRedirectToInternalRefused(t *testing.T) {
	internal, hits := internalServer(t)
	var proxied atomic.Int32
	fwd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		http.Redirect(w, r, internal.URL+"/secret", http.StatusFound)
	}))
	defer fwd.Close()
	viaProxy := func(r *http.Request) (*url.URL, error) {
		if r.URL.Hostname() == "repo.example" {
			return url.Parse(fwd.URL)
		}
		return nil, nil
	}
	f, _, _ := setup(t, &remote{})
	f.Client = newClient(viaProxy, resolvesTo("203.0.113.10"))
	if _, err := f.Fetch(ctx, proxyRepo("mirror", internal.URL), "warm", Content); err != nil {
		t.Fatalf("internal remote: %v", err)
	}

	if _, err := f.Fetch(ctx, proxyRepo("central", "http://repo.example/maven2"), "g/a.jar", Content); !errors.Is(err, ErrRemote) {
		t.Fatalf("err = %v, want ErrRemote", err)
	}
	if proxied.Load() != 1 || hits.Load() != 1 {
		t.Fatalf("proxy hits = %d (want 1), internal hits = %d (want 1: the internal remote's own fetch)", proxied.Load(), hits.Load())
	}
}

// A forward proxy connects for us, so only an address literal can be checked:
// a public remote's index cannot point the proxy at the metadata service,
// while names still go through the proxy.
func TestEgressProxiedRequestChecksLiteral(t *testing.T) {
	var proxied atomic.Int32
	fwd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		io.WriteString(w, "via proxy")
	}))
	defer fwd.Close()
	f, _, _ := setup(t, &remote{})
	f.Client = newClient(func(*http.Request) (*url.URL, error) { return url.Parse(fwd.URL) }, resolvesTo("203.0.113.10"))
	index := proxyRepo("py", "https://index.example/")

	if _, err := f.FetchTarget(ctx, index, Target{Key: "m", URL: "http://169.254.169.254/latest/meta-data/"}, Content); !errors.Is(err, ErrRemote) {
		t.Fatalf("err = %v, want ErrRemote", err)
	}
	if proxied.Load() != 0 {
		t.Fatal("the metadata address was sent to the proxy")
	}
	res, err := f.FetchTarget(ctx, index, Target{Key: "x.whl", URL: "http://files.example/x.whl"}, Content)
	if err != nil || read(t, f, res.Asset) != "via proxy" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// A host that netip does not parse but that ends in a number is an IPv4
// address to inet_aton, so to getaddrinfo and most forward proxies (10.1 and
// 167772161 are 10.0.0.1): it is refused on the proxied and the direct path
// alike, for an internal remote too, before a proxy or resolver reads it.
// Names that merely contain digits still go through.
func TestEgressRefusesNumericIPv4Spellings(t *testing.T) {
	var proxied atomic.Int32
	fwd := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxied.Add(1) }))
	defer fwd.Close()
	viaProxy := func(*http.Request) (*url.URL, error) { return url.Parse(fwd.URL) }
	get := func(c *http.Client, host string) error {
		req, err := http.NewRequestWithContext(withRemote(ctx, &url.URL{Scheme: "http", Host: "nexus.corp"}), http.MethodGet, "http://"+host+"/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	for _, proxy := range []func(*http.Request) (*url.URL, error){viaProxy, nil} {
		c := newClient(proxy, resolvesTo("10.0.0.5"))
		for _, host := range []string{"10.1", "167772161", "0x0a000001", "0X0A000001", "012.0.0.1", "127.1",
			"2130706433:8080", "0x7f.1", "127.0.0.1.", "0251.0376.0251.0376", "0x"} {
			if err := get(c, host); !errors.Is(err, errEgress) {
				t.Errorf("proxied %v, %s: err = %v, want the egress refusal", proxy != nil, host, err)
			}
		}
	}
	if proxied.Load() != 0 {
		t.Fatalf("%d numeric hosts reached the proxy", proxied.Load())
	}
	c := newClient(viaProxy, resolvesTo("203.0.113.10"))
	for _, host := range []string{"1e100.net", "123.example", "0x.example", "10.1.example", "0xsvc"} {
		if err := get(c, host); err != nil {
			t.Errorf("%s: %v", host, err)
		}
	}
}

// net/http maps a host that is not ASCII with UTS #46 for the dial and for a
// forward proxy's CONNECT target, so fullwidth digits and dots (U+FF10-FF19,
// U+FF0E) reached the proxy as "CONNECT 169.254.169.254:443" or
// "CONNECT 10.1:443", past the literal and numeric checks. Such a host, and a
// name not written in punycode, is refused on either path.
func TestEgressRefusesNonASCIIHosts(t *testing.T) {
	var mu sync.Mutex
	var connects []string
	fwd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connects = append(connects, r.Method+" "+r.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer fwd.Close()
	viaProxy := func(*http.Request) (*url.URL, error) { return url.Parse(fwd.URL) }
	for _, proxy := range []func(*http.Request) (*url.URL, error){viaProxy, nil} {
		c := newClient(proxy, resolvesTo("203.0.113.10"))
		for _, host := range []string{"１６９．２５４．１６９．２５４", "１０．１", "bücher.example"} {
			req, err := http.NewRequestWithContext(withRemote(ctx, &url.URL{Scheme: "https", Host: "index.example"}), http.MethodGet, "https://"+host+"/latest/meta-data/", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.Do(req)
			if err == nil {
				resp.Body.Close()
			}
			if !errors.Is(err, errEgress) {
				t.Errorf("proxied %v, %s: err = %v, want the egress refusal", proxy != nil, host, err)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(connects) > 0 {
		t.Fatalf("public remote: the proxy was asked for %q", connects)
	}
}

// net/http copies the first request's Authorization to a redirect whose host
// name is the same or a subdomain, whatever its scheme or port. Credentials
// stay with the remote's own origin: here https://127.0.0.1:A keeps them on a
// redirect to itself, then sends the fetch to http://127.0.0.1:B (a backend
// URL behind a TLS proxy), which must not receive them in clear text.
func TestEgressRedirectKeepsCredentialsAtHome(t *testing.T) {
	var mu sync.Mutex
	var moved, leaked string
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = r.Header.Get("Authorization")
		mu.Unlock()
		io.WriteString(w, "jar")
	}))
	defer plain.Close()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path, ok := strings.CutPrefix(r.URL.Path, "/moved"); ok {
			mu.Lock()
			moved = r.Header.Get("Authorization")
			mu.Unlock()
			http.Redirect(w, r, plain.URL+path, http.StatusFound)
			return
		}
		http.Redirect(w, r, "/moved"+r.URL.Path, http.StatusFound)
	}))
	defer upstream.Close()

	f, _, _ := setup(t, &remote{})
	c := newClient(nil, resolvesTo("127.0.0.1"))
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	c.Transport.(*egress).base.TLSClientConfig = &tls.Config{RootCAs: pool}
	f.Client = c
	cfg := &config.Config{Repositories: []config.Repository{{Name: "a", Type: "proxy", Remote: upstream.URL + "/",
		RemoteAuth: &config.RemoteAuth{Username: "u", Password: "p"}, ContentMaxAge: config.Never}}}
	rp := repo.Build(cfg)["a"]
	if _, err := f.Fetch(ctx, rp, "g/a.jar", Content); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if moved != "Basic dTpw" || leaked != "" {
		t.Fatalf("Authorization on the redirect to the remote itself = %q (want it kept), on the redirect to %s = %q (want none)", moved, plain.URL, leaked)
	}
}

// The origin compared is scheme, host name and port, an omitted port being
// the scheme's default, so a remote written as host:443 keeps its
// credentials on a redirect to host.
func TestEgressRedirectComparesEffectivePort(t *testing.T) {
	check := newClient(nil, resolvesTo("203.0.113.10")).CheckRedirect
	first := httptest.NewRequest(http.MethodGet, "https://repo.example:443/a", nil)
	for target, keep := range map[string]bool{
		"https://repo.example/b":      true,
		"https://repo.example:443/b":  true,
		"http://repo.example/b":       false,
		"http://repo.example:443/b":   false,
		"https://repo.example:8443/b": false,
		"https://cdn.repo.example/b":  false,
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Basic dTpw") // as net/http copied it
		if err := check(req, []*http.Request{first}); err != nil {
			t.Fatal(err)
		}
		if kept := req.Header.Get("Authorization") != ""; kept != keep {
			t.Errorf("%s: Authorization kept = %v, want %v", target, kept, keep)
		}
	}
}

// An http remote whose server moves every request to TLS on the same host
// keeps its credentials there: the first hop already sent them to that host.
// Any other change of scheme, port or host still drops them.
func TestEgressRedirectKeepsCredentialsOnUpgradeToTLS(t *testing.T) {
	check := newClient(nil, resolvesTo("203.0.113.10")).CheckRedirect
	first := httptest.NewRequest(http.MethodGet, "http://nexus.corp/repository/maven-public/a", nil)
	for target, keep := range map[string]bool{
		"https://nexus.corp/repository/maven-public/a":     true,
		"https://nexus.corp:443/repository/maven-public/a": true,
		"http://nexus.corp/b":                              true,
		"https://nexus.corp:8443/a":                        false,
		"http://nexus.corp:8081/a":                         false,
		"https://cdn.nexus.corp/a":                         false,
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Basic dTpw")
		if err := check(req, []*http.Request{first}); err != nil {
			t.Fatal(err)
		}
		if kept := req.Header.Get("Authorization") != ""; kept != keep {
			t.Errorf("%s: Authorization kept = %v, want %v", target, kept, keep)
		}
	}
	// On a remote at a non-default port the move to TLS is a new origin.
	req := httptest.NewRequest(http.MethodGet, "https://nexus.corp/a", nil)
	req.Header.Set("Authorization", "Basic dTpw")
	if err := check(req, []*http.Request{httptest.NewRequest(http.MethodGet, "http://nexus.corp:8081/a", nil)}); err != nil || req.Header.Get("Authorization") != "" {
		t.Errorf("http://nexus.corp:8081 to https://nexus.corp kept the credentials (err %v)", err)
	}
}

// The remote's name is looked up on every request to classify it. When the
// lookup fails, the classes the last successful one found, at most classTTL
// ago, stand in: a DNS blip does not make an internal remote public, which
// would fail its fetches even with warm connections in its pool. With nothing
// known the remote counts as public, the most restricted.
func TestEgressKeepsRemoteClassesWhenLookupFails(t *testing.T) {
	defer func(d time.Duration) { classTTL = d }(classTTL)
	internal, hits := internalServer(t)
	_, port, _ := net.SplitHostPort(internal.Listener.Addr().String())
	rp := proxyRepo("mirror", "http://localhost:"+port+"/")
	var down atomic.Bool
	f, _, _ := setup(t, &remote{})
	f.Client = newClient(nil, func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if down.Load() {
			return nil, &net.DNSError{Err: "server misbehaving", Name: host, IsTemporary: true}
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})

	if _, err := f.Fetch(ctx, rp, "a.jar", Content); err != nil {
		t.Fatalf("lookup working: %v", err)
	}
	down.Store(true)
	if _, err := f.Fetch(ctx, rp, "b.jar", Content); err != nil {
		t.Fatalf("lookup failing, remote known to be loopback: %v", err)
	}
	classTTL = 0
	if _, err := f.Fetch(ctx, rp, "c.jar", Content); !errors.Is(err, ErrRemote) {
		t.Fatalf("lookup failing, last answer too old: err = %v, want ErrRemote", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("internal hits = %d, want 2", hits.Load())
	}
}

// An internal remote keeps working, redirects to other internal hosts
// included (here loopback, where the remote itself is).
func TestEgressInternalRemoteFollowsInternalRedirect(t *testing.T) {
	internal, hits := internalServer(t)
	mirror := httptest.NewServer(http.RedirectHandler(internal.URL+"/blob", http.StatusFound))
	defer mirror.Close()
	f, _, _ := setup(t, &remote{})
	f.Client = NewClient()

	res, err := f.Fetch(ctx, proxyRepo("mirror", mirror.URL), "g/a.jar", Content)
	if err != nil || read(t, f, res.Asset) != "internal" || hits.Load() != 1 {
		t.Fatalf("res=%+v err=%v hits=%d", res, err, hits.Load())
	}
}

func TestEgressStopsAfterMaxRedirects(t *testing.T) {
	var hits atomic.Int32
	loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("/hop%d", hits.Add(1)), http.StatusFound)
	}))
	defer loop.Close()
	f, _, _ := setup(t, &remote{})
	f.Client = NewClient()

	if _, err := f.Fetch(ctx, proxyRepo("loop", loop.URL), "g/a.jar", Content); !errors.Is(err, ErrRemote) {
		t.Fatalf("err = %v, want ErrRemote", err)
	}
	if got := hits.Load(); got != maxRedirects+1 {
		t.Fatalf("requests = %d, want %d (the first plus %d redirects)", got, maxRedirects+1, maxRedirects)
	}
}
