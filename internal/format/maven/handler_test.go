package maven

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
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

// remoteRepo serves a tiny upstream Maven repository.
func remoteRepo() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/maven2/org/up/lib/1.0/lib-1.0.jar":
			io.WriteString(w, "upstream-jar")
		case "/maven2/.meta/prefixes.txt":
			io.WriteString(w, "## repository-prefixes/2.0\n/org/up\n")
		case "/maven2/org/example/app/maven-metadata.xml":
			io.WriteString(w, `<metadata><groupId>org.example</groupId><artifactId>app</artifactId>
<versioning><versions><version>0.9</version></versions><lastUpdated>20250101000000</lastUpdated></versioning></metadata>`)
		default:
			http.NotFound(w, r)
		}
	}
}

func newEnv(t *testing.T, extra ...config.Repository) *env {
	t.Helper()
	ts := httptest.NewServer(remoteRepo())
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	b, _ := blob.Open(dir)
	db, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: time.Now}
	repos := []config.Repository{
		{Name: "releases", Format: "maven", Type: "hosted", VersionPolicy: "release", WritePolicy: "allow_once"},
		{Name: "snapshots", Format: "maven", Type: "hosted", VersionPolicy: "snapshot", WritePolicy: "allow"},
		{Name: "central", Format: "maven", Type: "proxy", Remote: ts.URL + "/maven2",
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "public", Format: "maven", Type: "group", Members: []string{"releases", "snapshots", "central"}, WriteMember: "releases"},
		{Name: "readonly-group", Format: "maven", Type: "group", Members: []string{"central"}},
	}
	cfg := &config.Config{Repositories: append(repos, extra...)}
	res := &resolve.Resolver{Store: store, Fetcher: proxy.New(store, ts.Client(), 4)}
	return &env{h: New(store, res), repos: repo.Build(cfg)}
}

func (e *env) do(method, repoName, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repoName+"/"+path, strings.NewReader(body))
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

func (e *env) seed(t *testing.T, repoName, path, body string) {
	t.Helper()
	if _, err := e.h.Store.Put(context.Background(), repoName, path, ContentType(path), strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

func sha1hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestGetHostedAndChecksums(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "releases", "org/example/app/1.0/app-1.0.jar", "jar-bytes")
	w := e.do("GET", "releases", "org/example/app/1.0/app-1.0.jar", "")
	if w.Code != 200 || w.Body.String() != "jar-bytes" || w.Header().Get("Content-Type") != "application/java-archive" {
		t.Fatalf("GET: %d %q %v", w.Code, w.Body, w.Header())
	}
	w = e.do("GET", "releases", "org/example/app/1.0/app-1.0.jar.sha1", "")
	if w.Code != 200 || w.Body.String() != sha1hex("jar-bytes") || w.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("sha1: %d %q", w.Code, w.Body)
	}
	for _, h := range []string{"md5", "sha256", "sha512"} {
		if w := e.do("GET", "releases", "org/example/app/1.0/app-1.0.jar."+h, ""); w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("%s: %d", h, w.Code)
		}
	}
	if w := e.do("HEAD", "releases", "org/example/app/1.0/app-1.0.jar", ""); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD: %d", w.Code)
	}
	if w := e.do("GET", "releases", "org/example/app/9.9/app-9.9.jar", ""); w.Code != 404 {
		t.Fatalf("missing: %d", w.Code)
	}
	if w := e.do("GET", "releases", "../etc/passwd", ""); w.Code != 400 {
		t.Fatalf("unsafe: %d", w.Code)
	}
}

func TestGetProxy(t *testing.T) {
	e := newEnv(t)
	w := e.do("GET", "central", "org/up/lib/1.0/lib-1.0.jar", "")
	if w.Code != 200 || w.Body.String() != "upstream-jar" || w.Header().Get("Content-Type") != "application/java-archive" {
		t.Fatalf("proxy: %d %q %v", w.Code, w.Body, w.Header())
	}
	w = e.do("GET", "central", "org/up/lib/1.0/lib-1.0.jar.sha1", "")
	if w.Code != 200 || w.Body.String() != sha1hex("upstream-jar") {
		t.Fatalf("proxy sha1: %d %q", w.Code, w.Body)
	}
	if w := e.do("GET", "central", "org/up/lib/2.0/lib-2.0.jar", ""); w.Code != 404 {
		t.Fatalf("proxy missing: %d", w.Code)
	}
}

func TestGetGroupContentAndMergedMetadata(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "releases", "org/example/app/1.0/app-1.0.pom", "<project/>")
	e.seed(t, "releases", "org/example/app/maven-metadata.xml", `<metadata><groupId>org.example</groupId><artifactId>app</artifactId>
<versioning><versions><version>1.0</version></versions><lastUpdated>20260101000000</lastUpdated></versioning></metadata>`)

	if w := e.do("GET", "public", "org/example/app/1.0/app-1.0.pom", ""); w.Code != 200 || w.Body.String() != "<project/>" {
		t.Fatalf("group content: %d %q", w.Code, w.Body)
	}
	if w := e.do("GET", "public", "org/up/lib/1.0/lib-1.0.jar", ""); w.Code != 200 || w.Body.String() != "upstream-jar" {
		t.Fatalf("group via proxy: %d %q", w.Code, w.Body)
	}

	w := e.do("GET", "public", "org/example/app/maven-metadata.xml", "")
	if w.Code != 200 {
		t.Fatalf("merged metadata: %d %s", w.Code, w.Body)
	}
	m, err := ParseMetadata(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(m.Versioning.Versions, ",") != "0.9,1.0" || m.Versioning.Release != "1.0" {
		t.Fatalf("merged = %+v", m.Versioning)
	}
	merged := e.do("GET", "public", "org/example/app/maven-metadata.xml", "").Body.String()
	if w := e.do("GET", "public", "org/example/app/maven-metadata.xml.sha1", ""); w.Body.String() != sha1hex(merged) {
		t.Fatalf("merged sha1 mismatch")
	}
}

// H-1: a group must not pass on one member's prefix list, which would hide
// the other members' groupIds from Maven's prefix filter.
func TestGroupPrefixesTxtIsNotFound(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "releases", "org/example/app/1.0/app-1.0.pom", "<project/>")
	for _, p := range []string{".meta/prefixes.txt", ".meta/prefixes.txt.sha1"} {
		if w := e.do("GET", "public", p, ""); w.Code != 404 {
			t.Fatalf("group %s: %d %q", p, w.Code, w.Body)
		}
		if w := e.do("HEAD", "public", p, ""); w.Code != 404 {
			t.Fatalf("group HEAD %s: %d", p, w.Code)
		}
	}
	if w := e.do("GET", "central", ".meta/prefixes.txt", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "/org/up") {
		t.Fatalf("proxy prefixes.txt: %d %q", w.Code, w.Body)
	}
}
