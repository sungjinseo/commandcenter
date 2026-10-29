package ui

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"commandcenter/internal/meta"
)

// General settings (plan 12 Task 3). Reads need an admin; a change needs an
// admin who signed in with a password or session, plus the UI header. The
// values apply at once: the auth snapshot is reloaded, and baseURL,
// maxUploadMiB and the session idle time are read live per request.

func (u *UI) settingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/settings", u.getSettings)
	mux.HandleFunc("PUT /-/api/settings", u.putSettings)
}

type settingsDTO struct {
	BaseURL            string  `json:"baseURL"`
	AnonymousRead      bool    `json:"anonymousRead"`
	AuthenticatedRead  bool    `json:"authenticatedRead"`
	SessionIdleMinutes int     `json:"sessionIdleMinutes"`
	MaxUploadMiB       int     `json:"maxUploadMiB"`
	DefaultLanguage    string  `json:"defaultLanguage"`
	MetricsAnonymous   bool    `json:"metricsAnonymous"`
	HTTP               httpDTO `json:"http"`
	// TerraformDefaultRepository names the terraform repository the
	// host-root /.well-known/terraform.json points at; "" is the first
	// terraform repository by name.
	TerraformDefaultRepository string          `json:"terraformDefaultRepository"`
	UserTokens                 meta.UserTokens `json:"userTokens"`
	// DefaultRoles are granted to every signed-in user (Nexus DefaultRoleRealm).
	DefaultRoles []string `json:"defaultRoles"`
}

// httpDTO is the outbound HTTP configuration without the proxy password.
type httpDTO struct {
	UserAgentSuffix       string       `json:"userAgentSuffix"`
	ConnectTimeoutSeconds int          `json:"connectTimeoutSeconds"`
	TimeoutSeconds        int          `json:"timeoutSeconds"`
	Retries               int          `json:"retries"`
	Proxy                 httpProxyDTO `json:"proxy"`
}

type httpProxyDTO struct {
	Host          string   `json:"host"`
	Port          int      `json:"port"`
	Username      string   `json:"username"`
	PasswordSet   bool     `json:"passwordSet"`
	NonProxyHosts []string `json:"nonProxyHosts"`
}

// httpRequest is the http value of PUT: the whole configuration; an empty
// proxy password keeps the stored one, clearPassword blanks it.
type httpRequest struct {
	UserAgentSuffix       string `json:"userAgentSuffix"`
	ConnectTimeoutSeconds int    `json:"connectTimeoutSeconds"`
	TimeoutSeconds        int    `json:"timeoutSeconds"`
	Retries               int    `json:"retries"`
	Proxy                 struct {
		Host          string   `json:"host"`
		Port          int      `json:"port"`
		Username      string   `json:"username"`
		Password      string   `json:"password"`
		ClearPassword bool     `json:"clearPassword"`
		NonProxyHosts []string `json:"nonProxyHosts"`
	} `json:"proxy"`
}

func httpJSON(c meta.HTTP) httpDTO {
	hosts := c.Proxy.NonProxyHosts
	if hosts == nil {
		hosts = []string{}
	}
	return httpDTO{UserAgentSuffix: c.UserAgentSuffix, ConnectTimeoutSeconds: c.ConnectTimeoutSeconds,
		TimeoutSeconds: c.TimeoutSeconds, Retries: c.Retries,
		Proxy: httpProxyDTO{Host: c.Proxy.Host, Port: c.Proxy.Port, Username: c.Proxy.Username,
			PasswordSet: c.Proxy.Password != "", NonProxyHosts: hosts}}
}

// httpChanged names the keys that differ, never their values.
func httpChanged(a, b meta.HTTP) string {
	var keys []string
	add := func(changed bool, k string) {
		if changed {
			keys = append(keys, k)
		}
	}
	add(a.UserAgentSuffix != b.UserAgentSuffix, "userAgentSuffix")
	add(a.ConnectTimeoutSeconds != b.ConnectTimeoutSeconds, "connectTimeoutSeconds")
	add(a.TimeoutSeconds != b.TimeoutSeconds, "timeoutSeconds")
	add(a.Retries != b.Retries, "retries")
	add(a.Proxy.Host != b.Proxy.Host, "proxy.host")
	add(a.Proxy.Port != b.Proxy.Port, "proxy.port")
	add(a.Proxy.Username != b.Proxy.Username, "proxy.username")
	add(a.Proxy.Password != b.Proxy.Password, "proxy.password")
	add(strings.Join(a.Proxy.NonProxyHosts, ",") != strings.Join(b.Proxy.NonProxyHosts, ","), "proxy.nonProxyHosts")
	return strings.Join(keys, ", ")
}

func settingsJSON(s meta.Settings) settingsDTO {
	return settingsDTO{BaseURL: s.BaseURL, AnonymousRead: s.AnonymousRead, AuthenticatedRead: s.AuthenticatedRead,
		SessionIdleMinutes: s.SessionIdleMinutes, MaxUploadMiB: s.MaxUploadMiB, DefaultLanguage: s.DefaultLanguage,
		MetricsAnonymous: s.MetricsAnonymous, HTTP: httpJSON(s.HTTP), TerraformDefaultRepository: s.TerraformDefaultRepository,
		UserTokens: s.UserTokens, DefaultRoles: nonNil(s.DefaultRoles)}
}

// settingsRequest holds only the fields the caller wants to change.
type settingsRequest struct {
	BaseURL                    *string          `json:"baseURL"`
	AnonymousRead              *bool            `json:"anonymousRead"`
	AuthenticatedRead          *bool            `json:"authenticatedRead"`
	SessionIdleMinutes         *int             `json:"sessionIdleMinutes"`
	MaxUploadMiB               *int             `json:"maxUploadMiB"`
	DefaultLanguage            *string          `json:"defaultLanguage"`
	MetricsAnonymous           *bool            `json:"metricsAnonymous"`
	HTTP                       *httpRequest     `json:"http"`
	TerraformDefaultRepository *string          `json:"terraformDefaultRepository"`
	UserTokens                 *meta.UserTokens `json:"userTokens"`
	DefaultRoles               *[]string        `json:"defaultRoles"`
}

func (u *UI) getSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, settingsJSON(u.Auth.Snapshot().Settings()))
}

func (u *UI) putSettings(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body settingsRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if body.DefaultRoles != nil { // a role reference: serialised with role deletes
		u.accessMu.Lock()
		defer u.accessMu.Unlock()
	}
	// Start from the stored settings, not the snapshot, so a concurrent
	// change to other keys (realms) is not written back stale.
	s, err := u.DB.LoadSettings(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	var changed []string
	if u.settingsLoaded != nil {
		u.settingsLoaded()
	}
	var v validation
	check := func(key string, err error) {
		if err != nil {
			v.add("%s", err.Error())
		} else {
			changed = append(changed, key)
		}
	}
	if body.BaseURL != nil {
		b := strings.TrimRight(strings.TrimSpace(*body.BaseURL), "/")
		err := meta.ValidateBaseURL(b)
		if err == nil {
			s.BaseURL = b
		}
		check("baseURL", err)
	}
	if body.AnonymousRead != nil {
		s.AnonymousRead = *body.AnonymousRead
		changed = append(changed, "anonymousRead")
	}
	if body.AuthenticatedRead != nil {
		s.AuthenticatedRead = *body.AuthenticatedRead
		changed = append(changed, "authenticatedRead")
	}
	if body.SessionIdleMinutes != nil {
		err := meta.ValidateSessionIdleMinutes(*body.SessionIdleMinutes)
		if err == nil {
			s.SessionIdleMinutes = *body.SessionIdleMinutes
		}
		check("sessionIdleMinutes", err)
	}
	if body.MaxUploadMiB != nil {
		err := meta.ValidateMaxUploadMiB(*body.MaxUploadMiB)
		if err == nil {
			s.MaxUploadMiB = *body.MaxUploadMiB
		}
		check("maxUploadMiB", err)
	}
	if body.DefaultLanguage != nil {
		err := meta.ValidateDefaultLanguage(*body.DefaultLanguage)
		if err == nil {
			s.DefaultLanguage = *body.DefaultLanguage
		}
		check("defaultLanguage", err)
	}
	if body.MetricsAnonymous != nil {
		s.MetricsAnonymous = *body.MetricsAnonymous
		changed = append(changed, "metricsAnonymous")
	}
	var httpDetail string
	if h := body.HTTP; h != nil {
		next := meta.HTTP{UserAgentSuffix: strings.TrimSpace(h.UserAgentSuffix), ConnectTimeoutSeconds: h.ConnectTimeoutSeconds,
			TimeoutSeconds: h.TimeoutSeconds, Retries: h.Retries}
		next.Proxy = meta.HTTPProxy{Host: strings.TrimSpace(h.Proxy.Host), Port: h.Proxy.Port, Username: h.Proxy.Username,
			Password: s.HTTP.Proxy.Password, NonProxyHosts: []string{}}
		for _, x := range h.Proxy.NonProxyHosts {
			if x = strings.TrimSpace(x); x != "" {
				next.Proxy.NonProxyHosts = append(next.Proxy.NonProxyHosts, x)
			}
		}
		switch {
		case h.Proxy.ClearPassword, next.Proxy.Host == "":
			next.Proxy.Password = ""
		case h.Proxy.Password != "":
			next.Proxy.Password = h.Proxy.Password
		}
		err := meta.ValidateHTTP(next)
		if err == nil {
			httpDetail = httpChanged(s.HTTP, next)
			s.HTTP = next
		}
		check(meta.SettingHTTP, err)
	}
	if body.TerraformDefaultRepository != nil {
		name := strings.TrimSpace(*body.TerraformDefaultRepository)
		var err error
		if rp := u.Repos.Get(name); name != "" && (rp == nil || rp.Format != "terraform") {
			err = fmt.Errorf("terraformDefaultRepository: %q is not a terraform repository", name)
		}
		if err == nil {
			s.TerraformDefaultRepository = name
		}
		check(meta.SettingTerraformDefaultRepository, err)
	}
	var tokensDetail string
	if t := body.UserTokens; t != nil {
		err := meta.ValidateUserTokens(*t)
		if err == nil {
			tokensDetail = fmt.Sprintf("enabled %t, required %t, expirationDays %d", t.Enabled, t.Required, t.ExpirationDays)
			s.UserTokens = *t
		}
		check(meta.SettingUserTokens, err)
	}
	if body.DefaultRoles != nil {
		roles := append([]string{}, *body.DefaultRoles...)
		var rv validation
		if err := u.checkRoleRefs(r.Context(), &rv, roles); err != nil {
			u.fail(w, r, err)
			return
		}
		if contains(roles, meta.AdminRole) {
			rv.add("the admin role cannot be a default role")
		}
		for _, msg := range rv {
			v.add("defaultRoles: %s", msg)
		}
		if len(rv) == 0 {
			s.DefaultRoles = roles
			changed = append(changed, meta.SettingDefaultRoles)
		}
	}
	if err := v.err(); err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if len(changed) == 0 {
		writeError(w, http.StatusBadRequest, "no settings provided")
		return
	}
	// Write only the keys this request changed, in one transaction: the
	// realms, LDAP and SMTP keys have their own APIs (with lockout and
	// secret rules), and a stale read must never write them back.
	values := map[string]any{
		meta.SettingBaseURL: s.BaseURL, meta.SettingAnonymousRead: s.AnonymousRead,
		meta.SettingAuthenticatedRead: s.AuthenticatedRead, meta.SettingSessionIdleMinutes: s.SessionIdleMinutes,
		meta.SettingMaxUploadMiB: s.MaxUploadMiB, meta.SettingDefaultLanguage: s.DefaultLanguage,
		meta.SettingMetricsAnonymous: s.MetricsAnonymous, meta.SettingHTTP: s.HTTP,
		meta.SettingTerraformDefaultRepository: s.TerraformDefaultRepository,
		meta.SettingUserTokens:                 s.UserTokens,
		meta.SettingDefaultRoles:               s.DefaultRoles,
	}
	err = u.DB.Tx(r.Context(), func(tx *meta.Tx) error {
		for _, k := range changed {
			v, ok := values[k]
			if !ok {
				return fmt.Errorf("settings: no value for key %q", k)
			}
			if err := tx.SetSettingJSON(r.Context(), k, v); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		u.fail(w, r, err)
		return
	}
	if err := u.Auth.Reload(r.Context()); err != nil {
		u.fail(w, r, err)
		return
	}
	if body.HTTP != nil {
		u.emit(r, "settings.http", "", "", httpDetail)
	}
	if body.UserTokens != nil {
		u.emit(r, "settings.userTokens", "", "", tokensDetail)
	}
	if body.DefaultRoles != nil {
		u.emit(r, "settings.defaultRoles", "", "", strings.Join(s.DefaultRoles, ", "))
	}
	if rest := slices.DeleteFunc(slices.Clone(changed), func(k string) bool {
		return k == meta.SettingHTTP || k == meta.SettingUserTokens || k == meta.SettingDefaultRoles
	}); len(rest) > 0 {
		u.emit(r, "settings.update", "", "", strings.Join(rest, ", "))
	}
	writeJSON(w, http.StatusOK, settingsJSON(u.Auth.Snapshot().Settings()))
}
