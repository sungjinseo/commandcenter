package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"syscall"
	"time"
)

// maxRedirects bounds the redirects one remote request follows.
const maxRedirects = 5

// Address classes of the egress policy, as bits of a set.
const (
	classPublic  = 1 << iota
	classPrivate // RFC 1918, CGNAT 100.64.0.0/10, IPv6 ULA fc00::/7
	classLoopback
	classLinkLocal   // 169.254.0.0/16 (cloud metadata), fe80::/10
	classUnspecified // 0.0.0.0/8, ::
	classMulticast
)

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func classOf(ip netip.Addr) int {
	ip = ip.Unmap()
	switch {
	case ip.IsUnspecified() || ip.Is4() && ip.As4()[0] == 0:
		return classUnspecified
	case ip.IsLoopback():
		return classLoopback
	case ip.IsMulticast():
		return classMulticast
	case ip.IsLinkLocalUnicast():
		return classLinkLocal
	case ip.IsPrivate() || cgnat.Contains(ip):
		return classPrivate
	}
	return classPublic
}

// allowedFor returns the classes a fetch may connect to when its configured
// remote resolves to addresses of the classes in remote: public addresses,
// whatever the remote itself points at, and private hosts when the remote is
// not public (a company mirror may redirect to its other internal hosts; a
// public registry, or a URL its index hands out, may not).
func allowedFor(remote int) int {
	if remote&^classPublic != 0 {
		remote |= classPrivate
	}
	return remote | classPublic
}

var errEgress = errors.New("address refused by the remote egress policy")

func checkAddr(ip netip.Addr, allowed int) error {
	if classOf(ip)&allowed == 0 {
		return errEgress
	}
	return nil
}

type remoteKey struct{}

// withRemote marks ctx as a fetch for the repository whose remote is u.
func withRemote(ctx context.Context, u *url.URL) context.Context {
	return context.WithValue(ctx, remoteKey{}, u)
}

// egress is the transport of NewClient. A direct request goes through a
// transport whose dialer checks every resolved address at connect time (so a
// name that rebinds after an earlier lookup is still caught) against the
// classes allowedFor the remote of the request's context. Each allowed set
// has its own transport: a pooled connection is only reused under the policy
// that dialed it. A request that base.Proxy routes to a forward proxy is
// connected, and its host resolved, by that proxy: only an address literal
// can be checked then, and the proxy's own address is the admin's choice.
type egress struct {
	base   *http.Transport
	lookup func(ctx context.Context, network, host string) ([]netip.Addr, error)

	mu     sync.Mutex
	direct map[int]*http.Transport
}

func (e *egress) RoundTrip(req *http.Request) (*http.Response, error) {
	var remote string
	if u, _ := req.Context().Value(remoteKey{}).(*url.URL); u != nil {
		remote = u.Hostname()
	}
	if e.base.Proxy != nil {
		px, err := e.base.Proxy(req)
		if err != nil {
			return nil, err
		}
		if px != nil {
			class := classPublic // no DNS here: the proxy may be the only resolver
			if ip, err := netip.ParseAddr(remote); err == nil {
				class = classOf(ip)
			}
			if ip, err := netip.ParseAddr(req.URL.Hostname()); err == nil {
				if err := checkAddr(ip, allowedFor(class)); err != nil {
					return nil, err
				}
			}
			return e.base.RoundTrip(req)
		}
	}
	return e.transport(allowedFor(e.classes(req.Context(), remote))).RoundTrip(req)
}

// classes returns the classes of the addresses host resolves to. A host that
// does not resolve counts as public, the most restricted remote.
func (e *egress) classes(ctx context.Context, host string) int {
	ips, err := e.lookup(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return classPublic
	}
	c := 0
	for _, ip := range ips {
		c |= classOf(ip)
	}
	return c
}

func (e *egress) transport(allowed int) *http.Transport {
	e.mu.Lock()
	defer e.mu.Unlock()
	t := e.direct[allowed]
	if t == nil {
		t = e.base.Clone()
		t.Proxy = nil
		t.DialContext = newDialer(func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			return checkAddr(ap.Addr(), allowed)
		}).DialContext
		e.direct[allowed] = t
	}
	return t
}

func newDialer(control func(network, address string, c syscall.RawConn) error) *net.Dialer {
	return &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: control}
}

// NewClient returns the HTTP client for remote fetches. It follows at most
// maxRedirects redirects and connects only where the egress policy allows:
// a repository whose remote is public never reaches a private, loopback,
// link-local, unspecified or multicast address, whether a redirect or an
// index-supplied URL leads there; an internal remote may also reach other
// private hosts, and any class the remote itself resolves to. HTTP_PROXY,
// HTTPS_PROXY and NO_PROXY apply as usual; through a forward proxy only an
// address-literal destination can be checked.
func NewClient() *http.Client {
	return newClient(http.ProxyFromEnvironment, net.DefaultResolver.LookupNetIP)
}

func newClient(proxy func(*http.Request) (*url.URL, error), lookup func(context.Context, string, string) ([]netip.Addr, error)) *http.Client {
	return &http.Client{
		Transport: &egress{
			base: &http.Transport{
				Proxy:                 proxy,
				DialContext:           newDialer(nil).DialContext,
				MaxIdleConnsPerHost:   8,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				ForceAttemptHTTP2:     true,
			},
			lookup: lookup,
			direct: map[int]*http.Transport{},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}
}
