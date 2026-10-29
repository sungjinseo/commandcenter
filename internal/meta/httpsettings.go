package meta

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
	"unicode"
)

// Operations settings: outbound HTTP, the Prometheus endpoint's access and
// the SSL trust store. Each lives under its own settings key.
const (
	SettingHTTP                = "http"
	SettingMetricsAnonymous    = "metricsAnonymous"
	SettingTrustedCertificates = "trustedCertificates"
)

// HTTPProxy is the forward proxy for every outbound HTTP and HTTPS request
// (remote fetches, webhooks, OIDC). An empty Host means none: the
// HTTP_PROXY, HTTPS_PROXY and NO_PROXY environment applies. Password is a
// secret: stored sealed, never returned by the API. NonProxyHosts are
// patterns of hosts reached directly: a host name, "*.corp.local" (any
// subdomain) or "10.*" (an address prefix), compared case-insensitively.
type HTTPProxy struct {
	Host          string   `json:"host"`
	Port          int      `json:"port"`
	Username      string   `json:"username"`
	Password      string   `json:"password"`
	NonProxyHosts []string `json:"nonProxyHosts"`
}

// HTTP is the outbound HTTP configuration. UserAgentSuffix is appended to
// the User-Agent of remote fetches; ConnectTimeoutSeconds bounds a dial,
// TimeoutSeconds the wait for a response's headers; Retries is how many
// times an idempotent request is repeated after a network error (not a
// response), one second apart.
type HTTP struct {
	UserAgentSuffix       string    `json:"userAgentSuffix"`
	ConnectTimeoutSeconds int       `json:"connectTimeoutSeconds"`
	TimeoutSeconds        int       `json:"timeoutSeconds"`
	Retries               int       `json:"retries"`
	Proxy                 HTTPProxy `json:"proxy"`
}

// DefaultHTTP returns the values of an unset http key.
func DefaultHTTP() HTTP {
	return HTTP{ConnectTimeoutSeconds: 10, TimeoutSeconds: 30, Proxy: HTTPProxy{NonProxyHosts: []string{}}}
}

// Copy returns c with its own slice.
func (c HTTP) Copy() HTTP {
	c.Proxy.NonProxyHosts = append([]string{}, c.Proxy.NonProxyHosts...)
	return c
}

// Limits of the HTTP settings.
const (
	MaxHTTPTimeoutSeconds  = 600
	MaxHTTPRetries         = 10
	MaxUserAgentSuffixLen  = 100
	MaxTrustedCertificates = 200
)

func hasSpaceOrControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

// ValidateHTTP checks c.
func ValidateHTTP(c HTTP) error {
	if len(c.UserAgentSuffix) > MaxUserAgentSuffixLen {
		return fmt.Errorf("http.userAgentSuffix must be at most %d characters", MaxUserAgentSuffixLen)
	}
	if strings.ContainsFunc(c.UserAgentSuffix, func(r rune) bool { return r < 0x20 || r > 0x7e }) {
		return fmt.Errorf("http.userAgentSuffix must be printable ASCII")
	}
	if c.ConnectTimeoutSeconds < 1 || c.ConnectTimeoutSeconds > MaxHTTPTimeoutSeconds {
		return fmt.Errorf("http.connectTimeoutSeconds must be between 1 and %d", MaxHTTPTimeoutSeconds)
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > MaxHTTPTimeoutSeconds {
		return fmt.Errorf("http.timeoutSeconds must be between 1 and %d", MaxHTTPTimeoutSeconds)
	}
	if c.Retries < 0 || c.Retries > MaxHTTPRetries {
		return fmt.Errorf("http.retries must be between 0 and %d", MaxHTTPRetries)
	}
	p := c.Proxy
	if hasSpaceOrControl(p.Host) || strings.ContainsAny(p.Host, "/@:") {
		return fmt.Errorf("http.proxy.host must be a host name or address without scheme, port or spaces")
	}
	if p.Host != "" && (p.Port < 1 || p.Port > 65535) {
		return fmt.Errorf("http.proxy.port must be between 1 and 65535")
	}
	if hasSpaceOrControl(p.Username) {
		return fmt.Errorf("http.proxy.username must not contain spaces or control characters")
	}
	for _, h := range p.NonProxyHosts {
		if h == "" || hasSpaceOrControl(h) || strings.ContainsAny(h, "/@") {
			return fmt.Errorf("http.proxy.nonProxyHosts: %q is not a host pattern", h)
		}
	}
	return nil
}

// ParseCertificates decodes every CERTIFICATE block of a PEM text. Other
// blocks are ignored; a text without a certificate is an error.
func ParseCertificates(pemText string) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := []byte(pemText)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("certificate does not parse: %w", err)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no PEM certificate found")
	}
	return out, nil
}

// Fingerprint is the id of a certificate in the trust store: the lowercase
// hex SHA-256 of its DER encoding.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}

// ValidateTrustedCertificates checks the trust store: each entry one PEM
// certificate, no two the same.
func ValidateTrustedCertificates(pems []string) error {
	if len(pems) > MaxTrustedCertificates {
		return fmt.Errorf("trustedCertificates: at most %d certificates", MaxTrustedCertificates)
	}
	seen := map[string]bool{}
	for i, p := range pems {
		certs, err := ParseCertificates(p)
		if err != nil {
			return fmt.Errorf("trustedCertificates[%d]: %w", i, err)
		}
		if len(certs) != 1 {
			return fmt.Errorf("trustedCertificates[%d]: one certificate per entry", i)
		}
		id := Fingerprint(certs[0])
		if seen[id] {
			return fmt.Errorf("trustedCertificates[%d]: certificate %s is listed twice", i, id)
		}
		seen[id] = true
	}
	return nil
}

// TrustedCertificates parses the stored trust store; entries that do not
// parse are skipped (ValidateTrustedCertificates refuses them on write).
func (s Settings) TrustedCertificates() []*x509.Certificate {
	var out []*x509.Certificate
	for _, p := range s.TrustedCerts {
		if certs, err := ParseCertificates(p); err == nil {
			out = append(out, certs...)
		}
	}
	return out
}
