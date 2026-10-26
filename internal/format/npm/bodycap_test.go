package npm

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/iotest"

	"commandcenter/internal/auth"
)

// doCapped sends a request whose body is capped like server.MaxUpload does.
func (e *env) doCapped(method, repoName, path, body string, limit int64, p auth.Principal) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repoName+"/"+path, nil)
	r.Body = http.MaxBytesReader(w, io.NopCloser(strings.NewReader(body)), limit)
	r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

func TestLoginOverGlobalCapIs413(t *testing.T) {
	e := newEnv(t)
	body := `{"name":"ci","password":"` + strings.Repeat("x", 500) + `"}`
	if w := e.doCapped("PUT", "npm-all", "-/user/org.couchdb.user:ci", body, 50, anon); w.Code != 413 {
		t.Fatalf("status %d %s, want 413", w.Code, w.Body.String())
	}
}

func TestDistTagOverGlobalCapIs413(t *testing.T) {
	e := newEnv(t)
	body := `"` + strings.Repeat("1", 500) + `"`
	if w := e.doCapped("PUT", "npm-all", "-/package/left-pad/dist-tags/beta", body, 50, ci); w.Code != 413 {
		t.Fatalf("status %d %s, want 413", w.Code, w.Body.String())
	}
}

// QA N-8: a body the server's IdleBody cut for stalling is a 408, not a 400.
func TestStalledBodyIs408(t *testing.T) {
	e := newEnv(t)
	for path, p := range map[string]auth.Principal{"-/user/org.couchdb.user:ci": anon, "-/package/left-pad/dist-tags/beta": ci} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("PUT", "/repository/npm-all/"+path, nil)
		r.Body = io.NopCloser(iotest.ErrReader(&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}))
		r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		e.h.Serve(w, r, e.repos["npm-all"], path)
		if w.Code != 408 {
			t.Errorf("%s: status %d %s, want 408", path, w.Code, w.Body.String())
		}
	}
}
