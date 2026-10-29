package proxy

import (
	"errors"
	"testing"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// A routing rule refuses the path before the cache and the remote are
// consulted: no remote request, and a cached copy is not served either.
func TestFetchRoutingRuleBlocksBeforeRemoteAndCache(t *testing.T) {
	rm := &remote{}
	f, rp, c := setup(t, rm)
	rp.RoutingRule = "no-internal"
	rt, _ := repo.CompileRoute(meta.RoutingRule{Name: "no-internal", Mode: meta.RoutingBlock, Matchers: []string{`^/internal/`}})
	f.RouteAllowed = func(r *repo.Repository, path string) bool {
		if r.RoutingRule == "" {
			return true
		}
		ok, _ := rt.Allowed(path)
		return ok
	}
	if _, err := f.Fetch(ctx, rp, "internal/secret.jar", Content); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("blocked fetch: %v", err)
	}
	if rm.hits.Load() != 0 {
		t.Fatalf("remote was asked %d times", rm.hits.Load())
	}
	if _, err := f.Fetch(ctx, rp, "org/a.jar", Content); err != nil || rm.hits.Load() != 1 {
		t.Fatalf("allowed fetch: %v hits=%d", err, rm.hits.Load())
	}
	// Cached, then blocked by a rule added later: still refused, no remote request.
	if _, err := f.Fetch(ctx, rp, "org/cached.jar", Content); err != nil {
		t.Fatal(err)
	}
	_ = c
	f.RouteAllowed = func(*repo.Repository, string) bool { return false }
	hits := rm.hits.Load()
	if _, err := f.Fetch(ctx, rp, "org/cached.jar", Content); !errors.Is(err, repo.ErrNotFound) || rm.hits.Load() != hits {
		t.Fatalf("cached but blocked: %v hits=%d", err, rm.hits.Load()-hits)
	}
	// The path a rule sees: the remote path, or an absolute URL's path.
	for _, c := range []struct {
		t    Target
		want string
	}{
		{Target{Key: "k", Remote: "org/a.jar"}, "/org/a.jar"},
		{Target{Key: "k", Remote: "/org/a.jar"}, "/org/a.jar"},
		{Target{Key: "k", URL: "https://cdn.example/files/x.tgz?sig=1"}, "/files/x.tgz"},
	} {
		if got := routePath(c.t); got != c.want {
			t.Errorf("routePath(%+v) = %q, want %q", c.t, got, c.want)
		}
	}
	_ = time.Second
}
