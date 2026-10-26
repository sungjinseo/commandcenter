package golang

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"hash/crc32"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/repo"
)

func TestModulePath(t *testing.T) {
	cases := map[string]string{
		"module example.com/hello\n\ngo 1.21\n":       "example.com/hello",
		"// comment\nmodule \"example.com/q\" // x\n": "example.com/q",
		"go 1.21\nmodule   example.com/late\n":        "example.com/late",
		// every directive Go 1.26 knows, in lines and blocks, with CRLF
		"module example.com/full // Deprecated: use v2\r\n\r\ngo 1.24.0\r\ntoolchain go1.24.1\r\n" +
			"godebug default=go1.21\r\nrequire (\r\n\texample.com/a v1.0.0\r\n\texample.com/b v1.2.0 // indirect\r\n)\r\n" +
			"require example.com/c v0.1.0\r\nexclude example.com/a v0.9.0\r\nreplace example.com/b => ../b\r\n" +
			"retract [v0.1.0, v0.2.0] // broken\r\nretract (\r\n\tv0.0.1\r\n)\r\ntool example.com/a/cmd/x\r\n" +
			"ignore ./third_party\r\nrequire ()\r\n": "example.com/full",
		"module (\n\texample.com/block\n)\n":              "example.com/block",
		"module example.com/lax\ngo 1.21.x\n":             "example.com/lax",
		"module example.com/rc\ngo 1.26rc1\n":             "example.com/rc",
		"module example.com/s\nrequire \"x//y\" v1.0.0\n": "example.com/s",
		// a newer Go may add directives; the go command then asks for it
		"module example.com/new\nfuture x\nfuture (\n\ty\n)\ngo 1.27\n": "example.com/new",
	}
	for in, want := range cases {
		if got, err := modulePath(strings.NewReader(in)); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for name, in := range map[string]string{
		"no module directive":   "go 1.21\n",
		"unterminated block":    "module example.com/a\nrequire (\n\texample.com/b v1.0.0\n",
		"stray paren":           "module example.com/a\n)\n",
		"nested block":          "module example.com/a\nrequire (\n\tfoo (\n)\n)\n",
		"text after paren":      "module example.com/a\nrequire (\n\texample.com/b v1.0.0\n) x\n",
		"unknown directive":     "module example.com/a\nrequre example.com/b v1.0.0\n",
		"unknown block type":    "module example.com/a\nfoo (\n)\n",
		"go block":              "module example.com/a\ngo (\n\t1.21\n)\n",
		"repeated module":       "module example.com/a\nmodule example.com/a\n",
		"module with two paths": "module example.com/a example.com/b\n",
		"module in quotes":      "module 'example.com/a'\n",
		"unterminated string":   "module \"example.com/a\n",
		"repeated go":           "module example.com/a\ngo 1.21\ngo 1.22\n",
		"bad go version":        "module example.com/a\ngo banana\n",
		"block comment":         "module example.com/a\n/* c */\n",
		"control character":     "module example.com/a\x00\n",
		"broken, newer go":      "module example.com/a\ngo 1.99\nrequire (\n",
	} {
		if p, err := modulePath(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted as %q", name, p)
		}
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

// archive/zip reads central directory headers until one is not valid and
// compares the declared count only modulo 65536, so 65537 headers that fit the
// declared directory and are declared as one are refused before it allocates
// for them.
func TestCheckZipCountsDirectoryHeaders(t *testing.T) {
	const n = 65537
	z := make([]byte, 46*n+22)
	for i := range n {
		copy(z[46*i:], "PK\x01\x02")
	}
	e := z[46*n:]
	copy(e, "PK\x05\x06")
	binary.LittleEndian.PutUint16(e[8:], 1)
	binary.LittleEndian.PutUint16(e[10:], 1)
	binary.LittleEndian.PutUint32(e[12:], 46*n)
	r := bytes.NewReader(z)
	if zr, err := zip.NewReader(r, r.Size()); err != nil || len(zr.File) != n {
		t.Fatalf("archive/zip should read all %d headers: %v", n, err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := checkZip(r, r.Size(), "example.com/m", "v1.0.0")
	runtime.ReadMemStats(&after)
	if repo.StatusOf(err) != 400 || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("got %v, want a 400 for the directory", err)
	}
	if d := after.TotalAlloc - before.TotalAlloc; d > 1<<20 {
		t.Fatalf("allocated %d bytes: zip directory must not be parsed", d)
	}
}

// QA L-22 review: goModTokens kept every token of a line. A go.mod line of
// 1 MiB of commas is a million one-byte tokens, a 16 MiB []string per parse,
// in a zip of a few KiB, and nothing bounds how many uploads validate at once.
func TestCheckZipGoModLineMemory(t *testing.T) {
	line := strings.Repeat(",", maxDoc-1)
	gomod := strings.Repeat(line+"\n", 4) + "module example.com/m\n"

	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("example.com/m@v1.0.0/go.mod")
	w.Write([]byte(gomod))
	zw.Close()

	defer debug.SetMemoryLimit(debug.SetMemoryLimit(80 << 20)) // GOMEMLIMIT of docker-compose.yml and the Kubernetes manifest
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Microsecond):
			}
		}
	}()
	const uploads = 6
	var wg sync.WaitGroup
	for range uploads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// what upload() runs on the stored zip (validate -> checkZip -> modulePath)
			z := bytes.NewReader(zbuf.Bytes())
			if err := checkZip(z, z.Size(), "example.com/m", "v1.0.0"); repo.StatusOf(err) != 400 || !strings.Contains(err.Error(), "unknown directive") {
				t.Errorf("got %v, want a 400 for the comma line", err)
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-sampled
	grew := (int64(peak.Load()) - int64(base.HeapInuse)) >> 20
	t.Logf("heap in use grew by %d MiB for %d concurrent go.mod checks of a %d-byte zip", grew, uploads, zbuf.Len())
	if grew > 32 {
		t.Fatalf("%d concurrent go.mod checks took %d MiB of heap", uploads, grew)
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
}

// forgedZip builds a zip of one entry whose header declares size and crc
// for body, which is stored or, with deflate, compressed.
func forgedZip(t *testing.T, name string, body []byte, size uint64, crc uint32, deflate bool) *bytes.Reader {
	t.Helper()
	data, method := body, zip.Store
	if deflate {
		var c bytes.Buffer
		fw, _ := flate.NewWriter(&c, flate.BestSpeed)
		fw.Write(body)
		fw.Close()
		data, method = c.Bytes(), zip.Deflate
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{Name: name, Method: method, CRC32: crc, CompressedSize64: uint64(len(data)), UncompressedSize64: size})
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

// QA N-3: every entry is read back, so a forged CRC or size is a 400, and the
// size limit holds for the bytes really there, not the declared ones.
func TestCheckZipReadsEntries(t *testing.T) {
	const name = "example.com/Hello@v1.0.0/a.go"
	body := []byte(strings.Repeat("package a\n", 200))
	crc := crc32.ChecksumIEEE(body)
	n := uint64(len(body))
	for _, deflate := range []bool{false, true} {
		if z := forgedZip(t, name, body, n, crc, deflate); checkZip(z, z.Size(), "example.com/Hello", "v1.0.0") != nil {
			t.Fatalf("honest headers must pass (deflate %v)", deflate)
		}
	}
	zeros := make([]byte, 4<<20) // QA: a big entry that claims 1024 bytes
	cases := map[string]*bytes.Reader{
		"wrong crc":               forgedZip(t, name, body, n, 0xdeadbeef, false),
		"size understated":        forgedZip(t, name, body, n/2, crc, false),
		"size overstated":         forgedZip(t, name, body, n+1, crc, false),
		"deflated, understated":   forgedZip(t, name, zeros, 1024, crc32.ChecksumIEEE(zeros), true),
		"deflated, overstated":    forgedZip(t, name, body, n*2, crc, true),
		"deflated, corrupt crc":   forgedZip(t, name, body, n, crc^1, true),
		"directory holding bytes": rawZip(t, []string{"example.com/Hello@v1.0.0/sub/"}, []uint64{5}),
	}
	for name, z := range cases {
		if err := checkZip(z, z.Size(), "example.com/Hello", "v1.0.0"); repo.StatusOf(err) != 400 {
			t.Errorf("%s: %v", name, err)
		}
	}

	old := maxUnzipped
	maxUnzipped = 2000
	defer func() { maxUnzipped = old }()
	const p = "example.com/Hello@v1.0.0/"
	full := makeZip(t, map[string]string{p + "a.go": strings.Repeat("a", 1000), p + "b.go": strings.Repeat("b", 1000)})
	if err := checkZip(full, full.Size(), "example.com/Hello", "v1.0.0"); err != nil {
		t.Errorf("total at the limit must pass: %v", err)
	}
	over := makeZip(t, map[string]string{p + "a.go": strings.Repeat("a", 1001), p + "b.go": strings.Repeat("b", 1000)})
	if err := checkZip(over, over.Size(), "example.com/Hello", "v1.0.0"); repo.StatusOf(err) != 400 {
		t.Errorf("total over the limit: %v", err)
	}
}

// QA N-13: directory entries, as zip -r writes them, are accepted like the go
// command does; a directory must not share a name with a file.
func TestCheckZipDirectoryEntries(t *testing.T) {
	const p = "example.com/Hello@v1.0.0/"
	ok := map[string]*bytes.Reader{
		"zip -r layout": makeZip(t, map[string]string{
			p: "", p + "sub/": "", p + "sub/deeper/": "", p + "sub/x.go": "package sub\n",
			p + "go.mod": "module example.com/Hello\n",
		}),
		"repeated directory": rawZip(t, []string{p + "sub/", p + "sub/", p + "sub/x.go"}, nil),
		"go.mod directory":   rawZip(t, []string{p + "go.mod/", p + "go.mod/x.go"}, nil),
	}
	for name, z := range ok {
		if err := checkZip(z, z.Size(), "example.com/Hello", "v1.0.0"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]*bytes.Reader{
		"directory and file": rawZip(t, []string{p + "sub/", p + "sub"}, nil),
		"case-fold clash":    rawZip(t, []string{p + "Sub/", p + "sub/"}, nil),
		"double slash":       rawZip(t, []string{p + "sub//"}, nil),
		"empty name":         rawZip(t, []string{p + "/"}, nil),
		"dot directory":      rawZip(t, []string{p + "./"}, nil),
	}
	for name, z := range bad {
		if err := checkZip(z, z.Size(), "example.com/Hello", "v1.0.0"); repo.StatusOf(err) != 400 {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// QA N-14: the go command's per-file limits, 16 MiB for the root LICENSE and
// for go.mod, hold inside the zip.
func TestCheckZipFileLimits(t *testing.T) {
	const p = "example.com/Hello@v1.0.0/"
	big := makeZip(t, map[string]string{p + "LICENSE": strings.Repeat("x", maxLICENSE+1)})
	if err := checkZip(big, big.Size(), "example.com/Hello", "v1.0.0"); repo.StatusOf(err) != 400 || !strings.Contains(err.Error(), "LICENSE") {
		t.Errorf("LICENSE over 16 MiB: %v", err)
	}
	old := maxModUpload
	maxModUpload = 64
	defer func() { maxModUpload = old }()
	gomod := "module example.com/Hello\n" + strings.Repeat("// padding\n", 10)
	z := makeZip(t, map[string]string{p + "go.mod": gomod})
	if err := checkZip(z, z.Size(), "example.com/Hello", "v1.0.0"); repo.StatusOf(err) != 400 || !strings.Contains(err.Error(), "go.mod exceeds") {
		t.Errorf("go.mod over the limit: %v", err)
	}
	broken := makeZip(t, map[string]string{p + "go.mod": "module example.com/Hello\n)\n"})
	if err := checkZip(broken, broken.Size(), "example.com/Hello", "v1.0.0"); repo.StatusOf(err) != 400 {
		t.Errorf("malformed go.mod: %v", err)
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
