package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/truststore"
)

// Config is the live outbound HTTP configuration (the http general setting)
// every client of this package reads per request: a change applies to the
// next request, and the transports are rebuilt (idle connections dropped).
type Config struct {
	UserAgentSuffix string
	ConnectTimeout  time.Duration // 0: 10 s
	HeaderTimeout   time.Duration // wait for a response's headers; 0: 30 s
	Retries         int           // idempotent requests repeated after a network error, 1 s apart
	Proxy           *url.URL      // the forward proxy; nil: the HTTP_PROXY, HTTPS_PROXY and NO_PROXY environment
	NoProxy         []string      // hosts reached directly when Proxy is set
}

var live atomic.Pointer[Config]

// SetConfig installs c for every client from now on.
func SetConfig(c Config) { live.Store(&c) }

// Current returns the live Config.
func Current() Config { return *current() }

func current() *Config {
	if c := live.Load(); c != nil {
		return c
	}
	return &Config{}
}

// SetSettings installs the outbound HTTP settings and the trust store of s.
func SetSettings(s meta.Settings) {
	h := s.HTTP
	c := Config{
		UserAgentSuffix: h.UserAgentSuffix,
		ConnectTimeout:  time.Duration(h.ConnectTimeoutSeconds) * time.Second,
		HeaderTimeout:   time.Duration(h.TimeoutSeconds) * time.Second,
		Retries:         h.Retries,
		NoProxy:         h.Proxy.NonProxyHosts,
	}
	if h.Proxy.Host != "" {
		c.Proxy = &url.URL{Scheme: "http", Host: net.JoinHostPort(h.Proxy.Host, strconv.Itoa(h.Proxy.Port))}
		if h.Proxy.Username != "" {
			c.Proxy.User = url.UserPassword(h.Proxy.Username, h.Proxy.Password)
		}
	}
	truststore.Set(truststore.Build(s.TrustedCertificates()))
	SetConfig(c)
}

// proxyFunc is the Proxy function of a transport under c: the configured
// proxy for every host that no NoProxy pattern matches, else env.
func (c *Config) proxyFunc(env func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	if c.Proxy == nil {
		return env
	}
	return func(req *http.Request) (*url.URL, error) {
		if noProxy(req.URL.Hostname(), c.NoProxy) {
			return nil, nil
		}
		return c.Proxy, nil
	}
}

// noProxy reports whether host matches a pattern, case-insensitively, as
// Java's http.nonProxyHosts globs do: the host itself, a "*" at the start
// (any prefix: "*.corp.local", "*corp.local"), at the end (any suffix:
// "10.*", "build*") or both, plus ".suffix" for any subdomain of suffix.
func noProxy(host string, patterns []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSuffix(p, "."))
		core := strings.Trim(p, "*")
		lead, trail := strings.HasPrefix(p, "*"), strings.HasSuffix(p, "*")
		switch {
		case p == host,
			strings.HasPrefix(p, ".") && strings.HasSuffix(host, p),
			lead && trail && strings.Contains(host, core),
			lead && !trail && strings.HasSuffix(host, core),
			trail && !lead && strings.HasPrefix(host, core):
			return true
		}
	}
	return false
}

// retryWait is the pause between retries; a var for tests.
var retryWait = time.Second

// retryable reports whether req may be sent again: GET or HEAD with no
// body, and the error is from the network, not from the caller's context.
func retryable(req *http.Request, err error) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead || req.Body != nil && req.Body != http.NoBody {
		return false
	}
	if req.Context().Err() != nil || errors.Is(err, errEgress) {
		return false
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	var netErr net.Error
	// ponytail: a closed connection surfaces as io.EOF / "unexpected EOF"
	// text, not a net.Error; match the text until net/http types it.
	return errors.As(err, &netErr) || strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "connection reset")
}

// RetrieveCertificates connects to host:port with TLS, under the egress
// policy and the proxy settings, without verifying the server, and returns
// the certificate chain it presented, leaf first. The service need not
// speak HTTP: the chain is captured during the handshake.
func RetrieveCertificates(ctx context.Context, host string, port int) ([]*x509.Certificate, error) {
	if host == "" || port < 1 || port > 65535 {
		return nil, errors.New("host and port are required")
	}
	remote := &url.URL{Scheme: "https", Host: net.JoinHostPort(host, strconv.Itoa(port))}
	var chain []*x509.Certificate
	tc := &tls.Config{InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
		chain = cs.PeerCertificates
		return nil
	}}
	c := NewClientFor(remote, tc)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, remote.String()+"/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := c.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	c.CloseIdleConnections()
	if len(chain) == 0 {
		if err != nil {
			return nil, fmt.Errorf("no certificate received: %w", err)
		}
		return nil, errors.New("no certificate received")
	}
	return chain, nil
}
