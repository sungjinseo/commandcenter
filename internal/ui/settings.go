package ui

import (
	"fmt"
	"net/http"
	"strings"
	"time"

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
	BaseURL            string `json:"baseURL"`
	AnonymousRead      bool   `json:"anonymousRead"`
	AuthenticatedRead  bool   `json:"authenticatedRead"`
	SessionIdleMinutes int    `json:"sessionIdleMinutes"`
	MaxUploadMiB       int    `json:"maxUploadMiB"`
	DefaultLanguage    string `json:"defaultLanguage"`
}

func settingsJSON(s meta.Settings) settingsDTO {
	return settingsDTO{BaseURL: s.BaseURL, AnonymousRead: s.AnonymousRead, AuthenticatedRead: s.AuthenticatedRead,
		SessionIdleMinutes: s.SessionIdleMinutes, MaxUploadMiB: s.MaxUploadMiB, DefaultLanguage: s.DefaultLanguage}
}

// settingsRequest holds only the fields the caller wants to change.
type settingsRequest struct {
	BaseURL            *string `json:"baseURL"`
	AnonymousRead      *bool   `json:"anonymousRead"`
	AuthenticatedRead  *bool   `json:"authenticatedRead"`
	SessionIdleMinutes *int    `json:"sessionIdleMinutes"`
	MaxUploadMiB       *int    `json:"maxUploadMiB"`
	DefaultLanguage    *string `json:"defaultLanguage"`
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
	if u.Auth.Sessions != nil {
		u.Auth.Sessions.SetIdle(time.Duration(s.SessionIdleMinutes) * time.Minute)
	}
	u.emit(r, "settings.update", "", "", strings.Join(changed, ", "))
	writeJSON(w, http.StatusOK, settingsJSON(u.Auth.Snapshot().Settings()))
}
