package pypi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

const baseURL = "http://repo.test"

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type env struct {
	h     *Handler
	repos map[string]*repo.Repository
	hits  *atomic.Int32
}

// fakeIndex serves a tiny PyPI: a JSON page for six, an HTML page for legacy,
// and the files on a separate path (like files.pythonhosted.org).
func fakeIndex(hits *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/six/":
			if !strings.Contains(r.Header.Get("Accept"), jsonType) {
				http.Error(w, "want json", 406)
				return
			}
			w.Header().Set("Content-Type", jsonType)
			io.WriteString(w, `{"meta":{"api-version":"1.0"},"name":"six","files":[
 {"filename":"six-1.16.0-py2.py3-none-any.whl","url":"/files/six-1.16.0-py2.py3-none-any.whl","hashes":{"sha256":"`+sha("six-wheel")+`"}},
 {"filename":"six-1.0.0.tar.gz","url":"/files/six-1.0.0.tar.gz","hashes":{"sha256":"`+sha("something else")+`"}}]}`)
		case "/simple/legacy/":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<a href="../../files/legacy-1.0.tar.gz#sha256=`+sha("legacy")+`" data-requires-python="&gt;=3.8">legacy-1.0.tar.gz</a>`)
		case "/files/six-1.16.0-py2.py3-none-any.whl":
			hits.Add(1)
			io.WriteString(w, "six-wheel")
		case "/files/six-1.0.0.tar.gz":
			io.WriteString(w, "tampered")
		case "/files/legacy-1.0.tar.gz":
			io.WriteString(w, "legacy")
		default:
			http.NotFound(w, r)
		}
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	hits := &atomic.Int32{}
	ts := httptest.NewServer(fakeIndex(hits))
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	b, _ := blob.Open(dir)
	db, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: time.Now}
	cfg := &config.Config{Repositories: []config.Repository{
		{Name: "py-hosted", Format: "pypi", Type: "hosted", WritePolicy: "allow_once"},
		{Name: "py-ro", Format: "pypi", Type: "hosted", WritePolicy: "deny"},
		{Name: "py-proxy", Format: "pypi", Type: "proxy", Remote: ts.URL + "/",
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "py-all", Format: "pypi", Type: "group", Members: []string{"py-hosted", "py-proxy"}, WriteMember: "py-hosted"},
	}}
	return &env{h: New(store, proxy.New(store, ts.Client(), 4), func() string { return baseURL }), repos: repo.Build(cfg), hits: hits}
}

func (e *env) do(method, repoName, path, accept string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repoName+"/"+path, nil)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

// seedHosted stores one hosted file the way an upload does.
func (e *env) seedHosted(t *testing.T, project, file, content string) {
	t.Helper()
	ctx := context.Background()
	sums, err := e.h.Store.WriteBlob("py-hosted", strings.NewReader(content), nil)
	if err != nil {
		t.Fatal(err)
	}
	attrs, _ := json.Marshal(fileAttrs{Filename: file, Version: "1.0", SHA256: sums.SHA256, RequiresPython: ">=3.9"})
	err = e.h.Store.DB.Tx(ctx, func(tx *meta.Tx) error {
		if err := tx.InsertPackage(ctx, meta.Package{Repo: "py-hosted", Format: "pypi", Name: project, Version: file, Attrs: string(attrs)}); err != nil {
			return err
		}
		return tx.PutAsset(ctx, e.h.Store.NewAsset("py-hosted", fileKey(project, file), "application/octet-stream", sums))
	})
	if err != nil {
		t.Fatal(err)
	}
}

const pipAccept = "application/vnd.pypi.simple.v1+json, application/vnd.pypi.simple.v1+html; q=0.1, text/html; q=0.01"

func TestProxyProjectPageAndDownload(t *testing.T) {
	e := newEnv(t)
	w := e.do("GET", "py-proxy", "simple/six/", "")
	body := w.Body.String()
	want := baseURL + "/repository/py-proxy/packages/six/six-1.16.0-py2.py3-none-any.whl#sha256=" + sha("six-wheel")
	if w.Code != 200 || w.Header().Get("Content-Type") != htmlType || !strings.Contains(body, `href="`+want+`"`) {
		t.Fatalf("%d %s", w.Code, body)
	}
	w = e.do("GET", "py-proxy", "simple/Six/", pipAccept)
	var doc struct{ Files []map[string]any }
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || len(doc.Files) != 2 || w.Header().Get("Content-Type") != jsonType {
		t.Fatalf("json: %s", w.Body.String())
	}
	for i := 0; i < 2; i++ {
		w = e.do("GET", "py-proxy", "packages/six/six-1.16.0-py2.py3-none-any.whl", "")
		if w.Code != 200 || w.Body.String() != "six-wheel" {
			t.Fatalf("download: %d %q", w.Code, w.Body.String())
		}
	}
	if e.hits.Load() != 1 {
		t.Fatalf("file fetched %d times, want 1 (cached)", e.hits.Load())
	}
	if w := e.do("GET", "py-proxy", "packages/six/six-1.0.0.tar.gz", ""); w.Code != 502 {
		t.Fatalf("sha256 mismatch: %d", w.Code)
	}
	if w := e.do("GET", "py-proxy", "packages/six/six-1.0.0.tar.gz", ""); w.Code != 404 {
		t.Fatalf("sha256 mismatch (second GET, negative-cached): %d", w.Code)
	}
	if w := e.do("GET", "py-proxy", "packages/six/six-9.9.tar.gz", ""); w.Code != 404 {
		t.Fatalf("unknown file: %d", w.Code)
	}
	if w := e.do("GET", "py-proxy", "simple/nope/", ""); w.Code != 404 {
		t.Fatalf("unknown project: %d", w.Code)
	}
}

func TestProxyHTMLUpstream(t *testing.T) {
	e := newEnv(t)
	w := e.do("GET", "py-proxy", "simple/legacy/", pipAccept)
	if !strings.Contains(w.Body.String(), `"requires-python":">=3.8"`) && !strings.Contains(w.Body.String(), `"requires-python":"\u003e=3.8"`) {
		t.Fatalf("json from html upstream: %s", w.Body.String())
	}
	if w := e.do("GET", "py-proxy", "packages/legacy/legacy-1.0.tar.gz", ""); w.Body.String() != "legacy" {
		t.Fatalf("download: %d %q", w.Code, w.Body.String())
	}
}

func TestHostedAndGroup(t *testing.T) {
	e := newEnv(t)
	e.seedHosted(t, "six", "six-2.0.0.tar.gz", "internal six")
	e.seedHosted(t, "internal-pkg", "internal_pkg-1.0.tar.gz", "internal")

	w := e.do("GET", "py-hosted", "simple/six/", "")
	if !strings.Contains(w.Body.String(), baseURL+"/repository/py-hosted/packages/six/six-2.0.0.tar.gz#sha256="+sha("internal six")) ||
		!strings.Contains(w.Body.String(), `data-requires-python="&gt;=3.9"`) {
		t.Fatalf("hosted page: %s", w.Body.String())
	}
	// py-hosted owns six: its files answer alone and the proxy's never mix in,
	// in either page format or by direct URL (dependency confusion, QA N-1).
	w = e.do("GET", "py-all", "simple/six/", "")
	body := w.Body.String()
	if strings.Count(body, "<a ") != 1 || !strings.Contains(body, "/repository/py-all/packages/six/six-2.0.0.tar.gz") {
		t.Fatalf("group page: %s", body)
	}
	w = e.do("GET", "py-all", "simple/six/", pipAccept)
	var doc struct{ Files []map[string]any }
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || len(doc.Files) != 1 || doc.Files[0]["filename"] != "six-2.0.0.tar.gz" {
		t.Fatalf("group json page: %s", w.Body.String())
	}
	if w := e.do("GET", "py-all", "packages/six/six-2.0.0.tar.gz", ""); w.Body.String() != "internal six" {
		t.Fatalf("group hosted file: %q", w.Body.String())
	}
	if w := e.do("GET", "py-all", "packages/six/six-1.16.0-py2.py3-none-any.whl", ""); w.Code != http.StatusNotFound {
		t.Fatalf("proxy-only file through the group: %d %q", w.Code, w.Body.String())
	}
	// A project only the proxy has still comes from the proxy.
	if w := e.do("GET", "py-all", "simple/legacy/", ""); !strings.Contains(w.Body.String(), "/repository/py-all/packages/legacy/legacy-1.0.tar.gz") {
		t.Fatalf("proxy-only project page: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "py-all", "packages/legacy/legacy-1.0.tar.gz", ""); w.Body.String() != "legacy" {
		t.Fatalf("proxy-only project file: %d %q", w.Code, w.Body.String())
	}

	w = e.do("GET", "py-all", "simple/", "")
	if !strings.Contains(w.Body.String(), `<a href="internal-pkg/">`) || !strings.Contains(w.Body.String(), `<a href="six/">`) {
		t.Fatalf("root: %s", w.Body.String())
	}
	if strings.Count(w.Body.String(), `<a href="six/">`) != 1 {
		t.Fatalf("root must de-duplicate: %s", w.Body.String())
	}
}

func TestRootWithoutTrailingSlashRedirects(t *testing.T) {
	e := newEnv(t)
	w := e.do("GET", "py-hosted", "simple", "")
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/repository/py-hosted/simple/" {
		t.Fatalf("redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := e.do("GET", "py-hosted", "simple/", ""); w.Code != 200 {
		t.Fatalf("with slash: %d", w.Code)
	}
}

func TestMethodsAndHead(t *testing.T) {
	e := newEnv(t)
	if w := e.do("HEAD", "py-proxy", "simple/six/", ""); w.Code != 200 {
		t.Fatalf("HEAD: %d", w.Code)
	}
	if w := e.do("DELETE", "py-proxy", "simple/six/", ""); w.Code != 405 {
		t.Fatalf("DELETE: %d", w.Code)
	}
	if w := e.do("POST", "py-proxy", "", ""); w.Code != 405 {
		t.Fatalf("upload to proxy: %d", w.Code)
	}
}

func TestFileURLUsesLiveBaseURL(t *testing.T) {
	e := newEnv(t)
	cur := "https://one.example/"
	e.h.BaseURL = func() string { return cur }
	rp := e.repos["py-hosted"]
	if got := e.h.fileURL(rp, "p", "f.whl"); got != "https://one.example/repository/py-hosted/packages/p/f.whl" {
		t.Fatalf("first: %s", got)
	}
	cur = "https://two.example"
	if got := e.h.fileURL(rp, "p", "f.whl"); got != "https://two.example/repository/py-hosted/packages/p/f.whl" {
		t.Fatalf("after change: %s", got)
	}
}

// QA N-1: an offline member ahead of the one that has the project fails the
// request (503) instead of letting the proxy's same-named project through;
// one behind the owner is not consulted.
func TestGroupFailsClosedOnOfflineMember(t *testing.T) {
	e := newEnv(t)
	e.seedHosted(t, "six", "six-2.0.0.tar.gz", "internal six")
	off, on := false, true
	e.repos["py-hosted"].Online = &off // the group holds the same pointer
	for _, p := range []string{"simple/six/", "packages/six/six-1.16.0-py2.py3-none-any.whl"} {
		if w := e.do("GET", "py-all", p, ""); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with the hosted member offline: %d %q, want 503", p, w.Code, w.Body.String())
		}
	}

	e.repos["py-hosted"].Online = &on
	e.repos["py-proxy"].Online = &off
	w := e.do("GET", "py-all", "simple/six/", "")
	if body := w.Body.String(); strings.Count(body, "<a ") != 1 || !strings.Contains(body, "six-2.0.0.tar.gz") {
		t.Fatalf("owner ahead of the offline member: %d %s", w.Code, body)
	}
	// No online member has legacy and the offline one might: not a 404.
	for _, p := range []string{"simple/legacy/", "packages/legacy/legacy-1.0.tar.gz"} {
		if w := e.do("GET", "py-all", p, ""); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with the proxy offline: %d %q, want 503", p, w.Code, w.Body.String())
		}
	}
	if n := len(e.repos["py-all"].MemberRepos); n != 2 {
		t.Fatalf("MemberRepos must keep the offline member: %d", n)
	}
}

// A member ahead of the owner that fails, here a hosted row that cannot be
// read, is a 5xx, not a reason to serve the proxy's same-named project.
func TestGroupFailsClosedOnMemberError(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	err := e.h.Store.DB.Tx(ctx, func(tx *meta.Tx) error {
		return tx.InsertPackage(ctx, meta.Package{Repo: "py-hosted", Format: "pypi", Name: "six", Version: "x", Attrs: "not json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"simple/six/", "packages/six/six-1.16.0-py2.py3-none-any.whl"} {
		if w := e.do("GET", "py-all", p, ""); w.Code < 500 || strings.Contains(w.Body.String(), "six-wheel") || strings.Contains(w.Body.String(), "six-1.16.0") {
			t.Errorf("%s: %d %q, want 5xx", p, w.Code, w.Body.String())
		}
	}
}
