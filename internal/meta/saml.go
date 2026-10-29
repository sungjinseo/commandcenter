package meta

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"unicode"
)

// SettingSAML is the settings key of the SAML 2.0 service provider
// configuration.
const SettingSAML = "saml"

// SAML is the SAML login configuration. The identity provider is described
// by its metadata, pasted (IdPMetadataXML) or fetched (IdPMetadataURL).
// SPPrivateKey is a secret: stored sealed, never returned by the API,
// logged or put in an event; String and LogValue redact it. Roles are
// mapped from the groups attribute as LDAP maps them from the group
// attribute.
type SAML struct {
	DisplayName                string         `json:"displayName"`
	IdPMetadataXML             string         `json:"idpMetadataXML"`
	IdPMetadataURL             string         `json:"idpMetadataURL"`
	EntityID                   string         `json:"entityId"`          // "" is <baseURL>/-/sso/saml/metadata
	UsernameAttribute          string         `json:"usernameAttribute"` // "NameID" is the subject's NameID
	FirstNameAttribute         string         `json:"firstNameAttribute"`
	LastNameAttribute          string         `json:"lastNameAttribute"`
	EmailAttribute             string         `json:"emailAttribute"`
	GroupsAttribute            string         `json:"groupsAttribute"`
	GroupMappings              []GroupMapping `json:"groupMappings"`
	DefaultRoles               []string       `json:"defaultRoles"`
	ValidateResponseSignature  bool           `json:"validateResponseSignature"`
	ValidateAssertionSignature bool           `json:"validateAssertionSignature"`
	AllowSHA1Signatures        bool           `json:"allowSHA1Signatures"`
	AllowIdPInitiated          bool           `json:"allowIdpInitiated"`
	ClockSkewSeconds           int            `json:"clockSkewSeconds"`
	SignAuthnRequests          bool           `json:"signAuthnRequests"`
	SPPrivateKey               string         `json:"spPrivateKey"`  // PEM (PKCS #8), sealed
	SPCertificate              string         `json:"spCertificate"` // PEM
}

// SAMLNameID is the UsernameAttribute that means the subject's NameID.
const SAMLNameID = "NameID"

// MaxSAMLClockSkewSeconds bounds ClockSkewSeconds.
const MaxSAMLClockSkewSeconds = 600

// DefaultSAML is the configuration of an install that never set one.
func DefaultSAML() SAML {
	return SAML{DisplayName: "SAML", UsernameAttribute: SAMLNameID, ValidateResponseSignature: true,
		ValidateAssertionSignature: true, ClockSkewSeconds: 120, GroupMappings: []GroupMapping{}, DefaultRoles: []string{}}
}

// Copy returns c with its own slices.
func (c SAML) Copy() SAML {
	maps := make([]GroupMapping, len(c.GroupMappings))
	for i, m := range c.GroupMappings {
		maps[i] = GroupMapping{Group: m.Group, Roles: append([]string{}, m.Roles...)}
	}
	c.GroupMappings = maps
	c.DefaultRoles = append([]string{}, c.DefaultRoles...)
	return c
}

// Complete reports whether c names the identity provider's metadata:
// enough to start a login.
func (c SAML) Complete() bool {
	return strings.TrimSpace(c.IdPMetadataXML) != "" || strings.TrimSpace(c.IdPMetadataURL) != ""
}

// Mapping returns the group-to-role mapping of c in the LDAP form, so
// ldap.MapRoles applies to SAML groups too.
func (c SAML) Mapping() LDAP {
	return LDAP{GroupMappings: c.GroupMappings, DefaultRoles: c.DefaultRoles}
}

type samlRedacted SAML

// String prints c with the private key redacted.
func (c SAML) String() string {
	r := samlRedacted(c)
	if r.SPPrivateKey != "" {
		r.SPPrivateKey = "***"
	}
	return fmt.Sprintf("%+v", r)
}

// LogValue logs c with the private key redacted.
func (c SAML) LogValue() slog.Value { return slog.StringValue(c.String()) }

// ValidateSAML checks c's shape; whether the metadata parses is the API's
// to check (it knows the SAML package), as are role ids.
func ValidateSAML(c SAML) error {
	for i, m := range c.GroupMappings {
		if strings.TrimSpace(m.Group) == "" {
			return fmt.Errorf("group mapping %d: group is required", i+1)
		}
		if len(m.Roles) == 0 {
			return fmt.Errorf("group mapping %d: at least one role is required", i+1)
		}
	}
	if !c.ValidateResponseSignature && !c.ValidateAssertionSignature {
		return fmt.Errorf("saml validateResponseSignature and validateAssertionSignature cannot both be off")
	}
	if c.ClockSkewSeconds < 0 || c.ClockSkewSeconds > MaxSAMLClockSkewSeconds {
		return fmt.Errorf("saml clockSkewSeconds must be between 0 and %d", MaxSAMLClockSkewSeconds)
	}
	for _, f := range []struct{ name, v string }{{"usernameAttribute", c.UsernameAttribute}, {"firstNameAttribute", c.FirstNameAttribute},
		{"lastNameAttribute", c.LastNameAttribute}, {"emailAttribute", c.EmailAttribute}, {"groupsAttribute", c.GroupsAttribute}} {
		if f.v != "" && !validClaim(f.v) {
			return fmt.Errorf("saml %s must be an attribute name without spaces or control characters", f.name)
		}
	}
	if c.UsernameAttribute == "" {
		return fmt.Errorf("saml usernameAttribute is required (%q for the NameID)", SAMLNameID)
	}
	if strings.ContainsFunc(c.DisplayName, unicode.IsControl) || len(c.DisplayName) > 64 {
		return fmt.Errorf("saml displayName must be at most 64 characters without control characters")
	}
	if c.EntityID != "" && (len(c.EntityID) > 1024 || !validClaim(c.EntityID) || !strings.Contains(c.EntityID, ":")) {
		return fmt.Errorf("saml entityId must be an absolute URI of at most 1024 characters")
	}
	if strings.TrimSpace(c.IdPMetadataXML) != "" && strings.TrimSpace(c.IdPMetadataURL) != "" {
		return fmt.Errorf("set saml idpMetadataXML or idpMetadataURL, not both")
	}
	if len(c.IdPMetadataXML) > 1<<20 {
		return fmt.Errorf("saml idpMetadataXML is larger than 1 MiB")
	}
	if c.IdPMetadataURL != "" {
		u, err := url.Parse(c.IdPMetadataURL)
		if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || !validClaim(c.IdPMetadataURL) {
			return fmt.Errorf("saml idpMetadataURL must be an absolute https URL")
		}
		switch h := u.Hostname(); {
		case u.Scheme == "https":
		case u.Scheme == "http" && (h == "127.0.0.1" || h == "localhost" || h == "::1"):
		default:
			return fmt.Errorf("saml idpMetadataURL must use https (http is allowed for localhost only)")
		}
	}
	if (c.SPPrivateKey == "") != (c.SPCertificate == "") {
		return fmt.Errorf("saml spPrivateKey and spCertificate go together")
	}
	return nil
}
