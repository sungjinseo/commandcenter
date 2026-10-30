package repo

import (
	"context"
	"net/http"
)

// InvalidateCache makes the cached content of the named proxy stale, even
// content that never expires, and forgets its recorded misses (Nexus
// "invalidate cache"): the next request for each asset asks the remote. A
// group does so for every proxy it reaches, nested groups included. It
// returns the proxies it invalidated. A hosted repository is a 400, an
// unknown one a 404.
func (r *Registry) InvalidateCache(ctx context.Context, name string) ([]string, error) {
	if err := r.store(); err != nil {
		return nil, err
	}
	rp := r.Lookup(ctx, name)
	switch {
	case rp == nil:
		return nil, Errorf(http.StatusNotFound, "repository %q not found", name)
	case rp.Type != "proxy" && rp.Type != "group":
		return nil, Errorf(http.StatusBadRequest, "repository %q is not a proxy or group repository: it has no cache", name)
	}
	var proxies []string
	seen := map[string]bool{}
	var walk func(*Repository)
	walk = func(rp *Repository) {
		if rp == nil || seen[rp.Name] {
			return
		}
		seen[rp.Name] = true
		if rp.Type == "proxy" {
			proxies = append(proxies, rp.Name)
		}
		for _, m := range rp.MemberRepos {
			walk(m)
		}
	}
	walk(rp)
	return proxies, r.db.InvalidateCache(ctx, proxies)
}
