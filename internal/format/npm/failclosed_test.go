package npm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/repo"
)

// brokenMember is an npm proxy whose upstream answers 500 for everything:
// it stands for a member that errors (not "not found").
func brokenMember(t *testing.T, e *env) *repo.Repository {
	t.Helper()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }))
	t.Cleanup(bad.Close)
	def := e.repos["npm-proxy"].Repository
	def.Name, def.Remote = "npm-broken", bad.URL
	return repo.Build(&config.Config{Repositories: []config.Repository{def}})["npm-broken"]
}

// groupOf is npm-all with the given members in order.
func groupOf(e *env, name string, members ...*repo.Repository) *repo.Repository {
	g := *e.repos["npm-all"]
	g.Name = name
	g.MemberRepos = members
	e.repos[name] = &g
	return &g
}

// QA N-1: an offline member ahead of the one that has the package fails the
// request (503) instead of letting the proxy's same-named package through.
// While a member is offline, a name no online member has is not a 404 either:
// the offline member might own it.
func TestGroupFailsClosedWhenEarlierMemberOffline(t *testing.T) {
	e := newEnv(t)
	cacheProxyTarball(t, e)
	off := false
	e.repos["npm-hosted"].Online = &off // npm-all holds the same pointer
	for _, p := range []string{"left-pad", "left-pad/latest", "-/package/left-pad/dist-tags", "left-pad/-/left-pad-1.1.0.tgz"} {
		if w := e.do("GET", "npm-all", p, "", anon); w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "remote-tgz") {
			t.Errorf("%s: %d %q, want 503", p, w.Code, w.Body.String())
		}
	}

	// An offline member behind the owner is never consulted.
	on := true
	e.repos["npm-hosted"].Online = &on
	e.repos["npm-proxy"].Online = &off
	e.seed(t, "npm-hosted", PackumentKey("left-pad"), hostedDoc)
	if w := e.do("GET", "npm-all", "left-pad", "", anon); w.Code != 200 || get(jsonBody(t, w), "dist-tags", "latest") != "1.0.1" {
		t.Fatalf("owner ahead of the offline member: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "npm-all", "nope", "", anon); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("name only an offline member could have: %d %s", w.Code, w.Body.String())
	}
}

// Final review I1 (QA M-4): an npm group fails closed when a member ordered
// before the first member that answered errors, so a transient error of the
// internal member never serves the proxy's public latest or tarball.
func TestGroupFailsClosedWhenEarlierMemberErrors(t *testing.T) {
	e := newEnv(t)
	cacheProxyTarball(t, e)
	broken := brokenMember(t, e)
	groupOf(e, "g-broken-first", broken, e.repos["npm-proxy"])
	for _, p := range []string{"left-pad", "left-pad/latest", "-/package/left-pad/dist-tags", "left-pad/-/left-pad-1.1.0.tgz"} {
		w := e.do("GET", "g-broken-first", p, "", anon)
		if w.Code < 500 || strings.Contains(w.Body.String(), "remote-tgz") || strings.Contains(w.Body.String(), "1.1.0") {
			t.Errorf("%s: %d %q, want 5xx", p, w.Code, w.Body.String())
		}
	}
}

// A member that fails AFTER one that answered is skipped: a proxy outage
// behind the hosted member still serves the hosted document.
func TestGroupServesEarlierMemberWhenLaterMemberErrors(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "npm-hosted", PackumentKey("left-pad"), hostedDoc)
	e.seed(t, "npm-hosted", TarballKey("left-pad", "left-pad-1.0.1.tgz"), "hosted-tgz")
	groupOf(e, "g-broken-last", e.repos["npm-hosted"], brokenMember(t, e))
	w := e.do("GET", "g-broken-last", "left-pad", "", anon)
	if w.Code != 200 {
		t.Fatalf("packument: %d %s", w.Code, w.Body.String())
	}
	if m := jsonBody(t, w); get(m, "dist-tags", "latest") != "1.0.1" || get(m, "versions", "1.0.1", "from") != "hosted" {
		t.Fatalf("packument = %v", m)
	}
	if w := e.do("GET", "g-broken-last", "left-pad/-/left-pad-1.0.1.tgz", "", anon); w.Code != 200 || w.Body.String() != "hosted-tgz" {
		t.Fatalf("tarball: %d %q", w.Code, w.Body.String())
	}
}

// Not found (hosted 404) is never an error: the proxy's document is served.
func TestGroupFallsThroughOnEarlierMemberNotFound(t *testing.T) {
	e := newEnv(t)
	w := e.do("GET", "npm-all", "left-pad", "", anon)
	if w.Code != 200 || get(jsonBody(t, w), "dist-tags", "latest") != "1.1.0" {
		t.Fatalf("packument: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "npm-all", "left-pad/-/left-pad-1.1.0.tgz", "", anon); w.Code != 200 || w.Body.String() != "remote-tgz" {
		t.Fatalf("tarball: %d %q", w.Code, w.Body.String())
	}
}

const tgz110 = "left-pad/-/left-pad-1.1.0.tgz"

// cacheProxyTarball has npm-proxy fetch and cache left-pad 1.1.0, so a group
// test shows that the later member's cached tarball does not get through.
func cacheProxyTarball(t *testing.T, e *env) {
	t.Helper()
	if w := e.do("GET", "npm-proxy", tgz110, "", anon); w.Code != 200 {
		t.Fatalf("caching the proxy's tarball: %d %s", w.Code, w.Body.String())
	}
}

// fakeUpstream is a registry a test can switch: while status is 0 it serves
// its files (path to body), otherwise it answers everything with status
// (503: down, 404: the package was removed). docs counts requests for the
// left-pad packument.
type fakeUpstream struct{ status, docs atomic.Int32 }

// upstreamMember is an npm proxy member named name, with npm-proxy's
// settings, on a fakeUpstream serving files.
func upstreamMember(t *testing.T, e *env, name string, files map[string]string) (*repo.Repository, *fakeUpstream) {
	t.Helper()
	u := &fakeUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/left-pad" {
			u.docs.Add(1)
		}
		body, ok := files[r.URL.Path]
		switch s := int(u.status.Load()); {
		case s != 0:
			http.Error(w, "switched off", s)
		case !ok:
			http.NotFound(w, r)
		default:
			io.WriteString(w, body)
		}
	}))
	t.Cleanup(srv.Close)
	def := e.repos["npm-proxy"].Repository
	def.Name, def.Remote = name, srv.URL
	m := repo.Build(&config.Config{Repositories: []config.Repository{def}})[name]
	e.repos[name] = m
	return m, u
}

// upEnv is newEnv plus g-up = npm-hosted, npm-up, where npm-up serves left-pad
// with 1.1.0 as "up-tgz".
func upEnv(t *testing.T) (*env, *fakeUpstream) {
	t.Helper()
	e := newEnv(t)
	up, u := upstreamMember(t, e, "npm-up", map[string]string{"/left-pad": leftPad, "/" + tgz110: "up-tgz"})
	groupOf(e, "g-up", e.repos["npm-hosted"], up)
	return e, u
}

// Format review F1: npm ci asks only for tarballs. On a cold cache a group
// tarball fetches the tarball, not the packument first.
func TestGroupTarballColdCacheFetchesNoPackument(t *testing.T) {
	e, u := upEnv(t)
	if w := e.do("GET", "g-up", tgz110, "", anon); w.Code != 200 || w.Body.String() != "up-tgz" {
		t.Fatalf("tarball: %d %q", w.Code, w.Body.String())
	}
	if n := u.docs.Load(); n != 0 {
		t.Fatalf("the packument was fetched %d times for a tarball", n)
	}
}

// A tarball the proxy has cached, here without its packument as npm ci or an
// older build leaves it, is served through the group while the upstream is
// down, as by the proxy URL.
func TestGroupTarballServedWhileUpstreamDown(t *testing.T) {
	e, u := upEnv(t)
	if w := e.do("GET", "npm-up", tgz110, "", anon); w.Code != 200 {
		t.Fatalf("caching: %d %s", w.Code, w.Body.String())
	}
	u.status.Store(http.StatusServiceUnavailable)
	if w := e.do("GET", "g-up", tgz110, "", anon); w.Code != 200 || w.Body.String() != "up-tgz" {
		t.Fatalf("upstream down: %d %q", w.Code, w.Body.String())
	}
}

// ... and after the upstream removed the package, once the cached packument
// has expired.
func TestGroupTarballServedAfterUpstreamRemovedPackage(t *testing.T) {
	e, u := upEnv(t)
	for _, p := range []string{"left-pad", tgz110} {
		if w := e.do("GET", "g-up", p, "", anon); w.Code != 200 {
			t.Fatalf("caching %s: %d %s", p, w.Code, w.Body.String())
		}
	}
	u.status.Store(http.StatusNotFound)
	e.h.Resolver.Fetcher.Now = func() time.Time { return time.Now().Add(2 * time.Hour) } // past metadataMaxAge
	if w := e.do("GET", "g-up", tgz110, "", anon); w.Code != 200 || w.Body.String() != "up-tgz" {
		t.Fatalf("package removed upstream: %d %q", w.Code, w.Body.String())
	}
}

// The owner rule holds for a tarball cached in the later member: hosted owns
// left-pad once it has 1.0.0, so the proxy's cached 1.1.0 is a 404 through
// the group, also while the upstream is down, and hosted offline is a 503.
func TestGroupTarballOwnerRuleHoldsForCachedTarballs(t *testing.T) {
	e, u := upEnv(t)
	if w := e.do("GET", "npm-up", tgz110, "", anon); w.Code != 200 {
		t.Fatalf("caching: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("PUT", "npm-hosted", "left-pad", publishBody("left-pad", "1.0.0", []byte("hosted-tgz"), false), ci); w.Code != 201 {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	u.status.Store(http.StatusServiceUnavailable)
	if w := e.do("GET", "g-up", tgz110, "", anon); w.Code != 404 {
		t.Fatalf("proxy tarball of a hosted package: %d %q, want 404", w.Code, w.Body.String())
	}
	if w := e.do("GET", "g-up", "left-pad/-/left-pad-1.0.0.tgz", "", anon); w.Code != 200 || w.Body.String() != string(testTarball("left-pad", "1.0.0", []byte("hosted-tgz"))) {
		t.Fatalf("hosted tarball: %d %q", w.Code, w.Body.String())
	}
	off := false
	e.repos["npm-hosted"].Online = &off
	if w := e.do("GET", "g-up", tgz110, "", anon); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("hosted offline: %d %q, want 503", w.Code, w.Body.String())
	}
}

// A proxy member that is not last answers for a tarball it has, from its
// cache while its upstream is down. One that has the package but not that
// tarball owns the name (404, not the last member's tarball); one without
// the package passes the request on.
func TestGroupTarballFromProxyThatIsNotLast(t *testing.T) {
	e := newEnv(t)
	has, u := upstreamMember(t, e, "npm-has", map[string]string{"/left-pad": leftPad, "/" + tgz110: "has-tgz"})
	owns, _ := upstreamMember(t, e, "npm-owns", map[string]string{"/left-pad": leftPad})
	lacks, _ := upstreamMember(t, e, "npm-lacks", nil)
	for _, c := range []struct {
		m    *repo.Repository
		code int
		body string
	}{{has, 200, "has-tgz"}, {owns, 404, ""}, {lacks, 200, "remote-tgz"}} {
		g := groupOf(e, "g-"+c.m.Name, e.repos["npm-hosted"], c.m, e.repos["npm-proxy"])
		if w := e.do("GET", g.Name, tgz110, "", anon); w.Code != c.code || c.body != "" && w.Body.String() != c.body {
			t.Errorf("%s: %d %q, want %d %q", g.Name, w.Code, w.Body.String(), c.code, c.body)
		}
	}
	u.status.Store(http.StatusServiceUnavailable)
	if w := e.do("GET", "g-npm-has", tgz110, "", anon); w.Code != 200 || w.Body.String() != "has-tgz" {
		t.Fatalf("cached in npm-has, upstream down: %d %q", w.Code, w.Body.String())
	}
}
