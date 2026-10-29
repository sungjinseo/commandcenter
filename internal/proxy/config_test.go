package proxy

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/truststore"
)

func resetConfig(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { SetConfig(Config{}); truststore.Set(nil) })
}

// get fetches u as a request for the remote at u's origin.
func get(t *testing.T, c *http.Client, u string) (*http.Response, string, error) {
	t.Helper()
	ru, _ := url.Parse(u)
	req, _ := http.NewRequestWithContext(withRemote(context.Background(), ru), http.MethodGet, u, nil)
	req.Header.Set("User-Agent", UserAgent)
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b), nil
}

func TestNoProxyPatterns(t *testing.T) {
	for host, want := range map[string]bool{
		"repo.corp.local": true, "CORP.LOCAL": false, "x.y.corp.local": true, "10.1.2.3": true, "10.1": true,
		"192.168.1.1": false, "mirror.example": true, "mirror.example.com": false, "Mirror.Example": true,
	} {
		if got := noProxy(host, []string{"*.corp.local", "10.*", "mirror.example"}); got != want {
			t.Errorf("noProxy(%q) = %v, want %v", host, got, want)
		}
	}
	if !noProxy("anything", []string{"*"}) || noProxy("anything", nil) {
		t.Error("wildcard or empty list wrong")
	}
}

// The http.proxy setting routes plain-http fetches through the forward
// proxy, except hosts in nonProxyHosts; a User-Agent suffix is appended.
func TestProxySettingAndUserAgentSuffix(t *testing.T) {
	resetConfig(t)
	var ua atomic.Value
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua.Store(r.Header.Get("User-Agent"))
		io.WriteString(w, "direct")
	}))
	defer remote.Close()
	var proxied atomic.Int32
	fwd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() {
			http.Error(w, "not a proxy request", 400)
			return
		}
		proxied.Add(1)
		io.WriteString(w, "via-proxy")
	}))
	defer fwd.Close()
	pu, _ := url.Parse(fwd.URL)
	c := newClient(nil, resolvesTo("127.0.0.1"))

	SetConfig(Config{Proxy: pu})
	if _, body, err := get(t, c, remote.URL+"/a"); err != nil || body != "via-proxy" {
		t.Fatalf("through proxy: %q %v", body, err)
	}
	SetConfig(Config{Proxy: pu, NoProxy: []string{"127.*"}, UserAgentSuffix: "corp-mirror/2"})
	if _, body, err := get(t, c, remote.URL+"/b"); err != nil || body != "direct" {
		t.Fatalf("non-proxy host: %q %v", body, err)
	}
	if got := ua.Load(); got != UserAgent+" corp-mirror/2" {
		t.Fatalf("User-Agent %q", got)
	}
	if proxied.Load() != 1 {
		t.Fatalf("proxied %d requests, want 1", proxied.Load())
	}
	// Credentials in the proxy URL reach the proxy as Proxy-Authorization.
	authed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Header.Get("Proxy-Authorization"))
	}))
	defer authed.Close()
	au, _ := url.Parse(authed.URL)
	au.User = url.UserPassword("bob", "s3cret")
	SetConfig(Config{Proxy: au})
	if _, body, err := get(t, c, remote.URL+"/c"); err != nil || !strings.HasPrefix(body, "Basic ") {
		t.Fatalf("proxy auth: %q %v", body, err)
	}
}

func TestSetSettingsBuildsProxyURL(t *testing.T) {
	resetConfig(t)
	s := meta.DefaultSettings()
	s.HTTP.Proxy = meta.HTTPProxy{Host: "proxy.corp", Port: 3128, Username: "u", Password: "p", NonProxyHosts: []string{"*.corp"}}
	s.HTTP.UserAgentSuffix = "x"
	s.HTTP.Retries = 3
	SetSettings(s)
	c := Current()
	if c.Proxy == nil || c.Proxy.String() != "http://u:p@proxy.corp:3128" || c.Retries != 3 || c.UserAgentSuffix != "x" ||
		c.ConnectTimeout != 10*time.Second || c.HeaderTimeout != 30*time.Second || len(c.NoProxy) != 1 {
		t.Fatalf("config %+v", c)
	}
	s.HTTP.Proxy.Host = ""
	SetSettings(s)
	if Current().Proxy != nil {
		t.Fatal("empty host still has a proxy")
	}
}

// Retries repeat an idempotent request after a network error; a response,
// even an error status, is never retried.
func TestRetries(t *testing.T) {
	resetConfig(t)
	retryWait = time.Millisecond
	defer func() { retryWait = time.Second }()
	var hits atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			panic(http.ErrAbortHandler) // the connection is closed without a response
		}
		io.WriteString(w, "ok")
	}))
	defer remote.Close()
	c := newClient(nil, resolvesTo("127.0.0.1"))
	if _, _, err := get(t, c, remote.URL+"/x"); err == nil {
		t.Fatal("no retries: expected a failure")
	}
	hits.Store(0)
	SetConfig(Config{Retries: 2})
	if _, body, err := get(t, c, remote.URL+"/x"); err != nil || body != "ok" {
		t.Fatalf("with retries: %q %v", body, err)
	}
	if hits.Load() != 3 {
		t.Fatalf("hits %d, want 3", hits.Load())
	}
	// A 503 is a response: no retry.
	hits.Store(0)
	status := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(503)
	}))
	defer status.Close()
	if resp, _, err := get(t, c, status.URL+"/y"); err != nil || resp.StatusCode != 503 || hits.Load() != 1 {
		t.Fatalf("503: %v %v hits=%d", resp, err, hits.Load())
	}
}

// A fetch from a TLS remote with a private CA fails until the CA is in the
// trust store, and works after; RetrieveCertificates returns its chain.
func TestTrustStore(t *testing.T) {
	resetConfig(t)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "tls") }))
	defer ts.Close()
	c := newClient(nil, resolvesTo("127.0.0.1"))
	if _, _, err := get(t, c, ts.URL+"/a"); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("untrusted CA: err = %v", err)
	}
	s := meta.DefaultSettings()
	s.TrustedCerts = []string{string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}))}
	SetSettings(s)
	if _, body, err := get(t, c, ts.URL+"/b"); err != nil || body != "tls" {
		t.Fatalf("trusted: %q %v", body, err)
	}
	s.TrustedCerts = nil
	SetSettings(s)
	if _, _, err := get(t, c, ts.URL+"/c"); err == nil {
		t.Fatal("removed CA still trusted")
	}

	u, _ := url.Parse(ts.URL)
	port := 0
	for _, ch := range u.Port() {
		port = port*10 + int(ch-'0')
	}
	chain, err := RetrieveCertificates(context.Background(), "127.0.0.1", port)
	if err != nil || len(chain) == 0 || !chain[0].Equal(ts.Certificate()) {
		t.Fatalf("retrieve: %d certs, %v", len(chain), err)
	}
	if _, err := RetrieveCertificates(context.Background(), "127.0.0.1", 1); err == nil {
		t.Fatal("closed port: no error")
	}
	var _ *x509.Certificate = chain[0]
}
