package swift

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/component"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
)

const ccBase = "http://cc.example"

const manifest = `// swift-tools-version:5.7
import PackageDescription

let package = Package(
    name: "LinkedList",
    products: [.library(name: "LinkedList", targets: ["LinkedList"])],
    dependencies: [
        .package(url: "https://github.com/apple/swift-argument-parser.git", from: "1.2.0"),
        .package(url: "https://github.com/apple/swift-collections", .upToNextMajor(from: "1.0.0")),
        .package(name: "Nimble", url: "https://github.com/Quick/Nimble.git", exact: "12.0.0"),
        .package(id: "mona.Other", "1.0.0"..<"2.0.0"),
        .package(path: "../Local"),
    ],
    targets: [.target(name: "LinkedList")]
)
`

// archive builds a package zip: Package.swift (and variants) at the root,
// or under dir as `swift package archive-source` lays it out.
func archive(t *testing.T, dir string, variants ...string) []byte {
	t.Helper()
	files := map[string]string{dir + "Package.swift": manifest, dir + "Sources/LinkedList/LinkedList.swift": "public struct LinkedList {}"}
	for _, v := range variants {
		files[dir+"Package@swift-"+v+".swift"] = "// swift-tools-version:" + v + "\n" + manifest
	}
	return zipBytes(t, files)
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// publishBody is the multipart form `swift package-registry publish`
// sends: source-archive, metadata and optionally source-archive-signature.
func publishBody(t *testing.T, zipb []byte, metadata string, sig []byte) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part := func(name, filename, ct string, body []byte) {
		h := textproto.MIMEHeader{}
		if filename != "" {
			h.Set("Content-Disposition", `form-data; name="`+name+`"; filename="`+filename+`"`)
		} else {
			h.Set("Content-Disposition", `form-data; name="`+name+`"`)
		}
		h.Set("Content-Type", ct)
		w, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(body)
	}
	part("source-archive", "LinkedList-1.1.1.zip", "application/zip", zipb)
	if metadata != "" {
		part("metadata", "", "application/json", []byte(metadata))
	}
	if sig != nil {
		part("source-archive-signature", "", "application/octet-stream", sig)
	}
	mw.Close()
	return mw.FormDataContentType(), buf.Bytes()
}

type env struct {
	h      *Handler
	repos  map[string]*repo.Repository
	sink   *repotest.Sink
	up     string // the upstream registry URL
	hits   atomic.Int32
	accept atomic.Pointer[string]
	zips   map[string][]byte
	now    time.Time
}

// upstream is a fake registry: mona.LinkedList 1.1.1 (good checksum) and
// 1.0.0 (wrong checksum), acme.demo 9.0.0 (also hosted: the group's
// hosted member wins) and the identifiers lookup.
func (e *env) upstream(w http.ResponseWriter, r *http.Request) {
	a := r.Header.Get("Accept")
	e.accept.Store(&a)
	j := func(v string) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Version", "1")
		io.WriteString(w, v)
	}
	release := func(id, version, sum string) string {
		return `{"id":"` + id + `","version":"` + version + `","resources":[{"name":"source-archive","type":"application/zip","checksum":"` + sum + `"}],"metadata":{"description":"upstream"},"publishedAt":"2026-01-02T00:00:00Z"}`
	}
	switch p := strings.ToLower(r.URL.Path); {
	case p == "/mona/linkedlist":
		j(`{"releases":{"1.1.1":{"url":"` + e.up + `/mona/LinkedList/1.1.1"},"1.0.0":{"url":"` + e.up + `/mona/LinkedList/1.0.0","problem":{"status":410,"title":"Gone"}}}}`)
	case p == "/mona/linkedlist/1.1.1":
		j(release("mona.LinkedList", "1.1.1", sha256hex(e.zips["linkedlist"])))
	case p == "/mona/linkedlist/1.0.0":
		j(release("mona.LinkedList", "1.0.0", strings.Repeat("0", 64)))
	case p == "/mona/linkedlist/1.1.1.zip", p == "/mona/linkedlist/1.0.0.zip":
		e.hits.Add(1)
		w.Header().Set("Content-Type", "application/zip")
		w.Write(e.zips["linkedlist"])
	case p == "/acme/demo":
		j(`{"releases":{"9.0.0":{"url":"` + e.up + `/acme/demo/9.0.0"}}}`)
	case p == "/acme/demo/9.0.0":
		j(release("acme.demo", "9.0.0", sha256hex(e.zips["linkedlist"])))
	case p == "/identifiers" && r.URL.Query().Get("url") == "https://github.com/mona/LinkedList":
		j(`{"identifiers":["mona.LinkedList"]}`)
	default:
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(404)
		io.WriteString(w, `{"detail":"not found"}`)
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC), zips: map[string][]byte{}}
	e.zips["linkedlist"] = archive(t, "LinkedList/", "5.9")
	ts := httptest.NewServer(http.HandlerFunc(e.upstream))
	t.Cleanup(ts.Close)
	e.up = ts.URL
	dir := t.TempDir()
	b, _ := blob.Open(dir)
	db, err := meta.Open(metatest.Target(t, filepath.Join(dir, "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.sink = &repotest.Sink{}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: func() time.Time { return e.now }, Events: e.sink}
	cfg := &config.Config{AnonymousRead: true, Repositories: []config.Repository{
		{Name: "swift-hosted", Format: "swift", Type: "hosted", WritePolicy: "allow_once"},
		{Name: "swift-allow", Format: "swift", Type: "hosted", WritePolicy: "allow"},
		{Name: "swift-ro", Format: "swift", Type: "hosted", WritePolicy: "deny"},
		{Name: "swift-proxy", Format: "swift", Type: "proxy", Remote: ts.URL + "/",
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "swift-all", Format: "swift", Type: "group", Members: []string{"swift-hosted", "swift-proxy"}, WriteMember: "swift-hosted"},
		{Name: "swift-proxy-first", Format: "swift", Type: "group", Members: []string{"swift-proxy", "swift-hosted"}},
	}}
	f := proxy.New(store, ts.Client(), 4)
	f.Now = func() time.Time { return e.now }
	e.h = New(store, f, auth.New(cfg, db), func() string { return ccBase + "/" })
	e.h.Now = func() time.Time { return e.now }
	e.repos = repo.Build(cfg)
	return e
}

func (e *env) do(method, repoName, path string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	return e.doAs(auth.Principal{}, method, repoName, path, body, hdr...)
}

func (e *env) doAs(p auth.Principal, method, repoName, path string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repoName+"/"+path, bytes.NewReader(body))
	r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	r.Header.Set("Accept", ctRegistry+"+json")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	path, _, _ = strings.Cut(path, "?")
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

// publish publishes zipb as scope.name version with the given metadata.
func (e *env) publish(t *testing.T, repoName, scope, name, version string, zipb []byte, metadata string) *httptest.ResponseRecorder {
	t.Helper()
	ct, body := publishBody(t, zipb, metadata, nil)
	return e.do("PUT", repoName, scope+"/"+name+"/"+version, body, "Content-Type", ct, "Expect", "100-continue")
}

func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if w.Code != 200 || w.Header().Get("Content-Type") != ctJSON || w.Header().Get("Content-Version") != "1" {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body)
	}
	reflect.ValueOf(v).Elem().Set(reflect.Zero(reflect.TypeOf(v).Elem())) // a reused map would merge
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
}

type releaseList struct {
	Releases map[string]struct {
		URL     string         `json:"url"`
		Problem map[string]any `json:"problem"`
	} `json:"releases"`
}

type releaseDoc struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	Resources []struct {
		Name, Type, Checksum string
		Signing              *signing `json:"signing"`
	} `json:"resources"`
	Metadata    map[string]any `json:"metadata"`
	PublishedAt string         `json:"publishedAt"`
}

func problemOf(t *testing.T, w *httptest.ResponseRecorder, status int) string {
	t.Helper()
	if w.Code != status || w.Header().Get("Content-Type") != ctProblem || w.Header().Get("Content-Version") != "1" {
		t.Fatalf("%d %v %s, want %d problem+json", w.Code, w.Header(), w.Body, status)
	}
	var p struct{ Detail string }
	json.Unmarshal(w.Body.Bytes(), &p)
	return p.Detail
}

const metadataJSON = `{"author":{"name":"Mona"},"description":"A linked list","licenseURL":"https://example.org/LICENSE","readmeURL":"https://example.org/README.md","repositoryURLs":["https://github.com/mona/LinkedList","git@github.com:mona/LinkedList.git"],"originalPublicationTime":"2026-01-01T00:00:00Z"}`

// TestHosted: publish as `swift package-registry publish` sends it, then
// the release list, release metadata, manifests, archive and identifiers
// lookup with the headers the specification prescribes.
func TestHosted(t *testing.T) {
	e := newEnv(t)
	z := archive(t, "LinkedList/", "5.9", "5.7.1")
	ct, body := publishBody(t, z, metadataJSON, []byte("SIG"))
	w := e.do("PUT", "swift-hosted", "mona/LinkedList/1.1.1", body, "Content-Type", ct, "Expect", "100-continue")
	if w.Code != 201 || w.Header().Get("Location") != ccBase+"/repository/swift-hosted/mona/LinkedList/1.1.1" || w.Header().Get("Content-Version") != "1" {
		t.Fatalf("publish: %d %v %s", w.Code, w.Header(), w.Body)
	}
	for _, v := range []string{"1.0.0", "1.2.0-beta.1"} {
		if w := e.publish(t, "swift-hosted", "mona", "LinkedList", v, archive(t, "LinkedList/"), ""); w.Code != 201 {
			t.Fatalf("publish %s: %d %s", v, w.Code, w.Body)
		}
	}
	// a raw zip body (curl -T) publishes too
	if w := e.do("PUT", "swift-hosted", "mona/LinkedList/0.9.0", archive(t, "LinkedList/"), "Content-Type", ctZip); w.Code != 201 {
		t.Fatalf("raw publish: %d %s", w.Code, w.Body)
	}
	if got := e.sink.Events(); len(got) != 4 || got[0].Kind != "upload" || got[0].Subject != "mona.linkedlist@1.1.1" {
		t.Fatalf("events: %+v", got)
	}

	// the release list, looked up without case; latest-version is the highest semver
	var list releaseList
	w = e.do("GET", "swift-hosted", "MONA/linkedlist", nil)
	decode(t, w, &list)
	if len(list.Releases) != 4 || list.Releases["1.1.1"].URL != ccBase+"/repository/swift-hosted/MONA/linkedlist/1.1.1" {
		t.Fatalf("list: %+v", list)
	}
	if got := w.Header().Values("Link"); len(got) != 1 || got[0] != "<"+ccBase+`/repository/swift-hosted/MONA/linkedlist/1.2.0-beta.1>; rel="latest-version"` {
		t.Fatalf("list links: %q", got)
	}
	if w := e.do("GET", "swift-hosted", "mona/LinkedList.json", nil); w.Code != 200 {
		t.Fatalf(".json list: %d", w.Code)
	}

	// the release metadata: checksum, signature, the metadata part and the version links
	var rel releaseDoc
	w = e.do("GET", "swift-hosted", "mona/LinkedList/1.1.1", nil)
	decode(t, w, &rel)
	if rel.ID != "mona.LinkedList" || rel.Version != "1.1.1" || len(rel.Resources) != 1 || rel.Resources[0].Checksum != sha256hex(z) || rel.Resources[0].Type != ctZip ||
		rel.Resources[0].Signing == nil || rel.Resources[0].Signing.SignatureBase64Encoded != base64.StdEncoding.EncodeToString([]byte("SIG")) || rel.Resources[0].Signing.SignatureFormat != "cms-1.0.0" ||
		rel.Metadata["description"] != "A linked list" || rel.PublishedAt != "2026-10-28T12:00:00Z" {
		t.Fatalf("release: %+v", rel)
	}
	links := strings.Join(w.Header().Values("Link"), "\n")
	base := ccBase + "/repository/swift-hosted/mona/LinkedList/"
	for _, want := range []string{"<" + base + `1.2.0-beta.1>; rel="latest-version"`, "<" + base + `1.2.0-beta.1>; rel="successor-version"`, "<" + base + `1.0.0>; rel="predecessor-version"`} {
		if !strings.Contains(links, want) {
			t.Errorf("release links lack %s:\n%s", want, links)
		}
	}
	w = e.do("GET", "swift-hosted", "mona/LinkedList/1.0.0", nil)
	decode(t, w, &rel)
	if links := strings.Join(w.Header().Values("Link"), "\n"); !strings.Contains(links, base+`1.1.1>; rel="successor-version"`) || !strings.Contains(links, base+`0.9.0>; rel="predecessor-version"`) || len(rel.Metadata) != 0 {
		t.Fatalf("1.0.0: %s %+v", links, rel)
	}
	if w := e.do("GET", "swift-hosted", "mona/LinkedList/0.9.0", nil); strings.Contains(strings.Join(w.Header().Values("Link"), "\n"), "predecessor") {
		t.Fatalf("oldest has a predecessor: %v", w.Header())
	}

	// the manifest, its alternates, a variant, and a missing variant's redirect
	w = e.do("GET", "swift-hosted", "mona/LinkedList/1.1.1/Package.swift", nil, "Accept", ctRegistry+"+swift")
	if w.Code != 200 || w.Header().Get("Content-Type") != ctSwift || w.Body.String() != manifest || w.Header().Get("Content-Disposition") != `attachment; filename="Package.swift"` {
		t.Fatalf("manifest: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if got := w.Header().Values("Link"); len(got) != 2 ||
		got[0] != "<"+base+`1.1.1/Package.swift?swift-version=5.7.1>; rel="alternate"; filename="Package@swift-5.7.1.swift"; swift-tools-version="5.7.1"` ||
		got[1] != "<"+base+`1.1.1/Package.swift?swift-version=5.9>; rel="alternate"; filename="Package@swift-5.9.swift"; swift-tools-version="5.9"` {
		t.Fatalf("manifest links: %q", got)
	}
	w = e.do("GET", "swift-hosted", "mona/LinkedList/1.1.1/Package.swift?swift-version=5.9", nil)
	if w.Code != 200 || !strings.HasPrefix(w.Body.String(), "// swift-tools-version:5.9") || w.Header().Get("Content-Disposition") != `attachment; filename="Package@swift-5.9.swift"` || len(w.Header().Values("Link")) != 1 {
		t.Fatalf("variant: %d %v %s", w.Code, w.Header(), w.Body)
	}
	w = e.do("GET", "swift-hosted", "mona/LinkedList/1.1.1/Package.swift?swift-version=4.2", nil)
	if w.Code != 303 || w.Header().Get("Location") != base+"1.1.1/Package.swift" {
		t.Fatalf("missing variant: %d %v", w.Code, w.Header())
	}
	if w := e.do("GET", "swift-hosted", "mona/LinkedList/1.0.0/Package.swift", nil); w.Code != 200 || len(w.Header().Values("Link")) != 0 {
		t.Fatalf("root manifest without variants: %d %v", w.Code, w.Header())
	}

	// the archive: Digest, Content-Disposition, byte ranges
	w = e.do("GET", "swift-hosted", "mona/LinkedList/1.1.1.zip", nil, "Accept", ctRegistry+"+zip")
	raw, _ := hex.DecodeString(sha256hex(z))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), z) || w.Header().Get("Content-Type") != ctZip || w.Header().Get("Digest") != "sha-256="+base64.StdEncoding.EncodeToString(raw) ||
		w.Header().Get("Content-Disposition") != `attachment; filename="LinkedList-1.1.1.zip"` || w.Header().Get("Accept-Ranges") != "bytes" || w.Header().Get("Content-Version") != "1" {
		t.Fatalf("archive: %d %v", w.Code, w.Header())
	}
	if w := e.do("GET", "swift-hosted", "mona/LinkedList/1.1.1.zip", nil, "Range", "bytes=0-3"); w.Code != 206 || w.Body.String() != "PK\x03\x04" {
		t.Fatalf("range: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("HEAD", "swift-hosted", "mona/LinkedList/1.1.1.zip", nil); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %d", w.Code, w.Body.Len())
	}

	// identifiers by repository URL (without case, .git or a trailing /)
	var ids struct{ Identifiers []string }
	decode(t, e.do("GET", "swift-hosted", "identifiers?url=https://github.com/mona/linkedlist.git", nil), &ids)
	if len(ids.Identifiers) != 1 || ids.Identifiers[0] != "mona.LinkedList" {
		t.Fatalf("identifiers: %+v", ids)
	}
	if d := problemOf(t, e.do("GET", "swift-hosted", "identifiers?url=https://github.com/nobody/x", nil), 404); !strings.Contains(d, "nobody/x") {
		t.Fatalf("unknown url: %s", d)
	}
	problemOf(t, e.do("GET", "swift-hosted", "identifiers", nil), 400)

	// problem details: 404s, 406 for another API version, 405
	if d := problemOf(t, e.do("GET", "swift-hosted", "mona/Nope", nil), 404); d != "package mona.Nope not found" {
		t.Fatalf("404: %s", d)
	}
	if d := problemOf(t, e.do("GET", "swift-hosted", "mona/LinkedList/3.0.0", nil), 404); d != "release mona.LinkedList 3.0.0 not found" {
		t.Fatalf("release 404: %s", d)
	}
	problemOf(t, e.do("GET", "swift-hosted", "mona/LinkedList/3.0.0.zip", nil), 404)
	problemOf(t, e.do("GET", "swift-hosted", "mona/LinkedList/3.0.0/Package.swift", nil), 404)
	problemOf(t, e.do("GET", "swift-hosted", "mona/LinkedList", nil, "Accept", "application/vnd.swift.registry.v2+json"), 406)
	if w := e.do("GET", "swift-hosted", "mona/LinkedList", nil, "Accept", "*/*"); w.Code != 200 {
		t.Fatalf("no vendor accept: %d", w.Code)
	}
	w = e.do("POST", "swift-hosted", "mona/LinkedList/1.1.1", nil)
	if problemOf(t, w, 405); w.Header().Get("Allow") != "GET, HEAD, PUT" {
		t.Fatalf("405 Allow: %q", w.Header().Get("Allow"))
	}
	w = e.do("PUT", "swift-proxy", "mona/LinkedList/1.1.1", archive(t, "LinkedList/"), "Content-Type", ctZip)
	if problemOf(t, w, 405); w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("proxy PUT Allow: %q", w.Header().Get("Allow"))
	}
	// the stored row and archive
	pkgs, _ := e.h.Store.DB.ListPackages(context.Background(), "swift-hosted", "mona.linkedlist")
	if len(pkgs) != 4 || pkgs[0].Format != "swift" {
		t.Fatalf("rows: %+v", pkgs)
	}
	if _, err := e.h.Store.Get(context.Background(), "swift-hosted", "mona/linkedlist/1.1.1/source-archive.zip"); err != nil {
		t.Fatal(err)
	}
}

// TestValidation: identifiers and versions as the specification bounds
// them, and archives that are not packages.
func TestValidation(t *testing.T) {
	e := newEnv(t)
	good := archive(t, "LinkedList/")
	cases := []struct {
		name, path string
		zip        []byte
		metadata   string
		want       int
	}{
		{"ok", "mona/LinkedList/1.0.0", good, "", 201},
		{"build metadata", "mona/LinkedList/1.0.1+build.5", good, "", 201},
		{"v prefix", "mona/LinkedList/v1.0.0", good, "", 404},
		{"two-part version", "mona/LinkedList/1.0", good, "", 404},
		{"leading zero", "mona/LinkedList/01.0.0", good, "", 404},
		{"scope with _", "mo_na/LinkedList/1.0.0", good, "", 404},
		{"scope starting with -", "-mona/LinkedList/1.0.0", good, "", 404},
		{"scope double -", "mo--na/LinkedList/1.0.0", good, "", 404},
		{"scope too long", strings.Repeat("a", 40) + "/LinkedList/1.0.0", good, "", 404},
		{"name too long", "mona/" + strings.Repeat("b", 101) + "/1.0.0", good, "", 404},
		{"name with .", "mona/Linked.List/1.0.0", good, "", 404},
		{"name with _", "mona/Linked_List/1.0.0", good, "", 201},
		{"name ending in _", "mona/LinkedList_/1.0.0", good, "", 404},
		{"scope of 39 with hyphens", strings.Repeat("a-", 19) + "a/LinkedList/1.0.0", good, "", 201},
		{"scope of 41 with hyphens", strings.Repeat("a-", 20) + "a/LinkedList/1.0.0", good, "", 404},
		{"Package.swift at the root", "mona/Root/1.0.0", archive(t, ""), "", 400}, // SwiftPM strips the top level of a download
		{"two top-level directories", "mona/Two/1.0.0", zipBytes(t, map[string]string{"a/Package.swift": manifest, "b/x.swift": "x"}), "", 400},
		{"no Package.swift", "mona/Empty/1.0.0", zipBytes(t, map[string]string{"README.md": "x"}), "", 400},
		{"Package.swift two deep", "mona/Deep/1.0.0", zipBytes(t, map[string]string{"a/b/Package.swift": manifest}), "", 400},
		{"not a zip", "mona/Text/1.0.0", []byte("not a zip"), "", 400},
		{"empty", "mona/Nothing/1.0.0", nil, "", 400},
		{"metadata not JSON", "mona/Meta/1.0.0", good, "[1]", 400},
		{"metadata invalid", "mona/Meta/1.0.0", good, "{", 400},
	}
	for _, c := range cases {
		ct, body := publishBody(t, c.zip, c.metadata, nil)
		w := e.do("PUT", "swift-hosted", c.path, body, "Content-Type", ct)
		if w.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, w.Code, w.Body, c.want)
		}
		if c.want != 201 && w.Header().Get("Content-Type") != ctProblem {
			t.Errorf("%s: not a problem: %v", c.name, w.Header())
		}
	}
	// a multipart form without the archive
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("metadata", "{}")
	mw.Close()
	if w := e.do("PUT", "swift-hosted", "mona/LinkedList/2.0.0", buf.Bytes(), "Content-Type", mw.FormDataContentType()); w.Code != 400 {
		t.Fatalf("no archive part: %d %s", w.Code, w.Body)
	}
	// identifiers and versions as the API reads them
	for p, want := range map[string]int{"mona": 404, "login": 405, "mona/LinkedList/1.0.0/Other.swift": 404, "mona/LinkedList/1.0.0/Package.swift/x": 404, "mona/LinkedList/1.0.0.tar.gz": 404, "../x": 404} {
		if w := e.do("GET", "swift-hosted", p, nil); w.Code != want {
			t.Errorf("GET %s: %d, want %d", p, w.Code, want)
		}
	}
}

// TestWritePolicy: allow_once refuses a second publish of a version with
// the specification's 409, allow needs Edit, deny refuses every publish.
func TestWritePolicy(t *testing.T) {
	e := newEnv(t)
	z := archive(t, "LinkedList/")
	if w := e.publish(t, "swift-hosted", "mona", "LinkedList", "1.0.0", z, ""); w.Code != 201 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	if d := problemOf(t, e.publish(t, "swift-hosted", "Mona", "linkedlist", "1.0.0", z, ""), 409); !strings.HasPrefix(d, "a release with the same version exists") {
		t.Fatalf("409: %s", d)
	}
	if w := e.publish(t, "swift-allow", "mona", "LinkedList", "1.0.0", z, ""); w.Code != 201 {
		t.Fatalf("allow first: %d %s", w.Code, w.Body)
	}
	problemOf(t, e.publish(t, "swift-allow", "mona", "LinkedList", "1.0.0", z, ""), 403) // anonymous: no Edit
	z2 := archive(t, "LinkedList/", "5.9")
	ct, body := publishBody(t, z2, `{"description":"second"}`, nil)
	if w := e.doAs(auth.Principal{User: "root", Admin: true}, "PUT", "swift-allow", "mona/LinkedList/1.0.0", body, "Content-Type", ct); w.Code != 201 {
		t.Fatalf("overwrite: %d %s", w.Code, w.Body)
	}
	var rel releaseDoc
	decode(t, e.do("GET", "swift-allow", "mona/LinkedList/1.0.0", nil), &rel)
	if rel.Resources[0].Checksum != sha256hex(z2) || rel.Metadata["description"] != "second" {
		t.Fatalf("after overwrite: %+v", rel)
	}
	problemOf(t, e.publish(t, "swift-ro", "mona", "LinkedList", "1.0.0", z, ""), 400)
}

// TestProxy: the upstream release list with its URLs rewritten, release
// metadata passed through, manifests out of the cached archive, the
// archive checked against the upstream checksum, identifiers forwarded.
func TestProxy(t *testing.T) {
	e := newEnv(t)
	var list releaseList
	w := e.do("GET", "swift-proxy", "mona/LinkedList", nil)
	decode(t, w, &list)
	if len(list.Releases) != 2 || list.Releases["1.1.1"].URL != ccBase+"/repository/swift-proxy/mona/LinkedList/1.1.1" || list.Releases["1.0.0"].Problem["status"] != 410.0 {
		t.Fatalf("list: %+v", list)
	}
	if got := w.Header().Values("Link"); len(got) != 1 || !strings.Contains(got[0], "swift-proxy/mona/LinkedList/1.1.1>; rel=\"latest-version\"") {
		t.Fatalf("links: %q", got)
	}
	if a := e.accept.Load(); a == nil || *a != ctRegistry+"+json" {
		t.Fatalf("upstream Accept: %v", a)
	}
	var rel releaseDoc
	w = e.do("GET", "swift-proxy", "mona/LinkedList/1.1.1", nil)
	decode(t, w, &rel)
	if rel.ID != "mona.LinkedList" || rel.Resources[0].Checksum != sha256hex(e.zips["linkedlist"]) || rel.Metadata["description"] != "upstream" {
		t.Fatalf("release: %+v", rel)
	}
	if links := strings.Join(w.Header().Values("Link"), "\n"); !strings.Contains(links, `swift-proxy/mona/LinkedList/1.0.0>; rel="predecessor-version"`) {
		t.Fatalf("release links: %s", links)
	}
	// the manifest comes out of the archive, fetched once
	w = e.do("GET", "swift-proxy", "mona/LinkedList/1.1.1/Package.swift", nil)
	if w.Code != 200 || w.Body.String() != manifest || len(w.Header().Values("Link")) != 1 || !strings.Contains(w.Header().Get("Link"), `swift-version=5.9>; rel="alternate"`) {
		t.Fatalf("manifest: %d %v %s", w.Code, w.Header(), w.Body)
	}
	w = e.do("GET", "swift-proxy", "mona/LinkedList/1.1.1.zip", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), e.zips["linkedlist"]) || !strings.HasPrefix(w.Header().Get("Digest"), "sha-256=") {
		t.Fatalf("archive: %d %v", w.Code, w.Header())
	}
	if e.hits.Load() != 1 {
		t.Fatalf("archive fetched %d times", e.hits.Load())
	}
	// an archive that does not match the upstream checksum is refused and not cached
	if w := e.do("GET", "swift-proxy", "mona/LinkedList/1.0.0.zip", nil); w.Code != 502 {
		t.Fatalf("mismatch: %d %s", w.Code, w.Body)
	}
	if _, err := e.h.Store.Get(context.Background(), "swift-proxy", "mona/linkedlist/1.0.0/source-archive.zip"); err == nil {
		t.Fatal("mismatching archive cached")
	}
	var ids struct{ Identifiers []string }
	decode(t, e.do("GET", "swift-proxy", "identifiers?url=https://github.com/mona/LinkedList", nil), &ids)
	if len(ids.Identifiers) != 1 || ids.Identifiers[0] != "mona.LinkedList" {
		t.Fatalf("identifiers: %+v", ids)
	}
	problemOf(t, e.do("GET", "swift-proxy", "identifiers?url=https://github.com/nobody/x", nil), 404)
	problemOf(t, e.do("GET", "swift-proxy", "mona/Nope", nil), 404)
	problemOf(t, e.do("GET", "swift-proxy", "mona/LinkedList/5.0.0", nil), 404)
	// the cached documents are components only under a version directory
	if _, name, version, ok := component.DeriveAsset("swift", "mona/linkedlist/1.1.1/release.json"); !ok || name != "mona.linkedlist" || version != "1.1.1" {
		t.Fatalf("release.json component: %q %q %v", name, version, ok)
	}
	if _, _, _, ok := component.DeriveAsset("swift", "mona/linkedlist/releases.json"); ok {
		t.Fatal("releases.json is a component")
	}
}

// TestGroup: a group answers from the first member that has the package;
// a package the hosted member has is never taken from the proxy, and the
// identifiers of every member are merged.
func TestGroup(t *testing.T) {
	e := newEnv(t)
	if w := e.publish(t, "swift-all", "acme", "demo", "1.0.0", archive(t, "LinkedList/"), `{"repositoryURLs":["https://github.com/acme/demo"]}`); w.Code != 201 || !strings.Contains(w.Header().Get("Location"), "/swift-hosted/acme/demo/1.0.0") {
		t.Fatalf("publish via group: %d %v %s", w.Code, w.Header(), w.Body)
	}
	var list releaseList
	decode(t, e.do("GET", "swift-all", "acme/demo", nil), &list)
	if len(list.Releases) != 1 || list.Releases["1.0.0"].URL != ccBase+"/repository/swift-all/acme/demo/1.0.0" {
		t.Fatalf("hosted first: %+v", list)
	}
	decode(t, e.do("GET", "swift-proxy-first", "acme/demo", nil), &list)
	if len(list.Releases) != 1 || list.Releases["9.0.0"].URL != ccBase+"/repository/swift-proxy-first/acme/demo/9.0.0" {
		t.Fatalf("proxy first: %+v", list)
	}
	decode(t, e.do("GET", "swift-all", "mona/LinkedList", nil), &list)
	if len(list.Releases) != 2 || list.Releases["1.1.1"].URL != ccBase+"/repository/swift-all/mona/LinkedList/1.1.1" {
		t.Fatalf("proxy only: %+v", list)
	}
	var rel releaseDoc
	w := e.do("GET", "swift-all", "mona/LinkedList/1.1.1", nil)
	decode(t, w, &rel)
	if rel.Metadata["description"] != "upstream" || !strings.Contains(w.Header().Get("Link"), "swift-all/mona/LinkedList/1.1.1") {
		t.Fatalf("group release: %+v %v", rel, w.Header())
	}
	if w := e.do("GET", "swift-all", "mona/LinkedList/1.1.1.zip", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), e.zips["linkedlist"]) {
		t.Fatalf("group archive: %d", w.Code)
	}
	if w := e.do("GET", "swift-all", "acme/demo/1.0.0/Package.swift", nil); w.Code != 200 || w.Body.String() != manifest {
		t.Fatalf("group manifest: %d", w.Code)
	}
	problemOf(t, e.do("GET", "swift-all", "mona/Nope", nil), 404)
	var ids struct{ Identifiers []string }
	decode(t, e.do("GET", "swift-all", "identifiers?url=https://github.com/mona/LinkedList", nil), &ids)
	if len(ids.Identifiers) != 1 || ids.Identifiers[0] != "mona.LinkedList" {
		t.Fatalf("group identifiers: %+v", ids)
	}
	decode(t, e.do("GET", "swift-all", "identifiers?url=https://github.com/acme/demo", nil), &ids)
	if len(ids.Identifiers) != 1 || ids.Identifiers[0] != "acme.demo" {
		t.Fatalf("group identifiers, hosted: %+v", ids)
	}
	// a group without a writeMember accepts no publish
	w = e.do("PUT", "swift-proxy-first", "acme/demo/2.0.0", archive(t, "LinkedList/"), "Content-Type", ctZip)
	if problemOf(t, w, 405); w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("group Allow: %q", w.Header().Get("Allow"))
	}
}

// TestDeleteComponent: the UI's and cleanup's delete removes the row and
// the archive; the component model maps rows and paths both ways.
func TestDeleteComponent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if w := e.publish(t, "swift-hosted", "mona", "LinkedList", v, archive(t, "LinkedList/"), ""); w.Code != 201 {
			t.Fatalf("publish: %d", w.Code)
		}
	}
	_, name, version, p := component.PackageComponent("swift", "mona.linkedlist", "1.0.0")
	if name != "mona.linkedlist" || version != "1.0.0" || p != "mona/linkedlist/1.0.0/source-archive.zip" {
		t.Fatalf("PackageComponent: %s %s %s", name, version, p)
	}
	c := component.Component{Repo: "swift-hosted", Format: "swift", Name: "Mona.LinkedList", Version: "1.0.0"}
	res, err := e.h.DeleteComponent(ctx, e.repos["swift-hosted"], c, "admin")
	if err != nil || res.Assets != 1 {
		t.Fatalf("delete: %+v %v", res, err)
	}
	problemOf(t, e.do("GET", "swift-hosted", "mona/LinkedList/1.0.0", nil), 404)
	if _, err := e.h.Store.Get(ctx, "swift-hosted", p); err == nil {
		t.Fatal("archive still stored")
	}
	var list releaseList
	decode(t, e.do("GET", "swift-hosted", "mona/LinkedList", nil), &list)
	if len(list.Releases) != 1 {
		t.Fatalf("after delete: %+v", list)
	}
	if res, err := e.h.DeleteComponent(ctx, e.repos["swift-hosted"], c, "admin"); err != nil || res.Assets != 0 {
		t.Fatalf("delete again: %+v %v", res, err)
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["swift-proxy"], c, "admin"); err == nil {
		t.Fatal("proxy delete accepted")
	}
	if got := e.sink.Events(); got[len(got)-1].Kind != "delete" || got[len(got)-1].Subject != "Mona.LinkedList@1.0.0" {
		t.Fatalf("events: %+v", got)
	}
}

func TestParseDependencies(t *testing.T) {
	got := ParseDependencies(manifest)
	want := []Dependency{
		{Name: "swift-argument-parser", URL: "https://github.com/apple/swift-argument-parser.git", Requirement: `from: "1.2.0"`},
		{Name: "swift-collections", URL: "https://github.com/apple/swift-collections", Requirement: `.upToNextMajor(from: "1.0.0")`},
		{Name: "Nimble", URL: "https://github.com/Quick/Nimble.git", Requirement: `exact: "12.0.0"`},
		{Name: "mona.Other", Requirement: `"1.0.0"..<"2.0.0"`},
		{Name: "../Local", Requirement: ""},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	if d := ParseDependencies("let package = Package(name: \"x\")"); len(d) != 0 {
		t.Fatalf("no deps: %+v", d)
	}
	for _, v := range []string{"1.0.0", "1.0.0-alpha.1", "1.0.0+build", "10.20.30-rc.1+x.y"} {
		if !ValidVersion(v) {
			t.Errorf("%s invalid", v)
		}
	}
	for _, v := range []string{"v1.0.0", "1.0", "1.0.0-", "1.0.0-01", "01.0.0", "1.0.0 "} {
		if ValidVersion(v) {
			t.Errorf("%s valid", v)
		}
	}
}
