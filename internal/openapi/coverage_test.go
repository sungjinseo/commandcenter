package openapi_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"commandcenter/internal/nexusapi"
	"commandcenter/internal/openapi"
	_ "commandcenter/internal/ui" // registers its route descriptions
)

// notAPI are registrations that are not API routes: the web UI's static
// files and the mount point of the /-/api/ routes listed one by one.
var notAPI = map[string]bool{"GET /ui/": true, "/-/api/": true}

// registered returns every mux pattern the API packages register, read from
// their source: each HandleFunc or Handle call whose pattern is a string
// literal, or a concatenation of literals and nexusapi.Prefix (named p or
// Prefix there). A pattern built any other way fails the test, so a new
// route cannot slip past it.
func registered(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, dir := range []string{"../ui", "../nexusapi", "../server"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, name, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle" {
					return true
				}
				pat, ok := eval(call.Args[0])
				if !ok {
					t.Errorf("%s: cannot read the pattern of this route; use a literal", fset.Position(call.Pos()))
					return true
				}
				_, path := openapi.Split(pat)
				if (strings.HasPrefix(path, "/-/") || strings.HasPrefix(path, "/service/") || path == "/{$}") && !notAPI[pat] {
					out[pat] = fset.Position(call.Pos()).String()
				}
				return true
			})
		}
	}
	return out
}

func eval(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if s, err := strconv.Unquote(e.Value); err == nil && e.Kind == token.STRING {
			return s, true
		}
	case *ast.Ident:
		if e.Name == "p" || e.Name == "Prefix" {
			return nexusapi.Prefix, true
		}
	case *ast.BinaryExpr:
		a, ok1 := eval(e.X)
		b, ok2 := eval(e.Y)
		return a + b, ok1 && ok2 && e.Op == token.ADD
	}
	return "", false
}

// TestEveryRouteDocumented: the OpenAPI document describes every API route
// registered, and nothing that is not registered.
func TestEveryRouteDocumented(t *testing.T) {
	reg := registered(t)
	if len(reg) < 100 {
		t.Fatalf("found only %d routes: is the scan broken?", len(reg))
	}
	documented := map[string]bool{}
	for _, r := range openapi.Routes() {
		if documented[r.Pattern] {
			t.Errorf("%q is described twice", r.Pattern)
		}
		documented[r.Pattern] = true
		if r.Summary == "" || r.Tag == "" {
			t.Errorf("%q needs a summary and a tag", r.Pattern)
		}
		if _, ok := reg[r.Pattern]; !ok {
			t.Errorf("%q is described but not registered", r.Pattern)
		}
	}
	for pat, at := range reg {
		if !documented[pat] {
			t.Errorf("%s: route %q has no description (ui/openapi.go or nexusapi/openapi.go)", at, pat)
		}
	}
}

// TestDocument: valid JSON in the OpenAPI 3.0 shape, path parameters
// declared, {name...} rendered as {name}.
func TestDocument(t *testing.T) {
	var doc struct {
		OpenAPI string                               `json:"openapi"`
		Paths   map[string]map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(openapi.Document(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.0.3" {
		t.Fatalf("openapi %q", doc.OpenAPI)
	}
	op := doc.Paths["/-/api/repositories/{name}/packages/{pkg}"]["get"]
	if op == nil || len(op["parameters"].([]any)) != 2 {
		t.Fatalf("packages route: %v", op)
	}
	if doc.Paths[nexusapi.Prefix+"/blobstores/{name}/quota-status"]["get"] == nil || doc.Paths["/-/api/realms/rutauth"]["put"] == nil {
		t.Fatal("new routes missing")
	}
	for p, ops := range doc.Paths {
		for m, op := range ops {
			if op["responses"] == nil {
				t.Errorf("%s %s: no responses", m, p)
			}
		}
	}
}
