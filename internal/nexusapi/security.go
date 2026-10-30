package nexusapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"commandcenter/internal/ui"
)

// Routing rules, content selectors and content-selector privileges of the
// Nexus security API, as in-process calls of the UI's JSON API.

func (h *Handler) securityRoutes(m *http.ServeMux, p string) {
	m.HandleFunc("GET "+p+"/routing-rules", h.listRoutingRules)
	m.HandleFunc("POST "+p+"/routing-rules", h.createRoutingRule)
	m.HandleFunc("GET "+p+"/routing-rules/{name}", h.getRoutingRule)
	m.HandleFunc("PUT "+p+"/routing-rules/{name}", h.updateRoutingRule)
	m.HandleFunc("DELETE "+p+"/routing-rules/{name}", h.deleteRoutingRule)
	m.HandleFunc("GET "+p+"/security/content-selectors", h.listSelectors)
	m.HandleFunc("POST "+p+"/security/content-selectors", h.createSelector)
	m.HandleFunc("GET "+p+"/security/content-selectors/{name}", h.getSelector)
	m.HandleFunc("PUT "+p+"/security/content-selectors/{name}", h.updateSelector)
	m.HandleFunc("DELETE "+p+"/security/content-selectors/{name}", h.deleteSelector)
	m.HandleFunc("POST "+p+"/security/privileges/repository-content-selector", h.createSelectorPrivilege)
	m.HandleFunc("PUT "+p+"/security/privileges/repository-content-selector/{name}", h.updateSelectorPrivilege)
}

// decodeSmall reads a small JSON body into v.
func decodeSmall(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		writeMessage(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// ---- routing rules: {name, description, mode, matchers} ----

type routingRuleBody struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Mode        string   `json:"mode"`
	Matchers    []string `json:"matchers"`
}

func ruleOut(r routingRuleBody) map[string]any {
	m := r.Matchers
	if m == nil {
		m = []string{}
	}
	return map[string]any{"name": r.Name, "description": r.Description, "mode": r.Mode, "matchers": m}
}

func (h *Handler) listRoutingRules(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/routing-rules", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var res struct {
		Rules []routingRuleBody `json:"routingRules"`
	}
	json.Unmarshal(body, &res)
	out := make([]map[string]any, 0, len(res.Rules))
	for _, x := range res.Rules {
		out = append(out, ruleOut(x))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getRoutingRule(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/routing-rules/"+url.PathEscape(r.PathValue("name")), nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var x routingRuleBody
	json.Unmarshal(body, &x)
	writeJSON(w, http.StatusOK, ruleOut(x))
}

func (h *Handler) createRoutingRule(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var b routingRuleBody
	if !decodeSmall(w, r, &b) {
		return
	}
	status, body := h.call(r, http.MethodPost, "/-/api/routing-rules", b)
	if status != http.StatusCreated {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent) // Nexus answers no body
}

func (h *Handler) updateRoutingRule(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var b routingRuleBody
	if !decodeSmall(w, r, &b) {
		return
	}
	b.Name = r.PathValue("name")
	status, body := h.call(r, http.MethodPut, "/-/api/routing-rules/"+url.PathEscape(b.Name), b)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteRoutingRule(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodDelete, "/-/api/routing-rules/"+url.PathEscape(r.PathValue("name")), nil)
	if status == http.StatusConflict {
		status = http.StatusBadRequest // still used by a repository: Nexus's 400
	}
	if status != http.StatusNoContent {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- content selectors: {name, description, expression} ----

type selectorBody struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Expression  string `json:"expression"`
}

func selectorOut(s selectorBody) map[string]any {
	return map[string]any{"name": s.Name, "description": s.Description, "expression": s.Expression}
}

func (h *Handler) listSelectors(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/content-selectors", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var res struct {
		Selectors []selectorBody `json:"contentSelectors"`
	}
	json.Unmarshal(body, &res)
	out := make([]map[string]any, 0, len(res.Selectors))
	for _, x := range res.Selectors {
		out = append(out, selectorOut(x))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getSelector(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/content-selectors/"+url.PathEscape(r.PathValue("name")), nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var x selectorBody
	json.Unmarshal(body, &x)
	writeJSON(w, http.StatusOK, selectorOut(x))
}

func (h *Handler) createSelector(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var b selectorBody
	if !decodeSmall(w, r, &b) {
		return
	}
	status, body := h.call(r, http.MethodPost, "/-/api/content-selectors", b)
	if status != http.StatusCreated {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent) // Nexus answers 204 here
}

func (h *Handler) updateSelector(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var b struct {
		Description string `json:"description"`
		Expression  string `json:"expression"`
	}
	if !decodeSmall(w, r, &b) {
		return
	}
	name := r.PathValue("name")
	status, body := h.call(r, http.MethodPut, "/-/api/content-selectors/"+url.PathEscape(name),
		selectorBody{Name: name, Description: b.Description, Expression: b.Expression})
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteSelector(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodDelete, "/-/api/content-selectors/"+url.PathEscape(r.PathValue("name")), nil)
	if status != http.StatusNoContent {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- repository-content-selector privileges ----

// selectorPrivilege is Nexus's body: a privilege is a standalone object in
// Nexus, while privileges live in roles here. It is mapped onto the role
// "priv:<name>" holding that single privilege, so `security/roles`
// assignment works; format is implied by the selector's own expression and
// not stored.
type selectorPrivilege struct {
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Actions         []string `json:"actions"` // BROWSE, READ, EDIT, ADD, DELETE (or lowercase)
	Format          string   `json:"format"`
	Repository      string   `json:"repository"`
	ContentSelector string   `json:"contentSelector"`
}

func (p selectorPrivilege) role() map[string]any {
	actions := make([]string, 0, len(p.Actions))
	for _, a := range p.Actions {
		actions = append(actions, strings.ToLower(a))
	}
	repos := []string{"*"}
	if p.Repository != "" && p.Repository != "*" {
		repos = []string{p.Repository}
	}
	return map[string]any{"id": ui.NexusPrivRolePrefix + p.Name, "description": p.Description,
		"privileges": []map[string]any{{"target": "selector:" + p.ContentSelector, "actions": actions, "repos": repos}}}
}

func (h *Handler) createSelectorPrivilege(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var p selectorPrivilege
	if !decodeSmall(w, r, &p) {
		return
	}
	status, body := h.call(r, http.MethodPost, "/-/api/roles", p.role())
	if status != http.StatusCreated {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) updateSelectorPrivilege(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var p selectorPrivilege
	if !decodeSmall(w, r, &p) {
		return
	}
	p.Name = r.PathValue("name")
	body := p.role()
	delete(body, "id")
	status, res := h.call(r, http.MethodPut, "/-/api/roles/"+url.PathEscape(ui.NexusPrivRolePrefix+p.Name), body)
	if status != http.StatusOK {
		writeUIError(w, status, res)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
