package proxy

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
)

// NewClientFor returns NewClient's client for requests to the service at
// remote (an identity provider, say), policed as a proxy repository's
// fetches to its remote are: every request may reach public addresses and
// the classes remote itself resolves to (private hosts too when remote is
// not public), so a redirect or a discovery document cannot lead it to the
// cloud metadata address or the loopback. tlsConfig applies to the direct
// connections; nil keeps the defaults.
func NewClientFor(remote *url.URL, tlsConfig *tls.Config) *http.Client {
	c := newClient(http.ProxyFromEnvironment, net.DefaultResolver.LookupNetIP)
	e := c.Transport.(*egress)
	e.base.TLSClientConfig = tlsConfig // transport clones it before the first dial
	c.Transport = forRemote{e, remote}
	return c
}

type forRemote struct {
	http.RoundTripper
	remote *url.URL
}

func (f forRemote) RoundTrip(req *http.Request) (*http.Response, error) {
	return f.RoundTripper.RoundTrip(req.WithContext(withRemote(req.Context(), f.remote)))
}
