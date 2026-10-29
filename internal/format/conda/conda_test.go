package conda

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// tarInfo is a package's tar stream (what a .tar.bz2 decompresses to):
// info/index.json, info/about.json when about is not "", then a payload.
func tarInfo(index, about string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name, body string) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))})
		io.WriteString(tw, body)
	}
	add("info/hash_input.json", "{}")
	add("info/index.json", index)
	if about != "" {
		add("info/about.json", about)
	}
	add("lib/python3.12/site-packages/foo/__init__.py", "print('hi')")
	tw.Close()
	return buf.Bytes()
}

// condaFile is a .conda: a zip with metadata.json and the two zstd tarballs.
func condaFile(stem string, entries ...string) []byte {
	if entries == nil {
		entries = []string{"metadata.json", "info-" + stem + ".tar.zst", "pkg-" + stem + ".tar.zst"}
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range entries {
		w, _ := zw.Create(name)
		if name == "metadata.json" {
			io.WriteString(w, `{"conda_pkg_format_version": 2}`)
		} else {
			io.WriteString(w, "\x28\xb5\x2f\xfd"+name) // zstd magic, not a real frame
		}
	}
	zw.Close()
	return buf.Bytes()
}

// emptyBz2 is an empty bzip2 stream (what bzip2 makes of no input).
var emptyBz2 = []byte{0x42, 0x5a, 0x68, 0x39, 0x17, 0x72, 0x45, 0x38, 0x50, 0x90, 0x00, 0x00, 0x00, 0x00}

func sums(b []byte) blob.Sums {
	s, m := sha256.Sum256(b), md5.Sum(b)
	return blob.Sums{SHA256: hex.EncodeToString(s[:]), MD5: hex.EncodeToString(m[:]), Size: int64(len(b))}
}

const fooIndex = `{"arch": null, "build": "py_1", "build_number": 1, "depends": ["python >=3.8", "numpy"], "constrains": ["pandas <3"], "license": "MIT", "license_family": "MIT", "name": "foo", "noarch": "python", "platform": null, "subdir": "noarch", "timestamp": 1700000000123, "version": "1.10"}`
const fooAbout = `{"home": "https://example.org/foo", "license": "MIT", "summary": "Foo does things", "description": "Longer text.", "dev_url": "https://git.example.org/foo", "doc_url": "https://docs.example.org/foo", "extra": {"recipe-maintainers": ["qa"]}}`

// tarRow is the row of a .tar.bz2 package made of a tar stream.
func tarRow(t *testing.T, subdir, file string, stream []byte) row {
	t.Helper()
	r := row{File: file, Subdir: subdir}
	var err error
	if r.Index, r.About, err = readInfo(bytes.NewReader(stream)); err != nil {
		t.Fatal(err)
	}
	if r, err = r.finish(sums(stream)); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReadInfoAndFinish(t *testing.T) {
	stream := tarInfo(fooIndex, fooAbout)
	r := tarRow(t, "noarch", "foo-1.10-py_1.tar.bz2", stream)
	s := sums(stream)
	b, _ := json.Marshal(r.Index)
	want := `{"arch":null,"build":"py_1","build_number":1,"constrains":["pandas \u003c3"],"depends":["python \u003e=3.8","numpy"],"license":"MIT","license_family":"MIT","md5":"` + s.MD5 + `","name":"foo","noarch":"python","platform":null,"sha256":"` + s.SHA256 + `","size":` + fmt.Sprint(s.Size) + `,"subdir":"noarch","timestamp":1700000000123,"version":"1.10"}`
	if string(b) != want {
		t.Fatalf("index:\n%s\nwant:\n%s", b, want)
	}
	if r.About["summary"] != "Foo does things" || r.version() != "1.10-py_1" || r.path() != "noarch/foo-1.10-py_1.tar.bz2" {
		t.Fatalf("row: %+v", r)
	}
	// no about.json, no build_number or depends: defaults
	r = tarRow(t, "linux-64", "bar-2-0.tar.bz2", tarInfo(`{"name":"bar","version":"2","build":"0"}`, ""))
	if b, _ := json.Marshal(r.Index); !strings.Contains(string(b), `"build_number":0,"depends":[]`) || !strings.Contains(string(b), `"subdir":"linux-64"`) || r.About != nil {
		t.Fatalf("defaults: %s %v", b, r.About)
	}
	// the tar's info/ comes first: a payload entry after the index ends the read
	stream = tarInfo(`{"name":"bar","version":"2","build":"0"}`, "")
	stream = append(stream[:len(stream)-1024], []byte("garbage after the archive end that is not a tar header")...)
	if _, _, err := readInfo(bytes.NewReader(stream)); err != nil {
		t.Fatalf("stopped before the payload: %v", err)
	}
	bad := map[string]struct {
		subdir, file string
		stream       []byte
		msg          string
	}{
		"no index": {"noarch", "foo-1.10-py_1.tar.bz2", nil, "no info/index.json"},
		"index absent": {"noarch", "foo-1.10-py_1.tar.bz2", func() []byte {
			var b bytes.Buffer
			tw := tar.NewWriter(&b)
			tw.WriteHeader(&tar.Header{Name: "info/files", Size: 1})
			io.WriteString(tw, "x")
			tw.Close()
			return b.Bytes()
		}(), "no info/index.json"},
		"bad json":      {"noarch", "foo-1.10-py_1.tar.bz2", tarInfo("{", ""), "info/index.json"},
		"name differs":  {"noarch", "bar-1.10-py_1.tar.bz2", tarInfo(fooIndex, ""), "does not match"},
		"build differs": {"noarch", "foo-1.10-py_2.tar.bz2", tarInfo(fooIndex, ""), "does not match"},
		"subdir":        {"linux-64", "foo-1.10-py_1.tar.bz2", tarInfo(fooIndex, ""), `for subdir "noarch"`},
		"huge index":    {"noarch", "foo-1.10-py_1.tar.bz2", tarInfo(`{"name":"foo","pad":"`+strings.Repeat("x", maxInfo)+`"}`, ""), "larger than"},
	}
	for name, c := range bad {
		r := row{File: c.file, Subdir: c.subdir}
		var err error
		if r.Index, r.About, err = readInfo(bytes.NewReader(c.stream)); err == nil {
			_, err = r.finish(sums(c.stream))
		}
		if repo.StatusOf(err) != 400 || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestReadConda(t *testing.T) {
	b := condaFile("foo-1.0-py_0")
	if err := readConda(bytes.NewReader(b), int64(len(b)), "foo-1.0-py_0"); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		b   []byte
		msg string
	}{
		"not zip":     {[]byte("not a zip file at all"), "not a .conda"},
		"no metadata": {condaFile("foo-1.0-py_0", "info-foo-1.0-py_0.tar.zst", "pkg-foo-1.0-py_0.tar.zst"), "no metadata.json"},
		"other stem":  {condaFile("bar-1.0-py_0"), "no info-foo-1.0-py_0.tar.zst"},
		"no pkg":      {condaFile("foo-1.0-py_0", "metadata.json", "info-foo-1.0-py_0.tar.zst"), "no pkg-foo-1.0-py_0.tar.zst"},
		"bad metadata": {func() []byte {
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			for _, n := range []string{"metadata.json", "info-foo-1.0-py_0.tar.zst", "pkg-foo-1.0-py_0.tar.zst"} {
				w, _ := zw.Create(n)
				io.WriteString(w, "{}")
			}
			zw.Close()
			return buf.Bytes()
		}(), "conda_pkg_format_version"},
	} {
		if err := readConda(bytes.NewReader(c.b), int64(len(c.b)), "foo-1.0-py_0"); repo.StatusOf(err) != 400 || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range [][2]string{{"1.9", "1.10"}, {"1.0rc1", "1.0"}, {"1.0", "1.0.1"}, {"1.0", "1.0.post1"}, {"1.0a1", "1.0b1"}, {"1.0b1", "1.0rc1"}, {"0.9", "1"}, {"2023.1", "2024.1"}, {"1.0.dev1", "1.0"}} {
		if !versionLess(c[0], c[1]) || versionLess(c[1], c[0]) {
			t.Errorf("%s < %s", c[0], c[1])
		}
	}
	if versionLess("1.0", "1.0") || versionLess("1.0", "1.00") {
		t.Error("equal")
	}
}

func TestDocumentsFromRows(t *testing.T) {
	rows := []row{
		tarRow(t, "noarch", "foo-1.10-py_1.tar.bz2", tarInfo(fooIndex, fooAbout)),
		tarRow(t, "noarch", "foo-1.9-py_0.tar.bz2", tarInfo(`{"name":"foo","version":"1.9","build":"py_0","build_number":0,"depends":["python"],"subdir":"noarch"}`, `{"summary":"old foo"}`)),
		tarRow(t, "noarch", "foo-1.10-py_2.tar.bz2", tarInfo(`{"name":"foo","version":"1.10","build":"py_2","build_number":2,"depends":["python"],"subdir":"noarch"}`, `{"summary":"foo build 2","home":"https://example.org/foo2"}`)),
		tarRow(t, "linux-64", "bar-2-0.tar.bz2", tarInfo(`{"name":"bar","version":"2","build":"0"}`, "")),
		tarRow(t, "linux-64", "foo-1.10-py_1.tar.bz2", tarInfo(`{"name":"foo","version":"1.10","build":"py_1","build_number":1,"subdir":"linux-64"}`, "")),
	}
	// a .conda of a build uploaded as both: listed under packages.conda
	condaRow := row{File: "foo-1.10-py_2.conda", Subdir: "noarch", Index: map[string]any{}}
	condaRow, err := condaRow.finish(sums([]byte("conda")))
	if err != nil {
		t.Fatal(err)
	}
	rows = append(rows, condaRow)
	rd, cd := newRepodata("noarch"), newChanneldata()
	for _, r := range rows {
		cd.add(r)
		if r.Subdir == "noarch" {
			if err := rd.add(r); err != nil {
				t.Fatal(err)
			}
		}
	}
	entry := func(r row) string { b, _ := json.Marshal(r.Index); return string(b) }
	b, _ := json.Marshal(rd)
	want := `{"info":{"subdir":"noarch"},"packages":{"foo-1.10-py_1.tar.bz2":` + entry(rows[0]) + `,"foo-1.10-py_2.tar.bz2":` + entry(rows[2]) + `,"foo-1.9-py_0.tar.bz2":` + entry(rows[1]) + `},"packages.conda":{"foo-1.10-py_2.conda":` + entry(condaRow) + `},"removed":[],"repodata_version":1}`
	if string(b) != want {
		t.Fatalf("repodata:\n%s\nwant:\n%s", b, want)
	}
	if !strings.Contains(entry(condaRow), `{"build":"py_2","build_number":0,"depends":[],"md5":"`) || !strings.Contains(entry(condaRow), `"name":"foo","sha256":"`) || !strings.HasSuffix(entry(condaRow), `"size":5,"subdir":"noarch","version":"1.10"}`) {
		t.Fatalf("conda entry: %s", entry(condaRow))
	}
	// channeldata: the latest version (then build number) of each name gives the about
	b, _ = json.Marshal(cd)
	want = `{"channeldata_version":1,"packages":{"bar":{"subdirs":["linux-64"],"version":"2"},"foo":{"home":"https://example.org/foo2","subdirs":["linux-64","noarch"],"summary":"foo build 2","version":"1.10"}},"subdirs":["linux-64","noarch"]}`
	if string(b) != want {
		t.Fatalf("channeldata:\n%s\nwant:\n%s", b, want)
	}
}

func TestMerge(t *testing.T) {
	ctx := context.Background()
	hosted := `{"info":{"subdir":"linux-64"},"packages":{"a-1-0.tar.bz2":{"name":"a","src":"hosted"}},"packages.conda":{"dup-1-0.conda":{"name":"dup","src":"hosted"}},"removed":[],"repodata_version":1}`
	upstream := `{"info":{"subdir":"linux-64"},"packages.conda":{"dup-1-0.conda":{"name":"dup","src":"up"},"c-2-0.conda":{"name":"c", "nested":{"x":[1,{"y":null}]}}},"packages":{"b-1-0.tar.bz2":{"name":"b"}},"removed":["old-0-0.tar.bz2"],"repodata_version":1,"extra":[1,2,{"k":"v"}]}`
	var out bytes.Buffer
	if err := mergeRepodata("linux-64")(ctx, []io.ReadSeeker{strings.NewReader(hosted), strings.NewReader(upstream)}, &out); err != nil {
		t.Fatal(err)
	}
	want := `{"info":{"subdir":"linux-64"},"packages":{"a-1-0.tar.bz2":{"name":"a","src":"hosted"},"b-1-0.tar.bz2":{"name":"b"}},"packages.conda":{"dup-1-0.conda":{"name":"dup","src":"hosted"},"c-2-0.conda":{"name":"c", "nested":{"x":[1,{"y":null}]}}},"removed":[],"repodata_version":1}`
	if out.String() != want {
		t.Fatalf("merged:\n%s\nwant:\n%s", out.String(), want)
	}
	var parsed repodata
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil || len(parsed.Packages) != 2 || len(parsed.Conda) != 2 {
		t.Fatalf("merged is not valid repodata: %v %+v", err, parsed)
	}
	out.Reset()
	if err := mergeRepodata("noarch")(ctx, nil, &out); err != nil || out.String() != `{"info":{"subdir":"noarch"},"packages":{},"packages.conda":{},"removed":[],"repodata_version":1}` {
		t.Fatalf("no members: %v %s", err, out.String())
	}
	for name, doc := range map[string]string{"truncated": hosted[:len(hosted)/2], "array": `[1,2]`, "not json": "<html>"} {
		out.Reset()
		if err := mergeRepodata("linux-64")(ctx, []io.ReadSeeker{strings.NewReader(doc)}, &out); err == nil {
			t.Errorf("%s: merged", name)
		}
	}
	// channeldata: first member's entry per name, the subdirs united
	out.Reset()
	h := `{"channeldata_version":1,"packages":{"dup":{"version":"1","subdirs":["noarch"]}},"subdirs":["noarch"]}`
	u := `{"channeldata_version":1,"subdirs":["linux-64","noarch"],"packages":{"up":{"version":"2","subdirs":["linux-64"]},"dup":{"version":"9","subdirs":["linux-64"]}}}`
	if err := mergeChanneldata(ctx, []io.ReadSeeker{strings.NewReader(h), strings.NewReader(u)}, &out); err != nil {
		t.Fatal(err)
	}
	if want := `{"channeldata_version":1,"packages":{"dup":{"version":"1","subdirs":["noarch"]},"up":{"version":"2","subdirs":["linux-64"]}},"subdirs":["linux-64","noarch"]}`; out.String() != want {
		t.Fatalf("channeldata:\n%s\nwant:\n%s", out.String(), want)
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
	big   []byte // the upstream's big/repodata.json
}

const upRepodata = `{"info":{"subdir":"linux-64"},"packages":{"up-1.0-0.tar.bz2":{"build":"0","build_number":0,"depends":[],"md5":"m","name":"up","sha256":"s","size":2,"subdir":"linux-64","version":"1.0"}},"packages.conda":{"dup-1.0-0.conda":{"name":"dup","src":"proxy"},"up-2.0-0.conda":{"name":"up","version":"2.0"}},"removed":[],"repodata_version":1}`
const upChanneldata = `{"channeldata_version":1,"packages":{"dup":{"subdirs":["linux-64"],"version":"proxy"},"up":{"subdirs":["linux-64"],"version":"2.0"}},"subdirs":["linux-64","noarch"]}`

// upstream is a channel: linux-64 with two packages, channeldata, a
// compressed repodata, and a big repodata.json under big/.
func (e *env) upstream(w http.ResponseWriter, r *http.Request) {
	e.hits.Add(1)
	switch r.URL.Path {
	case "/linux-64/repodata.json", "/linux-64/current_repodata.json":
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, upRepodata)
	case "/linux-64/repodata.json.zst":
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "ZSTD")
	case "/channeldata.json":
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, upChanneldata)
	case "/linux-64/up-1.0-0.tar.bz2":
		w.Header().Set("Content-Type", "application/x-tar")
		io.WriteString(w, "up")
	case "/big/repodata.json":
		w.Header().Set("Content-Type", "application/json")
		w.Write(e.big)
	case "/linux-64/":
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html>Index of /linux-64/</html>")
	default:
		http.NotFound(w, r)
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	// ~6 MB: many entries, more than any buffer the proxy or merge keeps
	var big strings.Builder
	big.WriteString(`{"info":{"subdir":"big"},"packages":{`)
	for i := range 40000 {
		if i > 0 {
			big.WriteByte(',')
		}
		fmt.Fprintf(&big, `"p%05d-1.0-0.tar.bz2":{"build":"0","build_number":0,"depends":["python >=3.8","numpy"],"license":"BSD-3-Clause","md5":"0123456789abcdef0123456789abcdef","name":"p%05d","sha256":"%064d","size":%d,"subdir":"big","timestamp":1700000000000,"version":"1.0"}`, i, i, i, i)
	}
	big.WriteString(`},"packages.conda":{},"removed":[],"repodata_version":1}`)
	e.big = []byte(big.String())
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
		{Name: "conda-hosted", Format: "conda", Type: "hosted", WritePolicy: "allow_once"},
		{Name: "conda-allow", Format: "conda", Type: "hosted", WritePolicy: "allow"},
		{Name: "conda-ro", Format: "conda", Type: "hosted", WritePolicy: "deny"},
		{Name: "conda-proxy", Format: "conda", Type: "proxy", Remote: e.up.URL + "/",
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "conda-all", Format: "conda", Type: "group", Members: []string{"conda-hosted", "conda-proxy"}, WriteMember: "conda-hosted"},
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

// get fetches a document and checks the status and content type.
func (e *env) get(t *testing.T, repoName, path string, want int) string {
	t.Helper()
	w := e.do("GET", repoName, path, nil)
	if w.Code != want || (want == 200 && strings.HasSuffix(path, ".json") && w.Header().Get("Content-Type") != ctJSON) {
		t.Fatalf("GET %s/%s: %d %v %s", repoName, path, w.Code, w.Header(), w.Body)
	}
	return w.Body.String()
}

// condaEntry is the repodata entry of a .conda upload: what the file name and sums give.
func condaEntry(name, version, build, subdir string, b []byte) string {
	s := sums(b)
	return fmt.Sprintf(`{"build":%q,"build_number":0,"depends":[],"md5":%q,"name":%q,"sha256":%q,"size":%d,"subdir":%q,"version":%q}`, build, s.MD5, name, s.SHA256, s.Size, subdir, version)
}

func TestHostedUploadAndDocuments(t *testing.T) {
	e := newEnv(t)
	// before any upload: empty documents for a subdir, nothing for other paths
	empty := `{"info":{"subdir":"noarch"},"packages":{},"packages.conda":{},"removed":[],"repodata_version":1}`
	if got := e.get(t, "conda-hosted", "noarch/repodata.json", 200); got != empty {
		t.Fatalf("empty repodata: %s", got)
	}
	if got := e.get(t, "conda-hosted", "channeldata.json", 200); got != `{"channeldata_version":1,"packages":{},"subdirs":[]}` {
		t.Fatalf("empty channeldata: %s", got)
	}
	for _, p := range []string{"Bad_Dir/repodata.json", "noarch/repodata.json.bz2", "noarch/repodata.json.zst", "noarch/x.json", "noarch/foo-1.0-py_0.conda", "noarch/sub/repodata.json"} {
		e.get(t, "conda-hosted", p, 404)
	}
	foo := condaFile("foo-1.0-py_0")
	if w := e.do("PUT", "conda-hosted", "noarch/foo-1.0-py_0.conda", foo); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0] != (repotest.Event{Kind: "upload", Repo: "conda-hosted", Subject: "noarch/foo@1.0-py_0", Detail: "noarch/foo-1.0-py_0.conda"}) {
		t.Fatalf("events: %v", got)
	}
	w := e.do("GET", "conda-hosted", "noarch/foo-1.0-py_0.conda", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), foo) || w.Header().Get("Content-Type") != ctZip {
		t.Fatalf("GET package: %d %v", w.Code, w.Header())
	}
	want := `{"info":{"subdir":"noarch"},"packages":{},"packages.conda":{"foo-1.0-py_0.conda":` + condaEntry("foo", "1.0", "py_0", "noarch", foo) + `},"removed":[],"repodata_version":1}`
	for _, doc := range []string{"noarch/repodata.json", "noarch/current_repodata.json"} {
		if got := e.get(t, "conda-hosted", doc, 200); got != want {
			t.Fatalf("%s:\n%s\nwant:\n%s", doc, got, want)
		}
	}
	if got := e.get(t, "conda-hosted", "channeldata.json", 200); got != `{"channeldata_version":1,"packages":{"foo":{"subdirs":["noarch"],"version":"1.0"}},"subdirs":["noarch"]}` {
		t.Fatalf("channeldata: %s", got)
	}
	if got := e.get(t, "conda-hosted", "linux-64/repodata.json", 200); got != strings.Replace(empty, "noarch", "linux-64", 1) {
		t.Fatalf("other subdir: %s", got)
	}
	// allow_once: the same file again is a 409, whatever its content
	if w := e.do("PUT", "conda-hosted", "noarch/foo-1.0-py_0.conda", condaFile("foo-1.0-py_0")); w.Code != 409 {
		t.Fatalf("republish: %d %s", w.Code, w.Body)
	}
	// an empty bzip2 stream decompresses to no tar at all: the bzip2 path
	// is taken, and the package has no index
	if w := e.do("PUT", "conda-hosted", "noarch/foo-1.0-py_1.tar.bz2", emptyBz2); w.Code != 400 || !strings.Contains(w.Body.String(), "no info/index.json") {
		t.Fatalf("empty bz2: %d %s", w.Code, w.Body)
	}
	for _, c := range []struct {
		path string
		body []byte
		want int
	}{
		{"foo-1.0-py_0.conda", foo, 400},                 // no subdir
		{"noarch/foo-1.0.conda", foo, 400},               // no build
		{"noarch/foo.tar.bz2", foo, 400},                 // no version
		{"noarch/foo-1.0-py_0.zip", foo, 400},            // not a package
		{"noarch/repodata.json", []byte("{}"), 400},      // not a package
		{"noarch/a/foo-1.0-py_0.conda", foo, 400},        // nested
		{"Noarch/foo-1.0-py_0.conda", foo, 400},          // not a subdir
		{"noarch/empty-1.0-0.conda", nil, 400},           // empty
		{"noarch/bar-1.0-py_0.conda", foo, 400},          // the zip's tarballs are foo's
		{"noarch/foo-1.0-py_0.tar.bz2", foo, 400},        // a zip is not bzip2
		{"../noarch/foo-1.0-py_0.conda", foo, 400},       // escapes
		{"noarch/junk-1.0-0.conda", []byte("junk"), 400}, // not a zip
	} {
		if w := e.do("PUT", "conda-hosted", c.path, c.body); w.Code != c.want {
			t.Errorf("PUT %s: %d %s", c.path, w.Code, w.Body)
		}
	}
	// more packages: another subdir of the same build is its own row and
	// document; a later version moves channeldata on
	for _, p := range []string{"linux-64/foo-1.0-py_0.conda", "noarch/foo-1.1-py_0.conda", "noarch/alpha-2-0.conda"} {
		if w := e.do("PUT", "conda-hosted", p, condaFile(strings.TrimSuffix(p[strings.Index(p, "/")+1:], ".conda"))); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", p, w.Code, w.Body)
		}
	}
	var rd repodata
	if err := json.Unmarshal([]byte(e.get(t, "conda-hosted", "noarch/repodata.json", 200)), &rd); err != nil || len(rd.Conda) != 3 || rd.Conda["alpha-2-0.conda"] == nil || len(rd.Packages) != 0 {
		t.Fatalf("noarch after more uploads: %v %+v", err, rd)
	}
	var rd64 repodata // a fresh value: Unmarshal adds to a map it finds
	if err := json.Unmarshal([]byte(e.get(t, "conda-hosted", "linux-64/repodata.json", 200)), &rd64); err != nil || len(rd64.Conda) != 1 || rd64.Info.Subdir != "linux-64" {
		t.Fatalf("linux-64: %v %+v", err, rd64)
	}
	if got := e.get(t, "conda-hosted", "channeldata.json", 200); got != `{"channeldata_version":1,"packages":{"alpha":{"subdirs":["noarch"],"version":"2"},"foo":{"subdirs":["linux-64","noarch"],"version":"1.1"}},"subdirs":["linux-64","noarch"]}` {
		t.Fatalf("channeldata: %s", got)
	}
	if w := e.do("DELETE", "conda-hosted", "noarch/alpha-2-0.conda", nil); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD, PUT" {
		t.Fatalf("DELETE: %d %q", w.Code, w.Header().Get("Allow"))
	}
	if w := e.do("PUT", "conda-proxy", "noarch/x-1.0-0.conda", condaFile("x-1.0-0")); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("PUT proxy: %d %q", w.Code, w.Header().Get("Allow"))
	}
}

func TestWritePolicy(t *testing.T) {
	e := newEnv(t)
	foo := condaFile("foo-1.0-py_0")
	if w := e.do("PUT", "conda-ro", "noarch/foo-1.0-py_0.conda", foo); w.Code != 400 {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	a, ps := repotest.Principals(t, e.db, "conda-allow", map[string][]string{
		"adder":  {meta.ActionAdd},
		"editor": {meta.ActionAdd, meta.ActionEdit},
	})
	e.h.Auth = a
	if w := e.doAs(ps["adder"], "PUT", "conda-allow", "noarch/foo-1.0-py_0.conda", foo); w.Code != 201 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	if w := e.doAs(ps["adder"], "PUT", "conda-allow", "noarch/foo-1.0-py_0.conda", foo); w.Code != 403 {
		t.Fatalf("overwrite without edit: %d %s", w.Code, w.Body)
	}
	again := condaFile("foo-1.0-py_0", "metadata.json", "info-foo-1.0-py_0.tar.zst", "pkg-foo-1.0-py_0.tar.zst", "extra")
	if w := e.doAs(ps["editor"], "PUT", "conda-allow", "noarch/foo-1.0-py_0.conda", again); w.Code != 201 {
		t.Fatalf("overwrite with edit: %d %s", w.Code, w.Body)
	}
	if got := e.get(t, "conda-allow", "noarch/repodata.json", 200); !strings.Contains(got, sums(again).SHA256) || strings.Contains(got, sums(foo).SHA256) {
		t.Fatalf("repodata after overwrite: %s", got)
	}
	if w := e.do("GET", "conda-allow", "noarch/foo-1.0-py_0.conda", nil); !bytes.Equal(w.Body.Bytes(), again) {
		t.Fatal("the file was not replaced")
	}
}

func TestProxyAndGroup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// documents are metadata: fetched again after metadataMaxAge; a package is content: cached for good
	for i := range 2 {
		if got := e.get(t, "conda-proxy", "linux-64/repodata.json", 200); got != upRepodata {
			t.Fatalf("repodata %d: %s", i, got)
		}
		if got := e.get(t, "conda-proxy", "linux-64/repodata.json.zst", 200); got != "ZSTD" {
			t.Fatalf("zst %d: %s", i, got)
		}
		if w := e.do("GET", "conda-proxy", "linux-64/up-1.0-0.tar.bz2", nil); w.Code != 200 || w.Body.String() != "up" || w.Header().Get("Content-Type") != "application/x-tar" {
			t.Fatalf("package %d: %d %v", i, w.Code, w.Header())
		}
	}
	if e.hits.Load() != 3 {
		t.Fatalf("upstream hits: %d", e.hits.Load())
	}
	e.now = e.now.Add(2 * time.Hour)
	e.get(t, "conda-proxy", "linux-64/repodata.json", 200)
	e.get(t, "conda-proxy", "linux-64/repodata.json.zst", 200)
	e.do("GET", "conda-proxy", "linux-64/up-1.0-0.tar.bz2", nil)
	if e.hits.Load() != 5 {
		t.Fatalf("upstream hits after expiry: %d", e.hits.Load())
	}
	// a big repodata.json goes through whole, as the upstream sent it
	if got := e.get(t, "conda-proxy", "big/repodata.json", 200); got != string(e.big) {
		t.Fatalf("big: %d bytes, want %d", len(got), len(e.big))
	}
	// a proxy has no empty documents of its own: the upstream's 404 shows
	e.get(t, "conda-proxy", "osx-64/repodata.json", 404)
	e.get(t, "conda-proxy", "linux-64/", 404) // a directory listing is not a file
	e.get(t, "conda-proxy", "linux-64/nope-1.0-0.conda", 404)
	// the cached package is a component the proxy deleter can remove
	if g, name, version, ok := component.DeriveAsset("conda", "linux-64/up-1.0-0.tar.bz2"); !ok || g != "linux-64" || name != "up" || version != "1.0-0" {
		t.Fatalf("derive: %s %s %s %v", g, name, version, ok)
	}

	// group: a file from the first member that has it; uploads go to the writeMember
	dup := condaFile("dup-1.0-0")
	if w := e.do("PUT", "conda-all", "linux-64/dup-1.0-0.conda", dup); w.Code != 201 {
		t.Fatalf("PUT group: %d %s", w.Code, w.Body)
	}
	if _, err := e.h.Store.Get(ctx, "conda-hosted", "linux-64/dup-1.0-0.conda"); err != nil {
		t.Fatalf("writeMember: %v", err)
	}
	if w := e.do("GET", "conda-all", "linux-64/up-1.0-0.tar.bz2", nil); w.Code != 200 || w.Body.String() != "up" {
		t.Fatalf("group package from proxy: %d", w.Code)
	}
	if w := e.do("GET", "conda-all", "linux-64/dup-1.0-0.conda", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), dup) {
		t.Fatalf("group package from hosted: %d", w.Code)
	}
	// the merged repodata: the hosted member's packages first, the
	// upstream's dup-1.0-0.conda hidden by the hosted one
	hits := e.hits.Load()
	merged := `{"info":{"subdir":"linux-64"},"packages":{"up-1.0-0.tar.bz2":{"build":"0","build_number":0,"depends":[],"md5":"m","name":"up","sha256":"s","size":2,"subdir":"linux-64","version":"1.0"}},"packages.conda":{"dup-1.0-0.conda":` + condaEntry("dup", "1.0", "0", "linux-64", dup) + `,"up-2.0-0.conda":{"name":"up","version":"2.0"}},"removed":[],"repodata_version":1}`
	for i := range 2 {
		for _, doc := range []string{"linux-64/repodata.json", "linux-64/current_repodata.json"} {
			if got := e.get(t, "conda-all", doc, 200); got != merged {
				t.Fatalf("%s %d:\n%s\nwant:\n%s", doc, i, got, merged)
			}
		}
	}
	if got := e.get(t, "conda-all", "channeldata.json", 200); got != `{"channeldata_version":1,"packages":{"dup":{"subdirs":["linux-64"],"version":"1.0"},"up":{"subdirs":["linux-64"],"version":"2.0"}},"subdirs":["linux-64","noarch"]}` {
		t.Fatalf("group channeldata: %s", got)
	}
	// the merges are cached: two upstream fetches (repodata, channeldata), not one per request
	if e.hits.Load() != hits+2 {
		t.Fatalf("upstream hits for the group: %d", e.hits.Load()-hits)
	}
	// a hosted change re-merges; the compressed forms are not served by a
	// group, so conda falls back to the merged repodata.json
	if w := e.do("PUT", "conda-all", "linux-64/new-3-0.conda", condaFile("new-3-0")); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if got := e.get(t, "conda-all", "linux-64/repodata.json", 200); !strings.Contains(got, `"new-3-0.conda":`) || !strings.Contains(got, `"up-2.0-0.conda":`) {
		t.Fatalf("re-merged: %s", got)
	}
	e.get(t, "conda-all", "linux-64/repodata.json.zst", 404)
	e.get(t, "conda-all", "linux-64/repodata.json.bz2", 404)
	e.get(t, "conda-all", "linux-64/repodata.jlap", 404)
	// a subdir no member has: an empty document, as a hosted channel answers
	if got := e.get(t, "conda-all", "osx-arm64/repodata.json", 200); got != `{"info":{"subdir":"osx-arm64"},"packages":{},"packages.conda":{},"removed":[],"repodata_version":1}` {
		t.Fatalf("group empty: %s", got)
	}
	e.get(t, "conda-all", "osx-arm64/nope-1.0-0.conda", 404)
}

func TestCleanupAndDelete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	foo := condaFile("foo-1.0-py_0")
	if w := e.do("PUT", "conda-hosted", "noarch/foo-1.0-py_0.conda", foo); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	// the same build as a .tar.bz2 (as conda-forge ships both): one component, two files
	stream := tarInfo(`{"name":"foo","version":"1.0","build":"py_0","subdir":"noarch"}`, "")
	rw := tarRow(t, "noarch", "foo-1.0-py_0.tar.bz2", stream)
	attrs, _ := json.Marshal(rw)
	s, err := e.h.Store.WriteBlob("conda-hosted", bytes.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.Tx(ctx, func(tx *meta.Tx) error {
		if err := tx.InsertPackage(ctx, meta.Package{Repo: "conda-hosted", Format: "conda", Name: "foo", Version: rw.path(), Attrs: string(attrs), CreatedAt: e.now}); err != nil {
			return err
		}
		return tx.PutAsset(ctx, e.h.Store.NewAsset("conda-hosted", rw.path(), ctBzip2, s))
	}); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(48 * time.Hour)
	ev := &cleanup.Evaluator{DB: e.db, Now: func() time.Time { return e.now }}
	pol := cleanup.Policy{Name: "p", Format: "conda", Criteria: cleanup.Criteria{ComponentAgeDays: 1}}
	if err := pol.Validate(); err != nil {
		t.Fatal(err)
	}
	cands, _, _, _, err := ev.Evaluate(ctx, e.repos["conda-hosted"], pol, cleanup.DefaultScanBudget)
	if err != nil || len(cands) != 1 || cands[0].Label() != "noarch/foo@1.0-py_0" || len(cands[0].Assets) != 2 {
		t.Fatalf("candidates: %+v %v", cands, err) // the documents are not components
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["conda-proxy"], cands[0].Component, "system"); !errors.Is(err, component.ErrNotSupported) {
		t.Fatalf("proxy: %v", err)
	}
	for _, c := range []component.Component{{Format: "conda", Name: "foo", Version: "1.0-py_0"}, {Format: "conda", Group: "noarch", Name: "foo"}, {Format: "conda", Group: "noarch", Name: "foo", Version: "1.0"}} {
		if _, err := e.h.DeleteComponent(ctx, e.repos["conda-hosted"], c, "system"); err == nil {
			t.Fatalf("incomplete component %+v deleted", c)
		}
	}
	// downloaded since the evaluation: skipped
	if err := e.db.FlushDownloads(ctx, []meta.DownloadDelta{{Repo: "conda-hosted", Path: "noarch/foo-1.0-py_0.conda", N: 1, Last: e.now}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["conda-hosted"], cands[0].Component, "system"); !errors.Is(err, cleanup.ErrChanged) {
		t.Fatalf("changed: %v", err)
	}
	cands, _, _, _, _ = ev.Evaluate(ctx, e.repos["conda-hosted"], pol, cleanup.DefaultScanBudget)
	e.sink.Reset()
	res, err := e.h.DeleteComponent(ctx, e.repos["conda-hosted"], cands[0].Component, "system")
	if err != nil || res.Assets != 2 || res.Bytes != int64(len(foo)+len(stream)) {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0] != (repotest.Event{Kind: "delete", Actor: "system", Repo: "conda-hosted", Subject: "noarch/foo@1.0-py_0", Detail: "assets=2"}) {
		t.Fatalf("events: %v", got)
	}
	for _, p := range []string{"noarch/foo-1.0-py_0.conda", "noarch/foo-1.0-py_0.tar.bz2"} {
		e.get(t, "conda-hosted", p, 404)
	}
	if pkgs, _ := e.db.ListPackages(ctx, "conda-hosted", "foo"); len(pkgs) != 0 {
		t.Fatalf("rows left: %+v", pkgs)
	}
	if got := e.get(t, "conda-hosted", "noarch/repodata.json", 200); got != `{"info":{"subdir":"noarch"},"packages":{},"packages.conda":{},"removed":[],"repodata_version":1}` {
		t.Fatalf("repodata after delete: %s", got)
	}
	if got := e.get(t, "conda-hosted", "channeldata.json", 200); got != `{"channeldata_version":1,"packages":{},"subdirs":[]}` {
		t.Fatalf("channeldata after delete: %s", got)
	}
	// gone already: no error, nothing counted
	if res, err := e.h.DeleteComponent(ctx, e.repos["conda-hosted"], cands[0].Component, "system"); err != nil || res.Assets != 0 {
		t.Fatalf("again: %+v %v", res, err)
	}
}
