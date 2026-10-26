package npm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo/repotest"
)

// Regression tests for the npm findings of the 2026-10-22 QA report (plan 13).

// unpublishDoc is the document `npm unpublish name@version` sends back when
// other versions remain: the one it read, without the version and the
// dist-tags that pointed at it (npm keeps the version under "time").
func unpublishDoc(t *testing.T, e *env, name, version string) string {
	t.Helper()
	doc := jsonBody(t, e.do("GET", "npm-hosted", name, "", ci))
	delete(doc["versions"].(map[string]any), version)
	tags := doc["dist-tags"].(map[string]any)
	for tag, v := range tags {
		if v == version {
			delete(tags, tag)
		}
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// TestUnpublishOneVersion: npm unpublish of one version (QA H-2, N-10) is a
// PUT of the document without it, then a DELETE of its tarball, each with the
// _rev npm read just before. The PUT removes the whole version; the DELETE
// then finds nothing, which npm reads as done. It works through the group's
// writeMember.
func TestUnpublishOneVersion(t *testing.T) {
	e := newEnv(t)
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if w := e.do("PUT", "npm-all", "@s/p", publishBody("@s/p", v, []byte("tgz-"+v), false), ci); w.Code != 201 {
			t.Fatalf("publish %s: %d %s", v, w.Code, w.Body.String())
		}
	}
	sink := &repotest.Sink{}
	e.h.Store.Events = sink
	if w := e.do("PUT", "npm-all", "@s/p/-rev/"+revOf(t, e, "@s/p"), unpublishDoc(t, e, "@s/p", "1.1.0"), ci); w.Code != 200 || jsonBody(t, w)["ok"] != true {
		t.Fatalf("unpublish PUT: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("DELETE", "npm-all", "@s/p/-/p-1.1.0.tgz/-rev/"+revOf(t, e, "@s/p"), "", ci); w.Code != 404 {
		t.Fatalf("tarball DELETE after the PUT: %d %s", w.Code, w.Body.String())
	}
	for _, repoName := range []string{"npm-hosted", "npm-all"} {
		m := jsonBody(t, e.do("GET", repoName, "@s/p", "", anon))
		if get(m, "versions", "1.1.0") != nil || get(m, "versions", "1.0.0") == nil || get(m, "dist-tags", "latest") != "1.0.0" {
			t.Fatalf("%s packument = %v", repoName, m)
		}
		if w := e.do("GET", repoName, "@s/p/-/p-1.1.0.tgz", "", anon); w.Code != 404 {
			t.Fatalf("%s: unpublished tarball: %d", repoName, w.Code)
		}
	}
	want := repotest.Event{Kind: "delete", Actor: "ci", Repo: "npm-hosted", Subject: "@s/p@1.1.0", Detail: "assets=1"}
	if got := sink.Events(); len(got) != 1 || got[0] != want {
		t.Fatalf("events: %v", got)
	}
	// npm must not keep offering 1.1.0 from a heuristically fresh cache: a
	// hosted document is always revalidated; a proxied one is not.
	for _, c := range []struct{ repo, path, want string }{{"npm-all", "@s/p", "no-cache"}, {"npm-hosted", "@s/p/1.0.0", "no-cache"}, {"npm-proxy", "left-pad", ""}} {
		if w := e.do("GET", c.repo, c.path, "", anon); w.Code != 200 || w.Header().Get("Cache-Control") != c.want {
			t.Fatalf("%s %s: %d Cache-Control %q, want %q", c.repo, c.path, w.Code, w.Header().Get("Cache-Control"), c.want)
		}
	}
}

// TestUnpublishWholePackage: DELETE {pkg}/-rev/{rev} removes every version,
// its dist-tags and the document in one go.
func TestUnpublishWholePackage(t *testing.T) {
	e := newEnv(t)
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", v, []byte("tgz-"+v), false), ci); w.Code != 201 {
			t.Fatalf("publish %s: %d", v, w.Code)
		}
	}
	if w := e.do("PUT", "npm-hosted", "-/package/pkg/dist-tags/beta", `"1.0.0"`, ci); w.Code != 201 {
		t.Fatalf("dist-tag: %d", w.Code)
	}
	sink := &repotest.Sink{}
	e.h.Store.Events = sink
	if w := e.do("DELETE", "npm-all", "pkg/-rev/undefined", "", ci); w.Code != 200 {
		t.Fatalf("unpublish: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "npm-hosted", "pkg", "", anon); w.Code != 404 {
		t.Fatalf("packument after unpublish: %d", w.Code)
	}
	if as, _ := e.h.Store.DB.ListAssets(context.Background(), "npm-hosted", "pkg/"); len(as) != 0 {
		t.Fatalf("assets left: %+v", as)
	}
	if got := tagsOf(t, e, "pkg"); len(got) != 0 {
		t.Fatalf("tags left: %v", got)
	}
	if got := sink.Events(); len(got) != 2 || got[0].Subject != "pkg@1.0.0" || got[1].Subject != "pkg@1.1.0" {
		t.Fatalf("events: %v", got)
	}
	if w := e.do("DELETE", "npm-hosted", "pkg/-rev/undefined", "", ci); w.Code != 404 {
		t.Fatalf("second unpublish: %d", w.Code)
	}
}

// revOf is the _rev of name's hosted document, as npm reads it with ?write=true.
func revOf(t *testing.T, e *env, name string) string {
	t.Helper()
	rev, _ := jsonBody(t, e.do("GET", "npm-hosted", name, "", ci))["_rev"].(string)
	if rev == "" {
		t.Fatalf("%s: the document has no _rev", name)
	}
	return rev
}

// TestUnpublishWholeKeepsVersionPublishedAfterRead: npm 10 (libnpmpublish
// 9.0.9) sends DELETE {pkg}/-rev/{_rev} for `npm unpublish pkg@1.0.0 --force`
// when the document it read with ?write=true held only that version. A
// version published between that read and the DELETE changes the _rev, so the
// DELETE is refused (409) and deletes nothing; a dist-tag edit keeps the _rev.
func TestUnpublishWholeKeepsVersionPublishedAfterRead(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "1.0.0", []byte("a"), false), ci); w.Code != 201 {
		t.Fatalf("publish 1.0.0: %d %s", w.Code, w.Body.String())
	}
	// npm: GET pkg?write=true -> one version -> onlyVersion -> DELETE.
	doc := jsonBody(t, e.do("GET", "npm-hosted", "pkg", "", ci))
	if n := len(doc["versions"].(map[string]any)); n != 1 {
		t.Fatalf("versions npm saw: %d", n)
	}
	rev := revOf(t, e, "pkg")
	// A CI job publishes 1.1.0 meanwhile.
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "1.1.0", []byte("b"), false), ci); w.Code != 201 {
		t.Fatalf("publish 1.1.0: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("DELETE", "npm-hosted", "pkg/-rev/"+rev, "", ci); w.Code != 409 {
		t.Fatalf("DELETE with the _rev npm read: %d %s, want 409", w.Code, w.Body.String())
	}
	if pkgs, _ := e.h.Store.DB.ListPackages(context.Background(), "npm-hosted", "pkg"); len(pkgs) != 2 {
		t.Fatalf("versions after a refused unpublish: %+v", pkgs)
	}
	rev = revOf(t, e, "pkg")
	if w := e.do("PUT", "npm-hosted", "-/package/pkg/dist-tags/beta", `"1.0.0"`, ci); w.Code != 201 {
		t.Fatalf("dist-tag: %d", w.Code)
	}
	if got := revOf(t, e, "pkg"); got != rev {
		t.Fatalf("a dist-tag edit changed _rev %s to %s", rev, got)
	}
	if w := e.do("DELETE", "npm-hosted", "pkg/-rev/"+rev, "", ci); w.Code != 200 {
		t.Fatalf("DELETE with the current _rev: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "npm-hosted", "pkg", "", anon); w.Code != 404 {
		t.Fatalf("packument after unpublish: %d", w.Code)
	}
}

// TestUnpublishTarballAlone: a tarball DELETE without the PUT removes its
// version too, unless its rev is not the document's; a file of another
// package, or of no stored version, is not found whatever the rev.
func TestUnpublishTarballAlone(t *testing.T) {
	e := newEnv(t)
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", v, []byte("tgz-"+v), false), ci); w.Code != 201 {
			t.Fatalf("publish %s: %d", v, w.Code)
		}
	}
	for _, file := range []string{"other-1.0.0.tgz", "pkg-.tgz", "pkg-9.9.9.tgz"} {
		if w := e.do("DELETE", "npm-hosted", "pkg/-/"+file+"/-rev/x", "", ci); w.Code != 404 {
			t.Fatalf("%s: %d %s", file, w.Code, w.Body.String())
		}
	}
	rev := revOf(t, e, "pkg")
	if w := e.do("DELETE", "npm-hosted", "pkg/-/pkg-1.1.0.tgz/-rev/x", "", ci); w.Code != 409 {
		t.Fatalf("tarball DELETE with a stale rev: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("DELETE", "npm-hosted", "pkg/-/pkg-1.1.0.tgz/-rev/undefined", "", ci); w.Code != 200 {
		t.Fatalf("tarball DELETE: %d %s", w.Code, w.Body.String())
	}
	m := jsonBody(t, e.do("GET", "npm-hosted", "pkg", "", anon))
	if get(m, "versions", "1.1.0") != nil || get(m, "dist-tags", "latest") != "1.0.0" {
		t.Fatalf("packument = %v", m)
	}
	// 1.1.0 published again is not the 1.1.0 that rev saw.
	e.h.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "1.1.0", []byte("again"), false), ci); w.Code != 201 {
		t.Fatalf("republish 1.1.0: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("DELETE", "npm-hosted", "pkg/-/pkg-1.1.0.tgz/-rev/"+rev, "", ci); w.Code != 409 {
		t.Fatalf("tarball DELETE of a republished version: %d %s", w.Code, w.Body.String())
	}
}

// TestUnpublishKeepsVersionsNotRead: a version published after npm read the
// document is in neither "versions" nor "time" of the PUT; the PUT is
// refused (409) instead of deleting it, and nothing goes.
func TestUnpublishKeepsVersionsNotRead(t *testing.T) {
	e := newEnv(t)
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", v, []byte("tgz-"+v), false), ci); w.Code != 201 {
			t.Fatalf("publish %s: %d", v, w.Code)
		}
	}
	stale, staleRev := unpublishDoc(t, e, "pkg", "1.0.0"), revOf(t, e, "pkg")
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "1.2.0", []byte("tgz-1.2.0"), false), ci); w.Code != 201 {
		t.Fatalf("publish 1.2.0: %d", w.Code)
	}
	if w := e.do("PUT", "npm-hosted", "pkg/-rev/undefined", stale, ci); w.Code != 409 {
		t.Fatalf("stale unpublish: %d %s", w.Code, w.Body.String())
	}
	// A real rev is compared too: here the document is current, the rev is not.
	if w := e.do("PUT", "npm-hosted", "pkg/-rev/"+staleRev, unpublishDoc(t, e, "pkg", "1.0.0"), ci); w.Code != 409 {
		t.Fatalf("unpublish with a stale rev: %d %s", w.Code, w.Body.String())
	}
	if pkgs, _ := e.h.Store.DB.ListPackages(context.Background(), "npm-hosted", "pkg"); len(pkgs) != 3 {
		t.Fatalf("versions after a refused unpublish: %+v", pkgs)
	}
	// Nothing to remove is no change.
	if w := e.do("PUT", "npm-hosted", "pkg/-rev/undefined", unpublishDoc(t, e, "pkg", "9.9.9"), ci); w.Code != 200 {
		t.Fatalf("unpublish of nothing: %d %s", w.Code, w.Body.String())
	}
}

// TestUnpublishDocumentChecks: the PUT body is a bounded, checked document;
// a refused one deletes nothing.
func TestUnpublishDocumentChecks(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "1.0.0", []byte("x"), false), ci); w.Code != 201 {
		t.Fatalf("publish: %d", w.Code)
	}
	deep := `{"name":"pkg","versions":{"1.0.0":` + strings.Repeat("[", 100) + strings.Repeat("]", 100) + `},"time":{}}`
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"name":"other","versions":{},"time":{"1.0.0":"x"}}`, 400},
		{`{"name":"pkg","time":{"1.0.0":"x"}}`, 400},
		{`["pkg"]`, 400},
		{`{"name":"pkg","versions":{},"time":{"1.0.0":`, 400},
		{deep, 400},
		{`{"name":"pkg","readme":"` + strings.Repeat("x", maxManifest) + `"}`, 413},
	} {
		if w := e.do("PUT", "npm-hosted", "pkg/-rev/undefined", c.body, ci); w.Code != c.want {
			t.Errorf("body %.60q: %d %s, want %d", c.body, w.Code, w.Body.String(), c.want)
		}
	}
	if w := e.do("PUT", "npm-hosted", "ghost/-rev/undefined", `{"name":"ghost","versions":{}}`, ci); w.Code != 404 {
		t.Fatalf("unknown package: %d", w.Code)
	}
	if w := e.do("GET", "npm-hosted", "pkg/1.0.0", "", anon); w.Code != 200 {
		t.Fatalf("version gone after refused unpublishes: %d", w.Code)
	}
}

// TestUnpublishWhereNotSupported: a proxy accepts no method here (an empty
// Allow and a message that says where unpublish works), the other methods are
// 405 with the true Allow, and a read-only repository refuses.
func TestUnpublishWhereNotSupported(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "1.0.0", []byte("x"), false), ci); w.Code != 201 {
		t.Fatalf("publish: %d", w.Code)
	}
	for _, c := range []struct{ method, path string }{{"PUT", "pkg/-rev/undefined"}, {"DELETE", "pkg/-rev/undefined"}, {"DELETE", "pkg/-/pkg-1.0.0.tgz/-rev/x"}} {
		w := e.do(c.method, "npm-proxy", c.path, `{}`, ci)
		if msg, _ := jsonBody(t, w)["error"].(string); w.Code != 405 || !emptyAllow(w) || !strings.Contains(msg, "unpublish is not supported in proxy repository") {
			t.Errorf("proxy %s %s: %d %v %q", c.method, c.path, w.Code, w.Header(), w.Body.String())
		}
	}
	for _, c := range []struct{ method, path, allow string }{
		{"GET", "pkg/-rev/undefined", "PUT, DELETE"},
		{"POST", "pkg/-rev/undefined", "PUT, DELETE"},
		{"PUT", "pkg/-/pkg-1.0.0.tgz/-rev/x", "DELETE"},
	} {
		if w := e.do(c.method, "npm-hosted", c.path, `{}`, ci); w.Code != 405 || w.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s: %d Allow %q, want %q", c.method, c.path, w.Code, w.Header().Get("Allow"), c.allow)
		}
	}
	e.repos["npm-hosted"].WritePolicy = "deny"
	if w := e.do("DELETE", "npm-all", "pkg/-rev/undefined", "", ci); w.Code != 400 {
		t.Fatalf("read-only repository: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "npm-hosted", "pkg/1.0.0", "", anon); w.Code != 200 {
		t.Fatalf("package must survive: %d", w.Code)
	}
}

// TestAuditNotSupported: audit must not claim "0 vulnerabilities" (QA M-3,
// Decision 2).
func TestAuditNotSupported(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"-/npm/v1/security/advisories/bulk", "-/npm/v1/security/audits/quick"} {
		w := e.do("POST", "npm-all", path, `{"express":["4.0.0"]}`, anon)
		if w.Code != http.StatusNotImplemented || jsonBody(t, w)["error"] != "npm audit is not supported by this registry" {
			t.Fatalf("%s: %d %q", path, w.Code, w.Body.String())
		}
	}
}

// TestLogoutRevokesOwnToken: `npm logout` sends DELETE /-/user/token/<tok>
// with that token; it must be revoked (QA M-6). The ldap:<id> owner case is
// covered by auth.TestRevokeTokenLDAPOwner.
func TestLogoutRevokesOwnToken(t *testing.T) {
	e := newEnv(t)
	login := func() string {
		w := e.do("PUT", "npm-all", "-/user/org.couchdb.user:ci", `{"name":"ci","password":"pw"}`, anon)
		tok, _ := jsonBody(t, w)["token"].(string)
		if tok == "" {
			t.Fatalf("login: %d %s", w.Code, w.Body.String())
		}
		return tok
	}
	authn := func(tok string) (auth.Principal, error) {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		return e.h.Auth.Authenticate(r)
	}
	tok, other := login(), login()
	p, err := authn(tok)
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do("DELETE", "npm-all", "-/user/token/"+other, "", anon); w.Code != 401 {
		t.Fatalf("anonymous logout: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "npm-all", "-/user/token/"+tok, "", p); w.Code != 405 || w.Header().Get("Allow") != "DELETE" {
		t.Fatalf("GET: %d %v", w.Code, w.Header())
	}
	// Someone else's token is not found for this caller and survives.
	w := e.do("DELETE", "npm-all", "-/user/token/"+other, "", auth.Principal{User: "someone"})
	if w.Code != 404 {
		t.Fatalf("other user's token: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), other) {
		t.Fatalf("error echoes the token: %s", w.Body.String())
	}
	if _, err := authn(other); err != nil {
		t.Fatalf("other token must survive: %v", err)
	}
	w = e.do("DELETE", "npm-all", "-/user/token/"+tok, "", p)
	if w.Code != 200 || jsonBody(t, w)["ok"] != true {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}
	if _, err := authn(tok); err == nil {
		t.Fatal("revoked token still authenticates")
	}
	if w := e.do("DELETE", "npm-hosted", "-/user/token/"+tok, "", p); w.Code != 404 {
		t.Fatalf("second logout: %d", w.Code)
	}
	if _, err := authn(other); err != nil {
		t.Fatalf("logout must revoke only the sent token: %v", err)
	}
}

// TestPublishRejectsNonSemver: new versions must be strict SemVer 2.0 (QA M-5)
// without build metadata, which npm ignores: 3.0.0+a next to 3.0.0 broke
// `npm install X@3.0.0` (QA N-12).
func TestPublishRejectsNonSemver(t *testing.T) {
	e := newEnv(t)
	for _, v := range []string{"latest", "abc", "1.0.0 ", "=1.0.0", "v1.0.5", "1.0", "1.0.0-<script>", "3.0.0+a", "1.0.0-beta.1+build.5"} {
		if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", v, []byte("x"), false), ci); w.Code != 400 {
			t.Errorf("version %q: %d %s", v, w.Code, w.Body.String())
		}
	}
	if w := e.do("GET", "npm-hosted", "pkg", "", anon); w.Code != 404 {
		t.Fatalf("rejected publishes left a package: %d", w.Code)
	}
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "3.0.0", []byte("x"), false), ci); w.Code != 201 {
		t.Fatalf("valid semver: %d %s", w.Code, w.Body.String())
	}
	w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "3.0.0+a", []byte("y"), false), ci)
	if msg, _ := jsonBody(t, w)["error"].(string); w.Code != 400 || !strings.Contains(msg, "build metadata") {
		t.Fatalf("3.0.0+a next to 3.0.0: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "3.0.1-beta.1", []byte("z"), false), ci); w.Code != 201 {
		t.Fatalf("valid prerelease: %d %s", w.Code, w.Body.String())
	}
}

// seedLegacy stores rows the way an older build could have: a non-semver
// version and a dist-tag name that is now refused.
func seedLegacy(t *testing.T, e *env) {
	t.Helper()
	ctx := context.Background()
	rp := e.repos["npm-hosted"]
	err := e.h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		attrs := `{"name":"legacy","version":"v1.0.5","dist":{"tarball":"x"}}`
		if err := tx.InsertPackage(ctx, meta.Package{Repo: rp.Name, Format: "npm", Name: "legacy", Version: "v1.0.5", Attrs: attrs, CreatedAt: time.Now()}); err != nil {
			return err
		}
		if err := tx.SetDistTag(ctx, rp.Name, "legacy", "latest", "v1.0.5"); err != nil {
			return err
		}
		if err := tx.SetDistTag(ctx, rp.Name, "legacy", "^old", "v1.0.5"); err != nil {
			return err
		}
		return e.h.writePackument(ctx, tx, rp, "legacy")
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestLegacyNonSemverStillServed: validation applies to new versions and new
// tag names only; stored rows keep loading and can still be deprecated
// (Review Focus 1).
func TestLegacyNonSemverStillServed(t *testing.T) {
	e := newEnv(t)
	seedLegacy(t, e)
	for _, repoName := range []string{"npm-hosted", "npm-all"} {
		m := jsonBody(t, e.do("GET", repoName, "legacy", "", anon))
		if get(m, "versions", "v1.0.5") == nil || get(m, "dist-tags", "latest") != "v1.0.5" || get(m, "dist-tags", "^old") != "v1.0.5" {
			t.Fatalf("%s packument = %v", repoName, m)
		}
		if w := e.do("GET", repoName, "legacy/v1.0.5", "", anon); w.Code != 200 {
			t.Fatalf("%s version doc: %d", repoName, w.Code)
		}
	}
	// A new valid version publishes next to the legacy one.
	if w := e.do("PUT", "npm-hosted", "legacy", publishBody("legacy", "1.0.6", []byte("y"), false), ci); w.Code != 201 {
		t.Fatalf("publish next to legacy: %d %s", w.Code, w.Body.String())
	}
	// npm deprecate sends the whole document back, legacy rows included.
	doc := jsonBody(t, e.do("GET", "npm-hosted", "legacy", "", anon))
	get(doc, "versions", "v1.0.5").(map[string]any)["deprecated"] = "old"
	b, _ := json.Marshal(doc)
	if w := e.do("PUT", "npm-all", "legacy", string(b), ci); w.Code != 201 {
		t.Fatalf("deprecate legacy: %d %s", w.Code, w.Body.String())
	}
	if got := jsonBody(t, e.do("GET", "npm-hosted", "legacy/v1.0.5", "", anon)); got["deprecated"] != "old" {
		t.Fatalf("legacy version doc = %v", got)
	}
}

// TestDistTagNameValidation: a tag name must not look like a version or a
// range (QA L-16), on the dist-tag route and inside a publish.
func TestDistTagNameValidation(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "1.0.0", []byte("x"), false), ci); w.Code != 201 {
		t.Fatalf("setup publish: %d %s", w.Code, w.Body.String())
	}
	for _, tag := range []string{"1.0.0", "^1.0.0", "a b", "~1", "1.x", ">=1", "v1", "*", "1"} {
		if w := e.do("PUT", "npm-hosted", "-/package/pkg/dist-tags/"+tag, `"1.0.0"`, ci); w.Code != 400 {
			t.Errorf("tag %q: %d %s", tag, w.Code, w.Body.String())
		}
	}
	for _, tag := range []string{"beta", "next", "rc-1", "v1-compat"} {
		if w := e.do("PUT", "npm-hosted", "-/package/pkg/dist-tags/"+tag, `"1.0.0"`, ci); w.Code != 201 {
			t.Errorf("tag %q: %d %s", tag, w.Code, w.Body.String())
		}
	}
	body := withTags(t, publishBody("pkg", "1.1.0", []byte("y"), false), map[string]string{"latest": "1.1.0", "1.x": "1.1.0"})
	if w := e.do("PUT", "npm-hosted", "pkg", body, ci); w.Code != 400 {
		t.Fatalf("range tag in publish: %d %s", w.Code, w.Body.String())
	}
}

// TestGroupWriteTrueReturnsWriteMemberOnly: npm deprecate reads the document
// with ?write=true and sends it back; it must hold only the writeMember's
// versions, and an offline writeMember is a 503 (QA M-7, Review Focus 5).
func TestGroupWriteTrueReturnsWriteMemberOnly(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "npm-hosted", "left-pad", publishBody("left-pad", "0.5.0", []byte("x"), false), ci); w.Code != 201 {
		t.Fatalf("setup publish: %d %s", w.Code, w.Body.String())
	}
	read := func(query string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/repository/npm-all/left-pad"+query, nil)
		r = r.WithContext(auth.WithPrincipal(r.Context(), ci))
		e.h.Serve(w, r, e.repos["npm-all"], "left-pad")
		return w
	}
	// The hosted member owns left-pad, so a plain read holds only its
	// versions too (QA N-1).
	if m := jsonBody(t, read("")); get(m, "versions", "0.5.0") == nil || get(m, "versions", "1.1.0") != nil {
		t.Fatalf("plain read: versions = %v", get(m, "versions"))
	}
	w := read("?write=true")
	m := jsonBody(t, w)
	if w.Code != 200 || get(m, "versions", "0.5.0") == nil || get(m, "versions", "1.1.0") != nil || get(m, "versions", "1.0.0") != nil {
		t.Fatalf("write=true: %d versions = %v", w.Code, get(m, "versions"))
	}
	// The deprecate round trip through the group now works.
	get(m, "versions", "0.5.0").(map[string]any)["deprecated"] = "x"
	b, _ := json.Marshal(m)
	if w := e.do("PUT", "npm-all", "left-pad", string(b), ci); w.Code != 201 {
		t.Fatalf("deprecate via group: %d %s", w.Code, w.Body.String())
	}

	off := false
	e.repos["npm-hosted"].Online = &off
	if w := read("?write=true"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline writeMember: %d %s", w.Code, w.Body.String())
	}
}

// TestOldMergedPackumentIsNotServed: a group packument merged and stored by an
// older build is never served again; the owner's own document is (QA N-1).
func TestOldMergedPackumentIsNotServed(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "npm-hosted", PackumentKey("left-pad"), hostedDoc)
	oldMerge := func(_ context.Context, _ []io.ReadSeeker, w io.Writer) error {
		_, err := io.WriteString(w, `{"name":"left-pad","dist-tags":{"latest":"1.1.0"},"versions":{"1.1.0":{"version":"1.1.0"}}}`)
		return err
	}
	old, err := e.h.Resolver.MergedTarget(context.Background(), e.repos["npm-all"], packumentTarget("left-pad"), "application/json", oldMerge)
	if err != nil || old.Repo != "npm-all" {
		t.Fatalf("setup: merged copy not stored in the group: %+v %v", old, err)
	}
	m := jsonBody(t, e.do("GET", "npm-all", "left-pad", "", anon))
	if get(m, "dist-tags", "latest") != "1.0.1" || get(m, "versions", "1.0.0", "from") != "hosted" || get(m, "versions", "1.1.0") != nil {
		t.Fatalf("old merged copy served: %v", m)
	}
}
