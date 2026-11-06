package conan

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/repo/repotest"
)

// The latest revision decides who owns a recipe in a group: what the remote of
// a member refuses, or a page, or JSON that is no such answer, fails closed,
// and only its 404 and 410 hand the name to the next member. The other
// documents keep their copy when a remote answers garbage.
func TestOwnerDocumentFailsClosed(t *testing.T) {
	e := newEnv(t)
	rm := repotest.NewRemote(t)
	e.repos = repotest.With(e.repos, repotest.ProxyOn("internal", "conan", rm.URL+"/"), repotest.GroupOf("g", "conan", "internal", "conan-proxy"))
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
	const path = "v2/conans/zlib/1.3/_/_/latest"
	repotest.FailsClosed(t, repotest.OwnerDocument{
		Remote: rm,
		Path:   "/" + path,
		Good:   repotest.Answer{ContentType: ctJSON, Body: `{"revision":"ours","time":"2026-01-02T03:04:05.000+0000"}`},
		Get:    func() *httptest.ResponseRecorder { return e.do("GET", "g", path, nil) },
		Ours:   has("ours"),
		Theirs: has("aaaa"),
		Cached: cached(path),
		Expire: expire,
	})
	repotest.FailsOffline(t, e.repos["internal"], func() *httptest.ResponseRecorder { return e.do("GET", "g", path, nil) })
	const revisions = "v2/conans/zlib/1.3/_/_/revisions"
	repotest.KeepsCopy(t, rm, "/"+revisions, repotest.Answer{ContentType: ctJSON, Body: `{"revisions":[]}`},
		func() *httptest.ResponseRecorder { return e.do("GET", "internal", revisions, nil) },
		cached(revisions), expire)
}
