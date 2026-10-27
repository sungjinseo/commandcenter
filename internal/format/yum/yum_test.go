package yum

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
	"commandcenter/internal/resolve"
)

// tv is one header entry for the test writer: a string, []string (as a
// STRING_ARRAY, or I18NSTRING when i18n), []uint16, []int32 or []int64.
type tv struct {
	tag  int32
	val  any
	i18n bool
}

// hdr writes a header structure: the entries' data in order, each
// integer type aligned as rpm aligns it.
func hdr(entries []tv) []byte {
	var index, data bytes.Buffer
	align := func(n int) {
		for data.Len()%n != 0 {
			data.WriteByte(0)
		}
	}
	for _, e := range entries {
		var typ, count int32
		off := data.Len()
		switch v := e.val.(type) {
		case string:
			typ, count = typeString, 1
			data.WriteString(v)
			data.WriteByte(0)
		case []string:
			typ, count = typeArray, int32(len(v))
			if e.i18n {
				typ = typeI18N
			}
			for _, s := range v {
				data.WriteString(s)
				data.WriteByte(0)
			}
		case []uint16:
			align(2)
			off = data.Len()
			typ, count = typeInt16, int32(len(v))
			binary.Write(&data, binary.BigEndian, v)
		case []int32:
			align(4)
			off = data.Len()
			typ, count = typeInt32, int32(len(v))
			binary.Write(&data, binary.BigEndian, v)
		case []int64:
			align(8)
			off = data.Len()
			typ, count = typeInt64, int32(len(v))
			binary.Write(&data, binary.BigEndian, v)
		default:
			panic(fmt.Sprintf("tag %d: %T", e.tag, e.val))
		}
		binary.Write(&index, binary.BigEndian, []int32{e.tag, typ, int32(off), count})
	}
	var out bytes.Buffer
	out.Write(headerMagic[:])
	out.Write([]byte{0, 0, 0, 0})
	binary.Write(&out, binary.BigEndian, []int32{int32(len(entries)), int32(data.Len())})
	out.Write(index.Bytes())
	out.Write(data.Bytes())
	return out.Bytes()
}

// rpmFile assembles an RPM: lead, signature header, padding, main header,
// payload.
func rpmFile(sig, main []tv, payload string) []byte {
	var out bytes.Buffer
	lead := make([]byte, leadSize)
	copy(lead, []byte{0xed, 0xab, 0xee, 0xdb, 3, 0})
	out.Write(lead)
	out.Write(hdr(sig))
	for out.Len()%8 != 0 {
		out.WriteByte(0)
	}
	out.Write(hdr(main))
	out.WriteString(payload)
	return out.Bytes()
}

// demoTags are the header of demo-1.0-1.el9.x86_64: every field the
// repodata prints, i18n summary and description, dependencies of each
// kind, files of each type and a changelog.
func demoTags() []tv {
	return []tv{
		{tag: tagName, val: "demo"}, {tag: tagVersion, val: "1.0"}, {tag: tagRelease, val: "1.el9"}, {tag: tagArch, val: "x86_64"},
		{tag: tagSummary, val: []string{"A demo package", "Ein Demo-Paket"}, i18n: true},
		{tag: tagDescription, val: []string{"Demo does <nothing> & \"less\".", "Demo tut nichts."}, i18n: true},
		{tag: tagBuildTime, val: []int32{1700000000}}, {tag: tagBuildHost, val: "build.example.org"},
		{tag: tagSize, val: []int32{4096}}, {tag: tagLicense, val: "MIT"}, {tag: tagGroup, val: "Applications/Text"},
		{tag: tagURL, val: "https://example.org/demo"}, {tag: tagPackager, val: "QA <qa@example.org>"}, {tag: tagVendor, val: "Example"},
		{tag: tagSourceRPM, val: "demo-1.0-1.el9.src.rpm"},
		{tag: tagProvideName, val: []string{"demo", "demo(x86-64)", "config(demo)"}},
		{tag: tagProvideFlags, val: []int32{senseEqual, senseEqual, senseEqual}},
		{tag: tagProvideVer, val: []string{"1.0-1.el9", "1.0-1.el9", "1.0-1.el9"}},
		{tag: tagRequireName, val: []string{"/bin/sh", "libc.so.6()(64bit)", "rpmlib(PayloadIsZstd)", "bar", "baz", "qux"}},
		{tag: tagRequireFlags, val: []int32{0x200, 0, senseLess | senseEqual, senseGreater | senseEqual, senseLess, senseGreater | 0x40}},
		{tag: tagRequireVer, val: []string{"", "", "5.4.18-1", "2.0", "1:3.0", "0.1-1"}},
		{tag: tagConflictName, val: []string{"olddemo"}}, {tag: tagConflictFlags, val: []int32{senseLess}}, {tag: tagConflictVer, val: []string{"1.0"}},
		{tag: tagObsoleteName, val: []string{"demo-compat"}}, {tag: tagObsoleteFlags, val: []int32{0}}, {tag: tagObsoleteVer, val: []string{""}},
		{tag: tagDirnames, val: []string{"/usr/bin/", "/etc/demo/", "/usr/share/doc/demo/", "/var/cache/"}},
		{tag: tagBasenames, val: []string{"demo", "demo.conf", "README", "demo"}},
		{tag: tagDirIndexes, val: []int32{0, 1, 2, 3}},
		{tag: tagFileModes, val: []uint16{0o100755, 0o100644, 0o100644, 0o040755}},
		{tag: tagFileFlags, val: []int32{0, 1, fileFlagGhost, 0}},
		{tag: tagChangeTime, val: []int32{1699000000, 1698000000}},
		{tag: tagChangeName, val: []string{"QA <qa@example.org> - 1.0-1", "QA <qa@example.org> - 0.9-1"}},
		{tag: tagChangeText, val: []string{"- Release 1.0", "- First build"}},
	}
}

func demoRPM() []byte {
	return rpmFile([]tv{{tag: 1000, val: []int32{9999}}, {tag: sigTagPayload, val: []int32{8192}}}, demoTags(), "PAYLOAD-NOT-A-REAL-CPIO")
}

// nvra builds a minimal package of another name, version, release and arch.
func nvra(name, ver, rel, arch string) []byte {
	return rpmFile([]tv{{tag: 1000, val: []int32{1}}}, []tv{
		{tag: tagName, val: name}, {tag: tagVersion, val: ver}, {tag: tagRelease, val: rel}, {tag: tagArch, val: arch},
		{tag: tagSummary, val: name + " summary"}, {tag: tagDescription, val: name + " description"},
		{tag: tagBuildTime, val: []int32{1700000001}}, {tag: tagSize, val: []int32{1}},
	}, name)
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestParse(t *testing.T) {
	demo := demoRPM()
	p, err := Parse(bytes.NewReader(demo), int64(len(demo)))
	if err != nil {
		t.Fatal(err)
	}
	// the main header follows the signature header, padded to 8 bytes
	sigEnd := leadSize + len(hdr([]tv{{tag: 1000, val: []int32{9999}}, {tag: sigTagPayload, val: []int32{8192}}}))
	start := (sigEnd + 7) &^ 7
	end := start + len(hdr(demoTags()))
	switch {
	case p.NVRA() != "demo-1.0-1.el9.x86_64.rpm", p.Epoch != "0":
		t.Fatalf("nvra: %+v", p)
	case p.Summary != "A demo package", p.Description != "Demo does <nothing> & \"less\".": // i18n: the first string
		t.Fatalf("i18n: %q %q", p.Summary, p.Description)
	case p.HeaderStart != int64(start), p.HeaderEnd != int64(end), p.SizePackage != int64(len(demo)), p.SizeInstalled != 4096, p.SizeArchive != 8192:
		t.Fatalf("ranges and sizes: %+v (want %d-%d)", p, start, end)
	case len(p.Requires) != 5, p.Requires[0] != (Dep{Name: "/bin/sh", Pre: true}), p.Requires[1] != (Dep{Name: "libc.so.6()(64bit)"}):
		t.Fatalf("requires: %+v", p.Requires) // rpmlib() dropped, scriptlet requires are pre
	case p.Requires[2] != (Dep{Name: "bar", Flags: "GE", Epoch: "0", Ver: "2.0"}), p.Requires[3] != (Dep{Name: "baz", Flags: "LT", Epoch: "1", Ver: "3.0"}), p.Requires[4] != (Dep{Name: "qux", Flags: "GT", Epoch: "0", Ver: "0.1", Rel: "1", Pre: true}):
		t.Fatalf("requires: %+v", p.Requires)
	case len(p.Files) != 4, p.Files[2] != (File{Path: "/usr/share/doc/demo/README", Type: "ghost"}), p.Files[3] != (File{Path: "/var/cache/demo", Type: "dir"}):
		t.Fatalf("files: %+v", p.Files)
	case len(p.Changelog) != 2, p.Changelog[1] != (Changelog{Author: "QA <qa@example.org> - 0.9-1", Date: 1698000000, Text: "- First build"}):
		t.Fatalf("changelog: %+v", p.Changelog)
	}
	// optional tags missing, epoch and long sizes present, a source package
	src := rpmFile([]tv{{tag: sigTagLongPayload, val: []int64{1 << 33}}}, []tv{
		{tag: tagName, val: "tiny"}, {tag: tagVersion, val: "2"}, {tag: tagRelease, val: "3"}, {tag: tagArch, val: "x86_64"},
		{tag: tagEpoch, val: []int32{2}}, {tag: tagLongSize, val: []int64{1 << 32}}, {tag: tagSourcePackage, val: []int32{1}},
	}, "")
	p, err = Parse(bytes.NewReader(src), int64(len(src)))
	if err != nil || p.NVRA() != "tiny-2-3.src.rpm" || p.Epoch != "2" || p.SizeInstalled != 1<<32 || p.SizeArchive != 1<<33 || p.Summary != "" || len(p.Files) != 0 || p.Provides != nil {
		t.Fatalf("minimal: %+v %v", p, err)
	}
	bad := map[string][]byte{
		"not an rpm":  []byte("hello, this is not an rpm file at all......................................................................"),
		"short":       demo[:200],
		"no name":     rpmFile([]tv{{tag: 1000, val: []int32{1}}}, []tv{{tag: tagVersion, val: "1"}, {tag: tagRelease, val: "1"}, {tag: tagArch, val: "noarch"}}, ""),
		"dash in ver": rpmFile([]tv{{tag: 1000, val: []int32{1}}}, []tv{{tag: tagName, val: "a"}, {tag: tagVersion, val: "1-2"}, {tag: tagRelease, val: "1"}, {tag: tagArch, val: "noarch"}}, ""),
		"no dir":      rpmFile([]tv{{tag: 1000, val: []int32{1}}}, []tv{{tag: tagName, val: "a"}, {tag: tagVersion, val: "1"}, {tag: tagRelease, val: "1"}, {tag: tagArch, val: "noarch"}, {tag: tagBasenames, val: []string{"f"}}, {tag: tagDirIndexes, val: []int32{3}}}, ""),
	}
	for name, b := range bad {
		if _, err := Parse(bytes.NewReader(b), int64(len(b))); repo.StatusOf(err) != 400 {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a header claiming more data than there is, or too many entries, is refused, not read
	huge := demo[:leadSize+16]
	huge = append(huge[:leadSize+8:leadSize+8], 0, 1, 0, 0, 0x7f, 0xff, 0xff, 0xff)
	if _, err := Parse(bytes.NewReader(huge), int64(len(huge))); repo.StatusOf(err) != 400 || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("huge: %v", err)
	}
}

type env struct {
	h     *Handler
	repos map[string]*repo.Repository
	sink  *repotest.Sink
	db    *meta.DB
	now   time.Time
	hits  atomic.Int32 // upstream requests
	up    *httptest.Server
}

// upstream is a mirror: el9/repodata/repomd.xml and one package.
func (e *env) upstream(w http.ResponseWriter, r *http.Request) {
	e.hits.Add(1)
	switch r.URL.Path {
	case "/el9/repodata/repomd.xml":
		w.Header().Set("Content-Type", "text/xml")
		io.WriteString(w, "<repomd>upstream</repomd>")
	case "/el9/Packages/u/up-1.0-1.el9.noarch.rpm":
		w.Header().Set("Content-Type", "application/x-rpm")
		w.Write(nvra("up", "1.0", "1.el9", "noarch"))
	case "/el9/":
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html>Index of /el9/</html>")
	default:
		http.NotFound(w, r)
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	e.up = httptest.NewServer(http.HandlerFunc(e.upstream))
	t.Cleanup(e.up.Close)
	dir := t.TempDir()
	b, _ := blob.Open(dir)
	db, err := meta.Open(metatest.Target(t, filepath.Join(dir, "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.db, e.sink = db, &repotest.Sink{}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: func() time.Time { return e.now }, Events: e.sink}
	cfg := &config.Config{AnonymousRead: true, Repositories: []config.Repository{
		{Name: "yum-hosted", Format: "yum", Type: "hosted", WritePolicy: "allow_once"},
		{Name: "yum-allow", Format: "yum", Type: "hosted", WritePolicy: "allow"},
		{Name: "yum-ro", Format: "yum", Type: "hosted", WritePolicy: "deny"},
		{Name: "yum-deep", Format: "yum", Type: "hosted", WritePolicy: "allow_once", Depth: 1},
		{Name: "yum-proxy", Format: "yum", Type: "proxy", Remote: e.up.URL + "/",
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "yum-all", Format: "yum", Type: "group", Members: []string{"yum-hosted", "yum-proxy"}, WriteMember: "yum-hosted"},
	}}
	if err := config.ValidateRepositories(cfg.Repositories); err != nil {
		t.Fatal(err)
	}
	f := proxy.New(store, e.up.Client(), 4)
	f.Now = func() time.Time { return e.now }
	e.h = New(store, &resolve.Resolver{Store: store, Fetcher: f}, auth.New(cfg, db))
	e.h.Now = func() time.Time { return e.now }
	e.repos = repo.Build(cfg)
	return e
}

func (e *env) do(method, repoName, path string, body []byte) *httptest.ResponseRecorder {
	return e.doAs(auth.Principal{}, method, repoName, path, body)
}

func (e *env) doAs(p auth.Principal, method, repoName, path string, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repoName+"/"+path, bytes.NewReader(body))
	r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

// repodata reads <dir>repodata/repomd.xml of repoName and returns the
// data files it names (by type) decompressed, checking every checksum and
// size against the stored files.
func (e *env) repodata(t *testing.T, repoName, dir string) (repomd string, data map[string]string) {
	t.Helper()
	w := e.do("GET", repoName, dir+"repodata/repomd.xml", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != ctXML {
		t.Fatalf("repomd.xml: %d %v %s", w.Code, w.Header(), w.Body)
	}
	repomd = w.Body.String()
	data = map[string]string{}
	re := regexp.MustCompile(`(?s)<data type="(\w+)">\s*<checksum type="sha256">(\w+)</checksum>\s*<open-checksum type="sha256">(\w+)</open-checksum>\s*<location href="([^"]+)"/>\s*<timestamp>(\d+)</timestamp>\s*<size>(\d+)</size>\s*<open-size>(\d+)</open-size>`)
	for _, m := range re.FindAllStringSubmatch(repomd, -1) {
		typ, sha, openSha, href, ts, size, openSize := m[1], m[2], m[3], m[4], m[5], m[6], m[7]
		w := e.do("GET", repoName, dir+href, nil)
		if w.Code != 200 || w.Header().Get("Content-Type") != ctGzip {
			t.Fatalf("%s: %d %v", href, w.Code, w.Header())
		}
		gz, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		open, err := io.ReadAll(gz)
		if err != nil {
			t.Fatal(err)
		}
		if href != "repodata/"+sha+"-"+typ+".xml.gz" || sum(w.Body.Bytes()) != sha || sum(open) != openSha || size != fmt.Sprint(w.Body.Len()) || openSize != fmt.Sprint(len(open)) || ts != fmt.Sprint(e.now.Unix()) {
			t.Fatalf("%s: href %s, sha %s/%s, size %s/%d open %s/%d ts %s", typ, href, sha, sum(w.Body.Bytes()), size, w.Body.Len(), openSize, len(open), ts)
		}
		data[typ] = string(open)
	}
	if len(data) != 3 {
		t.Fatalf("repomd.xml lists %d data files:\n%s", len(data), repomd)
	}
	return repomd, data
}

func TestHostedUploadAndRepodata(t *testing.T) {
	e := newEnv(t)
	if w := e.do("GET", "yum-hosted", "repodata/repomd.xml", nil); w.Code != 404 {
		t.Fatalf("before any upload: %d", w.Code) // nothing to list yet
	}
	demo := demoRPM()
	if w := e.do("PUT", "yum-hosted", "Packages/d/demo-1.0-1.el9.x86_64.rpm", demo); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0] != (repotest.Event{Kind: "upload", Repo: "yum-hosted", Subject: "Packages/d/demo@1.0-1.el9.x86_64"}) {
		t.Fatalf("events: %v", got)
	}
	w := e.do("GET", "yum-hosted", "Packages/d/demo-1.0-1.el9.x86_64.rpm", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), demo) || w.Header().Get("Content-Type") != ctRPM {
		t.Fatalf("GET rpm: %d %v", w.Code, w.Header())
	}
	p, _ := Parse(bytes.NewReader(demo), int64(len(demo)))
	_, data := e.repodata(t, "yum-hosted", "")
	wantPrimary := `<?xml version="1.0" encoding="UTF-8"?>
<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="1">
<package type="rpm">
  <name>demo</name>
  <arch>x86_64</arch>
  <version epoch="0" ver="1.0" rel="1.el9"/>
  <checksum type="sha256" pkgid="YES">` + sum(demo) + `</checksum>
  <summary>A demo package</summary>
  <description>Demo does &lt;nothing&gt; &amp; &#34;less&#34;.</description>
  <packager>QA &lt;qa@example.org&gt;</packager>
  <url>https://example.org/demo</url>
  <time file="` + fmt.Sprint(e.now.Unix()) + `" build="1700000000"/>
  <size package="` + fmt.Sprint(len(demo)) + `" installed="4096" archive="8192"/>
  <location href="Packages/d/demo-1.0-1.el9.x86_64.rpm"/>
  <format>
    <rpm:license>MIT</rpm:license>
    <rpm:vendor>Example</rpm:vendor>
    <rpm:group>Applications/Text</rpm:group>
    <rpm:buildhost>build.example.org</rpm:buildhost>
    <rpm:sourcerpm>demo-1.0-1.el9.src.rpm</rpm:sourcerpm>
    <rpm:header-range start="` + fmt.Sprint(p.HeaderStart) + `" end="` + fmt.Sprint(p.HeaderEnd) + `"/>
    <rpm:provides>
      <rpm:entry name="demo" flags="EQ" epoch="0" ver="1.0" rel="1.el9"/>
      <rpm:entry name="demo(x86-64)" flags="EQ" epoch="0" ver="1.0" rel="1.el9"/>
      <rpm:entry name="config(demo)" flags="EQ" epoch="0" ver="1.0" rel="1.el9"/>
    </rpm:provides>
    <rpm:requires>
      <rpm:entry name="/bin/sh" pre="1"/>
      <rpm:entry name="libc.so.6()(64bit)"/>
      <rpm:entry name="bar" flags="GE" epoch="0" ver="2.0"/>
      <rpm:entry name="baz" flags="LT" epoch="1" ver="3.0"/>
      <rpm:entry name="qux" flags="GT" epoch="0" ver="0.1" rel="1" pre="1"/>
    </rpm:requires>
    <rpm:conflicts>
      <rpm:entry name="olddemo" flags="LT" epoch="0" ver="1.0"/>
    </rpm:conflicts>
    <rpm:obsoletes>
      <rpm:entry name="demo-compat"/>
    </rpm:obsoletes>
    <file>/usr/bin/demo</file>
    <file>/etc/demo/demo.conf</file>
  </format>
</package>
</metadata>
`
	wantFilelists := `<?xml version="1.0" encoding="UTF-8"?>
<filelists xmlns="http://linux.duke.edu/metadata/filelists" packages="1">
<package pkgid="` + sum(demo) + `" name="demo" arch="x86_64">
  <version epoch="0" ver="1.0" rel="1.el9"/>
  <file>/usr/bin/demo</file>
  <file>/etc/demo/demo.conf</file>
  <file type="ghost">/usr/share/doc/demo/README</file>
  <file type="dir">/var/cache/demo</file>
</package>
</filelists>
`
	wantOther := `<?xml version="1.0" encoding="UTF-8"?>
<otherdata xmlns="http://linux.duke.edu/metadata/other" packages="1">
<package pkgid="` + sum(demo) + `" name="demo" arch="x86_64">
  <version epoch="0" ver="1.0" rel="1.el9"/>
  <changelog author="QA &lt;qa@example.org&gt; - 1.0-1" date="1699000000">- Release 1.0</changelog>
  <changelog author="QA &lt;qa@example.org&gt; - 0.9-1" date="1698000000">- First build</changelog>
</package>
</otherdata>
`
	for typ, want := range map[string]string{"primary": wantPrimary, "filelists": wantFilelists, "other": wantOther} {
		if data[typ] != want {
			t.Errorf("%s.xml:\n%s\nwant:\n%s", typ, data[typ], want)
		}
	}
	// allow_once: the same package again is a 409, whatever its content
	if w := e.do("PUT", "yum-hosted", "Packages/d/demo-1.0-1.el9.x86_64.rpm", demo); w.Code != 409 {
		t.Fatalf("republish: %d %s", w.Code, w.Body)
	}
	// the file name must be the package's NVRA; only .rpm outside repodata/
	for _, c := range []struct {
		path string
		body []byte
		want int
	}{
		{"demo-1.0-1.el9.x86_64.rpm", nvra("other", "1.0", "1.el9", "x86_64"), 400},
		{"demo.rpm", demo, 400},
		{"repodata/x-1.0-1.noarch.rpm", nvra("x", "1.0", "1", "noarch"), 400},
		{"notes.txt", []byte("x"), 400},
		{"empty-1.0-1.noarch.rpm", nil, 400},
		{"../x-1.0-1.noarch.rpm", nvra("x", "1.0", "1", "noarch"), 400},
		{"junk-1.0-1.noarch.rpm", []byte("not an rpm at all, just some bytes that go on for a while and a while longer"), 400},
	} {
		if w := e.do("PUT", "yum-hosted", c.path, c.body); w.Code != c.want {
			t.Errorf("PUT %s: %d %s", c.path, w.Code, w.Body)
		}
	}
	// more packages: a second arch of the same version is its own row,
	// and the repodata lists all, name then version order
	for _, b := range [][]byte{nvra("demo", "1.0", "1.el9", "aarch64"), nvra("alpha", "2", "1", "noarch")} {
		p, _ := Parse(bytes.NewReader(b), int64(len(b)))
		if w := e.do("PUT", "yum-hosted", p.NVRA(), b); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", p.NVRA(), w.Code, w.Body)
		}
	}
	_, data = e.repodata(t, "yum-hosted", "")
	if got := regexp.MustCompile(`<location href="([^"]+)"/>`).FindAllStringSubmatch(data["primary"], -1); len(got) != 3 || got[0][1] != "Packages/d/demo-1.0-1.el9.x86_64.rpm" || got[1][1] != "alpha-2-1.noarch.rpm" || got[2][1] != "demo-1.0-1.el9.aarch64.rpm" {
		t.Fatalf("locations: %v", got)
	}
	// the old data files are gone: repodata/ holds repomd.xml and three files
	assets, err := e.db.ListAssets(context.Background(), "yum-hosted", "repodata/")
	if err != nil || len(assets) != 4 {
		t.Fatalf("repodata assets: %d %v", len(assets), err)
	}
	if w := e.do("DELETE", "yum-hosted", "alpha-2-1.noarch.rpm", nil); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD, PUT" {
		t.Fatalf("DELETE: %d %q", w.Code, w.Header().Get("Allow"))
	}
	if w := e.do("PUT", "yum-proxy", "x-1.0-1.noarch.rpm", nvra("x", "1.0", "1", "noarch")); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("PUT proxy: %d %q", w.Code, w.Header().Get("Allow"))
	}
}

func TestWritePolicy(t *testing.T) {
	e := newEnv(t)
	demo := demoRPM()
	if w := e.do("PUT", "yum-ro", "demo-1.0-1.el9.x86_64.rpm", demo); w.Code != 400 {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	a, ps := repotest.Principals(t, e.db, "yum-allow", map[string][]string{
		"adder":  {meta.ActionAdd},
		"editor": {meta.ActionAdd, meta.ActionEdit},
	})
	e.h.Auth = a
	if w := e.doAs(ps["adder"], "PUT", "yum-allow", "demo-1.0-1.el9.x86_64.rpm", demo); w.Code != 201 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	if w := e.doAs(ps["adder"], "PUT", "yum-allow", "demo-1.0-1.el9.x86_64.rpm", demo); w.Code != 403 {
		t.Fatalf("overwrite without edit: %d %s", w.Code, w.Body)
	}
	// the replacement has another summary: the repodata follows it
	again := rpmFile([]tv{{tag: 1000, val: []int32{2}}}, append(demoTags()[:4], tv{tag: tagSummary, val: "Rebuilt"}), "payload 2")
	if w := e.doAs(ps["editor"], "PUT", "yum-allow", "demo-1.0-1.el9.x86_64.rpm", again); w.Code != 201 {
		t.Fatalf("overwrite with edit: %d %s", w.Code, w.Body)
	}
	_, data := e.repodata(t, "yum-allow", "")
	if !strings.Contains(data["primary"], "<summary>Rebuilt</summary>") || !strings.Contains(data["primary"], sum(again)) || strings.Contains(data["primary"], sum(demo)) {
		t.Fatalf("primary after overwrite:\n%s", data["primary"])
	}
	if w := e.do("GET", "yum-allow", "demo-1.0-1.el9.x86_64.rpm", nil); !bytes.Equal(w.Body.Bytes(), again) {
		t.Fatal("the file was not replaced")
	}
}

func TestDepth(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "yum-deep", "demo-1.0-1.el9.x86_64.rpm", demoRPM()); w.Code != 400 || !strings.Contains(w.Body.String(), "depth 1") {
		t.Fatalf("root upload at depth 1: %d %s", w.Code, w.Body)
	}
	for _, c := range []struct{ dir, name, rel string }{{"el9/", "demo", "1.el9"}, {"el8/", "demo", "1.el8"}, {"el9/", "beta", "1.el9"}} {
		b := nvra(c.name, "1.0", c.rel, "noarch")
		if w := e.do("PUT", "yum-deep", c.dir+"x/"+c.name+"-1.0-"+c.rel+".noarch.rpm", b); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", c.dir, w.Code, w.Body)
		}
	}
	if w := e.do("GET", "yum-deep", "repodata/repomd.xml", nil); w.Code != 404 {
		t.Fatalf("root repodata at depth 1: %d", w.Code)
	}
	_, el9 := e.repodata(t, "yum-deep", "el9/")
	_, el8 := e.repodata(t, "yum-deep", "el8/")
	// each directory lists its own packages, hrefs relative to it
	if hrefs := regexp.MustCompile(`<location href="([^"]+)"/>`).FindAllStringSubmatch(el9["primary"], -1); len(hrefs) != 2 || hrefs[0][1] != "x/beta-1.0-1.el9.noarch.rpm" || hrefs[1][1] != "x/demo-1.0-1.el9.noarch.rpm" {
		t.Fatalf("el9 locations: %v", hrefs)
	}
	if !strings.Contains(el8["primary"], `packages="1"`) || !strings.Contains(el8["primary"], `<location href="x/demo-1.0-1.el8.noarch.rpm"/>`) {
		t.Fatalf("el8 primary:\n%s", el8["primary"])
	}
	// deleting a package of el9 rebuilds el9 only
	e.sink.Reset()
	c := component.Component{Repo: "yum-deep", Format: "yum", Name: "el9/x/beta", Version: "1.0-1.el9.noarch"}
	if res, err := e.h.DeleteComponent(context.Background(), e.repos["yum-deep"], c, "admin"); err != nil || res.Assets != 1 {
		t.Fatalf("delete: %+v %v", res, err)
	}
	_, el9 = e.repodata(t, "yum-deep", "el9/")
	if !strings.Contains(el9["primary"], `packages="1"`) || strings.Contains(el9["primary"], "beta") {
		t.Fatalf("el9 after delete:\n%s", el9["primary"])
	}
	if got := e.sink.Events(); len(got) != 1 || got[0] != (repotest.Event{Kind: "delete", Actor: "admin", Repo: "yum-deep", Subject: "el9/x/beta@1.0-1.el9.noarch", Detail: "assets=1"}) {
		t.Fatalf("events: %v", got)
	}
}

func TestProxyAndGroup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// repodata is metadata: fetched again after metadataMaxAge; a package is content: cached for good
	for i := range 2 {
		if w := e.do("GET", "yum-proxy", "el9/repodata/repomd.xml", nil); w.Code != 200 || w.Body.String() != "<repomd>upstream</repomd>" {
			t.Fatalf("repomd %d: %d %s", i, w.Code, w.Body)
		}
		if w := e.do("GET", "yum-proxy", "el9/Packages/u/up-1.0-1.el9.noarch.rpm", nil); w.Code != 200 || w.Header().Get("Content-Type") != "application/x-rpm" {
			t.Fatalf("rpm %d: %d %v", i, w.Code, w.Header())
		}
	}
	if e.hits.Load() != 2 {
		t.Fatalf("upstream hits: %d", e.hits.Load())
	}
	e.now = e.now.Add(2 * time.Hour)
	e.do("GET", "yum-proxy", "el9/repodata/repomd.xml", nil)
	e.do("GET", "yum-proxy", "el9/Packages/u/up-1.0-1.el9.noarch.rpm", nil)
	if e.hits.Load() != 3 {
		t.Fatalf("upstream hits after expiry: %d", e.hits.Load())
	}
	// a directory listing is not a file: not cached, 404
	if w := e.do("GET", "yum-proxy", "el9/", nil); w.Code != 404 {
		t.Fatalf("dir: %d", w.Code)
	}
	if w := e.do("GET", "yum-proxy", "el9/nope.rpm", nil); w.Code != 404 {
		t.Fatalf("missing: %d", w.Code)
	}
	// the cached package is a component the proxy deleter can remove
	if _, name, version, ok := component.DeriveAsset("yum", "el9/Packages/u/up-1.0-1.el9.noarch.rpm"); !ok || name != "el9/Packages/u/up" || version != "1.0-1.el9.noarch" {
		t.Fatalf("derive: %s %s %v", name, version, ok)
	}
	// group: the first member that has the path answers; uploads go to the writeMember
	if w := e.do("PUT", "yum-all", "demo-1.0-1.el9.x86_64.rpm", demoRPM()); w.Code != 201 {
		t.Fatalf("PUT group: %d %s", w.Code, w.Body)
	}
	if _, err := e.h.Store.Get(ctx, "yum-hosted", "demo-1.0-1.el9.x86_64.rpm"); err != nil {
		t.Fatalf("writeMember: %v", err)
	}
	if w := e.do("GET", "yum-all", "repodata/repomd.xml", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "<repomd xmlns=") {
		t.Fatalf("group repomd from hosted: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "yum-all", "el9/repodata/repomd.xml", nil); w.Code != 200 || w.Body.String() != "<repomd>upstream</repomd>" {
		t.Fatalf("group repomd from proxy: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "yum-all", "el9/Packages/u/up-1.0-1.el9.noarch.rpm", nil); w.Code != 200 {
		t.Fatalf("group rpm from proxy: %d", w.Code)
	}
	if w := e.do("GET", "yum-all", "nothing/repodata/repomd.xml", nil); w.Code != 404 {
		t.Fatalf("group miss: %d", w.Code)
	}
}

func TestCleanupAndDelete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	demo := demoRPM()
	if w := e.do("PUT", "yum-hosted", "demo-1.0-1.el9.x86_64.rpm", demo); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	e.now = e.now.Add(48 * time.Hour)
	ev := &cleanup.Evaluator{DB: e.db, Now: func() time.Time { return e.now }}
	pol := cleanup.Policy{Name: "p", Format: "yum", Criteria: cleanup.Criteria{ComponentAgeDays: 1}}
	if err := pol.Validate(); err != nil {
		t.Fatal(err)
	}
	cands, _, _, _, err := ev.Evaluate(ctx, e.repos["yum-hosted"], pol, cleanup.DefaultScanBudget)
	if err != nil || len(cands) != 1 || cands[0].Label() != "demo@1.0-1.el9.x86_64" || len(cands[0].Assets) != 1 {
		t.Fatalf("candidates: %+v %v", cands, err) // the repodata files are not components
	}
	// a wrong repository or format, or a component without a version, is refused
	if _, err := e.h.DeleteComponent(ctx, e.repos["yum-proxy"], cands[0].Component, "system"); !errors.Is(err, component.ErrNotSupported) {
		t.Fatalf("proxy: %v", err)
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["yum-hosted"], component.Component{Format: "yum", Name: "demo"}, "system"); err == nil {
		t.Fatal("incomplete component deleted")
	}
	// downloaded since the evaluation: skipped
	if err := e.db.FlushDownloads(ctx, []meta.DownloadDelta{{Repo: "yum-hosted", Path: "demo-1.0-1.el9.x86_64.rpm", N: 1, Last: e.now}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["yum-hosted"], cands[0].Component, "system"); !errors.Is(err, cleanup.ErrChanged) {
		t.Fatalf("changed: %v", err)
	}
	cands, _, _, _, _ = ev.Evaluate(ctx, e.repos["yum-hosted"], pol, cleanup.DefaultScanBudget)
	res, err := e.h.DeleteComponent(ctx, e.repos["yum-hosted"], cands[0].Component, "system")
	if err != nil || res.Assets != 1 || res.Bytes != int64(len(demo)) {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if w := e.do("GET", "yum-hosted", "demo-1.0-1.el9.x86_64.rpm", nil); w.Code != 404 {
		t.Fatalf("after delete: %d", w.Code)
	}
	if pkgs, _ := e.db.ListPackages(ctx, "yum-hosted", "demo"); len(pkgs) != 0 {
		t.Fatalf("row left: %+v", pkgs)
	}
	_, data := e.repodata(t, "yum-hosted", "")
	if !strings.Contains(data["primary"], `packages="0"`) || strings.Contains(data["primary"], "<package") {
		t.Fatalf("primary after delete:\n%s", data["primary"])
	}
	// gone already: no error, nothing counted
	if res, err := e.h.DeleteComponent(ctx, e.repos["yum-hosted"], cands[0].Component, "system"); err != nil || res.Assets != 0 {
		t.Fatalf("again: %+v %v", res, err)
	}
}
