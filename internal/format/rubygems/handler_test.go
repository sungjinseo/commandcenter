package rubygems

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// gemspec is a metadata.gz as `gem build` writes one: Psych's tagged
// Gem::Version, Gem::Requirement and Gem::Dependency mappings, symbols
// for the dependency types, a timestamp date.
func gemspec(name, version, platform string, deps bool) string {
	s := `--- !ruby/object:Gem::Specification
name: ` + name + `
version: !ruby/object:Gem::Version
  version: ` + version + `
platform: ` + platform + `
authors:
- Jane Doe
autorequire:
bindir: bin
cert_chain: []
date: 2026-10-27 00:00:00.000000000 Z
dependencies:`
	if !deps {
		s += " []"
	} else {
		s += `
- !ruby/object:Gem::Dependency
  name: rake
  requirement: !ruby/object:Gem::Requirement
    requirements:
    - - "~>"
      - !ruby/object:Gem::Version
        version: '13.0'
    - - ">="
      - !ruby/object:Gem::Version
        version: 13.0.1
  type: :development
  prerelease: false
  version_requirements: !ruby/object:Gem::Requirement
    requirements:
    - - "~>"
      - !ruby/object:Gem::Version
        version: '13.0'
- !ruby/object:Gem::Dependency
  name: rack
  requirement: !ruby/object:Gem::Requirement
    requirements:
    - - ">="
      - !ruby/object:Gem::Version
        version: '2.0'
  type: :runtime
  prerelease: false
  version_requirements: !ruby/object:Gem::Requirement
    requirements:
    - - ">="
      - !ruby/object:Gem::Version
        version: '2.0'`
	}
	return s + `
description: A demo gem
email: jane@example.org
executables: []
extensions: []
extra_rdoc_files: []
files:
- lib/` + name + `.rb
homepage: https://example.org/demo
licenses:
- MIT
metadata:
  source_code_uri: https://git.example.org/demo
post_install_message:
rdoc_options: []
require_paths:
- lib
required_ruby_version: !ruby/object:Gem::Requirement
  requirements:
  - - ">="
    - !ruby/object:Gem::Version
      version: 2.7.0
required_rubygems_version: !ruby/object:Gem::Requirement
  requirements:
  - - ">="
    - !ruby/object:Gem::Version
      version: '0'
requirements: []
rubygems_version: 3.5.0
signing_key:
specification_version: 4
summary: A demo
test_files: []
`
}

func gzipped(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(body))
	gz.Close()
	return buf.Bytes()
}

// tarball packs files in order, as a .gem is: metadata.gz, data.tar.gz,
// checksums.yaml.gz.
func tarball(t *testing.T, files [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o444, Size: int64(len(f[1])), ModTime: time.Unix(0, 0)}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(f[1]))
	}
	tw.Close()
	return buf.Bytes()
}

// gem builds a .gem of name version[-platform] as gem build does.
func gem(t *testing.T, name, version, platform string, deps bool) []byte {
	t.Helper()
	if platform == "" {
		platform = "ruby"
	}
	data := string(tarball(t, [][2]string{{"lib/" + name + ".rb", "puts 1\n"}}))
	return tarball(t, [][2]string{
		{"metadata.gz", string(gzipped(t, gemspec(name, version, platform, deps)))},
		{"data.tar.gz", string(gzipped(t, data))},
		{"checksums.yaml.gz", string(gzipped(t, "---\nSHA256:\n  metadata.gz: x\n"))},
	})
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type env struct {
	h     *Handler
	repos map[string]*repo.Repository
	sink  *repotest.Sink
	up    string       // the upstream server URL
	hits  atomic.Int32 // upstream gem downloads
	deps  atomic.Pointer[string]
	down  atomic.Bool
	now   time.Time
	gems  map[string][]byte // upstream gems by file name
}

const upVersions = "created_at: 2026-01-01T00:00:00Z\n---\nrack 2.2.8,3.0.0 1111\ndemo 9.0.0 2222\n"

// upstream is a fake rubygems.org: the compact index, gems, gemspecs,
// legacy specs and the dependency API.
func (e *env) upstream(w http.ResponseWriter, r *http.Request) {
	if e.down.Load() {
		http.Error(w, "down", 503)
		return
	}
	text := func(v string) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, v)
	}
	switch p := r.URL.Path; {
	case p == "/versions":
		text(upVersions)
	case p == "/names":
		text("---\ndemo\nrack\n")
	case p == "/info/rack":
		text("---\n2.2.8 |checksum:aa,ruby:>= 2.3.0\n3.0.0 |checksum:bb,ruby:>= 2.4.0,rubygems:>= 0\n")
	case p == "/info/demo":
		text("---\n9.0.0 |checksum:cc\n")
	case strings.HasPrefix(p, "/gems/") && e.gems[filepath.Base(p)] != nil:
		e.hits.Add(1)
		w.Write(e.gems[filepath.Base(p)])
	case p == "/quick/Marshal.4.8/rack-3.0.0.gemspec.rz":
		w.Write([]byte("rz-rack"))
	case p == "/specs.4.8.gz":
		body, _ := specsDoc("specs.4.8.gz", []specEntry{{"rack", "3.0.0", "ruby"}})
		w.Write(body)
	case p == "/api/v1/dependencies":
		q := r.URL.Query().Get("gems")
		e.deps.Store(&q)
		var entries []any
		for _, n := range strings.Split(q, ",") {
			switch n {
			case "rack":
				entries = append(entries, hash{{sym("name"), "rack"}, {sym("number"), "3.0.0"}, {sym("platform"), "ruby"}, {sym("dependencies"), []any{}}})
			case "demo":
				entries = append(entries, hash{{sym("name"), "demo"}, {sym("number"), "9.0.0"}, {sym("platform"), "ruby"}, {sym("dependencies"), []any{}}})
			}
		}
		body, _ := marshalDump(entries)
		w.Write(body)
	default:
		http.NotFound(w, r)
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), gems: map[string][]byte{}}
	ts := httptest.NewServer(http.HandlerFunc(e.upstream))
	t.Cleanup(ts.Close)
	e.up = ts.URL
	e.gems["rack-3.0.0.gem"] = gem(t, "rack", "3.0.0", "", false)
	e.gems["demo-9.0.0.gem"] = gem(t, "demo", "9.0.0", "", false)
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
		{Name: "gems-hosted", Format: "rubygems", Type: "hosted", WritePolicy: "allow_once"},
		{Name: "gems-allow", Format: "rubygems", Type: "hosted", WritePolicy: "allow"},
		{Name: "gems-ro", Format: "rubygems", Type: "hosted", WritePolicy: "deny"},
		{Name: "gems-proxy", Format: "rubygems", Type: "proxy", Remote: ts.URL + "/",
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "gems-all", Format: "rubygems", Type: "group", Members: []string{"gems-hosted", "gems-proxy"}, WriteMember: "gems-hosted"},
		{Name: "gems-proxy-first", Format: "rubygems", Type: "group", Members: []string{"gems-proxy", "gems-hosted"}},
	}}
	f := proxy.New(store, ts.Client(), 4)
	f.Now = func() time.Time { return e.now }
	e.h = New(store, f, auth.New(cfg, db))
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
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	path, _, _ = strings.Cut(path, "?")
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

// push sends a gem as gem push does, a minute after the previous one, so
// rows order by creation.
func (e *env) push(t *testing.T, repoName string, pkg []byte) *httptest.ResponseRecorder {
	t.Helper()
	e.now = e.now.Add(time.Minute)
	return e.do("POST", repoName, "api/v1/gems", pkg, "Content-Type", ctBinary)
}

func (e *env) get(t *testing.T, repoName, path string) string {
	t.Helper()
	w := e.do("GET", repoName, path, nil)
	if w.Code != 200 {
		t.Fatalf("GET %s/%s: %d %s", repoName, path, w.Code, w.Body)
	}
	return w.Body.String()
}

// TestHostedCompactIndex: pushes derive info/<gem> and names, and append
// to the versions file, byte for byte; the files carry the md5 ETag and
// sha-256 digest Bundler checks and answer Range and If-None-Match.
func TestHostedCompactIndex(t *testing.T) {
	e := newEnv(t)
	if w := e.do("GET", "gems-hosted", "info/demo", nil); w.Code != 404 {
		t.Fatalf("empty info: %d", w.Code)
	}
	if got := e.get(t, "gems-hosted", "versions"); got != "created_at: 2026-10-27T12:00:00Z\n---\n" {
		t.Fatalf("empty versions: %q", got)
	}
	demo100, demo110, demoJava, other := gem(t, "demo", "1.0.0", "", true), gem(t, "demo", "1.1.0", "", false), gem(t, "demo", "1.0.0", "java", true), gem(t, "other", "0.1.0", "", false)
	for i, g := range [][]byte{demo100, demo110, other, demoJava} {
		if w := e.push(t, "gems-hosted", g); w.Code != 200 || i == 0 && w.Body.String() != "Successfully registered gem: demo (1.0.0)\n" {
			t.Fatalf("push: %d %s", w.Code, w.Body)
		}
	}
	line100 := "1.0.0 rack:>= 2.0|checksum:" + sha256hex(demo100) + ",ruby:>= 2.7.0,rubygems:>= 0\n"
	line110 := "1.1.0 |checksum:" + sha256hex(demo110) + ",ruby:>= 2.7.0,rubygems:>= 0\n"
	lineJava := "1.0.0-java rack:>= 2.0|checksum:" + sha256hex(demoJava) + ",ruby:>= 2.7.0,rubygems:>= 0\n"
	info1, info2, info3 := "---\n"+line100, "---\n"+line100+line110, "---\n"+line100+line110+lineJava
	if got := e.get(t, "gems-hosted", "info/demo"); got != info3 {
		t.Fatalf("info:\n%s\nwant:\n%s", got, info3)
	}
	otherInfo := "---\n0.1.0 |checksum:" + sha256hex(other) + ",ruby:>= 2.7.0,rubygems:>= 0\n"
	if got := e.get(t, "gems-hosted", "info/other"); got != otherInfo {
		t.Fatalf("other info: %q", got)
	}
	if got := e.get(t, "gems-hosted", "names"); got != "---\ndemo\nother\n" {
		t.Fatalf("names: %q", got)
	}
	versions := "created_at: 2026-10-27T12:00:00Z\n---\n" + // the file was created by the first GET, before the pushes
		"demo 1.0.0 " + md5hex([]byte(info1)) + "\n" +
		"demo 1.1.0 " + md5hex([]byte(info2)) + "\n" +
		"other 0.1.0 " + md5hex([]byte(otherInfo)) + "\n" +
		"demo 1.0.0-java " + md5hex([]byte(info3)) + "\n"
	w := e.do("GET", "gems-hosted", "versions", nil)
	if w.Code != 200 || w.Body.String() != versions {
		t.Fatalf("versions: %d\n%s\nwant:\n%s", w.Code, w.Body, versions)
	}
	etag := `"` + md5hex([]byte(versions)) + `"`
	sum := sha256.Sum256([]byte(versions))
	if w.Header().Get("ETag") != etag || w.Header().Get("Repr-Digest") != "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("validators: %v", w.Header())
	}
	if w := e.do("HEAD", "gems-hosted", "versions", nil); w.Code != 200 || w.Header().Get("ETag") != etag {
		t.Fatalf("HEAD: %d %v", w.Code, w.Header())
	}
	if w := e.do("GET", "gems-hosted", "versions", nil, "If-None-Match", etag); w.Code != 304 {
		t.Fatalf("If-None-Match: %d", w.Code)
	}
	// Bundler's increment: from one byte before its copy's end, with its old ETag
	n := len(versions)
	if w := e.push(t, "gems-hosted", gem(t, "other", "0.2.0", "", false)); w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = e.do("GET", "gems-hosted", "versions", nil, "Range", fmt.Sprintf("bytes=%d-", n-1), "If-None-Match", etag)
	full := e.get(t, "gems-hosted", "versions")
	if w.Code != 206 || w.Body.String() != full[n-1:] || !strings.HasPrefix(w.Body.String(), "\nother 0.2.0 ") || w.Header().Get("ETag") != `"`+md5hex([]byte(full))+`"` {
		t.Fatalf("range: %d %q %v", w.Code, w.Body, w.Header())
	}
	if !strings.HasPrefix(full, versions) {
		t.Fatalf("not append-only:\n%s", full)
	}
	// info files answer ranges and conditionals the same way
	if w := e.do("GET", "gems-hosted", "info/demo", nil, "If-None-Match", `"`+md5hex([]byte(info3))+`"`); w.Code != 304 {
		t.Fatalf("info If-None-Match: %d", w.Code)
	}
	if w := e.do("GET", "gems-hosted", "info/demo", nil, "Range", fmt.Sprintf("bytes=%d-", len(info2)-1)); w.Code != 206 || w.Body.String() != "\n"+lineJava {
		t.Fatalf("info range: %d %q", w.Code, w.Body)
	}
	// the stored gem, and the events
	if w := e.do("GET", "gems-hosted", "gems/demo-1.0.0-java.gem", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), demoJava) || w.Header().Get("Content-Type") != ctBinary {
		t.Fatalf("gem: %d %v", w.Code, w.Header())
	}
	if w := e.do("GET", "gems-hosted", "gems/demo-2.0.0.gem", nil); w.Code != 404 {
		t.Fatalf("missing gem: %d", w.Code)
	}
	if got := e.sink.String(); !strings.Contains(got, "upload") || !strings.Contains(got, "demo@1.0.0-java") {
		t.Fatalf("events: %s", got)
	}
}

// loadGz reads a gzipped Marshal document.
func loadGz(t *testing.T, body []byte) any {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(gz)
	v, err := marshalLoad(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func specsOf(t *testing.T, v any) string {
	t.Helper()
	var out []string
	for _, e := range v.([]any) {
		tuple := e.([]any)
		out = append(out, fmt.Sprintf("%s-%s-%s", tuple[0], tuple[1].(userObj).val.([]any)[0], tuple[2]))
	}
	return strings.Join(out, " ")
}

// TestLegacyIndexes: specs.4.8.gz and its latest and prerelease variants,
// the quick gemspec and the dependency API are built from the rows.
func TestLegacyIndexes(t *testing.T) {
	e := newEnv(t)
	for _, g := range [][]byte{gem(t, "demo", "1.1.0", "", false), gem(t, "demo", "1.0.0", "", true), gem(t, "demo", "1.0.0", "java", false), gem(t, "demo", "2.0.0.pre.1", "", false), gem(t, "ab", "0.1.0", "", false)} {
		if w := e.push(t, "gems-hosted", g); w.Code != 200 {
			t.Fatalf("push: %d %s", w.Code, w.Body)
		}
	}
	for file, want := range map[string]string{
		"specs.4.8.gz":            "ab-0.1.0-ruby demo-1.0.0-ruby demo-1.0.0-java demo-1.1.0-ruby",
		"latest_specs.4.8.gz":     "ab-0.1.0-ruby demo-1.0.0-java demo-1.1.0-ruby",
		"prerelease_specs.4.8.gz": "demo-2.0.0.pre.1-ruby",
	} {
		w := e.do("GET", "gems-hosted", file, nil)
		if w.Code != 200 || w.Header().Get("Content-Type") != ctGzip {
			t.Fatalf("%s: %d %v", file, w.Code, w.Header())
		}
		if got := specsOf(t, loadGz(t, w.Body.Bytes())); got != want {
			t.Errorf("%s: %q, want %q", file, got, want)
		}
	}
	// the gemspec: zlib around Marshal around Gem::Specification._dump
	w := e.do("GET", "gems-hosted", "quick/Marshal.4.8/demo-1.0.0.gemspec.rz", nil)
	if w.Code != 200 {
		t.Fatalf("gemspec: %d", w.Code)
	}
	zr, err := zlib.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	v, err := marshalLoad(raw)
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := v.(userDump)
	if !ok || spec.class != "Gem::Specification" {
		t.Fatalf("gemspec: %#v", v)
	}
	inner, err := marshalLoad(spec.data)
	if err != nil {
		t.Fatal(err)
	}
	fields := inner.([]any)
	if fields[2] != "demo" || !reflect.DeepEqual(fields[3], gemVersion("1.0.0")) || fields[5] != "A demo" || fields[8] != "ruby" || len(fields[9].([]any)) != 2 || fields[16] != "ruby" {
		t.Fatalf("gemspec fields: %#v", fields)
	}
	if w := e.do("GET", "gems-hosted", "quick/Marshal.4.8/demo-1.0.0-java.gemspec.rz", nil); w.Code != 200 {
		t.Fatalf("java gemspec: %d", w.Code)
	}
	if w := e.do("GET", "gems-hosted", "quick/Marshal.4.8/demo-3.0.0.gemspec.rz", nil); w.Code != 404 {
		t.Fatalf("missing gemspec: %d", w.Code)
	}
	// the dependency API: every version of the gems asked, runtime deps only
	w = e.do("GET", "gems-hosted", "api/v1/dependencies?gems=demo,nope,ab", nil)
	if w.Code != 200 {
		t.Fatalf("dependencies: %d", w.Code)
	}
	deps, err := marshalLoad(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		hash{{sym("name"), "ab"}, {sym("number"), "0.1.0"}, {sym("platform"), "ruby"}, {sym("dependencies"), []any{}}},
		hash{{sym("name"), "demo"}, {sym("number"), "1.1.0"}, {sym("platform"), "ruby"}, {sym("dependencies"), []any{}}},
		hash{{sym("name"), "demo"}, {sym("number"), "1.0.0"}, {sym("platform"), "ruby"}, {sym("dependencies"), []any{[]any{"rack", ">= 2.0"}}}},
		hash{{sym("name"), "demo"}, {sym("number"), "1.0.0"}, {sym("platform"), "java"}, {sym("dependencies"), []any{}}},
		hash{{sym("name"), "demo"}, {sym("number"), "2.0.0.pre.1"}, {sym("platform"), "ruby"}, {sym("dependencies"), []any{}}},
	}
	if !reflect.DeepEqual(deps, want) {
		t.Fatalf("dependencies:\n%#v\nwant:\n%#v", deps, want)
	}
	if w := e.do("GET", "gems-hosted", "api/v1/dependencies", nil); w.Code != 200 || w.Body.String() != "\x04\x08[\x00" {
		t.Fatalf("no gems: %d %q", w.Code, w.Body)
	}
	if w := e.do("GET", "gems-hosted", "api/v1/dependencies?gems=a/b", nil); w.Code != 400 {
		t.Fatalf("bad gem name: %d", w.Code)
	}
}

// TestHostedRejections: what a push refuses, and the write policies.
func TestHostedRejections(t *testing.T) {
	e := newEnv(t)
	bad := func(name string, body []byte, want int, msg string) {
		t.Helper()
		w := e.push(t, "gems-hosted", body)
		if w.Code != want || !strings.Contains(w.Body.String(), msg) {
			t.Errorf("%s: %d %q, want %d %q", name, w.Code, w.Body, want, msg)
		}
	}
	bad("empty", nil, 400, "empty upload")
	bad("not a tar", []byte("hello"), 400, "not a tar")
	bad("no metadata", tarball(t, [][2]string{{"data.tar.gz", "x"}}), 400, "no metadata.gz")
	bad("bad metadata gzip", tarball(t, [][2]string{{"metadata.gz", "x"}}), 400, "metadata.gz")
	bad("not a spec", tarball(t, [][2]string{{"metadata.gz", string(gzipped(t, "- a\n- b\n"))}}), 400, "not a Gem::Specification")
	bad("bad name", tarball(t, [][2]string{{"metadata.gz", string(gzipped(t, gemspec("a/b", "1.0", "ruby", false)))}}), 400, "invalid gem name")
	bad("bad version", tarball(t, [][2]string{{"metadata.gz", string(gzipped(t, gemspec("demo", "1.0.0 beta", "ruby", false)))}}), 400, "invalid version")
	bad("bad platform", tarball(t, [][2]string{{"metadata.gz", string(gzipped(t, gemspec("demo", "1.0.0", "1-java", false)))}}), 400, "invalid platform")
	bad("ambiguous name", tarball(t, [][2]string{{"metadata.gz", string(gzipped(t, gemspec("foo-1-java", "2.0.0", "ruby", false)))}}), 400, "does not split back")
	// a hyphenated version is normalized as Gem::Version does
	if w := e.push(t, "gems-hosted", tarball(t, [][2]string{{"metadata.gz", string(gzipped(t, gemspec("demo", "1.0.0-rc1", "ruby", false)))}})); w.Code != 200 || !strings.Contains(w.Body.String(), "(1.0.0.pre.rc1)") {
		t.Fatalf("hyphen version: %d %s", w.Code, w.Body)
	}
	// write policies: allow_once 409, deny 400, allow needs Edit
	g := gem(t, "demo", "1.0.0", "", false)
	if w := e.push(t, "gems-hosted", g); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := e.push(t, "gems-hosted", g); w.Code != 409 {
		t.Fatalf("again: %d %s", w.Code, w.Body)
	}
	if w := e.push(t, "gems-ro", g); w.Code != 400 || !strings.Contains(w.Body.String(), "read-only") {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	if w := e.push(t, "gems-allow", g); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := e.push(t, "gems-allow", g); w.Code != 403 {
		t.Fatalf("allow without edit: %d %s", w.Code, w.Body)
	}
	e.now = e.now.Add(time.Minute)
	g2 := gem(t, "demo", "1.0.0", "", true) // the same version with a dependency now
	if w := e.doAs(auth.Principal{User: "admin", Admin: true}, "POST", "gems-allow", "api/v1/gems", g2); w.Code != 200 {
		t.Fatalf("allow with edit: %d %s", w.Code, w.Body)
	}
	if got := e.get(t, "gems-allow", "info/demo"); !strings.Contains(got, "1.0.0 rack:>= 2.0|checksum:"+sha256hex(g2)) {
		t.Fatalf("overwritten info: %q", got)
	}
	if w := e.do("GET", "gems-allow", "gems/demo-1.0.0.gem", nil); !bytes.Equal(w.Body.Bytes(), g2) {
		t.Fatal("overwritten gem")
	}
	// the versions file got a second line for the version, with the new md5
	if got := e.get(t, "gems-allow", "versions"); strings.Count(got, "demo 1.0.0 ") != 2 || !strings.HasSuffix(got, md5hex([]byte(e.get(t, "gems-allow", "info/demo")))+"\n") {
		t.Fatalf("versions after overwrite: %q", got)
	}
	// methods
	if w := e.do("PUT", "gems-hosted", "api/v1/gems", g); w.Code != 405 || w.Header().Get("Allow") != "POST" {
		t.Fatalf("PUT: %d %v", w.Code, w.Header())
	}
	if w := e.do("POST", "gems-proxy", "api/v1/gems", g); w.Code != 405 {
		t.Fatalf("proxy push: %d", w.Code)
	}
	if w := e.do("POST", "gems-proxy-first", "api/v1/gems", g); w.Code != 405 {
		t.Fatalf("group without writeMember: %d", w.Code)
	}
	if w := e.do("POST", "gems-hosted", "versions", nil); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST versions: %d", w.Code)
	}
	if w := e.do("GET", "gems-hosted", "nope", nil); w.Code != 404 {
		t.Fatalf("unknown path: %d", w.Code)
	}
}

// TestYank: gem yank's DELETE with a form body removes the version
// (through a group's writeMember too) and appends a "-" line.
func TestYank(t *testing.T) {
	e := newEnv(t)
	for _, g := range [][]byte{gem(t, "demo", "1.0.0", "", false), gem(t, "demo", "1.1.0", "", false), gem(t, "demo", "1.1.0", "java", false)} {
		if w := e.push(t, "gems-hosted", g); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	before := e.get(t, "gems-hosted", "versions")
	yank := func(repoName, form string) *httptest.ResponseRecorder {
		return e.do("DELETE", repoName, "api/v1/gems/yank", []byte(form), "Content-Type", "application/x-www-form-urlencoded")
	}
	if w := yank("gems-all", "gem_name=demo&version=1.0.0"); w.Code != 200 || w.Body.String() != "Successfully deleted gem: demo (1.0.0)\n" {
		t.Fatalf("yank: %d %s", w.Code, w.Body)
	}
	if w := yank("gems-hosted", "gem_name=demo&version=1.0.0"); w.Code != 404 {
		t.Fatalf("yank again: %d %s", w.Code, w.Body)
	}
	if w := yank("gems-hosted", "gem_name=demo&version=1.1.0&platform=java"); w.Code != 200 {
		t.Fatalf("yank platform: %d %s", w.Code, w.Body)
	}
	if w := yank("gems-hosted", "gem_name=demo"); w.Code != 400 {
		t.Fatalf("yank without version: %d", w.Code)
	}
	if w := yank("gems-ro", "gem_name=demo&version=1.0.0"); w.Code != 400 {
		t.Fatalf("yank read-only: %d", w.Code)
	}
	if w := e.do("DELETE", "gems-hosted", "api/v1/gems/yank?gem_name=demo&version=9.9.9", nil); w.Code != 404 {
		t.Fatalf("yank by query: %d", w.Code)
	}
	info := e.get(t, "gems-hosted", "info/demo")
	if !strings.HasPrefix(info, "---\n1.1.0 |checksum:") || strings.Count(info, "\n") != 2 {
		t.Fatalf("info after yank: %q", info)
	}
	after := e.get(t, "gems-hosted", "versions")
	infoAfterFirst := "---\n" + strings.Split(info, "\n")[1] + "\n1.1.0-java |checksum:" + sha256hex(gem(t, "demo", "1.1.0", "java", false)) + ",ruby:>= 2.7.0,rubygems:>= 0\n"
	if after != before+"demo -1.0.0 "+md5hex([]byte(infoAfterFirst))+"\ndemo -1.1.0-java "+md5hex([]byte(info))+"\n" {
		t.Fatalf("versions after yank:\n%s", after)
	}
	if w := e.do("GET", "gems-hosted", "gems/demo-1.0.0.gem", nil); w.Code != 404 {
		t.Fatalf("yanked gem: %d", w.Code)
	}
	if got := specsOf(t, loadGz(t, []byte(e.get(t, "gems-hosted", "specs.4.8.gz")))); got != "demo-1.1.0-ruby" {
		t.Fatalf("specs after yank: %q", got)
	}
	if w := yank("gems-hosted", "gem_name=demo&version=1.1.0"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := e.do("GET", "gems-hosted", "info/demo", nil); w.Code != 404 {
		t.Fatalf("info with nothing left: %d", w.Code)
	}
	if got := e.get(t, "gems-hosted", "names"); got != "---\n" {
		t.Fatalf("names with nothing left: %q", got)
	}
	if got := e.sink.String(); strings.Count(got, "delete") != 3 {
		t.Fatalf("events: %s", got)
	}
}

// TestProxy: the compact index, gems, gemspecs, specs and dependency
// answers of rubygems.org are cached (metadata for metadataMaxAge) and
// served stale when the remote is down.
func TestProxy(t *testing.T) {
	e := newEnv(t)
	w := e.do("GET", "gems-proxy", "versions", nil)
	if w.Code != 200 || w.Body.String() != upVersions || w.Header().Get("ETag") != `"`+md5hex([]byte(upVersions))+`"` {
		t.Fatalf("versions: %d %q %v", w.Code, w.Body, w.Header())
	}
	n := len(upVersions)
	if w := e.do("GET", "gems-proxy", "versions", nil, "Range", fmt.Sprintf("bytes=%d-", n-5)); w.Code != 206 || w.Body.String() != upVersions[n-5:] {
		t.Fatalf("range: %d %q", w.Code, w.Body)
	}
	if got := e.get(t, "gems-proxy", "names"); got != "---\ndemo\nrack\n" {
		t.Fatalf("names: %q", got)
	}
	if got := e.get(t, "gems-proxy", "info/rack"); !strings.HasPrefix(got, "---\n2.2.8 ") {
		t.Fatalf("info: %q", got)
	}
	if w := e.do("GET", "gems-proxy", "info/nope", nil); w.Code != 404 {
		t.Fatalf("missing info: %d", w.Code)
	}
	for i := 0; i < 2; i++ {
		if w := e.do("GET", "gems-proxy", "gems/rack-3.0.0.gem", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), e.gems["rack-3.0.0.gem"]) {
			t.Fatalf("gem: %d", w.Code)
		}
	}
	if e.hits.Load() != 1 {
		t.Fatalf("gem fetched %d times", e.hits.Load())
	}
	if got := e.get(t, "gems-proxy", "quick/Marshal.4.8/rack-3.0.0.gemspec.rz"); got != "rz-rack" {
		t.Fatalf("gemspec: %q", got)
	}
	if got := specsOf(t, loadGz(t, []byte(e.get(t, "gems-proxy", "specs.4.8.gz")))); got != "rack-3.0.0-ruby" {
		t.Fatalf("specs: %q", got)
	}
	w = e.do("GET", "gems-proxy", "api/v1/dependencies?gems=rack,rack,demo", nil)
	if v, err := marshalLoad(w.Body.Bytes()); w.Code != 200 || err != nil || len(v.([]any)) != 2 || *e.deps.Load() != "demo,rack" {
		t.Fatalf("dependencies: %d %v %q", w.Code, err, *e.deps.Load())
	}
	// cached for metadataMaxAge; stale afterwards when the remote is down
	e.down.Store(true)
	if w := e.do("GET", "gems-proxy", "versions", nil); w.Code != 200 || w.Header().Get("Warning") != "" {
		t.Fatalf("fresh: %d %v", w.Code, w.Header())
	}
	e.now = e.now.Add(2 * time.Hour)
	if w := e.do("GET", "gems-proxy", "versions", nil); w.Code != 200 || w.Header().Get("Warning") == "" || w.Body.String() != upVersions {
		t.Fatalf("stale: %d %v", w.Code, w.Header())
	}
	if w := e.do("GET", "gems-proxy", "info/other", nil); w.Code != 502 {
		t.Fatalf("down and not cached: %d", w.Code)
	}
}

// TestGroup: versions and names are merged with the first member owning a
// gem it has; info, gems, gemspecs and dependencies come from the owner.
func TestGroup(t *testing.T) {
	e := newEnv(t)
	demo := gem(t, "demo", "1.0.0", "", true)
	if w := e.push(t, "gems-hosted", demo); w.Code != 200 {
		t.Fatal(w.Code)
	}
	hostedInfo := e.get(t, "gems-hosted", "info/demo")
	// versions: the hosted line, then the proxy's lines except demo's
	want := "created_at: 2026-10-27T12:01:00Z\n---\ndemo 1.0.0 " + md5hex([]byte(hostedInfo)) + "\nrack 2.2.8,3.0.0 1111\n"
	w := e.do("GET", "gems-all", "versions", nil)
	if w.Code != 200 || w.Body.String() != want || w.Header().Get("ETag") != `"`+md5hex([]byte(want))+`"` {
		t.Fatalf("versions: %d\n%s\nwant:\n%s", w.Code, w.Body, want)
	}
	if w := e.do("GET", "gems-all", "versions", nil, "Range", "bytes=10-"); w.Code != 206 || w.Body.String() != want[10:] {
		t.Fatalf("range: %d %q", w.Code, w.Body)
	}
	// proxy first: its demo hides the hosted one
	if got := e.get(t, "gems-proxy-first", "versions"); got != "created_at: 2026-10-27T12:01:00Z\n---\nrack 2.2.8,3.0.0 1111\ndemo 9.0.0 2222\n" {
		t.Fatalf("proxy first: %q", got)
	}
	if got := e.get(t, "gems-proxy-first", "info/demo"); got != "---\n9.0.0 |checksum:cc\n" {
		t.Fatalf("proxy-first info: %q", got)
	}
	if got := e.get(t, "gems-all", "names"); got != "---\ndemo\nrack\n" {
		t.Fatalf("names: %q", got)
	}
	if got := e.get(t, "gems-all", "info/demo"); got != hostedInfo {
		t.Fatalf("info: %q", got)
	}
	if got := e.get(t, "gems-all", "info/rack"); !strings.HasPrefix(got, "---\n2.2.8 ") {
		t.Fatalf("proxy info: %q", got)
	}
	if w := e.do("GET", "gems-all", "info/nope", nil); w.Code != 404 {
		t.Fatalf("missing info: %d", w.Code)
	}
	if w := e.do("GET", "gems-all", "gems/demo-1.0.0.gem", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), demo) {
		t.Fatalf("hosted gem: %d", w.Code)
	}
	if w := e.do("GET", "gems-all", "gems/demo-9.0.0.gem", nil); w.Code != 404 { // the owner has no such version
		t.Fatalf("owner rule: %d", w.Code)
	}
	if w := e.do("GET", "gems-all", "gems/rack-3.0.0.gem", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), e.gems["rack-3.0.0.gem"]) {
		t.Fatalf("proxy gem: %d", w.Code)
	}
	if got := e.get(t, "gems-all", "quick/Marshal.4.8/rack-3.0.0.gemspec.rz"); got != "rz-rack" {
		t.Fatalf("proxy gemspec: %q", got)
	}
	if w := e.do("GET", "gems-all", "quick/Marshal.4.8/demo-1.0.0.gemspec.rz", nil); w.Code != 200 {
		t.Fatalf("hosted gemspec: %d", w.Code)
	}
	if got := specsOf(t, loadGz(t, []byte(e.get(t, "gems-all", "specs.4.8.gz")))); got != "demo-1.0.0-ruby" { // the first member's, not merged
		t.Fatalf("specs: %q", got)
	}
	w = e.do("GET", "gems-all", "api/v1/dependencies?gems=demo,rack", nil)
	v, err := marshalLoad(w.Body.Bytes())
	if w.Code != 200 || err != nil {
		t.Fatalf("dependencies: %d %v", w.Code, err)
	}
	wantDeps := []any{
		hash{{sym("name"), "demo"}, {sym("number"), "1.0.0"}, {sym("platform"), "ruby"}, {sym("dependencies"), []any{[]any{"rack", ">= 2.0"}}}},
		hash{{sym("name"), "rack"}, {sym("number"), "3.0.0"}, {sym("platform"), "ruby"}, {sym("dependencies"), []any{}}},
	}
	if !reflect.DeepEqual(v, wantDeps) || *e.deps.Load() != "rack" {
		t.Fatalf("dependencies: %#v (asked %q)", v, *e.deps.Load())
	}
	// the merge is cached per group for a minute
	if w := e.do("POST", "gems-hosted", "api/v1/gems", gem(t, "other", "0.1.0", "", false)); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if got := e.get(t, "gems-all", "versions"); got != want {
		t.Fatalf("cached: %q", got)
	}
	e.now = e.now.Add(2 * time.Minute)
	if got := e.get(t, "gems-all", "versions"); !strings.Contains(got, "\nother 0.1.0 ") {
		t.Fatalf("after the cache: %q", got)
	}
	// a failing member is a 502, never skipped
	e.down.Store(true)
	e.now = e.now.Add(2 * time.Hour)
	if w := e.do("GET", "gems-all", "info/zzz", nil); w.Code != 502 {
		t.Fatalf("member down: %d", w.Code)
	}
}

// TestDeleteComponent: the UI's and cleanup's delete, and the component
// mapping of gem paths.
func TestDeleteComponent(t *testing.T) {
	e := newEnv(t)
	for _, g := range [][]byte{gem(t, "demo", "1.0.0", "", false), gem(t, "demo", "1.0.0", "java", false)} {
		if w := e.push(t, "gems-hosted", g); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	e.sink.Reset()
	ctx := context.Background()
	rp := e.repos["gems-hosted"]
	res, err := e.h.DeleteComponent(ctx, rp, component.Component{Repo: "gems-hosted", Format: "rubygems", Name: "demo", Version: "1.0.0-java"}, "admin")
	if err != nil || res.Assets != 1 {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0].Kind != "delete" || got[0].Actor != "admin" || got[0].Subject != "demo@1.0.0-java" || got[0].Detail != "assets=1" {
		t.Fatalf("events: %v", got)
	}
	if w := e.do("GET", "gems-hosted", "gems/demo-1.0.0-java.gem", nil); w.Code != 404 {
		t.Fatalf("deleted: %d", w.Code)
	}
	if got := e.get(t, "gems-hosted", "info/demo"); strings.Contains(got, "java") || !strings.Contains(got, "1.0.0 |") {
		t.Fatalf("info: %q", got)
	}
	if got := e.get(t, "gems-hosted", "versions"); !strings.HasSuffix(got, "demo -1.0.0-java "+md5hex([]byte(e.get(t, "gems-hosted", "info/demo")))+"\n") {
		t.Fatalf("versions: %q", got)
	}
	if res, err := e.h.DeleteComponent(ctx, rp, component.Component{Format: "rubygems", Name: "demo", Version: "1.0.0-java"}, "admin"); err != nil || res.Assets != 0 || len(e.sink.Events()) != 1 {
		t.Fatalf("again: %+v %v", res, err)
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["gems-proxy"], component.Component{Format: "rubygems", Name: "demo", Version: "1.0.0"}, "admin"); err == nil {
		t.Fatal("proxy: want ErrNotSupported")
	}
	if _, err := e.h.DeleteComponent(ctx, rp, component.Component{Format: "rubygems", Name: "demo"}, "admin"); err == nil {
		t.Fatal("incomplete: want an error")
	}
	// an evaluated component that changed since is skipped
	a, _ := e.h.Store.Get(ctx, "gems-hosted", "gems/demo-1.0.0.gem")
	c := component.Component{Format: "rubygems", Name: "demo", Version: "1.0.0", Evaluated: &component.Evaluated{Paths: map[string]bool{a.Path: true}, MaxCreated: a.CreatedAt.Add(-time.Hour), MaxModified: a.LastModified.Add(-time.Hour)}}
	if _, err := e.h.DeleteComponent(ctx, rp, c, "system"); err == nil {
		t.Fatal("changed component: want ErrChanged")
	}
	if _, name, version, path := component.PackageComponent("rubygems", "demo", "1.0.0-java"); name != "demo" || version != "1.0.0-java" || path != "gems/demo-1.0.0-java.gem" {
		t.Fatalf("PackageComponent: %s %s %s", name, version, path)
	}
	for p, want := range map[string]string{
		"gems/demo-1.0.0.gem": "demo@1.0.0", "gems/demo-1.0.0-java.gem": "demo@1.0.0-java", "gems/rack-test-2.1.0.gem": "rack-test@2.1.0",
		"gems/ruby-2d-0.1.0.gem": "ruby-2d@0.1.0", "gems/nokogiri-1.15.0-x86_64-linux.gem": "nokogiri@1.15.0-x86_64-linux", "gems/a-1.0.0.pre.1.gem": "a@1.0.0.pre.1",
		"versions": "", "info/demo": "", "quick/Marshal.4.8/demo-1.0.0.gemspec.rz": "", "gems/demo.gem": "", "gems/x/demo-1.0.0.gem": "", "specs.4.8.gz": "",
	} {
		_, name, version, ok := component.DeriveAsset("rubygems", p)
		got := ""
		if ok {
			got = name + "@" + version
		}
		if got != want {
			t.Errorf("DeriveAsset(%s) = %q, want %q", p, got, want)
		}
	}
}
