package meta

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"commandcenter/internal/config"
)

func TestRoutingRuleCRUDAndInUse(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	r := RoutingRule{Name: "no-snapshots", Description: "d", Mode: RoutingBlock, Matchers: []string{`.*-SNAPSHOT.*`, `^/internal/`}}
	if err := db.CreateRoutingRule(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRoutingRule(ctx, r); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := db.GetRoutingRule(ctx, "no-snapshots")
	if err != nil || got.Mode != RoutingBlock || !reflect.DeepEqual(got.Matchers, r.Matchers) || got.CreatedAt.IsZero() {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := db.GetRoutingRule(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	r.Mode, r.Matchers = RoutingAllow, []string{`^/org/`}
	if err := db.UpdateRoutingRule(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateRoutingRule(ctx, RoutingRule{Name: "nope", Mode: RoutingAllow, Matchers: []string{"x"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	list, err := db.ListRoutingRules(ctx)
	if err != nil || len(list) != 1 || list[0].Mode != RoutingAllow || !list[0].UpdatedAt.After(got.UpdatedAt) && !list[0].UpdatedAt.Equal(got.UpdatedAt) {
		t.Fatalf("list: %+v %v", list, err)
	}

	// A proxy naming the rule blocks its delete.
	if err := db.CreateRepository(ctx, config.Repository{Name: "central", Format: "maven", Type: "proxy", Remote: "https://x/", RoutingRule: "no-snapshots"}); err != nil {
		t.Fatal(err)
	}
	var inUse *RuleInUseError
	if err := db.DeleteRoutingRule(ctx, "no-snapshots"); !errors.As(err, &inUse) || !reflect.DeepEqual(inUse.Repos, []string{"central"}) {
		t.Fatalf("delete in use: %v", err)
	}
	users, _ := db.RepositoriesUsingRoutingRule(ctx, "no-snapshots")
	if !reflect.DeepEqual(users, []string{"central"}) {
		t.Fatalf("users = %v", users)
	}
	if _, err := db.DeleteRepository(ctx, "central"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteRoutingRule(ctx, "no-snapshots"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteRoutingRule(ctx, "no-snapshots"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestValidateRoutingRule(t *testing.T) {
	for _, c := range []struct {
		r    RoutingRule
		want string // "" for valid
	}{
		{RoutingRule{Name: "ok", Mode: "BLOCK", Matchers: []string{"^/a"}}, ""},
		{RoutingRule{Name: "bad name!", Mode: "BLOCK", Matchers: []string{"^/a"}}, "invalid routing rule name"},
		{RoutingRule{Name: "ok", Mode: "block", Matchers: []string{"^/a"}}, "mode must be"},
		{RoutingRule{Name: "ok", Mode: "ALLOW"}, "at least one matcher"},
		{RoutingRule{Name: "ok", Mode: "ALLOW", Matchers: []string{"("}}, "invalid regular expression"},
		{RoutingRule{Name: "ok", Mode: "ALLOW", Matchers: []string{strings.Repeat("a", 257)}}, "longer than"},
		{RoutingRule{Name: "ok", Mode: "ALLOW", Matchers: make([]string, 51)}, "at most 50"},
	} {
		errs := ValidateRoutingRule(c.r)
		if c.want == "" && len(errs) > 0 || c.want != "" && !strings.Contains(strings.Join(errs, ";"), c.want) {
			t.Errorf("%+v: %v, want %q", c.r, errs, c.want)
		}
	}
}

func TestContentSelectorCRUDAndInUse(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	s := ContentSelector{Name: "acme", Description: "d", Expression: `path =^ "/org/acme/"`}
	if err := db.CreateContentSelector(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateContentSelector(ctx, s); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := db.GetContentSelector(ctx, "acme")
	if err != nil || got.Expression != s.Expression || got.CreatedAt.IsZero() {
		t.Fatalf("get: %+v %v", got, err)
	}
	s.Expression = `format == "maven2"`
	if err := db.UpdateContentSelector(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateContentSelector(ctx, ContentSelector{Name: "nope", Expression: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	// LoadAccess carries the selectors, and a role privilege keeps its repos.
	role := Role{ID: "acme-dev", Privileges: []Privilege{{Target: "selector:acme", Actions: []string{"read"}, Repos: []string{"maven-releases"}}}}
	if err := db.CreateRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	data, err := db.LoadAccess(ctx)
	if err != nil || len(data.Selectors) != 1 || data.Selectors[0].Expression != s.Expression {
		t.Fatalf("load: %+v %v", data.Selectors, err)
	}
	var stored Role
	for _, r := range data.Roles {
		if r.ID == "acme-dev" {
			stored = r
		}
	}
	if !reflect.DeepEqual(stored.Privileges, role.Privileges) {
		t.Fatalf("privileges = %+v", stored.Privileges)
	}
	var inUse *SelectorInUseError
	if err := db.DeleteContentSelector(ctx, "acme"); !errors.As(err, &inUse) || !reflect.DeepEqual(inUse.Roles, []string{"acme-dev"}) {
		t.Fatalf("delete in use: %v", err)
	}
	if err := db.DeleteRole(ctx, "acme-dev"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteContentSelector(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteContentSelector(ctx, "acme"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if errs := ValidateContentSelector(ContentSelector{Name: "x", Expression: `size == "1"`}); len(errs) != 1 || !strings.Contains(errs[0], "unknown field") {
		t.Fatalf("validate: %v", errs)
	}
}

func TestImportAccessImportsSecurity(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	cfg := &config.Config{
		RoutingRules:     []config.RoutingRule{{Name: "block-internal", Mode: "block", Matchers: []string{"^/internal/"}}},
		ContentSelectors: []config.ContentSelector{{Name: "acme", Expression: `path =^ "/org/acme/"`}},
	}
	if _, err := db.ImportAccess(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	rule, err := db.GetRoutingRule(ctx, "block-internal")
	if err != nil || rule.Mode != RoutingBlock {
		t.Fatalf("rule: %+v %v", rule, err)
	}
	if _, err := db.GetContentSelector(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	// An invalid rule fails the import of a fresh database.
	db2, _ := openTestDB(t)
	bad := &config.Config{RoutingRules: []config.RoutingRule{{Name: "x", Mode: "BLOCK", Matchers: []string{"("}}}}
	if _, err := db2.ImportAccess(ctx, bad); err == nil || !strings.Contains(err.Error(), "invalid regular expression") {
		t.Fatalf("bad import: %v", err)
	}
}
