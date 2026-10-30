package nexusapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"commandcenter/internal/meta"
)

// Routing rules and content selectors of the Nexus security API, as
// in-process calls of the UI's JSON API (users, roles, privileges, realms
// and anonymous access: nxsecurity.go).

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
	h.nxSecurityRoutes(m, p)
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
	m := []string{}
	for _, x := range r.Matchers {
		m = append(m, unanchor(x))
	}
	return map[string]any{"name": r.Name, "description": r.Description, "mode": r.Mode, "matchers": m}
}

// Nexus matches a routing rule's matcher against the whole path (with its
// leading "/"); Command Center finds it anywhere in the path, as rules
// stored before keep doing. So this API stores a matcher m as ^(?:m)$,
// which matches what m matches in Nexus, and returns it as m again.
func anchor(ms []string) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, "^(?:"+m+")$")
	}
	return out
}

// anchorable answers 400 for a matcher that is not a regular expression by
// itself (anchor would hide a stray ")" in its group).
func anchorable(w http.ResponseWriter, ms []string) bool {
	for i, m := range ms {
		if _, err := regexp.Compile(m); err != nil {
			writeValidation(w, []string{fmt.Sprintf("matcher %d: invalid regular expression: %v", i+1, err)})
			return false
		}
	}
	return true
}

// unanchor undoes anchor: ^(?:m)$ is m when m is a regular expression by
// itself (then the group spans all of it); anything else is returned as stored.
func unanchor(m string) string {
	inner, ok := strings.CutPrefix(m, "^(?:")
	if inner, ok2 := strings.CutSuffix(inner, ")$"); ok && ok2 {
		if _, err := regexp.Compile(inner); err == nil {
			return inner
		}
	}
	return m
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
	if !decodeSmall(w, r, &b) || !anchorable(w, b.Matchers) {
		return
	}
	b.Matchers = anchor(b.Matchers)
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
	if !decodeSmall(w, r, &b) || !anchorable(w, b.Matchers) {
		return
	}
	b.Name = r.PathValue("name")
	b.Matchers = anchor(b.Matchers)
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
	return map[string]any{"name": s.Name, "type": "csel", "description": s.Description, "expression": s.Expression}
}

func (h *Handler) listSelectors(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "selectors", meta.ActionRead); !ok {
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
	if _, ok := h.app(w, r, "selectors", meta.ActionRead); !ok {
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
	if _, ok := h.app(w, r, "selectors", meta.ActionAdd); !ok {
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
	if _, ok := h.app(w, r, "selectors", meta.ActionEdit); !ok {
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
	if _, ok := h.app(w, r, "selectors", meta.ActionDelete); !ok {
		return
	}
	status, body := h.call(r, http.MethodDelete, "/-/api/content-selectors/"+url.PathEscape(r.PathValue("name")), nil)
	if status != http.StatusNoContent {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
