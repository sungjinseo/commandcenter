package r

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
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

const fooDesc = `Package: foo
Type: Package
Title: Foo Does Things
Version: 1.0.0
Authors@R: person("Ann", "Author", role = c("aut", "cre"))
Description: Longer text
    across lines.
License: MIT + file LICENSE
URL: https://example.org/foo, https://git.example.org/foo
BugReports: https://git.example.org/foo/issues
Depends: R (>= 4.1)
Imports: jsonlite (>= 1.8),
    httr
Suggests: testthat (>= 3.0.0)
LinkingTo: Rcpp
Encoding: UTF-8
NeedsCompilation: yes
`

// tarGz is a source (or macOS binary) package: <pkg>/DESCRIPTION among
// the usual files, gzipped tar.
func tarGz(pkg, description string, prefix string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name, body string) {
		tw.WriteHeader(&tar.Header{Name: prefix + name, Mode: 0o644, Size: int64(len(body))})
		io.WriteString(tw, body)
	}
	add(pkg+"/NAMESPACE", "export(hello)\n")
	if description != "" {
		add(pkg+"/DESCRIPTION", description)
	}
	add(pkg+"/R/hello.R", "hello <- function() 'hi'\n")
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// zipPkg is a Windows binary package: a zip with <pkg>/DESCRIPTION.
func zipPkg(pkg, description string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range [][2]string{{pkg + "/NAMESPACE", "export(hello)\n"}, {pkg + "/DESCRIPTION", description}, {pkg + "/R/" + pkg + ".rdb", "x"}} {
		if f[1] == "" {
			continue
		}
		w, _ := zw.Create(f[0])
		io.WriteString(w, f[1])
	}
	zw.Close()
	return buf.Bytes()
}

func sums(b []byte) blob.Sums {
	s, m := sha256.Sum256(b), md5.Sum(b)
	return blob.Sums{SHA256: hex.EncodeToString(s[:]), MD5: hex.EncodeToString(m[:]), Size: int64(len(b))}
}

// memFile is a blob.File over bytes.
type memFile struct{ *bytes.Reader }

func (memFile) Close() error { return nil }

func parseBytes(t *testing.T, p string, b []byte) (row, error) {
	t.Helper()
	return parse(memFile{bytes.NewReader(b)}, sums(b), p)
}

func TestParseDescription(t *testing.T) {
	src := tarGz("foo", fooDesc, "")
	r, err := parseBytes(t, "src/contrib/foo_1.0.0.tar.gz", src)
	if err != nil {
		t.Fatal(err)
	}
	if r.Dir != "src/contrib" || r.File != "src/contrib/foo_1.0.0.tar.gz" || r.MD5 != sums(src).MD5 || r.Size != int64(len(src)) {
		t.Fatalf("row: %+v", r)
	}
	if got := r.field("Description"); got != "Longer text\nacross lines." {
		t.Fatalf("continuation: %q", got)
	}
	if got := r.field("Imports"); got != "jsonlite (>= 1.8),\nhttr" {
		t.Fatalf("imports: %q", got)
	}
	if r.Fields[0] != [2]string{"Package", "foo"} || r.field("Authors@R") == "" || r.field("needscompilation") != "yes" {
		t.Fatalf("fields: %v", r.Fields)
	}
	// "./" prefixed entries, a BOM and CRLF are read too
	crlf := "\xef\xbb\xbf" + strings.ReplaceAll(fooDesc, "\n", "\r\n")
	if r, err := parseBytes(t, "src/contrib/foo_1.0.0.tar.gz", tarGz("foo", crlf, "./")); err != nil || r.field("Package") != "foo" || r.field("Title") != "Foo Does Things" {
		t.Fatalf("./ and CRLF: %+v %v", r, err)
	}
	// a Windows binary: zip
	winDesc := fooDesc + "Built: R 4.4.1; x86_64-w64-mingw32; 2026-10-01 10:00:00 UTC; windows\n"
	if r, err := parseBytes(t, "bin/windows/contrib/4.4/foo_1.0.0.zip", zipPkg("foo", winDesc)); err != nil || r.Dir != "bin/windows/contrib/4.4" || !strings.HasPrefix(r.field("Built"), "R 4.4.1") {
		t.Fatalf("zip: %+v %v", r, err)
	}
	// a macOS binary: tgz
	if r, err := parseBytes(t, "bin/macosx/big-sur-arm64/contrib/4.4/foo_1.0.0.tgz", tarGz("foo", winDesc, "")); err != nil || r.Dir != "bin/macosx/big-sur-arm64/contrib/4.4" {
		t.Fatalf("tgz: %+v %v", r, err)
	}
	bad := map[string]struct {
		path string
		b    []byte
		msg  string
	}{
		"name differs":    {"src/contrib/bar_1.0.0.tar.gz", tarGz("bar", fooDesc, ""), `Package "foo"`},
		"version differs": {"src/contrib/foo_1.0.1.tar.gz", src, `Version "1.0.0"`},
		"no description":  {"src/contrib/foo_1.0.0.tar.gz", tarGz("foo", "", ""), "no foo/DESCRIPTION"},
		"other dir":       {"src/contrib/foo_1.0.0.tar.gz", tarGz("bar", fooDesc, ""), "no foo/DESCRIPTION"},
		"not gzip":        {"src/contrib/foo_1.0.0.tar.gz", []byte("plain"), "not a gzip"},
		"not tar":         {"src/contrib/foo_1.0.0.tar.gz", gzipBytes([]byte("not a tar")), "not a tar archive"},
		"zip for source":  {"src/contrib/foo_1.0.0.tar.gz", zipPkg("foo", fooDesc), "not a gzip"},
		"not zip":         {"bin/windows/contrib/4.4/foo_1.0.0.zip", src, "not a zip"},
		"bad dcf":         {"src/contrib/foo_1.0.0.tar.gz", tarGz("foo", "Package foo\n", ""), `not "Field: value"`},
		"continuation":    {"src/contrib/foo_1.0.0.tar.gz", tarGz("foo", "  Package: foo\n", ""), "starts with a continuation"},
		"empty":           {"src/contrib/foo_1.0.0.tar.gz", tarGz("foo", "\n\n", ""), "DESCRIPTION is empty"},
		"huge":            {"src/contrib/foo_1.0.0.tar.gz", tarGz("foo", "Package: foo\nX: "+strings.Repeat("x", maxDescription)+"\n", ""), "larger than"},
		"not a package":   {"src/contrib/foo-1.0.0.tar.gz", src, "upload <directory>"},
		"bad version":     {"src/contrib/foo_1.tar.gz", src, "upload <directory>"},
		"no dir":          {"foo_1.0.0.tar.gz", src, "upload <directory>"},
	}
	for name, c := range bad {
		if _, err := parseBytes(t, c.path, c.b); repo.StatusOf(err) != 400 || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSplitR(t *testing.T) {
	for _, c := range []struct {
		p, dir, name, version string
		ok                    bool
	}{
		{"src/contrib/foo_1.0.0.tar.gz", "src/contrib", "foo", "1.0.0", true},
		{"src/contrib/data.table_1.15-2.tar.gz", "src/contrib", "data.table", "1.15-2", true},
		{"bin/windows/contrib/4.4/foo_1.0.0.zip", "bin/windows/contrib/4.4", "foo", "1.0.0", true},
		{"bin/macosx/contrib/4.3/foo_1.0.0.tgz", "bin/macosx/contrib/4.3", "foo", "1.0.0", true},
		{"bin/macosx/big-sur-arm64/contrib/4.4/foo_1.0.0.tgz", "bin/macosx/big-sur-arm64/contrib/4.4", "foo", "1.0.0", true},
		{"src/contrib/Archive/foo/foo_0.9.tar.gz", "src/contrib/Archive/foo", "foo", "0.9", true},
		{"prod/src/contrib/foo_1.0.tar.gz", "prod/src/contrib", "foo", "1.0", true},                                     // below a prefix
		{"prod/bin/windows/contrib/4.4/foo_1.0.zip", "prod/bin/windows/contrib/4.4", "foo", "1.0", true},                // ... the prefix keeps the binary's extension
		{"prod/bin/macosx/contrib/4.4/foo_1.0.tgz", "prod/bin/macosx/contrib/4.4", "foo", "1.0", true},                  //
		{"bin/linux/jammy/contrib/4.4/foo_1.0.tar.gz", "bin/linux/jammy/contrib/4.4", "foo", "1.0", true},               // a layout of its own
		{"__linux__/jammy/latest/src/contrib/foo_1.0.tar.gz", "__linux__/jammy/latest/src/contrib", "foo", "1.0", true}, // a proxied Posit layout
		{"lib/foo_1.0.tar.gz", "lib", "foo", "1.0", true},                                                               // a proxy's directory; an upload needs two levels
		{"src/contrib/foo_1.0.0.zip", "", "", "", false},                                                                // wrong extension for the directory
		{"prod/bin/windows/contrib/4.4/foo_1.0.tar.gz", "", "", "", false},
		{"bin/windows/contrib/4.4/foo_1.0.0.tar.gz", "", "", "", false},
		{"src/contrib/PACKAGES", "", "", "", false}, // the index
		{"src/contrib/PACKAGES.rds", "", "", "", false},
		{"src/contrib/foo_1.tar.gz", "", "", "", false},    // one-part version
		{"src/contrib/1foo_1.0.tar.gz", "", "", "", false}, // name must start with a letter
		{"src/contrib/foo._1.0.tar.gz", "", "", "", false}, // and not end with a dot
		{"src/contrib/foo_bar_1.0.tar.gz", "", "", "", false},
		{"foo_1.0.tar.gz", "", "", "", false},
	} {
		dir, name, version, ok := component.SplitR(c.p)
		if dir != c.dir || name != c.name || version != c.version || ok != c.ok {
			t.Errorf("%s: %q %q %q %v", c.p, dir, name, version, ok)
		}
		if g, n, v, ok := component.DeriveAsset("r", c.p); g != c.dir || n != c.name || v != c.version || ok != c.ok {
			t.Errorf("DeriveAsset %s: %q %q %q %v", c.p, g, n, v, ok)
		}
	}
	if g, n, v, p := component.PackageComponent("r", "bin/windows/contrib/4.4/foo", "1.0.0"); g != "bin/windows/contrib/4.4" || n != "foo" || v != "1.0.0" || p != "bin/windows/contrib/4.4/foo_1.0.0.zip" {
		t.Fatalf("PackageComponent: %q %q %q %q", g, n, v, p)
	}
	if g, n, v, p := component.PackageComponent("r", "src/contrib/foo", "1.0.0"); g != "src/contrib" || n != "foo" || v != "1.0.0" || p != "src/contrib/foo_1.0.0.tar.gz" {
		t.Fatalf("PackageComponent: %q %q %q %q", g, n, v, p)
	}
	if g, n, v, p := component.PackageComponent("r", "prod/bin/windows/contrib/4.4/foo", "1.0.0"); g != "prod/bin/windows/contrib/4.4" || n != "foo" || v != "1.0.0" || p != "prod/bin/windows/contrib/4.4/foo_1.0.0.zip" {
		t.Fatalf("PackageComponent: %q %q %q %q", g, n, v, p)
	}
	for dir, want := range map[string]string{"src/contrib": "", "bin/windows/contrib/4.4": "src/contrib", "prod/bin/macosx/big-sur-arm64/contrib/4.4": "prod/src/contrib", "bin/linux/jammy": "src/contrib", "binary/x": ""} {
		if got := sourceDir(dir); got != want {
			t.Errorf("sourceDir(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range [][2]string{{"1.0", "1.0.1"}, {"1.9", "1.10"}, {"1.0", "1.0-1"}, {"0.9-2", "0.10"}, {"1.15-2", "1.15.2.1"}} {
		if !versionLess(c[0], c[1]) || versionLess(c[1], c[0]) {
			t.Errorf("%s < %s", c[0], c[1])
		}
	}
	if versionLess("1.0", "1.0") || versionLess("1.0", "1-0") {
		t.Error("equal")
	}
}

// rdsReader reads back what writeRDS writes (and the general shape of an
// R serialization, format 2): the matrix's cells, dim and dimnames.
type rdsReader struct {
	t *testing.T
	r *bytes.Reader
}

const na = "\x00NA"

func (d *rdsReader) int() int32 {
	var v int32
	if err := binary.Read(d.r, binary.BigEndian, &v); err != nil {
		d.t.Fatalf("rds: %v", err)
	}
	return v
}

type rdsVec struct {
	vals  []any
	attrs map[string]any
}

// item reads one serialized object.
func (d *rdsReader) item() any {
	flags := d.int()
	typ, hasAttr, hasTag := flags&0xff, flags&(1<<9) != 0, flags&(1<<10) != 0
	switch typ {
	case sxpNil:
		return nil
	case sxpChar:
		n := d.int()
		if n == -1 {
			return na
		}
		b := make([]byte, n)
		io.ReadFull(d.r, b)
		if levels := flags >> 12; !isASCII(string(b)) && levels&(1<<3) == 0 {
			d.t.Fatalf("rds: non-ASCII %q without the UTF-8 flag", b)
		}
		return string(b)
	case sxpSym:
		return d.item().(string)
	case sxpInt, sxpStr, sxpVec:
		v := rdsVec{}
		n := d.int()
		for range n {
			if typ == sxpInt {
				v.vals = append(v.vals, d.int())
			} else {
				v.vals = append(v.vals, d.item())
			}
		}
		if hasAttr {
			v.attrs = d.item().(map[string]any)
		}
		return v
	case sxpList:
		attrs := map[string]any{}
		for {
			if hasAttr {
				d.item()
			}
			tag := ""
			if hasTag {
				tag = d.item().(string)
			}
			attrs[tag] = d.item()
			flags = d.int()
			if flags == sxpNil {
				return attrs
			}
			if flags&0xff != sxpList {
				d.t.Fatalf("rds: %d in a pairlist", flags)
			}
			hasAttr, hasTag = flags&(1<<9) != 0, flags&(1<<10) != 0
		}
	}
	d.t.Fatalf("rds: unexpected type %d", typ)
	return nil
}

// readRDS decodes an rds character matrix: the column names and the rows.
func readRDS(t *testing.T, b []byte) (cols []string, rows [][]string) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte("X\n")) {
		t.Fatalf("rds: not XDR: %q", raw[:2])
	}
	d := &rdsReader{t, bytes.NewReader(raw[2:])}
	if v, w, m := d.int(), d.int(), d.int(); v != 2 || w != rVersion || m != minVersion {
		t.Fatalf("rds header: %d %x %x", v, w, m)
	}
	m := d.item().(rdsVec)
	if d.r.Len() != 0 {
		t.Fatalf("rds: %d bytes left", d.r.Len())
	}
	dim := m.attrs["dim"].(rdsVec)
	nrow, ncol := int(dim.vals[0].(int32)), int(dim.vals[1].(int32))
	if len(m.vals) != nrow*ncol {
		t.Fatalf("rds: %d cells for %dx%d", len(m.vals), nrow, ncol)
	}
	dn := m.attrs["dimnames"].(rdsVec)
	if dn.vals[0] != nil {
		t.Fatalf("rds: row names %v", dn.vals[0])
	}
	for _, c := range dn.vals[1].(rdsVec).vals {
		cols = append(cols, c.(string))
	}
	if len(cols) != ncol {
		t.Fatalf("rds: %d column names for %d columns", len(cols), ncol)
	}
	for i := range nrow {
		var r []string
		for j := range ncol {
			r = append(r, m.vals[j*nrow+i].(string)) // column-major
		}
		rows = append(rows, r)
	}
	return cols, rows
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestIndexFiles(t *testing.T) {
	src := tarGz("foo", fooDesc, "")
	foo, err := parseBytes(t, "src/contrib/foo_1.0.0.tar.gz", src)
	if err != nil {
		t.Fatal(err)
	}
	barB := tarGz("bar", "Package: bar\nVersion: 0.2\nTitle: Bär\nLicense: GPL-3\nDepends: R (>= 3.5.0), foo\nNeedsCompilation: no\n", "")
	bar, err := parseBytes(t, "src/contrib/bar_0.2.tar.gz", barB)
	if err != nil {
		t.Fatal(err)
	}
	ss := []stanza{stanzaOf(foo), stanzaOf(bar)}
	sortStanzas(ss)
	want := "Package: bar\nVersion: 0.2\nDepends: R (>= 3.5.0), foo\nLicense: GPL-3\nMD5sum: " + sums(barB).MD5 + "\nNeedsCompilation: no\n" +
		"\n" +
		"Package: foo\nVersion: 1.0.0\nDepends: R (>= 4.1)\nImports: jsonlite (>= 1.8),\n        httr\nLinkingTo: Rcpp\nSuggests: testthat (>= 3.0.0)\nLicense: MIT + file LICENSE\nMD5sum: " + sums(src).MD5 + "\nNeedsCompilation: yes\n"
	if got := render(ss); got != want {
		t.Fatalf("PACKAGES:\n%s\nwant:\n%s", got, want)
	}
	if got := gunzip(t, gzipBytes([]byte(want))); got != want {
		t.Fatal("PACKAGES.gz")
	}
	if !bytes.Equal(gzipBytes([]byte(want)), gzipBytes([]byte(want))) {
		t.Fatal("gzip is not deterministic")
	}
	// parsing the text back gives the stanzas
	back, err := parsePackages(strings.NewReader(want))
	if err != nil || len(back) != 2 || back[1].get("Imports") != "jsonlite (>= 1.8),\nhttr" || back[0].get("MD5sum") != sums(barB).MD5 {
		t.Fatalf("parsePackages: %v %+v", err, back)
	}
	// PACKAGES.rds: a character matrix, the standard columns, NA where a
	// stanza has no field, read back by the test reader
	rds := writeRDS(columns("src/contrib", ss), ss)
	cols, rows := readRDS(t, rds)
	if strings.Join(cols, ",") != strings.Join(stdFields, ",") {
		t.Fatalf("columns: %v", cols)
	}
	if len(rows) != 2 || rows[0][0] != "bar" || rows[0][1] != "0.2" || rows[0][2] != na || rows[0][3] != "R (>= 3.5.0), foo" || rows[0][13] != sums(barB).MD5 || rows[0][14] != "no" {
		t.Fatalf("row 0: %q", rows[0])
	}
	if rows[1][0] != "foo" || rows[1][4] != "jsonlite (>= 1.8),\nhttr" || rows[1][5] != "Rcpp" || rows[1][6] != "testthat (>= 3.0.0)" || rows[1][7] != na {
		t.Fatalf("row 1: %q", rows[1])
	}
	// the hand-checked prefix: "X\n", format 2, R 4.4.0, R 2.3.0, a
	// STRSXP with attributes of 2*15 cells, "bar" as an ASCII CHARSXP
	raw := gunzip(t, rds)
	prefix := "X\n" + "\x00\x00\x00\x02" + "\x00\x04\x04\x00" + "\x00\x02\x03\x00" + "\x00\x00\x02\x10" + "\x00\x00\x00\x1e" + "\x00\x04\x00\x09" + "\x00\x00\x00\x03" + "bar"
	if !strings.HasPrefix(raw, prefix) {
		t.Fatalf("rds prefix:\n%s\nwant:\n%s", hex.Dump([]byte(raw[:len(prefix)])), hex.Dump([]byte(prefix)))
	}
	// NA is a CHARSXP of length -1; the attributes end with the pairlist's NILVALUE
	if !strings.Contains(raw, "\x00\x00\x00\x09\xff\xff\xff\xff") || !strings.HasSuffix(raw, "NeedsCompilation\x00\x00\x00\xfe") {
		t.Fatalf("rds NA or tail:\n%s", hex.Dump([]byte(raw)))
	}
	// a binary directory has Built; an extra field becomes a column after
	// the standard ones; the empty index is a 0-row matrix
	winDesc := fooDesc + "Built: R 4.4.1; x86_64-w64-mingw32; 2026-10-01 10:00:00 UTC; windows\n"
	win, err := parseBytes(t, "bin/windows/contrib/4.4/foo_1.0.0.zip", zipPkg("foo", winDesc))
	if err != nil {
		t.Fatal(err)
	}
	ws := stanzaOf(win)
	if !strings.HasSuffix(render([]stanza{ws}), "NeedsCompilation: yes\nBuilt: R 4.4.1; x86_64-w64-mingw32; 2026-10-01 10:00:00 UTC; windows\n") {
		t.Fatalf("Built: %s", render([]stanza{ws}))
	}
	ws = append(ws, [2]string{"File", "foo_1.0.0_custom.zip"})
	cols, rows = readRDS(t, writeRDS(columns("bin/windows/contrib/4.4", []stanza{ws}), []stanza{ws}))
	if len(cols) != 17 || cols[15] != "Built" || cols[16] != "File" || rows[0][15] != ws.get("Built") || rows[0][16] != "foo_1.0.0_custom.zip" {
		t.Fatalf("binary columns: %v %q", cols, rows)
	}
	cols, rows = readRDS(t, writeRDS(columns("src/contrib", nil), nil))
	if len(cols) != 15 || len(rows) != 0 {
		t.Fatalf("empty: %v %v", cols, rows)
	}
	// a UTF-8 cell carries the UTF-8 flag (the reader checks)
	readRDS(t, writeRDS([]string{"Title"}, []stanza{{{"Title", "Bär"}}}))
}

// merge runs mergePackages over index and source documents stored in r-hosted.
func (e *env) merge(t *testing.T, docs ...[2]string) (string, error) {
	t.Helper()
	store := func(doc string) *meta.Asset {
		if doc == "" {
			return nil
		}
		a, err := e.h.Store.Put(context.Background(), "r-hosted", "doc-"+sums([]byte(doc)).SHA256, ctText, strings.NewReader(doc))
		if err != nil {
			t.Fatal(err)
		}
		return &a
	}
	members := make([]member, len(docs))
	for i, d := range docs {
		members[i] = member{store(d[0]), store(d[1])}
	}
	var out bytes.Buffer
	err := e.h.mergePackages(&out, members, false)
	return out.String(), err
}

func TestMerge(t *testing.T) {
	e := newEnv(t)
	// a name is the first member's, whatever version a later member has: the
	// public dup 1.1 and a 1.0 are hidden, b comes through; all the versions
	// the first member lists stay.
	hosted := "Package: dup\nVersion: 1.0\nMD5sum: hosted\n\nPackage: a\nVersion: 2.0\n\nPackage: a\nVersion: 2.1\n"
	upstream := "Package: a\nVersion: 1.0\nDepends: R (>= 3.5)\n\nPackage: dup\nVersion: 1.0\nMD5sum: upstream\n\nPackage: dup\nVersion: 1.1\nMD5sum: upstream\n\nPackage: b\nVersion: 1.0\nDepends: R (>= 3.5),\n    a\n\n\n"
	got, err := e.merge(t, [2]string{hosted}, [2]string{upstream})
	want := "Package: dup\nVersion: 1.0\nMD5sum: hosted\n\nPackage: a\nVersion: 2.0\n\nPackage: a\nVersion: 2.1\n\nPackage: b\nVersion: 1.0\nDepends: R (>= 3.5),\n        a\n"
	if err != nil || got != want {
		t.Fatalf("merged: %v\n%s\nwant:\n%s", err, got, want)
	}
	// a member's source index counts for a binary directory's names, listing nothing itself
	got, err = e.merge(t, [2]string{"", "Package: dup\nVersion: 0.1\n"}, [2]string{"Package: dup\nVersion: 1.1\n\nPackage: b\nVersion: 1.0\n"})
	if err != nil || got != "Package: b\nVersion: 1.0\n" {
		t.Fatalf("with a source index: %v %q", err, got)
	}
	// a gzipped member is read as well
	gz, err := e.h.Store.Put(context.Background(), "r-hosted", "doc.gz", ctGzip, bytes.NewReader(gzipBytes([]byte("Package: z\nVersion: 1.0\n"))))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := e.h.mergePackages(&out, []member{{index: &gz}}, false); err != nil || out.String() != "Package: z\nVersion: 1.0\n" {
		t.Fatalf("gzipped: %v %q", err, out.String())
	}
	// mergeMembers: every member's versions, one listed by both being the first's
	a, _ := e.h.Store.Put(context.Background(), "r-hosted", "first", ctText, strings.NewReader("Package: dup\nVersion: 1.0\nMD5sum: hosted\n"))
	b, _ := e.h.Store.Put(context.Background(), "r-hosted", "second", ctText, strings.NewReader("Package: dup\nVersion: 1.0\nMD5sum: up\n\nPackage: dup\nVersion: 1.1\n"))
	out.Reset()
	if err := e.h.mergePackages(&out, []member{{index: &a}, {index: &b}}, true); err != nil || out.String() != "Package: dup\nVersion: 1.0\nMD5sum: hosted\n\nPackage: dup\nVersion: 1.1\n" {
		t.Fatalf("every version: %v %q", err, out.String())
	}
	if got, err := e.merge(t); err != nil || got != "" {
		t.Fatalf("no members: %v %q", err, got)
	}
	if _, err := e.merge(t, [2]string{"<html>"}); err == nil {
		t.Fatal("html merged")
	}
}

type env struct {
	h      *Handler
	repos  map[string]*repo.Repository
	sink   *repotest.Sink
	db     *meta.DB
	now    time.Time
	hits   atomic.Int32 // upstream requests
	up     *httptest.Server
	webRDS atomic.Value // the upstream's web/packages/packages.rds
}

const upPackages = "Package: dup\nVersion: 1.0\nLicense: GPL-2\nMD5sum: 00000000000000000000000000000000\nNeedsCompilation: no\n\nPackage: up\nVersion: 1.0\nDepends: R (>= 3.5)\nLicense: MIT\nMD5sum: 11111111111111111111111111111111\nNeedsCompilation: no\n"

// upstream is a CRAN-like mirror: src/contrib with PACKAGES, PACKAGES.gz,
// PACKAGES.rds (bytes the proxy passes through), a package, an Archive
// and a Meta/ database.
func (e *env) upstream(w http.ResponseWriter, r *http.Request) {
	e.hits.Add(1)
	switch r.URL.Path {
	case "/src/contrib/PACKAGES":
		io.WriteString(w, upPackages)
	case "/src/contrib/PACKAGES.gz":
		w.Header().Set("Content-Type", "application/x-gzip")
		w.Write(gzipBytes([]byte(upPackages)))
	case "/src/contrib/PACKAGES.rds":
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "RDS-FROM-UPSTREAM")
	case "/src/contrib/Meta/current.rds":
		io.WriteString(w, "META")
	case "/src/contrib/up_1.0.tar.gz", "/src/contrib/Archive/up/up_0.9.tar.gz":
		w.Header().Set("Content-Type", "application/x-gzip")
		io.WriteString(w, "up")
	case "/src/contrib/":
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html>Index of /src/contrib/</html>")
	case "/web/packages/packages.rds":
		io.WriteString(w, e.webRDS.Load().(string))
	case "/bin/windows/contrib/4.5/PACKAGES": // binaries: dup is also a source package of the hosted member
		io.WriteString(w, "Package: dup\nVersion: 9.9\n\nPackage: winonly\nVersion: 1.0\n")
	case "/gz/src/contrib/PACKAGES.gz": // a remote that publishes only this, as a Nexus hosted repository does
		w.Header().Set("Content-Type", "application/x-gzip")
		w.Write(gzipBytes([]byte("Package: dup\nVersion: 8.8\n\nPackage: gzonly\nVersion: 1.0\n")))
	default:
		if strings.HasPrefix(r.URL.Path, "/failing/") {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	e.webRDS.Store("web-1")
	e.up = httptest.NewServer(http.HandlerFunc(e.upstream))
	t.Cleanup(e.up.Close)
	dir := t.TempDir()
	b, _ := blob.Open(dir)
	db, err := meta.Open(metatest.URL(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.db, e.sink = db, &repotest.Sink{}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: func() time.Time { return e.now }, Events: e.sink}
	cfg := &config.Config{AnonymousRead: true, Repositories: []config.Repository{
		{Name: "r-hosted", Format: "r", Type: "hosted", WritePolicy: "allow_once"},
		{Name: "r-allow", Format: "r", Type: "hosted", WritePolicy: "allow"},
		{Name: "r-ro", Format: "r", Type: "hosted", WritePolicy: "deny"},
		{Name: "r-proxy", Format: "r", Type: "proxy", Remote: e.up.URL + "/",
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "r-all", Format: "r", Type: "group", Members: []string{"r-hosted", "r-proxy"}, WriteMember: "r-hosted"},
		{Name: "r-failing", Format: "r", Type: "proxy", Remote: e.up.URL + "/failing/",
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "r-failing-first", Format: "r", Type: "group", Members: []string{"r-failing", "r-hosted"}},
		{Name: "r-failing-last", Format: "r", Type: "group", Members: []string{"r-hosted", "r-failing"}},
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

// get fetches a file and checks the status (and an index file's content type).
func (e *env) get(t *testing.T, repoName, path string, want int) []byte {
	t.Helper()
	w := e.do("GET", repoName, path, nil)
	cts := map[string]string{"PACKAGES": ctText, "PACKAGES.gz": ctGzip, "PACKAGES.rds": ctRDS}
	if w.Code != want || (want == 200 && repoName != "r-proxy" && cts[filepath.Base(path)] != "" && w.Header().Get("Content-Type") != cts[filepath.Base(path)]) {
		t.Fatalf("GET %s/%s: %d %v %s", repoName, path, w.Code, w.Header(), w.Body)
	}
	return w.Body.Bytes()
}

func TestHostedUploadAndIndex(t *testing.T) {
	e := newEnv(t)
	// before any upload: empty index files for a contrib directory, nothing else
	if got := e.get(t, "r-hosted", "src/contrib/PACKAGES", 200); len(got) != 0 {
		t.Fatalf("empty PACKAGES: %q", got)
	}
	if got := gunzip(t, e.get(t, "r-hosted", "src/contrib/PACKAGES.gz", 200)); got != "" {
		t.Fatalf("empty PACKAGES.gz: %q", got)
	}
	if cols, rows := readRDS(t, e.get(t, "r-hosted", "bin/windows/contrib/4.4/PACKAGES.rds", 200)); len(cols) != 16 || len(rows) != 0 {
		t.Fatalf("empty PACKAGES.rds: %v %v", cols, rows)
	}
	for _, p := range []string{"PACKAGES", "src/PACKAGES", "contrib/PACKAGES", "src/contrib/PACKAGES.json", "src/contrib/foo_1.0.0.tar.gz", "src/contrib/"} {
		e.get(t, "r-hosted", p, 404)
	}
	src := tarGz("foo", fooDesc, "")
	if w := e.do("PUT", "r-hosted", "src/contrib/foo_1.0.0.tar.gz", src); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0] != (repotest.Event{Kind: "upload", Repo: "r-hosted", Subject: "src/contrib/foo@1.0.0", Detail: "src/contrib/foo_1.0.0.tar.gz"}) {
		t.Fatalf("events: %v", got)
	}
	w := e.do("GET", "r-hosted", "src/contrib/foo_1.0.0.tar.gz", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), src) || w.Header().Get("Content-Type") != ctGzip {
		t.Fatalf("GET package: %d %v", w.Code, w.Header())
	}
	want := "Package: foo\nVersion: 1.0.0\nDepends: R (>= 4.1)\nImports: jsonlite (>= 1.8),\n        httr\nLinkingTo: Rcpp\nSuggests: testthat (>= 3.0.0)\nLicense: MIT + file LICENSE\nMD5sum: " + sums(src).MD5 + "\nNeedsCompilation: yes\n"
	if got := string(e.get(t, "r-hosted", "src/contrib/PACKAGES", 200)); got != want {
		t.Fatalf("PACKAGES:\n%s\nwant:\n%s", got, want)
	}
	if got := gunzip(t, e.get(t, "r-hosted", "src/contrib/PACKAGES.gz", 200)); got != want {
		t.Fatalf("PACKAGES.gz:\n%s", got)
	}
	if cols, rows := readRDS(t, e.get(t, "r-hosted", "src/contrib/PACKAGES.rds", 200)); len(cols) != 15 || len(rows) != 1 || rows[0][0] != "foo" || rows[0][13] != sums(src).MD5 {
		t.Fatalf("PACKAGES.rds: %v %q", cols, rows)
	}
	if got := e.get(t, "r-hosted", "bin/windows/contrib/4.4/PACKAGES", 200); len(got) != 0 {
		t.Fatalf("other directory: %q", got)
	}
	// the row: directory-qualified name, the version, the DESCRIPTION
	pkgs, err := e.db.ListPackages(context.Background(), "r-hosted", "src/contrib/foo")
	if err != nil || len(pkgs) != 1 || pkgs[0].Version != "1.0.0" || pkgs[0].Format != "r" || !strings.Contains(pkgs[0].Attrs, `["BugReports","https://git.example.org/foo/issues"]`) {
		t.Fatalf("rows: %+v %v", pkgs, err)
	}
	// allow_once: the same file again is a 409, whatever its content
	if w := e.do("PUT", "r-hosted", "src/contrib/foo_1.0.0.tar.gz", tarGz("foo", fooDesc+"X: y\n", "")); w.Code != 409 {
		t.Fatalf("republish: %d %s", w.Code, w.Body)
	}
	for _, c := range []struct {
		path string
		body []byte
		want int
	}{
		{"foo_1.0.0.tar.gz", src, 400},                             // no directory
		{"contrib/foo_1.0.0.tar.gz", src, 400},                     // one level, as Nexus refuses
		{"src/contrib/foo-1.0.0.tar.gz", src, 400},                 // not <pkg>_<version>
		{"src/contrib/foo_1.0.0.zip", zipPkg("foo", fooDesc), 400}, // a zip is not a source package
		{"src/contrib/PACKAGES", []byte("Package: x\n"), 400},      // the index
		{"src/contrib/bar_1.0.0.tar.gz", src, 400},                 // the DESCRIPTION is foo's
		{"src/contrib/foo_1.0.1.tar.gz", src, 400},                 // and version 1.0.0's
		{"src/contrib/empty_1.0.tar.gz", nil, 400},                 // empty
		{"src/contrib/junk_1.0.tar.gz", []byte("junk"), 400},       // not gzip
		{"bin/windows/contrib/4.4/foo_1.0.0.zip", src, 400},        // not a zip
		{"../src/contrib/foo_1.0.0.tar.gz", src, 400},              // escapes
		{"bin/windows/contrib/4.4/foo_1.0.0.tgz", src, 400},        // wrong extension
		{"bin/macosx/contrib/4.4/foo_1.0.0.tar.gz", src, 400},      // wrong extension
		{"src/contrib/foo_1.0.0.tar.gz/x", src, 400},               // a file inside a package path
	} {
		if w := e.do("PUT", "r-hosted", c.path, c.body); w.Code != c.want {
			t.Errorf("PUT %s: %d %s", c.path, w.Code, w.Body)
		}
	}
	// binaries for two R versions and a second source version: each
	// directory has its own index, sorted by package then version
	winDesc := fooDesc + "Built: R 4.4.1; x86_64-w64-mingw32; 2026-10-01 10:00:00 UTC; windows\n"
	for _, p := range []string{"bin/windows/contrib/4.4/foo_1.0.0.zip", "bin/windows/contrib/4.3/foo_1.0.0.zip"} {
		if w := e.do("PUT", "r-hosted", p, zipPkg("foo", winDesc)); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", p, w.Code, w.Body)
		}
	}
	macBuilt := "R 4.4.1; x86_64-w64-mingw32; 2026-10-01 10:00:00 UTC; unix"
	macB := tarGz("foo", fooDesc+"Built: "+macBuilt+"\n", "")
	if w := e.do("PUT", "r-hosted", "bin/macosx/big-sur-arm64/contrib/4.4/foo_1.0.0.tgz", macB); w.Code != 201 {
		t.Fatalf("PUT mac: %d %s", w.Code, w.Body)
	}
	for _, p := range []string{"src/contrib/foo_1.0.1.tar.gz", "src/contrib/bar_0.9-2.tar.gz", "src/contrib/foo_0.9.tar.gz"} {
		_, name, version, _ := component.SplitR(p)
		if w := e.do("PUT", "r-hosted", p, tarGz(name, "Package: "+name+"\nVersion: "+version+"\nLicense: GPL-3\n", "")); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", p, w.Code, w.Body)
		}
	}
	ss, err := parsePackages(bytes.NewReader(e.get(t, "r-hosted", "src/contrib/PACKAGES", 200)))
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, s := range ss {
		order = append(order, s.get("Package")+"_"+s.get("Version"))
	}
	if strings.Join(order, " ") != "bar_0.9-2 foo_0.9 foo_1.0.0 foo_1.0.1" {
		t.Fatalf("order: %v", order)
	}
	if got := string(e.get(t, "r-hosted", "bin/windows/contrib/4.4/PACKAGES", 200)); !strings.HasSuffix(got, "MD5sum: "+sums(zipPkg("foo", winDesc)).MD5+"\nNeedsCompilation: yes\nBuilt: R 4.4.1; x86_64-w64-mingw32; 2026-10-01 10:00:00 UTC; windows\n") || strings.Count(got, "Package:") != 1 {
		t.Fatalf("windows PACKAGES:\n%s", got)
	}
	if cols, rows := readRDS(t, e.get(t, "r-hosted", "bin/macosx/big-sur-arm64/contrib/4.4/PACKAGES.rds", 200)); cols[15] != "Built" || len(rows) != 1 || rows[0][15] != macBuilt {
		t.Fatalf("mac PACKAGES.rds: %v %q", cols, rows)
	}
	if w := e.do("GET", "r-hosted", "bin/windows/contrib/4.4/foo_1.0.0.zip", nil); w.Code != 200 || w.Header().Get("Content-Type") != ctZip {
		t.Fatalf("GET zip: %d %v", w.Code, w.Header())
	}
	if w := e.do("DELETE", "r-hosted", "src/contrib/foo_1.0.0.tar.gz", nil); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD, PUT" {
		t.Fatalf("DELETE: %d %q", w.Code, w.Header().Get("Allow"))
	}
	if w := e.do("PUT", "r-proxy", "src/contrib/foo_1.0.0.tar.gz", src); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("PUT proxy: %d %q", w.Code, w.Header().Get("Allow"))
	}
	if w := e.do("HEAD", "r-hosted", "src/contrib/PACKAGES.gz", nil); w.Code != 200 {
		t.Fatalf("HEAD: %d", w.Code)
	}
}

// TestUploadAnyDirectory: what Nexus takes besides CRAN's own directories,
// each directory with its own index files: CRAN's Archive, a contrib
// directory below a prefix (one repository holding prod and dev), and a
// binary layout of its own. The file must still be <pkg>_<version> of its
// DESCRIPTION, and is a component that can be deleted again.
func TestUploadAnyDirectory(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := tarGz("foo", fooDesc, "")
	win := zipPkg("foo", fooDesc+"Built: R 4.4.1; x86_64-w64-mingw32; 2026-10-01 10:00:00 UTC; windows\n")
	for p, b := range map[string][]byte{
		"src/contrib/Archive/foo/foo_1.0.0.tar.gz":     src,
		"prod/src/contrib/foo_1.0.0.tar.gz":            src,
		"dev/src/contrib/foo_1.0.0.tar.gz":             src,
		"prod/bin/windows/contrib/4.4/foo_1.0.0.zip":   win,
		"bin/linux/jammy/contrib/4.4/foo_1.0.0.tar.gz": src,
	} {
		if w := e.do("PUT", "r-hosted", p, b); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", p, w.Code, w.Body)
		}
		dir, name, version, ok := component.SplitR(p)
		if !ok || name != "foo" || version != "1.0.0" {
			t.Fatalf("SplitR %s", p)
		}
		if got := string(e.get(t, "r-hosted", dir+"/PACKAGES", 200)); !strings.HasPrefix(got, "Package: foo\nVersion: 1.0.0\n") {
			t.Errorf("%s/PACKAGES: %q", dir, got)
		}
		if _, rows := readRDS(t, e.get(t, "r-hosted", dir+"/PACKAGES.rds", 200)); len(rows) != 1 {
			t.Errorf("%s/PACKAGES.rds: %q", dir, rows)
		}
		if got := gunzip(t, e.get(t, "r-hosted", dir+"/PACKAGES.gz", 200)); !strings.HasPrefix(got, "Package: foo\n") {
			t.Errorf("%s/PACKAGES.gz: %q", dir, got)
		}
		g, cn, cv, file := component.PackageComponent("r", dir+"/foo", "1.0.0")
		if g != dir || cn != "foo" || cv != "1.0.0" || file != p {
			t.Errorf("PackageComponent of %s: %q %q %q %q", p, g, cn, cv, file)
		}
	}
	// the binary directory below a prefix lists Built
	if got := string(e.get(t, "r-hosted", "prod/bin/windows/contrib/4.4/PACKAGES", 200)); !strings.Contains(got, "\nBuilt: R 4.4.1;") {
		t.Errorf("no Built: %s", got)
	}
	// a directory's index holds only that directory's packages
	if got := string(e.get(t, "r-hosted", "src/contrib/PACKAGES", 200)); got != "" {
		t.Errorf("src/contrib lists a package of its Archive: %q", got)
	}
	// an index nobody uploaded to is empty, not missing
	if got := e.get(t, "r-hosted", "other/src/contrib/PACKAGES", 200); len(got) != 0 {
		t.Errorf("empty directory: %q", got)
	}
	e.get(t, "r-hosted", "PACKAGES", 404) // the root is no directory
	// the checks stay: DESCRIPTION against the file name, extension against the directory
	if w := e.do("PUT", "r-hosted", "prod/src/contrib/bar_1.0.0.tar.gz", tarGz("bar", fooDesc, "")); w.Code != 400 || !strings.Contains(w.Body.String(), "does not match") {
		t.Errorf("wrong package: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "r-hosted", "prod/stuff/foo_1.0.0.zip", win); w.Code != 400 {
		t.Errorf("zip outside bin/windows: %d %s", w.Code, w.Body)
	}
	// deleting one directory's package rebuilds that directory only
	c := component.Component{Repo: "r-hosted", Format: "r", Group: "prod/src/contrib", Name: "foo", Version: "1.0.0"}
	if res, err := e.h.DeleteComponent(ctx, e.repos["r-hosted"], c, "system"); err != nil || res.Assets != 1 {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if got := e.get(t, "r-hosted", "prod/src/contrib/PACKAGES", 200); len(got) != 0 {
		t.Errorf("after delete: %q", got)
	}
	if got := e.get(t, "r-hosted", "dev/src/contrib/PACKAGES", 200); len(got) == 0 {
		t.Error("the other prefix lost its index")
	}
	// the rebuild task regenerates every directory's lost index files
	for _, f := range []string{"PACKAGES", "PACKAGES.gz", "PACKAGES.rds"} {
		if err := e.db.DeleteAsset(ctx, "r-hosted", "dev/src/contrib/"+f); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.get(t, "r-hosted", "dev/src/contrib/PACKAGES", 200); len(got) != 0 {
		t.Fatalf("the lost index answers: %q", got)
	}
	if err := e.h.RebuildMetadata(ctx, e.repos["r-hosted"], func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if got := string(e.get(t, "r-hosted", "dev/src/contrib/PACKAGES", 200)); !strings.HasPrefix(got, "Package: foo\n") {
		t.Errorf("after the rebuild: %q", got)
	}
	if _, rows := readRDS(t, e.get(t, "r-hosted", "dev/src/contrib/PACKAGES.rds", 200)); len(rows) != 1 {
		t.Errorf("rds after the rebuild: %q", rows)
	}
	if err := e.h.RebuildMetadata(ctx, e.repos["r-proxy"], func(string, ...any) {}); !errors.Is(err, component.ErrNotSupported) {
		t.Errorf("rebuild of a proxy: %v", err)
	}
}

func TestWritePolicy(t *testing.T) {
	e := newEnv(t)
	src := tarGz("foo", fooDesc, "")
	if w := e.do("PUT", "r-ro", "src/contrib/foo_1.0.0.tar.gz", src); w.Code != 400 {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	a, ps := repotest.Principals(t, e.db, "r-allow", map[string][]string{
		"adder":  {meta.ActionAdd},
		"editor": {meta.ActionAdd, meta.ActionEdit},
	})
	e.h.Auth = a
	if w := e.doAs(ps["adder"], "PUT", "r-allow", "src/contrib/foo_1.0.0.tar.gz", src); w.Code != 201 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	if w := e.doAs(ps["adder"], "PUT", "r-allow", "src/contrib/foo_1.0.0.tar.gz", src); w.Code != 403 {
		t.Fatalf("overwrite without edit: %d %s", w.Code, w.Body)
	}
	again := tarGz("foo", fooDesc+"Note: rebuilt\n", "")
	if w := e.doAs(ps["editor"], "PUT", "r-allow", "src/contrib/foo_1.0.0.tar.gz", again); w.Code != 201 {
		t.Fatalf("overwrite with edit: %d %s", w.Code, w.Body)
	}
	if got := string(e.get(t, "r-allow", "src/contrib/PACKAGES", 200)); !strings.Contains(got, sums(again).MD5) || strings.Contains(got, sums(src).MD5) {
		t.Fatalf("PACKAGES after overwrite: %s", got)
	}
	if w := e.do("GET", "r-allow", "src/contrib/foo_1.0.0.tar.gz", nil); !bytes.Equal(w.Body.Bytes(), again) {
		t.Fatal("the file was not replaced")
	}
}

func TestProxyAndGroup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// index files are metadata: fetched again after metadataMaxAge; a package is content: cached for good
	for i := range 2 {
		if got := string(e.get(t, "r-proxy", "src/contrib/PACKAGES", 200)); got != upPackages {
			t.Fatalf("PACKAGES %d: %s", i, got)
		}
		if got := gunzip(t, e.get(t, "r-proxy", "src/contrib/PACKAGES.gz", 200)); got != upPackages {
			t.Fatalf("PACKAGES.gz %d: %s", i, got)
		}
		if got := string(e.get(t, "r-proxy", "src/contrib/PACKAGES.rds", 200)); got != "RDS-FROM-UPSTREAM" {
			t.Fatalf("PACKAGES.rds %d: %s (passed through, never rewritten)", i, got)
		}
		if w := e.do("GET", "r-proxy", "src/contrib/up_1.0.tar.gz", nil); w.Code != 200 || w.Body.String() != "up" || w.Header().Get("Content-Type") != "application/x-gzip" {
			t.Fatalf("package %d: %d %v", i, w.Code, w.Header())
		}
		if w := e.do("GET", "r-proxy", "src/contrib/Archive/up/up_0.9.tar.gz", nil); w.Code != 200 || w.Body.String() != "up" {
			t.Fatalf("archive %d: %d", i, w.Code)
		}
		if got := string(e.get(t, "r-proxy", "src/contrib/Meta/current.rds", 200)); got != "META" {
			t.Fatalf("meta %d: %s", i, got)
		}
	}
	if e.hits.Load() != 6 {
		t.Fatalf("upstream hits: %d", e.hits.Load())
	}
	e.now = e.now.Add(2 * time.Hour)
	e.get(t, "r-proxy", "src/contrib/PACKAGES", 200)
	e.get(t, "r-proxy", "src/contrib/PACKAGES.gz", 200)
	e.get(t, "r-proxy", "src/contrib/PACKAGES.rds", 200)
	e.get(t, "r-proxy", "src/contrib/Meta/current.rds", 200)
	e.do("GET", "r-proxy", "src/contrib/up_1.0.tar.gz", nil)
	e.do("GET", "r-proxy", "src/contrib/Archive/up/up_0.9.tar.gz", nil)
	if e.hits.Load() != 10 {
		t.Fatalf("upstream hits after expiry: %d", e.hits.Load())
	}
	// a proxy has no empty index of its own: the upstream's 404 shows
	e.get(t, "r-proxy", "bin/windows/contrib/4.4/PACKAGES", 404)
	e.get(t, "r-proxy", "src/contrib/", 404) // a directory listing is not a file
	e.get(t, "r-proxy", "src/contrib/nope_1.0.tar.gz", 404)
	// the cached package and archive are components the proxy deleter can remove
	if g, name, version, ok := component.DeriveAsset("r", "src/contrib/Archive/up/up_0.9.tar.gz"); !ok || g != "src/contrib/Archive/up" || name != "up" || version != "0.9" {
		t.Fatalf("derive: %s %s %s %v", g, name, version, ok)
	}

	// group: before the hosted member has anything, the proxy's PACKAGES as it is
	if got := string(e.get(t, "r-all", "src/contrib/PACKAGES", 200)); got != upPackages {
		t.Fatalf("group PACKAGES, proxy only: %s", got)
	}
	// uploads go to the writeMember; a file comes from the first member that has it
	dup := tarGz("dup", "Package: dup\nVersion: 1.0\nLicense: MIT\nNeedsCompilation: no\n", "")
	if w := e.do("PUT", "r-all", "src/contrib/dup_1.0.tar.gz", dup); w.Code != 201 {
		t.Fatalf("PUT group: %d %s", w.Code, w.Body)
	}
	if _, err := e.h.Store.Get(ctx, "r-hosted", "src/contrib/dup_1.0.tar.gz"); err != nil {
		t.Fatalf("writeMember: %v", err)
	}
	if w := e.do("GET", "r-all", "src/contrib/up_1.0.tar.gz", nil); w.Code != 200 || w.Body.String() != "up" {
		t.Fatalf("group package from proxy: %d", w.Code)
	}
	if w := e.do("GET", "r-all", "src/contrib/dup_1.0.tar.gz", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), dup) {
		t.Fatalf("group package from hosted: %d", w.Code)
	}
	// the merged PACKAGES: the hosted member's stanzas first, the
	// upstream's dup 1.0 hidden by the hosted one; .gz and .rds derived
	hits := e.hits.Load()
	merged := "Package: dup\nVersion: 1.0\nLicense: MIT\nMD5sum: " + sums(dup).MD5 + "\nNeedsCompilation: no\n\nPackage: up\nVersion: 1.0\nDepends: R (>= 3.5)\nLicense: MIT\nMD5sum: 11111111111111111111111111111111\nNeedsCompilation: no\n"
	for i := range 2 {
		if got := string(e.get(t, "r-all", "src/contrib/PACKAGES", 200)); got != merged {
			t.Fatalf("merged %d:\n%s\nwant:\n%s", i, got, merged)
		}
		if got := gunzip(t, e.get(t, "r-all", "src/contrib/PACKAGES.gz", 200)); got != merged {
			t.Fatalf("merged gz %d:\n%s", i, got)
		}
		cols, rows := readRDS(t, e.get(t, "r-all", "src/contrib/PACKAGES.rds", 200))
		if len(cols) != 15 || len(rows) != 2 || rows[0][0] != "dup" || rows[0][13] != sums(dup).MD5 || rows[1][0] != "up" || rows[1][3] != "R (>= 3.5)" {
			t.Fatalf("merged rds %d: %v %q", i, cols, rows)
		}
	}
	// the merge and its derived files are cached: no upstream fetch while
	// the proxy's PACKAGES is fresh, and one stored copy of each
	if e.hits.Load() != hits {
		t.Fatalf("upstream hits for the group: %d", e.hits.Load()-hits)
	}
	rds1, _ := e.h.Store.Get(ctx, "r-all", "src/contrib/PACKAGES.rds")
	e.get(t, "r-all", "src/contrib/PACKAGES.rds", 200)
	if rds2, _ := e.h.Store.Get(ctx, "r-all", "src/contrib/PACKAGES.rds"); rds2.LastModified != rds1.LastModified || !strings.HasPrefix(rds1.RemoteETag, "r:") {
		t.Fatalf("derived file rebuilt: %+v %+v", rds1, rds2)
	}
	// a hosted change re-merges and re-derives
	if w := e.do("PUT", "r-all", "src/contrib/new_3.0.tar.gz", tarGz("new", "Package: new\nVersion: 3.0\n", "")); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if got := string(e.get(t, "r-all", "src/contrib/PACKAGES", 200)); !strings.HasPrefix(got, "Package: dup\n") || !strings.Contains(got, "\nPackage: new\nVersion: 3.0\n") || !strings.Contains(got, "\nPackage: up\n") {
		t.Fatalf("re-merged: %s", got)
	}
	if _, rows := readRDS(t, e.get(t, "r-all", "src/contrib/PACKAGES.rds", 200)); len(rows) != 3 || rows[1][0] != "new" {
		t.Fatalf("re-derived rds: %q", rows)
	}
	// a directory no member has: an empty index, as a hosted repository answers
	if got := e.get(t, "r-all", "bin/windows/contrib/4.4/PACKAGES", 200); len(got) != 0 {
		t.Fatalf("group empty: %q", got)
	}
	if cols, rows := readRDS(t, e.get(t, "r-all", "bin/windows/contrib/4.4/PACKAGES.rds", 200)); len(cols) != 16 || len(rows) != 0 {
		t.Fatalf("group empty rds: %v %v", cols, rows)
	}
	e.get(t, "r-all", "bin/windows/contrib/4.4/nope_1.0.zip", 404)
	e.get(t, "r-all", "src/contrib/Archive/up/up_0.9.tar.gz", 200)
}

// TestGroupIndex: a group lists a package name once per directory, from the
// first member that has it, so a newer public version cannot beat an internal
// package of the same name, in its own directory or in a binary one.
func TestGroupIndex(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, v := range []string{"0.9", "1.0"} { // the upstream has dup 1.0; every version the first member lists stays
		if w := e.do("PUT", "r-hosted", "src/contrib/dup_"+v+".tar.gz", tarGz("dup", "Package: dup\nVersion: "+v+"\nNeedsCompilation: no\n", "")); w.Code != 201 {
			t.Fatalf("PUT: %d %s", w.Code, w.Body)
		}
	}
	names := func(body []byte) []string {
		t.Helper()
		ss, err := parsePackages(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range ss {
			out = append(out, s.get("Package")+"_"+s.get("Version"))
		}
		return out
	}
	if got := strings.Join(names(e.get(t, "r-all", "src/contrib/PACKAGES", 200)), " "); got != "dup_0.9 dup_1.0 up_1.0" {
		t.Errorf("source index: %s", got)
	}
	if _, rows := readRDS(t, e.get(t, "r-all", "src/contrib/PACKAGES.rds", 200)); len(rows) != 3 || rows[0][1] != "0.9" || rows[2][0] != "up" {
		t.Errorf("source rds: %q", rows)
	}
	// a Windows or macOS client reads the binary directory too: the
	// upstream's dup 9.9 binary would beat the internal source package
	if got := strings.Join(names(e.get(t, "r-all", "bin/windows/contrib/4.5/PACKAGES", 200)), " "); got != "winonly_1.0" {
		t.Errorf("binary index: %s", got)
	}
	// a file is still the first member's that has it
	if w := e.do("GET", "r-all", "src/contrib/dup_1.0.tar.gz", nil); w.Code != 200 || w.Body.String() == "up" {
		t.Errorf("internal file: %d", w.Code)
	}
	// a member with only PACKAGES.gz is merged too, not left out
	if got := strings.Join(names(e.get(t, "r-all", "gz/src/contrib/PACKAGES", 200)), " "); got != "dup_8.8 gzonly_1.0" {
		t.Errorf("gzip-only member: %s", got)
	}
	if got := gunzip(t, e.get(t, "r-all", "gz/src/contrib/PACKAGES.gz", 200)); !strings.Contains(got, "Package: gzonly\n") {
		t.Errorf("gzip-only member, .gz: %s", got)
	}
	if _, rows := readRDS(t, e.get(t, "r-all", "gz/src/contrib/PACKAGES.rds", 200)); len(rows) != 2 {
		t.Errorf("gzip-only member, .rds: %q", rows)
	}
	if w := e.do("PUT", "r-hosted", "gz/src/contrib/gzonly_0.1.tar.gz", tarGz("gzonly", "Package: gzonly\nVersion: 0.1\n", "")); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if got := strings.Join(names(e.get(t, "r-all", "gz/src/contrib/PACKAGES", 200)), " "); got != "gzonly_0.1 dup_8.8" {
		t.Errorf("gzip-only member after an upload: %s", got)
	}
	// a proxy's routing rule sees the path of the request, whichever index file the member has
	var routed []string
	e.h.Resolver.Fetcher.RouteAllowed = func(_ *repo.Repository, p string) bool {
		routed = append(routed, p)
		return p != "/gz/src/contrib/PACKAGES.rds"
	}
	if got := strings.Join(names(e.get(t, "r-all", "gz/src/contrib/PACKAGES", 200)), " "); got != "gzonly_0.1 dup_8.8" {
		t.Errorf("rule allows the path: %s", got)
	}
	for _, p := range routed {
		if p != "/gz/src/contrib/PACKAGES" {
			t.Errorf("the rule saw %s", p)
		}
	}
	if _, rows := readRDS(t, e.get(t, "r-all", "gz/src/contrib/PACKAGES.rds", 200)); len(rows) != 1 || rows[0][0] != "gzonly" {
		t.Errorf("rule refuses the path, the proxy is left out: %q", rows)
	}
	e.h.Resolver.Fetcher.RouteAllowed = nil
	// a merged copy of an older build (its fingerprint from before the rule) is merged again
	old := "Package: stale\nVersion: 1.0\n"
	if _, err := e.h.Store.Put(ctx, "r-all", "src/contrib/PACKAGES", ctText, strings.NewReader(old), func(a *meta.Asset) { a.RemoteETag = "group:abc" }); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(e.get(t, "r-all", "src/contrib/PACKAGES", 200)), " "); got != "dup_0.9 dup_1.0 up_1.0" {
		t.Errorf("after an older build's merge: %s", got)
	}
	// a failing member is left out only when nothing after it answers: a
	// later member never stands in for it
	if w := e.do("GET", "r-failing-first", "src/contrib/PACKAGES", nil); w.Code != http.StatusBadGateway {
		t.Errorf("failing first member: %d %s", w.Code, w.Body)
	}
	if got := strings.Join(names(e.get(t, "r-failing-last", "src/contrib/PACKAGES", 200)), " "); got != "dup_0.9 dup_1.0" {
		t.Errorf("failing last member: %s", got)
	}
}

// TestProxyCacheClasses: a package archive is content, fetched once; every
// other file is metadata, as a mirror rewrites it in place.
func TestProxyCacheClasses(t *testing.T) {
	e := newEnv(t)
	if got := string(e.get(t, "r-proxy", "web/packages/packages.rds", 200)); got != "web-1" {
		t.Fatalf("first: %s", got)
	}
	e.webRDS.Store("web-2")
	if got := string(e.get(t, "r-proxy", "web/packages/packages.rds", 200)); got != "web-1" {
		t.Fatalf("within metadataMaxAge: %s", got)
	}
	e.do("GET", "r-proxy", "src/contrib/up_1.0.tar.gz", nil)
	hits := e.hits.Load()
	e.now = e.now.Add(2 * time.Hour)
	if got := string(e.get(t, "r-proxy", "web/packages/packages.rds", 200)); got != "web-2" {
		t.Fatalf("after metadataMaxAge: %s", got)
	}
	e.do("GET", "r-proxy", "src/contrib/up_1.0.tar.gz", nil)
	if e.hits.Load() != hits+1 {
		t.Fatalf("upstream hits: %d", e.hits.Load()-hits)
	}
	for p, want := range map[string]bool{"web/packages/packages.rds": true, "src/contrib/Views.rds": true, "src/contrib/PACKAGES.json": true, "src/contrib/Meta/archive.rds": true, "bin/windows/base/release.html": true,
		"src/contrib/foo_1.0.tar.gz": false, "bin/windows/contrib/4.4/foo_1.0.zip": false, "bin/macosx/contrib/4.4/foo_1.0.tgz": false, "src/contrib/Archive/foo/foo_0.9.tar.gz": false} {
		if isMetadata(p) != want {
			t.Errorf("isMetadata(%q) = %v", p, !want)
		}
	}
}

func TestCleanupAndDelete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := tarGz("foo", fooDesc, "")
	win := zipPkg("foo", fooDesc)
	if w := e.do("PUT", "r-hosted", "src/contrib/foo_1.0.0.tar.gz", src); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "r-hosted", "bin/windows/contrib/4.4/foo_1.0.0.zip", win); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	e.now = e.now.Add(48 * time.Hour)
	ev := &cleanup.Evaluator{DB: e.db, Now: func() time.Time { return e.now }}
	pol := cleanup.Policy{Name: "p", Format: "r", Criteria: cleanup.Criteria{ComponentAgeDays: 1}}
	if err := pol.Validate(); err != nil {
		t.Fatal(err)
	}
	cands, _, _, _, err := ev.Evaluate(ctx, e.repos["r-hosted"], pol, cleanup.DefaultScanBudget)
	if err != nil || len(cands) != 2 || cands[0].Label() != "bin/windows/contrib/4.4/foo@1.0.0" || cands[1].Label() != "src/contrib/foo@1.0.0" || len(cands[1].Assets) != 1 {
		t.Fatalf("candidates: %+v %v", cands, err) // the index files are not components
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["r-proxy"], cands[1].Component, "system"); !errors.Is(err, component.ErrNotSupported) {
		t.Fatalf("proxy: %v", err)
	}
	for _, c := range []component.Component{{Format: "r", Name: "foo", Version: "1.0.0"}, {Format: "r", Group: "src/contrib", Name: "foo"}, {Format: "r", Group: "src", Name: "foo", Version: "1.0.0"}} {
		if _, err := e.h.DeleteComponent(ctx, e.repos["r-hosted"], c, "system"); err == nil {
			t.Fatalf("incomplete component %+v deleted", c)
		}
	}
	// downloaded since the evaluation: skipped
	if err := e.db.FlushDownloads(ctx, []meta.DownloadDelta{{Repo: "r-hosted", Path: "src/contrib/foo_1.0.0.tar.gz", N: 1, Last: e.now}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["r-hosted"], cands[1].Component, "system"); !errors.Is(err, cleanup.ErrChanged) {
		t.Fatalf("changed: %v", err)
	}
	cands, _, _, _, _ = ev.Evaluate(ctx, e.repos["r-hosted"], pol, cleanup.DefaultScanBudget)
	e.sink.Reset()
	res, err := e.h.DeleteComponent(ctx, e.repos["r-hosted"], cands[1].Component, "system")
	if err != nil || res.Assets != 1 || res.Bytes != int64(len(src)) {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if got := e.sink.Events(); len(got) != 1 || got[0] != (repotest.Event{Kind: "delete", Actor: "system", Repo: "r-hosted", Subject: "src/contrib/foo@1.0.0", Detail: "assets=1"}) {
		t.Fatalf("events: %v", got)
	}
	e.get(t, "r-hosted", "src/contrib/foo_1.0.0.tar.gz", 404)
	if pkgs, _ := e.db.ListPackages(ctx, "r-hosted", "src/contrib/foo"); len(pkgs) != 0 {
		t.Fatalf("rows left: %+v", pkgs)
	}
	if got := e.get(t, "r-hosted", "src/contrib/PACKAGES", 200); len(got) != 0 {
		t.Fatalf("PACKAGES after delete: %q", got)
	}
	if _, rows := readRDS(t, e.get(t, "r-hosted", "src/contrib/PACKAGES.rds", 200)); len(rows) != 0 {
		t.Fatalf("PACKAGES.rds after delete: %q", rows)
	}
	// the other directory is untouched
	if got := string(e.get(t, "r-hosted", "bin/windows/contrib/4.4/PACKAGES", 200)); !strings.Contains(got, "MD5sum: "+sums(win).MD5) {
		t.Fatalf("windows PACKAGES: %s", got)
	}
	// gone already: no error, nothing counted
	if res, err := e.h.DeleteComponent(ctx, e.repos["r-hosted"], cands[1].Component, "system"); err != nil || res.Assets != 0 {
		t.Fatalf("again: %+v %v", res, err)
	}
	// the attrs hold what the metadata API reads
	var rw row
	pkgs, _ := e.db.ListPackages(ctx, "r-hosted", "bin/windows/contrib/4.4/foo")
	if len(pkgs) != 1 || json.Unmarshal([]byte(pkgs[0].Attrs), &rw) != nil || rw.field("Title") != "Foo Does Things" || rw.Dir != "bin/windows/contrib/4.4" {
		t.Fatalf("attrs: %+v", pkgs)
	}
}
