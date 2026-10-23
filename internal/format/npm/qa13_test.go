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
)

// Regression tests for the npm findings of the 2026-10-22 QA report (plan 13).

// TestUnpublishAnswers405: npm treats a 404 on `-rev` as "already deleted", so
// unpublish must fail visibly instead (QA H-2, Decision 1).
func TestUnpublishAnswers405(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "1.0.0", []byte("x"), false), ci); w.Code != 201 {
		t.Fatalf("setup publish: %d %s", w.Code, w.Body.String())
	}
	for _, c := range []struct{ method, repo, path string }{
		{"PUT", "npm-hosted", "pkg/-rev/undefined"},
		{"DELETE", "npm-hosted", "pkg/-rev/undefined"},
		{"DELETE", "npm-hosted", "pkg/-/pkg-1.0.0.tgz/-rev/x"},
		{"PUT", "npm-all", "pkg/-rev/undefined"},
		{"DELETE", "npm-all", "pkg/-/pkg-1.0.0.tgz/-rev/x"},
		{"GET", "npm-hosted", "pkg/-rev/undefined"},
	} {
		for _, p := range []auth.Principal{ci, anon} {
			w := e.do(c.method, c.repo, c.path, `{"_id":"pkg"}`, p)
			if w.Code != http.StatusMethodNotAllowed || !emptyAllow(w) ||
				jsonBody(t, w)["error"] != "unpublish is not supported; use the UI or ask an admin" {
				t.Errorf("%s %s/%s as %q: %d %v %q", c.method, c.repo, c.path, p.User, w.Code, w.Header(), w.Body.String())
			}
		}
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

// TestPublishRejectsNonSemver: new versions must be strict SemVer 2.0 (QA M-5).
func TestPublishRejectsNonSemver(t *testing.T) {
	e := newEnv(t)
	for _, v := range []string{"latest", "abc", "1.0.0 ", "=1.0.0", "v1.0.5", "1.0", "1.0.0-<script>"} {
		if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", v, []byte("x"), false), ci); w.Code != 400 {
			t.Errorf("version %q: %d %s", v, w.Code, w.Body.String())
		}
	}
	if w := e.do("GET", "npm-hosted", "pkg", "", anon); w.Code != 404 {
		t.Fatalf("rejected publishes left a package: %d", w.Code)
	}
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "1.0.0-beta.1+build.5", []byte("x"), false), ci); w.Code != 201 {
		t.Fatalf("valid semver: %d %s", w.Code, w.Body.String())
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
	if m := jsonBody(t, read("")); get(m, "versions", "1.1.0") == nil {
		t.Fatalf("plain read must merge the proxy: %v", get(m, "versions"))
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

// TestOldMergedPackumentIsReMerged: a group packument merged and stored by an
// older build (unsalted fingerprint, newest-release latest) is re-merged on
// the next read, so latest follows member order (plan 13 fix I-1).
func TestOldMergedPackumentIsReMerged(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "npm-hosted", PackumentKey("left-pad"), hostedDoc)
	t0 := packumentTarget("left-pad")
	oldMerge := func(_ context.Context, _ []io.ReadSeeker, w io.Writer) error {
		_, err := io.WriteString(w, `{"name":"left-pad","dist-tags":{"latest":"1.1.0"},"versions":{"1.1.0":{"version":"1.1.0"}}}`)
		return err
	}
	old, err := e.h.Resolver.MergedTarget(context.Background(), e.repos["npm-all"], t0, "application/json", oldMerge)
	if err != nil {
		t.Fatal(err)
	}
	if mergeSalt != "" && strings.Contains(old.RemoteETag, mergeSalt) {
		t.Fatalf("setup: old copy already salted: %q", old.RemoteETag)
	}
	m := jsonBody(t, e.do("GET", "npm-all", "left-pad", "", anon))
	if get(m, "dist-tags", "latest") != "1.0.1" || get(m, "versions", "1.0.0", "from") != "hosted" {
		t.Fatalf("old merged copy served: %v", m)
	}
	a, err := e.h.Store.Get(context.Background(), "npm-all", PackumentKey("left-pad"))
	if err != nil || !strings.HasPrefix(a.RemoteETag, "group:"+mergeSalt) {
		t.Fatalf("re-merged copy fingerprint %q %v", a.RemoteETag, err)
	}
}
