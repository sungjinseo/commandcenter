package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
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

var (
	cgnat = netip.MustParsePrefix("100.64.0.0/10")
	// NAT64 (RFC 6052, RFC 8215): the translator connects to the IPv4
	// address in the last 32 bits.
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48")
)

func classOf(ip netip.Addr) int {
	ip = ip.Unmap()
	// ponytail: the /96 format only; a local prefix shorter than /96 (RFC 6052
	// splits the IPv4 address around bits 64-71 then) or a network-specific
	// prefix is not recognised. Make the prefixes a setting if one shows up.
	if a := ip.WithZone(""); nat64.Contains(a) || nat64Local.Contains(a) {
		b := a.As16()
		ip = netip.AddrFrom4([4]byte(b[12:]))
	}
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

// numericHost reports whether host, which netip does not parse, ends in a
// number as the WHATWG URL standard puts it (a decimal or 0x hex label).
// inet_aton, and so getaddrinfo and most forward proxies, reads 10.1,
// 167772161, 0x0a000001 or 012.0.0.1 as an IPv4 address that the checks here
// would not see. No top-level domain is numeric.
func numericHost(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	last := strings.TrimSuffix(host, ".")
	last = strings.ToLower(last[strings.LastIndexByte(last, '.')+1:])
	if hex, ok := strings.CutPrefix(last, "0x"); ok {
		return strings.Trim(hex, "0123456789abcdef") == ""
	}
	return last != "" && strings.Trim(last, "0123456789") == ""
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
// can be checked then, and the proxy's own address is the admin's choice. A
// numericHost is refused on either path, whatever the remote.
type egress struct {
	base   *http.Transport
	lookup func(ctx context.Context, network, host string) ([]netip.Addr, error)

	mu     sync.Mutex
	direct map[int]*http.Transport
	// ponytail: never pruned, one entry per remote host ever configured;
	// drop expired entries if remotes churn.
	known map[string]classified
}

type classified struct {
	classes int
	at      time.Time
}

func (e *egress) RoundTrip(req *http.Request) (*http.Response, error) {
	if numericHost(req.URL.Hostname()) {
		return nil, errEgress
	}
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

// classTTL is how long the classes a lookup of a remote host found stand in
// for a later lookup that fails; a var for tests.
var classTTL = 10 * time.Minute

// classes returns the classes of the addresses host resolves to. When the
// lookup fails, what the last successful one found within classTTL stands in,
// so a DNS blip neither makes an internal remote public nor strands the
// connections pooled under its policy. A host with no such answer counts as
// public, the most restricted remote.
func (e *egress) classes(ctx context.Context, host string) int {
	ips, err := e.lookup(ctx, "ip", host)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil || len(ips) == 0 {
		if k, ok := e.known[host]; ok && time.Since(k.at) < classTTL {
			return k.classes
		}
		return classPublic
	}
	c := 0
	for _, ip := range ips {
		c |= classOf(ip)
	}
	e.known[host] = classified{c, time.Now()}
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
// address-literal destination can be checked. An IPv4 address spelled other
// than in dotted-decimal form (10.1, 167772161) is refused everywhere.
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
			known:  map[string]classified{},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}
}
