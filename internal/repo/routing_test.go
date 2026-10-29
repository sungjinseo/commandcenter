package repo

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
)

func TestRouteAllowed(t *testing.T) {
	block, _ := CompileRoute(meta.RoutingRule{Name: "b", Mode: meta.RoutingBlock, Matchers: []string{`^/internal/`, `.*-SNAPSHOT.*`}})
	allow, _ := CompileRoute(meta.RoutingRule{Name: "a", Mode: meta.RoutingAllow, Matchers: []string{`^/org/acme/`}})
	for _, c := range []struct {
		rt      *Route
		path    string
		allowed bool
		by      string
	}{
		{block, "/org/acme/lib/1.0/lib.jar", true, ""},
		{block, "/internal/x", false, `^/internal/`},
		{block, "/org/acme/lib/1.0-SNAPSHOT/lib.jar", false, `.*-SNAPSHOT.*`},
		{allow, "/org/acme/lib.jar", true, `^/org/acme/`},
		{allow, "/org/other/lib.jar", false, ""},
		{allow, "/", false, ""},
	} {
		allowed, by := c.rt.Allowed(c.path)
		if allowed != c.allowed || by != c.by {
			t.Errorf("%s %s: %v %q, want %v %q", c.rt.Mode, c.path, allowed, by, c.allowed, c.by)
		}
	}
	if _, err := CompileRoute(meta.RoutingRule{Name: "x", Mode: meta.RoutingBlock, Matchers: []string{"("}}); err == nil {
		t.Fatal("bad regex compiled")
	}
}

func TestRegistryRouteAllowedAndRuleCheck(t *testing.T) {
	defs := []config.Repository{
		{Name: "p", Format: "maven", Type: "proxy", Remote: "https://x/", RoutingRule: "b"},
		{Name: "free", Format: "maven", Type: "proxy", Remote: "https://x/"},
		{Name: "ghost", Format: "maven", Type: "proxy", Remote: "https://x/", RoutingRule: "nope"},
	}
	for i := range defs {
		config.ApplyRepositoryDefaults(&defs[i])
	}
	db, err := meta.Open(metatest.Target(t, filepath.Join(t.TempDir(), "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	reg := NewRegistry(db, defs)
	if err := reg.SetRoutingRules([]meta.RoutingRule{{Name: "b", Mode: meta.RoutingBlock, Matchers: []string{`^/internal/`}}}); err != nil {
		t.Fatal(err)
	}
	if !reg.RouteAllowed(reg.Get("p"), "org/x") || reg.RouteAllowed(reg.Get("p"), "internal/x") || reg.RouteAllowed(reg.Get("p"), "/internal/x") {
		t.Fatal("rule not applied")
	}
	if !reg.RouteAllowed(reg.Get("free"), "internal/x") || !reg.RouteAllowed(reg.Get("ghost"), "internal/x") {
		t.Fatal("a proxy without a (known) rule must allow everything")
	}
	// Create and update refuse an unknown rule and a rule on a hosted repository.
	ctx := context.Background()
	err = reg.Create(ctx, config.Repository{Name: "q", Format: "maven", Type: "proxy", Remote: "https://x/", RoutingRule: "nope"})
	if err == nil || !strings.Contains(err.Error(), `unknown routing rule "nope"`) {
		t.Fatalf("create: %v", err)
	}
	err = reg.Create(ctx, config.Repository{Name: "h", Format: "maven", Type: "hosted", RoutingRule: "b"})
	if err == nil || !strings.Contains(err.Error(), "only valid for proxy") {
		t.Fatalf("hosted: %v", err)
	}
	if err := reg.Create(ctx, config.Repository{Name: "q", Format: "maven", Type: "proxy", Remote: "https://x/", RoutingRule: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Update(ctx, "q", config.Repository{Remote: "https://x/", RoutingRule: "nope"}, ClearSecrets{}); err == nil {
		t.Fatal("update to unknown rule accepted")
	}
	if err := reg.Update(ctx, "q", config.Repository{Remote: "https://x/"}, ClearSecrets{}); err != nil || reg.Get("q").RoutingRule != "" {
		t.Fatalf("clear rule: %v %q", err, reg.Get("q").RoutingRule)
	}
}
