package nexusapi

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/ui"
)

// Nexus privileges over Command Center roles. A Nexus privilege is a named
// object that roles list; here privileges live inside roles. So:
//
//   - the privileges Nexus builds in (nx-all, nx-repository-view-*,
//     nx-repository-admin-*, nx-users-read, ...) are computed from the
//     repositories and translated into a role's own privileges;
//   - a privilege made through this API is the role "priv:<name>" holding
//     its translation, which other roles contain (nested roles);
//   - what a role or privilege was written as is kept with it (meta.Role
//     Nexus) and read back as long as it still translates to what the role
//     holds, so a client reads what it wrote (Terraform sees no drift),
//     although edit means add and edit here.

// nxPriv is a Nexus privilege as the API reads and writes it.
type nxPriv struct {
	Type            string   `json:"type"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	ReadOnly        bool     `json:"readOnly"`
	Pattern         string   `json:"pattern,omitempty"`
	Domain          string   `json:"domain,omitempty"`
	Actions         []string `json:"actions,omitempty"`
	Format          string   `json:"format,omitempty"`
	Repository      string   `json:"repository,omitempty"`
	ContentSelector string   `json:"contentSelector,omitempty"`
	ScriptName      string   `json:"scriptName,omitempty"`
}

// Nexus privilege types.
const (
	ptApplication = "application"
	ptWildcard    = "wildcard"
	ptView        = "repository-view"
	ptAdmin       = "repository-admin"
	ptSelector    = "repository-content-selector"
	ptScript      = "script"
)

// grant is the translation of Nexus privileges: Command Center privileges
// by target, and contained roles.
type grant struct {
	privs map[string]*meta.Privilege // by target (and repos for selectors)
	roles []string
}

func newGrant() *grant { return &grant{privs: map[string]*meta.Privilege{}} }

func (g *grant) add(target string, repos []string, actions ...string) {
	key := target + "|" + strings.Join(repos, ",")
	p := g.privs[key]
	if p == nil {
		p = &meta.Privilege{Target: target, Repos: repos}
		g.privs[key] = p
	}
	for _, a := range actions {
		if !slices.Contains(p.Actions, a) {
			p.Actions = append(p.Actions, a)
		}
	}
}

func (g *grant) contain(role string) {
	if !slices.Contains(g.roles, role) {
		g.roles = append(g.roles, role)
	}
}

// privileges lists g's privileges in a stable order, actions canonical.
func (g *grant) privileges() []meta.Privilege {
	out := make([]meta.Privilege, 0, len(g.privs))
	for _, p := range g.privs {
		q := *p
		q.Actions = canonActions(q.Actions)
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Target+strings.Join(out[i].Repos, ",") < out[j].Target+strings.Join(out[j].Repos, ",")
	})
	return out
}

var actionOrder = []string{meta.ActionBrowse, meta.ActionRead, meta.ActionAdd, meta.ActionEdit, meta.ActionDelete, meta.ActionRun}

func canonActions(acts []string) []string {
	out := []string{}
	for _, a := range actionOrder {
		if slices.Contains(acts, a) {
			out = append(out, a)
		}
	}
	return out
}

// canon is a role's content as a comparable string: its privileges with
// sorted actions and its contained roles, both sorted.
func canon(privs []meta.Privilege, roles []string) string {
	var parts []string
	for _, p := range privs {
		parts = append(parts, p.Target+"|"+strings.Join(p.Repos, ",")+"|"+strings.Join(canonActions(p.Actions), ","))
	}
	sort.Strings(parts)
	r := slices.Clone(roles)
	sort.Strings(r)
	return strings.Join(parts, ";") + "#" + strings.Join(r, ",")
}

// badRequest is a translation error: answered 400 with its message.
type badRequest struct{ msg string }

func (e *badRequest) Error() string { return e.msg }

func bad(format string, args ...any) error { return &badRequest{fmt.Sprintf(format, args...)} }

// viewActions maps Nexus repository actions to Command Center's. A Nexus
// edit is every PUT, a new file or an overwrite; here a new file is add, so
// edit becomes add and edit (security-10): a CI role that deploys with
// read and edit in Nexus keeps deploying.
func viewActions(in []string, admin bool) ([]string, error) {
	var out []string
	for _, a := range in {
		switch strings.ToUpper(strings.TrimSpace(a)) {
		case "BROWSE":
			out = append(out, meta.ActionBrowse)
		case "READ":
			out = append(out, meta.ActionRead)
		case "ADD", "CREATE":
			out = append(out, meta.ActionAdd)
		case "EDIT", "UPDATE":
			if admin {
				out = append(out, meta.ActionEdit)
			} else {
				out = append(out, meta.ActionAdd, meta.ActionEdit)
			}
		case "DELETE":
			out = append(out, meta.ActionDelete)
		case "ALL", "*":
			out = append(out, meta.Actions...)
		default:
			return nil, bad("action %q is not a repository action (BROWSE, READ, EDIT, ADD, DELETE or ALL)", a)
		}
	}
	if len(out) == 0 {
		return nil, bad("actions: at least one action is required")
	}
	return out, nil
}

// appActions maps Nexus application actions of domain to Command Center's.
// ALL is every action the domain delegates here; an action it does not
// delegate is refused.
func appActions(domain string, in []string) ([]string, error) {
	have := auth.AppDomains[domain]
	var out []string
	for _, a := range in {
		var act string
		switch strings.ToUpper(strings.TrimSpace(a)) {
		case "READ", "BROWSE":
			act = meta.ActionRead
		case "ADD", "CREATE":
			act = meta.ActionAdd
		case "EDIT", "UPDATE":
			act = meta.ActionEdit
		case "DELETE":
			act = meta.ActionDelete
		case "RUN", "START", "STOP":
			act = meta.ActionRun
		case "ALL", "*":
			out = append(out, have...)
			continue
		default:
			return nil, bad("action %q of the %q domain has no Command Center equivalent", a, domain)
		}
		if !slices.Contains(have, act) {
			return nil, bad("the %q domain delegates only %s here; %s stays with administrators", domain, strings.Join(nxAppActions(have), ", "), strings.ToUpper(a))
		}
		out = append(out, act)
	}
	if len(out) == 0 {
		return nil, bad("actions: at least one action is required")
	}
	return out, nil
}

// repoAdminActions refuses add on one repository (add creates
// repositories, so it is granted on a format or on all), except as part of
// ALL, which there means every other action, as Nexus's per-repository
// administration privileges have no add.
func repoAdminActions(target string, acts, in []string) ([]string, error) {
	if !strings.HasPrefix(target, "repo:") || !slices.Contains(acts, meta.ActionAdd) {
		return acts, nil
	}
	if !slices.ContainsFunc(in, func(a string) bool { a = strings.TrimSpace(a); return a == "*" || strings.EqualFold(a, "ALL") }) {
		return nil, bad("actions: ADD creates repositories: grant it on a format or on every repository")
	}
	return slices.DeleteFunc(slices.Clone(acts), func(a string) bool { return a == meta.ActionAdd }), nil
}

// nxAppActions are Command Center application actions in Nexus spelling.
func nxAppActions(acts []string) []string {
	out := []string{}
	for _, a := range acts {
		switch a {
		case meta.ActionRead:
			out = append(out, "READ")
		case meta.ActionAdd:
			out = append(out, "ADD")
		case meta.ActionEdit:
			out = append(out, "EDIT")
		case meta.ActionDelete:
			out = append(out, "DELETE")
		case meta.ActionRun:
			out = append(out, "START", "STOP")
		}
	}
	return out
}

// repoKnown answers a repository's Command Center format ("" when there is
// no such repository).
type repoKnown func(name string) string

// scopeTarget is the target of format and repository (Nexus spelling, "*"
// for any): "*", "format:<f>" or "repo:<name>", checked against the
// repositories.
func scopeTarget(format, repository string, repos repoKnown) (string, error) {
	format, repository = strings.TrimSpace(format), strings.TrimSpace(repository)
	if format == "" {
		format = "*"
	}
	if repository == "" || repository == "*" {
		if format == "*" {
			return "*", nil
		}
		if !slices.Contains(ui.AccessFormats(), ccFormat(format)) {
			return "", bad("format: unknown format %q", format)
		}
		return "format:" + ccFormat(format), nil
	}
	f := repos(repository)
	if f == "" {
		return "", bad("repository: unknown repository %q", repository)
	}
	if format != "*" && ccFormat(format) != f {
		return "", bad("format: repository %q is %s, not %s", repository, nexusFormat(f), format)
	}
	return "repo:" + repository, nil
}

// translatePriv is the grant of one Nexus privilege object.
func translatePriv(p nxPriv, repos repoKnown) (*grant, error) {
	g := newGrant()
	switch p.Type {
	case ptView, ptAdmin:
		acts, err := viewActions(p.Actions, p.Type == ptAdmin)
		if err != nil {
			return nil, err
		}
		t, err := scopeTarget(p.Format, p.Repository, repos)
		if err != nil {
			return nil, err
		}
		if p.Type == ptAdmin {
			if acts, err = repoAdminActions(t, acts, p.Actions); err != nil {
				return nil, err
			}
			t = meta.AdminTargetPrefix + t
		}
		g.add(t, nil, acts...)
	case ptSelector:
		acts, err := viewActions(p.Actions, false)
		if err != nil {
			return nil, err
		}
		scope := strings.TrimSpace(p.Repository)
		switch {
		case scope == "" || scope == "*":
			scope = "*"
		case strings.HasPrefix(scope, "*-"):
			if !slices.Contains(ui.AccessFormats(), ccFormat(scope[2:])) {
				return nil, bad("repository: unknown format in %q", scope)
			}
		default:
			f := repos(scope)
			if f == "" {
				return nil, bad("repository: unknown repository %q", scope)
			}
			if fm := strings.TrimSpace(p.Format); fm != "" && fm != "*" && ccFormat(fm) != f {
				return nil, bad("format: repository %q is %s, not %s", scope, nexusFormat(f), fm)
			}
		}
		if fm := strings.TrimSpace(p.Format); fm != "" && fm != "*" && !slices.Contains(ui.AccessFormats(), ccFormat(fm)) {
			return nil, bad("format: unknown format %q", fm)
		}
		if strings.TrimSpace(p.ContentSelector) == "" {
			return nil, bad("contentSelector: a content selector is required")
		}
		g.add(meta.SelectorPrefix+p.ContentSelector, []string{scope}, acts...)
	case ptApplication:
		if err := addApp(g, strings.TrimSpace(p.Domain), p.Actions); err != nil {
			return nil, err
		}
	case ptWildcard:
		if err := addWildcard(g, strings.TrimSpace(p.Pattern), repos); err != nil {
			return nil, err
		}
	case ptScript:
		return nil, bad("script privileges have no Command Center equivalent: it has no script API")
	default:
		return nil, bad("unknown privilege type %q", p.Type)
	}
	return g, nil
}

// addApp adds an application privilege of domain: "metrics" is the
// metrics target, the rest "app:<domain>".
func addApp(g *grant, domain string, actions []string) error {
	if domain == "metrics" {
		for _, a := range actions {
			if u := strings.ToUpper(a); u != "READ" && u != "ALL" && u != "*" {
				return bad("the metrics domain has only READ here")
			}
		}
		g.add(auth.MetricsTarget, nil, meta.ActionRead)
		return nil
	}
	if _, ok := auth.AppDomains[domain]; !ok {
		return bad("domain: the %q domain has no Command Center equivalent (it delegates %s)", domain, strings.Join(appDomainNames(), ", "))
	}
	acts, err := appActions(domain, actions)
	if err != nil {
		return err
	}
	g.add(meta.AppTargetPrefix+domain, nil, acts...)
	return nil
}

func appDomainNames() []string {
	out := []string{"metrics"}
	for d := range auth.AppDomains {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// addWildcard translates the wildcard patterns that map cleanly: nexus:*
// (the admin role), nexus:repository-view|repository-admin:<formats>:
// <repositories>:<actions> and nexus:<domain>:<actions>. A missing part
// is "*", and a part may list several values, as Shiro reads them.
func addWildcard(g *grant, pattern string, repos repoKnown) error {
	parts := strings.Split(pattern, ":")
	if len(parts) < 2 || parts[0] != "nexus" {
		return bad("pattern: %q has no Command Center equivalent (use nexus:*, nexus:repository-view:..., nexus:repository-admin:... or nexus:<domain>:<actions>)", pattern)
	}
	part := func(i int) []string {
		if i >= len(parts) || parts[i] == "" {
			return []string{"*"}
		}
		return strings.Split(parts[i], ",")
	}
	switch kind := parts[1]; kind {
	case "*":
		if len(parts) > 2 {
			return bad("pattern: %q has no Command Center equivalent", pattern)
		}
		g.contain(meta.AdminRole)
	case "repository-view", "repository-admin":
		if len(parts) > 5 {
			return bad("pattern: %q has too many parts", pattern)
		}
		acts, err := viewActions(part(4), kind == "repository-admin")
		if err != nil {
			return bad("pattern: %v", err)
		}
		for _, f := range part(2) {
			for _, r := range part(3) {
				t, err := scopeTarget(f, r, repos)
				if err != nil {
					return bad("pattern: %v", err)
				}
				acts := acts
				if kind == "repository-admin" {
					if acts, err = repoAdminActions(t, acts, part(4)); err != nil {
						return bad("pattern: %v", err)
					}
					t = meta.AdminTargetPrefix + t
				}
				g.add(t, nil, acts...)
			}
		}
	default:
		if len(parts) > 3 {
			return bad("pattern: %q has no Command Center equivalent", pattern)
		}
		if err := addApp(g, kind, part(2)); err != nil {
			return bad("pattern: %v", err)
		}
	}
	return nil
}

// ---- built-in privileges ----

// builtin is a privilege Nexus builds in, computed from the repositories.
type builtin struct {
	nxPriv
	grant *grant
}

// viewName is the built-in name of a repository-view (or, with admin,
// repository-admin) privilege; format and repo "*" for any.
func viewName(admin bool, format, repo, action string) string {
	kind := "view"
	if admin {
		kind = "admin"
	}
	return "nx-repository-" + kind + "-" + format + "-" + repo + "-" + action
}

// nxAction is a Command Center repository action in a built-in name.
func nxAction(a string) string {
	if a == "all" {
		return "*"
	}
	return a
}

// appBuiltins are the built-in application privileges this API knows, by
// name: domain and Nexus actions.
var appBuiltins = func() map[string]nxPriv {
	m := map[string]nxPriv{
		"nx-metrics-all":      {Domain: "metrics", Actions: []string{"ALL"}},
		"nx-search-read":      {Domain: "search", Actions: []string{"READ"}},
		"nx-healthcheck-read": {Domain: "healthcheck", Actions: []string{"READ"}},
		"nx-userschangepw":    {Domain: "userschangepw", Actions: []string{"ADD", "READ"}},
		"nx-component-upload": {Domain: "component", Actions: []string{"ADD"}},
		"nx-apikey-all":       {Domain: "apikey", Actions: []string{"ALL"}},
		"nx-settings-read":    {Domain: "settings", Actions: []string{"READ"}},
		"nx-settings-all":     {Domain: "settings", Actions: []string{"ALL"}},
		"nx-blobstores-read":  {Domain: "blobstores", Actions: []string{"READ"}},
		"nx-blobstores-all":   {Domain: "blobstores", Actions: []string{"ALL"}},
		"nx-tasks-read":       {Domain: "tasks", Actions: []string{"READ"}},
		"nx-tasks-run":        {Domain: "tasks", Actions: []string{"START", "STOP"}},
		"nx-tasks-all":        {Domain: "tasks", Actions: []string{"ALL"}},
	}
	for _, d := range []string{"users", "roles", "selectors"} {
		for _, a := range []string{"create", "read", "update", "delete", "all"} {
			m["nx-"+d+"-"+a] = nxPriv{Domain: d, Actions: []string{strings.ToUpper(a)}}
		}
	}
	for n, p := range m {
		p.Type, p.Name, p.ReadOnly = ptApplication, n, true
		p.Description = "Built-in: " + strings.ToLower(strings.Join(p.Actions, ", ")) + " of " + p.Domain
		m[n] = p
	}
	return m
}()

// builtinPriv resolves a built-in privilege name; ok is false for a name
// that is not one.
func builtinPriv(name string, repos repoKnown) (builtin, bool) {
	if name == "nx-all" {
		g := newGrant()
		g.contain(meta.AdminRole)
		return builtin{nxPriv{Type: ptWildcard, Name: name, Description: "Built-in: everything (the admin role)", ReadOnly: true, Pattern: "nexus:*"}, g}, true
	}
	if p, ok := appBuiltins[name]; ok {
		g := newGrant()
		if addApp(g, p.Domain, p.Actions) != nil {
			return builtin{}, false
		}
		return builtin{p, g}, true
	}
	for _, kind := range []string{"view", "admin"} {
		rest, ok := strings.CutPrefix(name, "nx-repository-"+kind+"-")
		if !ok {
			continue
		}
		format, rest, ok := strings.Cut(rest, "-")
		i := strings.LastIndex(rest, "-")
		if !ok || i <= 0 {
			return builtin{}, false
		}
		repo, action := rest[:i], rest[i+1:]
		if format == "*" && repo != "*" {
			return builtin{}, false // Nexus has no per-repository name without its format
		}
		if repo != "*" && nexusFormat(repos(repo)) != format {
			return builtin{}, false
		}
		admin := kind == "admin"
		if admin && action == "add" && repo != "*" {
			return builtin{}, false
		}
		var acts []string
		switch action {
		case "browse", "read", "edit", "add", "delete":
			acts = []string{strings.ToUpper(action)}
		case "*":
			acts = []string{"ALL"}
		default:
			return builtin{}, false
		}
		p := nxPriv{Type: ptView, Name: name, ReadOnly: true, Actions: acts, Format: format, Repository: repo}
		if admin {
			p.Type = ptAdmin
		}
		g, err := translatePriv(p, repos)
		if err != nil {
			return builtin{}, false
		}
		p.Description = "Built-in: " + strings.ToLower(action) + " on " + scopeText(format, repo)
		if admin {
			p.Description = "Built-in: administer (" + strings.ToLower(action) + ") " + scopeText(format, repo)
		}
		return builtin{p, g}, true
	}
	return builtin{}, false
}

func scopeText(format, repo string) string {
	switch {
	case format == "*":
		return "every repository"
	case repo == "*":
		return "every " + format + " repository"
	}
	return "repository " + repo
}

// builtinNames lists every built-in privilege name for the repositories
// (name to Command Center format), sorted.
func builtinNames(repos map[string]string, formats []string) []string {
	out := []string{"nx-all"}
	for n := range appBuiltins {
		out = append(out, n)
	}
	views := []string{"browse", "read", "edit", "add", "delete", "*"}
	admins := []string{"browse", "read", "edit", "delete", "*"}
	scopes := [][2]string{{"*", "*"}}
	for _, f := range formats {
		scopes = append(scopes, [2]string{nexusFormat(f), "*"})
	}
	for name, f := range repos {
		scopes = append(scopes, [2]string{nexusFormat(f), name})
	}
	for _, s := range scopes {
		for _, a := range views {
			out = append(out, viewName(false, s[0], s[1], a))
		}
		for _, a := range admins {
			out = append(out, viewName(true, s[0], s[1], a))
		}
		if s[1] == "*" {
			out = append(out, viewName(true, s[0], s[1], "add"))
		}
	}
	sort.Strings(out)
	return out
}

// ---- reading roles back ----

// derivedNames are the built-in privilege names of a Command Center
// privilege, "" target parts skipped: what a role holds, read in Nexus
// terms when it has no Nexus form to read back. A selector privilege
// inside a role (not a "priv:" role) has no Nexus name and is left out.
func derivedNames(p meta.Privilege, repos repoKnown) []string {
	target := p.Target
	admin := false
	if t, ok := strings.CutPrefix(target, meta.AdminTargetPrefix); ok {
		target, admin = t, true
	}
	var format, repo string
	switch {
	case target == "*":
		format, repo = "*", "*"
	case strings.HasPrefix(target, "format:"):
		format, repo = nexusFormat(strings.TrimPrefix(target, "format:")), "*"
	case strings.HasPrefix(target, "repo:"):
		repo = strings.TrimPrefix(target, "repo:")
		f := repos(repo)
		if f == "" {
			return nil
		}
		format = nexusFormat(f)
	case target == auth.MetricsTarget && !admin:
		return []string{"nx-metrics-all"}
	case strings.HasPrefix(target, meta.AppTargetPrefix) && !admin:
		return appNames(strings.TrimPrefix(target, meta.AppTargetPrefix), p.Actions)
	default:
		return nil
	}
	acts := canonActions(p.Actions)
	all := []string{meta.ActionBrowse, meta.ActionRead, meta.ActionAdd, meta.ActionEdit, meta.ActionDelete}
	if admin && repo != "*" {
		all = slices.DeleteFunc(all, func(a string) bool { return a == meta.ActionAdd })
	}
	if slices.Equal(acts, all) {
		return []string{viewName(admin, format, repo, "*")}
	}
	var out []string
	for _, a := range acts {
		out = append(out, viewName(admin, format, repo, nxAction(a)))
	}
	return out
}

// appNames are the built-in names of an "app:<domain>" privilege.
func appNames(domain string, acts []string) []string {
	switch domain {
	case "search":
		return []string{"nx-search-read"}
	case "healthcheck":
		return []string{"nx-healthcheck-read"}
	case "userschangepw":
		return []string{"nx-userschangepw"}
	case "component":
		return []string{"nx-component-upload"}
	case "apikey":
		return []string{"nx-apikey-all"}
	}
	have := auth.AppDomains[domain]
	if len(have) > 1 && len(canonActions(acts)) == len(have) {
		return []string{"nx-" + domain + "-all"}
	}
	var out []string
	for _, a := range canonActions(acts) {
		switch a {
		case meta.ActionRead:
			out = append(out, "nx-"+domain+"-read")
		case meta.ActionAdd:
			out = append(out, "nx-"+domain+"-create")
		case meta.ActionEdit:
			out = append(out, "nx-"+domain+"-update")
		case meta.ActionDelete:
			out = append(out, "nx-"+domain+"-delete")
		case meta.ActionRun:
			out = append(out, "nx-"+domain+"-run")
		}
	}
	return out
}

// nexusForm is what meta.Role.Nexus holds: a role's privilege and role
// lists as written (Privileges, Roles), or a "priv:" role's privilege.
type nexusForm struct {
	Privileges []string `json:"privileges,omitempty"`
	Roles      []string `json:"roles,omitempty"`
	Privilege  *nxPriv  `json:"privilege,omitempty"`
}

func (f nexusForm) encode() string {
	b, _ := json.Marshal(f)
	return string(b)
}

func decodeForm(s string) (nexusForm, bool) {
	var f nexusForm
	if s == "" || json.Unmarshal([]byte(s), &f) != nil {
		return nexusForm{}, false
	}
	return f, true
}
