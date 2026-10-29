package nexusapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Tags and staging (Nexus Pro's REST API): every call goes to the web UI's
// /-/api/tags and /-/api/staging, whose association and staging answers
// already have Nexus's shape.

func (h *Handler) stagingRoutes(m *http.ServeMux, p string) {
	m.HandleFunc("GET "+p+"/tags", h.listTags)
	m.HandleFunc("POST "+p+"/tags", h.tagCall(http.MethodPost, false))
	m.HandleFunc("GET "+p+"/tags/{name}", h.tagCall(http.MethodGet, true))
	m.HandleFunc("PUT "+p+"/tags/{name}", h.tagCall(http.MethodPut, true))
	m.HandleFunc("DELETE "+p+"/tags/{name}", h.tagCall(http.MethodDelete, true))
	m.HandleFunc("POST "+p+"/tags/associate/{name}", h.associate)
	m.HandleFunc("DELETE "+p+"/tags/associate/{name}", h.associate)
	m.HandleFunc("POST "+p+"/staging/move/{destination}", h.stagingMove)
	m.HandleFunc("POST "+p+"/staging/delete", h.stagingDelete)
}

func (c *comp) metaKey() meta.ComponentKey {
	return meta.ComponentKey{Repo: c.repo, Group: c.group, Name: c.name, Version: c.version}
}

// tagged returns the components associated with tag, and an entry
// {Repo: name} for each repository that has one; nil when tag is "".
func (h *Handler) tagged(ctx context.Context, tag string) (map[meta.ComponentKey]bool, error) {
	if tag == "" {
		return nil, nil
	}
	keys, err := h.DB.TagMembers(ctx, tag)
	if err != nil {
		return nil, err
	}
	out := map[meta.ComponentKey]bool{}
	for _, k := range keys {
		out[k], out[meta.ComponentKey{Repo: k.Repo}] = true, true
	}
	return out, nil
}

// Find returns the components of repos (groups skipped) that the search
// parameters q select, matched as the search API matches them (format and
// tag included), each with the paths of its matching files. The web UI's
// tag association and staging use it (ui.ComponentFinder), so they act on
// what a search shows.
func (h *Handler) Find(ctx context.Context, repos []*repo.Repository, q url.Values) ([]component.Component, error) {
	comps, err := h.find(ctx, repos, q)
	if err != nil {
		return nil, err
	}
	out := make([]component.Component, 0, len(comps))
	for _, c := range comps {
		cc := component.Component{Repo: c.repo, Format: c.format, Group: c.group, Name: c.name, Version: c.version}
		for _, p := range c.paths {
			cc.Assets = append(cc.Assets, component.Asset{Path: p})
		}
		out = append(out, cc)
	}
	return out, nil
}

// passThrough answers what the UI answered when it is Nexus-shaped (a body
// with "data", as the association and staging answers have), else the
// translated error.
func passThrough(w http.ResponseWriter, status int, body []byte) {
	var shaped struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &shaped) == nil && shaped.Data != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
		return
	}
	writeUIError(w, status, body)
}

func (h *Handler) signedIn(w http.ResponseWriter, r *http.Request) bool {
	if p := principal(r); p.Anonymous() {
		deny(w, p)
		return false
	}
	return true
}

// GET /v1/tags: {"items": [tag], "continuationToken"}.
func (h *Handler) listTags(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/tags?"+url.Values{"continuationToken": {r.URL.Query().Get("continuationToken")}}.Encode(), nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var res struct {
		Tags  []json.RawMessage `json:"tags"`
		Token string            `json:"continuationToken"`
	}
	json.Unmarshal(body, &res)
	if res.Tags == nil {
		res.Tags = []json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": res.Tags, "continuationToken": nullable(res.Token)})
}

// tagCall forwards a tag create, read, update or delete; a create answers
// 200 with the tag, a delete 204.
func (h *Handler) tagCall(method string, named bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.signedIn(w, r) {
			return
		}
		path := "/-/api/tags"
		if named {
			path += "/" + url.PathEscape(r.PathValue("name"))
		}
		var body any
		if method == http.MethodPost || method == http.MethodPut {
			b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
			if err != nil || !json.Valid(b) {
				writeMessage(w, http.StatusBadRequest, "invalid JSON body")
				return
			}
			body = json.RawMessage(b)
		}
		status, res := h.call(r, method, path, body)
		switch {
		case status == http.StatusNoContent && method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case status == http.StatusOK || status == http.StatusCreated:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(res)
		default:
			writeUIError(w, status, res)
		}
	}
}

// POST|DELETE /v1/tags/associate/{tagName}?<search parameters>.
func (h *Handler) associate(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	status, body := h.call(r, r.Method, "/-/api/tags/"+url.PathEscape(r.PathValue("name"))+"/associate?"+r.URL.RawQuery, nil)
	passThrough(w, status, body)
}

// POST /v1/staging/move/{destination}?repository=<source>&<search parameters>.
func (h *Handler) stagingMove(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	q := r.URL.Query()
	q.Set("destination", r.PathValue("destination"))
	status, body := h.call(r, http.MethodPost, "/-/api/staging/move?"+q.Encode(), nil)
	passThrough(w, status, body)
}

// POST /v1/staging/delete?repository=<name>&<search parameters>.
func (h *Handler) stagingDelete(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	status, body := h.call(r, http.MethodPost, "/-/api/staging/delete?"+r.URL.RawQuery, nil)
	passThrough(w, status, body)
}
