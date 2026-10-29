package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode"

	"commandcenter/internal/config"
)

// General settings (plan 12 Decision 5) and realms (Decision 3). Each lives
// under its own settings key as a JSON value; a missing key means the default.
const (
	SettingBaseURL            = "baseURL"
	SettingAnonymousRead      = "anonymousRead"
	SettingAuthenticatedRead  = "authenticatedRead"
	SettingMaxUploadMiB       = "maxUploadMiB"
	SettingSessionIdleMinutes = "sessionIdleMinutes"
	SettingDefaultLanguage    = "defaultLanguage"
	SettingRealms             = "realms"
	SettingSMTP               = "smtp"
)

// Realm ids (Decision 3).
const (
	RealmLocal = "local" // password login, UI and Basic auth
	RealmToken = "token" // API tokens
	RealmLDAP  = "ldap"  // password login against LDAP (plan 12 Task 6)
	RealmOIDC  = "oidc"  // browser login through an OpenID Connect provider
)

var knownRealms = map[string]bool{RealmLocal: true, RealmToken: true, RealmLDAP: true, RealmOIDC: true}

// Realm is one entry of the ordered realm list.
type Realm struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

// ValidateRealms checks a realm list: known ids only, each at most once.
func ValidateRealms(realms []Realm) error {
	seen := map[string]bool{}
	for _, r := range realms {
		if !knownRealms[r.ID] {
			return fmt.Errorf("unknown realm %q", r.ID)
		}
		if seen[r.ID] {
			return fmt.Errorf("realm %q is listed twice", r.ID)
		}
		seen[r.ID] = true
	}
	return nil
}

// SMTP security modes.
const (
	SMTPNone     = "none"
	SMTPStartTLS = "starttls"
	SMTPTLS      = "tls"
)

// SMTP is the outgoing mail server (plan 12 Decision 6). Password is a
// secret: it is stored but never returned by the API or written to a log.
type SMTP struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
	Enabled  bool   `json:"enabled"`
}

// ValidateSMTP checks c. A disabled configuration may be partial; an enabled
// one needs a host, a port, a known security mode and a valid From address.
// A host with a space or a control character (CR, LF) is refused either way
// (QA N-17).
func ValidateSMTP(c SMTP) error {
	if strings.ContainsFunc(c.Host, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("smtp host must not contain spaces or control characters")
	}
	if !c.Enabled {
		return nil
	}
	switch {
	case strings.TrimSpace(c.Host) == "":
		return fmt.Errorf("smtp host is required")
	case c.Port < 1 || c.Port > 65535:
		return fmt.Errorf("smtp port must be between 1 and 65535")
	case c.Security != SMTPNone && c.Security != SMTPStartTLS && c.Security != SMTPTLS:
		return fmt.Errorf(`smtp security must be "none", "starttls" or "tls"`)
	case strings.TrimSpace(c.From) == "":
		return fmt.Errorf("smtp from address is required")
	}
	if _, err := mail.ParseAddress(c.From); err != nil {
		return fmt.Errorf("smtp from address is not a valid e-mail address")
	}
	return nil
}

// Settings are the typed general settings.
type Settings struct {
	BaseURL            string
	AnonymousRead      bool
	AuthenticatedRead  bool
	MaxUploadMiB       int
	SessionIdleMinutes int
	DefaultLanguage    string
	Realms             []Realm
	SMTP               SMTP
	LDAP               LDAP
	OIDC               OIDC
	HTTP               HTTP
	MetricsAnonymous   bool
	TrustedCerts       []string // PEM, one certificate each (the SSL trust store)
}

// DefaultSettings returns the values used for missing keys: today's behaviour.
func DefaultSettings() Settings {
	return Settings{
		BaseURL:            "http://localhost:8081",
		AuthenticatedRead:  true,
		MaxUploadMiB:       1024,
		SessionIdleMinutes: 720,
		DefaultLanguage:    "ko",
		Realms:             []Realm{{ID: RealmLocal, Enabled: true}, {ID: RealmToken, Enabled: true}},
		LDAP:               DefaultLDAP(),
		OIDC:               DefaultOIDC(),
		HTTP:               DefaultHTTP(),
		TrustedCerts:       []string{},
	}
}

// SettingsFromConfig maps the config file's settings for the one-time import.
func SettingsFromConfig(cfg *config.Config) Settings {
	s := DefaultSettings()
	if cfg.BaseURL != "" {
		s.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	}
	if cfg.MaxUploadMiB > 0 {
		s.MaxUploadMiB = cfg.MaxUploadMiB
	}
	s.AnonymousRead = cfg.AnonymousRead
	return s
}

// GetSettingJSON decodes the JSON value of key into v. It reports false (and
// leaves v alone) when the key is unset. A secret in v is opened.
func (d *DB) GetSettingJSON(ctx context.Context, key string, v any) (bool, error) {
	found, err := getSettingJSON(ctx, d.sql, key, v)
	if found && err == nil {
		err = d.box.openValue(key, v)
	}
	return found, err
}

func getSettingJSON(ctx context.Context, q querier, key string, v any) (bool, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return false, fmt.Errorf("setting %q: %w", key, err)
	}
	return true, nil
}

// SetSettingJSON stores v as the JSON value of key. The realms key is
// validated (ValidateRealms). A secret in v (SMTP, LDAP, OIDC) is sealed.
func (d *DB) SetSettingJSON(ctx context.Context, key string, v any) error {
	v, err := d.box.sealValue(v)
	if err != nil {
		return err
	}
	return setSettingJSON(ctx, d.sql, key, v)
}

// SetSettingJSON stores v as the JSON value of key within the transaction.
func (t *Tx) SetSettingJSON(ctx context.Context, key string, v any) error {
	v, err := t.box.sealValue(v)
	if err != nil {
		return err
	}
	return setSettingJSON(ctx, t.q, key, v)
}

func setSettingJSON(ctx context.Context, q querier, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := validateSettingValue(key, b); err != nil {
		return err
	}
	if key == SettingRealms {
		var realms []Realm
		if err := json.Unmarshal(b, &realms); err != nil {
			return fmt.Errorf("realms: %w", err)
		}
		if err := ValidateRealms(realms); err != nil {
			return err
		}
	}
	_, err = q.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, string(b))
	return err
}

// LoadSettings reads every general setting, using the default for unset
// keys. A stored value that does not decode, an invalid realm list or a
// secret that does not open is an error.
func (d *DB) LoadSettings(ctx context.Context) (Settings, error) {
	s, err := loadSettings(ctx, d.sql)
	if err == nil {
		err = d.box.openSettings(&s)
	}
	return s, err
}

// loadSettings leaves the secrets as stored (sealed or not): the realm
// recovery needs no key.
func loadSettings(ctx context.Context, q querier) (Settings, error) {
	s := DefaultSettings()
	for _, f := range []struct {
		key string
		v   any
	}{
		{SettingBaseURL, &s.BaseURL},
		{SettingAnonymousRead, &s.AnonymousRead},
		{SettingAuthenticatedRead, &s.AuthenticatedRead},
		{SettingMaxUploadMiB, &s.MaxUploadMiB},
		{SettingSessionIdleMinutes, &s.SessionIdleMinutes},
		{SettingDefaultLanguage, &s.DefaultLanguage},
		{SettingRealms, &s.Realms},
		{SettingSMTP, &s.SMTP},
		{SettingLDAP, &s.LDAP},
		{SettingOIDC, &s.OIDC},
		{SettingHTTP, &s.HTTP},
		{SettingMetricsAnonymous, &s.MetricsAnonymous},
		{SettingTrustedCertificates, &s.TrustedCerts},
	} {
		if _, err := getSettingJSON(ctx, q, f.key, f.v); err != nil {
			return Settings{}, err
		}
	}
	if s.Realms == nil {
		s.Realms = []Realm{}
	}
	if s.TrustedCerts == nil {
		s.TrustedCerts = []string{}
	}
	if s.HTTP.Proxy.NonProxyHosts == nil {
		s.HTTP.Proxy.NonProxyHosts = []string{}
	}
	if err := ValidateRealms(s.Realms); err != nil {
		return Settings{}, fmt.Errorf("setting %q: %w", SettingRealms, err)
	}
	return s, nil
}

// SaveSettings stores every general setting in one transaction.
func (d *DB) SaveSettings(ctx context.Context, s Settings) error {
	var err error
	if s.SMTP.Password, err = d.box.seal(s.SMTP.Password); err != nil {
		return err
	}
	if s.LDAP.BindPassword, err = d.box.seal(s.LDAP.BindPassword); err != nil {
		return err
	}
	if s.OIDC.ClientSecret, err = d.box.seal(s.OIDC.ClientSecret); err != nil {
		return err
	}
	if s.HTTP.Proxy.Password, err = d.box.seal(s.HTTP.Proxy.Password); err != nil {
		return err
	}
	return d.Tx(ctx, func(tx *Tx) error { return saveSettings(ctx, tx.q, s) })
}

// saveSettings stores the secrets as given: callers seal them.
func saveSettings(ctx context.Context, q querier, s Settings) error {
	realms := s.Realms
	if realms == nil {
		realms = []Realm{}
	}
	for _, f := range []struct {
		key string
		v   any
	}{
		{SettingRealms, realms}, // validated first, so nothing is written for a bad list
		{SettingBaseURL, s.BaseURL},
		{SettingAnonymousRead, s.AnonymousRead},
		{SettingAuthenticatedRead, s.AuthenticatedRead},
		{SettingMaxUploadMiB, s.MaxUploadMiB},
		{SettingSessionIdleMinutes, s.SessionIdleMinutes},
		{SettingDefaultLanguage, s.DefaultLanguage},
		{SettingSMTP, s.SMTP},
		{SettingLDAP, s.LDAP},
		{SettingOIDC, s.OIDC},
		{SettingHTTP, s.HTTP},
		{SettingMetricsAnonymous, s.MetricsAnonymous},
		{SettingTrustedCertificates, s.TrustedCerts},
	} {
		if err := setSettingJSON(ctx, q, f.key, f.v); err != nil {
			return err
		}
	}
	return nil
}

// AccessData is everything the auth snapshot is built from.
type AccessData struct {
	Users     []User
	Roles     []Role
	Settings  Settings
	Selectors []ContentSelector // the content selectors "selector:" privileges name
}

// LoadAccess reads users, roles and settings in one read transaction, so a
// snapshot never mixes states from before and after a concurrent write.
func (d *DB) LoadAccess(ctx context.Context) (AccessData, error) {
	// A read transaction: one snapshot, and no write lock for a reload.
	tx, err := d.sql.begin(ctx, false)
	if err != nil {
		return AccessData{}, err
	}
	defer tx.Rollback()
	q := txConn{tx: tx, pg: d.sql.pg}
	var out AccessData
	if out.Users, err = listUsers(ctx, q); err != nil {
		return AccessData{}, fmt.Errorf("load users: %w", err)
	}
	if out.Roles, err = listRoles(ctx, q); err != nil {
		return AccessData{}, fmt.Errorf("load roles: %w", err)
	}
	if out.Selectors, err = listContentSelectors(ctx, q); err != nil {
		return AccessData{}, fmt.Errorf("load content selectors: %w", err)
	}
	if out.Settings, err = loadSettings(ctx, q); err != nil {
		return AccessData{}, fmt.Errorf("load settings: %w", err)
	}
	if err := d.box.openSettings(&out.Settings); err != nil {
		return AccessData{}, fmt.Errorf("load settings: %w", err)
	}
	return out, nil
}

// Limits of the general settings.
const (
	MinSessionIdleMinutes = 5
	MaxSessionIdleMinutes = 10080 // 7 days
	MaxMaxUploadMiB       = 1 << 20
)

// ValidateBaseURL checks a baseURL: the config file's rule
// (config.ValidateBaseURL), so a value the import accepts is accepted here.
func ValidateBaseURL(v string) error { return config.ValidateBaseURL(v) }

// ValidateSessionIdleMinutes checks the idle timeout.
func ValidateSessionIdleMinutes(v int) error {
	if v < MinSessionIdleMinutes || v > MaxSessionIdleMinutes {
		return fmt.Errorf("sessionIdleMinutes must be between %d and %d", MinSessionIdleMinutes, MaxSessionIdleMinutes)
	}
	return nil
}

// ValidateMaxUploadMiB checks the upload cap.
func ValidateMaxUploadMiB(v int) error {
	if v < 1 || v > MaxMaxUploadMiB {
		return fmt.Errorf("maxUploadMiB must be between 1 and %d", MaxMaxUploadMiB)
	}
	return nil
}

// ValidateDefaultLanguage accepts "ko" or "en".
func ValidateDefaultLanguage(v string) error {
	if v != "ko" && v != "en" {
		return fmt.Errorf(`defaultLanguage must be "ko" or "en"`)
	}
	return nil
}

// validateSettingValue checks the JSON value stored under a general key.
func validateSettingValue(key string, b []byte) error {
	switch key {
	case SettingBaseURL:
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		return ValidateBaseURL(v)
	case SettingSessionIdleMinutes:
		var v int
		if err := json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		return ValidateSessionIdleMinutes(v)
	case SettingMaxUploadMiB:
		var v int
		if err := json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		return ValidateMaxUploadMiB(v)
	case SettingDefaultLanguage:
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		return ValidateDefaultLanguage(v)
	case SettingSMTP:
		var v SMTP
		if err := json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		return ValidateSMTP(v)
	case SettingLDAP:
		v := DefaultLDAP()
		if err := json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		return ValidateLDAP(v)
	case SettingOIDC:
		v := DefaultOIDC()
		if err := json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		return ValidateOIDC(v)
	case SettingHTTP:
		v := DefaultHTTP()
		if err := json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		return ValidateHTTP(v)
	case SettingTrustedCertificates:
		var v []string
		if err := json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		return ValidateTrustedCertificates(v)
	}
	return nil
}
