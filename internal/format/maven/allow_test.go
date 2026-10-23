package maven

import (
	"net/http"
	"testing"
)

// Final review (Task 7): a Maven 405 lists the methods the repository accepts.
func TestMethodNotAllowedListsTrueMethods(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, repo, allow string }{
		{"POST", "releases", "GET, HEAD, PUT, DELETE"},
		{"POST", "public", "GET, HEAD, PUT, DELETE"},
		{"POST", "central", "GET, HEAD"},
		{"PUT", "central", "GET, HEAD"},
		{"DELETE", "readonly-group", "GET, HEAD"},
	} {
		w := e.do(c.method, c.repo, "org/x/1.0/x-1.0.jar", "x")
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s: %d Allow %q, want 405 and %q", c.method, c.repo, w.Code, w.Header().Get("Allow"), c.allow)
		}
	}
}
