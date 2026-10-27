package maven

import (
	"context"
	"fmt"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/repo"
)

func TestCheckPOM(t *testing.T) {
	rel := &Coordinates{GroupID: "org.example", ArtifactID: "pv", BaseVersion: "1.1", Version: "1.1", Extension: "pom"}
	snap := &Coordinates{GroupID: "org.example", ArtifactID: "pv", BaseVersion: "1.1-SNAPSHOT", Version: "1.1-20261026.120000-1", Extension: "pom"}
	accent := &Coordinates{GroupID: "org.example", ArtifactID: "pv", BaseVersion: "1.1-\u00e9", Version: "1.1-\u00e9", Extension: "pom"}
	pom := func(body string) string {
		return "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<project xmlns=\"http://maven.apache.org/POM/4.0.0\">" + body + "</project>\n"
	}
	const gav = "<groupId>org.example</groupId><artifactId>pv</artifactId><version>1.1</version>"
	nest := func(n int) string { return strings.Repeat("<a>", n) + strings.Repeat("</a>", n) }
	bomb := "<groupId>${a0}</groupId><artifactId>pv</artifactId><version>1.1</version><properties>"
	for i := range 10 {
		bomb += "<a" + string(rune('0'+i)) + ">" + strings.Repeat("${a"+string(rune('1'+i))+"}", 10) + "</a" + string(rune('0'+i)) + ">"
	}
	bomb += "</properties>"
	var many strings.Builder // maxPOMValues+10 properties: the last ones are not kept
	for i := range maxPOMValues + 10 {
		fmt.Fprintf(&many, "<p%d>9.9</p%d>", i, i)
	}
	for _, c := range []struct {
		name, doc string
		c         *Coordinates
		err       string // "" when the POM is accepted
	}{
		{"plain", pom("<modelVersion>4.0.0</modelVersion>" + gav), rel, ""},
		{"parent", pom("<parent><groupId>org.example</groupId><artifactId>parent</artifactId><version>1.1</version></parent><artifactId>pv</artifactId>"), rel, ""},
		{"own over parent", pom("<parent><groupId>org.parent</groupId><version>9</version></parent>" + gav), rel, ""},
		{"trimmed, split", pom("<groupId>\n  org.example\n</groupId><artifactId>p<!-- x -->v</artifactId><version><![CDATA[1.1]]></version>"), rel, ""},
		{"property", pom("<groupId>org.example</groupId><artifactId>pv</artifactId><version>${revision}</version><properties><revision>1.1</revision></properties>"), rel, ""},
		{"nested properties", pom("<groupId>${g}</groupId><artifactId>pv</artifactId><version>${revision}${changelist}</version>" +
			"<properties><revision>${major}.${minor}</revision><major>1</major><minor>1</minor><changelist/><g>org.example</g></properties>"), rel, ""},
		{"-Drevision over the default", pom("<groupId>org.example</groupId><artifactId>pv</artifactId><version>${revision}</version><properties><revision>1.0-SNAPSHOT</revision></properties>"), rel, ""},
		{"unresolved version", pom("<groupId>org.example</groupId><artifactId>pv</artifactId><version>${revision}</version>"), rel, ""},
		{"property past the cap", pom("<groupId>org.example</groupId><artifactId>pv</artifactId><version>${p1005}</version><properties>" + many.String() + "</properties>"), rel, ""},
		{"coordinates after the cap", pom("<properties>" + many.String() + "</properties>" + gav), rel, ""},
		{"parent's properties", pom("<groupId>${g}</groupId><artifactId>p${suffix}</artifactId><version>1.1</version>"), rel, ""},
		{"unresolved parent version", pom("<parent><groupId>org.example</groupId><version>${revision}</version></parent><artifactId>pv</artifactId>"), rel, ""},
		{"snapshot", pom("<groupId>org.example</groupId><artifactId>pv</artifactId><version>1.1-SNAPSHOT</version>"), snap, ""},
		{"timestamped snapshot", pom("<groupId>org.example</groupId><artifactId>pv</artifactId><version>1.1-20261026.120000-1</version>"), snap, ""},
		{"bom, latin1", "\xef\xbb\xbf<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><project><name>Jos\xe9</name>" + gav + "</project>", rel, ""},
		{"euc-kr", "<?xml version=\"1.0\" encoding=\"EUC-KR\"?><project><name>\xc7\xd1\xb1\xdb</name>" + gav + "</project>", rel, ""},
		{"latin1 version", "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><project><groupId>org.example</groupId><artifactId>pv</artifactId><version>1.1-\xe9</version></project>", accent, ""},
		{"text not UTF-8", "<project><name>\xc7\xd1</name>" + gav + "</project>", rel, ""},
		{"prolog, epilog", "<?xml version=\"1.0\"?>\n<!-- license -->\n<!DOCTYPE project>\n<project>" + gav + "</project>\n<!-- end -->\n", rel, ""},
		{"deepest", "<project>" + gav + nest(maxPOMDepth-1) + "</project>", rel, ""},

		{"text", "data", rel, "invalid .pom: text outside the <project> element"},
		{"empty", "", rel, "invalid .pom: no <project> element"},
		{"cut short", "<project>" + gav, rel, "invalid .pom: XML syntax error on line 1: unexpected EOF"},
		{"other root", "<metadata>" + gav + "</metadata>", rel, "invalid .pom: want one <project> root element, found <metadata>"},
		{"two roots", "<project>" + gav + "</project><project/>", rel, "invalid .pom: want one <project> root element, found <project>"},
		{"trailing text", pom(gav) + "junk", rel, "invalid .pom: text outside the <project> element"},
		{"not UTF-8, cut short", "<project><name>\xc7\xd1</name>" + gav, rel, "invalid .pom: XML syntax error on line 1: unexpected EOF"},
		{"coordinate not UTF-8", pom("<groupId>org.example</groupId><artifactId>p\xffv</artifactId><version>1.1</version>"), rel, "declares \"org.example:p\ufffdv:1.1\""},
		{"too deep", "<project>" + gav + nest(maxPOMDepth) + "</project>", rel, "invalid .pom: elements nest more than 1000 deep"},
		{"QA N-6", pom("<groupId>evil</groupId><artifactId>other</artifactId><version>9</version>"), rel, `.pom declares "evil:other:9", want "org.example:pv:1.1"`},
		{"groupId", pom("<groupId>org.other</groupId><artifactId>pv</artifactId><version>1.1</version>"), rel, `declares "org.other:pv:1.1"`},
		{"artifactId", pom("<groupId>org.example</groupId><artifactId>pw</artifactId><version>1.1</version>"), rel, `declares "org.example:pw:1.1"`},
		{"version", pom("<groupId>org.example</groupId><artifactId>pv</artifactId><version>1.2</version>"), rel, `declares "org.example:pv:1.2"`},
		{"no coordinates", "<project/>", rel, `declares "::"`},
		{"literal part", pom("<groupId>${g}</groupId><artifactId>pw${suffix}</artifactId><version>1.1</version>"), rel, `declares "${g}:pw${suffix}:1.1"`},
		{"property bomb", pom(bomb), rel, `.pom declares "${a2}${a2}`}, // expansion stops past 256 bytes
	} {
		err := checkPOM(strings.NewReader(c.doc), c.c)
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.err != "" && (repo.StatusOf(err) != 400 || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: %v (status %d), want 400 %q", c.name, err, repo.StatusOf(err), c.err)
		}
	}
}

// QA N-6: a .pom is checked on disk before it is recorded. Text and POMs
// naming other coordinates get 400 and one over maxPOMSize 413, leaving
// nothing behind; an attached POM (with a classifier) is not checked.
func TestPutPOMChecked(t *testing.T) {
	e := newEnv(t)
	const path = "org/example/pv/1.1/pv-1.1.pom"
	for _, body := range []string{"data", "<project><groupId>evil</groupId><artifactId>other</artifactId><version>9</version></project>"} {
		if w := e.do("PUT", "releases", path, body); w.Code != 400 {
			t.Fatalf("PUT %q: %d %s", body, w.Code, w.Body)
		}
	}
	pad := func(p string, size int) string { return fileBody(p) + strings.Repeat(" ", size-len(fileBody(p))) }
	if w := e.do("PUT", "releases", path, pad(path, maxPOMSize+1)); w.Code != 413 {
		t.Fatalf("PUT over the cap: %d %s", w.Code, w.Body)
	}
	for _, p := range []string{path, "org/example/pv/maven-metadata.xml"} {
		if w := e.do("GET", "releases", p, ""); w.Code != 404 {
			t.Fatalf("GET %s after refused PUTs: %d", p, w.Code)
		}
	}
	// allow_once: the refused uploads did not take the path.
	if w := e.do("PUT", "releases", path, pad(path, maxPOMSize)); w.Code != 201 {
		t.Fatalf("PUT at the cap: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "releases", "org/example/pv/1.1/pv-1.1-site.pom", "not checked"); w.Code != 201 {
		t.Fatalf("PUT with a classifier: %d %s", w.Code, w.Body)
	}
	if n := len(pomSem); n != 0 {
		t.Fatalf("%d POM parse slots still taken", n)
	}
}

// A Scala module may name its artifactId with a property only its parent
// defines, as Apache Hudi's hudi-spark_2.12 POM on Maven Central does. Maven
// only warns about that expression and deploys the POM as written, so the
// reference stands for any text there; the rest must still match.
func TestPutPOMArtifactIDFromParentProperty(t *testing.T) {
	e := newEnv(t)
	const path = "org/apache/hudi/hudi-spark_2.12/0.14.0/hudi-spark_2.12-0.14.0.pom"
	pom := `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
	<modelVersion>4.0.0</modelVersion>
	<parent>
		<groupId>org.apache.hudi</groupId>
		<artifactId>hudi-spark-datasource</artifactId>
		<version>0.14.0</version>
	</parent>
	<artifactId>hudi-spark_${scala.binary.version}</artifactId>
	<properties>
		<main.basedir>${project.parent.parent.basedir}</main.basedir>
	</properties>
</project>
`
	if w := e.do("PUT", "releases", path, pom); w.Code != 201 {
		t.Fatalf("PUT %s: %d %s", path, w.Code, w.Body)
	}
}

// Only properties count against maxPOMValues: a POM whose 1000 properties (a
// large platform BOM's) come before its coordinates keeps them.
func TestPutPOMPropertiesBeforeCoordinates(t *testing.T) {
	e := newEnv(t)
	const path = "org/example/platform-bom/1.0/platform-bom-1.0.pom"
	var props strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&props, "<lib%d.version>1.%d</lib%d.version>", i, i, i)
	}
	pom := `<project><modelVersion>4.0.0</modelVersion><properties>` + props.String() +
		`</properties><groupId>org.example</groupId><artifactId>platform-bom</artifactId><version>1.0</version><packaging>pom</packaging></project>`
	if w := e.do("PUT", "releases", path, pom); w.Code != 201 {
		t.Fatalf("PUT %s: %d %s", path, w.Code, w.Body)
	}
}

// A POM declared UTF-8 whose <name> was saved in a legacy code page (MS949
// here) builds with Maven, which reads those bytes as U+FFFD; its ASCII
// coordinates name its path, so it is accepted.
func TestPutPOMLegacyCodepageText(t *testing.T) {
	e := newEnv(t)
	const path = "kr/example/app/1.0/app-1.0.pom"
	pom := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<project><modelVersion>4.0.0</modelVersion>" +
		"<groupId>kr.example</groupId><artifactId>app</artifactId><version>1.0</version>" +
		"<name>\xc7\xd1\xb1\xdb \xbe\xdb</name></project>\n" // "한글 앱" in MS949
	if w := e.do("PUT", "releases", path, pom); w.Code != 201 {
		t.Fatalf("PUT %s: %d %s", path, w.Code, w.Body)
	}
}

// POMs are parsed in at most cap(pomSem) slots: an upload waits for one.
func TestPOMParseSlots(t *testing.T) {
	e := newEnv(t)
	const path = "org/example/pv/1.1/pv-1.1.pom"
	for range cap(pomSem) {
		pomSem <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w := httptest.NewRecorder()
	e.h.Serve(w, httptest.NewRequestWithContext(ctx, "PUT", "/repository/releases/"+path, strings.NewReader(fileBody(path))), e.repos["releases"], path)
	for range cap(pomSem) {
		<-pomSem
	}
	if w.Code == 201 {
		t.Fatal("POM recorded while every parse slot was taken")
	}
	e.putAll(t, "releases", path)
}

// ReadPOMInfo: the "${name}" map is built once, not per dependency, so a
// POM with 1000 properties and 2000 dependencies costs a few MiB, not
// hundreds.
func TestReadPOMInfoAllocs(t *testing.T) {
	var b strings.Builder
	b.WriteString("<project><properties>")
	for i := range 1000 {
		fmt.Fprintf(&b, "<p%d>v</p%d>", i, i)
	}
	b.WriteString("</properties><dependencies>")
	for i := range 2000 {
		fmt.Fprintf(&b, "<dependency><groupId>${p1}</groupId><artifactId>a%d</artifactId><version>${p2}</version></dependency>", i)
	}
	b.WriteString("</dependencies></project>")
	pom := b.String()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	info := ReadPOMInfo(strings.NewReader(pom))
	runtime.ReadMemStats(&after)
	mb := float64(after.TotalAlloc-before.TotalAlloc) / (1 << 20)
	t.Logf("pom %d bytes: %.1f MiB allocated in one ReadPOMInfo", len(pom), mb)
	if mb > 50 {
		t.Fatalf("%.0f MiB allocated for a %d byte POM", mb, len(pom))
	}
	if len(info.Dependencies) != 2000 || info.Dependencies[1999].Version != "v" {
		t.Fatalf("deps %d, last %+v", len(info.Dependencies), info.Dependencies[len(info.Dependencies)-1])
	}
}

// ReadPOMInfo: an element nested deeper than maxPOMDepth is skipped but its
// end is matched, so what follows it is still read.
func TestReadPOMInfoDeepNesting(t *testing.T) {
	var b strings.Builder
	b.WriteString("<project><build>")
	for range 1100 {
		b.WriteString("<x>")
	}
	for range 1100 {
		b.WriteString("</x>")
	}
	b.WriteString("</build><description>desc</description><dependencies><dependency><groupId>g</groupId><artifactId>a</artifactId><version>1</version></dependency></dependencies></project>")
	info := ReadPOMInfo(strings.NewReader(b.String()))
	if info.Description != "desc" || len(info.Dependencies) != 1 {
		t.Fatalf("after a 1100-deep element: description %q, deps %d (want desc, 1)", info.Description, len(info.Dependencies))
	}
}

// ReadPOMInfo: ${project.*}, ${pom.*} and bare ${version} come from the
// project's own coordinates, the parent's when the project omits them.
func TestReadPOMInfoProjectCoordinates(t *testing.T) {
	for _, c := range []struct{ name, pom, group, version string }{
		{"own", `<project><groupId>g</groupId><artifactId>parent</artifactId><version>2.5</version>
<dependencies><dependency><groupId>${project.groupId}</groupId><artifactId>sibling</artifactId><version>${project.version}</version></dependency></dependencies></project>`, "g", "2.5"},
		{"from parent", `<project><parent><groupId>pg</groupId><artifactId>p</artifactId><version>7</version></parent><artifactId>child</artifactId>
<dependencies><dependency><groupId>${pom.groupId}</groupId><artifactId>sibling</artifactId><version>${version}</version></dependency></dependencies></project>`, "pg", "7"},
		{"over property", `<project><groupId>g</groupId><artifactId>a</artifactId><version>1</version><properties><project.version>9</project.version></properties>
<dependencies><dependency><groupId>${project.groupId}</groupId><artifactId>sibling</artifactId><version>${project.version}</version></dependency></dependencies></project>`, "g", "1"},
	} {
		info := ReadPOMInfo(strings.NewReader(c.pom))
		if len(info.Dependencies) != 1 || info.Dependencies[0].GroupID != c.group || info.Dependencies[0].Version != c.version {
			t.Fatalf("%s: deps %+v (want %s:sibling:%s)", c.name, info.Dependencies, c.group, c.version)
		}
	}
}
