package nexusapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
)

// User tokens of the Nexus security API, as in-process calls of the UI's
// JSON API: the settings (protectContent is our "required"), the reset of
// every token, the list, and one user's token by user id and realm.

func (h *Handler) userTokenRoutes(m *http.ServeMux, p string) {
	m.HandleFunc("GET "+p+"/security/user-tokens", h.getUserTokenSettings)
	m.HandleFunc("PUT "+p+"/security/user-tokens", h.putUserTokenSettings)
	m.HandleFunc("POST "+p+"/security/user-tokens", h.putUserTokenSettings)
	m.HandleFunc("DELETE "+p+"/security/user-tokens", h.resetAllUserTokens)
	m.HandleFunc("GET "+p+"/security/user-tokens/tokens", h.listUserTokens)
	m.HandleFunc("GET "+p+"/security/users/{userId}/{realm}/user-token", h.getUserToken)
	m.HandleFunc("POST "+p+"/security/users/{userId}/{realm}/user-token", h.createUserToken)
	m.HandleFunc("DELETE "+p+"/security/users/{userId}/{realm}/user-token", h.deleteUserToken)
	m.HandleFunc("DELETE "+p+"/security/users/{userId}/{realm}/user-token-reset", h.deleteUserToken)
}

// nexusTokenSettings is Nexus's shape of the settings.
type nexusTokenSettings struct {
	Enabled           bool `json:"enabled"`
	ProtectContent    bool `json:"protectContent"`
	ExpirationEnabled bool `json:"expirationEnabled"`
	ExpirationDays    int  `json:"expirationDays"`
}

type ourTokenSettings struct {
	Enabled        bool `json:"enabled"`
	Required       bool `json:"required"`
	ExpirationDays int  `json:"expirationDays"`
}

func toNexusTokenSettings(c ourTokenSettings) nexusTokenSettings {
	days := c.ExpirationDays
	if days == 0 {
		days = 30 // shown while expiration is off, as Nexus's default
	}
	return nexusTokenSettings{Enabled: c.Enabled, ProtectContent: c.Required, ExpirationEnabled: c.ExpirationDays > 0, ExpirationDays: days}
}

func (h *Handler) getUserTokenSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/settings", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var s struct {
		UserTokens ourTokenSettings `json:"userTokens"`
	}
	json.Unmarshal(body, &s)
	writeJSON(w, http.StatusOK, toNexusTokenSettings(s.UserTokens))
}

func (h *Handler) putUserTokenSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var b nexusTokenSettings
	if !decodeSmall(w, r, &b) {
		return
	}
	next := ourTokenSettings{Enabled: b.Enabled, Required: b.ProtectContent}
	if b.ExpirationEnabled {
		if b.ExpirationDays < 1 {
			writeValidation(w, []string{"expirationDays: must be at least 1 when expirationEnabled is true"})
			return
		}
		next.ExpirationDays = b.ExpirationDays
	}
	status, body := h.call(r, http.MethodPut, "/-/api/settings", map[string]any{"userTokens": next})
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	writeJSON(w, http.StatusOK, toNexusTokenSettings(next))
}

func (h *Handler) resetAllUserTokens(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodDelete, "/-/api/usertokens?confirm=true", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// nexusRealms maps Nexus's realm ids (and ours) to owner key prefixes.
var nexusRealms = map[string]string{
	"NexusAuthenticatingRealm": "", "LdapRealm": "ldap:", "Crowd": "crowd:",
	"local": "", "ldap": "ldap:", "oidc": "oidc:", "crowd": "crowd:",
}

// realmOf names an owner key's realm the Nexus way where there is one.
func realmOf(source string) string {
	switch source {
	case "local":
		return "NexusAuthenticatingRealm"
	case "ldap":
		return "LdapRealm"
	case "crowd":
		return "Crowd"
	}
	return source
}

type uiUserToken struct {
	User     string `json:"user"`
	UserID   string `json:"userId"`
	Source   string `json:"source"`
	NameCode string `json:"nameCode"`
	PassCode string `json:"passCode,omitempty"`
	Created  string `json:"created"`
	Expires  string `json:"expires"`
	Expired  bool   `json:"expired"`
	LastUsed string `json:"lastUsed"`
}

func tokenOut(t uiUserToken) map[string]any {
	out := map[string]any{"userId": t.UserID, "realm": realmOf(t.Source), "nameCode": t.NameCode,
		"created": t.Created, "expiresAt": t.Expires, "expired": t.Expired, "lastUsed": t.LastUsed}
	if t.PassCode != "" {
		out["passCode"] = t.PassCode
	}
	return out
}

func (h *Handler) listUserTokens(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	q := r.URL.Query()
	skip, err1 := strconv.Atoi(q.Get("skip"))
	limit, err2 := strconv.Atoi(q.Get("limit"))
	if q.Get("skip") == "" {
		skip, err1 = 0, nil
	}
	if q.Get("limit") == "" {
		limit, err2 = 25, nil
	}
	if err1 != nil || err2 != nil || skip < 0 || limit < 1 || limit > 100 {
		writeMessage(w, http.StatusBadRequest, "skip must be 0 or more and limit between 1 and 100")
		return
	}
	prefix, realmOK := nexusRealms[q.Get("realm")]
	if q.Get("realm") != "" && !realmOK {
		writeMessage(w, http.StatusBadRequest, "unknown realm")
		return
	}
	nameCode := q.Get("nameCode")
	if nameCode == "" {
		nameCode = q.Get("namecode")
	}
	status, body := h.call(r, http.MethodGet, "/-/api/usertokens", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var res struct {
		Tokens []uiUserToken `json:"userTokens"`
	}
	json.Unmarshal(body, &res)
	out := []map[string]any{}
	for _, t := range res.Tokens {
		switch {
		case q.Get("realm") != "" && sourcePrefix(t.Source) != prefix,
			q.Get("userId") != "" && t.UserID != q.Get("userId"),
			nameCode != "" && t.NameCode != nameCode,
			t.Expired && q.Get("includeExpired") != "true":
			continue
		}
		if skip > 0 {
			skip--
			continue
		}
		if len(out) < limit {
			out = append(out, tokenOut(t))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func sourcePrefix(source string) string {
	if source == "local" {
		return ""
	}
	return source + ":"
}

// tokenOwner returns the owner key the path names, or answers 400.
func tokenOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	prefix, ok := nexusRealms[r.PathValue("realm")]
	if !ok {
		writeMessage(w, http.StatusBadRequest, "unknown realm")
		return "", false
	}
	return prefix + r.PathValue("userId"), true
}

func (h *Handler) getUserToken(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	owner, ok := tokenOwner(w, r)
	if !ok {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/usertokens", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var res struct {
		Tokens []uiUserToken `json:"userTokens"`
	}
	json.Unmarshal(body, &res)
	for _, t := range res.Tokens {
		if t.User == owner {
			writeJSON(w, http.StatusOK, tokenOut(t))
			return
		}
	}
	writeMessage(w, http.StatusNotFound, "user token not found")
}

func (h *Handler) createUserToken(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	owner, ok := tokenOwner(w, r)
	if !ok {
		return
	}
	status, body := h.call(r, http.MethodPost, "/-/api/usertokens/"+url.PathEscape(owner), nil)
	if status != http.StatusCreated {
		writeUIError(w, status, body)
		return
	}
	var t uiUserToken
	json.Unmarshal(body, &t)
	writeJSON(w, http.StatusOK, tokenOut(t))
}

func (h *Handler) deleteUserToken(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	owner, ok := tokenOwner(w, r)
	if !ok {
		return
	}
	status, body := h.call(r, http.MethodDelete, "/-/api/usertokens/"+url.PathEscape(owner), nil)
	if status != http.StatusNoContent {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
