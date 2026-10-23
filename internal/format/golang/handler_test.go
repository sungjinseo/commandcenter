package golang

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
)

type env struct {
	h     *Handler
	repos map[string]*repo.Repository
}

// fakeProxy serves rsc.io/quote and github.com/Azure/x like proxy.golang.org.
func fakeProxy() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rsc.io/quote/@v/list":
			io.WriteString(w, "v1.5.2\nv1.5.1\n")
		case "/rsc.io/quote/@v/v1.5.2.info":
			io.WriteString(w, `{"Version":"v1.5.2","Time":"2018-02-14T15:44:20Z"}`)
		case "/rsc.io/quote/@v/master.info":
			io.WriteString(w, `{"Version":"v1.5.3-0.20180214154420-abcdefabcdef","Time":"2018-02-14T15:44:20Z"}`)
		case "/rsc.io/quote/@v/v1.5.2.mod":
			io.WriteString(w, "module rsc.io/quote\n")
		case "/rsc.io/quote/@v/v1.5.2.zip":
			io.WriteString(w, "zip-bytes")
		case "/github.com/!azure/x/@v/list":
			io.WriteString(w, "")
		case "/github.com/!azure/x/@latest":
			io.WriteString(w, `{"Version":"v0.0.0-20200101000000-abcdefabcdef","Time":"2020-01-01T00:00:00Z"}`)
		default:
			http.Error(w, "not found", http.StatusGone)
		}
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ts := httptest.NewServer(fakeProxy())
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
		{Name: "go-hosted", Format: "go", Type: "hosted", WritePolicy: "allow_once"},
		{Name: "go-proxy", Format: "go", Type: "proxy", Remote: ts.URL,
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "go-all", Format: "go", Type: "group", Members: []string{"go-hosted", "go-proxy"}, WriteMember: "go-hosted"},
	}}
	res := &resolve.Resolver{Store: store, Fetcher: proxy.New(store, ts.Client(), 4)}
	return &env{h: New(store, res), repos: repo.Build(cfg)}
}

func (e *env) do(method, repoName, path string, body io.Reader) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repoName+"/x", body)
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

func (e *env) seed(t *testing.T, repoName, key, body string) {
	t.Helper()
	if _, err := e.h.Store.Put(context.Background(), repoName, key, "", strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

func moduleZip(t *testing.T, module, version, gomod string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	prefix := module + "@" + version + "/"
	w, _ := zw.Create(prefix + "go.mod")
	io.WriteString(w, gomod)
	w, _ = zw.Create(prefix + "hello.go")
	io.WriteString(w, "package hello\n")
	zw.Close()
	return buf.Bytes()
}

func TestProxyReads(t *testing.T) {
	e := newEnv(t)
	if w := e.do("GET", "go-proxy", "rsc.io/quote/@v/list", nil); w.Code != 200 || w.Body.String() != "v1.5.1\nv1.5.2\n" {
		t.Fatalf("list: %d %q", w.Code, w.Body.String())
	}
	w := e.do("GET", "go-proxy", "rsc.io/quote/@v/v1.5.2.info", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Body.String(), "v1.5.2") {
		t.Fatalf("info: %d %v %q", w.Code, w.Header(), w.Body.String())
	}
	if w := e.do("GET", "go-proxy", "rsc.io/quote/@v/v1.5.2.zip", nil); w.Body.String() != "zip-bytes" || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("zip: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("GET", "go-proxy", "rsc.io/quote/@latest", nil); !strings.Contains(w.Body.String(), `"v1.5.2"`) {
		t.Fatalf("latest from list: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("GET", "go-proxy", "github.com/!azure/x/@latest", nil); !strings.Contains(w.Body.String(), "v0.0.0-2020") {
		t.Fatalf("latest without tags: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("GET", "go-proxy", "example.com/none/@v/list", nil); w.Code != 404 {
		t.Fatalf("unknown module (410 upstream): %d", w.Code)
	}
	if w := e.do("GET", "go-proxy", "sumdb/sum.golang.org/supported", nil); w.Code != 404 {
		t.Fatalf("sumdb: %d", w.Code)
	}
}

func TestHostedAndGroupReads(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "go-hosted", "rsc.io/quote/@v/v1.6.0-beta.1.mod", "module rsc.io/quote\n")
	e.seed(t, "go-hosted", "rsc.io/quote/@v/v1.6.0-beta.1.info", `{"Version":"v1.6.0-beta.1"}`)
	e.seed(t, "go-hosted", "rsc.io/quote/@v/v1.6.0-beta.1.zip", "zip")
	e.seed(t, "go-hosted", "example.com/!hello/@v/v1.0.0.mod", "module example.com/Hello\n")
	e.seed(t, "go-hosted", "example.com/!hello/@v/v1.0.0.zip", "zip")
	e.seed(t, "go-hosted", "example.com/!hello/@v/v1.0.0.info", `{"Version":"v1.0.0"}`)

	if w := e.do("GET", "go-hosted", "example.com/!hello/@v/list", nil); w.Body.String() != "v1.0.0\n" {
		t.Fatalf("hosted list: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("GET", "go-hosted", "example.com/!hello/@latest", nil); !strings.Contains(w.Body.String(), `"v1.0.0"`) {
		t.Fatalf("hosted latest: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("GET", "go-all", "rsc.io/quote/@v/list", nil); w.Body.String() != "v1.5.1\nv1.5.2\nv1.6.0-beta.1\n" {
		t.Fatalf("group list: %q", w.Body.String())
	}
	if w := e.do("GET", "go-all", "rsc.io/quote/@latest", nil); !strings.Contains(w.Body.String(), `"v1.5.2"`) {
		t.Fatalf("group latest prefers releases: %q", w.Body.String())
	}
	if w := e.do("GET", "go-all", "rsc.io/quote/@v/v1.6.0-beta.1.mod", nil); w.Body.String() != "module rsc.io/quote\n" {
		t.Fatalf("group mod from hosted: %q", w.Body.String())
	}
	if w := e.do("GET", "go-all", "rsc.io/quote/@v/v1.5.2.zip", nil); w.Body.String() != "zip-bytes" {
		t.Fatalf("group zip from proxy: %q", w.Body.String())
	}
	if w := e.do("GET", "go-hosted", "example.com/none/@latest", nil); w.Code != 404 {
		t.Fatalf("hosted latest of unknown module: %d", w.Code)
	}
	if w := e.do("DELETE", "go-hosted", "example.com/!hello/@v/list", nil); w.Code != 405 {
		t.Fatalf("DELETE: %d", w.Code)
	}
}

func TestVersionQueries(t *testing.T) {
	e := newEnv(t)
	if w := e.do("GET", "go-all", "rsc.io/quote/@v/master.info", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "v1.5.3-0.2018") {
		t.Fatalf("group query: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("GET", "go-proxy", "rsc.io/quote/@v/master.info", nil); w.Code != 200 {
		t.Fatalf("proxy query: %d", w.Code)
	}
	if w := e.do("GET", "go-hosted", "rsc.io/quote/@v/master.info", nil); w.Code != 404 {
		t.Fatalf("hosted query: %d", w.Code)
	}
	if w := e.do("GET", "go-all", "rsc.io/quote/@v/master.zip", nil); w.Code != 404 {
		t.Fatalf("non-canonical zip: %d", w.Code)
	}
	if w := e.do("GET", "go-all", "rsc.io/quote/@v/master.mod", nil); w.Code != 404 {
		t.Fatalf("non-canonical mod: %d", w.Code)
	}
	if w := e.do("PUT", "go-hosted", "rsc.io/quote/@v/master.info", strings.NewReader("{}")); w.Code != 400 {
		t.Fatalf("PUT query: %d", w.Code)
	}
}

func TestEmptyListIsFound(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"go-proxy", "go-all"} {
		w := e.do("GET", name, "github.com/!azure/x/@v/list", nil)
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatalf("%s empty list: %d %q", name, w.Code, w.Body.String())
		}
	}
	if w := e.do("GET", "go-hosted", "example.com/none/@v/list", nil); w.Code != 404 {
		t.Fatalf("hosted unknown list: %d", w.Code)
	}
	if w := e.do("GET", "go-all", "github.com/!azure/x/@latest", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "v0.0.0-2020") {
		t.Fatalf("group latest of untagged: %d %q", w.Code, w.Body.String())
	}
}

func TestListTimestamps(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "go-hosted", "ts.io/m/@v/list", "v1.0.0 2020-01-01T00:00:00Z\n")
	a, err := e.h.Store.DB.GetAsset(context.Background(), "go-hosted", "ts.io/m/@v/list")
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.h.readList(a)
	if err != nil || len(got) != 1 || got[0] != "v1.0.0" {
		t.Fatalf("readList: %v %v", got, err)
	}
}

func TestBadVersionMessage(t *testing.T) {
	_, err := ParseRoute("rsc.io/quote/@v/1.0.0.mod")
	if !errors.Is(err, errBadVersion) || repo.StatusOf(err) != 400 || !strings.Contains(err.Error(), "1.0.0") {
		t.Fatalf("err = %v status %d", err, repo.StatusOf(err))
	}
}

func TestReadListDropsInvalidEntries(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "go-hosted", "bad.io/m/@v/list", "v1.0.0\nnot-a-version\nv1.1\nv1.2.0 2020-01-01T00:00:00Z\n<html>\n")
	a, err := e.h.Store.DB.GetAsset(context.Background(), "go-hosted", "bad.io/m/@v/list")
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.h.readList(a)
	if err != nil || strings.Join(got, ",") != "v1.0.0,v1.2.0" {
		t.Fatalf("readList: %v %v", got, err)
	}
}

func TestReadListTooLongIs502(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "go-hosted", "long.io/m/@v/list", strings.Repeat("x", 70<<10)+"\n")
	a, err := e.h.Store.DB.GetAsset(context.Background(), "go-hosted", "long.io/m/@v/list")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.readList(a); repo.StatusOf(err) != 502 {
		t.Fatalf("long line: %v", err)
	}
	e.seed(t, "go-hosted", "huge.io/m/@v/list", strings.Repeat("v1.0.0\n", (maxDoc/7)+10))
	a, _ = e.h.Store.DB.GetAsset(context.Background(), "go-hosted", "huge.io/m/@v/list")
	if _, err := e.h.readList(a); repo.StatusOf(err) != 502 {
		t.Fatalf("huge list: %v", err)
	}
}
