package yum

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http/httptest"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/resolve"
)

// rpmOf is a package of that name with content of its own.
func rpmOf(name, ver, payload string) []byte {
	return append(nvra(name, ver, "1", "noarch"), payload...)
}

// published is a package on the upstream: where its repodata says it is.
type published struct {
	at  string
	rpm []byte
}

// publish puts a repository on the upstream at dir: its packages and repodata,
// made as a hosted repository's, with more <data> elements if given.
func (e *env) publish(t *testing.T, dir string, pkgs []published, extra ...string) {
	t.Helper()
	var list []*Package
	for _, c := range pkgs {
		p, err := Parse(bytes.NewReader(c.rpm), int64(len(c.rpm)))
		if err != nil {
			t.Fatal(err)
		}
		p.Checksum, p.Location, p.Uploaded = sum(c.rpm), c.at, e.now.Unix()
		list = append(list, p)
		e.files.Store("/"+dir+c.at, c.rpm)
	}
	var data []repoData
	for _, d := range []struct{ typ, xml string }{{"primary", Primary(list)}, {"filelists", Filelists(list)}, {"other", Other(list)}} {
		rd, err := newRepoData(d.typ, d.xml)
		if err != nil {
			t.Fatal(err)
		}
		e.files.Store("/"+dir+rd.file(), rd.gz)
		data = append(data, rd)
	}
	e.files.Store("/"+dir+repomdFile, []byte(Repomd(data, e.now, extra...)))
}

// publishPrimary puts at dir a repomd.xml that lists one file as the primary:
// doc, as it is, with no filelists or other.
func (e *env) publishPrimary(dir string, doc []byte) {
	rd := repoData{typ: "primary", sum: sum(doc), size: int64(len(doc)), openSum: sum(doc), openSize: int64(len(doc))}
	e.files.Store("/"+dir+rd.file(), doc)
	e.files.Store("/"+dir+repomdFile, []byte(Repomd([]repoData{rd}, e.now)))
}

func gzipped(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	io.WriteString(w, s)
	w.Close()
	return b.Bytes()
}

var hrefRE = regexp.MustCompile(`<location href="([^"]+)"/>`)

func hrefs(primary string) []string {
	var out []string
	for _, m := range hrefRE.FindAllStringSubmatch(primary, -1) {
		out = append(out, m[1])
	}
	return out
}

func pkgids(doc string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`<package pkgid="(\w+)"`).FindAllStringSubmatch(doc, -1) {
		out = append(out, m[1])
	}
	return out
}

// wellFormed parses a merged document as a client would.
func wellFormed(t *testing.T, doc string) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(doc))
	for {
		_, err := d.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("%v in:\n%s", err, doc)
		}
	}
}

// The merge at a path: every member's packages through one baseurl, the first
// member keeping a package and a path it has, primary, filelists and other in
// agreement, and the answer the same until a member's repomd.xml changes.
func TestMergedGroup(t *testing.T) {
	e := newEnv(t)
	bothHosted, bothUp := rpmOf("both", "1.0", "hosted"), rpmOf("both", "1.0", "upstream")
	for p, b := range map[string][]byte{"demo-1.0-1.el9.x86_64.rpm": demoRPM(), "both-1.0-1.noarch.rpm": bothHosted} {
		if w := e.do("PUT", "yum-hosted", p, b); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", p, w.Code, w.Body)
		}
	}
	up := rpmOf("up", "1.0", "upstream")
	e.publish(t, "", []published{
		{"Packages/u/up-1.0-1.noarch.rpm", up},
		{"Packages/b/both-1.0-1.noarch.rpm", bothUp},           // the package hosted has, elsewhere
		{"demo-1.0-1.el9.x86_64.rpm", rpmOf("clash", "1", "")}, // another package at a path of hosted
	})
	repomd, data := e.repodata(t, "yum-merged", "")
	if strings.Contains(repomd, "repodata/repomd") || !strings.Contains(repomd, "<revision>"+strconv.FormatInt(e.now.Unix(), 10)) {
		t.Fatalf("repomd.xml:\n%s", repomd)
	}
	want := []string{"both-1.0-1.noarch.rpm", "demo-1.0-1.el9.x86_64.rpm", "Packages/u/up-1.0-1.noarch.rpm"}
	if got := hrefs(data["primary"]); !slices.Equal(got, want) {
		t.Fatalf("locations: %v, want %v", got, want)
	}
	ids := []string{sum(bothHosted), sum(demoRPM()), sum(up)}
	if !strings.Contains(data["primary"], `<checksum type="sha256" pkgid="YES">`+sum(bothHosted)+"<") || strings.Contains(data["primary"], sum(bothUp)) {
		t.Fatal("the first member does not win the package")
	}
	for typ, doc := range data {
		wellFormed(t, doc)
		if !strings.Contains(doc, `packages="3">`) {
			t.Errorf("%s: not 3 packages:\n%.300s", typ, doc)
		}
		if typ != "primary" && !slices.Equal(pkgids(doc), ids) {
			t.Errorf("%s: pkgids %v, want %v", typ, pkgids(doc), ids)
		}
	}
	if !strings.Contains(data["filelists"], "/usr/bin/demo") || !strings.Contains(data["other"], "- Release 1.0") {
		t.Fatalf("a member's files or changelog:\n%s\n%s", data["filelists"], data["other"])
	}
	// the files it names come from where the index says
	for p, b := range map[string][]byte{"both-1.0-1.noarch.rpm": bothHosted, "Packages/u/up-1.0-1.noarch.rpm": up} {
		if w := e.do("GET", "yum-merged", p, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), b) {
			t.Fatalf("GET %s: %d", p, w.Code)
		}
	}
	// without the setting, as ever: the first member that has the path
	_, data2 := e.repodata(t, "yum-all", "")
	if got := hrefs(data2["primary"]); !slices.Equal(got, want[:2]) {
		t.Fatalf("a group without mergeMembers: %v", got)
	}

	// reused: nothing is merged or asked again while nothing changed
	before := e.do("GET", "yum-merged", repomdFile, nil).Body.String()
	hits := e.hits.Load()
	e.now = e.now.Add(time.Minute)
	if after := e.do("GET", "yum-merged", repomdFile, nil).Body.String(); after != before {
		t.Fatalf("merged again:\n%s\n%s", before, after)
	}
	if e.hits.Load() != hits {
		t.Fatalf("%d more upstream requests", e.hits.Load()-hits)
	}
	// a hosted member's change shows at once
	if w := e.do("PUT", "yum-merged", "new-1-1.noarch.rpm", nvra("new", "1", "1", "noarch")); w.Code != 201 {
		t.Fatalf("PUT through the group: %d %s", w.Code, w.Body)
	}
	_, data = e.repodata(t, "yum-merged", "")
	if got := hrefs(data["primary"]); len(got) != 4 || !slices.Contains(got, "new-1-1.noarch.rpm") {
		t.Fatalf("after an upload: %v", got)
	}
	// a proxy's after its metadataMaxAge, not before
	up2 := rpmOf("up", "2.0", "upstream")
	e.publish(t, "", []published{{"Packages/u/up-1.0-1.noarch.rpm", up}, {"Packages/u/up-2.0-1.noarch.rpm", up2}})
	if _, data = e.repodata(t, "yum-merged", ""); slices.Contains(hrefs(data["primary"]), "Packages/u/up-2.0-1.noarch.rpm") {
		t.Fatal("the proxy's metadataMaxAge did not hold")
	}
	first := e.do("GET", "yum-merged", repomdFile, nil).Body.String()
	e.now = e.now.Add(2 * time.Hour)
	_, data = e.repodata(t, "yum-merged", "")
	if got := hrefs(data["primary"]); !slices.Contains(got, "Packages/u/up-2.0-1.noarch.rpm") || !slices.Contains(got, "demo-1.0-1.el9.x86_64.rpm") {
		t.Fatalf("after the proxy's metadataMaxAge: %v", got)
	}
	// the data files of the previous merge stay, for a client that has just read it; no older ones
	stored, err := e.db.ListAssets(context.Background(), "yum-merged", "repodata/")
	if err != nil || len(stored) != 7 {
		t.Fatalf("assets: %d %v", len(stored), err)
	}
	for _, m := range regexp.MustCompile(`href="([^"]+)"`).FindAllStringSubmatch(first, -1) {
		if w := e.do("GET", "yum-merged", m[1], nil); w.Code != 200 {
			t.Fatalf("the previous merge's %s: %d", m[1], w.Code)
		}
	}
}

// Only the members with the file at that path take part, and a path one
// member has is that member's own: the group does not make the file anew and
// its signature goes through. A merge has no signature.
func TestMergedGroupOnePath(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "yum-deep", "el9/x-1-1.noarch.rpm", nvra("x", "1", "1", "noarch")); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	e.publish(t, "el8/", []published{{"y-1-1.noarch.rpm", nvra("y", "1", "1", "noarch")}})
	e.files.Store("/el8/repodata/repomd.xml.asc", []byte("signature"))
	e.publish(t, "el9/", []published{{"up-1-1.noarch.rpm", nvra("up", "1", "1", "noarch")}})
	e.files.Store("/el9/repodata/repomd.xml.asc", []byte("signature"))

	direct := e.do("GET", "yum-proxy", "el8/"+repomdFile, nil)
	own := e.do("GET", "yum-merged-deep", "el8/"+repomdFile, nil)
	if own.Code != 200 || own.Body.String() != direct.Body.String() {
		t.Fatalf("one member's: %d %s", own.Code, own.Body)
	}
	if w := e.do("GET", "yum-merged-deep", "el8/repodata/repomd.xml.asc", nil); w.Code != 200 {
		t.Fatalf("one member's signature: %d", w.Code)
	}
	// repomd.xml's Rocky-style data files come from the member, by path
	for _, m := range hrefRE.FindAllStringSubmatch(own.Body.String(), -1) {
		if w := e.do("GET", "yum-merged-deep", "el8/"+m[1], nil); w.Code != 200 {
			t.Fatalf("%s: %d", m[1], w.Code)
		}
	}
	_, data := e.repodata(t, "yum-merged-deep", "el9/")
	if got := hrefs(data["primary"]); !slices.Equal(got, []string{"x-1-1.noarch.rpm", "up-1-1.noarch.rpm"}) {
		t.Fatalf("el9: %v", got)
	}
	if w := e.do("GET", "yum-merged-deep", "el9/repodata/repomd.xml.asc", nil); w.Code != 404 {
		t.Fatalf("a member's signature on a merge: %d", w.Code)
	}
	if w := e.do("GET", "yum-merged-deep", "nothing/"+repomdFile, nil); w.Code != 404 {
		t.Fatalf("no member: %d", w.Code)
	}
	if _, ok := e.h.Store.Get(context.Background(), "yum-merged-deep", "el8/"+repomdFile); ok == nil {
		t.Fatal("a merged copy of one member's file")
	}
}

// group, updateinfo and modules come from the first member that lists them;
// a form only that member can answer for (sqlite, zchunk) is not passed on.
func TestMergedGroupCarries(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "yum-hosted", "repodata/comps.xml", []byte(compsDoc)); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	data := func(typ, href string) string {
		return `<data type="` + typ + `"><checksum type="sha256">aa</checksum><location href="` + href + `"/><timestamp>1</timestamp><size>2</size></data>` + "\n"
	}
	e.publish(t, "", []published{{"up-1-1.noarch.rpm", nvra("up", "1", "1", "noarch")}},
		data("group", "repodata/up-comps.xml"), data("group_xz", "repodata/up-comps.xml.xz"), data("updateinfo", "repodata/up-updateinfo.xml.gz"),
		data("modules", "repodata/up-modules.yaml.xz"), data("primary_db", "repodata/up-primary.sqlite.xz"), data("primary_zck", "repodata/up-primary.xml.zck"),
		data("group_zck", "repodata/up-comps.xml.zck"), data("prestodelta", "repodata/up-prestodelta.xml.gz"),
		data("updateinfo_db", "repodata/up-updateinfo.sqlite.xz"))
	e.files.Store("/repodata/up-modules.yaml.xz", []byte("modules"))
	e.files.Store("/repodata/up-comps.xml", []byte("not the first member's"))
	w := e.do("GET", "yum-merged", repomdFile, nil)
	if w.Code != 200 {
		t.Fatalf("GET: %d %s", w.Code, w.Body)
	}
	var types []string
	for _, m := range regexp.MustCompile(`<data type="(\w+)"`).FindAllStringSubmatch(w.Body.String(), -1) {
		types = append(types, m[1])
	}
	slices.Sort(types)
	if !slices.Equal(types, []string{"filelists", "group", "modules", "other", "primary", "updateinfo"}) {
		t.Fatalf("types: %v\n%s", types, w.Body)
	}
	sha := sum([]byte(compsDoc))
	if !strings.Contains(groupData(w.Body.String(), "group"), `href="repodata/`+sha+`-comps.xml"`) {
		t.Fatalf("group:\n%s", w.Body)
	}
	if !strings.Contains(groupData(w.Body.String(), "modules"), `href="repodata/up-modules.yaml.xz"`) {
		t.Fatalf("modules:\n%s", w.Body)
	}
	for p, want := range map[string]string{"repodata/" + sha + "-comps.xml": compsDoc, "repodata/up-modules.yaml.xz": "modules"} {
		if w := e.do("GET", "yum-merged", p, nil); w.Code != 200 || w.Body.String() != want {
			t.Fatalf("GET %s: %d %s", p, w.Code, w.Body)
		}
	}
	// deleting comps.xml takes it out of the merge too, and the next member's shows
	if w := e.do("DELETE", "yum-merged", "repodata/comps.xml", nil); w.Code != 204 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "yum-merged", repomdFile, nil); !strings.Contains(groupData(w.Body.String(), "group"), `href="repodata/up-comps.xml"`) {
		t.Fatalf("after the delete:\n%s", w.Body)
	}
	// what the group could not answer is not passed on
	e.publish(t, "", []published{{"up-1-1.noarch.rpm", nvra("up", "1", "1", "noarch")}},
		data("group", "https://elsewhere.example/comps.xml"), data("updateinfo", "../../updateinfo.xml"))
	e.now = e.now.Add(2 * time.Hour)
	if w := e.do("GET", "yum-merged", repomdFile, nil); strings.Contains(w.Body.String(), "elsewhere") || strings.Contains(w.Body.String(), "updateinfo") {
		t.Fatalf("what the group cannot answer:\n%s", w.Body)
	}
}

// doc0 is a primary.xml of one package, to compress for TestMergeReads.
var doc0 = []byte(`<?xml version="1.0" encoding="UTF-8"?>
<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="1">
<package type="rpm">
  <name>zstded</name>
  <arch>noarch</arch>
  <version epoch="0" ver="1" rel="1"/>
  <checksum type="sha256" pkgid="YES">bbbb</checksum>
  <summary>s</summary>
  <description>d</description>
  <location href="zstded-1-1.noarch.rpm"/>
  <format>
    <rpm:license>MIT</rpm:license>
  </format>
</package>
</metadata>
`)

// What the document may hold and still be merged: other compressions, what the
// schema does not forbid, and hrefs the group can answer. An xml:base goes, as
// the group serves the file.
func TestMergeReads(t *testing.T) {
	unbase := func(s string) string {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// zstandard, a frame of one raw block: the single segment flag, two bytes of size, the block's three
	zst := append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0x60, byte(len(doc0) - 256), byte((len(doc0) - 256) >> 8),
		byte(len(doc0)<<3 | 1), byte(len(doc0) >> 5), byte(len(doc0) >> 13)}, doc0...)
	bz, err := base64.StdEncoding.DecodeString("QlpoOTFBWSZTWc9ZEQcAAEFfgEAQUAPzV4MiDiA/799wMAE6QBqn6U09SaGgDR4RAaHlMgMAaaGjRiMgGgA0MEohBono0yQ0yJoPU9QGRtG4IiKjhBUCjYVOuzNl63OlFtbHcEFZz3uRPR2i9xoCXuGkPGohoxCTTkOoAcSx8J5DcMK7tr4IeyoyYEmhAuFywjlizO0YkcfpqsWtabL1YTStTGy3x8eoyrL0tXRFDslx3Mb8Q4h27JGC19zoMUfZyTIg3Ma9X0hRU+g1OmBMtk5jMRxSXnWYv8j948N1188EjahFTdieZCDCniqn+YD0+DFhKXySGk6qhFaVkiilPtKTKpRHUWcss5DhxkNuiOLVTUmW7svYeMmJB4LMOdECRZLNkOFhYTxaxRSq40HvrEqBYkiJEzDMIDXrkasLh55HgXckU4UJDPWREHA=")
	if err != nil {
		t.Fatal(err)
	}
	pkg := func(name, inner, location string) string {
		return `<package type="rpm"><name>` + name + `</name><arch>noarch</arch><version epoch="0" ver="1" rel="1"/><checksum type="sha256" pkgid="YES">` +
			name + `aa</checksum><summary>s</summary><description>` + inner + `</description>` + location + `<format><rpm:license>MIT</rpm:license></format></package>`
	}
	doc := func(body string) string {
		return `<?xml version="1.0" encoding="UTF-8"?><metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="9">` + body + `</metadata>`
	}
	for name, c := range map[string]struct {
		file string
		want []string // the upstream's hrefs in the merge
	}{
		"bzip2":         {string(bz), []string{"bzipped-1-1.noarch.rpm"}},
		"xz":            {unbase("/Td6WFoAAATm1rRGAgAhARYAAAB0L+Wj4AHtAStdAB4Py4cR2M5mkQ+DHsr9ezPUf+m32igxdiVmIE0qCW1q9ylwODDnzB1WFyLJTqZ9oa333bBqpp2Bla6LTLQcY3rY+z7IhSZgjywiWlBOQXXWA3D4i9YP35FNPsYeAfjU+PLvrU3fh9ofDfX2f4tXu6YGfzEtuxYtc9BwwhFUF2gSPjJIB94wIHr98argeXGFylDiiJJJTu3ul1SBmlxl3vE6tlChOA8MBiNmO4T7uMb5zf7FHpXsKM/Xe5cQ17y752TY8Y5u/SkLP7ialch44Gqgki/fPa/+Zr4e1rKQL2pVDrzzk5Gufwm7fsSbYD/zhdw0EnPZcDIQdLBs1EhHRsX0pOJXQGZJpxmTcpVQIecR3GQhSNOAPbwoyLM7PwGgF+5aBH6BhQslFo6AAAAga0Ae+hktggABxwLuAwAA4yGegbHEZ/sCAAAAAARZWg=="), []string{"xzipped-1-1.noarch.rpm"}},
		"zstd":          {string(zst), []string{"zstded-1-1.noarch.rpm"}},
		"plain":         {doc(pkg("a", "x", `<location href="a-1-1.noarch.rpm"/>`)), []string{"a-1-1.noarch.rpm"}},
		"an empty root": {`<metadata xmlns="http://linux.duke.edu/metadata/common" packages="0"/>`, nil},
		"no packages":   {doc(""), nil},
		"markup in text": {doc(`<!-- <package> --><?pi </metadata> ?>` + pkg("a", `<![CDATA[</package><package><location href="http://x/y"/>]]>&lt;/package&gt;`, `<location href="a-1-1.noarch.rpm"/>`)),
			[]string{"a-1-1.noarch.rpm"}},
		"quotes in a tag": {doc(`<package type="a>b" x='"'><name>a</name><arch>noarch</arch><version epoch="0" ver="1" rel="1"/><checksum>aa</checksum><location xml:base="http://x/" href="a&gt;1-1-1.noarch.rpm"/></package>`),
			[]string{"a&gt;1-1-1.noarch.rpm"}},
		"xml:base":              {doc(pkg("a", "x", `<location xml:base="https://mirror.example/pub" href="Packages/a-1-1.noarch.rpm"/>`)), []string{"Packages/a-1-1.noarch.rpm"}},
		"inside the repository": {doc(pkg("a", "x", `<location href="b/../pool/a-1-1.noarch.rpm"/>`)), []string{"b/../pool/a-1-1.noarch.rpm"}},
		"gzip of gzips":         {string(append(gzipped(doc(pkg("a", "x", `<location href="a-1-1.noarch.rpm"/>`))[:200]), gzipped(doc(pkg("a", "x", `<location href="a-1-1.noarch.rpm"/>`))[200:])...)), []string{"a-1-1.noarch.rpm"}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			if w := e.do("PUT", "yum-hosted", "x-1-1.noarch.rpm", nvra("x", "1", "1", "noarch")); w.Code != 201 {
				t.Fatalf("PUT: %d %s", w.Code, w.Body)
			}
			e.publishPrimary("", []byte(c.file))
			w := e.do("GET", "yum-merged", repomdFile, nil)
			if w.Code != 200 {
				t.Fatalf("GET: %d %s", w.Code, w.Body)
			}
			p := regexp.MustCompile(`href="(repodata/\w+-primary.xml.gz)"`).FindStringSubmatch(w.Body.String())
			z, err := gzip.NewReader(e.do("GET", "yum-merged", p[1], nil).Body)
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(z)
			if err != nil {
				t.Fatal(err)
			}
			wellFormed(t, string(b))
			var got []string
			for _, m := range regexp.MustCompile(`<location[^>]*href="([^"]+)"`).FindAllStringSubmatch(string(b), -1) {
				if m[1] != "x-1-1.noarch.rpm" {
					got = append(got, m[1])
				}
			}
			if !slices.Equal(got, c.want) && !strings.Contains(name, "markup") {
				t.Fatalf("hrefs: %v, want %v\n%s", got, c.want, b)
			}
			if strings.Contains(string(b), "mirror.example") || strings.Contains(string(b), "xml:base") {
				t.Fatalf("xml:base left:\n%s", b)
			}
		})
	}
}

// A merged index is complete or it is not served: whatever cannot be read
// fails the merge, 502 without a merged copy, the previous copy (marked stale)
// with one. A failed merge is not tried again at once. Each case is a member
// that could have been left out, with the other's packages served alone.
func TestMergeRefuses(t *testing.T) {
	good := gzipped(Primary([]*Package{{Name: "a", Arch: "noarch", Epoch: "0", Version: "1", Release: "1", Checksum: "aa", Location: "a-1-1.noarch.rpm"}}))
	corrupt := slices.Clone(good)
	corrupt[len(corrupt)-5] ^= 0xff // the gzip checksum
	pkg := func(location string) string {
		return `<package type="rpm"><name>a</name><arch>noarch</arch><version epoch="0" ver="1" rel="1"/><checksum>aa</checksum>` + location + `</package>`
	}
	doc := func(body string) []byte {
		return []byte(`<metadata xmlns="http://linux.duke.edu/metadata/common">` + body + `</metadata>`)
	}
	bigWindow, err := base64.StdEncoding.DecodeString("/Td6WFoAAATm1rRGAgAhARwAAAAQz1jM4AHtAStdAB4Py4cR2M5mkQ+DHsr9ezPUf+m32igxdiVmIE0qCW1q9ylwODDnzB1WFyLJTqZ9oa333bBqpp2Bla6LTLQcY3rY+z7IhSZgjywiWlBOQXXWA3D4i9YP35FNPsYeAfjU+PLvrU3fh9ofDfX2f4tXu6YGfzEtuxYtc9BwwhFUF2gSPjJIB94wIHr98argeXGFylDiiJJJTu3ul1SBmlxl3vE6tlChOA8MBiNmO4T7uMb5zf7FHpXsKM/Xe5cQ17y752TY8Y5u/SkLP7ialch44Gqgki/fPa/+Zr4e1rKQL2pVDrzzk5Gufwm7fsSbYD/zhdw0EnPZcDIQdLBs1EhHRsX0pOJXQGZJpxmTcpVQIecR3GQhSNOAPbwoyLM7PwGgF+5aBH6BhQslFo6AAAAga0Ae+hktggABxwLuAwAA4yGegbHEZ/sCAAAAAARZWg==") // xz -9
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"corrupt zstd":             {0x28, 0xb5, 0x2f, 0xfd, 1, 2, 3, 4, 5, 6},
		"corrupt xz":               {0xfd, '7', 'z', 'X', 'Z', 0, 1, 2, 3, 4},
		"a window too large":       bigWindow,
		"not a document":           []byte("hello"),
		"truncated gzip":           good[:len(good)/2],
		"corrupt gzip":             corrupt,
		"truncated document":       doc(pkg(`<location href="a.rpm"/>`))[:90],
		"an absolute url":          doc(pkg(`<location href="https://elsewhere.example/a.rpm"/>`)),
		"an absolute path":         doc(pkg(`<location href="/a.rpm"/>`)),
		"above the repository":     doc(pkg(`<location href="../../a.rpm"/>`)),
		"an escaped url":           doc(pkg(`<location href="https:&#47;&#47;elsewhere.example/a.rpm"/>`)),
		"no location":              doc(pkg(``)),
		"no checksum":              doc(`<package><name>a</name><arch>noarch</arch><version ver="1"/><location href="a.rpm"/></package>`),
		"a later location":         doc(`<package><name>a</name><arch>noarch</arch><version ver="1"/><checksum>aa</checksum><location href="a.rpm"/><format/><location href="https://elsewhere.example/a.rpm"/></package>`),
		"a package in one":         doc(`<package><name>a</name><package>` + pkg(`<location href="a.rpm"/>`) + `</package></package>`),
		"another namespace":        []byte(`<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:suse="http://novell.com/package/metadata/suse/common">` + pkg(`<location href="a.rpm"/>`) + `</metadata>`),
		"a package head too large": doc(`<package><name>a</name><description>` + strings.Repeat("x", maxHead) + `</description></package>`),
		"a tag too large":          doc(`<package x="` + strings.Repeat("x", maxTag) + `"></package>`),
	}
	e := newEnv(t)
	i := 0
	for name, file := range cases {
		i++
		t.Run(name, func(t *testing.T) {
			dir := fmt.Sprintf("d%d/", i)
			if w := e.do("PUT", "yum-deep", dir+"x-1-1.noarch.rpm", nvra("x", "1", "1", "noarch")); w.Code != 201 {
				t.Fatalf("PUT: %d %s", w.Code, w.Body)
			}
			e.publishPrimary(dir, file)
			if w := e.do("GET", "yum-merged-deep", dir+repomdFile, nil); w.Code != 502 || strings.Contains(w.Body.String(), "repomd") {
				t.Fatalf("GET: %d %s", w.Code, w.Body)
			}
			if a, err := e.h.Store.Get(context.Background(), "yum-merged-deep", dir+repomdFile); err == nil {
				t.Fatalf("a merged copy: %+v", a)
			}
		})
	}
	// a repomd.xml that is no such thing, a member's file that is not what it says
	e.files.Store("/d-bad/"+repomdFile, []byte("not xml <"))
	if w := e.do("PUT", "yum-deep", "d-bad/x-1-1.noarch.rpm", nvra("x", "1", "1", "noarch")); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "yum-merged-deep", "d-bad/"+repomdFile, nil); w.Code != 502 {
		t.Fatalf("a bad repomd.xml: %d %s", w.Code, w.Body)
	}
	e.publishPrimary("d-sum/", good)
	rd := repoData{typ: "primary", sum: sum(good)}
	e.files.Store("/d-sum/"+rd.file(), []byte("other bytes"))
	if w := e.do("PUT", "yum-deep", "d-sum/x-1-1.noarch.rpm", nvra("x", "1", "1", "noarch")); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "yum-merged-deep", "d-sum/"+repomdFile, nil); w.Code != 502 {
		t.Fatalf("a file that is not what repomd.xml says: %d %s", w.Code, w.Body)
	}
}

// A merged copy outlives what would fail the next merge, and the merge is
// tried again, not at every request.
func TestMergeFallsBack(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "yum-hosted", "x-1-1.noarch.rpm", nvra("x", "1", "1", "noarch")); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	e.publish(t, "", []published{{"up-1-1.noarch.rpm", nvra("up", "1", "1", "noarch")}})
	good := e.do("GET", "yum-merged", repomdFile, nil)
	if good.Code != 200 || good.Header().Get("Warning") != "" {
		t.Fatalf("GET: %d %v", good.Code, good.Header())
	}
	// the remote breaks: the merge fails, the copy stands
	e.publishPrimary("", []byte{0x28, 0xb5, 0x2f, 0xfd, 1, 2, 3, 4, 5, 6})
	e.now = e.now.Add(2 * time.Hour)
	w := e.do("GET", "yum-merged", repomdFile, nil)
	if w.Code != 200 || w.Body.String() != good.Body.String() || !strings.Contains(w.Header().Get("Warning"), "Stale") {
		t.Fatalf("stale: %d %v\n%s", w.Code, w.Header(), w.Body)
	}
	for _, m := range hrefRE.FindAllStringSubmatch(good.Body.String(), -1) {
		if w := e.do("GET", "yum-merged", m[1], nil); w.Code != 200 {
			t.Fatalf("%s: %d", m[1], w.Code)
		}
	}
	key := "yum-merged\x00"
	failedAt := e.h.failed[key].at
	if failedAt.IsZero() {
		t.Fatalf("no failure noted: %v", e.h.failed)
	}
	// not tried again within mergeRetry, tried again after it
	e.now = e.now.Add(mergeRetry / 2)
	if w := e.do("GET", "yum-merged", repomdFile, nil); w.Code != 200 || e.h.failed[key].at != failedAt {
		t.Fatalf("tried again at once: %d", w.Code)
	}
	e.now = e.now.Add(mergeRetry)
	if w := e.do("GET", "yum-merged", repomdFile, nil); w.Code != 200 || !e.h.failed[key].at.After(failedAt) {
		t.Fatalf("not tried again: %d", w.Code)
	}
	// and it mends: the same members' change wipes the failure
	e.publish(t, "", []published{{"up-1-1.noarch.rpm", nvra("up", "1", "1", "noarch")}, {"up-2-1.noarch.rpm", nvra("up", "2", "1", "noarch")}})
	e.now = e.now.Add(2 * time.Hour)
	_, data := e.repodata(t, "yum-merged", "")
	if got := hrefs(data["primary"]); len(got) != 3 {
		t.Fatalf("mended: %v", got)
	}
	if _, ok := e.h.failed[key]; ok {
		t.Fatalf("failure kept: %v", e.h.failed)
	}
	if w := e.do("GET", "yum-merged", repomdFile, nil); w.Header().Get("Warning") != "" {
		t.Fatalf("still stale: %v", w.Header())
	}
}

// A member that fails is not left out: never cached, its remote down or
// refusing, there is no complete index to give.
func TestMergeMemberFails(t *testing.T) {
	e := newEnv(t)
	for _, dir := range []string{"fail/", "denied/"} {
		if w := e.do("PUT", "yum-deep", dir+"x-1-1.noarch.rpm", nvra("x", "1", "1", "noarch")); w.Code != 201 {
			t.Fatalf("PUT: %d %s", w.Code, w.Body)
		}
		if w := e.do("GET", "yum-merged-deep", dir+repomdFile, nil); w.Code != 502 {
			t.Fatalf("GET: %d %s", w.Code, w.Body)
		}
	}
	// offline it is not a member at all, as in any group
	offline := false
	e.repos["yum-proxy"].Online = &offline
	w := e.do("GET", "yum-merged-deep", "fail/"+repomdFile, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<repomd") {
		t.Fatalf("offline: %d %s", w.Code, w.Body)
	}
	if direct := e.do("GET", "yum-deep", "fail/"+repomdFile, nil); direct.Body.String() != w.Body.String() {
		t.Fatalf("not the member's own:\n%s", w.Body)
	}
}

// Requests at once, on two nodes: one merge. Two handlers share the database
// and blob store as nodes do; every merge would have another revision.
func TestMergeOnce(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "yum-hosted", "x-1-1.noarch.rpm", nvra("x", "1", "1", "noarch")); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	e.publish(t, "", []published{{"up-1-1.noarch.rpm", nvra("up", "1", "1", "noarch")}})
	var tick atomic.Int64
	now := func() time.Time { return e.now.Add(time.Duration(tick.Add(1)) * time.Second) }
	nodes := []*Handler{New(e.h.Store, e.h.Resolver, e.h.Auth), New(e.h.Store, &resolve.Resolver{Store: e.h.Store, Fetcher: e.h.Resolver.Fetcher}, e.h.Auth)}
	var wg sync.WaitGroup
	got := make([]string, 8)
	for _, h := range nodes {
		h.Now = now
	}
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := nodes[i%2]
			r := httptest.NewRequest("GET", "/repository/yum-merged/"+repomdFile, nil)
			r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{}))
			w := httptest.NewRecorder()
			h.Serve(w, r, e.repos["yum-merged"], repomdFile)
			if w.Code == 200 {
				got[i] = w.Body.String()
			}
		}()
	}
	wg.Wait()
	for i := range got {
		if got[i] == "" || got[i] != got[0] {
			t.Fatalf("answers differ:\n%s\n%s", got[0], got[i])
		}
	}
}

// A group inside a merging group counts by its members, each once.
func TestMergeNestedGroup(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "yum-hosted", "x-1-1.noarch.rpm", nvra("x", "1", "1", "noarch")); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	e.publish(t, "", []published{{"up-1-1.noarch.rpm", nvra("up", "1", "1", "noarch")}})
	_, data := e.repodata(t, "yum-outer", "")
	if got := hrefs(data["primary"]); !slices.Equal(got, []string{"x-1-1.noarch.rpm", "up-1-1.noarch.rpm"}) {
		t.Fatalf("locations: %v", got)
	}
}

// A request does not wait for long when it has a merged copy to give, and the
// merge goes on without the request that started it.
func TestMergeDoesNotHoldRequests(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "yum-hosted", "x-1-1.noarch.rpm", nvra("x", "1", "1", "noarch")); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	e.publish(t, "", []published{{"up-1-1.noarch.rpm", nvra("up", "1", "1", "noarch")}})
	busy := func() {
		for range mergeWorkers {
			e.h.mergeSlots <- struct{}{}
		}
	}
	free := func() {
		for range mergeWorkers {
			<-e.h.mergeSlots
		}
	}
	// the first request waits with nothing to give; it goes away, the merge does not
	busy()
	ctx, cancel := context.WithTimeout(auth.WithPrincipal(context.Background(), auth.Principal{}), 50*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("GET", "/repository/yum-merged/"+repomdFile, nil).WithContext(ctx)
	e.h.Serve(httptest.NewRecorder(), r, e.repos["yum-merged"], repomdFile)
	free()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := e.h.Store.Get(context.Background(), "yum-merged", repomdFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the merge did not finish without its request")
		}
		time.Sleep(10 * time.Millisecond)
	}
	first := e.do("GET", "yum-merged", repomdFile, nil)
	if first.Code != 200 || first.Header().Get("Warning") != "" {
		t.Fatalf("GET: %d %v %s", first.Code, first.Header(), first.Body)
	}
	// with a merged copy it gets that when the next merge takes too long
	busy()
	defer func(d time.Duration) { mergeWait = d }(mergeWait)
	mergeWait = 10 * time.Millisecond
	if w := e.do("PUT", "yum-hosted", "y-1-1.noarch.rpm", nvra("y", "1", "1", "noarch")); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	w := e.do("GET", "yum-merged", repomdFile, nil)
	if w.Code != 200 || w.Body.String() != first.Body.String() || !strings.Contains(w.Header().Get("Warning"), "Stale") {
		t.Fatalf("while merging: %d %v", w.Code, w.Header())
	}
	free()
	mergeWait = time.Minute
	_, data := e.repodata(t, "yum-merged", "")
	if got := hrefs(data["primary"]); len(got) != 3 {
		t.Fatalf("after the merge: %v", got)
	}
}

// syntheticPrimary is a primary.xml generated as it is read: packages small
// packages, and every each-th of them holding kernel-like provides of about
// perHuge bytes.
func syntheticPrimary(packages, each, perHuge int) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		w := bufio.NewWriterSize(pw, 64<<10)
		w.WriteString(docStart("primary", packages))
		entries := strings.Repeat(`      <rpm:entry name="kernel(a_symbol_that_a_kernel_provides)" flags="EQ" epoch="0" ver="0x12345678"/>`+"\n", 650) // 64 KiB or so
		for i := range packages {
			fmt.Fprintf(w, `<package type="rpm"><name>p%d</name><arch>x86_64</arch><version epoch="0" ver="1" rel="1"/><checksum type="sha256" pkgid="YES">%064d</checksum>`, i, i)
			fmt.Fprintf(w, "<summary>s</summary><description>d</description><time file=\"1\" build=\"1\"/><size package=\"1\" installed=\"1\" archive=\"1\"/><location href=\"Packages/p%d-1-1.x86_64.rpm\"/><format><rpm:provides>\n", i)
			if i%each == 0 {
				for n := 0; n < perHuge; n += len(entries) {
					w.WriteString(entries)
				}
			}
			w.WriteString("</rpm:provides></format></package>\n")
		}
		w.WriteString("</metadata>\n")
		pw.CloseWithError(w.Flush())
	}()
	return pr
}

// peakHeap runs f and returns how much the heap grew at most meanwhile.
func peakHeap(t *testing.T, f func()) uint64 {
	t.Helper()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base, peak := ms.HeapAlloc, ms.HeapAlloc
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				peak = max(peak, ms.HeapAlloc)
			}
		}
	}()
	f()
	close(stop)
	<-stopped
	return peak - base
}

// A merge holds no package and no document: a primary.xml many times what it
// may use, with packages larger than all of that, is read and copied in the
// memory of a few buffers.
func TestMergeStreams(t *testing.T) {
	const packages, each, perHuge, limit = 40_000, 10_000, 48 << 20, 24 << 20
	var size countWriter
	var seen int
	peak := peakHeap(t, func() {
		err := scanPrimary(syntheticPrimary(packages, each, perHuge), func(_ int, p *primaryHead) error {
			seen++
			if p.Name == "" || p.Location.Href == "" {
				return fmt.Errorf("package %d: %+v", seen, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = copyPackages(syntheticPrimary(packages, each, perHuge), &size, "primary", "", func(int, []byte) (bool, error) { return false, nil })
		if err != nil {
			t.Fatal(err)
		}
	})
	if seen != packages || size.n < 4*perHuge {
		t.Fatalf("%d packages, %d bytes copied", seen, size.n)
	}
	t.Logf("%d packages, %d MiB, peak heap growth %.1f MiB", seen, size.n>>20, float64(peak)/(1<<20))
	if peak > limit {
		t.Fatalf("the heap grew by %.1f MiB, more than the %d MiB that shows it holds no package", float64(peak)/(1<<20), limit>>20)
	}
}

// A real merge's documents, end to end in the same memory: many packages of
// two members through the whole handler, with duplicates.
func TestMergeManyPackages(t *testing.T) {
	const packages = 20_000
	e := newEnv(t)
	if w := e.do("PUT", "yum-hosted", "x-1-1.noarch.rpm", nvra("x", "1", "1", "noarch")); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	raw, err := io.ReadAll(syntheticPrimary(packages, packages, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	e.publishPrimary("", gzipped(string(raw)))
	peak := peakHeap(t, func() {
		if w := e.do("GET", "yum-merged", repomdFile, nil); w.Code != 200 {
			t.Errorf("GET: %d %s", w.Code, w.Body)
		}
	})
	t.Logf("%d packages, peak heap growth %.1f MiB", packages, float64(peak)/(1<<20))
	if peak > 40<<20 {
		t.Fatalf("the heap grew by %.1f MiB", float64(peak)/(1<<20))
	}
}
