package r

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/repo/repotest"
)

// A directory's PACKAGES decides whose a package name is in a group: what the
// remote of a member refuses, or what is no index, fails closed, and only its
// 404 and 410 leave the name to the next member. An empty index is one.
func TestOwnerDocumentFailsClosed(t *testing.T) {
	e := newEnv(t)
	rm := repotest.NewRemote(t)
	merging := repotest.GroupOf("merging", "r", "internal", "r-proxy")
	merging.MergeMembers = true
	e.repos = repotest.With(e.repos, repotest.ProxyOn("internal", "r", rm.URL+"/"), repotest.GroupOf("g", "r", "internal", "r-proxy"),
		merging, repotest.GroupOf("behind", "r", "r-proxy", "internal"))
	has := func(s string) func(*httptest.ResponseRecorder) bool {
		return func(w *httptest.ResponseRecorder) bool { return strings.Contains(w.Body.String(), s) }
	}
	repotest.FailsClosed(t, repotest.OwnerDocument{
		Remote: rm,
		Path:   "/src/contrib/PACKAGES",
		Good:   repotest.Answer{ContentType: "text/plain", Body: "Package: up\nVersion: 99.0\n"},
		Get:    func() *httptest.ResponseRecorder { return e.do("GET", "g", "src/contrib/PACKAGES", nil) },
		Ours:   has("Package: up\nVersion: 99.0"),
		Theirs: has("Package: up\nVersion: 1.0"),
		Cached: func() string {
			a, _ := e.h.Store.Get(context.Background(), "internal", "src/contrib/PACKAGES")
			return a.BlobSHA256
		},
		Expire: func() { e.now = e.now.Add(2 * time.Hour) },
	})
	get := func(group, p string) func() *httptest.ResponseRecorder {
		return func() *httptest.ResponseRecorder { return e.do("GET", group, p, nil) }
	}
	for _, p := range []string{"src/contrib/PACKAGES", "src/contrib/PACKAGES.gz", "src/contrib/PACKAGES.rds", "src/contrib/up_1.0.tar.gz"} {
		repotest.FailsOffline(t, e.repos["internal"], get("g", p))
	}
	e.repos["internal"].Online = new(false)
	for group, why := range map[string]string{"merging": "mergeMembers leaves it out", "behind": "no member after it stands in"} {
		if w := get(group, "src/contrib/PACKAGES")(); w.Code != 200 || !has("Package: up\nVersion: 1.0")(w) {
			t.Errorf("%s: %d %.200s", why, w.Code, w.Body)
		}
	}
	e.repos["internal"].Online = nil
	for i, c := range []struct {
		body string
		want int
	}{{"", 200}, {"\n\n", 200}, {"Package: a\nVersion: 1\n", 200}, {"Version: 1\nPackage: a\n", 200}, {"Version: 1\n\nPackage: a\n", 502}, {"<html>\n</html>\n", 502}} {
		p := "e/" + strconv.Itoa(i) + "/PACKAGES"
		rm.Set("/"+p, repotest.Answer{ContentType: "text/plain", Body: c.body})
		if w := e.do("GET", "internal", p, nil); w.Code != c.want {
			t.Errorf("index %q: %d, want %d", c.body, w.Code, c.want)
		}
	}
}

func TestIndexGzipIsChecked(t *testing.T) {
	e := newEnv(t)
	rm := repotest.NewRemote(t)
	e.repos = repotest.With(e.repos, repotest.ProxyOn("internal", "r", rm.URL+"/"))
	rm.Set("/src/contrib/PACKAGES.gz", repotest.Answer{ContentType: "application/x-gzip", Body: "Sign in"})
	if w := e.do("GET", "internal", "src/contrib/PACKAGES.gz", nil); w.Code != 502 {
		t.Fatalf("no gzip: %d %s", w.Code, w.Body)
	}
	rm.Set("/src/contrib/PACKAGES.gz", repotest.Answer{ContentType: "application/x-gzip", Body: string(gzipBytes([]byte("Package: a\n")))})
	if w := e.do("GET", "internal", "src/contrib/PACKAGES.gz", nil); w.Code != 200 {
		t.Fatalf("gzip: %d %s", w.Code, w.Body)
	}
}
