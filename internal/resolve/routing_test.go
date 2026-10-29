package resolve

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// A group member whose routing rule blocks the path is skipped like a member
// without the path, and the next member answers.
func TestGroupSkipsMemberBlockedByRoutingRule(t *testing.T) {
	hits := 0
	r, repos := setup(t, func(w http.ResponseWriter, req *http.Request) {
		hits++
		w.Write([]byte("from-remote"))
	})
	rt, _ := repo.CompileRoute(meta.RoutingRule{Name: "only-org", Mode: meta.RoutingAllow, Matchers: []string{`^/org/`}})
	r.Fetcher.RouteAllowed = func(rp *repo.Repository, path string) bool {
		if rp.RoutingRule == "" {
			return true
		}
		ok, _ := rt.Allowed(path)
		return ok
	}
	repos["p"].RoutingRule = "only-org"
	put(t, r, "h2", "com/x", "from-h2") // g = h1, inner(h2, p), h2
	res, err := r.Lookup(ctx, repos["g"], "com/x", proxy.Content)
	if err != nil || content(t, r, res.Asset) != "from-h2" {
		t.Fatalf("got %+v %v", res, err)
	}
	// Only the proxy could answer for a blocked path: not found, remote never asked.
	if _, err := r.Lookup(ctx, repos["g"], "com/only-remote", proxy.Content); repo.StatusOf(err) != 404 || hits != 0 {
		t.Fatalf("err = %v, hits = %d", err, hits)
	}
	// An allowed path reaches the remote through the group.
	if res, err := r.Lookup(ctx, repos["g"], "org/lib", proxy.Content); err != nil || content(t, r, res.Asset) != "from-remote" || hits != 1 {
		t.Fatalf("got %v %v hits=%d", res, err, hits)
	}
	_ = httptest.NewRecorder
}
