package conda

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/repo/repotest"
)

// A channel's repodata.json and channeldata.json decide whose a package name
// is in a group: what the remote of a member refuses, or what is no such
// document, fails closed, and only its 404 and 410 leave the name to the next
// member.
func TestOwnerDocumentFailsClosed(t *testing.T) {
	e := newEnv(t)
	rm := repotest.NewRemote(t)
	merging := repotest.GroupOf("merging", "conda", "internal", "conda-proxy")
	merging.MergeMembers = true
	e.repos = repotest.With(e.repos, repotest.ProxyOn("internal", "conda", rm.URL+"/"), repotest.GroupOf("g", "conda", "internal", "conda-proxy"),
		merging, repotest.GroupOf("behind", "conda", "conda-proxy", "internal"))
	has := func(s string) func(*httptest.ResponseRecorder) bool {
		return func(w *httptest.ResponseRecorder) bool { return strings.Contains(w.Body.String(), s) }
	}
	expire := func() { e.now = e.now.Add(2 * time.Hour) }
	repotest.FailsClosed(t, repotest.OwnerDocument{
		Remote: rm,
		Path:   "/linux-64/repodata.json",
		Good:   repotest.Answer{ContentType: ctJSON, Body: `{"info":{"subdir":"linux-64"},"packages":{},"packages.conda":{"up-9.0-0.conda":{"name":"up","version":"9.0"}},"repodata_version":1}`},
		Get:    func() *httptest.ResponseRecorder { return e.do("GET", "g", "linux-64/repodata.json", nil) },
		Ours:   has("up-9.0-0.conda"),
		Theirs: has("up-2.0-0.conda"),
		Cached: func() string {
			a, _ := e.h.Store.Get(context.Background(), "internal", "linux-64/repodata.json")
			return a.BlobSHA256
		},
		Expire: expire,
	})
	get := func(group, p string) func() *httptest.ResponseRecorder {
		return func() *httptest.ResponseRecorder { return e.do("GET", group, p, nil) }
	}
	for _, p := range []string{"linux-64/repodata.json", "linux-64/current_repodata.json", "channeldata.json", "linux-64/up-1.0-0.tar.bz2"} {
		repotest.FailsOffline(t, e.repos["internal"], get("g", p))
	}
	e.repos["internal"].Online = new(false)
	for group, why := range map[string]string{"merging": "mergeMembers leaves it out", "behind": "no member after it stands in"} {
		if w := get(group, "linux-64/repodata.json")(); w.Code != 200 || !has("up-1.0-0.tar.bz2")(w) {
			t.Errorf("%s: %d %.200s", why, w.Code, w.Body)
		}
	}
	e.repos["internal"].Online = nil
	// The member's names of all subdirs: refused, nobody can tell whose a name is.
	for _, a := range repotest.Refusals {
		e2 := newEnv(t)
		rm2 := repotest.NewRemote(t)
		e2.repos = repotest.With(e2.repos, repotest.ProxyOn("internal", "conda", rm2.URL+"/"), repotest.GroupOf("g", "conda", "internal", "conda-proxy"))
		rm2.Set("/channeldata.json", a)
		if w := e2.do("GET", "g", "linux-64/repodata.json", nil); w.Code < 500 {
			t.Errorf("channeldata.json %d %.30q: %d %.200s, want a 5xx", a.Status, a.Body, w.Code, w.Body)
		}
	}
}
