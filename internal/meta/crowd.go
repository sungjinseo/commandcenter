package meta

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"unicode"
)

// SettingCrowd is the settings key of the Atlassian Crowd configuration.
const SettingCrowd = "crowd"

// Crowd is the Atlassian Crowd (Jira, Confluence user directory) login
// configuration: the server URL (Crowd's context path, e.g.
// https://crowd.example.com/crowd) and the application this server signs in
// to it as. ApplicationPassword is a secret: stored sealed, never returned
// by the API, logged or put in an event. Roles are mapped from the user's
// nested Crowd groups as LDAP maps them from the group attribute.
// CacheMinutes is how long a successful sign-in (keyed by user and password
// hash) and a user's groups are reused without asking Crowd.
type Crowd struct {
	URL                 string         `json:"url"`
	ApplicationName     string         `json:"applicationName"`
	ApplicationPassword string         `json:"applicationPassword"`
	TimeoutSeconds      int            `json:"timeoutSeconds"`
	CacheMinutes        int            `json:"cacheMinutes"`
	GroupMappings       []GroupMapping `json:"groupMappings"`
	DefaultRoles        []string       `json:"defaultRoles"`
}

// Limits of the Crowd configuration.
const (
	MinCrowdCacheMinutes = 1
	MaxCrowdCacheMinutes = 60
)

// DefaultCrowd is the configuration of an install that never set one.
func DefaultCrowd() Crowd { return Crowd{TimeoutSeconds: 10, CacheMinutes: 1} }

// Copy returns c with its own slices.
func (c Crowd) Copy() Crowd {
	m := c.Mapping().Copy()
	c.GroupMappings, c.DefaultRoles = m.GroupMappings, m.DefaultRoles
	return c
}

// Complete reports whether c names a server and an application.
func (c Crowd) Complete() bool {
	return strings.TrimSpace(c.URL) != "" && strings.TrimSpace(c.ApplicationName) != ""
}

// Mapping returns the group-to-role mapping of c in the LDAP form, so
// ldap.MapRoles applies to Crowd groups too.
func (c Crowd) Mapping() LDAP {
	return LDAP{GroupMappings: c.GroupMappings, DefaultRoles: c.DefaultRoles}
}

// GrantsAdmin reports whether a group mapping or the default roles grant admin.
func (c Crowd) GrantsAdmin() bool { return c.Mapping().GrantsAdmin() }

type crowdRedacted Crowd

// String prints c with the application password redacted.
func (c Crowd) String() string {
	r := crowdRedacted(c)
	if r.ApplicationPassword != "" {
		r.ApplicationPassword = "***"
	}
	return fmt.Sprintf("%+v", r)
}

// LogValue logs c with the application password redacted.
func (c Crowd) LogValue() slog.Value { return slog.StringValue(c.String()) }

// ValidateCrowd checks c. A configuration without a URL may be partial (the
// realm must then stay disabled: the API checks that). Role ids are checked
// by the API, which knows the roles.
func ValidateCrowd(c Crowd) error {
	for i, m := range c.GroupMappings {
		if strings.TrimSpace(m.Group) == "" {
			return fmt.Errorf("group mapping %d: group is required", i+1)
		}
		if len(m.Roles) == 0 {
			return fmt.Errorf("group mapping %d: at least one role is required", i+1)
		}
	}
	if c.TimeoutSeconds < MinLDAPTimeoutSeconds || c.TimeoutSeconds > MaxLDAPTimeoutSeconds {
		return fmt.Errorf("crowd timeoutSeconds must be between %d and %d", MinLDAPTimeoutSeconds, MaxLDAPTimeoutSeconds)
	}
	if c.CacheMinutes < MinCrowdCacheMinutes || c.CacheMinutes > MaxCrowdCacheMinutes {
		return fmt.Errorf("crowd cacheMinutes must be between %d and %d", MinCrowdCacheMinutes, MaxCrowdCacheMinutes)
	}
	if len(c.ApplicationName) > 255 || strings.ContainsFunc(c.ApplicationName, unicode.IsControl) || strings.Contains(c.ApplicationName, ":") {
		return fmt.Errorf("crowd applicationName must be at most 255 characters without control characters or ':'")
	}
	if strings.TrimSpace(c.URL) == "" {
		return nil
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" ||
		u.Fragment != "" || strings.ContainsAny(c.URL, "?#") || strings.ContainsFunc(c.URL, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("crowd url must be an absolute http or https URL without user info, query or fragment")
	}
	if strings.TrimSpace(c.ApplicationName) == "" {
		return fmt.Errorf("crowd applicationName is required with a url")
	}
	return nil
}
