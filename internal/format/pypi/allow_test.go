package pypi

import (
	"net/http"
	"testing"
)

// Final review (Task 7): a PyPI 405 lists the methods the resource accepts.
func TestMethodNotAllowedListsTrueMethods(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, repo, path, allow string }{
		{"GET", "py-hosted", "", "POST"},
		{"PUT", "py-all", "", "POST"},
		{"PUT", "py-hosted", "simple/", "GET, HEAD"},
		{"POST", "py-hosted", "simple/x/", "GET, HEAD"},
		{"DELETE", "py-proxy", "packages/x/x-1.0.tar.gz", "GET, HEAD"},
	} {
		w := e.do(c.method, c.repo, c.path, "")
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s/%s: %d Allow %q, want 405 and %q", c.method, c.repo, c.path, w.Code, w.Header().Get("Allow"), c.allow)
		}
	}
	// The upload endpoint of a proxy accepts no method.
	for _, m := range []string{"GET", "POST"} {
		w := e.do(m, "py-proxy", "", "")
		if v, ok := w.Header()["Allow"]; w.Code != 405 || !ok || len(v) != 1 || v[0] != "" {
			t.Errorf("%s proxy upload: %d %v", m, w.Code, w.Header())
		}
	}
}
