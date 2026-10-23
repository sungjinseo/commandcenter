package pypi

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func wheelBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, c := range files {
		w, _ := zw.Create(n)
		w.Write([]byte(c))
	}
	zw.Close()
	return buf.Bytes()
}

func sdistBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, c := range files {
		tw.WriteHeader(&tar.Header{Name: n, Size: int64(len(c)), Mode: 0o644, Typeflag: tar.TypeReg})
		tw.Write([]byte(c))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestReadRequiresPython(t *testing.T) {
	meta := "Metadata-Version: 2.1\nName: x\nRequires-Python: >=3.99\n\nRequires-Python: >=1\n"
	whl := wheelBytes(t, map[string]string{"x-1.0.dist-info/METADATA": meta})
	got, err := readRequiresPython(bytes.NewReader(whl), int64(len(whl)), "x-1.0-py3-none-any.whl")
	if err != nil || got != ">=3.99" {
		t.Fatalf("wheel: %q %v", got, err)
	}
	sd := sdistBytes(t, map[string]string{"x-1.0/PKG-INFO": meta, "x-1.0/sub/PKG-INFO": "Requires-Python: >=2\n"})
	got, err = readRequiresPython(bytes.NewReader(sd), int64(len(sd)), "x-1.0.tar.gz")
	if err != nil || got != ">=3.99" {
		t.Fatalf("sdist: %q %v", got, err)
	}
	zs := wheelBytes(t, map[string]string{"x-1.0/PKG-INFO": meta})
	if got, _ := readRequiresPython(bytes.NewReader(zs), int64(len(zs)), "x-1.0.zip"); got != ">=3.99" {
		t.Fatalf("zip sdist: %q", got)
	}
	// absent, not an archive
	none := wheelBytes(t, map[string]string{"x-1.0.dist-info/METADATA": "Name: x\n"})
	if got, err := readRequiresPython(bytes.NewReader(none), int64(len(none)), "x-1.0.whl"); got != "" || err != nil {
		t.Fatalf("absent: %q %v", got, err)
	}
	if got, err := readRequiresPython(strings.NewReader("junk"), 4, "x-1.0.tar.gz"); got != "" || err != nil {
		t.Fatalf("junk: %q %v", got, err)
	}
}

func TestValidVersionAndSpecifier(t *testing.T) {
	for _, v := range []string{"1.0", "1.0.0", "2.0.0rc1", "1!2.0", "1.0.post1", "1.0.dev3", "1.0+Local.1", "1.0A1"} {
		if !validVersion(v) {
			t.Errorf("want valid %q", v)
		}
	}
	for _, v := range []string{"", "1.0ü", "3.0.0 ", "v1", "1.0-1", "a", "01.0"} {
		if validVersion(v) {
			t.Errorf("want invalid %q", v)
		}
	}
	for _, s := range []string{">=3.8", ">=3.8, <4", "~=3.9.1", "!=3.0.*", "==3.*"} {
		if !validSpecifier(s) {
			t.Errorf("want valid spec %q", s)
		}
	}
	for _, s := range []string{`>=3"><script>`, "3.8", ">=", "<a b>"} {
		if validSpecifier(s) {
			t.Errorf("want invalid spec %q", s)
		}
	}
}

func TestCanonicalFile(t *testing.T) {
	if canonicalFile("qa_sec_shadow-1.0.tar.gz") != canonicalFile("QA.SEC.SHADOW-1.0.TAR.GZ") {
		t.Fatal("spellings must collapse")
	}
	if canonicalFile("x-1.0-py3-none-any.whl") == canonicalFile("x-1.0-cp39-linux_x86_64.whl") {
		t.Fatal("different wheel tags are different files")
	}
}

func TestReadRequiresPythonSkipsHugeZipDirectory(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < maxEntries+1; i++ {
		zw.Create("d/" + strconv.Itoa(i))
	}
	w, _ := zw.Create("x-1.0.dist-info/METADATA")
	w.Write([]byte("Requires-Python: >=3.1\n"))
	zw.Close()
	data := buf.Bytes()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := readRequiresPython(bytes.NewReader(data), int64(len(data)), "x-1.0.whl")
	runtime.ReadMemStats(&after)
	if got != "" || err != nil {
		t.Fatalf("got %q %v", got, err)
	}
	if d := after.TotalAlloc - before.TotalAlloc; d > 1<<20 {
		t.Fatalf("allocated %d bytes: zip directory must not be parsed", d)
	}
}

func TestReadRequiresPythonCapsDecompression(t *testing.T) {
	old := maxDecompressed
	maxDecompressed = 1 << 20
	defer func() { maxDecompressed = old }()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "x-1.0/big", Size: 8 << 20, Mode: 0o644, Typeflag: tar.TypeReg})
	tw.Write(make([]byte, 8<<20))
	tw.WriteHeader(&tar.Header{Name: "x-1.0/PKG-INFO", Size: 22, Mode: 0o644, Typeflag: tar.TypeReg})
	tw.Write([]byte("Requires-Python: >=3.1\n"[:22]))
	tw.Close()
	gz.Close()
	start := time.Now()
	got, err := readRequiresPython(bytes.NewReader(buf.Bytes()), int64(buf.Len()), "x-1.0.tar.gz")
	if got != "" || err != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("got %q %v in %v", got, err, time.Since(start))
	}
}

func TestCanonicalFileDistinctAndEqual(t *testing.T) {
	distinct := [][2]string{
		{"x-1.0-1-py3-none-any.whl", "x-1.0.1-py3-none-any.whl"},
		{"x-1.0-py2.py3-none-any.whl", "x-1.0-py2-py3-none-any.whl"},
	}
	for _, p := range distinct {
		if canonicalFile(p[0]) == canonicalFile(p[1]) {
			t.Errorf("%s and %s must differ", p[0], p[1])
		}
	}
	same := [][2]string{
		{"pkg-1.0.tar.gz", "pkg-1.0.0.tar.gz"},
		{"qa_sec_shadow-1.0.tar.gz", "QA.SEC.SHADOW-1.0.tar.gz"},
		{"Pkg-1.0.TAR.GZ", "pkg-1.0.0.0.tar.gz"},
	}
	for _, p := range same {
		if canonicalFile(p[0]) != canonicalFile(p[1]) {
			t.Errorf("%s and %s must match", p[0], p[1])
		}
	}
	if canonicalFile("pkg-1.0.1.tar.gz") == canonicalFile("pkg-1.0.tar.gz") || canonicalFile("pkg-10.tar.gz") == canonicalFile("pkg-1.tar.gz") {
		t.Error("distinct versions collided")
	}
}

// zip64Wheel rewrites a valid one-entry wheel so that its EOCD triggers the
// zip64 path (via the count or the size field) and a zip64 record claims
// count entries. noLocator drops the locator and record.
func zip64Wheel(t *testing.T, trigger string, count uint64, noLocator bool) []byte {
	t.Helper()
	base := wheelBytes(t, map[string]string{"x-1.0.dist-info/METADATA": "Requires-Python: >=3.7\n"})
	eocd := append([]byte(nil), base[len(base)-22:]...)
	body := append([]byte(nil), base[:len(base)-22]...)
	dirSize := uint64(binary.LittleEndian.Uint32(eocd[12:]))
	dirOff := uint64(binary.LittleEndian.Uint32(eocd[16:]))
	z64 := uint64(len(body))
	switch trigger {
	case "count":
		binary.LittleEndian.PutUint16(eocd[10:], 0xffff)
	case "size":
		binary.LittleEndian.PutUint32(eocd[12:], 0xffff)
	}
	if !noLocator {
		rec := make([]byte, 56)
		copy(rec, "PK\x06\x06")
		binary.LittleEndian.PutUint64(rec[4:], 44)
		binary.LittleEndian.PutUint64(rec[24:], count)
		binary.LittleEndian.PutUint64(rec[32:], count)
		binary.LittleEndian.PutUint64(rec[40:], dirSize)
		binary.LittleEndian.PutUint64(rec[48:], dirOff)
		loc := make([]byte, 20)
		copy(loc, "PK\x06\x07")
		binary.LittleEndian.PutUint64(loc[8:], z64)
		binary.LittleEndian.PutUint32(loc[16:], 1)
		body = append(append(body, rec...), loc...)
	}
	return append(body, eocd...)
}

func TestZip64TriggersAreChecked(t *testing.T) {
	read := func(b []byte) string {
		got, err := readRequiresPython(bytes.NewReader(b), int64(len(b)), "x-1.0.whl")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(zip64Wheel(t, "count", 1, false)); got != ">=3.7" {
		t.Fatalf("honest zip64 wheel: %q", got)
	}
	for _, trigger := range []string{"count", "size"} {
		b := zip64Wheel(t, trigger, 50_000_000, false)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		got := read(b)
		runtime.ReadMemStats(&after)
		if got != "" || after.TotalAlloc-before.TotalAlloc > 1<<20 {
			t.Errorf("%s: got %q, alloc %d", trigger, got, after.TotalAlloc-before.TotalAlloc)
		}
		if got := read(zip64Wheel(t, trigger, 1, true)); got != "" {
			t.Errorf("%s without zip64 record must be skipped, got %q", trigger, got)
		}
	}
}

func TestCanonicalVersionSpellings(t *testing.T) {
	for _, p := range [][2]string{
		{"x-1.0.post1.tar.gz", "x-1.0.0.post1.tar.gz"},
		{"x-2.0.dev3.tar.gz", "x-2.0.0.dev3.tar.gz"},
	} {
		if canonicalFile(p[0]) != canonicalFile(p[1]) {
			t.Errorf("%s and %s must match", p[0], p[1])
		}
	}
	if canonicalFile("x-1.0.post1.tar.gz") == canonicalFile("x-1.0.dev1.tar.gz") {
		t.Error("post and dev must differ")
	}
}

// Final review I2: the header block is read up to the first blank line only.
func TestRequiresPythonIgnoresLargeBody(t *testing.T) {
	body := strings.Repeat("long description\n", (3<<20)/17)
	sd := sdistBytes(t, map[string]string{"x-1.0/PKG-INFO": "Metadata-Version: 2.1\nName: x\nRequires-Python: >=3.10\n\n" + body})
	if got, err := readRequiresPython(bytes.NewReader(sd), int64(len(sd)), "x-1.0.tar.gz"); got != ">=3.10" || err != nil {
		t.Fatalf("sdist: %q %v", got, err)
	}
	huge := "Metadata-Version: 2.1\nSummary: " + strings.Repeat("a", maxMetadata) + "\nRequires-Python: >=3.10\n\n"
	whl := wheelBytes(t, map[string]string{"x-1.0.dist-info/METADATA": huge})
	if got, err := readRequiresPython(bytes.NewReader(whl), int64(len(whl)), "x-1.0.whl"); got != "" || err != nil {
		t.Fatalf("oversized header block: %q %v", got, err)
	}
}
