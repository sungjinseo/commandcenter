package yum

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo/repotest"
)

const compsDoc = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE comps PUBLIC "-//Red Hat, Inc.//DTD Comps info//EN" "comps.dtd">
<comps>
  <group><id>tools</id><name>Tools</name><default>false</default><uservisible>true</uservisible>
    <packagelist><packagereq type="mandatory">demo</packagereq></packagelist></group>
</comps>
`

// groupData is the <data type=...> of repomd, "" when there is none.
func groupData(repomd, typ string) string {
	return regexp.MustCompile(`(?s)<data type="` + typ + `">.*?</data>`).FindString(repomd)
}

// Nexus takes a repodata/comps.xml or comps.xml.gz by those exact names and
// lists it in repomd.xml; dnf reads the package groups from there.
func TestComps(t *testing.T) {
	e := newEnv(t)
	if w := e.do("GET", "yum-hosted", "repodata/comps.xml", nil); w.Code != 404 {
		t.Fatalf("before the upload: %d", w.Code)
	}
	// not a package group file
	for _, body := range [][]byte{nil, []byte("not xml"), []byte("<other/>"), demoRPM()} {
		if w := e.do("PUT", "yum-hosted", "repodata/comps.xml", body); w.Code != 400 {
			t.Fatalf("PUT %.20q: %d %s", body, w.Code, w.Body)
		}
	}
	if w := e.do("PUT", "yum-hosted", "repodata/other.xml", []byte(compsDoc)); w.Code != 400 {
		t.Fatalf("another file in repodata/: %d", w.Code)
	}
	if w := e.do("PUT", "yum-hosted", "repodata/comps.xml", []byte(compsDoc)); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	repomd, data := e.repodata(t, "yum-hosted", "") // an empty repository has its three documents and the group
	if !strings.Contains(data["primary"], `packages="0"`) {
		t.Fatalf("primary without packages:\n%s", data["primary"])
	}
	sha := sum([]byte(compsDoc))
	want := `<data type="group">
    <checksum type="sha256">` + sha + `</checksum>
    <location href="repodata/` + sha + `-comps.xml"/>
    <timestamp>` + fmt.Sprint(e.now.Unix()) + `</timestamp>
    <size>` + fmt.Sprint(len(compsDoc)) + `</size>
  </data>`
	if got := groupData(repomd, "group"); got != want {
		t.Fatalf("repomd.xml: %s\nwant: %s", got, want)
	}
	for _, p := range []string{"repodata/comps.xml", "repodata/" + sha + "-comps.xml"} {
		if w := e.do("GET", "yum-hosted", p, nil); w.Code != 200 || w.Body.String() != compsDoc || w.Header().Get("Content-Type") != ctXML {
			t.Fatalf("GET %s: %d %v", p, w.Code, w.Header())
		}
	}
	// allow_once: not again. An upload of a package keeps it.
	if w := e.do("PUT", "yum-hosted", "repodata/comps.xml", []byte(compsDoc)); w.Code != 409 {
		t.Fatalf("again: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "yum-hosted", "demo-1.0-1.el9.x86_64.rpm", demoRPM()); w.Code != 201 {
		t.Fatalf("PUT rpm: %d %s", w.Code, w.Body)
	}
	if repomd, data = e.repodata(t, "yum-hosted", ""); groupData(repomd, "group") == "" || !strings.Contains(data["primary"], "<name>demo</name>") {
		t.Fatalf("after a package:\n%s", repomd)
	}
	// repository.rebuild-metadata keeps it too
	if err := e.h.RebuildMetadata(context.Background(), e.repos["yum-hosted"], func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if repomd, _ = e.repodata(t, "yum-hosted", ""); groupData(repomd, "group") == "" {
		t.Fatalf("after a rebuild:\n%s", repomd)
	}
	// the gz form is listed beside it, by the file it is
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(compsDoc))
	zw.Close()
	if w := e.do("PUT", "yum-hosted", "repodata/comps.xml.gz", []byte(compsDoc)); w.Code != 400 {
		t.Fatalf("comps.xml.gz that is not gzip: %d", w.Code)
	}
	if w := e.do("PUT", "yum-hosted", "repodata/comps.xml.gz", gz.Bytes()); w.Code != 201 {
		t.Fatalf("comps.xml.gz: %d %s", w.Code, w.Body)
	}
	repomd, _ = e.repodata(t, "yum-hosted", "")
	if !strings.Contains(groupData(repomd, "group_gz"), `<location href="repodata/`+sum(gz.Bytes())+`-comps.xml.gz"/>`) || groupData(repomd, "group") == "" {
		t.Fatalf("both forms:\n%s", repomd)
	}
	// a delete takes it out of repomd.xml, with its copy; anything else in repodata/ is 405
	if w := e.do("DELETE", "yum-hosted", "repodata/repomd.xml", nil); w.Code != 405 {
		t.Fatalf("DELETE repomd.xml: %d", w.Code)
	}
	for _, f := range []string{"comps.xml", "comps.xml.gz"} {
		if w := e.do("DELETE", "yum-hosted", "repodata/"+f, nil); w.Code != 204 {
			t.Fatalf("DELETE %s: %d %s", f, w.Code, w.Body)
		}
	}
	if w := e.do("DELETE", "yum-hosted", "repodata/comps.xml", nil); w.Code != 404 {
		t.Fatalf("DELETE again: %d", w.Code)
	}
	if repomd, _ = e.repodata(t, "yum-hosted", ""); strings.Contains(repomd, "group") {
		t.Fatalf("after the delete:\n%s", repomd)
	}
	for _, p := range []string{"repodata/comps.xml", "repodata/" + sha + "-comps.xml"} {
		if w := e.do("GET", "yum-hosted", p, nil); w.Code != 404 {
			t.Fatalf("GET %s after the delete: %d", p, w.Code)
		}
	}
	if got := e.sink.Events(); len(got) < 2 || got[len(got)-1] != (repotest.Event{Kind: "delete", Repo: "yum-hosted", Subject: "repodata/comps.xml.gz"}) {
		t.Fatalf("events: %v", got)
	}
	// neither is a component
	cands, err := e.db.ListPackageNames(context.Background(), "yum-hosted")
	if err != nil || len(cands) != 1 {
		t.Fatalf("rows: %v %v", cands, err)
	}
	// read-only
	if w := e.do("PUT", "yum-ro", "repodata/comps.xml", []byte(compsDoc)); w.Code != 400 {
		t.Fatalf("deny: %d", w.Code)
	}
	if w := e.do("DELETE", "yum-ro", "repodata/comps.xml", nil); w.Code != 400 {
		t.Fatalf("deny, DELETE: %d", w.Code)
	}
}

// Under writePolicy allow a new comps.xml replaces the old, for whoever may
// edit; through a group it goes to the writeMember.
func TestCompsReplaced(t *testing.T) {
	e := newEnv(t)
	a, ps := repotest.Principals(t, e.db, "yum-allow", map[string][]string{
		"adder":  {meta.ActionAdd},
		"editor": {meta.ActionAdd, meta.ActionEdit},
	})
	e.h.Auth = a
	if w := e.doAs(ps["adder"], "PUT", "yum-allow", "repodata/comps.xml", []byte(compsDoc)); w.Code != 201 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	if w := e.doAs(ps["adder"], "PUT", "yum-allow", "repodata/comps.xml", []byte(compsDoc)); w.Code != 403 {
		t.Fatalf("without edit: %d", w.Code)
	}
	second := strings.Replace(compsDoc, "Tools", "Better tools", 1)
	if w := e.doAs(ps["editor"], "PUT", "yum-allow", "repodata/comps.xml", []byte(second)); w.Code != 201 {
		t.Fatalf("with edit: %d %s", w.Code, w.Body)
	}
	repomd, _ := e.repodata(t, "yum-allow", "")
	if !strings.Contains(groupData(repomd, "group"), sum([]byte(second))) {
		t.Fatalf("repomd.xml:\n%s", repomd)
	}
	if w := e.do("GET", "yum-allow", "repodata/"+sum([]byte(compsDoc))+"-comps.xml", nil); w.Code != 404 {
		t.Fatalf("the old copy: %d", w.Code)
	}
	e2 := newEnv(t)
	if w := e2.do("PUT", "yum-all", "repodata/comps.xml", []byte(compsDoc)); w.Code != 201 {
		t.Fatalf("through the group: %d %s", w.Code, w.Body)
	}
	if w := e2.do("GET", "yum-hosted", "repodata/comps.xml", nil); w.Code != 200 {
		t.Fatalf("in the writeMember: %d", w.Code)
	}
	if w := e2.do("DELETE", "yum-all", "repodata/comps.xml", nil); w.Code != 204 {
		t.Fatalf("delete through the group: %d %s", w.Code, w.Body)
	}
	if w := e2.do("DELETE", "yum-proxy", "repodata/comps.xml", nil); w.Code != 405 {
		t.Fatalf("delete in a proxy: %d", w.Code)
	}
}

// With a depth comps.xml belongs to the repodata/ of a directory that deep,
// which a rebuild finds without any package there.
func TestCompsAtDepth(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"repodata/comps.xml", "el9/x/repodata/comps.xml", "repodata/repodata/comps.xml"} {
		if w := e.do("PUT", "yum-deep", p, []byte(compsDoc)); w.Code != 400 || !strings.Contains(w.Body.String(), "depth 1") {
			t.Errorf("PUT %s: %d %s", p, w.Code, w.Body)
		}
	}
	if w := e.do("PUT", "yum-deep", "el9/repodata/comps.xml", []byte(compsDoc)); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "yum-deep", "el8/demo-1.0-1.el8.noarch.rpm", nvra("demo", "1.0", "1.el8", "noarch")); w.Code != 201 {
		t.Fatalf("PUT rpm: %d %s", w.Code, w.Body)
	}
	el9, _ := e.repodata(t, "yum-deep", "el9/")
	el8, _ := e.repodata(t, "yum-deep", "el8/")
	if groupData(el9, "group") == "" || groupData(el8, "group") != "" {
		t.Fatalf("el9:\n%s\nel8:\n%s", el9, el8)
	}
	// gone from the database, a rebuild makes it again from the comps.xml
	err := e.h.Store.Tx(context.Background(), e.repos["yum-deep"], func(tx *meta.Tx) error {
		_, err := tx.DeleteAsset(context.Background(), "yum-deep", "el9/repodata/repomd.xml")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Hour)
	if err := e.h.RebuildMetadata(context.Background(), e.repos["yum-deep"], func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if el9, _ = e.repodata(t, "yum-deep", "el9/"); groupData(el9, "group") == "" {
		t.Fatalf("el9 after a rebuild:\n%s", el9)
	}
	if w := e.do("DELETE", "yum-deep", "el9/repodata/comps.xml", nil); w.Code != 204 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body)
	}
	if el9, _ = e.repodata(t, "yum-deep", "el9/"); groupData(el9, "group") != "" {
		t.Fatalf("el9 after the delete:\n%s", el9)
	}
}

// Nexus's documentation uploads to .../yum-hosted/test.rpm. The package goes
// to that directory under its NVRA; the 201 says where. What the path alone
// used to decide stays: a depth, the second upload, the privileges.
func TestUploadUnderAnotherName(t *testing.T) {
	e := newEnv(t)
	demo := demoRPM()
	put := func(repoName, p string, body []byte) *httptest.ResponseRecorder {
		return e.doAs(auth.Principal{User: "admin", Admin: true}, "PUT", repoName, p, body)
	}
	w := put("yum-hosted", "el9/test.rpm", demo)
	if w.Code != 201 || w.Header().Get("Location") != "demo-1.0-1.el9.x86_64.rpm" {
		t.Fatalf("PUT: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0].Subject != "el9/demo@1.0-1.el9.x86_64" {
		t.Fatalf("events: %v", got)
	}
	if w := e.do("GET", "yum-hosted", "el9/test.rpm", nil); w.Code != 404 {
		t.Fatalf("GET of the name given: %d", w.Code)
	}
	if w := e.do("GET", "yum-hosted", "el9/demo-1.0-1.el9.x86_64.rpm", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), demo) {
		t.Fatalf("GET of the stored name: %d", w.Code)
	}
	_, data := e.repodata(t, "yum-hosted", "")
	if !strings.Contains(data["primary"], `<location href="el9/demo-1.0-1.el9.x86_64.rpm"/>`) {
		t.Fatalf("primary:\n%s", data["primary"])
	}
	// a name that is another package's says nothing either: the header counts
	if w := put("yum-hosted", "other-9-9.noarch.rpm", nvra("real", "1", "1", "noarch")); w.Code != 201 || w.Header().Get("Location") != "real-1-1.noarch.rpm" {
		t.Fatalf("PUT: %d %v", w.Code, w.Header())
	}
	if w := e.do("GET", "yum-hosted", "other-9-9.noarch.rpm", nil); w.Code != 404 {
		t.Fatalf("GET: %d", w.Code)
	}
	// the name it has needs no Location; the same package under any name is a 409
	if w := put("yum-hosted", "x-1-1.noarch.rpm", nvra("x", "1", "1", "noarch")); w.Code != 201 || w.Header().Get("Location") != "" {
		t.Fatalf("PUT: %d %v", w.Code, w.Header())
	}
	if w := put("yum-hosted", "el9/again.rpm", demo); w.Code != 409 {
		t.Fatalf("again: %d %s", w.Code, w.Body)
	}
	// a component like any other: deleted, its repodata follows
	res, err := e.h.DeleteComponent(context.Background(), e.repos["yum-hosted"], component.Component{Repo: "yum-hosted", Format: "yum", Name: "el9/demo", Version: "1.0-1.el9.x86_64"}, "admin")
	if err != nil || res.Assets != 1 {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if _, data = e.repodata(t, "yum-hosted", ""); strings.Contains(data["primary"], "<name>demo</name>") {
		t.Fatalf("after the delete:\n%s", data["primary"])
	}
	// a depth is checked on the directory given
	if w := put("yum-deep", "test.rpm", demo); w.Code != 400 || !strings.Contains(w.Body.String(), "depth 1") {
		t.Fatalf("above the depth: %d %s", w.Code, w.Body)
	}
	if w := put("yum-deep3", "7/os/test.rpm", demo); w.Code != 400 {
		t.Fatalf("above depth 3: %d %s", w.Code, w.Body)
	}
	if w := put("yum-deep3", "7/os/x86_64/test.rpm", demo); w.Code != 201 {
		t.Fatalf("at depth 3: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "yum-deep3", "7/os/x86_64/demo-1.0-1.el9.x86_64.rpm", nil); w.Code != 200 {
		t.Fatalf("stored at depth 3: %d", w.Code)
	}
}

// A privilege on some names of a repository also holds for the name the
// package is stored under, not only for the one the request gave.
func TestUploadNameIsCheckedWhereItIsStored(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.db.CreateContentSelector(ctx, meta.ContentSelector{Name: "mine", Expression: `path =^ "/mine"`}); err != nil {
		t.Fatal(err)
	}
	role := meta.Role{ID: "mine", Privileges: []meta.Privilege{{Target: "selector:mine", Actions: []string{meta.ActionAdd}, Repos: []string{"yum-hosted"}}}}
	if err := e.db.CreateRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	if err := e.db.CreateUser(ctx, meta.User{ID: "u", PasswordHash: "x", Roles: []string{"mine"}}); err != nil {
		t.Fatal(err)
	}
	a, err := auth.Open(ctx, e.db, e.db)
	if err != nil {
		t.Fatal(err)
	}
	e.h.Auth = a
	u, ok := a.Lookup("u")
	if !ok {
		t.Fatal("no principal")
	}
	if w := e.doAs(u, "PUT", "yum-hosted", "mine.rpm", nvra("other", "1", "1", "noarch")); w.Code != 403 {
		t.Fatalf("a name the privilege does not hold for: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "yum-hosted", "other-1-1.noarch.rpm", nil); w.Code != 404 {
		t.Fatalf("stored all the same: %d", w.Code)
	}
	if w := e.doAs(u, "PUT", "yum-hosted", "mine.rpm", nvra("mine", "1", "1", "noarch")); w.Code != 201 {
		t.Fatalf("one it holds for: %d %s", w.Code, w.Body)
	}
}
