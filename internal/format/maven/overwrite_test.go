package maven

import (
	"net/http/httptest"
	"strings"
	"testing"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo/repotest"
)

func (e *env) doAs(p auth.Principal, method, repoName, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repoName+"/"+path, strings.NewReader(body))
	r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

// Decision 2: overwriting an existing asset needs Edit; new paths, metadata
// and checksums need only Add.
func TestOverwriteNeedsEdit(t *testing.T) {
	e := newEnv(t)
	a, ps := repotest.Principals(t, e.h.Store.DB, "snapshots", map[string][]string{
		"adder":  {meta.ActionAdd},
		"editor": {meta.ActionAdd, meta.ActionEdit},
	})
	e.h.Auth = a
	adder, editor := ps["adder"], ps["editor"]
	const jar = "org/example/app/1.0-SNAPSHOT/app-1.0-SNAPSHOT.jar"
	const md = "org/example/app/1.0-SNAPSHOT/maven-metadata.xml"
	if w := e.doAs(adder, "PUT", "snapshots", jar, "a"); w.Code != 201 {
		t.Fatalf("add-only new path: %d %s", w.Code, w.Body)
	}
	if w := e.doAs(adder, "PUT", "snapshots", jar+".sha1", "x"); w.Code != 201 {
		t.Fatalf("add-only checksum: %d %s", w.Code, w.Body)
	}
	meta := `<metadata><groupId>org.example</groupId><artifactId>app</artifactId><version>1.0-SNAPSHOT</version></metadata>`
	for i := 0; i < 2; i++ {
		if w := e.doAs(adder, "PUT", "snapshots", md, meta); w.Code != 201 {
			t.Fatalf("add-only metadata %d: %d %s", i, w.Code, w.Body)
		}
	}
	if w := e.doAs(adder, "PUT", "snapshots", jar, "b"); w.Code != 403 {
		t.Fatalf("add-only overwrite: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "snapshots", jar, ""); w.Body.String() != "a" {
		t.Fatalf("refused overwrite changed the asset: %q", w.Body)
	}
	if w := e.doAs(editor, "PUT", "snapshots", jar, "c"); w.Code != 201 {
		t.Fatalf("edit overwrite: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "snapshots", jar, ""); w.Body.String() != "c" {
		t.Fatalf("overwrite not stored: %q", w.Body)
	}
}
