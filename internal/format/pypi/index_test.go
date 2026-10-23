package pypi

import (
	"bufio"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

var base, _ = url.Parse("https://pypi.example/simple/six/")

const jsonPage = `{"meta":{"api-version":"1.1"},"name":"six","versions":["1.16.0"],
 "files":[
  {"filename":"six-1.16.0-py2.py3-none-any.whl","url":"https://files.example/packages/ab/six-1.16.0-py2.py3-none-any.whl","hashes":{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"requires-python":">=2.7","yanked":false,"core-metadata":{"sha256":"x"}},
  {"filename":"six-1.15.0.tar.gz","url":"../../files/six-1.15.0.tar.gz","hashes":{},"yanked":"broken build"}
 ]}`

const htmlPage = `<!DOCTYPE html><html><body><h1>Links for six</h1>
<a href="https://files.example/six-1.16.0.tar.gz#sha256=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" data-requires-python="&gt;=2.7, !=3.0.*">six-1.16.0.tar.gz</a><br/>
<A HREF='../../files/six-1.14.0.tar.gz' data-yanked="">six-1.14.0.tar.gz</A><br/>
<a href="/bad">../evil</a>
</body></html>`

func collectFiles(t *testing.T, page, ct string) []File {
	t.Helper()
	var out []File
	if err := parseIndex(strings.NewReader(page), ct, base, func(f File) error { out = append(out, f); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestParseJSONIndex(t *testing.T) {
	files := collectFiles(t, jsonPage, "application/vnd.pypi.simple.v1+json")
	if len(files) != 2 {
		t.Fatalf("files = %+v", files)
	}
	f := files[0]
	if f.Filename != "six-1.16.0-py2.py3-none-any.whl" || f.SHA256 != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || f.RequiresPython != ">=2.7" || f.Yanked ||
		f.URL != "https://files.example/packages/ab/six-1.16.0-py2.py3-none-any.whl" {
		t.Fatalf("file 0 = %+v", f)
	}
	if g := files[1]; g.URL != "https://pypi.example/files/six-1.15.0.tar.gz" || !g.Yanked || g.YankedReason != "broken build" {
		t.Fatalf("file 1 = %+v", g)
	}
}

func TestParseHTMLIndex(t *testing.T) {
	files := collectFiles(t, htmlPage, "text/html")
	if len(files) != 2 {
		t.Fatalf("files = %+v (invalid names must be skipped)", files)
	}
	if f := files[0]; f.SHA256 != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" || f.RequiresPython != ">=2.7, !=3.0.*" || f.URL != "https://files.example/six-1.16.0.tar.gz" {
		t.Fatalf("file 0 = %+v", f)
	}
	if f := files[1]; f.URL != "https://pypi.example/files/six-1.14.0.tar.gz" || !f.Yanked {
		t.Fatalf("file 1 = %+v", f)
	}
}

func TestWantsJSON(t *testing.T) {
	cases := map[string]bool{
		"application/vnd.pypi.simple.v1+json, application/vnd.pypi.simple.v1+html; q=0.1, text/html; q=0.01": true,
		"text/html": false,
		"":          false,
		"application/vnd.pypi.simple.latest+json":                    true,
		"text/html, application/vnd.pypi.simple.v1+json;q=0.5":       false,
		"application/vnd.pypi.simple.v1+json;Q=0.1, text/html;q=0.9": false,
	}
	for accept, want := range cases {
		if got := wantsJSON(accept); got != want {
			t.Errorf("%q: %v", accept, got)
		}
	}
}

func TestRendererHTMLAndJSON(t *testing.T) {
	f := File{Filename: "six-1.16.0.tar.gz", SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RequiresPython: ">=2.7", Yanked: true, YankedReason: "bad"}
	href := "http://repo/repository/py/packages/six/six-1.16.0.tar.gz"

	w := httptest.NewRecorder()
	r := newRenderer(w, false, "six")
	if err := r.File(f, href); err != nil {
		t.Fatal(err)
	}
	if sent, err := r.Close(); !sent || err != nil {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
	body := w.Body.String()
	if w.Header().Get("Vary") != "Accept" {
		t.Fatalf("html response Vary = %q", w.Header().Get("Vary"))
	}
	if w.Header().Get("Content-Type") != htmlType || !strings.Contains(body, `href="`+href+`#sha256=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`) ||
		!strings.Contains(body, `data-requires-python="&gt;=2.7"`) || !strings.Contains(body, `data-yanked="bad"`) {
		t.Fatalf("html = %s", body)
	}

	w = httptest.NewRecorder()
	r = newRenderer(w, true, "six")
	r.File(f, href)
	r.Close()
	var doc struct {
		Name  string
		Files []map[string]any
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || w.Header().Get("Content-Type") != jsonType {
		t.Fatalf("json = %s (%v)", w.Body.String(), err)
	}
	if doc.Name != "six" || doc.Files[0]["url"] != href || doc.Files[0]["yanked"] != "bad" || doc.Files[0]["requires-python"] != ">=2.7" {
		t.Fatalf("json = %s", w.Body.String())
	}
	if w.Header().Get("Vary") != "Accept" {
		t.Fatalf("json response Vary = %q", w.Header().Get("Vary"))
	}

	w = httptest.NewRecorder()
	if sent, _ := newRenderer(w, true, "none").Close(); sent || w.Body.Len() != 0 {
		t.Fatal("an empty page must not be sent")
	}
}

func TestWriteProjects(t *testing.T) {
	w := httptest.NewRecorder()
	if err := writeProjects(w, false, []string{"six", "django"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), `<a href="six/">six</a>`) {
		t.Fatalf("html = %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	writeProjects(w, true, []string{"six"})
	if !strings.Contains(w.Body.String(), `"projects":[{"name":"six"}]`) {
		t.Fatalf("json = %s", w.Body.String())
	}
	if w.Header().Get("Vary") != "Accept" {
		t.Fatalf("projects Vary = %q", w.Header().Get("Vary"))
	}
}

func TestReadTag(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(`a title='x>y' href="z>w">rest`))
	tag, err := readTag(br)
	if err != nil || tag != `a title='x>y' href="z>w"` {
		t.Fatalf("tag = %q (%v)", tag, err)
	}
	big := bufio.NewReader(strings.NewReader("a title=\"" + strings.Repeat("x", 70<<10) + "\">"))
	if _, err := readTag(big); err != errIndex {
		t.Fatalf("oversized tag: %v", err)
	}
}

func TestParseHTMLQuotedGT(t *testing.T) {
	page := `<a href="/f/six-1.0.tar.gz" data-requires-python=">=3.6">six-1.0.tar.gz</a>`
	files := collectFiles(t, page, "text/html")
	if len(files) != 1 {
		t.Fatalf("files = %+v", files)
	}
	if f := files[0]; f.Filename != "six-1.0.tar.gz" || f.RequiresPython != ">=3.6" || f.URL != "https://pypi.example/f/six-1.0.tar.gz" {
		t.Fatalf("file 0 = %+v", f)
	}
}

func TestUnusableURLsSkipped(t *testing.T) {
	jsonBad := `{"files":[{"filename":"a-1.0.tar.gz","url":"","hashes":{}},{"filename":"b-1.0.tar.gz","url":"ftp://x/b-1.0.tar.gz","hashes":{}},{"filename":"c-1.0.tar.gz","url":"https://files.example/c-1.0.tar.gz","hashes":{}}]}`
	files := collectFiles(t, jsonBad, jsonType)
	if len(files) != 1 || files[0].Filename != "c-1.0.tar.gz" {
		t.Fatalf("json files = %+v", files)
	}
	htmlBad := `<a href="">six-1.0.tar.gz</a><a href="https://files.example/six-2.0.tar.gz">six-2.0.tar.gz</a>`
	files = collectFiles(t, htmlBad, "text/html")
	if len(files) != 1 || files[0].Filename != "six-2.0.tar.gz" {
		t.Fatalf("html files = %+v", files)
	}
}

func TestJSONNestedSkipped(t *testing.T) {
	nested := `{"meta":{"x":[1,{"y":"]}"}],"z":{"w":{}}},"files":[{"filename":"six-1.0.tar.gz","url":"https://files.example/six-1.0.tar.gz","hashes":{}}],"tail":[[]]}`
	files := collectFiles(t, nested, jsonType)
	if len(files) != 1 || files[0].Filename != "six-1.0.tar.gz" {
		t.Fatalf("files = %+v", files)
	}
}

func TestSHA256Lowercased(t *testing.T) {
	jsonUp := `{"files":[{"filename":"six-1.0.tar.gz","url":"https://files.example/six-1.0.tar.gz","hashes":{"sha256":"ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD"}}]}`
	if files := collectFiles(t, jsonUp, jsonType); len(files) != 1 || files[0].SHA256 != "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd" {
		t.Fatalf("json files = %+v", files)
	}
	htmlUp := `<a href="https://files.example/six-1.0.tar.gz#sha256=DEF123DEF123DEF123DEF123DEF123DEF123DEF123DEF123DEF123DEF123DEF1">six-1.0.tar.gz</a>`
	if files := collectFiles(t, htmlUp, "text/html"); len(files) != 1 || files[0].SHA256 != "def123def123def123def123def123def123def123def123def123def123def1" {
		t.Fatalf("html files = %+v", files)
	}
}

func TestInvalidSHA256Dropped(t *testing.T) {
	good := strings.Repeat("a", 64)
	for name, bad := range map[string]string{"short": "abc", "non-hex": strings.Repeat("g", 64), "long": strings.Repeat("a", 65), "empty": ""} {
		js := `{"files":[{"filename":"six-1.0.tar.gz","url":"https://files.example/six-1.0.tar.gz","hashes":{"sha256":"` + bad + `"}}]}`
		if files := collectFiles(t, js, jsonType); len(files) != 1 || files[0].SHA256 != "" {
			t.Errorf("json %s: %+v", name, files)
		}
		hp := `<a href="https://files.example/six-1.0.tar.gz#sha256=` + bad + `">six-1.0.tar.gz</a>`
		if files := collectFiles(t, hp, "text/html"); len(files) != 1 || files[0].SHA256 != "" {
			t.Errorf("html %s: %+v", name, files)
		}
	}
	js := `{"files":[{"filename":"six-1.0.tar.gz","url":"https://files.example/six-1.0.tar.gz","hashes":{"sha256":"` + good + `"}}]}`
	if files := collectFiles(t, js, jsonType); len(files) != 1 || files[0].SHA256 != good {
		t.Errorf("valid json hash dropped: %+v", files)
	}
}

func TestHTMLCommentsAndDeclarations(t *testing.T) {
	page := `<!-- don't --><?xml version="1.0"?><!DOCTYPE html><!-- a > b "q --><a href="https://files.example/six-1.0.tar.gz">six-1.0.tar.gz</a>`
	files := collectFiles(t, page, "text/html")
	if len(files) != 1 || files[0].Filename != "six-1.0.tar.gz" {
		t.Fatalf("files = %+v", files)
	}
}
