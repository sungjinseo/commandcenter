package config

import (
	"fmt"
	"strings"
)

// Connection is a proxy's own outbound HTTP connection settings over the
// global ones (Nexus httpClient.connection); a zero field follows them.
type Connection struct {
	// Timeout, in seconds (1-3600), bounds the connect, the wait for the
	// response headers and each wait for body data, in place of the global
	// timeouts; 0 follows them.
	Timeout int `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// Retries (0-10) replaces the global retries; nil follows them.
	Retries *int `yaml:"retries,omitempty" json:"retries,omitempty"`
	// UserAgentSuffix replaces the global User-Agent suffix.
	UserAgentSuffix string `yaml:"userAgentSuffix,omitempty" json:"userAgentSuffix,omitempty"`
	// EnableCookies keeps the cookies the remote sets, per repository and
	// node in memory, and sends them back to it.
	EnableCookies bool `yaml:"enableCookies,omitempty" json:"enableCookies,omitempty"`
	// EnableCircularRedirects and UseTrustStore are kept for Nexus clients
	// and change nothing: a redirect back to a URL already visited is
	// followed within the redirect limit either way, and the SSL trust
	// store applies to every outbound connection.
	EnableCircularRedirects bool `yaml:"enableCircularRedirects,omitempty" json:"enableCircularRedirects,omitempty"`
	UseTrustStore           bool `yaml:"useTrustStore,omitempty" json:"useTrustStore,omitempty"`
}

// Limits of Connection (the global ones are meta.ValidateHTTP's).
const (
	MaxConnectionTimeout = 3600
	MaxConnectionRetries = 10
	maxUserAgentSuffix   = 100 // meta.MaxUserAgentSuffixLen
)

// NegativeCacheOn reports whether a proxy records misses: enabled (unset is
// enabled) and a TTL that is not Zero.
func (r Repository) NegativeCacheOn() bool {
	return (r.NegativeCacheEnabled == nil || *r.NegativeCacheEnabled) && !r.NegativeCacheTTL.Off()
}

// CleanConnection returns c, or nil when it sets nothing.
func CleanConnection(c *Connection) *Connection {
	if c == nil || *c == (Connection{}) {
		return nil
	}
	out := *c
	if c.Retries != nil {
		v := *c.Retries
		out.Retries = &v
	}
	return &out
}

func validateConnection(r Repository) error {
	if r.NegativeCacheEnabled != nil && r.Type != "proxy" {
		return fmt.Errorf("repository %q: negativeCacheEnabled is only valid for proxy repositories", r.Name)
	}
	c := r.Connection
	if c == nil {
		return nil
	}
	switch {
	case r.Type != "proxy":
		return fmt.Errorf("repository %q: connection is only valid for proxy repositories", r.Name)
	case c.Timeout < 0 || c.Timeout > MaxConnectionTimeout:
		return fmt.Errorf("repository %q: connection.timeout must be between 1 and %d seconds (0 or absent: the global setting)", r.Name, MaxConnectionTimeout)
	case c.Retries != nil && (*c.Retries < 0 || *c.Retries > MaxConnectionRetries):
		return fmt.Errorf("repository %q: connection.retries must be between 0 and %d", r.Name, MaxConnectionRetries)
	case len(c.UserAgentSuffix) > maxUserAgentSuffix:
		return fmt.Errorf("repository %q: connection.userAgentSuffix must be at most %d characters", r.Name, maxUserAgentSuffix)
	case strings.ContainsFunc(c.UserAgentSuffix, func(r rune) bool { return r < 0x20 || r > 0x7e }):
		return fmt.Errorf("repository %q: connection.userAgentSuffix must be printable ASCII", r.Name)
	}
	return nil
}
