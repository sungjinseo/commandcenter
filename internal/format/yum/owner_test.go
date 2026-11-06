package yum

import (
	"net/http/httptest"
	"testing"

	"commandcenter/internal/repo/repotest"
)

// Without mergeMembers a group answers a path from the first member that has
// it, so what it lists is that member's: an offline member does not hand its
// paths to a later one. A merging group leaves it out, and one with no member
// after it hands nothing on.
func TestOfflineMemberFailsClosed(t *testing.T) {
	e := newEnv(t)
	get := func(group, p string) func() *httptest.ResponseRecorder {
		return func() *httptest.ResponseRecorder { return e.do("GET", group, p, nil) }
	}
	for _, p := range []string{"el9/repodata/repomd.xml", "el9/Packages/u/up-1.0-1.el9.noarch.rpm"} {
		repotest.FailsOffline(t, e.repos["yum-hosted"], get("yum-all", p))
	}
	e.repos["yum-hosted"].Online = new(false)
	if w := get("yum-merged", "el9/repodata/repomd.xml")(); w.Code != 200 {
		t.Errorf("merging: %d %.200s", w.Code, w.Body)
	}
	e.repos["yum-hosted"].Online = nil
	e.repos["yum-proxy"].Online = new(false)
	if w := get("yum-all", "el9/repodata/repomd.xml")(); w.Code != 404 {
		t.Errorf("no member after it: %d %.200s", w.Code, w.Body)
	}
}
