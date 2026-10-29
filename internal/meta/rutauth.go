package meta

import (
	"fmt"
	"net/netip"
	"strings"
)

// headerChars are the characters of an HTTP field name (RFC 9110 token).
const headerChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#$%&'*+-.^_`|~"

// SettingRUT is the settings key of the Remote User Token realm, RealmRUT
// its realm id: the user named by a trusted reverse proxy's header.
const (
	SettingRUT = "rutauth"
	RealmRUT   = "rutauth"
)

// RUT is the Remote User Token realm (Nexus "Rut Auth"): a reverse proxy or
// SSO gateway in front of Command Center authenticates the user and names it
// in Header; a request that carries the header, comes from one of
// TrustedProxies (its RemoteAddr, never a forwarded address) and has no
// Authorization of its own is that user. Roles come from GroupHeader
// (comma-separated groups) through GroupMappings, plus DefaultRoles, plus
// an override row (source rutauth) like LDAP and OIDC.
type RUT struct {
	Header         string         `json:"header"`
	GroupHeader    string         `json:"groupHeader"`
	TrustedProxies []string       `json:"trustedProxies"` // CIDRs
	DefaultRoles   []string       `json:"defaultRoles"`
	GroupMappings  []GroupMapping `json:"groupMappings"`
}

// DefaultRUT is the configuration of an install that never set one.
func DefaultRUT() RUT { return RUT{Header: "REMOTE_USER"} }

// Copy returns c with its own slices.
func (c RUT) Copy() RUT {
	c.TrustedProxies = append([]string{}, c.TrustedProxies...)
	c.DefaultRoles = append([]string{}, c.DefaultRoles...)
	maps := make([]GroupMapping, len(c.GroupMappings))
	for i, m := range c.GroupMappings {
		maps[i] = GroupMapping{Group: m.Group, Roles: append([]string{}, m.Roles...)}
	}
	c.GroupMappings = maps
	return c
}

// Complete reports whether c can be enabled: a header and a trusted proxy.
func (c RUT) Complete() bool { return strings.TrimSpace(c.Header) != "" && len(c.TrustedProxies) > 0 }

// Mapping returns the group-to-role mapping in the LDAP form (ldap.MapRoles).
func (c RUT) Mapping() LDAP {
	return LDAP{GroupMappings: c.GroupMappings, DefaultRoles: c.DefaultRoles}
}

// Trusts reports whether addr (a RemoteAddr, host:port or host) is within
// one of the trusted proxy ranges.
func (c RUT) Trusts(addr string) bool {
	ip, err := netip.ParseAddrPort(addr)
	var a netip.Addr
	if err == nil {
		a = ip.Addr()
	} else if a, err = netip.ParseAddr(addr); err != nil {
		return false
	}
	a = a.Unmap()
	for _, s := range c.TrustedProxies {
		if p, err := netip.ParsePrefix(s); err == nil && p.Contains(a) {
			return true
		}
	}
	return false
}

// ValidateRUT checks c: header names that are valid HTTP field names and
// trusted proxies that are CIDRs. Role ids are checked by the API.
func ValidateRUT(c RUT) error {
	for _, f := range []struct{ name, v string }{{"header", c.Header}, {"groupHeader", c.GroupHeader}} {
		if f.v != "" && strings.Trim(f.v, headerChars) != "" {
			return fmt.Errorf("rutauth %s must be an HTTP header name (e.g. REMOTE_USER, X-Forwarded-User)", f.name)
		}
	}
	for _, s := range c.TrustedProxies {
		if _, err := netip.ParsePrefix(s); err != nil {
			return fmt.Errorf("rutauth trustedProxies: %q is not a CIDR (e.g. 10.0.0.0/8)", s)
		}
	}
	for i, m := range c.GroupMappings {
		if strings.TrimSpace(m.Group) == "" {
			return fmt.Errorf("group mapping %d: group is required", i+1)
		}
		if len(m.Roles) == 0 {
			return fmt.Errorf("group mapping %d: at least one role is required", i+1)
		}
	}
	return nil
}
