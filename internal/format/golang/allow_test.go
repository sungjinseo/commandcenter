package golang

import (
	"net/http"
	"strings"
	"testing"
)

// Final review (Task 7): a Go 405 lists the methods the resource accepts.
func TestMethodNotAllowedListsTrueMethods(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, repo, path, allow string }{
		{"POST", "go-hosted", "example.com/m/@v/v1.0.0.mod", "GET, HEAD, PUT"},
		{"DELETE", "go-all", "example.com/m/@v/v1.0.0.zip", "GET, HEAD, PUT"},
		{"POST", "go-proxy", "example.com/m/@v/v1.0.0.mod", "GET, HEAD"},
		{"POST", "go-hosted", "example.com/m/@v/list", "GET, HEAD"},
		{"PUT", "go-hosted", "example.com/m/@v/list", "GET, HEAD"},
		{"PUT", "go-proxy", "example.com/m/@v/v1.0.0.mod", "GET, HEAD"},
	} {
		w := e.do(c.method, c.repo, c.path, strings.NewReader("x"))
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s/%s: %d Allow %q, want 405 and %q", c.method, c.repo, c.path, w.Code, w.Header().Get("Allow"), c.allow)
		}
	}
}
