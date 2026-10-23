package golang

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"

	"commandcenter/internal/repo"
)

func TestModulePath(t *testing.T) {
	cases := map[string]string{
		"module example.com/hello\n\ngo 1.21\n":       "example.com/hello",
		"// comment\nmodule \"example.com/q\" // x\n": "example.com/q",
		"go 1.21\nmodule   example.com/late\n":        "example.com/late",
	}
	for in, want := range cases {
		if got, err := modulePath(strings.NewReader(in)); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	if _, err := modulePath(strings.NewReader("go 1.21\n")); err == nil {
		t.Error("missing module directive must fail")
	}
}

func makeZip(t *testing.T, files map[string]string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	return bytes.NewReader(buf.Bytes())
}

func TestCheckZip(t *testing.T) {
	good := makeZip(t, map[string]string{
		"example.com/Hello@v1.0.0/go.mod":   "module example.com/Hello\n",
		"example.com/Hello@v1.0.0/hello.go": "package hello\n",
	})
	if err := checkZip(good, good.Size(), "example.com/Hello", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	bad := map[string]map[string]string{
		"outside prefix": {"other@v1.0.0/x.go": "package x"},
		"wrong module":   {"example.com/Hello@v1.0.0/go.mod": "module example.com/other\n"},
		"dot dot":        {"example.com/Hello@v1.0.0/../x.go": "package x"},
	}
	for name, files := range bad {
		z := makeZip(t, files)
		if err := checkZip(z, z.Size(), "example.com/Hello", "v1.0.0"); repo.StatusOf(err) != 400 {
			t.Errorf("%s: %v", name, err)
		}
	}
	notZip := bytes.NewReader([]byte("not a zip"))
	if err := checkZip(notZip, notZip.Size(), "example.com/Hello", "v1.0.0"); repo.StatusOf(err) != 400 {
		t.Errorf("not a zip: %v", err)
	}
}

func TestVersionHelpers(t *testing.T) {
	got := sortVersions([]string{"v1.10.0", "v1.2.0", "junk", "v1.2.0", "v2.0.0-beta.1", "v1.0.0+incompatible"})
	if strings.Join(got, " ") != "v1.0.0+incompatible v1.2.0 v1.10.0 v2.0.0-beta.1 junk" {
		t.Fatalf("sorted = %v", got)
	}
	if v := pickLatest([]string{"v1.0.0", "v2.0.0-beta.1", "v1.5.0"}); v != "v1.5.0" {
		t.Fatalf("latest release = %q", v)
	}
	if v := pickLatest([]string{"v2.0.0-beta.1", "v2.0.0-alpha"}); v != "v2.0.0-beta.1" {
		t.Fatalf("latest prerelease = %q", v)
	}
	if v := pickLatest(nil); v != "" {
		t.Fatalf("empty = %q", v)
	}
}

func TestCheckZipDirectoryBound(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < 60000; i++ {
		if _, err := zw.Create("example.com/Hello@v1.0.0/f" + strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	zw.Close()
	r := bytes.NewReader(buf.Bytes())
	err := checkZip(r, r.Size(), "example.com/Hello", "v1.0.0")
	if repo.StatusOf(err) != 400 || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("too many entries: %v", err)
	}
}

// eocd returns the offset of the end-of-central-directory record in z.
func eocd(t *testing.T, z []byte) int {
	t.Helper()
	i := bytes.LastIndex(z, []byte{0x50, 0x4b, 0x05, 0x06})
	if i < 0 {
		t.Fatal("no EOCD")
	}
	return i
}

func TestCheckDirectoryRejectsInconsistentRecords(t *testing.T) {
	good := makeZip(t, map[string]string{"example.com/a@v1.0.0/go.mod": "module example.com/a\n"})
	raw := make([]byte, good.Size())
	good.ReadAt(raw, 0)
	if err := checkDirectory(bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatalf("valid zip: %v", err)
	}
	patch := func(off int, v uint32, width int) []byte {
		z := append([]byte(nil), raw...)
		if width == 2 {
			binary.LittleEndian.PutUint16(z[off:], uint16(v))
		} else {
			binary.LittleEndian.PutUint32(z[off:], v)
		}
		return z
	}
	e := eocd(t, raw)
	cases := map[string][]byte{
		"directory size too small": patch(e+12, 1, 4),
		"zip64 offset sentinel":    patch(e+16, 0xFFFFFFFF, 4),
		"comment does not fit":     patch(e+20, 0xFFFF, 2),
	}
	for name, z := range cases {
		if err := checkDirectory(bytes.NewReader(z), int64(len(z))); repo.StatusOf(err) != 400 {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheckDirectoryRejectsZip64LocatorOutsideTail(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("example.com/a@v1.0.0/go.mod")
	w.Write([]byte("module example.com/a\n"))
	zw.SetComment(strings.Repeat("x", 65530))
	zw.Close()
	z := buf.Bytes()
	if err := checkDirectory(bytes.NewReader(z), int64(len(z))); err != nil {
		t.Fatalf("valid zip with long comment: %v", err)
	}
	e := eocd(t, z)
	if e >= len(z)-65557+20 {
		t.Fatalf("EOCD at %d is inside the tail window", e)
	}
	binary.LittleEndian.PutUint32(z[e-20:], 0x07064b50)
	err := checkDirectory(bytes.NewReader(z), int64(len(z)))
	if repo.StatusOf(err) != 400 || !strings.Contains(err.Error(), "zip64") {
		t.Fatalf("locator outside the tail window: %v", err)
	}
	// archive/zip also treats a directory size of 0xFFFF as a zip64 trigger.
	good := makeZip(t, map[string]string{"example.com/a@v1.0.0/go.mod": "module example.com/a\n"})
	raw := make([]byte, good.Size())
	good.ReadAt(raw, 0)
	binary.LittleEndian.PutUint32(raw[eocd(t, raw)+12:], 0xFFFF)
	if err := checkDirectory(bytes.NewReader(raw), int64(len(raw))); repo.StatusOf(err) != 400 || !strings.Contains(err.Error(), "zip64") {
		t.Fatalf("dirSize 0xFFFF: %v", err)
	}
}

// rawZip builds a zip whose entries are the given names, in order, with
// small bodies; sizes, when non-nil, forge the declared uncompressed size.
func rawZip(t *testing.T, names []string, sizes []uint64) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i, n := range names {
		if sizes == nil {
			w, err := zw.Create(n)
			if err != nil {
				t.Fatal(err)
			}
			w.Write([]byte("package x\n"))
			continue
		}
		w, err := zw.CreateRaw(&zip.FileHeader{Name: n, Method: zip.Store, CompressedSize64: 0, UncompressedSize64: sizes[i]})
		if err != nil {
			t.Fatal(err)
		}
		_ = w
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func TestCheckZipRejects(t *testing.T) {
	const p = "example.com/Hello@v1.0.0/"
	cases := map[string]*bytes.Reader{
		"duplicate name":     rawZip(t, []string{p + "a.go", p + "a.go"}, nil),
		"case-fold clash":    rawZip(t, []string{p + "A.go", p + "a.go"}, nil),
		"backslash":          rawZip(t, []string{p + `a\b.go`}, nil),
		"colon":              rawZip(t, []string{p + "a:b.go"}, nil),
		"double slash":       rawZip(t, []string{p + "/y.go"}, nil),
		"reserved name":      rawZip(t, []string{p + "con.go"}, nil),
		"reserved dir":       rawZip(t, []string{p + "Aux/x.go"}, nil),
		"reserved com1":      rawZip(t, []string{p + "COM1.txt"}, nil),
		"520 MiB uncompress": rawZip(t, []string{p + "a.bin", p + "b.bin"}, []uint64{260 << 20, 260 << 20}),
	}
	for name, z := range cases {
		if err := checkZip(z, z.Size(), "example.com/Hello", "v1.0.0"); repo.StatusOf(err) != 400 {
			t.Errorf("%s: %v", name, err)
		}
	}
	ok := rawZip(t, []string{p + "a.bin", p + "b.bin"}, []uint64{250 << 20, 250 << 20})
	if err := checkZip(ok, ok.Size(), "example.com/Hello", "v1.0.0"); err != nil {
		t.Errorf("500 MiB total must pass: %v", err)
	}
}

func TestCheckFilePath(t *testing.T) {
	for _, p := range []string{"a.go", "a/b/c.go", "go.mod", "console.go", "x/nul2/y.go"} {
		if err := checkFilePath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range []string{"", "/a", "a//b", "a/", `a\b`, "a:b", "con", "NUL.txt", "lpt9.x", "a/prn/b"} {
		if err := checkFilePath(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}
