package pypi

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/repo"
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

// readMeta runs readMetadata for a distribution of project x at version 1.0.
func readMeta(b []byte, filename string) (distMeta, error) {
	return readMetadata(bytes.NewReader(b), int64(len(b)), filename, "x", "1.0")
}

func TestReadMetadata(t *testing.T) {
	meta := "Metadata-Version: 2.1\nName: x\nVersion: 1.0\nRequires-Python: >=3.99\n\nRequires-Python: >=1\n"
	want := distMeta{Name: "x", Version: "1.0", RequiresPython: ">=3.99"}
	whl := wheelBytes(t, map[string]string{"x-1.0.dist-info/METADATA": meta, "x/__init__.py": ""})
	if got, err := readMeta(whl, "x-1.0-py3-none-any.whl"); err != nil || got != want {
		t.Fatalf("wheel: %+v %v", got, err)
	}
	sd := sdistBytes(t, map[string]string{"x-1.0/PKG-INFO": meta, "x-1.0/sub/PKG-INFO": "Requires-Python: >=2\n"})
	if got, err := readMeta(sd, "x-1.0.tar.gz"); err != nil || got != want {
		t.Fatalf("sdist: %+v %v", got, err)
	}
	zs := wheelBytes(t, map[string]string{"x-1.0/PKG-INFO": meta})
	if got, err := readMeta(zs, "x-1.0.zip"); err != nil || got != want {
		t.Fatalf("zip sdist: %+v %v", got, err)
	}
	// Requires-Python absent; a folded line (here of an old-style Description) is no header.
	none := wheelBytes(t, map[string]string{"x-1.0.dist-info/METADATA": "Name: x\nVersion: 1.0\nDescription: a\n        Requires-Python: banana\n"})
	if got, err := readMeta(none, "x-1.0-py3-none-any.whl"); err != nil || got.RequiresPython != "" {
		t.Fatalf("absent: %+v %v", got, err)
	}
	// The release directory may be spelled differently from the file name.
	sd = sdistBytes(t, map[string]string{"./X-1.0.0/PKG-INFO": meta})
	if got, err := readMeta(sd, "x-1.0.tar.gz"); err != nil || got != want {
		t.Fatalf("respelled release directory: %+v %v", got, err)
	}
}

// QA M-9: a file that is not a distribution of the release is a 400.
func TestReadMetadataRejects(t *testing.T) {
	meta := "Metadata-Version: 2.1\nName: x\nVersion: 1.0\n"
	for name, c := range map[string]struct {
		data []byte
		file string
	}{
		"text as wheel":               {[]byte("x"), "x-1.0-py3-none-any.whl"},
		"text as sdist":               {[]byte("x"), "x-1.0.tar.gz"},
		"text as zip sdist":           {[]byte("x"), "x-1.0.zip"},
		"sdist as wheel":              {sdistBytes(t, map[string]string{"x-1.0/PKG-INFO": meta}), "x-1.0-py3-none-any.whl"},
		"gzip but not tar":            {gzipBytes(t, "x"), "x-1.0.tar.gz"},
		"wheel without METADATA":      {wheelBytes(t, map[string]string{"x-1.0.dist-info/RECORD": ""}), "x-1.0-py3-none-any.whl"},
		"wheel without dist-info":     {wheelBytes(t, map[string]string{"x/__init__.py": ""}), "x-1.0-py3-none-any.whl"},
		"nested METADATA only":        {wheelBytes(t, map[string]string{"a/x-1.0.dist-info/METADATA": meta}), "x-1.0-py3-none-any.whl"},
		"two dist-info directories":   {wheelBytes(t, map[string]string{"x-1.0.dist-info/METADATA": meta, "y-1.0.dist-info/RECORD": ""}), "x-1.0-py3-none-any.whl"},
		"METADATA without Version":    {wheelBytes(t, map[string]string{"x-1.0.dist-info/METADATA": "Name: x\n"}), "x-1.0-py3-none-any.whl"},
		"Version only after the body": {wheelBytes(t, map[string]string{"x-1.0.dist-info/METADATA": "Name: x\n\nVersion: 1.0\n"}), "x-1.0-py3-none-any.whl"},
		"sdist without PKG-INFO":      {sdistBytes(t, map[string]string{"x-1.0/setup.py": ""}), "x-1.0.tar.gz"},
		"PKG-INFO of another project": {sdistBytes(t, map[string]string{"y-1.0/PKG-INFO": meta}), "x-1.0.tar.gz"},
		"PKG-INFO of another version": {wheelBytes(t, map[string]string{"x-2.0/PKG-INFO": meta}), "x-1.0.zip"},
		"PKG-INFO at the top":         {sdistBytes(t, map[string]string{"PKG-INFO": meta}), "x-1.0.tar.gz"},
	} {
		if _, err := readMeta(c.data, c.file); repo.StatusOf(err) != http.StatusBadRequest {
			t.Errorf("%s: %v, want a 400", name, err)
		}
	}
	two := zipWithDuplicate(t, "x-1.0.dist-info/METADATA", meta)
	if _, err := readMeta(two, "x-1.0-py3-none-any.whl"); repo.StatusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "more than one") {
		t.Errorf("two METADATA entries: %v", err)
	}
}

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(s))
	gz.Close()
	return buf.Bytes()
}

// zipWithDuplicate is a zip holding the entry name twice.
func zipWithDuplicate(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < 2; i++ {
		w, _ := zw.Create(name)
		w.Write([]byte(content))
	}
	zw.Close()
	return buf.Bytes()
}

func TestValidVersionAndSpecifier(t *testing.T) {
	// QA N-7: the PEP 440 grammar, so "1.0-1" and "01.0" are valid now.
	for _, v := range []string{"1.0", "1.0.0", "2.0.0rc1", "1!2.0", "1.0.post1", "1.0.dev3", "1.0+Local.1", "1.0A1", "1.0-1", "01.0"} {
		if !validVersion(v) {
			t.Errorf("want valid %q", v)
		}
	}
	for _, v := range []string{"", "1.0ü", "3.0.0 ", "v1", "V1.3", "a", "1.0+", "1..0", "1.0.", "1.0-", "1.0+a..b", "1.0-1-1", "1.0+\u212a", "1!", strings.Repeat("1", 101)} {
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

// QA N-7: versions are stored and compared in PEP 440 normal form.
func TestNormalizeVersion(t *testing.T) {
	for in, want := range map[string]string{
		"1.0-1": "1.0.post1", "01.2": "1.2", "2.0a": "2.0a0", "1.0post2": "1.0.post2", "1.0.dev": "1.0.dev0",
		"1.0RC2": "1.0rc2", "1.0.0": "1.0.0", "0!1.0": "1.0", "007!1.0": "7!1.0", "1.0alpha1": "1.0a1",
		"1.0-beta.2": "1.0b2", "1.0c1": "1.0rc1", "1.0pre": "1.0rc0", "1.0preview_3": "1.0rc3",
		"1.0-r4": "1.0.post4", "1.0rev": "1.0.post0", "1.0_post_5": "1.0.post5", "1.0-0": "1.0.post0",
		"1.0a1-1.dev_2": "1.0a1.post1.dev2", "1.0+Ubuntu-01_x.007": "1.0+ubuntu.1.x.7", "1.0+01a": "1.0+01a",
		"99999999999999999999.0": "99999999999999999999.0",
	} {
		if got, ok := normalizeVersion(in); !ok || got != want {
			t.Errorf("normalizeVersion(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for in, want := range map[string]string{
		"1.0": "1", "1.0.0": "1", "1.0-1": "1.post1", "1.0.0.post1": "1.post1", "0.0": "0", "10.0": "10",
		"2!1.0a": "2!1a0", "1.0+local": "1+local", "1.10": "1.10", "1.0.dev": "1.dev0", "x": "x", // invalid: unchanged
	} {
		if got := canonicalVersion(in); got != want {
			t.Errorf("canonicalVersion(%q) = %q, want %q", in, got, want)
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

func TestReadMetadataRefusesHugeZipDirectory(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < maxEntries+1; i++ {
		zw.Create("d/" + strconv.Itoa(i))
	}
	w, _ := zw.Create("x-1.0.dist-info/METADATA")
	w.Write([]byte("Name: x\nVersion: 1.0\nRequires-Python: >=3.1\n"))
	zw.Close()
	data := buf.Bytes()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := readMeta(data, "x-1.0-py3-none-any.whl")
	runtime.ReadMemStats(&after)
	if repo.StatusOf(err) != http.StatusBadRequest { // it cannot be checked, so it is refused
		t.Fatalf("got %v, want a 400", err)
	}
	if d := after.TotalAlloc - before.TotalAlloc; d > 1<<20 {
		t.Fatalf("allocated %d bytes: zip directory must not be parsed", d)
	}
}

func TestReadMetadataCapsDecompression(t *testing.T) {
	old := maxDecompressed
	maxDecompressed = 1 << 20
	defer func() { maxDecompressed = old }()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "x-1.0/big", Size: 8 << 20, Mode: 0o644, Typeflag: tar.TypeReg})
	tw.Write(make([]byte, 8<<20))
	meta := "Name: x\nVersion: 1.0\n"
	tw.WriteHeader(&tar.Header{Name: "x-1.0/PKG-INFO", Size: int64(len(meta)), Mode: 0o644, Typeflag: tar.TypeReg})
	tw.Write([]byte(meta))
	tw.Close()
	gz.Close()
	start := time.Now()
	_, err := readMeta(buf.Bytes(), "x-1.0.tar.gz")
	if repo.StatusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "first 1 MiB") || time.Since(start) > 2*time.Second {
		t.Fatalf("got %v in %v", err, time.Since(start))
	}
}

func TestCanonicalFileDistinctAndEqual(t *testing.T) {
	distinct := [][2]string{
		{"x-1.0-1-py3-none-any.whl", "x-1.0.1-py3-none-any.whl"},
		{"x-1.0-py2.py3-none-any.whl", "x-1.0-py2-py3-none-any.whl"},
		{"x-1.0-1-py3-none-any.whl", "x-1.0-py3-none-any.whl"}, // a build tag sorts after none
		{"x-1.0-1a-py3-none-any.whl", "x-1.0-1-py3-none-any.whl"},
		{"x-1.0-py2.py3-none-any.whl", "x-1.0-py3-none-any.whl"},
		{"x-1.0.tar.gz", "x-1.0-py3-none-any.whl"},
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
		// QA M-8: the same wheel tag set or build number spelled differently,
		// and a second sdist of a release.
		{"x-1.0-py3.py3-none-any.whl", "x-1.0-py3-none-any.whl"},
		{"x-1.0-py3.py2-none-any.whl", "x-1.0-py2.py3-none-any.whl"},
		{"x-1.0-PY3-none-ANY.whl", "x-1.0-py3-none-any.whl"},
		{"x-1.0-cp39-abi3.abi3-manylinux_2_17_x86_64.manylinux2014_x86_64.whl", "x-1.0-cp39-abi3-manylinux2014_x86_64.manylinux_2_17_x86_64.whl"},
		{"x-1.0-01-py3-none-any.whl", "x-1.0-1-py3-none-any.whl"},
		{"x-1.0-0-py3-none-any.whl", "x-1.0-000-py3-none-any.whl"},
		{"x-1.0.zip", "x-1.0.tar.gz"},
		{"x-1.0-1.tar.gz", "x-1.0.post1.zip"},
		{"x-01.2.tar.gz", "x-1.2.tar.gz"},
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
	base := wheelBytes(t, map[string]string{"x-1.0.dist-info/METADATA": "Name: x\nVersion: 1.0\nRequires-Python: >=3.7\n"})
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
		md, err := readMeta(b, "x-1.0-py3-none-any.whl")
		if err != nil && repo.StatusOf(err) != http.StatusBadRequest {
			t.Fatal(err)
		}
		return md.RequiresPython // "" when refused
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
	sd := sdistBytes(t, map[string]string{"x-1.0/PKG-INFO": "Metadata-Version: 2.1\nName: x\nVersion: 1.0\nRequires-Python: >=3.10\n\n" + body})
	if md, err := readMeta(sd, "x-1.0.tar.gz"); md.RequiresPython != ">=3.10" || err != nil {
		t.Fatalf("sdist: %+v %v", md, err)
	}
	huge := "Metadata-Version: 2.1\nName: x\nVersion: 1.0\nSummary: " + strings.Repeat("a", maxMetadata) + "\nRequires-Python: >=3.10\n\n"
	whl := wheelBytes(t, map[string]string{"x-1.0.dist-info/METADATA": huge})
	if md, err := readMeta(whl, "x-1.0-py3-none-any.whl"); md.Name != "x" || md.RequiresPython != "" || err != nil {
		t.Fatalf("oversized header block: %+v %v", md, err)
	}
	// Name and Version past the cap are not seen: refused.
	late := "Metadata-Version: 2.1\nSummary: " + strings.Repeat("a", maxMetadata) + "\nName: x\nVersion: 1.0\n\n"
	whl = wheelBytes(t, map[string]string{"x-1.0.dist-info/METADATA": late})
	if _, err := readMeta(whl, "x-1.0-py3-none-any.whl"); repo.StatusOf(err) != http.StatusBadRequest {
		t.Fatalf("Name past the cap: %v", err)
	}
}
