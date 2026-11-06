package nexusapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"commandcenter/internal/meta"
)

// Nexus's SAML settings (/security/saml, a Pro feature there) over
// /-/api/realms/saml: the identity provider's metadata, the attribute names
// and the signature checks. What only Command Center has (the group
// mappings and default roles, the display name, the clock skew, request
// signing, logins started at the identity provider) is kept by a PUT and
// set at /-/api/realms/saml. Both signature checks off is the UI's 400: a
// client that leaves them out of its body (they then default to true)
// or sends one of them true is accepted.

func (h *Handler) samlRoutes(m *http.ServeMux, p string) {
	m.HandleFunc("GET "+p+"/security/saml", h.getSAML)
	m.HandleFunc("PUT "+p+"/security/saml", h.putSAML)
	m.HandleFunc("DELETE "+p+"/security/saml", h.deleteSAML)
}

// nexusSAML is Nexus's body, request and answer.
type nexusSAML struct {
	EntityID                   string `json:"entityId"`
	IdPMetadata                string `json:"idpMetadata"`
	UsernameAttribute          string `json:"usernameAttribute"`
	FirstNameAttribute         string `json:"firstNameAttribute"`
	LastNameAttribute          string `json:"lastNameAttribute"`
	EmailAttribute             string `json:"emailAttribute"`
	GroupsAttribute            string `json:"groupsAttribute"`
	ValidateResponseSignature  *bool  `json:"validateResponseSignature"`
	ValidateAssertionSignature *bool  `json:"validateAssertionSignature"`
}

// uiSAML is the UI's view: the fields of its request, as its answer has them.
type uiSAML struct {
	DisplayName                string              `json:"displayName"`
	IdPMetadataXML             string              `json:"idpMetadataXML"`
	IdPMetadataURL             string              `json:"idpMetadataURL"`
	EntityID                   string              `json:"entityId"`
	UsernameAttribute          string              `json:"usernameAttribute"`
	FirstNameAttribute         string              `json:"firstNameAttribute"`
	LastNameAttribute          string              `json:"lastNameAttribute"`
	EmailAttribute             string              `json:"emailAttribute"`
	GroupsAttribute            string              `json:"groupsAttribute"`
	GroupMappings              []meta.GroupMapping `json:"groupMappings"`
	DefaultRoles               []string            `json:"defaultRoles"`
	ValidateResponseSignature  *bool               `json:"validateResponseSignature"`
	ValidateAssertionSignature *bool               `json:"validateAssertionSignature"`
	AllowSHA1Signatures        bool                `json:"allowSHA1Signatures"`
	AllowIdPInitiated          bool                `json:"allowIdpInitiated"`
	ClockSkewSeconds           *int                `json:"clockSkewSeconds"`
	SignAuthnRequests          bool                `json:"signAuthnRequests"`
}

// storedSAML reads the UI's settings; it answers the UI's error itself.
func (h *Handler) storedSAML(w http.ResponseWriter, r *http.Request) (uiSAML, bool) {
	var cur uiSAML
	status, body := h.call(r, http.MethodGet, "/-/api/realms/saml", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return cur, false
	}
	json.Unmarshal(body, &cur)
	return cur, true
}

// getSAML answers 404 while no identity provider is configured, as Nexus
// does without a configuration.
func (h *Handler) getSAML(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "settings", meta.ActionRead); !ok {
		return
	}
	cur, ok := h.storedSAML(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(cur.IdPMetadataXML) == "" && strings.TrimSpace(cur.IdPMetadataURL) == "" {
		writeMessage(w, http.StatusNotFound, "SAML is not configured")
		return
	}
	writeJSON(w, http.StatusOK, nexusSAML{EntityID: cur.EntityID, IdPMetadata: cur.IdPMetadataXML,
		UsernameAttribute: cur.UsernameAttribute, FirstNameAttribute: cur.FirstNameAttribute, LastNameAttribute: cur.LastNameAttribute,
		EmailAttribute: cur.EmailAttribute, GroupsAttribute: cur.GroupsAttribute,
		ValidateResponseSignature: cur.ValidateResponseSignature, ValidateAssertionSignature: cur.ValidateAssertionSignature})
}

func (h *Handler) putSAML(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var in nexusSAML
	if !decodeSmall(w, r, &in) {
		return
	}
	next, ok := h.storedSAML(w, r)
	if !ok {
		return
	}
	next.IdPMetadataXML, next.IdPMetadataURL, next.EntityID = in.IdPMetadata, "", in.EntityID
	next.UsernameAttribute, next.FirstNameAttribute, next.LastNameAttribute = in.UsernameAttribute, in.FirstNameAttribute, in.LastNameAttribute
	next.EmailAttribute, next.GroupsAttribute = in.EmailAttribute, in.GroupsAttribute
	next.ValidateResponseSignature, next.ValidateAssertionSignature = in.ValidateResponseSignature, in.ValidateAssertionSignature
	status, body := h.call(r, http.MethodPut, "/-/api/realms/saml", next)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// realmOff turns a realm off, when it is on, before its settings go: a realm
// without settings signs nobody in. The UI's check that an administrator can
// still sign in may refuse (409); it answers that itself.
func (h *Handler) realmOff(w http.ResponseWriter, r *http.Request, id string) bool {
	if status, body := h.call(r, http.MethodPut, "/-/api/realms", map[string]any{"off": id}); status != http.StatusOK {
		writeUIError(w, status, body)
		return false
	}
	return true
}

// deleteSAML resets the settings and turns the saml realm off.
func (h *Handler) deleteSAML(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	if !h.realmOff(w, r, meta.RealmSAML) {
		return
	}
	if status, body := h.call(r, http.MethodPut, "/-/api/realms/saml", uiSAML{}); status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
