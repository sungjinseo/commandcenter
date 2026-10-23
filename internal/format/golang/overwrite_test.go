package golang

import (
	"net/http/httptest"
	"strings"
	"testing"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo/repotest"
)

// Decision 2: under writePolicy allow, re-uploading an existing file needs Edit.
func TestOverwriteNeedsEdit(t *testing.T) {
	e := newEnv(t)
	e.repos["go-hosted"].WritePolicy = "allow"
	a, ps := repotest.Principals(t, e.h.Store.DB, "go-hosted", map[string][]string{
		"adder":  {meta.ActionAdd},
		"editor": {meta.ActionAdd, meta.ActionEdit},
	})
	e.h.Auth = a
	put := func(p auth.Principal, body string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("PUT", "/repository/go-hosted/x", strings.NewReader(body))
		r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		e.h.Serve(w, r, e.repos["go-hosted"], "example.com/a/@v/v1.0.0.mod")
		return w.Code
	}
	if c := put(ps["adder"], "module example.com/a\n"); c != 201 {
		t.Fatalf("add-only new: %d", c)
	}
	if c := put(ps["adder"], "module example.com/a\n\n// changed\n"); c != 403 {
		t.Fatalf("add-only overwrite: %d", c)
	}
	if c := put(ps["editor"], "module example.com/a\n\n// changed\n"); c != 201 {
		t.Fatalf("edit overwrite: %d", c)
	}
}
