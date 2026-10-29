package repo

import (
	"fmt"
	"regexp"
	"strings"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

// Route is a compiled routing rule (meta.RoutingRule).
type Route struct {
	Name     string
	Mode     string
	matchers []*regexp.Regexp
}

// CompileRoute compiles r's matchers; the rule must have passed
// meta.ValidateRoutingRule.
func CompileRoute(r meta.RoutingRule) (*Route, error) {
	out := &Route{Name: r.Name, Mode: r.Mode}
	for _, m := range r.Matchers {
		re, err := regexp.Compile(m)
		if err != nil {
			return nil, fmt.Errorf("routing rule %q: matcher %q: %w", r.Name, m, err)
		}
		out.matchers = append(out.matchers, re)
	}
	return out, nil
}

// Allowed reports whether path (with its leading "/") may be requested from
// the remote, and the matcher that decided it ("" when none matched: BLOCK
// allows, ALLOW refuses).
func (rt *Route) Allowed(path string) (allowed bool, matchedBy string) {
	for _, re := range rt.matchers {
		if re.MatchString(path) {
			return rt.Mode == meta.RoutingAllow, re.String()
		}
	}
	return rt.Mode == meta.RoutingBlock, ""
}

// SetRoutingRules replaces the registry's routing rules; a proxy naming a
// rule not in the list sends every path to its remote (RouteAllowed). The
// app loads the stored rules at start and the UI reloads them after a
// change.
func (r *Registry) SetRoutingRules(rules []meta.RoutingRule) error {
	m := make(map[string]*Route, len(rules))
	for _, rule := range rules {
		rt, err := CompileRoute(rule)
		if err != nil {
			return err
		}
		m[rule.Name] = rt
	}
	r.routes.Store(&m)
	return nil
}

// Route returns the compiled rule by name, or nil.
func (r *Registry) Route(name string) *Route {
	if m := r.routes.Load(); m != nil {
		return (*m)[name]
	}
	return nil
}

// RouteAllowed reports whether rp's routing rule lets path (with or
// without its leading "/") go to the remote: true without a rule, or when
// the named rule is unknown (a config import may name one that does not
// exist; the app warns). It suits proxy.Fetcher.RouteAllowed.
func (r *Registry) RouteAllowed(rp *Repository, path string) bool {
	if rp.RoutingRule == "" {
		return true
	}
	rt := r.Route(rp.RoutingRule)
	if rt == nil {
		return true
	}
	ok, _ := rt.Allowed("/" + strings.TrimPrefix(path, "/"))
	return ok
}

// checkRoute refuses a definition naming a routing rule the registry does
// not have.
func (r *Registry) checkRoute(def config.Repository) error {
	if def.RoutingRule != "" && r.Route(def.RoutingRule) == nil {
		return &ValidationError{Errors: []string{fmt.Sprintf("repository %q: unknown routing rule %q", def.Name, def.RoutingRule)}}
	}
	return nil
}
