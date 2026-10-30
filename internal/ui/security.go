package ui

import (
	"errors"
	"net/http"
	"strings"

	"commandcenter/internal/auth"
	"commandcenter/internal/csel"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Routing rules and content selectors (Nexus security features). Reads are
// admin only; mutations need an interactive admin and the UI header.

func (u *UI) securityRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/routing-rules", u.listRoutingRules)
	mux.HandleFunc("POST /-/api/routing-rules", u.createRoutingRule)
	mux.HandleFunc("POST /-/api/routing-rules/test", u.testRoutingRule)
	mux.HandleFunc("GET /-/api/routing-rules/{name}", u.getRoutingRule)
	mux.HandleFunc("PUT /-/api/routing-rules/{name}", u.updateRoutingRule)
	mux.HandleFunc("DELETE /-/api/routing-rules/{name}", u.deleteRoutingRule)
	mux.HandleFunc("GET /-/api/content-selectors", u.listSelectors)
	mux.HandleFunc("POST /-/api/content-selectors", u.createSelector)
	mux.HandleFunc("POST /-/api/content-selectors/preview", u.previewSelector)
	mux.HandleFunc("GET /-/api/content-selectors/{name}", u.getSelector)
	mux.HandleFunc("PUT /-/api/content-selectors/{name}", u.updateSelector)
	mux.HandleFunc("DELETE /-/api/content-selectors/{name}", u.deleteSelector)
}

// writeSecurityError maps validation 400, not found 404, taken or in use
// 409, the rest 500.
func (u *UI) writeSecurityError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *repo.ValidationError
	var ruleUse *meta.RuleInUseError
	var selUse *meta.SelectorInUseError
	switch {
	case errors.As(err, &ve):
		first := "invalid request"
		if len(ve.Errors) > 0 {
			first = ve.Errors[0]
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": first, "errors": ve.Errors})
	case errors.Is(err, meta.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, meta.ErrExists):
		writeError(w, http.StatusConflict, "a rule or selector with this name already exists")
	case errors.As(err, &ruleUse):
		writeJSON(w, http.StatusConflict, map[string]any{"error": ruleUse.Error(), "repositories": ruleUse.Repos})
	case errors.As(err, &selUse):
		writeJSON(w, http.StatusConflict, map[string]any{"error": selUse.Error(), "roles": selUse.Roles})
	default:
		u.fail(w, r, err)
	}
}

// ---- routing rules ----

type routingRuleJSON struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Mode         string   `json:"mode"` // BLOCK | ALLOW
	Matchers     []string `json:"matchers"`
	Created      string   `json:"created"`
	Updated      string   `json:"updated"`
	Repositories []string `json:"repositories"` // proxies using it, sorted
}

// ruleUsers maps each rule name to the proxies using it, from s.
func ruleUsers(s *repo.Snapshot) map[string][]string {
	out := map[string][]string{}
	for _, rp := range s.Sorted {
		if rp.RoutingRule != "" {
			out[rp.RoutingRule] = append(out[rp.RoutingRule], rp.Name)
		}
	}
	return out
}

func toRuleJSON(r meta.RoutingRule, users []string) routingRuleJSON {
	return routingRuleJSON{Name: r.Name, Description: r.Description, Mode: r.Mode, Matchers: append([]string{}, r.Matchers...),
		Created: rfc3339(r.CreatedAt), Updated: rfc3339(r.UpdatedAt), Repositories: append([]string{}, users...)}
}

// ruleBody is the body of POST and PUT. On PUT, name may be omitted and
// must otherwise be the rule's own.
type ruleBody struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Mode        string   `json:"mode"`
	Matchers    []string `json:"matchers"`
}

func (b ruleBody) rule() (meta.RoutingRule, error) {
	r := meta.RoutingRule{Name: b.Name, Description: b.Description, Mode: strings.ToUpper(b.Mode), Matchers: b.Matchers}
	if errs := meta.ValidateRoutingRule(r); len(errs) > 0 {
		return r, &repo.ValidationError{Errors: errs}
	}
	return r, nil
}

// reloadRoutes gives the registry the stored rules after a change. It
// reports false after having answered the request.
func (u *UI) reloadRoutes(w http.ResponseWriter, r *http.Request) bool {
	list, err := u.DB.ListRoutingRules(r.Context())
	if err == nil {
		err = u.Repos.SetRoutingRules(list)
	}
	if err != nil {
		u.fail(w, r, err)
		return false
	}
	u.notify(r.Context(), "routing")
	return true
}

func (u *UI) listRoutingRules(w http.ResponseWriter, r *http.Request) {
	if !u.requirePicker(w, r, "") {
		return
	}
	list, err := u.DB.ListRoutingRules(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	users := ruleUsers(u.Repos.Snapshot())
	out := make([]routingRuleJSON, 0, len(list))
	for _, x := range list {
		out = append(out, toRuleJSON(x, users[x.Name]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"routingRules": out})
}

func (u *UI) getRoutingRule(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	x, err := u.DB.GetRoutingRule(r.Context(), r.PathValue("name"))
	if err != nil {
		u.writeSecurityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toRuleJSON(x, ruleUsers(u.Repos.Snapshot())[x.Name]))
}

func (u *UI) createRoutingRule(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body ruleBody
	if !decodeBody(w, r, &body) {
		return
	}
	rule, err := body.rule()
	if err == nil {
		err = u.DB.CreateRoutingRule(r.Context(), rule)
	}
	if err != nil {
		u.writeSecurityError(w, r, err)
		return
	}
	if !u.reloadRoutes(w, r) {
		return
	}
	stored, err := u.DB.GetRoutingRule(r.Context(), rule.Name)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "routingrule.create", "", rule.Name, ruleSummary(stored))
	writeJSON(w, http.StatusCreated, toRuleJSON(stored, nil))
}

func (u *UI) updateRoutingRule(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	name := r.PathValue("name")
	var body ruleBody
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Name != "" && body.Name != name {
		u.writeSecurityError(w, r, &repo.ValidationError{Errors: []string{"a routing rule cannot be renamed"}})
		return
	}
	body.Name = name
	rule, err := body.rule()
	if err == nil {
		err = u.DB.UpdateRoutingRule(r.Context(), rule)
	}
	if err != nil {
		u.writeSecurityError(w, r, err)
		return
	}
	if !u.reloadRoutes(w, r) {
		return
	}
	stored, err := u.DB.GetRoutingRule(r.Context(), name)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "routingrule.update", "", name, ruleSummary(stored))
	writeJSON(w, http.StatusOK, toRuleJSON(stored, ruleUsers(u.Repos.Snapshot())[name]))
}

// deleteRoutingRule removes a rule no proxy uses (409 naming them
// otherwise); the check and the delete run in one transaction under the
// registry's write lock, so no proxy is connected meanwhile.
func (u *UI) deleteRoutingRule(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	name := r.PathValue("name")
	err := u.Repos.WithWriteLock(r.Context(), func(*repo.Snapshot) error {
		return u.DB.DeleteRoutingRule(r.Context(), name)
	})
	if err != nil {
		u.writeSecurityError(w, r, err)
		return
	}
	if !u.reloadRoutes(w, r) {
		return
	}
	u.emit(r, "routingrule.delete", "", name, "")
	w.WriteHeader(http.StatusNoContent)
}

func ruleSummary(r meta.RoutingRule) string {
	return "mode=" + r.Mode + " matchers=" + strings.Join(r.Matchers, " ")
}

// testRoutingRule answers POST /-/api/routing-rules/test {path, name} or
// {path, mode, matchers}: whether the path may go to the remote and the
// matcher that decided it (null when none matched).
func (u *UI) testRoutingRule(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	var body struct {
		Path     string   `json:"path"`
		Name     string   `json:"name"`
		Mode     string   `json:"mode"`
		Matchers []string `json:"matchers"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	rule := meta.RoutingRule{Name: "test", Mode: strings.ToUpper(body.Mode), Matchers: body.Matchers}
	if body.Name != "" {
		stored, err := u.DB.GetRoutingRule(r.Context(), body.Name)
		if err != nil {
			u.writeSecurityError(w, r, err)
			return
		}
		rule = stored
	} else if errs := meta.ValidateRoutingRule(rule); len(errs) > 0 {
		u.writeSecurityError(w, r, &repo.ValidationError{Errors: errs})
		return
	}
	rt, err := repo.CompileRoute(rule)
	if err != nil {
		u.writeSecurityError(w, r, &repo.ValidationError{Errors: []string{err.Error()}})
		return
	}
	allowed, by := rt.Allowed("/" + strings.TrimPrefix(body.Path, "/"))
	var matched any
	if by != "" {
		matched = by
	}
	writeJSON(w, http.StatusOK, map[string]any{"allowed": allowed, "matchedBy": matched})
}

// ---- content selectors ----

type selectorJSON struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Expression  string   `json:"expression"`
	Created     string   `json:"created"`
	Updated     string   `json:"updated"`
	Roles       []string `json:"roles"` // roles with a privilege on it, sorted
}

func (u *UI) toSelectorJSON(r *http.Request, s meta.ContentSelector) selectorJSON {
	roles, _ := u.DB.RolesUsingSelector(r.Context(), s.Name)
	return selectorJSON{Name: s.Name, Description: s.Description, Expression: s.Expression,
		Created: rfc3339(s.CreatedAt), Updated: rfc3339(s.UpdatedAt), Roles: append([]string{}, roles...)}
}

// selectorBody is the body of POST and PUT. On PUT, name may be omitted
// and must otherwise be the selector's own.
type selectorBody struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Expression  string `json:"expression"`
}

func (b selectorBody) selector() (meta.ContentSelector, error) {
	s := meta.ContentSelector{Name: b.Name, Description: b.Description, Expression: strings.TrimSpace(b.Expression)}
	if errs := meta.ValidateContentSelector(s); len(errs) > 0 {
		return s, &repo.ValidationError{Errors: errs}
	}
	return s, nil
}

func (u *UI) listSelectors(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.requireApp(w, r, "selectors", meta.ActionRead); !ok {
		return
	}
	list, err := u.DB.ListContentSelectors(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	out := make([]selectorJSON, 0, len(list))
	for _, x := range list {
		out = append(out, u.toSelectorJSON(r, x))
	}
	writeJSON(w, http.StatusOK, map[string]any{"contentSelectors": out})
}

func (u *UI) getSelector(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.requireApp(w, r, "selectors", meta.ActionRead); !ok {
		return
	}
	x, err := u.DB.GetContentSelector(r.Context(), r.PathValue("name"))
	if err != nil {
		u.writeSecurityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u.toSelectorJSON(r, x))
}

func (u *UI) createSelector(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.requireAppInteractive(w, r, "selectors", meta.ActionAdd); !ok {
		return
	}
	var body selectorBody
	if !decodeBody(w, r, &body) {
		return
	}
	sel, err := body.selector()
	if err == nil {
		err = u.DB.CreateContentSelector(r.Context(), sel)
	}
	if err != nil {
		u.writeSecurityError(w, r, err)
		return
	}
	if !u.applied(w, r, "") { // the auth snapshot parses the selectors
		return
	}
	stored, err := u.DB.GetContentSelector(r.Context(), sel.Name)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "selector.create", "", sel.Name, stored.Expression)
	writeJSON(w, http.StatusCreated, u.toSelectorJSON(r, stored))
}

func (u *UI) updateSelector(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.requireAppInteractive(w, r, "selectors", meta.ActionEdit); !ok {
		return
	}
	name := r.PathValue("name")
	var body selectorBody
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Name != "" && body.Name != name {
		u.writeSecurityError(w, r, &repo.ValidationError{Errors: []string{"a content selector cannot be renamed"}})
		return
	}
	body.Name = name
	sel, err := body.selector()
	if err == nil {
		err = u.DB.UpdateContentSelector(r.Context(), sel)
	}
	if err != nil {
		u.writeSecurityError(w, r, err)
		return
	}
	if !u.applied(w, r, "") {
		return
	}
	stored, err := u.DB.GetContentSelector(r.Context(), name)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "selector.update", "", name, stored.Expression)
	writeJSON(w, http.StatusOK, u.toSelectorJSON(r, stored))
}

// deleteSelector removes a selector no role names (409 naming the roles
// otherwise), under the access lock so no role edit adds one meanwhile.
func (u *UI) deleteSelector(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.requireAppInteractive(w, r, "selectors", meta.ActionDelete); !ok {
		return
	}
	name := r.PathValue("name")
	unlock, ok := u.lockAccess(w, r)
	if !ok {
		return
	}
	defer unlock()
	if err := u.DB.DeleteContentSelector(r.Context(), name); err != nil {
		u.writeSecurityError(w, r, err)
		return
	}
	if !u.applied(w, r, "") {
		return
	}
	u.emit(r, "selector.delete", "", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// Preview limits: paths listed, and assets read per preview.
const (
	selectorPreviewLimit = 100
	selectorPreviewScan  = 20000
)

// previewSelector answers POST /-/api/content-selectors/preview
// {expression, repo}: the first selectorPreviewLimit asset paths of the
// repository the expression matches, in path order, from the first
// selectorPreviewScan assets (truncated says whether the scan stopped
// early).
func (u *UI) previewSelector(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	var body struct {
		Expression string `json:"expression"`
		Repo       string `json:"repo"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	expr, err := csel.Parse(strings.TrimSpace(body.Expression))
	if err != nil {
		u.writeSecurityError(w, r, &repo.ValidationError{Errors: []string{"expression: " + err.Error()}})
		return
	}
	rp := u.Repos.Get(body.Repo)
	if rp == nil {
		writeError(w, http.StatusNotFound, "repository not found")
		return
	}
	paths := []string{}
	scanned, truncated := 0, false
	after := ""
	for scanned < selectorPreviewScan && len(paths) < selectorPreviewLimit {
		page, err := u.DB.ScanAssets(r.Context(), rp.Name, after, 500)
		if err != nil {
			u.fail(w, r, err)
			return
		}
		if len(page) == 0 {
			break
		}
		for _, a := range page {
			scanned++
			if len(paths) >= selectorPreviewLimit {
				truncated = true
				break
			}
			if expr.Match(auth.NexusFormat(rp.Format), "/"+a.Path) {
				paths = append(paths, a.Path)
			}
		}
		after = page[len(page)-1].Path
	}
	if scanned >= selectorPreviewScan {
		truncated = true
	}
	writeJSON(w, http.StatusOK, map[string]any{"paths": paths, "scanned": scanned, "truncated": truncated})
}
