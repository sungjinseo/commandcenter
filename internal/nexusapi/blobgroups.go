package nexusapi

import (
	"encoding/json"
	"net/http"
	"net/url"

	"commandcenter/internal/meta"
)

// Blob store groups in Nexus's shape: {"name","members":[names],
// "fillPolicy":"roundRobin"|"writeToFirst","softQuota":null}. Writes are the
// UI's /-/api/blobstores calls; an update keeps the read-only flag of the
// members it keeps (the Nexus shape has none).

func (h *Handler) blobGroupRoutes(m *http.ServeMux, p string) {
	m.HandleFunc("GET "+p+"/blobstores/group", h.listBlobGroups)
	m.HandleFunc("POST "+p+"/blobstores/group", h.createBlobGroup)
	m.HandleFunc("GET "+p+"/blobstores/group/{name}", h.getBlobGroup)
	m.HandleFunc("PUT "+p+"/blobstores/group/{name}", h.updateBlobGroup)
}

// uiGroup is the part of the UI's blob-store view a group answer needs.
type uiGroup struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Members []struct {
		Name     string `json:"name"`
		ReadOnly bool   `json:"readOnly"`
	} `json:"members"`
	FillPolicy string `json:"fillPolicy"`
}

func (g uiGroup) nexus() map[string]any {
	names := []string{}
	for _, m := range g.Members {
		names = append(names, m.Name)
	}
	return map[string]any{"name": g.Name, "members": names, "fillPolicy": g.FillPolicy, "softQuota": nil}
}

// nexusGroup is the body of POST and PUT.
type nexusGroup struct {
	Name       string   `json:"name"`
	Members    []string `json:"members"`
	FillPolicy string   `json:"fillPolicy"`
}

func (h *Handler) listBlobGroups(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/blobstores", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var res struct {
		Stores []uiGroup `json:"blobstores"`
	}
	json.Unmarshal(body, &res)
	out := []map[string]any{}
	for _, g := range res.Stores {
		if g.Type == meta.BlobStoreGroup {
			out = append(out, g.nexus())
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// group fetches the named group from the UI's API; it answers 404 itself for
// a missing store or one that is not a group.
func (h *Handler) group(w http.ResponseWriter, r *http.Request, name string) (uiGroup, bool) {
	status, body := h.call(r, http.MethodGet, "/-/api/blobstores/"+url.PathEscape(name), nil)
	var g uiGroup
	json.Unmarshal(body, &g)
	switch {
	case status == http.StatusOK && g.Type == meta.BlobStoreGroup:
		return g, true
	case status == http.StatusOK || status == http.StatusNotFound:
		writeMessage(w, http.StatusNotFound, "blob store group not found")
	default:
		writeUIError(w, status, body)
	}
	return uiGroup{}, false
}

func (h *Handler) getBlobGroup(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	if g, ok := h.group(w, r, r.PathValue("name")); ok {
		writeJSON(w, http.StatusOK, g.nexus())
	}
}

func (h *Handler) createBlobGroup(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var in nexusGroup
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeMessage(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	members := []map[string]any{}
	for _, m := range in.Members {
		members = append(members, map[string]any{"name": m})
	}
	status, body := h.call(r, http.MethodPost, "/-/api/blobstores",
		map[string]any{"name": in.Name, "type": meta.BlobStoreGroup, "members": members, "fillPolicy": in.FillPolicy})
	if status != http.StatusCreated {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) updateBlobGroup(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var in nexusGroup
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeMessage(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	name := r.PathValue("name")
	cur, ok := h.group(w, r, name)
	if !ok {
		return
	}
	readOnly := map[string]bool{}
	for _, m := range cur.Members {
		readOnly[m.Name] = m.ReadOnly
	}
	members := []map[string]any{}
	for _, m := range in.Members {
		members = append(members, map[string]any{"name": m, "readOnly": readOnly[m]})
	}
	status, body := h.call(r, http.MethodPut, "/-/api/blobstores/"+url.PathEscape(name),
		map[string]any{"members": members, "fillPolicy": in.FillPolicy})
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
