package meta

import (
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

// SettingLDAP is the settings key of the LDAP configuration (plan 12 Task 6).
const SettingLDAP = "ldap"

// LDAP is the directory configuration. BindPassword is a secret: it is
// stored but never returned by the API, logged or put in an event; String
// and LogValue redact it.
type LDAP struct {
	URL                string         `json:"url"`
	StartTLS           bool           `json:"startTLS"`
	InsecureSkipVerify bool           `json:"insecureSkipVerify"`
	BindDN             string         `json:"bindDN"`
	BindPassword       string         `json:"bindPassword"`
	TimeoutSeconds     int            `json:"timeoutSeconds"`
	UserBaseDN         string         `json:"userBaseDN"`
	UserFilter         string         `json:"userFilter"`
	UserIDAttr         string         `json:"userIdAttr"`
	NameAttr           string         `json:"nameAttr"`
	EmailAttr          string         `json:"emailAttr"`
	GroupAttr          string         `json:"groupAttr"`
	GroupMappings      []GroupMapping `json:"groupMappings"`
	DefaultRoles       []string       `json:"defaultRoles"`
}

// GroupMapping grants Roles to members of Group, matched against the
// user's group attribute by full DN or by the first RDN's value
// (case-insensitive).
type GroupMapping struct {
	Group string   `json:"group"`
	Roles []string `json:"roles"`
}

// Limits and defaults of the LDAP configuration.
const (
	MinLDAPTimeoutSeconds = 1
	MaxLDAPTimeoutSeconds = 60
)

// DefaultLDAP is the configuration of an install that never set one.
func DefaultLDAP() LDAP {
	return LDAP{TimeoutSeconds: 5, UserFilter: "(uid={user})", UserIDAttr: "uid", NameAttr: "cn",
		EmailAttr: "mail", GroupAttr: "memberOf"}
}

// Copy returns c with its own slices.
func (c LDAP) Copy() LDAP {
	maps := make([]GroupMapping, len(c.GroupMappings))
	for i, m := range c.GroupMappings {
		maps[i] = GroupMapping{Group: m.Group, Roles: append([]string{}, m.Roles...)}
	}
	c.GroupMappings = maps
	c.DefaultRoles = append([]string{}, c.DefaultRoles...)
	return c
}

// Complete reports whether c names a server, a base DN and a filter with
// {user}: enough to look a user up.
func (c LDAP) Complete() bool {
	return strings.TrimSpace(c.URL) != "" && strings.TrimSpace(c.UserBaseDN) != "" && strings.Contains(c.UserFilter, "{user}")
}

// GrantsAdmin reports whether a group mapping or the default roles grant
// the admin role.
func (c LDAP) GrantsAdmin() bool {
	for _, r := range c.DefaultRoles {
		if r == AdminRole {
			return true
		}
	}
	for _, m := range c.GroupMappings {
		for _, r := range m.Roles {
			if r == AdminRole {
				return true
			}
		}
	}
	return false
}

// ldapRedacted has LDAP's fields but no methods, so printing it does not recurse.
type ldapRedacted LDAP

// String prints c with the bind password redacted.
func (c LDAP) String() string {
	r := ldapRedacted(c)
	if r.BindPassword != "" {
		r.BindPassword = "***"
	}
	return fmt.Sprintf("%+v", r)
}

// LogValue logs c with the bind password redacted.
func (c LDAP) LogValue() slog.Value { return slog.StringValue(c.String()) }

type smtpRedacted SMTP

// String prints c with the password redacted.
func (c SMTP) String() string {
	r := smtpRedacted(c)
	if r.Password != "" {
		r.Password = "***"
	}
	return fmt.Sprintf("%+v", r)
}

// LogValue logs c with the password redacted.
func (c SMTP) LogValue() slog.Value { return slog.StringValue(c.String()) }

// ValidateLDAP checks c. A configuration without a URL may be partial (the
// realm must then stay disabled: the API checks that); with a URL it must
// be usable. Role ids are checked by the API, which knows the roles.
func ValidateLDAP(c LDAP) error {
	for i, m := range c.GroupMappings {
		if strings.TrimSpace(m.Group) == "" {
			return fmt.Errorf("group mapping %d: group is required", i+1)
		}
		if len(m.Roles) == 0 {
			return fmt.Errorf("group mapping %d: at least one role is required", i+1)
		}
	}
	if c.TimeoutSeconds < MinLDAPTimeoutSeconds || c.TimeoutSeconds > MaxLDAPTimeoutSeconds {
		return fmt.Errorf("ldap timeoutSeconds must be between %d and %d", MinLDAPTimeoutSeconds, MaxLDAPTimeoutSeconds)
	}
	if strings.TrimSpace(c.URL) == "" {
		return nil
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || (u.Scheme != "ldap" && u.Scheme != "ldaps") || (u.Path != "" && u.Path != "/") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("ldap url must be ldap://host[:port] or ldaps://host[:port]")
	}
	switch {
	case c.StartTLS && u.Scheme != "ldap":
		return fmt.Errorf("ldap startTLS needs an ldap:// url")
	case strings.TrimSpace(c.UserBaseDN) == "":
		return fmt.Errorf("ldap userBaseDN is required")
	case !strings.Contains(c.UserFilter, "{user}"):
		return fmt.Errorf("ldap userFilter must contain {user}")
	case c.UserIDAttr == "" || c.NameAttr == "" || c.EmailAttr == "" || c.GroupAttr == "":
		return fmt.Errorf("ldap attribute names are required")
	case c.BindDN != "" && c.BindPassword == "":
		return fmt.Errorf("ldap bindPassword is required with a bindDN")
	}
	return nil
}

// userIDRE is the user id rule shared by stored and LDAP users.
var userIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*$`)

// ValidUserID reports whether id is a valid user id: 1-64 characters of
// letters, digits, '.', '_', '@' and '-', starting with a letter or digit,
// and not the reserved "create".
func ValidUserID(id string) bool {
	return id != "" && len(id) <= 64 && userIDRE.MatchString(id) && id != "create"
}

type settingsRedacted Settings

// LogValue logs s with every secret (SMTP and LDAP passwords) redacted;
// the nested String methods do the redaction.
func (s Settings) LogValue() slog.Value {
	return slog.StringValue(fmt.Sprintf("%+v", settingsRedacted(s)))
}
