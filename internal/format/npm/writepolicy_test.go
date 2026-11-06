package npm

import (
	"context"
	"strings"
	"testing"
)

// npm-10: writePolicy deny makes an npm hosted repository read-only for the
// client: publish, deprecate (a document PUT), dist-tag add and rm and
// unpublish are 400, through the group too, and nothing is stored.
func TestWritePolicyDenyRefusesEveryWrite(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "1.0.0", []byte("x"), false), ci); w.Code != 201 {
		t.Fatalf("setup publish: %d %s", w.Code, w.Body.String())
	}
	doc := e.do("GET", "npm-hosted", "pkg", "", ci).Body.String()
	e.repos["npm-hosted"].WritePolicy = "deny" // npm-all holds the same pointer
	for _, c := range []struct{ method, repo, path, body string }{
		{"PUT", "npm-hosted", "pkg", publishBody("pkg", "1.0.1", []byte("y"), false)},
		{"PUT", "npm-all", "pkg", publishBody("pkg", "1.0.1", []byte("y"), false)},
		{"PUT", "npm-hosted", "other", publishBody("other", "1.0.0", []byte("y"), false)},
		{"PUT", "npm-hosted", "pkg", strings.Replace(doc, `"version":"1.0.0"`, `"version":"1.0.0","deprecated":"old"`, 1)},
		{"PUT", "npm-hosted", "-/package/pkg/dist-tags/beta", `"1.0.0"`},
		{"DELETE", "npm-hosted", "-/package/pkg/dist-tags/latest", ""},
		{"DELETE", "npm-hosted", "pkg/-rev/undefined", ""},
		{"DELETE", "npm-hosted", "pkg/-/pkg-1.0.0.tgz", ""},
		{"DELETE", "npm-hosted", "pkg", ""},
	} {
		w := e.do(c.method, c.repo, c.path, c.body, ci)
		if w.Code != 400 || !strings.Contains(w.Body.String(), `repository \"npm-hosted\" is read-only`) {
			t.Errorf("%s %s/%s: %d %s, want 400 read-only", c.method, c.repo, c.path, w.Code, w.Body.String())
		}
	}
	if after := e.do("GET", "npm-hosted", "pkg", "", ci).Body.String(); after != doc {
		t.Fatalf("the document changed in a read-only repository:\n%s\n%s", doc, after)
	}
	if pkgs, _ := e.h.Store.DB.ListPackages(context.Background(), "npm-hosted", "other"); len(pkgs) != 0 {
		t.Fatalf("a package was stored in a read-only repository: %v", pkgs)
	}
}

// npm-10: writePolicy allow lets `npm publish` send a version again: the
// tarball and the manifest are replaced, dist is the new tarball's, and the
// version counts as published now (_rev and its time move), so an unpublish
// that read the document before is refused. It needs edit as well as add.
func TestWritePolicyAllowPublishesAgain(t *testing.T) {
	e := newEnv(t)
	adder, _ := privilegedEnv(t, e)
	e.repos["npm-hosted"].WritePolicy = "allow"
	first, second := testTarball("pkg", "1.0.0", []byte("first")), testTarball("pkg", "1.0.0", []byte("second"))
	if w := e.do("PUT", "npm-all", "pkg", publishTgz("pkg", "1.0.0", first, false), ci); w.Code != 201 {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	before := jsonBody(t, e.do("GET", "npm-hosted", "pkg", "", ci))
	if w := e.do("PUT", "npm-hosted", "pkg", publishTgz("pkg", "1.0.0", second, false), adder); w.Code != 403 {
		t.Fatalf("publish again with add only: %d %s, want 403", w.Code, w.Body.String())
	}
	if w := e.do("GET", "npm-hosted", "pkg/-/pkg-1.0.0.tgz", "", ci); w.Body.String() != string(first) {
		t.Fatal("a refused publish replaced the tarball")
	}
	if w := e.do("PUT", "npm-all", "pkg", publishTgz("pkg", "1.0.0", second, false), ci); w.Code != 201 {
		t.Fatalf("publish again: %d %s", w.Code, w.Body.String())
	}
	after := jsonBody(t, e.do("GET", "npm-hosted", "pkg", "", ci))
	if a, b := get(before, "versions", "1.0.0", "dist", "shasum"), get(after, "versions", "1.0.0", "dist", "shasum"); a == b || b == nil {
		t.Fatalf("dist.shasum did not change: %v, %v", a, b)
	}
	if get(before, "_rev") == get(after, "_rev") || get(before, "time", "1.0.0") == get(after, "time", "1.0.0") {
		t.Fatalf("the version does not count as published again: _rev %v, %v", get(before, "_rev"), get(after, "_rev"))
	}
	if vs := get(after, "versions").(map[string]any); len(vs) != 1 || get(after, "dist-tags", "latest") != "1.0.0" {
		t.Fatalf("document after publishing again: %v", after)
	}
	if w := e.do("GET", "npm-all", "pkg/-/pkg-1.0.0.tgz", "", ci); w.Code != 200 || w.Body.String() != string(second) {
		t.Fatalf("tarball after publishing again: %d, the first one: %v", w.Code, w.Body.String() == string(first))
	}
	if w := e.do("DELETE", "npm-hosted", "pkg/-rev/"+get(before, "_rev").(string), "", ci); w.Code != 409 {
		t.Fatalf("unpublish with the revision read before: %d %s, want 409", w.Code, w.Body.String())
	}
	// allow_once, the default, still refuses, as registry.npmjs.org does.
	e.repos["npm-hosted"].WritePolicy = "allow_once"
	if w := e.do("PUT", "npm-hosted", "pkg", publishTgz("pkg", "1.0.0", first, false), ci); w.Code != 403 {
		t.Fatalf("allow_once: %d %s, want 403", w.Code, w.Body.String())
	}
}

// npm-11: Nexus also deletes a package or one tarball without a revision in
// the URL, which is what a cleanup script sends with curl.
func TestDeleteWithoutRevision(t *testing.T) {
	e := newEnv(t)
	for _, v := range []string{"1.0.0", "1.0.1"} {
		if w := e.do("PUT", "npm-hosted", "@s/pkg", publishBody("@s/pkg", v, []byte(v), false), ci); w.Code != 201 {
			t.Fatalf("publish %s: %d %s", v, w.Code, w.Body.String())
		}
	}
	if w := e.do("DELETE", "npm-all", "@s/pkg/-/pkg-1.0.1.tgz", "", ci); w.Code != 200 {
		t.Fatalf("delete a tarball: %d %s", w.Code, w.Body.String())
	}
	m := jsonBody(t, e.do("GET", "npm-hosted", "@s/pkg", "", ci))
	if vs := get(m, "versions").(map[string]any); len(vs) != 1 || vs["1.0.0"] == nil || get(m, "dist-tags", "latest") != "1.0.0" {
		t.Fatalf("after deleting 1.0.1: %v", m)
	}
	if w := e.do("DELETE", "npm-hosted", "@s/pkg/-/pkg-1.0.1.tgz", "", ci); w.Code != 404 {
		t.Fatalf("delete it again: %d, want 404", w.Code)
	}
	if w := e.do("DELETE", "npm-hosted", "@s/pkg", "", ci); w.Code != 200 {
		t.Fatalf("delete the package: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "npm-hosted", "@s/pkg", "", ci); w.Code != 404 {
		t.Fatalf("package after delete: %d", w.Code)
	}
	if w := e.do("DELETE", "npm-proxy", "left-pad", "", ci); w.Code != 405 {
		t.Fatalf("delete in a proxy: %d, want 405", w.Code)
	}
}
