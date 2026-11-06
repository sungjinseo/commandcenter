package pypi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
)

// What a Nexus PyPI repository does that scripts and clients rely on:
// writePolicy, signatures, deleting one file, a routing rule on the request
// path, merging groups, a proxy's root page and pip search.

const armored = "-----BEGIN PGP SIGNATURE-----\n\nwsBcBAEBCgAQBQJnc2ln\n=abcd\n-----END PGP SIGNATURE-----\n"

// form is an upload of the distribution file, with a signature when sig is set.
func form(t *testing.T, name, version, file, content, sig string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, kv := range [][2]string{{":action", "file_upload"}, {"name", name}, {"version", version}} {
		mw.WriteField(kv[0], kv[1])
	}
	if sig != "" {
		fw, _ := mw.CreateFormFile("gpg_signature", file+".asc")
		fw.Write([]byte(sig))
	}
	fw, _ := mw.CreateFormFile("content", file)
	fw.Write([]byte(content))
	mw.Close()
	return mw.FormDataContentType(), &buf
}

func (e *env) uploadAs(p auth.Principal, repoName, ct string, body *bytes.Buffer) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/repository/"+repoName+"/", body)
	r.Header.Set("Content-Type", ct)
	r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	e.h.Serve(w, r, e.repos[repoName], "")
	return w
}

// addRepos gives e more repositories, with the same upstream as py-proxy.
func (e *env) addRepos(t *testing.T, defs ...config.Repository) {
	t.Helper()
	cfg := &config.Config{}
	for _, rp := range e.repos {
		cfg.Repositories = append(cfg.Repositories, rp.Repository)
	}
	for i := range defs {
		if defs[i].Type == "proxy" {
			defs[i].Remote = e.repos["py-proxy"].Remote
			defs[i].MetadataMaxAge, defs[i].ContentMaxAge, defs[i].NegativeCacheTTL = config.Duration(time.Hour), config.Never, config.Duration(time.Minute)
		}
	}
	cfg.Repositories = append(cfg.Repositories, defs...)
	e.repos = repo.Build(cfg)
}

// writePolicy allow: the same file again replaces it, for a user with Edit;
// allow_once refuses it in words twine's --skip-existing knows; deny refuses
// every upload.
func TestWritePolicy(t *testing.T) {
	e := newEnv(t)
	e.addRepos(t, config.Repository{Name: "py-dev", Format: "pypi", Type: "hosted", WritePolicy: "allow"})
	a, ps := repotest.Principals(t, e.h.Store.DB, "py-dev", map[string][]string{
		"adder":  {meta.ActionAdd},
		"editor": {meta.ActionAdd, meta.ActionEdit},
	})
	e.h.Auth = a
	const file = "smoke_hello-1.0.0-py3-none-any.whl"
	build := func(summary string) string {
		return dist(t, "smoke-hello", "1.0.0", file, "Summary: "+summary+"\n")
	}
	first, second := build("first"), build("second")
	up := func(user, content, sig string) int {
		t.Helper()
		ct, body := form(t, "smoke-hello", "1.0.0", file, content, sig)
		w := e.uploadAs(ps[user], "py-dev", ct, body)
		return w.Code
	}
	if code := up("adder", first, armored); code != 200 {
		t.Fatalf("first upload: %d", code)
	}
	if code := up("adder", second, ""); code != 403 {
		t.Fatalf("an add-only user replacing a file: %d, want 403", code)
	}
	if got := e.do("GET", "py-dev", "packages/smoke-hello/"+file, ""); got.Body.String() != first {
		t.Fatal("a refused replace changed the file")
	}
	if code := up("editor", second, ""); code != 200 {
		t.Fatalf("replace: %d", code)
	}
	if got := e.do("GET", "py-dev", "packages/smoke-hello/"+file, ""); got.Body.String() != second {
		t.Fatal("the file was not replaced")
	}
	if got := e.do("GET", "py-dev", "packages/smoke-hello/"+file+".metadata", ""); !strings.Contains(got.Body.String(), "Summary: second") {
		t.Fatalf("the core metadata was not replaced: %s", got.Body)
	}
	if got := e.do("GET", "py-dev", "packages/smoke-hello/"+file+".asc", ""); got.Code != 404 {
		t.Fatalf("the signature of the replaced file stayed: %d", got.Code)
	}
	page := e.do("GET", "py-dev", "simple/smoke-hello/", "").Body.String()
	if strings.Count(page, "<a ") != 1 || !strings.Contains(page, "#sha256="+sha(second)) || strings.Contains(page, "data-gpg-sig") {
		t.Fatalf("page after the replace: %s", page)
	}
	// another spelling of the same file replaces it too
	const spelled = "Smoke.Hello-1.0-py3-none-any.whl"
	content := string(wheelBytes(t, map[string]string{"Smoke.Hello-1.0.dist-info/METADATA": "Name: smoke-hello\nVersion: 1.0\nSummary: third\n"}))
	ct, body := form(t, "smoke-hello", "1.0", spelled, content, "")
	if w := e.uploadAs(ps["editor"], "py-dev", ct, body); w.Code != 200 {
		t.Fatalf("another spelling: %d %s", w.Code, w.Body)
	}
	page = e.do("GET", "py-dev", "simple/smoke-hello/", "").Body.String()
	if strings.Count(page, "<a ") != 1 || !strings.Contains(page, spelled) {
		t.Fatalf("page after another spelling: %s", page)
	}
	if got := e.do("GET", "py-dev", "packages/smoke-hello/"+file, ""); got.Code != 404 {
		t.Fatalf("the old spelling stayed: %d", got.Code)
	}
	// allow_once: refused, whoever asks, in words twine's --skip-existing knows
	e.h.Auth = nil
	ct, body = form(t, "smoke-hello", "1.0.0", file, first, "")
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	ct, body = form(t, "smoke-hello", "1.0.0", file, second, "")
	if w := e.upload("py-hosted", ct, body); w.Code != 400 || !strings.Contains(w.Body.String(), "already exist") {
		t.Fatalf("allow_once: %d %s", w.Code, w.Body)
	}
	if got := e.do("GET", "py-hosted", "packages/smoke-hello/"+file, ""); got.Body.String() != first {
		t.Fatal("allow_once replaced the file")
	}
	ct, body = form(t, "smoke-hello", "1.0.0", file, first, "")
	if w := e.upload("py-ro", ct, body); w.Code != 400 || !strings.Contains(w.Body.String(), "read-only") {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
}

// A gpg_signature part is stored at <file>.asc, served at both path forms and
// through the group, marked on the file's link and in the JSON API, and goes
// with its file.
func TestSignature(t *testing.T) {
	e := newEnv(t)
	sink := &repotest.Sink{}
	e.h.Store.Events = sink
	const file = "smoke_hello-1.0.0.tar.gz"
	sd := smoke(t)
	ct, body := form(t, "Smoke_Hello", "1.0.0", file, sd, armored)
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	for _, c := range [][2]string{{"py-hosted", "packages/smoke-hello/" + file + ".asc"}, {"py-hosted", "packages/smoke-hello/1.0.0/" + file + ".asc"}, {"py-all", "packages/smoke-hello/" + file + ".asc"}} {
		if w := e.do("GET", c[0], c[1], ""); w.Code != 200 || w.Body.String() != armored {
			t.Fatalf("GET %s %s: %d %s", c[0], c[1], w.Code, w.Body)
		}
	}
	if page := e.do("GET", "py-hosted", "simple/smoke-hello/", "").Body.String(); !strings.Contains(page, `data-gpg-sig="true"`) || strings.Count(page, "<a ") != 1 {
		t.Fatalf("page: %s", page)
	}
	if page := e.do("GET", "py-hosted", "simple/smoke-hello/", jsonType).Body.String(); !strings.Contains(page, `"gpg-sig":true`) {
		t.Fatalf("json page: %s", page)
	}
	if doc := e.do("GET", "py-hosted", "pypi/smoke-hello/json", "").Body.String(); !strings.Contains(doc, `"has_sig":true`) {
		t.Fatalf("json api: %s", doc)
	}
	for _, c := range []struct {
		name, sig string
		want      int
	}{
		{"a binary signature", "\x89\x01", 400},
		{"an oversized signature", "-----BEGIN PGP SIGNATURE-----\n" + strings.Repeat("a", maxSignature), 413},
	} {
		sd := dist(t, "other", "1.0", "other-1.0.tar.gz")
		ct, body := form(t, "other", "1.0", "other-1.0.tar.gz", sd, c.sig)
		if w := e.upload("py-hosted", ct, body); w.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.want)
		}
	}
	// a file that never had one has none, and the page says nothing
	sd2 := dist(t, "other", "1.0", "other-1.0.tar.gz")
	ct, body = form(t, "other", "1.0", "other-1.0.tar.gz", sd2, "")
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := e.do("GET", "py-hosted", "packages/other/other-1.0.tar.gz.asc", ""); w.Code != 404 {
		t.Fatalf("no signature: %d", w.Code)
	}
	if page := e.do("GET", "py-hosted", "simple/other/", "").Body.String(); strings.Contains(page, "data-gpg-sig") {
		t.Fatalf("page of an unsigned file: %s", page)
	}
	_, assets := pypiState(t, e)
	if !strings.Contains(assets, "packages/smoke-hello/"+file+".asc") {
		t.Fatalf("assets: %s", assets)
	}
}

// A proxy serves a signature its upstream index marks, from <file URL>.asc.
func TestSignatureThroughProxy(t *testing.T) {
	var asked []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch r.URL.Path {
		case "/simple/signed/":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<a href="/files/signed-1.0.tar.gz" data-gpg-sig="true">signed-1.0.tar.gz</a><a href="/files/signed-2.0.tar.gz">signed-2.0.tar.gz</a>`+
				`<a href="/files/signed-3.0.tar.gz.asc">signed-3.0.tar.gz.asc</a>`) // a Nexus page lists a signature as a file
		case "/files/signed-1.0.tar.gz.asc", "/files/signed-3.0.tar.gz.asc":
			io.WriteString(w, armored)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	e := newEnv(t)
	e.repos["py-proxy"].RemoteURL.Scheme, e.repos["py-proxy"].RemoteURL.Host = "http", strings.TrimPrefix(up.URL, "http://")
	e.h.Fetcher = proxy.New(e.h.Store, up.Client(), 4)
	if w := e.do("GET", "py-proxy", "packages/signed/signed-1.0.tar.gz.asc", ""); w.Code != 200 || w.Body.String() != armored {
		t.Fatalf("marked: %d %s (%v)", w.Code, w.Body, asked)
	}
	if w := e.do("GET", "py-proxy", "packages/signed/signed-2.0.tar.gz.asc", ""); w.Code != 404 {
		t.Fatalf("not marked: %d", w.Code)
	}
	if w := e.do("GET", "py-proxy", "packages/signed/signed-3.0.tar.gz.asc", ""); w.Code != 200 || w.Body.String() != armored {
		t.Fatalf("listed as a file: %d %s", w.Code, w.Body)
	}
	if page := e.do("GET", "py-proxy", "simple/signed/", "").Body.String(); strings.Count(page, `data-gpg-sig="true"`) != 1 {
		t.Fatalf("page: %s", page)
	}
	for _, p := range asked {
		if p == "/files/signed-2.0.tar.gz.asc" {
			t.Fatal("asked the remote for a signature its index does not mark")
		}
	}
}

// DELETE of one file: the file with its row, core metadata and signature; the
// signature alone; the project ends with its last file.
func TestDeleteFile(t *testing.T) {
	e := newEnv(t)
	sink := &repotest.Sink{}
	e.h.Store.Events = sink
	const sdist, wheel = "smoke_hello-1.0.0.tar.gz", "smoke_hello-1.0.0-py3-none-any.whl"
	for _, f := range []string{sdist, wheel} {
		ct, body := form(t, "smoke-hello", "1.0.0", f, dist(t, "smoke-hello", "1.0.0", f), armored)
		if w := e.upload("py-hosted", ct, body); w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	sink.Reset()
	del := func(rp, file string) *httptest.ResponseRecorder {
		return e.do("DELETE", rp, "packages/smoke-hello/"+file, "")
	}
	if w := del("py-hosted", wheel+".metadata"); w.Code != 405 {
		t.Fatalf("a file's core metadata: %d", w.Code)
	}
	if w := del("py-hosted", "nothere-1.0.tar.gz"); w.Code != 404 {
		t.Fatalf("no such file: %d", w.Code)
	}
	if w := del("py-hosted", wheel+".asc"); w.Code != 204 {
		t.Fatalf("a signature: %d %s", w.Code, w.Body)
	}
	if page := e.do("GET", "py-hosted", "simple/smoke-hello/", "").Body.String(); strings.Count(page, `data-gpg-sig="true"`) != 1 || strings.Count(page, "<a ") != 2 {
		t.Fatalf("page without the wheel's signature: %s", page)
	}
	if w := del("py-hosted", wheel+".asc"); w.Code != 404 {
		t.Fatalf("that signature again: %d", w.Code)
	}
	if w := del("py-hosted", wheel); w.Code != 204 {
		t.Fatalf("the wheel: %d %s", w.Code, w.Body)
	}
	if got := sink.Events(); len(got) != 2 || got[1].Kind != "delete" || got[1].Subject != wheel {
		t.Fatalf("events: %v", got)
	}
	page := e.do("GET", "py-hosted", "simple/smoke-hello/", "").Body.String()
	if strings.Count(page, "<a ") != 1 || !strings.Contains(page, sdist) {
		t.Fatalf("page: %s", page)
	}
	if _, assets := pypiState(t, e); strings.Contains(assets, wheel) {
		t.Fatalf("the wheel left: %s", assets)
	}
	// by the path form of Nexus, through the group: the last file ends the project
	if w := e.do("DELETE", "py-all", "packages/smoke-hello/1.0.0/"+sdist, ""); w.Code != 204 {
		t.Fatalf("through the group: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "py-hosted", "simple/smoke-hello/", ""); w.Code != 404 {
		t.Fatalf("the project after its last file: %d", w.Code)
	}
	if rows, assets := pypiState(t, e); rows != "" || assets != "" {
		t.Fatalf("left: rows %q assets %q", rows, assets)
	}
	if w := e.do("DELETE", "py-ro", "packages/smoke-hello/"+sdist, ""); w.Code != 400 || !strings.Contains(w.Body.String(), "read-only") {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "py-proxy", "packages/six/six-1.16.0-py2.py3-none-any.whl", ""); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("a proxy: %d Allow %q", w.Code, w.Header().Get("Allow"))
	}
	if w := e.do("POST", "py-hosted", "packages/smoke-hello/"+sdist, ""); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD, DELETE" {
		t.Fatalf("Allow: %d %q", w.Code, w.Header().Get("Allow"))
	}
}

// A proxy's routing rule sees a file's request path, whichever host the index
// names and cached or not, directly and through a group.
func TestRoutingRuleSeesRequestPath(t *testing.T) {
	e := newEnv(t)
	const wheel = "six-1.16.0-py2.py3-none-any.whl"
	// cached before the rule exists
	for _, p := range []string{"packages/legacy/legacy-1.0.tar.gz.metadata", "packages/legacy/legacy-1.0.tar.gz"} {
		if w := e.do("GET", "py-proxy", p, ""); w.Code != 200 {
			t.Fatal(p, w.Code, w.Body)
		}
	}
	rule, err := repo.CompileRoute(meta.RoutingRule{Name: "only-six", Mode: meta.RoutingAllow, Matchers: []string{`^/simple/six/.*`, `^/packages/six/.*`}})
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	e.h.Fetcher.RouteAllowed = func(rp *repo.Repository, path string) bool {
		seen = append(seen, path)
		ok, _ := rule.Allowed(path)
		return ok
	}
	for _, rp := range []string{"py-proxy", "py-all"} {
		if w := e.do("GET", rp, "packages/six/"+wheel, ""); w.Code != 200 || w.Body.String() != "six-wheel" {
			t.Fatalf("%s: an allowed file whose URL is /files/...: %d %s (the rule saw %v)", rp, w.Code, w.Body, seen)
		}
		if w := e.do("GET", rp, "packages/legacy/legacy-1.0.tar.gz", ""); w.Code != 404 {
			t.Fatalf("%s: a cached file the rule refuses: %d", rp, w.Code)
		}
		if w := e.do("GET", rp, "packages/legacy/legacy-1.0.tar.gz.metadata", ""); w.Code != 404 {
			t.Fatalf("%s: its core metadata: %d", rp, w.Code)
		}
	}
	for _, p := range seen {
		if strings.HasPrefix(p, "/files/") {
			t.Fatalf("the rule saw an upstream path: %v", seen)
		}
	}
}

// A sign-in or error page answered with 200 has links, but none to a file of
// the project. It is refused before it can replace anything (checkPage): the
// fetch has failed, so a group's next member does not stand in, nothing stays
// cached, and a page cached before keeps answering. A page that lists files
// of other projects too lists only this one's.
func TestUpstreamPageWithoutFilesOfTheProject(t *testing.T) {
	var internal atomic.Value
	internal.Store(`<html><a href="/login">Sign in</a><a href="/help.tar.gz">help.tar.gz</a></html>`)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/internal/":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, internal.Load().(string))
		case "/simple/nolinks/":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><body>Sign in</body></html>`)
		case "/simple/jsonerror/":
			w.Header().Set("Content-Type", jsonType)
			io.WriteString(w, `{"error":"maintenance"}`)
		case "/simple/empty/":
			w.Header().Set("Content-Type", jsonType)
			io.WriteString(w, `{"meta":{"api-version":"1.0"},"name":"empty","files":[]}`)
		case "/simple/mixed/":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<a href="/files/other-1.0.tar.gz">other-1.0.tar.gz</a><a href="/files/Mixed-2.0.tar.gz">Mixed-2.0.tar.gz</a>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	e := newEnv(t)
	e.addRepos(t, config.Repository{Name: "py-first", Format: "pypi", Type: "group", Members: []string{"py-proxy", "py-hosted"}})
	e.repos["py-proxy"].RemoteURL.Scheme, e.repos["py-proxy"].RemoteURL.Host = "http", strings.TrimPrefix(up.URL, "http://")
	e.h.Fetcher = proxy.New(e.h.Store, up.Client(), 4)
	ctx := context.Background()
	const own = "internal-1.0-py3-none-any.whl"
	content := dist(t, "internal", "1.0", own)
	ct, body := form(t, "internal", "1.0", own, content, "")
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	for _, c := range []struct {
		repoName, project string
		want              int
	}{
		{"py-proxy", "internal", 502}, {"py-first", "internal", 502}, // the next member has one of that name: it does not stand in
		{"py-proxy", "nolinks", 502}, {"py-proxy", "jsonerror", 502},
		{"py-proxy", "empty", 404}, // a project without files is a page, and nothing to list
	} {
		if w := e.do("GET", c.repoName, "simple/"+c.project+"/", ""); w.Code != c.want {
			t.Fatalf("%s %s: %d %s, want %d", c.repoName, c.project, w.Code, w.Body, c.want)
		}
		if _, err := e.h.Store.Get(ctx, "py-proxy", "simple/"+c.project+"/index"); c.want == 502 && err == nil {
			t.Fatalf("%s was cached", c.project)
		}
	}
	page := e.do("GET", "py-proxy", "simple/mixed/", "").Body.String()
	if strings.Count(page, "<a ") != 1 || !strings.Contains(page, "Mixed-2.0.tar.gz") {
		t.Fatalf("mixed page: %s", page)
	}

	// The real page, served at once, and then a sign-in page at its refresh.
	signIn := internal.Load().(string)
	internal.Store(`<a href="/files/internal-2.0.tar.gz">internal-2.0.tar.gz</a>`)
	if w := e.do("GET", "py-proxy", "simple/internal/", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "internal-2.0.tar.gz") {
		t.Fatalf("the page after the sign-in pages: %d %s", w.Code, w.Body)
	}
	cached, err := e.h.Store.Get(ctx, "py-proxy", "simple/internal/index")
	if err != nil {
		t.Fatal(err)
	}
	internal.Store(signIn)
	e.h.Fetcher.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	for _, name := range []string{"py-proxy", "py-first"} {
		w := e.do("GET", name, "simple/internal/", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "internal-2.0.tar.gz") {
			t.Fatalf("%s during the sign-in page: %d %s", name, w.Code, w.Body)
		}
	}
	if now, err := e.h.Store.Get(ctx, "py-proxy", "simple/internal/index"); err != nil || now.BlobSHA256 != cached.BlobSHA256 {
		t.Fatalf("the sign-in page replaced the page: %+v %v", now, err)
	}
}

// mergeMembers: a group lists the files of a project of every member, the first
// member's of a file name, and serves each from the first member that has it.
// Without it the first member that has the project answers alone.
func TestMergeMembers(t *testing.T) {
	e := newEnv(t)
	merged := config.Repository{Name: "py-merged", Format: "pypi", Type: "group", Members: []string{"py-hosted", "py-proxy"}, MergeMembers: true}
	e.addRepos(t, merged)
	const own = "six-99.0-py3-none-any.whl"
	content := dist(t, "six", "99.0", own)
	ct, body := form(t, "six", "99.0", own, content, "")
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	const public = "six-1.16.0-py2.py3-none-any.whl"
	page := e.do("GET", "py-all", "simple/six/", "").Body.String()
	if strings.Count(page, "<a ") != 1 || !strings.Contains(page, own) {
		t.Fatalf("the default: the first member alone: %s", page)
	}
	if w := e.do("GET", "py-all", "packages/six/"+public, ""); w.Code != 404 {
		t.Fatalf("the default: a later member's file: %d", w.Code)
	}
	for _, accept := range []string{"", jsonType} {
		page = e.do("GET", "py-merged", "simple/six/", accept).Body.String()
		if !strings.Contains(page, own) || !strings.Contains(page, public) || !strings.Contains(page, "/repository/py-merged/packages/six/"+public) {
			t.Fatalf("merged (%q): %s", accept, page)
		}
	}
	if w := e.do("GET", "py-merged", "packages/six/"+own, ""); w.Code != 200 || w.Body.String() != content {
		t.Fatalf("merged: the hosted file: %d", w.Code)
	}
	if w := e.do("GET", "py-merged", "packages/six/"+public, ""); w.Code != 200 || w.Body.String() != "six-wheel" {
		t.Fatalf("merged: the proxy's file: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "py-merged", "simple/nosuchproject/", ""); w.Code != 404 {
		t.Fatalf("merged: no member has it: %d", w.Code)
	}
	if w := e.do("GET", "py-merged", "packages/six/six-0.1.tar.gz", ""); w.Code != 404 {
		t.Fatalf("merged: no member has the file: %d", w.Code)
	}
	// the first member's file of a name: the same name in the proxy stays out
	same := dist(t, "legacy", "1.0", "legacy-1.0.tar.gz")
	ct, body = form(t, "legacy", "1.0", "legacy-1.0.tar.gz", same, "")
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	page = e.do("GET", "py-merged", "simple/legacy/", "").Body.String()
	if strings.Count(page, "<a ") != 1 || !strings.Contains(page, sha(same)) {
		t.Fatalf("merged: a name two members have: %s", page)
	}
	if w := e.do("GET", "py-merged", "packages/legacy/legacy-1.0.tar.gz", ""); w.Body.String() != same {
		t.Fatal("merged: the first member's file")
	}
	// a member that fails is passed over while another has the project
	const fresh = "fresh-1.0.tar.gz" // the proxy has no cached page of it, so it can only fail
	ct, body = form(t, "fresh", "1.0", fresh, dist(t, "fresh", "1.0", fresh), "")
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	e.repos["py-proxy"].RemoteURL.Host = "127.0.0.1:1"
	e.h.Store.DB.InvalidateCache(context.Background(), []string{"py-proxy"})
	if w := e.do("GET", "py-merged", "simple/fresh/", ""); w.Code != 200 || !strings.Contains(w.Body.String(), fresh) {
		t.Fatalf("merged: a failing member: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "py-merged", "packages/fresh/"+fresh, ""); w.Code != 200 {
		t.Fatalf("merged: the hosted file with a failing member: %d", w.Code)
	}
	if w := e.do("GET", "py-merged", "pypi/fresh/json", ""); w.Code != 200 || !strings.Contains(w.Body.String(), fresh) {
		t.Fatalf("merged: the JSON API with a failing member: %d", w.Code)
	}
	// nothing else has it: the failure is the answer, not "no such project"
	if w := e.do("GET", "py-merged", "simple/onlyupstream/", ""); w.Code != 502 {
		t.Fatalf("merged: only a failing member could have it: %d %s", w.Code, w.Body)
	}
}

// A proxy's root page lists its remote's projects, then what it has cached;
// without a usable remote page the cached ones. A group lists its members'.
func TestProxyRootPage(t *testing.T) {
	var root, asked []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/":
			asked = append(asked, r.Header.Get("Accept")...)
			if root == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			w.Write(root)
		case "/simple/cached/":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<a href="/files/cached-1.0.tar.gz">cached-1.0.tar.gz</a>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	e := newEnv(t)
	e.repos["py-proxy"].RemoteURL.Scheme, e.repos["py-proxy"].RemoteURL.Host = "http", strings.TrimPrefix(up.URL, "http://")
	e.repos["py-proxy"].NegativeCacheTTL = config.Zero
	e.h.Fetcher = proxy.New(e.h.Store, up.Client(), 4)
	if w := e.do("GET", "py-proxy", "simple/cached/", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	e.seedHosted(t, "hosted-only", "hosted_only-1.0.tar.gz", "x")
	names := func(rp, accept string) string {
		t.Helper()
		w := e.do("GET", rp, "simple/", accept)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", rp, w.Code, w.Body)
		}
		var out []string
		if accept == jsonType {
			var doc struct {
				Projects []struct{ Name string } `json:"projects"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
				t.Fatalf("%v: %s", err, w.Body)
			}
			for _, p := range doc.Projects {
				out = append(out, p.Name)
			}
		} else {
			parseHTMLIndex(w.Body, e.repos["py-proxy"].RemoteURL, func(f File) error {
				out = append(out, f.Filename)
				return nil
			})
		}
		return strings.Join(out, ",")
	}
	// no root page at the remote: as before
	if got := names("py-proxy", ""); got != "cached" {
		t.Fatalf("a remote without a root page: %s", got)
	}
	root = []byte(`<!DOCTYPE html><html><body><a href="/simple/six/">six</a>
<a href="/simple/zope-interface/">zope.interface</a><a href="/simple/cached/">Cached</a><a href="not a name">not a name!</a></body></html>`)
	e.h.Store.DB.InvalidateCache(context.Background(), []string{"py-proxy"})
	for _, accept := range []string{"", jsonType} {
		if got := names("py-proxy", accept); got != "cached,six,zope-interface" {
			t.Fatalf("proxy (%q): %s", accept, got)
		}
		if got := names("py-all", accept); got != "cached,hosted-only,six,zope-interface" {
			t.Fatalf("group (%q): %s", accept, got)
		}
	}
	if !bytes.Contains(asked, []byte("text/html")) {
		t.Fatalf("Accept: %s", asked)
	}
	// cached for metadataMaxAge; a HEAD does not read it
	n := len(asked)
	names("py-proxy", "")
	if len(asked) != n {
		t.Fatal("the remote's page was asked for again within metadataMaxAge")
	}
	e.h.Store.DB.InvalidateCache(context.Background(), []string{"py-proxy"})
	if w := e.do("HEAD", "py-all", "simple/", ""); w.Code != 200 || len(asked) != n {
		t.Fatalf("HEAD: %d, and the remote's page was asked for: %v", w.Code, len(asked) != n)
	}
	// the remote is gone and the page is cached, or has never been: the list still answers
	up.Close()
	e.h.Store.DB.InvalidateCache(context.Background(), []string{"py-proxy"})
	if got := names("py-proxy", ""); got != "cached,six,zope-interface" {
		t.Fatalf("a remote that went away: %s", got)
	}
	if _, assets := pypiState(t, e); strings.Contains(assets, rootKey) {
		t.Fatalf("the proxy's root page among the hosted repository's: %s", assets)
	}
}

// A client that leaves during the list stops the reading, and nothing is
// answered after what went out.
func TestRootPageClientLeaves(t *testing.T) {
	e := newEnv(t)
	e.seedHosted(t, "hosted-only", "hosted_only-1.0.tar.gz", "x")
	w := &failingWriter{hdr: http.Header{}}
	e.h.Serve(w, httptest.NewRequest("GET", "/repository/py-hosted/simple/", nil), e.repos["py-hosted"], "simple/")
	if w.code != 0 {
		t.Fatalf("answered %d after the client left", w.code)
	}
}

type failingWriter struct {
	hdr  http.Header
	code int
}

func (f *failingWriter) Header() http.Header       { return f.hdr }
func (f *failingWriter) WriteHeader(code int)      { f.code = code }
func (f *failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// A large page of a remote is listed whole, streamed: pypi.org's has 930 000
// projects (test/smoke/pypi-smoke.sh reads it in the server's 128 MiB).
func TestProxyRootPageLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("a large page")
	}
	const projects = 400000
	var page bytes.Buffer
	page.WriteString("<!DOCTYPE html><html><body>\n")
	for i := 0; i < projects; i++ {
		fmt.Fprintf(&page, "<a href=\"/simple/project-%d/\">project-%d</a>\n", i, i)
	}
	page.WriteString("</body></html>\n")
	dir := t.TempDir()
	b, _ := blob.Open(dir)
	db, err := meta.Open(metatest.URL(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(page.Bytes())
	}))
	defer up.Close()
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: time.Now}
	rps := repo.Build(&config.Config{Repositories: []config.Repository{{Name: "px", Format: "pypi", Type: "proxy", Remote: up.URL + "/", MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never}}})
	h := New(store, proxy.New(store, up.Client(), 4), func() string { return baseURL })
	w := &lineCounter{hdr: http.Header{}}
	start := time.Now()
	h.Serve(w, httptest.NewRequest("GET", "/repository/px/simple/", nil), rps["px"], "simple/")
	t.Logf("%d projects, %d MiB: %v", projects, page.Len()>>20, time.Since(start))
	if want := projects + 3; w.lines != want { // the page's head is two lines, its end one
		t.Fatalf("lines: %d, want %d", w.lines, want)
	}
}

type lineCounter struct {
	hdr   http.Header
	lines int
}

func (c *lineCounter) Header() http.Header { return c.hdr }
func (c *lineCounter) WriteHeader(int)     {}
func (c *lineCounter) Write(b []byte) (int, error) {
	c.lines += bytes.Count(b, []byte("\n"))
	return len(b), nil
}

// pip search: search({"name": [...], "summary": [...]}, "or") on POST pypi.
func TestPipSearch(t *testing.T) {
	e := newEnv(t)
	for _, c := range [][3]string{
		{"smoke-hello", "1.0.0", "A friendly <greeter> & more"},
		{"smoke-hello", "1.1.0", "A friendly <greeter> & more"},
		{"other", "2.0", "Goes up in smoke"},
		{"quiet", "3.0", "Nothing to see"},
	} {
		file := strings.ReplaceAll(c[0], "-", "_") + "-" + c[1] + ".tar.gz"
		ct, body := form(t, c[0], c[1], file, dist(t, c[0], c[1], file, "Summary: "+c[2]+"\n"), "")
		if w := e.upload("py-hosted", ct, body); w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	call := func(rp, term, operator string) string {
		t.Helper()
		v := "<value><array><data><value><string>" + term + "</string></value></data></array></value>"
		xml := `<?xml version="1.0"?><methodCall><methodName>search</methodName><params><param><value><struct><member><name>name</name>` + v + `</member><member><name>summary</name>` + v + `</member></struct></value></param><param><value><string>` + operator + `</string></value></param></params></methodCall>`
		w := httptest.NewRecorder()
		e.h.Serve(w, httptest.NewRequest("POST", "/repository/"+rp+"/pypi", strings.NewReader(xml)), e.repos[rp], "pypi")
		if w.Code != 200 || w.Header().Get("Content-Type") != "text/xml" {
			t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body)
		}
		return w.Body.String()
	}
	got := call("py-hosted", "SMOKE", "or")
	for _, want := range []string{"<string>smoke-hello</string>", "<string>1.0.0</string>", "<string>1.1.0</string>", "<string>other</string>", "A friendly &lt;greeter&gt; &amp; more", "<boolean>0</boolean>"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %s in %s", want, got)
		}
	}
	if strings.Contains(got, "quiet") {
		t.Errorf("a project that does not match: %s", got)
	}
	if got := call("py-all", "quiet", "or"); !strings.Contains(got, "<string>3.0</string>") {
		t.Errorf("through the group: %s", got)
	}
	if got := call("py-proxy", "six", "or"); strings.Contains(got, "<struct>") || !strings.Contains(got, "<data></data>") {
		t.Errorf("a proxy has nothing to say: %s", got)
	}
	if got := call("py-hosted", "smoke", "and"); !strings.Contains(got, "<fault>") || !strings.Contains(got, "operator") {
		t.Errorf("another operator: %s", got)
	}
	for _, bad := range []string{"not xml", `<methodCall><methodName>list_packages</methodName></methodCall>`,
		`<methodCall><methodName>search</methodName><params><param><value><struct><member><name>keywords</name><value><string>x</string></value></member></struct></value></param></params></methodCall>`} {
		w := httptest.NewRecorder()
		e.h.Serve(w, httptest.NewRequest("POST", "/repository/py-hosted/pypi", strings.NewReader(bad)), e.repos["py-hosted"], "pypi")
		if !strings.Contains(w.Body.String(), "<fault>") {
			t.Errorf("%.40s: %d %s", bad, w.Code, w.Body)
		}
	}
	if w := e.do("GET", "py-hosted", "pypi", ""); w.Code != 405 {
		t.Fatalf("GET: %d", w.Code)
	}
	// a stored version pip cannot parse is no hit (pip stops at it)
	e.seedHosted(t, "smoke-legacy", "smoke_legacy-1.0.tar.gz", "x")
	ctx := context.Background()
	if err := e.h.Store.DB.Tx(ctx, func(tx *meta.Tx) error {
		return tx.UpdatePackage(ctx, "py-hosted", "smoke-legacy", "smoke_legacy-1.0.tar.gz", `{"filename":"smoke_legacy-1.0.tar.gz","version":"not a version"}`)
	}); err != nil {
		t.Fatal(err)
	}
	if got := call("py-hosted", "smoke-legacy", "or"); strings.Contains(got, "<struct>") {
		t.Errorf("a version pip cannot parse: %s", got)
	}
}
