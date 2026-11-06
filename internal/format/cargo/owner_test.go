package cargo

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/repo/repotest"
)

// The index file decides who owns a crate in a group: what the remote of a
// member refuses, or what is no index file, fails closed, and only its 404 and
// 410 hand the name to the next member, as they do for an empty file.
func TestOwnerDocumentFailsClosed(t *testing.T) {
	e := newEnv(t)
	rm := repotest.NewRemote(t)
	e.repos = repotest.With(e.repos, repotest.ProxyOn("internal", "cargo", rm.URL+"/"), repotest.GroupOf("g", "cargo", "internal", "cargo-proxy"))
	has := func(s string) func(*httptest.ResponseRecorder) bool {
		return func(w *httptest.ResponseRecorder) bool { return strings.Contains(w.Body.String(), s) }
	}
	shift := time.Duration(0)
	expire := func() {
		shift += 2 * time.Hour
		e.h.Fetcher.Now = func() time.Time { return time.Now().Add(shift) }
	}
	repotest.FailsClosed(t, repotest.OwnerDocument{
		Remote: rm,
		Path:   "/it/oa/itoa",
		Good:   repotest.Answer{ContentType: "text/plain", Body: `{"name":"itoa","vers":"99.0.0","deps":[],"cksum":"` + strings.Repeat("0", 64) + `","features":{},"yanked":false}` + "\n"},
		Get:    func() *httptest.ResponseRecorder { return e.do("GET", "g", "it/oa/itoa", nil) },
		Ours:   has("99.0.0"),
		Theirs: has("1.0.0"),
		Cached: func() string {
			a, _ := e.h.Store.Get(context.Background(), "internal", indexKey("it/oa/itoa"))
			return a.BlobSHA256
		},
		Expire: expire,
	})
	for _, p := range []string{"it/oa/itoa", "crates/itoa/1.0.0/download"} {
		repotest.FailsOffline(t, e.repos["internal"], func() *httptest.ResponseRecorder { return e.do("GET", "g", p, nil) })
	}
	rm.Set("/ze/ro/zero", repotest.Answer{ContentType: "text/plain"})
	if w := e.do("GET", "internal", "ze/ro/zero", nil); w.Code != 404 {
		t.Fatalf("an empty index file: %d %s", w.Code, w.Body)
	}
}
