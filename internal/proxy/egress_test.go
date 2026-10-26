package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
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
