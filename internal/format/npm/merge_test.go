package npm

import (
	"net/http"
	"testing"
)

// mergedEnv is newEnv with npm-merged = npm-hosted, npm-proxy and
// mergeMembers, and left-pad 1.0.0-acme.1 published to npm-hosted (the
// proxy's left-pad has 1.0.0, 1.1.0 and a beta tag).
func mergedEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	groupOf(e, "npm-merged", e.repos["npm-hosted"], e.repos["npm-proxy"]).MergeMembers = true
	if w := e.do("PUT", "npm-hosted", "left-pad", publishBody("left-pad", "1.0.0-acme.1", []byte("hosted-tgz"), false), ci); w.Code != 201 {
		t.Fatalf("setup publish: %d %s", w.Code, w.Body.String())
	}
	return e
}

// npm-04: a group with mergeMembers answers a package from every member
// that has it, as a Nexus group does: the versions of all, a version and a
// tag from the first member that has them, latest the newest of the
// members', tarball URLs of the group, and a tarball from the first member
// that has the file. Without the setting the first member answers alone
// (TestGroupOwnerAnswersAlone).
func TestGroupMergesMembers(t *testing.T) {
	e := mergedEnv(t)
	w := e.do("GET", "npm-merged", "left-pad", "", anon)
	m := jsonBody(t, w)
	vs, _ := get(m, "versions").(map[string]any)
	if w.Code != 200 || len(vs) != 3 || vs["1.0.0-acme.1"] == nil || vs["1.0.0"] == nil || vs["1.1.0"] == nil {
		t.Fatalf("merged versions: %d %v", w.Code, vs)
	}
	if tags, _ := get(m, "dist-tags").(map[string]any); len(tags) != 2 || tags["latest"] != "1.1.0" || tags["beta"] != "2.0.0-beta.1" {
		t.Fatalf("merged dist-tags = %v", get(m, "dist-tags"))
	}
	for v, file := range map[string]string{"1.0.0-acme.1": "left-pad-1.0.0-acme.1.tgz", "1.1.0": "left-pad-1.1.0.tgz"} {
		if got := get(m, "versions", v, "dist", "tarball"); got != base+"/repository/npm-merged/left-pad/-/"+file {
			t.Errorf("tarball of %s = %v", v, got)
		}
	}
	if get(m, "_rev") != nil || get(m, "name") != "left-pad" || get(m, "time", "1.0.0") != "2020-01-01T00:00:00.000Z" || get(m, "time", "1.0.0-acme.1") == nil {
		t.Fatalf("merged document: _rev %v, name %v, time %v", get(m, "_rev"), get(m, "name"), get(m, "time"))
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control of a document with a hosted part = %q", cc)
	}

	tags := jsonBody(t, e.do("GET", "npm-merged", "-/package/left-pad/dist-tags", "", anon))
	if len(tags) != 2 || tags["latest"] != "1.1.0" || tags["beta"] != "2.0.0-beta.1" {
		t.Fatalf("dist-tags = %v", tags)
	}
	for path, version := range map[string]string{"left-pad/latest": "1.1.0", "left-pad/1.1.0": "1.1.0", "left-pad/1.0.0-acme.1": "1.0.0-acme.1"} {
		v := jsonBody(t, e.do("GET", "npm-merged", path, "", anon))
		if v["version"] != version {
			t.Errorf("%s = %v", path, v)
		}
	}
	if w := e.do("GET", "npm-merged", "left-pad/9.9.9", "", anon); w.Code != 404 {
		t.Errorf("a version no member has: %d", w.Code)
	}
	if w := e.do("HEAD", "npm-merged", "left-pad/1.1.0", "", anon); w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD of a proxy version: %d %d", w.Code, w.Body.Len())
	}
	if w := e.do("HEAD", "npm-merged", "left-pad", "", anon); w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD: %d %d", w.Code, w.Body.Len())
	}
	if w := e.do("GET", "npm-merged", "left-pad/-/left-pad-1.1.0.tgz", "", anon); w.Code != 200 || w.Body.String() != remoteTgz {
		t.Fatalf("the proxy's tarball through the group: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("GET", "npm-merged", "left-pad/-/left-pad-1.0.0-acme.1.tgz", "", anon); w.Code != 200 || w.Body.String() != string(testTarball("left-pad", "1.0.0-acme.1", []byte("hosted-tgz"))) {
		t.Fatalf("the hosted tarball through the group: %d", w.Code)
	}
	// A package only one member has is that member's document, rewritten.
	if w := e.do("GET", "npm-merged", "nope", "", anon); w.Code != 404 {
		t.Errorf("a package no member has: %d", w.Code)
	}
	// The same members without the setting: the hosted member answers alone.
	m = jsonBody(t, e.do("GET", "npm-all", "left-pad", "", anon))
	if vs, _ := get(m, "versions").(map[string]any); len(vs) != 1 || get(m, "dist-tags", "latest") != "1.0.0-acme.1" {
		t.Fatalf("npm-all (no mergeMembers) = %v", m)
	}
}

// A merging group leaves out a member that is offline or fails, as Nexus
// does, while another member answers; when none does, the failure shows.
func TestGroupMergeSkipsFailingMembers(t *testing.T) {
	e := mergedEnv(t)
	g := groupOf(e, "g-merged-broken", brokenMember(t, e), e.repos["npm-hosted"], e.repos["npm-proxy"])
	g.MergeMembers = true
	if m := jsonBody(t, e.do("GET", g.Name, "left-pad", "", anon)); get(m, "versions", "1.1.0") == nil || get(m, "versions", "1.0.0-acme.1") == nil {
		t.Fatalf("with a failing first member: %v", m)
	}
	if w := e.do("GET", g.Name, "left-pad/-/left-pad-1.1.0.tgz", "", anon); w.Code != 200 {
		t.Fatalf("tarball with a failing first member: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", g.Name, "nope", "", anon); w.Code < 500 {
		t.Fatalf("a package only the failing member could have: %d, want 5xx", w.Code)
	}
	off := false
	e.repos["npm-hosted"].Online = &off
	m := jsonBody(t, e.do("GET", "npm-merged", "left-pad", "", anon))
	if vs, _ := get(m, "versions").(map[string]any); len(vs) != 2 || get(m, "dist-tags", "latest") != "1.1.0" {
		t.Fatalf("with the hosted member offline: %v", m)
	}
	if w := e.do("GET", "npm-all", "left-pad", "", anon); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("npm-all (no mergeMembers) with its first member offline: %d, want 503", w.Code)
	}
}

// A merging group nested in a merging group is entered; in a group that does
// not merge, the owner rule goes all the way down.
func TestGroupMergeNested(t *testing.T) {
	e := mergedEnv(t)
	inner := groupOf(e, "g-inner", e.repos["npm-hosted"], e.repos["npm-proxy"])
	inner.MergeMembers = true
	outer := groupOf(e, "g-outer", inner)
	outer.MergeMembers = true
	if vs, _ := get(jsonBody(t, e.do("GET", "g-outer", "left-pad", "", anon)), "versions").(map[string]any); len(vs) != 3 {
		t.Fatalf("merging in merging: %v", vs)
	}
	if w := e.do("GET", "g-outer", "left-pad/-/left-pad-1.1.0.tgz", "", anon); w.Code != 200 {
		t.Fatalf("tarball, merging in merging: %d", w.Code)
	}
	groupOf(e, "g-owner", inner)
	if vs, _ := get(jsonBody(t, e.do("GET", "g-owner", "left-pad", "", anon)), "versions").(map[string]any); len(vs) != 1 {
		t.Fatalf("merging in a group that does not merge: %v", vs)
	}
}
