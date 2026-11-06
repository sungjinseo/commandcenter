package swift

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/repo/repotest"
)

// The release list decides who owns a package in a group: what the remote of
// a member refuses, or a page, or JSON that is no such list, fails closed, and
// only its 404 and 410 hand the name to the next member. A release's metadata
// keeps its copy when a remote answers garbage.
func TestOwnerDocumentFailsClosed(t *testing.T) {
	e := newEnv(t)
	rm := repotest.NewRemote(t)
	e.repos = repotest.With(e.repos, repotest.ProxyOn("internal", "swift", rm.URL+"/"), repotest.GroupOf("g", "swift", "internal", "swift-proxy"))
	has := func(s string) func(*httptest.ResponseRecorder) bool {
		return func(w *httptest.ResponseRecorder) bool { return strings.Contains(w.Body.String(), s) }
	}
	cached := func(key string) func() string {
		return func() string {
			a, _ := e.h.Store.Get(context.Background(), "internal", key)
			return a.BlobSHA256
		}
	}
	expire := func() { e.now = e.now.Add(2 * time.Hour) }
	repotest.FailsClosed(t, repotest.OwnerDocument{
		Remote: rm,
		Path:   "/mona/LinkedList",
		Good:   repotest.Answer{ContentType: ctRegistry + "+json", Body: `{"releases":{"99.0.0":{"url":"x"}}}`},
		Get:    func() *httptest.ResponseRecorder { return e.do("GET", "g", "mona/LinkedList", nil) },
		Ours:   has("99.0.0"),
		Theirs: has("1.1.1"),
		Cached: cached("mona/linkedlist/" + releasesFile),
		Expire: expire,
	})
	repotest.FailsOffline(t, e.repos["internal"], func() *httptest.ResponseRecorder { return e.do("GET", "g", "mona/LinkedList", nil) })
	repotest.KeepsCopy(t, rm, "/mona/LinkedList/99.0.0", repotest.Answer{ContentType: ctRegistry + "+json", Body: `{"id":"mona.LinkedList","version":"99.0.0","resources":[]}`},
		func() *httptest.ResponseRecorder { return e.do("GET", "internal", "mona/LinkedList/99.0.0", nil) },
		cached(versionDir("mona", "LinkedList", "99.0.0")+releaseFile), expire)
}
