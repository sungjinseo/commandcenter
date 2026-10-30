package nexusapi_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/app"
	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
)

// startSearch runs the app with two maven hosted repositories behind the
// group maven-public, raw, npm and pypi hosted repositories and a raw proxy
// of remote; "ci" may write everywhere, anonymous callers read nothing.
func startSearch(t *testing.T, remote string) (*env, *app.App) {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
dataDir: %s
baseURL: http://nexus.example.com
anonymousRead: false
users:
  - { name: admin, passwordHash: %q, roles: [admin] }
  - { name: ci, passwordHash: %q, roles: [deployer] }
roles:
  deployer: { write: [maven-releases, maven-other, maven-snapshots, raw-files, npm-hosted, raw-px] }
repositories:
  - { name: maven-releases, format: maven, type: hosted }
  - { name: maven-other, format: maven, type: hosted }
  - { name: maven-snapshots, format: maven, type: hosted, versionPolicy: snapshot }
  - { name: maven-public, format: maven, type: group, members: [maven-releases, maven-other, maven-snapshots] }
  - { name: raw-files, format: raw, type: hosted }
  - { name: raw-px, format: raw, type: proxy, remote: %q }
  - { name: npm-hosted, format: npm, type: hosted }
  - { name: pypi-hosted, format: pypi, type: hosted }
`, t.TempDir(), string(h), string(h), remote)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.URL = metatest.URL(t, cfg.DataDir)
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(func() { ts.Close(); a.Close() })
	return &env{t, ts}, a
}

// items GETs a page of path as user and returns its items' field.
func (e *env) items(path, user, field string) []string {
	e.t.Helper()
	var p page[map[string]any]
	e.get(path, user, &p)
	var out []string
	for _, it := range p.Items {
		out = append(out, fmt.Sprint(it[field]))
	}
	return out
}

func eq(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

// npmPublish is the body `npm publish` sends for name@version.
func npmPublish(name, version string) string {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	pj, _ := json.Marshal(map[string]string{"name": name, "version": version})
	tw.WriteHeader(&tar.Header{Name: "package/package.json", Mode: 0o644, Size: int64(len(pj)), Typeflag: tar.TypeReg})
	tw.Write(pj)
	tw.Close()
	gz.Close()
	tgz := buf.Bytes()
	s1, s512 := sha1.Sum(tgz), sha512.Sum512(tgz)
	doc, _ := json.Marshal(map[string]any{
		"_id": name, "name": name, "dist-tags": map[string]string{"latest": version},
		"versions": map[string]any{version: map[string]any{"name": name, "version": version,
			"dist": map[string]any{"shasum": hex.EncodeToString(s1[:]), "integrity": "sha512-" + base64.StdEncoding.EncodeToString(s512[:])}}},
		"_attachments": map[string]any{name + "-" + version + ".tgz": map[string]any{
			"content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(tgz), "length": len(tgz)}},
	})
	return string(doc)
}

// A search that names a group searches its members: each hit keeps its
// member, its download goes through the group; several names and
// wildcards select several repositories (rest-08, core-68).
func TestSearchGroupRepository(t *testing.T) {
	e, _ := startSearch(t, "http://127.0.0.1:1/")
	e.upload("maven-releases/org/x/a/1.0/a-1.0.jar", "maven-other/org/x/b/1.0/b-1.0.jar")
	st, _, hd := e.do("GET", v1+"/search/assets/download?repository=maven-public&maven.artifactId=a&maven.extension=jar", "ci", "")
	if st != 302 || hd.Get("Location") != "http://nexus.example.com/repository/maven-public/org/x/a/1.0/a-1.0.jar" {
		t.Fatalf("download through the group: %d %q", st, hd.Get("Location"))
	}
	eq(t, "group search", e.items(v1+"/search?repository=maven-public", "ci", "repository"), "maven-other", "maven-releases")
	var assets page[asset]
	e.get(v1+"/search/assets?repository=maven-public&name=b", "ci", &assets)
	if len(assets.Items) != 1 || assets.Items[0].Repository != "maven-other" || !strings.HasPrefix(assets.Items[0].DownloadURL, "http://nexus.example.com/repository/maven-public/") {
		t.Fatalf("group asset search: %+v", assets.Items)
	}
	// A member named directly keeps its own URL.
	e.get(v1+"/search/assets?repository=maven-other&name=b", "ci", &assets)
	if !strings.HasPrefix(assets.Items[0].DownloadURL, "http://nexus.example.com/repository/maven-other/") {
		t.Fatalf("member asset search: %+v", assets.Items)
	}
	eq(t, "list", e.items(v1+"/search?repository="+url.QueryEscape("maven-releases maven-other"), "ci", "name"), "b", "a")
	eq(t, "wildcard", e.items(v1+"/search?repository=maven-o*", "ci", "name"), "b")
	eq(t, "? wildcard", e.items(v1+"/search?repository=maven-o?her", "ci", "name"), "b")
	e.want("GET", v1+"/search?repository=nope", "ci", "", 404)
	e.want("GET", v1+"/search?repository=nope*", "ci", "", 404)
	e.want("GET", v1+"/search?repository=maven-public", "", "", 401)
}

// An asset parameter given empty keeps the files without that attribute:
// the documented "main jar" download (rest-09).
func TestSearchEmptyClassifier(t *testing.T) {
	e, _ := startSearch(t, "http://127.0.0.1:1/")
	e.upload("maven-releases/org/x/a/1.0/a-1.0-javadoc.jar", "maven-releases/org/x/a/1.0/a-1.0-sources.jar", "maven-releases/org/x/a/1.0/a-1.0.jar",
		"maven-releases/org/x/a/0.9/a-0.9.jar")
	st, _, hd := e.do("GET", v1+"/search/assets/download?maven.artifactId=a&maven.extension=jar&maven.classifier=&sort=version", "ci", "")
	if st != 302 || !strings.HasSuffix(hd.Get("Location"), "/org/x/a/1.0/a-1.0.jar") {
		t.Fatalf("main jar: %d %q", st, hd.Get("Location"))
	}
	eq(t, "no classifier", e.items(v1+"/search/assets?maven.artifactId=a&maven.classifier=", "ci", "path"),
		"/org/x/a/0.9/a-0.9.jar", "/org/x/a/1.0/a-1.0.jar")
	// Maven lists the main file first, then the classified ones in path order.
	eq(t, "absent parameter", e.items(v1+"/search/assets?maven.artifactId=a&version=1.0", "ci", "path"),
		"/org/x/a/1.0/a-1.0.jar", "/org/x/a/1.0/a-1.0-javadoc.jar", "/org/x/a/1.0/a-1.0-sources.jar")
	// The full attribute names work as the aliases do.
	eq(t, "full names", e.items(v1+"/search/assets?attributes.maven2.artifactId=a&assets.attributes.maven2.classifier=sources", "ci", "path"),
		"/org/x/a/1.0/a-1.0-sources.jar")
}

// Checksums, prerelease and last_updated select; a maven parameter implies
// format=maven2; bad values and unknown sorts are 400 (rest-10, core-67,
// core-69).
func TestSearchCriteria(t *testing.T) {
	e, _ := startSearch(t, "http://127.0.0.1:1/")
	e.upload("maven-releases/org/x/a/1.0/a-1.0.jar", "maven-releases/org/x/b/1.0/b-1.0.jar", "maven-snapshots/org/x/a/2.0-SNAPSHOT/a-2.0-20261001.120000-1.jar",
		"raw-files/x/f.txt")
	var p page[asset]
	e.get(v1+"/search/assets?name=a&version=1.0", "ci", &p)
	one := p.Items[0]
	for _, algo := range []string{"sha1", "sha256", "sha512", "md5"} {
		eq(t, algo, e.items(v1+"/search?"+algo+"="+one.Checksum[algo], "ci", "name"), "a")
		eq(t, algo+" upper case", e.items(v1+"/search/assets?"+algo+"="+strings.ToUpper(one.Checksum[algo]), "ci", "path"), "/org/x/a/1.0/a-1.0.jar")
		eq(t, algo+" unknown", e.items(v1+"/search?"+algo+"=deadbeef", "ci", "name"))
	}
	eq(t, "full checksum name", e.items(v1+"/search?assets.attributes.checksum.sha1="+one.Checksum["sha1"], "ci", "name"), "a")
	st, _, hd := e.do("GET", v1+"/search/assets/download?sha1="+one.Checksum["sha1"], "ci", "")
	if st != 302 || !strings.HasSuffix(hd.Get("Location"), "/a-1.0.jar") {
		t.Fatalf("download by sha1: %d %q", st, hd.Get("Location"))
	}
	e.want("GET", v1+"/search/assets/download?sha1=deadbeef", "ci", "", 404)

	eq(t, "prerelease", e.items(v1+"/search?name=a&prerelease=true", "ci", "repository"), "maven-snapshots")
	eq(t, "release", e.items(v1+"/search?name=a&prerelease=false", "ci", "repository"), "maven-releases")
	b := e.want("GET", v1+"/search?prerelease=maybe", "ci", "", 400)
	if !strings.Contains(string(b), `"id":"prerelease"`) {
		t.Fatalf("prerelease error: %s", b)
	}
	if got := e.items(v1+"/search?last_updated=2000-01-01", "ci", "name"); len(got) != 4 {
		t.Fatalf("last_updated in the past: %q", got)
	}
	eq(t, "last_updated in the future", e.items(v1+"/search?lastBlobUpdated=2999-01-01T00:00:00Z", "ci", "name"))
	e.want("GET", v1+"/search?last_updated=yesterday", "ci", "", 400)

	// x/f.txt has the group /x (and x); a maven parameter keeps maven only.
	eq(t, "raw group", e.items(v1+"/search?group=x", "ci", "name"), "/x/f.txt")
	eq(t, "implied format", e.items(v1+"/search?maven.groupId=x", "ci", "name"))
	eq(t, "maven group", e.items(v1+"/search?maven.groupId=org.x&sort=name&direction=asc", "ci", "name"), "a", "a", "b")

	b = e.want("GET", v1+"/search?sort=size", "ci", "", 400)
	if !strings.Contains(string(b), `"id":"sort"`) {
		t.Fatalf("sort error: %s", b)
	}
	e.want("GET", v1+"/search?sort=name&direction=sideways", "ci", "", 400)
	e.want("GET", v1+"/search?timeout=10", "ci", "", 200)
}

// q splits into alternatives and takes the group:name:version form; every
// value splits on spaces, quotes keep a phrase, "?" is a wildcard
// (core-66, rest-12).
func TestSearchKeywordAndValues(t *testing.T) {
	e, _ := startSearch(t, "http://127.0.0.1:1/")
	e.upload("maven-releases/org/x/lib/1.0/lib-1.0.jar", "maven-releases/org/x/lib/1.0/lib-1.0-sources.jar", "maven-releases/org/x/lib/1.0.1/lib-1.0.1.jar",
		"maven-releases/org/x/lib/2.0/lib-2.0.jar", "maven-releases/com/y/tool/2.0/tool-2.0.jar", "raw-files/notes/my%20file.txt")
	eq(t, "gav", e.items(v1+"/search?q=org.x:lib:1.0", "ci", "version"), "1.0")
	eq(t, "ga", e.items(v1+"/search?q=org.x:lib&sort=version", "ci", "version"), "2.0", "1.0.1", "1.0")
	eq(t, "gav extension classifier", e.items(v1+"/search?q=org.x:lib::jar", "ci", "version")) // an empty part is no GAV
	eq(t, "gavec", e.items(v1+"/search?q=org.x:lib:1.0:jar:sources", "ci", "version"), "1.0")
	eq(t, "gavec none", e.items(v1+"/search?q=org.x:lib:2.0:jar:sources", "ci", "version"))
	eq(t, "q alternatives", e.items(v1+"/search?q="+url.QueryEscape("tool 1.0.1")+"&sort=name&direction=asc", "ci", "version"), "1.0.1", "2.0")
	eq(t, "q phrase", e.items(v1+"/search?q="+url.QueryEscape(`"my file"`), "ci", "name"), "/notes/my file.txt")
	eq(t, "version alternatives", e.items(v1+"/search?name=lib&version="+url.QueryEscape("1.0 2.0")+"&sort=version", "ci", "version"), "2.0", "1.0")
	eq(t, "? wildcard", e.items(v1+"/search?name=too?", "ci", "name"), "tool")
	eq(t, "quoted name", e.items(v1+"/search?name="+url.QueryEscape(`"notes/my file.txt"`), "ci", "name"), "/notes/my file.txt")
}

// A raw file's group is its directory and its name has a leading "/";
// searches accept both spellings (aptraw-13, rest-05).
func TestSearchRawGroupAndName(t *testing.T) {
	e, _ := startSearch(t, "http://127.0.0.1:1/")
	e.upload("raw-files/docs/a.txt", "raw-files/b.txt")
	eq(t, "group=/docs", e.items(v1+"/search?repository=raw-files&group=/docs", "ci", "name"), "/docs/a.txt")
	eq(t, "group=/", e.items(v1+"/search?repository=raw-files&group=/", "ci", "name"), "/b.txt")
	eq(t, "name with /", e.items(v1+"/search?name=/docs/a.txt", "ci", "group"), "/docs")
	eq(t, "name without /", e.items(v1+"/search?name=docs/a.txt", "ci", "group"), "/docs")
	eq(t, "sort by group", e.items(v1+"/search?repository=raw-files&sort=group&direction=desc", "ci", "name"), "/docs/a.txt", "/b.txt")
}

// sort=version uses the format's version order: a Maven qualifier sorts
// before its release (rest-11's sort).
func TestSearchSortVersionByFormat(t *testing.T) {
	e, _ := startSearch(t, "http://127.0.0.1:1/")
	e.upload("maven-releases/org/x/a/1.0-beta/a-1.0-beta.jar", "maven-releases/org/x/a/1.0/a-1.0.jar", "maven-releases/org/x/a/1.0-rc1/a-1.0-rc1.jar")
	eq(t, "desc", e.items(v1+"/search?name=a&sort=version&direction=desc", "ci", "version"), "1.0", "1.0-rc1", "1.0-beta")
	eq(t, "default desc", e.items(v1+"/search?name=a&sort=version", "ci", "version"), "1.0", "1.0-rc1", "1.0-beta")
	st, _, hd := e.do("GET", v1+"/search/assets/download?name=a&sort=version", "ci", "")
	if st != 302 || !strings.HasSuffix(hd.Get("Location"), "/a-1.0.jar") {
		t.Fatalf("newest: %d %q", st, hd.Get("Location"))
	}
}

// pageIDs pages through path as ci, calling each page's items' ids with
// del before asking for the next page (a cleanup script), and returns the
// field of every item seen.
func (e *env) pageAll(path, field string, del func(id string)) []string {
	e.t.Helper()
	var seen []string
	token := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			e.t.Fatalf("%s: too many pages", path)
		}
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		var p page[map[string]any]
		e.get(path+sep+"continuationToken="+url.QueryEscape(token), "ci", &p)
		for _, it := range p.Items {
			seen = append(seen, fmt.Sprint(it[field]))
			if del != nil {
				del(it["id"].(string))
			}
		}
		if p.ContinuationToken == nil {
			return seen
		}
		token = *p.ContinuationToken
	}
}

// A cleanup script deletes every item of a page, the last one included,
// then asks for the next page: the token still works, for components,
// assets, search and asset search, also with a sort (rest-03).
func TestPagingWhileDeleting(t *testing.T) {
	e, _ := startSearch(t, "http://127.0.0.1:1/")
	var paths []string
	for i := 0; i < 160; i++ {
		paths = append(paths, fmt.Sprintf("maven-releases/org/x/a%03d/1.0/a%03d-1.0.jar", i, i), fmt.Sprintf("raw-files/f%03d", i))
	}
	e.upload(paths...)
	delComp := func(id string) { e.want("DELETE", v1+"/components/"+id, "ci", "", 204) }
	delAsset := func(id string) { e.want("DELETE", v1+"/assets/"+id, "ci", "", 204) }

	got := e.pageAll(v1+"/search?repository=maven-releases&name=a0*&sort=name&direction=desc", "name", delComp)
	if len(got) != 100 || got[0] != "a099" || got[99] != "a000" {
		t.Fatalf("search with sort: %d %q", len(got), got)
	}
	if got = e.pageAll(v1+"/components?repository=maven-releases", "name", delComp); len(got) != 60 || got[0] != "a100" || got[59] != "a159" {
		t.Fatalf("components: %d %q", len(got), got)
	}
	if got = e.pageAll(v1+"/search/assets?repository=raw-files&name=f0*", "path", delAsset); len(got) != 100 || got[0] != "/f000" || got[99] != "/f099" {
		t.Fatalf("search/assets: %d %q", len(got), got)
	}
	if got = e.pageAll(v1+"/assets?repository=raw-files", "path", delAsset); len(got) != 60 || got[0] != "/f100" || got[59] != "/f159" {
		t.Fatalf("assets: %d %q", len(got), got)
	}
	if left := e.items(v1+"/search?repository="+url.QueryEscape("maven-releases raw-files"), "ci", "name"); len(left) != 0 {
		t.Fatalf("left over: %q", left)
	}
	e.want("GET", v1+"/search?continuationToken=bogus", "ci", "", 400)
	e.want("GET", v1+"/search/assets?continuationToken=bogus", "ci", "", 400)
}

// A group's components and assets are its members': each item keeps its
// member repository and id, its downloads go through the group, and the
// pages cross members (rest-04).
func TestGroupListing(t *testing.T) {
	e, _ := startSearch(t, "http://127.0.0.1:1/")
	var paths []string
	for i := 0; i < 30; i++ {
		paths = append(paths, fmt.Sprintf("maven-releases/org/x/r%02d/1.0/r%02d-1.0.jar", i, i), fmt.Sprintf("maven-other/org/x/o%02d/1.0/o%02d-1.0.jar", i, i))
	}
	e.upload(paths...)
	got := e.pageAll(v1+"/components?repository=maven-public", "repository", nil)
	if len(got) != 60 || got[0] != "maven-other" || got[59] != "maven-releases" {
		t.Fatalf("group components: %d %q", len(got), got)
	}
	var p page[comp]
	e.get(v1+"/components?repository=maven-public", "ci", &p)
	if c := p.Items[0]; c.Name != "o00" || !strings.HasPrefix(c.Assets[0].DownloadURL, "http://nexus.example.com/repository/maven-public/") {
		t.Fatalf("group component: %+v", c)
	}
	var one comp
	e.get(v1+"/components/"+p.Items[0].ID, "ci", &one) // the member's id
	if one.Repository != "maven-other" {
		t.Fatalf("component by id: %+v", one)
	}
	got = e.pageAll(v1+"/assets?repository=maven-public", "path", nil) // the jars and their maven-metadata.xml
	if len(got) != 120 || got[0] != "/org/x/o00/1.0/o00-1.0.jar" || got[119] != "/org/x/r29/maven-metadata.xml" {
		t.Fatalf("group assets: %d %q", len(got), got)
	}
	e.want("GET", v1+"/assets?repository=maven-public&continuationToken="+base64.RawURLEncoding.EncodeToString([]byte("raw-files:f")), "ci", "", 400)
}

// An asset as Nexus 3.91 renders it: the path with "/", the maven2
// attributes, blobUpdated and lastVerified (rest-05).
func TestAssetJSON(t *testing.T) {
	e, _ := startSearch(t, "http://127.0.0.1:1/")
	e.upload("maven-releases/org/x/a/1.0/a-1.0-sources.jar", "raw-files/r.txt")
	var p page[map[string]any]
	e.get(v1+"/search/assets?name=a", "ci", &p)
	a := p.Items[0]
	attrs, _ := a["maven2"].(map[string]any)
	if a["path"] != "/org/x/a/1.0/a-1.0-sources.jar" || a["downloadUrl"] != "http://nexus.example.com/repository/maven-releases/org/x/a/1.0/a-1.0-sources.jar" ||
		attrs["groupId"] != "org.x" || attrs["artifactId"] != "a" || attrs["version"] != "1.0" || attrs["extension"] != "jar" || attrs["classifier"] != "sources" ||
		a["blobUpdated"] == nil || a["lastVerified"] != nil {
		t.Fatalf("maven asset: %v", a)
	}
	var one map[string]any
	e.get(v1+"/assets/"+a["id"].(string), "ci", &one)
	if one["path"] != a["path"] {
		t.Fatalf("asset by id: %v", one)
	}
	// An id built from the "/" path names the same asset.
	e.want("GET", v1+"/assets/"+base64.RawURLEncoding.EncodeToString([]byte("maven-releases:/org/x/a/1.0/a-1.0-sources.jar")), "ci", "", 200)
	e.get(v1+"/assets?repository=raw-files", "ci", &p)
	if raw, ok := p.Items[0]["raw"].(map[string]any); !ok || len(raw) != 0 || p.Items[0]["path"] != "/r.txt" {
		t.Fatalf("raw asset: %v", p.Items[0])
	}
}

// DELETE /v1/assets/{id} deletes one file of any format by its rules
// (rest-06).
func TestDeleteOneAsset(t *testing.T) {
	var hits atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "remote "+r.URL.Path)
	}))
	defer remote.Close()
	e, a := startSearch(t, remote.URL+"/")

	// Maven: the sources jar goes, the jar and the metadata stay valid.
	e.upload("maven-releases/org/x/a/1.0/a-1.0.jar", "maven-releases/org/x/a/1.0/a-1.0-sources.jar", "maven-releases/org/x/a/1.0/a-1.0.pom")
	var p page[asset]
	e.get(v1+"/search/assets?name=a&maven.classifier=sources", "ci", &p)
	id := p.Items[0].ID
	e.want("DELETE", v1+"/assets/"+id, "", "", 401)
	e.want("DELETE", v1+"/assets/"+id, "ci", "", 204)
	e.want("GET", v1+"/assets/"+id, "ci", "", 404)
	e.want("GET", "/repository/maven-releases/org/x/a/1.0/a-1.0-sources.jar", "ci", "", 404)
	e.want("GET", "/repository/maven-releases/org/x/a/1.0/a-1.0.jar", "ci", "", 200)
	if md := e.want("GET", "/repository/maven-releases/org/x/a/maven-metadata.xml", "ci", "", 200); !strings.Contains(string(md), "<version>1.0</version>") {
		t.Fatalf("metadata: %s", md)
	}
	eq(t, "files left", e.items(v1+"/search/assets?name=a", "ci", "path"), "/org/x/a/1.0/a-1.0.jar", "/org/x/a/1.0/a-1.0.pom")

	// npm: a tarball is a version; the packument loses it.
	e.want("PUT", "/repository/npm-hosted/demo", "ci", npmPublish("demo", "1.0.0"), 201)
	e.want("PUT", "/repository/npm-hosted/demo", "ci", npmPublish("demo", "1.1.0"), 201)
	e.get(v1+"/search/assets?repository=npm-hosted&name=demo&version=1.0.0", "ci", &p)
	if len(p.Items) != 1 || p.Items[0].Path != "/demo/-/demo-1.0.0.tgz" {
		t.Fatalf("npm tarball: %+v", p.Items)
	}
	e.want("DELETE", v1+"/assets/"+p.Items[0].ID, "ci", "", 204)
	var doc struct{ Versions map[string]any }
	json.Unmarshal(e.want("GET", "/repository/npm-hosted/demo", "ci", "", 200), &doc)
	if _, gone := doc.Versions["1.0.0"]; gone || doc.Versions["1.1.0"] == nil {
		t.Fatalf("packument versions: %v", doc.Versions)
	}

	// PyPI: one file of a release with two is refused.
	ctx := context.Background()
	for _, f := range []string{"packages/demo/demo-1.0.tar.gz", "packages/demo/demo-1.0-py3-none-any.whl"} {
		if _, err := a.Store.Put(ctx, "pypi-hosted", f, "", strings.NewReader(f)); err != nil {
			t.Fatal(err)
		}
	}
	e.get(v1+"/assets?repository=pypi-hosted", "ci", &p)
	if b := e.want("DELETE", v1+"/assets/"+p.Items[0].ID, "admin", "", 400); !strings.Contains(string(b), "component") {
		t.Fatalf("pypi: %s", b)
	}

	// Proxy: the cached copy goes and is fetched again.
	e.want("GET", "/repository/raw-px/dir/f.bin", "ci", "", 200)
	e.get(v1+"/assets?repository=raw-px", "ci", &p)
	if len(p.Items) != 1 || hits.Load() != 1 {
		t.Fatalf("cached: %+v, %d remote requests", p.Items, hits.Load())
	}
	e.want("DELETE", v1+"/assets/"+p.Items[0].ID, "ci", "", 204)
	eq(t, "proxy after delete", e.items(v1+"/assets?repository=raw-px", "ci", "path"))
	e.want("GET", "/repository/raw-px/dir/f.bin", "ci", "", 200)
	if hits.Load() != 2 {
		t.Fatalf("%d remote requests", hits.Load())
	}
}

// GET /v1/tasks?type= lists the tasks of one type, by Nexus's id or
// Command Center's (rest-26).
func TestTasksByType(t *testing.T) {
	e, _ := startSearch(t, "http://127.0.0.1:1/")
	all := e.items(v1+"/tasks", "admin", "type")
	if !slices.Contains(all, "blobstore.compact") {
		t.Fatalf("tasks: %q", all)
	}
	for _, typ := range []string{"blobstore.compact", "repository.cleanup", "nope"} {
		for _, got := range e.items(v1+"/tasks?type="+typ, "admin", "type") {
			if got != typ {
				t.Fatalf("type=%s lists %s", typ, got)
			}
		}
	}
	if got := e.items(v1+"/tasks?type=blobstore.compact", "admin", "type"); len(got) == 0 {
		t.Fatal("no compact task")
	}
	e.want("POST", v1+"/tasks", "admin", `{"type":"repository.maven.rebuild-metadata","name":"rebuild","frequency":{"schedule":"manual"},"properties":{"repositoryName":"maven-releases"}}`, 201)
	eq(t, "nexus id", e.items(v1+"/tasks?type=repository.maven.rebuild-metadata", "admin", "name"), "rebuild")
	eq(t, "command center id", e.items(v1+"/tasks?type=repository.rebuild-metadata", "admin", "name"), "rebuild")
}

// Listing follows browse or read, also through a group that contains the
// repository (security-11), and a content selector's expression filters
// what a selector-only user lists, searches and gets (security-12).
func TestListingPermissions(t *testing.T) {
	e, _ := startSearch(t, "http://127.0.0.1:1/")
	e.upload("maven-releases/org/acme/lib/1.0/lib-1.0.jar", "maven-releases/org/other/lib/1.0/lib-1.0.jar", "maven-other/org/x/o/1.0/o-1.0.jar")
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/-/api/settings", `{"authenticatedRead":false}`},
		{"POST", "/-/api/content-selectors", `{"name":"acme","expression":"path =^ \"/org/acme/\""}`},
		{"POST", "/-/api/roles", `{"id":"browser","privileges":[{"target":"repo:maven-releases","actions":["browse"]}]}`},
		{"POST", "/-/api/roles", `{"id":"grouper","privileges":[{"target":"repo:maven-public","actions":["read"]}]}`},
		{"POST", "/-/api/roles", `{"id":"acme","privileges":[{"target":"selector:acme","actions":["browse","read"],"repos":["maven-releases"]}]}`},
		{"POST", "/-/api/users", `{"id":"bob","password":"longenough","roles":["browser"]}`},
		{"POST", "/-/api/users", `{"id":"gina","password":"longenough","roles":["grouper"]}`},
		{"POST", "/-/api/users", `{"id":"dev","password":"longenough","roles":["acme"]}`},
		{"POST", "/-/api/users", `{"id":"nobody","password":"longenough","roles":[]}`},
	} {
		if st, b := uiCall(e, c.method, c.path, c.body); st >= 300 {
			t.Fatalf("%s %s: %d %s", c.method, c.path, st, b)
		}
	}
	as := func(user, path string) (int, []string) {
		t.Helper()
		st, b := getAs(e, path, user, "longenough")
		var p page[map[string]any]
		json.Unmarshal(b, &p)
		var names []string
		for _, it := range p.Items {
			names = append(names, fmt.Sprint(it["name"])+fmt.Sprint(it["path"]))
		}
		return st, names
	}
	// Browse only: listed and listable.
	if st, b := getAs(e, v1+"/repositories", "bob", "longenough"); st != 200 || !strings.Contains(string(b), `"maven-releases"`) || strings.Contains(string(b), `"maven-other"`) {
		t.Fatalf("bob's repositories: %d %s", st, b)
	}
	if st, got := as("bob", v1+"/components?repository=maven-releases"); st != 200 || len(got) != 2 {
		t.Fatalf("bob's components: %d %q", st, got)
	}
	// Read on the group: its members' content, by member or through the group.
	if st, got := as("gina", v1+"/components?repository=maven-other"); st != 200 || len(got) != 1 {
		t.Fatalf("gina's member components: %d %q", st, got)
	}
	if st, got := as("gina", v1+"/search?q=lib"); st != 200 || len(got) != 2 {
		t.Fatalf("gina's search: %d %q", st, got)
	}
	if st, _ := as("nobody", v1+"/components?repository=maven-releases"); st != 404 {
		t.Fatalf("nobody: %d", st)
	}
	// Selector only: just the paths the expression matches.
	if st, got := as("dev", v1+"/components?repository=maven-releases"); st != 200 || len(got) != 1 || got[0] != "lib<nil>" {
		t.Fatalf("dev's components: %d %q", st, got)
	}
	if st, got := as("dev", v1+"/assets?repository=maven-releases"); st != 200 || !slices.Equal(got, []string{"<nil>/org/acme/lib/1.0/lib-1.0.jar", "<nil>/org/acme/lib/maven-metadata.xml"}) {
		t.Fatalf("dev's assets: %d %q", st, got)
	}
	var p page[asset]
	e.get(v1+"/search/assets?repository=maven-releases&name=lib", "admin", &p)
	if len(p.Items) != 2 {
		t.Fatalf("admin's search: %+v", p.Items)
	}
	if st, got := as("dev", v1+"/search/assets?name=lib"); st != 200 || !slices.Equal(got, []string{"<nil>/org/acme/lib/1.0/lib-1.0.jar"}) {
		t.Fatalf("dev's search: %d %q", st, got)
	}
	for _, x := range p.Items {
		want := 404
		if strings.HasPrefix(x.Path, "/org/acme/") {
			want = 200
		}
		if st, _ := getAs(e, v1+"/assets/"+x.ID, "dev", "longenough"); st != want {
			t.Fatalf("dev GET %s: %d, want %d", x.Path, st, want)
		}
	}
}
