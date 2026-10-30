package proxy

import (
	"context"
	"crypto/tls"
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

	"commandcenter/internal/truststore"
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
// numericHost or non-ASCII host is refused on either path, whatever the
// remote.
type egress struct {
	proxy  func(*http.Request) (*url.URL, error) // the environment's proxy (ProxyFromEnvironment, or a test's)
	tls    *tls.Config                           // the caller's TLS configuration; nil: the trust store
	lookup func(ctx context.Context, network, host string) ([]netip.Addr, error)

	mu     sync.Mutex
	cfg    *Config // the Config base was built under; rebuilt when live changes
	base   *http.Transport
	direct map[transportKey]*http.Transport
	// ponytail: never pruned, one entry per remote host ever configured;
	// drop expired entries if remotes churn.
	known map[string]classified
}

// transportKey selects a clone of base: the classes a direct connection
// may reach (0: through base's forward proxy, unchecked) and a
// repository's connection timeout (0: the Config's).
type transportKey struct {
	allowed int
	timeout time.Duration
}

type classified struct {
	classes int
	at      time.Time
}

// current returns the base transport under the live Config, rebuilding it
// (and dropping the per-policy clones and their idle connections) when
// the Config changed since the last request.
func (e *egress) current() *http.Transport {
	cfg := current()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.base != nil && e.cfg == cfg {
		return e.base
	}
	old := e.base
	e.cfg = cfg
	e.base = &http.Transport{
		Proxy:                 cfg.proxyFunc(e.proxy),
		DialContext:           newDialer(cfg, nil).DialContext,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: cfg.headerTimeout(),
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       e.tlsConfig(),
	}
	for _, t := range e.direct {
		t.CloseIdleConnections()
	}
	e.direct = map[transportKey]*http.Transport{}
	if old != nil {
		old.CloseIdleConnections()
	}
	return e.base
}

// tlsConfig is the caller's TLS configuration, or one trusting the trust
// store (nil: the system roots, as before).
func (e *egress) tlsConfig() *tls.Config {
	if e.tls != nil {
		return e.tls.Clone()
	}
	if pool := truststore.Pool(); pool != nil {
		return &tls.Config{RootCAs: pool}
	}
	return nil
}

func (c *Config) connectTimeout() time.Duration {
	if c.ConnectTimeout > 0 {
		return c.ConnectTimeout
	}
	return 10 * time.Second
}

func (c *Config) headerTimeout() time.Duration {
	if c.HeaderTimeout > 0 {
		return c.HeaderTimeout
	}
	return 30 * time.Second
}

func (e *egress) RoundTrip(req *http.Request) (*http.Response, error) {
	cfg := current()
	suffix, retries := cfg.UserAgentSuffix, cfg.Retries
	if c := connectionOf(req.Context()); c != nil { // a repository's own settings
		if c.UserAgentSuffix != "" {
			suffix = c.UserAgentSuffix
		}
		if c.Retries != nil {
			retries = *c.Retries
		}
	}
	if suffix != "" {
		if ua := req.Header.Get("User-Agent"); ua != "" {
			req = req.Clone(req.Context())
			req.Header.Set("User-Agent", ua+" "+suffix)
		}
	}
	for attempt := 0; ; attempt++ {
		resp, err := e.roundTrip(req)
		if err == nil || attempt >= retries || !retryable(req, err) {
			return resp, err
		}
		select {
		case <-time.After(retryWait):
		case <-req.Context().Done():
			return nil, err
		}
	}
}

func (e *egress) roundTrip(req *http.Request) (*http.Response, error) {
	// net/http maps a non-ASCII host with UTS #46 (fullwidth digits and dots
	// become ASCII ones) for the dial and the CONNECT target, past the checks
	// here; registries use ASCII or punycode host names.
	if h := req.URL.Hostname(); numericHost(h) || strings.ContainsFunc(h, func(r rune) bool { return r >= 0x80 }) {
		return nil, errEgress
	}
	var remote string
	if u, _ := req.Context().Value(remoteKey{}).(*url.URL); u != nil {
		remote = u.Hostname()
	}
	var timeout time.Duration
	if c := connectionOf(req.Context()); c != nil {
		timeout = time.Duration(c.Timeout) * time.Second
	}
	base := e.current()
	if base.Proxy != nil {
		px, err := base.Proxy(req)
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
			return e.transport(transportKey{timeout: timeout}).RoundTrip(req)
		}
	}
	return e.transport(transportKey{allowedFor(e.classes(req.Context(), remote)), timeout}).RoundTrip(req)
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

func (e *egress) transport(k transportKey) *http.Transport {
	base := e.current()
	if k == (transportKey{}) {
		return base
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t := e.direct[k]
	if t == nil {
		t = base.Clone()
		cfg := *e.cfg
		if k.timeout > 0 {
			cfg.ConnectTimeout, t.ResponseHeaderTimeout = k.timeout, k.timeout
		}
		var control func(network, address string, c syscall.RawConn) error
		if k.allowed != 0 {
			t.Proxy = nil
			control = func(_, address string, _ syscall.RawConn) error {
				ap, err := netip.ParseAddrPort(address)
				if err != nil {
					return err
				}
				return checkAddr(ap.Addr(), k.allowed)
			}
		}
		t.DialContext = newDialer(&cfg, control).DialContext
		e.direct[k] = t
	}
	return t
}

func newDialer(cfg *Config, control func(network, address string, c syscall.RawConn) error) *net.Dialer {
	return &net.Dialer{Timeout: cfg.connectTimeout(), KeepAlive: 30 * time.Second, Control: control}
}

// NewClient returns the HTTP client for remote fetches. It follows at most
// maxRedirects redirects and connects only where the egress policy allows:
// a repository whose remote is public never reaches a private, loopback,
// link-local, unspecified or multicast address, whether a redirect or an
// index-supplied URL leads there; an internal remote may also reach other
// private hosts, and any class the remote itself resolves to. HTTP_PROXY,
// HTTPS_PROXY and NO_PROXY apply as usual, unless the http.proxy setting
// names a forward proxy (SetSettings), which then takes every request but
// its nonProxyHosts; through a forward proxy only an address-literal
// destination can be checked. The live Config also sets the timeouts, the
// User-Agent suffix and the retries, and the trust store the root CAs. An IPv4 address spelled other
// than in dotted-decimal form (10.1, 167772161), and a host name that is not
// ASCII (its punycode form works), are refused everywhere.
func NewClient() *http.Client {
	return newClient(http.ProxyFromEnvironment, net.DefaultResolver.LookupNetIP)
}

func newClient(proxy func(*http.Request) (*url.URL, error), lookup func(context.Context, string, string) ([]netip.Addr, error)) *http.Client {
	return &http.Client{
		Transport: &egress{
			proxy:  proxy,
			lookup: lookup,
			direct: map[transportKey]*http.Transport{},
			known:  map[string]classified{},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			// net/http keeps the first request's Authorization for its host
			// name on any scheme or port, and for subdomains: credentials
			// stay with the first request's own origin (sendsCredentials),
			// or follow it to TLS on the same host (http:80 to https:443),
			// where the first hop already sent them to that host.
			if origin(req.URL) != origin(via[0].URL) && !upgradesToTLS(via[0].URL, req.URL) {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
}

// upgradesToTLS reports whether to is from's host moved from http on port 80
// to https on port 443.
func upgradesToTLS(from, to *url.URL) bool {
	h := from.Hostname()
	return to.Hostname() == h && origin(from) == "http://"+net.JoinHostPort(h, "80") && origin(to) == "https://"+net.JoinHostPort(h, "443")
}

// origin is u's scheme, host name and port, an omitted port being the
// scheme's default.
func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return u.Scheme + "://" + net.JoinHostPort(u.Hostname(), port)
}
