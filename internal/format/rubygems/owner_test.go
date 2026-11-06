package rubygems

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/repo/repotest"
)

// The info file decides who owns a gem in a group: what the remote of a
// member refuses, or what is no info file, fails closed, and only its 404 and
// 410 hand the name to the next member.
func TestOwnerDocumentFailsClosed(t *testing.T) {
	e := newEnv(t)
	rm := repotest.NewRemote(t)
	e.repos = repotest.With(e.repos, repotest.ProxyOn("internal", "rubygems", rm.URL+"/"), repotest.GroupOf("g", "rubygems", "internal", "gems-proxy"))
	has := func(s string) func(*httptest.ResponseRecorder) bool {
		return func(w *httptest.ResponseRecorder) bool { return strings.Contains(w.Body.String(), s) }
	}
	repotest.FailsClosed(t, repotest.OwnerDocument{
		Remote: rm,
		Path:   "/info/rack",
		Good:   repotest.Answer{ContentType: "text/plain", Body: "---\n99.0.0 |checksum:dd\n"},
		Get:    func() *httptest.ResponseRecorder { return e.do("GET", "g", "info/rack", nil) },
		Ours:   has("99.0.0"),
		Theirs: has("3.0.0"),
		Cached: func() string {
			a, _ := e.h.Store.Get(context.Background(), "internal", "info/rack")
			return a.BlobSHA256
		},
		Expire: func() { e.now = e.now.Add(2 * time.Hour) },
	})
	repotest.FailsOffline(t, e.repos["internal"], func() *httptest.ResponseRecorder { return e.do("GET", "g", "info/rack", nil) })
}
