package pypi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
)

// realMetadata is the METADATA of realWheel, a wheel built from real bytes
// that fakeIndex serves for the project "real".
const realMetadata = "Metadata-Version: 2.1\nName: real\nVersion: 1.0\nSummary: a real wheel\nRequires-Dist: six>=1.0\n\nThe body.\n"

var realWheel = zipBytes(map[string]string{"real-1.0.dist-info/METADATA": realMetadata, "real/__init__.py": ""})

func zipBytes(files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, c := range files {
		w, _ := zw.Create(n)
		w.Write([]byte(c))
	}
	zw.Close()
	return buf.Bytes()
}

// smokeMetadata is the metadata of the wheel TestJSONAndMetadataHosted
// uploads: a folded Description-less document with a body, as build
// backends write.
const smokeExtra = "Summary: Smoke test\nRequires-Python: >=3.8\nRequires-Dist: six>=1.16\nRequires-Dist: attrs; extra == \"dev\"\nClassifier: Programming Language :: Python :: 3\nProject-URL: Homepage, https://example.com\nProvides-Extra: dev\n\nLong description\n"

type jsonDoc struct {
	Info     map[string]any              `json:"info"`
	Releases map[string][]map[string]any `json:"releases"`
	URLs     []map[string]any            `json:"urls"`
}

func decodeDoc(t *testing.T, body []byte) jsonDoc {
	t.Helper()
	var d jsonDoc
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatalf("json: %v: %s", err, body)
	}
	return d
}

func TestJSONAndMetadataHosted(t *testing.T) {
	e := newEnv(t)
	whl := dist(t, "Smoke_Hello", "1.0.0", "smoke_hello-1.0.0-py3-none-any.whl", smokeExtra)
	f := goodFields(whl)
	f["filetype"] = "bdist_wheel"
	ct, body := uploadBody(t, f, "smoke_hello-1.0.0-py3-none-any.whl", whl)
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	sd := dist(t, "Smoke_Hello", "0.9", "smoke_hello-0.9.tar.gz", "Summary: old\n")
	f = goodFields(sd)
	f["version"] = "0.9"
	ct, body = uploadBody(t, f, "smoke_hello-0.9.tar.gz", sd)
	if w := e.upload("py-hosted", ct, body); w.Code != 200 {
		t.Fatalf("upload sdist: %d %s", w.Code, w.Body.String())
	}
	wantMD := "Metadata-Version: 2.1\nName: Smoke_Hello\nVersion: 1.0.0\n" + smokeExtra

	// PEP 658: the sidecar is the METADATA as uploaded, and the simple
	// index advertises it with its hash under both names (PEP 714).
	w := e.do("GET", "py-hosted", "packages/smoke-hello/smoke_hello-1.0.0-py3-none-any.whl.metadata", "")
	if w.Code != 200 || w.Body.String() != wantMD {
		t.Fatalf("metadata: %d %q", w.Code, w.Body.String())
	}
	page := e.do("GET", "py-all", "simple/smoke-hello/", "").Body.String()
	if !strings.Contains(page, `data-core-metadata="sha256=`+sha(wantMD)+`" data-dist-info-metadata="sha256=`+sha(wantMD)+`"`) {
		t.Fatalf("html page: %s", page)
	}
	var simple struct{ Files []map[string]any }
	if err := json.Unmarshal(e.do("GET", "py-all", "simple/smoke-hello/", pipAccept).Body.Bytes(), &simple); err != nil || len(simple.Files) != 2 {
		t.Fatalf("json page: %v %s", err, page)
	}
	for _, f := range simple.Files {
		md, _ := f["core-metadata"].(map[string]any)
		if md["sha256"] == "" || f["dist-info-metadata"] == nil {
			t.Fatalf("json page file: %+v", f)
		}
	}

	// The project document: info of the newest release, every release's files.
	w = e.do("GET", "py-all", "pypi/Smoke.Hello/json", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("json: %d %s", w.Code, w.Body.String())
	}
	d := decodeDoc(t, w.Body.Bytes())
	if d.Info["name"] != "Smoke_Hello" || d.Info["version"] != "1.0.0" || d.Info["summary"] != "Smoke test" ||
		d.Info["requires_python"] != ">=3.8" || d.Info["description"] != "Long description\n" || d.Info["author"] != nil {
		t.Fatalf("info: %+v", d.Info)
	}
	if rd, _ := d.Info["requires_dist"].([]any); len(rd) != 2 || rd[0] != "six>=1.16" {
		t.Fatalf("requires_dist: %+v", d.Info["requires_dist"])
	}
	if pu, _ := d.Info["project_urls"].(map[string]any); pu["Homepage"] != "https://example.com" {
		t.Fatalf("project_urls: %+v", d.Info["project_urls"])
	}
	if len(d.Releases) != 2 || len(d.Releases["0.9"]) != 1 || len(d.URLs) != 1 {
		t.Fatalf("releases: %+v urls: %+v", d.Releases, d.URLs)
	}
	u := d.URLs[0]
	dg, _ := u["digests"].(map[string]any)
	if u["filename"] != "smoke_hello-1.0.0-py3-none-any.whl" || u["url"] != baseURL+"/repository/py-all/packages/smoke-hello/smoke_hello-1.0.0-py3-none-any.whl" ||
		dg["sha256"] != sha(whl) || dg["md5"] == nil || len(dg["blake2b_256"].(string)) != 64 || u["size"] != float64(len(whl)) ||
		u["packagetype"] != "bdist_wheel" || u["python_version"] != "py3" || u["upload_time"] == nil || u["yanked"] != false {
		t.Fatalf("url: %+v", u)
	}
	if sd := d.Releases["0.9"][0]; sd["packagetype"] != "sdist" || sd["python_version"] != "source" {
		t.Fatalf("sdist entry: %+v", sd)
	}

	// The release document names that release and has no releases map.
	w = e.do("GET", "py-hosted", "pypi/smoke-hello/0.9.0/json", "")
	d = decodeDoc(t, w.Body.Bytes())
	if w.Code != 200 || d.Info["version"] != "0.9" || d.Info["summary"] != "old" || d.Releases != nil || len(d.URLs) != 1 {
		t.Fatalf("release json: %d %+v", w.Code, d)
	}
	for _, p := range []string{"pypi/smoke-hello/2.0/json", "pypi/nope/json", "packages/smoke-hello/nope-1.0.tar.gz.metadata"} {
		if w := e.do("GET", "py-hosted", p, ""); w.Code != 404 {
			t.Errorf("%s: %d, want 404", p, w.Code)
		}
	}
	if w := e.do("HEAD", "py-hosted", "pypi/smoke-hello/json", ""); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %d bytes", w.Code, w.Body.Len())
	}
	if w := e.do("POST", "py-hosted", "pypi/smoke-hello/json", ""); w.Code != 405 {
		t.Fatalf("POST: %d", w.Code)
	}

	// Deleting the release removes the sidecar with the file.
	if _, err := e.h.DeleteComponent(context.Background(), e.repos["py-hosted"], component.Component{Format: "pypi", Name: "smoke-hello", Version: "1.0.0"}, "system"); err != nil {
		t.Fatal(err)
	}
	if _, assets := pypiState(t, e); assets != "packages/smoke-hello/smoke_hello-0.9.tar.gz,packages/smoke-hello/smoke_hello-0.9.tar.gz.metadata" {
		t.Fatalf("assets after delete: %s", assets)
	}
}

// Files stored by older builds have no sidecar and no metadata hash in
// their row: the index advertises metadata without a hash, the first
// request extracts the sidecar from the file, and the row learns the hash.
func TestMetadataBackfill(t *testing.T) {
	e := newEnv(t)
	md := "Metadata-Version: 2.1\nName: old-pkg\nVersion: 1.0\nSummary: stored by an older build\n"
	e.seedHosted(t, "old-pkg", "old_pkg-1.0.tar.gz", string(sdistBytes(t, map[string]string{"old_pkg-1.0/PKG-INFO": md})))
	e.seedHosted(t, "junk", "junk-1.0.tar.gz", "not an sdist")
	// the first listing extracts the sidecar, so the index advertises its hash
	if page := e.do("GET", "py-hosted", "simple/old-pkg/", "").Body.String(); !strings.Contains(page, `data-core-metadata="sha256=`+sha(md)+`"`) {
		t.Fatalf("page: %s", page)
	}
	w := e.do("GET", "py-hosted", "packages/old-pkg/old_pkg-1.0.tar.gz.metadata", "")
	if w.Code != 200 || w.Body.String() != md {
		t.Fatalf("metadata: %d %q", w.Code, w.Body.String())
	}
	if a, err := e.h.Store.Get(context.Background(), "py-hosted", "packages/old-pkg/old_pkg-1.0.tar.gz.metadata"); err != nil || a.ContentType != metadataType {
		t.Fatalf("sidecar asset: %+v %v", a, err)
	}
	d := decodeDoc(t, e.do("GET", "py-hosted", "pypi/old-pkg/json", "").Body.Bytes())
	if d.Info["summary"] != "stored by an older build" || d.Info["version"] != "1.0" {
		t.Fatalf("json: %+v", d.Info)
	}
	if w := e.do("GET", "py-hosted", "packages/junk/junk-1.0.tar.gz.metadata", ""); w.Code != 404 {
		t.Fatalf("junk metadata: %d", w.Code)
	}
	// a file without readable metadata still has a project document: the
	// files, with only name and version in info
	w = e.do("GET", "py-hosted", "pypi/junk/json", "")
	if d := decodeDoc(t, w.Body.Bytes()); w.Code != 200 || d.Info["name"] != "junk" || d.Info["version"] != "1.0" || d.Info["summary"] != nil || len(d.Releases["1.0"]) != 1 {
		t.Fatalf("junk json: %d %s", w.Code, w.Body)
	}
}

// A row the initial release stored without reading the archive (it validated
// only the file name) whose core metadata cannot be extracted is not
// advertised in the simple index: pip fetches an advertised PEP 658 sidecar
// unconditionally and aborts the install on its 404 (review PYPI-1).
func TestUnreadableMetadataNotAdvertised(t *testing.T) {
	e := newEnv(t)
	e.seedHosted(t, "oldpkg", "oldpkg-1.0.tar.gz", string(sdistBytes(t, map[string]string{"src/PKG-INFO": "Metadata-Version: 2.1\nName: oldpkg\nVersion: 1.0\n"})))
	md := "Metadata-Version: 2.1\nName: good\nVersion: 1.0\n"
	e.seedHosted(t, "good", "good-1.0.tar.gz", string(sdistBytes(t, map[string]string{"good-1.0/PKG-INFO": md})))
	for _, accept := range []string{"", jsonType} {
		page := e.do("GET", "py-hosted", "simple/oldpkg/", accept).Body.String()
		if strings.Contains(page, "core-metadata") || strings.Contains(page, "dist-info-metadata") {
			t.Fatalf("advertised although GET .metadata is 404 (%s):\n%s", accept, page)
		}
		if w := e.do("GET", "py-hosted", "packages/oldpkg/oldpkg-1.0.tar.gz.metadata", ""); w.Code != 404 {
			t.Fatalf("sidecar: %d", w.Code)
		}
		// the one that can be read is advertised with its hash right away
		page = e.do("GET", "py-hosted", "simple/good/", accept).Body.String()
		if !strings.Contains(page, "sha256") || !strings.Contains(page, sha(md)) {
			t.Fatalf("readable row (%s):\n%s", accept, page)
		}
	}
	if w := e.do("GET", "py-hosted", "pypi/oldpkg/json", ""); w.Code != 200 {
		t.Fatalf("pypi/oldpkg/json: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "py-hosted", "packages/oldpkg/oldpkg-1.0.tar.gz", ""); w.Code != 200 {
		t.Fatalf("the file itself: %d", w.Code)
	}
	// the outcome is remembered on the row, not re-read on every listing
	pkgs, err := e.h.Store.DB.ListPackages(context.Background(), "py-hosted", "oldpkg")
	if err != nil || len(pkgs) != 1 || !strings.Contains(pkgs[0].Attrs, `"metadata_sha256":"-"`) {
		t.Fatalf("row: %+v %v", pkgs, err)
	}
}

// peakMem runs fn and returns the peak heap+stack in use while it ran, in MiB.
func peakMem(fn func()) (float64, time.Duration) {
	var stop atomic.Bool
	var max uint64
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for !stop.Load() {
			runtime.ReadMemStats(&ms)
			if v := ms.HeapInuse + ms.StackInuse; v > max {
				max = v
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	start := time.Now()
	fn()
	elapsed := time.Since(start)
	stop.Store(true)
	<-done
	return float64(max) / (1 << 20), elapsed
}

// rewriteJSON of the largest upstream document accepted (maxJSONDoc) fits
// the server's memory budget (128 MiB; it needs ~9x the document, which is
// why the cap is what it is). Review PYPI-2.
func TestProxyJSONMemory(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"info":{"name":"big","version":"1.0"},"releases":{`)
	n := 0
	for sb.Len() < maxJSONDoc-4096 {
		if n > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `"1.%d.0":[`, n)
		for f := 0; f < 2; f++ {
			if f > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, `{"comment_text":"","digests":{"blake2b_256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","md5":"0123456789abcdef0123456789abcdef","sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},"downloads":-1,"filename":"big-1.%d.0-py3-none-any.whl","has_sig":false,"md5_digest":"0123456789abcdef0123456789abcdef","packagetype":"bdist_wheel","python_version":"py3","requires_python":">=3.8","size":123456,"upload_time":"2024-01-02T03:04:05","upload_time_iso_8601":"2024-01-02T03:04:05.000000Z","url":"https://files.pythonhosted.org/packages/ab/cd/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef/big-1.%d.0-py3-none-any.whl","yanked":false,"yanked_reason":null}`, n, n)
		}
		sb.WriteString("]")
		n++
	}
	sb.WriteString(`},"urls":[]}`)
	raw := []byte(sb.String())
	runtime.GC()
	var err error
	peak, el := peakMem(func() { _, err = rewriteJSON(raw, func(f string) string { return "http://x/" + f }) })
	t.Logf("doc %d MiB, %d releases: peak %.0f MiB, %v, err=%v", len(raw)>>20, n, peak, el, err)
	if err != nil {
		t.Fatal(err)
	}
	if peak > 100 {
		t.Fatalf("rewriting a %d MiB upstream JSON needed %.0f MiB (container: 128 MiB)", len(raw)>>20, peak)
	}
}

func TestProxyAndGroupJSON(t *testing.T) {
	e := newEnv(t)
	w := e.do("GET", "py-proxy", "pypi/six/json", "")
	d := decodeDoc(t, w.Body.Bytes())
	if w.Code != 200 || d.Info["summary"] != "Python 2 and 3 compatibility utilities" || len(d.Releases) != 2 {
		t.Fatalf("proxy json: %d %s", w.Code, w.Body.String())
	}
	want := baseURL + "/repository/py-proxy/packages/six/six-1.16.0-py2.py3-none-any.whl"
	if d.URLs[0]["url"] != want || d.Releases["1.0.0"][0]["url"] != baseURL+"/repository/py-proxy/packages/six/six-1.0.0.tar.gz" {
		t.Fatalf("urls not rewritten: %+v %+v", d.URLs, d.Releases)
	}
	if a, err := e.h.Store.Get(context.Background(), "py-proxy", "pypi/six/json"); err != nil || a.Size == 0 {
		t.Fatalf("cached document: %+v %v", a, err)
	}
	if w := e.do("GET", "py-proxy", "pypi/six/1.16.0/json", ""); w.Code != 200 {
		t.Fatalf("release json: %d", w.Code)
	}
	if w := e.do("GET", "py-proxy", "pypi/nope/json", ""); w.Code != 404 {
		t.Fatalf("unknown: %d", w.Code)
	}
	// The group answers from the member that owns the project, with the group's URLs.
	e.seedHosted(t, "six", "six-2.0.0.tar.gz", string(sdistBytes(t, map[string]string{"six-2.0.0/PKG-INFO": "Name: six\nVersion: 2.0.0\nSummary: internal\n"})))
	d = decodeDoc(t, e.do("GET", "py-all", "pypi/six/json", "").Body.Bytes())
	if d.Info["summary"] != "internal" || len(d.Releases) != 1 || d.URLs[0]["url"] != baseURL+"/repository/py-all/packages/six/six-2.0.0.tar.gz" {
		t.Fatalf("group json: %+v", d)
	}
	if w := e.do("GET", "py-all", "pypi/real/json", ""); w.Code != 404 { // the proxy has the project but no JSON upstream
		t.Fatalf("group json, proxy without the document: %d", w.Code)
	}
	if page := e.do("GET", "py-all", "simple/", "").Body.String(); !strings.Contains(page, `<a href="six/">`) {
		t.Fatalf("root: %s", page)
	}
}

func TestProxyMetadata(t *testing.T) {
	e := newEnv(t)
	// Advertised upstream (HTML, old attribute name): re-advertised under both names and fetched on request.
	page := e.do("GET", "py-proxy", "simple/legacy/", "").Body.String()
	if !strings.Contains(page, `data-core-metadata="sha256=`+sha("legacy-metadata")+`" data-dist-info-metadata="sha256=`+sha("legacy-metadata")+`"`) {
		t.Fatalf("legacy page: %s", page)
	}
	w := e.do("GET", "py-proxy", "packages/legacy/legacy-1.0.tar.gz.metadata", "")
	if w.Code != 200 || w.Body.String() != "legacy-metadata" {
		t.Fatalf("legacy metadata: %d %q", w.Code, w.Body.String())
	}
	// Not advertised upstream: 404 until the file is cached, then extracted from it.
	page = e.do("GET", "py-proxy", "simple/real/", pipAccept).Body.String()
	if strings.Contains(page, "metadata") {
		t.Fatalf("real page must not advertise metadata: %s", page)
	}
	if w := e.do("GET", "py-proxy", "packages/real/real-1.0-py3-none-any.whl.metadata", ""); w.Code != 404 {
		t.Fatalf("real metadata before caching: %d", w.Code)
	}
	if w := e.do("GET", "py-proxy", "packages/real/real-1.0-py3-none-any.whl", ""); w.Code != 200 {
		t.Fatalf("real wheel: %d", w.Code)
	}
	w = e.do("GET", "py-all", "packages/real/real-1.0-py3-none-any.whl.metadata", "")
	if w.Code != 200 || w.Body.String() != realMetadata {
		t.Fatalf("real metadata via group: %d %q", w.Code, w.Body.String())
	}
	// A hash mismatch is rejected like a file's.
	if w := e.do("GET", "py-proxy", "packages/six/six-1.16.0-py2.py3-none-any.whl.metadata", ""); w.Code != 404 {
		t.Fatalf("six (not advertised, not cached): %d", w.Code)
	}
	if w := e.do("GET", "py-proxy", "packages/six/six-1.0.0.tar.gz.metadata", ""); w.Code != 404 {
		t.Fatalf("unknown metadata: %d", w.Code)
	}
}

func TestParseCore(t *testing.T) {
	h := parseCore([]byte("Metadata-Version: 2.1\r\nName: x\r\nDescription: first\r\n        second\r\nClassifier: a\r\nClassifier: b\r\nbroken line\r\n\r\nbody\r\nmore\r\n"))
	if h.get("name") != "x" || h.get("description") != "first\nsecond" || len(h.fields["classifier"]) != 2 || h.body != "body\r\nmore\r\n" {
		t.Fatalf("%+v", h)
	}
	inf := info(h)
	if inf["description"] != "body\r\nmore\r\n" || inf["requires_dist"] != nil || inf["summary"] != nil || len(inf["classifiers"].([]string)) != 2 {
		t.Fatalf("%+v", inf)
	}
	if inf := info(parseCore([]byte("Name: y\nVersion: 1\nDescription: old style\n"))); inf["description"] != "old style" {
		t.Fatalf("%+v", inf)
	}
}

func TestCompareVersion(t *testing.T) {
	ordered := []string{"1.0.dev1", "1.0a1", "1.0a2", "1.0b1", "1.0rc1", "1.0", "1.0+abc", "1.0+abc.5", "1.0.post1.dev1", "1.0.post1", "1.0.post2", "1.0.1", "1.1", "2!0.1"}
	for i := range ordered {
		for j := range ordered {
			got := compareVersion(ordered[i], ordered[j])
			if (got < 0) != (i < j) || (got == 0) != (i == j) {
				t.Errorf("compare(%s, %s) = %d", ordered[i], ordered[j], got)
			}
		}
	}
	if compareVersion("1.0", "1.0.0") != 0 || compareVersion("1.0.post1.dev1", "1.0.post1") >= 0 {
		t.Fatal("equal forms / post-dev")
	}
	if got := latestVersion([]string{"2.0rc1", "1.9", "1.10", "2.0.dev5"}); got != "1.10" {
		t.Fatalf("latest: %s", got)
	}
	if got := latestVersion([]string{"2.0rc1", "2.0b3"}); got != "2.0rc1" {
		t.Fatalf("latest, all pre: %s", got)
	}
}

func TestRewriteJSON(t *testing.T) {
	in := `{"info":{"name":"x"},"releases":{"1":[{"filename":"x-1.tar.gz","url":"https://u/x-1.tar.gz","size":3}]},"urls":[{"filename":"x-1.tar.gz","url":"https://u/x-1.tar.gz"}],"extra":[1,2]}`
	out, err := rewriteJSON([]byte(in), func(f string) string { return "/p/" + f })
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	json.Unmarshal(out, &d)
	rel := d["releases"].(map[string]any)["1"].([]any)[0].(map[string]any)
	if rel["url"] != "/p/x-1.tar.gz" || rel["size"] != float64(3) || d["urls"].([]any)[0].(map[string]any)["url"] != "/p/x-1.tar.gz" || d["extra"] == nil {
		t.Fatalf("%s", out)
	}
	if _, err := rewriteJSON([]byte(`[]`), nil); err == nil {
		t.Fatal("array accepted")
	}
	if _, err := rewriteJSON([]byte(`{"urls":[{"url":"x"}]}`), nil); err == nil {
		t.Fatal("file without filename accepted")
	}
}

// A row whose attrs cannot be read fails the document (5xx), not a 404.
func TestJSONBadRow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	err := e.h.Store.DB.Tx(ctx, func(tx *meta.Tx) error {
		return tx.InsertPackage(ctx, meta.Package{Repo: "py-hosted", Format: "pypi", Name: "bad", Version: "bad-1.0.tar.gz", Attrs: "not json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", "py-hosted", "pypi/bad/json", ""); w.Code < 500 {
		t.Fatalf("bad row: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "py-hosted", "simple/bad/", ""); w.Code < 500 {
		t.Fatalf("bad row page: %d", w.Code)
	}
}
