package npm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

// Final review I1 (QA M-4): an npm group fails closed when a member ordered
// before the first member that answered errors, so a transient error of the
// internal member never serves the proxy's public latest or tarball.
func TestGroupFailsClosedWhenEarlierMemberErrors(t *testing.T) {
	e := newEnv(t)
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
