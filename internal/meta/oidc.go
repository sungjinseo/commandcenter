package meta

import (
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"unicode"
)

// SettingOIDC is the settings key of the OpenID Connect configuration.
const SettingOIDC = "oidc"

// OIDC is the OpenID Connect login configuration. ClientSecret is a secret:
// it is stored (sealed with the secret key) but never returned by the API,
// logged or put in an event; String and LogValue redact it. Roles are
// mapped from the groups claim as LDAP maps them from the group attribute.
type OIDC struct {
	Issuer             string         `json:"issuer"`
	ClientID           string         `json:"clientId"`
	ClientSecret       string         `json:"clientSecret"`
	DisplayName        string         `json:"displayName"`
	Scopes             []string       `json:"scopes"`
	UsernameClaim      string         `json:"usernameClaim"`
	GroupsClaim        string         `json:"groupsClaim"`
	DefaultRoles       []string       `json:"defaultRoles"`
	GroupMappings      []GroupMapping `json:"groupMappings"`
	CACert             string         `json:"caCert"`
	InsecureSkipVerify bool           `json:"insecureSkipVerify"`
	TimeoutSeconds     int            `json:"timeoutSeconds"`
}

// DefaultOIDC is the configuration of an install that never set one.
func DefaultOIDC() OIDC {
	return OIDC{DisplayName: "SSO", Scopes: []string{"openid", "profile", "email", "groups"},
		UsernameClaim: "preferred_username", GroupsClaim: "groups", TimeoutSeconds: 10}
}

// Copy returns c with its own slices.
func (c OIDC) Copy() OIDC {
	maps := make([]GroupMapping, len(c.GroupMappings))
	for i, m := range c.GroupMappings {
		maps[i] = GroupMapping{Group: m.Group, Roles: append([]string{}, m.Roles...)}
	}
	c.GroupMappings = maps
	c.DefaultRoles = append([]string{}, c.DefaultRoles...)
	c.Scopes = append([]string{}, c.Scopes...)
	return c
}

// Complete reports whether c names an issuer and a client id: enough to
// start a login.
func (c OIDC) Complete() bool {
	return strings.TrimSpace(c.Issuer) != "" && strings.TrimSpace(c.ClientID) != ""
}

// GrantsAdmin reports whether a group mapping or the default roles grant
// the admin role.
func (c OIDC) GrantsAdmin() bool {
	return LDAP{GroupMappings: c.GroupMappings, DefaultRoles: c.DefaultRoles}.GrantsAdmin()
}

// Mapping returns the group-to-role mapping of c in the LDAP form, so
// ldap.MapRoles applies to OIDC groups too.
func (c OIDC) Mapping() LDAP {
	return LDAP{GroupMappings: c.GroupMappings, DefaultRoles: c.DefaultRoles}
}

type oidcRedacted OIDC

// String prints c with the client secret redacted.
func (c OIDC) String() string {
	r := oidcRedacted(c)
	if r.ClientSecret != "" {
		r.ClientSecret = "***"
	}
	return fmt.Sprintf("%+v", r)
}

// LogValue logs c with the client secret redacted.
func (c OIDC) LogValue() slog.Value { return slog.StringValue(c.String()) }

// ValidateOIDC checks c. A configuration without an issuer may be partial
// (the realm must then stay disabled: the API checks that); with one it
// must be usable. Role ids are checked by the API, which knows the roles.
func ValidateOIDC(c OIDC) error {
	for i, m := range c.GroupMappings {
		if strings.TrimSpace(m.Group) == "" {
			return fmt.Errorf("group mapping %d: group is required", i+1)
		}
		if len(m.Roles) == 0 {
			return fmt.Errorf("group mapping %d: at least one role is required", i+1)
		}
	}
	if c.TimeoutSeconds < MinLDAPTimeoutSeconds || c.TimeoutSeconds > MaxLDAPTimeoutSeconds {
		return fmt.Errorf("oidc timeoutSeconds must be between %d and %d", MinLDAPTimeoutSeconds, MaxLDAPTimeoutSeconds)
	}
	for _, f := range []struct{ name, v string }{{"usernameClaim", c.UsernameClaim}, {"groupsClaim", c.GroupsClaim}} {
		if !validClaim(f.v) {
			return fmt.Errorf("oidc %s must be a claim name without spaces or control characters", f.name)
		}
	}
	hasOpenID := false
	for _, s := range c.Scopes {
		if !validClaim(s) {
			return fmt.Errorf("oidc scopes must be names without spaces or control characters")
		}
		hasOpenID = hasOpenID || s == "openid"
	}
	if !hasOpenID {
		return fmt.Errorf(`oidc scopes must include "openid"`)
	}
	if strings.TrimSpace(c.CACert) != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(c.CACert)) {
		return fmt.Errorf("oidc caCert must be one or more PEM certificates")
	}
	if strings.ContainsFunc(c.ClientID, unicode.IsControl) || len(c.ClientID) > 512 {
		return fmt.Errorf("oidc clientId is not valid")
	}
	if strings.TrimSpace(c.Issuer) == "" {
		return nil
	}
	if err := ValidateIssuer(c.Issuer); err != nil {
		return err
	}
	return nil
}

// ValidateIssuer checks an issuer URL: absolute https (http only on the
// loopback names), without user info, query or fragment.
func ValidateIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(issuer, "?#") ||
		strings.ContainsFunc(issuer, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("oidc issuer must be an absolute https URL without user info, query or fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" && h != "::1" {
			return fmt.Errorf("oidc issuer must use https (http is allowed for localhost only)")
		}
	default:
		return fmt.Errorf("oidc issuer must be an absolute https URL without user info, query or fragment")
	}
	return nil
}

// validClaim accepts a non-empty claim or scope name of at most 256 bytes
// without spaces or control characters.
func validClaim(s string) bool {
	return s != "" && len(s) <= 256 && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}
