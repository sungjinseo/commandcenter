package helm

import (
	"net/http/httptest"
	"testing"

	"commandcenter/internal/repo/repotest"
)

// An offline member may own a chart: a later member does not list or serve it
// in its place. One with no member after it hands nothing on: the group's index
// is what the others have, and a chart none of them has may be its own.
func TestOfflineMemberFailsClosed(t *testing.T) {
	e := newEnv(t)
	get := func(p string) func() *httptest.ResponseRecorder {
		return func() *httptest.ResponseRecorder { return e.do("GET", "helm-all", p, nil) }
	}
	for _, p := range []string{"index.yaml", "other-1.0.0.tgz"} {
		repotest.FailsOffline(t, e.repos["helm-hosted"], get(p))
	}
	e.repos["helm-proxy"].Online = new(false)
	if w := get("index.yaml")(); w.Code != 200 {
		t.Errorf("index behind an offline member: %d %.200s", w.Code, w.Body)
	}
	if w := get("other-1.0.0.tgz")(); w.Code != 503 {
		t.Errorf("a chart the offline member may own: %d %.200s", w.Code, w.Body)
	}
}
