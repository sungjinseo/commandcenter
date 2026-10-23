package npm

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// emptyAllow reports whether w carries an Allow header listing no methods
// (the resource accepts none, RFC 9110 15.5.6).
func emptyAllow(w *httptest.ResponseRecorder) bool {
	v, ok := w.Header()["Allow"]
	return ok && len(v) == 1 && v[0] == ""
}

// Final review (Task 7): every 405 lists the methods the resource really
// accepts; a GET answered with 405 is never told GET is allowed.
func TestMethodNotAllowedListsTrueMethods(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "npm-hosted", PackumentKey("left-pad"), hostedDoc)
	for _, c := range []struct {
		method, repo, path, allow string
	}{
		{"GET", "npm-hosted", "-/package/left-pad/dist-tags/latest", "PUT, DELETE"},
		{"POST", "npm-hosted", "-/package/left-pad/dist-tags/latest", "PUT, DELETE"},
		{"POST", "npm-hosted", "left-pad", "GET, HEAD, PUT"},
		{"DELETE", "npm-hosted", "left-pad", "GET, HEAD, PUT"},
		{"PUT", "npm-hosted", "left-pad/-/left-pad-1.0.1.tgz", "GET, HEAD"},
		{"PUT", "npm-hosted", "left-pad/1.0.1", "GET, HEAD"},
		{"PUT", "npm-hosted", "-/package/left-pad/dist-tags", "GET, HEAD"},
		{"PUT", "npm-proxy", "left-pad", "GET, HEAD"},
		{"POST", "npm-all", "left-pad", "GET, HEAD, PUT"},
		{"GET", "npm-hosted", "-/user/org.couchdb.user:ci", "PUT"},
		{"GET", "npm-hosted", "-/user/token/abc", "DELETE"},
	} {
		w := e.do(c.method, c.repo, c.path, `"1.0.1"`, ci)
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s/%s: %d Allow %q, want 405 and %q", c.method, c.repo, c.path, w.Code, w.Header().Get("Allow"), c.allow)
		}
	}
	// A dist-tag of a proxy accepts no method at all.
	for _, m := range []string{"GET", "PUT"} {
		if w := e.do(m, "npm-proxy", "-/package/left-pad/dist-tags/latest", `"1.0.1"`, ci); w.Code != 405 || !emptyAllow(w) {
			t.Errorf("%s proxy dist-tag: %d %v", m, w.Code, w.Header())
		}
	}
}
