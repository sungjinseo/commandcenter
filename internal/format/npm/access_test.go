package npm

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
)

// privilegedEnv gives e an Authenticator with an add-only and an edit-only
// user on npm-hosted, and returns their principals.
func privilegedEnv(t *testing.T, e *env) (adder, editor auth.Principal) {
	t.Helper()
	// Imported config roles grant every action, so finer roles are stored
	// the way the API will store them.
	if err := e.h.Store.DB.CreateRole(t.Context(), metaRole("add-only", "add")); err != nil {
		t.Fatal(err)
	}
	if err := e.h.Store.DB.CreateRole(t.Context(), metaRole("edit-only", "edit")); err != nil {
		t.Fatal(err)
	}
	if err := e.h.Store.DB.CreateUser(t.Context(), metaUser("adder", "add-only")); err != nil {
		t.Fatal(err)
	}
	if err := e.h.Store.DB.CreateUser(t.Context(), metaUser("editor", "edit-only")); err != nil {
		t.Fatal(err)
	}
	a, err := auth.Open(t.Context(), e.h.Store.DB, e.h.Store.DB)
	if err != nil {
		t.Fatal(err)
	}
	e.h.Auth = a
	var ok bool
	if adder, ok = a.Lookup("adder"); !ok {
		t.Fatal("adder")
	}
	if editor, ok = a.Lookup("editor"); !ok {
		t.Fatal("editor")
	}
	return adder, editor
}

func TestAccessKindsByPrivilege(t *testing.T) {
	e := newEnv(t)
	adder, editor := privilegedEnv(t, e)
	access := func(p auth.Principal) auth.Access {
		r := httptest.NewRequest("PUT", "/", nil)
		return e.h.Access(r.WithContext(auth.WithPrincipal(r.Context(), p)), e.repos["npm-all"], "left-pad")
	}
	if got := access(adder); got != auth.Add {
		t.Fatalf("adder packument PUT = %v", got)
	}
	// An edit-only caller is checked for Edit, so it can deprecate.
	if got := access(editor); got != auth.Edit {
		t.Fatalf("editor packument PUT = %v", got)
	}
}

func metaRole(id, action string) meta.Role {
	return meta.Role{ID: id, Privileges: []meta.Privilege{{Target: "repo:npm-hosted", Actions: []string{meta.ActionRead, action}}}}
}

func metaUser(id, role string) meta.User {
	return meta.User{ID: id, PasswordHash: "x", Roles: []string{role}}
}

func TestPublishNeedsAddDeprecateNeedsEdit(t *testing.T) {
	e := newEnv(t)
	adder, editor := privilegedEnv(t, e)
	if w := e.do("PUT", "npm-all", "pkg", publishBody("pkg", "1.0.0", []byte("x"), false), editor); w.Code != 403 {
		t.Fatalf("edit-only publish: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("PUT", "npm-all", "pkg", publishBody("pkg", "1.0.0", []byte("x"), false), adder); w.Code != 201 {
		t.Fatalf("add publish: %d %s", w.Code, w.Body.String())
	}
	doc := jsonBody(t, e.do("GET", "npm-hosted", "pkg", "", anon))
	get(doc, "versions", "1.0.0").(map[string]any)["deprecated"] = "old"
	b, _ := json.Marshal(doc)
	if w := e.do("PUT", "npm-all", "pkg", string(b), adder); w.Code != 403 {
		t.Fatalf("add-only deprecate: %d %s", w.Code, w.Body.String())
	}
	if got := jsonBody(t, e.do("GET", "npm-hosted", "pkg/1.0.0", "", anon)); got["deprecated"] != nil {
		t.Fatalf("a refused deprecate was stored: %v", got)
	}
	if w := e.do("PUT", "npm-all", "pkg", string(b), editor); w.Code != 201 {
		t.Fatalf("edit deprecate: %d %s", w.Code, w.Body.String())
	}
	if got := jsonBody(t, e.do("GET", "npm-hosted", "pkg/1.0.0", "", anon)); got["deprecated"] != "old" {
		t.Fatalf("deprecate not stored: %v", got)
	}
}

func TestDistTagsAreNeverServedFromCache(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "npm-hosted", "pkg", publishBody("pkg", "1.0.0", []byte("x"), false), ci); w.Code != 201 {
		t.Fatalf("publish: %d", w.Code)
	}
	// npm caches responses with Last-Modified heuristically; `npm dist-tag ls`
	// right after `npm dist-tag add` must still see the new tag.
	w := e.do("GET", "npm-hosted", "-/package/pkg/dist-tags", "", anon)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "no-cache") {
		t.Fatalf("%d Cache-Control=%q", w.Code, w.Header().Get("Cache-Control"))
	}
}

// withTags replaces the dist-tags of a publish document.
func withTags(t *testing.T, body string, tags map[string]string) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	doc["dist-tags"] = tags
	b, _ := json.Marshal(doc)
	return string(b)
}

func TestAddOnlyCannotRetargetDistTags(t *testing.T) {
	e := newEnv(t)
	adder, editor := privilegedEnv(t, e)
	pub := func(p auth.Principal, version string, tags map[string]string) int {
		return e.do("PUT", "npm-all", "pkg", withTags(t, publishBody("pkg", version, []byte("tgz-"+version), false), tags), p).Code
	}
	if c := pub(adder, "1.0.0", map[string]string{"latest": "1.0.0"}); c != 201 {
		t.Fatalf("first publish: %d", c)
	}
	// `npm publish --tag next`: a tag on the version being added is Add.
	if c := pub(adder, "1.1.0", map[string]string{"next": "1.1.0"}); c != 201 {
		t.Fatalf("publish --tag next: %d", c)
	}
	// Pointing a tag at an existing version needs Edit.
	if c := pub(adder, "1.2.0", map[string]string{"latest": "1.2.0", "beta": "1.0.0"}); c != 403 {
		t.Fatalf("add-only retarget to an existing version: %d", c)
	}
	tags := jsonBody(t, e.do("GET", "npm-hosted", "-/package/pkg/dist-tags", "", anon))
	if tags["beta"] != nil || tags["latest"] != "1.0.0" || tags["next"] != "1.1.0" {
		t.Fatalf("tags after refused publish: %v", tags)
	}
	// Re-sending an unchanged tag is no change.
	if c := pub(adder, "1.2.0", map[string]string{"latest": "1.2.0", "next": "1.1.0"}); c != 201 {
		t.Fatalf("unchanged existing tag: %d", c)
	}
	// An editor may retarget, but cannot add the version that comes with it.
	if c := pub(editor, "1.3.0", map[string]string{"beta": "1.0.0"}); c != 403 {
		t.Fatalf("edit-only publish: %d", c)
	}
}

func TestPublishWithoutAddWritesNoBlob(t *testing.T) {
	e := newEnv(t)
	_, editor := privilegedEnv(t, e)
	count := func() int {
		n := 0
		e.h.Store.StoreByName(repo.DefaultStore).Walk(func(string, time.Time) error { n++; return nil })
		return n
	}
	before := count()
	if w := e.do("PUT", "npm-all", "pkg", publishBody("pkg", "1.0.0", []byte("big tarball"), false), editor); w.Code != 403 {
		t.Fatalf("edit-only publish with an attachment: %d %s", w.Code, w.Body.String())
	}
	if after := count(); after != before {
		t.Fatalf("blobs written for a refused publish: %d -> %d", before, after)
	}
}

// deployer is a non-admin user with every action on npm-hosted, as an
// imported `write: [npm-hosted]` role grants.
func deployer(t *testing.T, e *env) auth.Principal {
	t.Helper()
	a, ps := repotest.Principals(t, e.h.Store.DB, "npm-hosted", map[string][]string{"deployer": meta.Actions})
	e.h.Auth = a
	return ps["deployer"]
}
