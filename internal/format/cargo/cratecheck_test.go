package cargo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/repo"
)

type tarFile struct{ name, body string }

// tgz builds a gzip+tar archive of files.
func tgz(files ...tarFile) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(f.body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// testCrate is a minimal valid .crate; payload is the Cargo.toml body.
func testCrate(name, vers, payload string) []byte {
	return tgz(tarFile{name + "-" + vers + "/Cargo.toml", payload}, tarFile{name + "-" + vers + "/src/lib.rs", ""})
}

// publishCrate builds a publish body whose crate is a valid archive for the
// name and version in meta (raw payload when meta is not JSON or crate is empty).
func publishCrate(t *testing.T, meta string, crate []byte) []byte {
	t.Helper()
	var m struct{ Name, Vers string }
	if len(crate) > 0 && json.Unmarshal([]byte(meta), &m) == nil && m.Name != "" {
		crate = testCrate(m.Name, m.Vers, string(crate))
	}
	return publishBody(t, meta, crate)
}

func TestCheckCrate(t *testing.T) {
	ok := tarFile{"qa-1.0.0/Cargo.toml", "[package]"}
	cases := map[string][]byte{
		"not gzip":        []byte("hello"),
		"empty":           nil,
		"wrong top dir":   tgz(tarFile{"other-1.0.0/Cargo.toml", "x"}),
		"no Cargo.toml":   tgz(tarFile{"qa-1.0.0/src/lib.rs", "x"}),
		"nested toml":     tgz(tarFile{"qa-1.0.0/sub/Cargo.toml", "x"}),
		"dotdot":          tgz(ok, tarFile{"qa-1.0.0/../evil", "x"}),
		"absolute":        tgz(ok, tarFile{"/etc/passwd", "x"}),
		"stray top-level": tgz(ok, tarFile{"README", "x"}),
		"prefix only":     tgz(tarFile{"qa-1.0.0x/Cargo.toml", "x"}),
	}
	for name, b := range cases {
		err := checkCrate(bytes.NewReader(b), "qa", "1.0.0")
		if repo.StatusOf(err) != http.StatusBadRequest {
			t.Errorf("%s: %v, want 400", name, err)
		}
	}
	if err := checkCrate(bytes.NewReader(tgz(ok)), "qa", "1.0.0"); err != nil {
		t.Errorf("valid crate: %v", err)
	}
}

func TestCheckCrateLimits(t *testing.T) {
	oldE, oldS, oldT := maxCrateEntries, maxCrateEntry, maxCrateTotal
	defer func() { maxCrateEntries, maxCrateEntry, maxCrateTotal = oldE, oldS, oldT }()
	files := []tarFile{{"qa-1.0.0/Cargo.toml", "x"}, {"qa-1.0.0/a", "x"}, {"qa-1.0.0/b", "x"}}
	maxCrateEntries = 2
	if err := checkCrate(bytes.NewReader(tgz(files...)), "qa", "1.0.0"); repo.StatusOf(err) != http.StatusBadRequest {
		t.Errorf("too many entries: %v", err)
	}
	maxCrateEntries = oldE
	maxCrateEntry = 10
	big := tgz(tarFile{"qa-1.0.0/Cargo.toml", strings.Repeat("x", 11)})
	if err := checkCrate(bytes.NewReader(big), "qa", "1.0.0"); repo.StatusOf(err) != http.StatusBadRequest {
		t.Errorf("entry too big: %v", err)
	}
	maxCrateEntry = oldS
	maxCrateTotal = 10
	if err := checkCrate(bytes.NewReader(tgz(tarFile{"qa-1.0.0/Cargo.toml", "123456"}, tarFile{"qa-1.0.0/a", "123456"})), "qa", "1.0.0"); repo.StatusOf(err) != http.StatusBadRequest {
		t.Errorf("total too big: %v", err)
	}
}

func TestPublishRejectsBadCrate(t *testing.T) {
	e := newEnv(t)
	meta := `{"name":"qa-crate-mm","vers":"0.1.0","deps":[],"features":{}}`
	cases := map[string][]byte{
		"not gzip":      []byte("plain bytes, not a crate"),
		"wrong top dir": tgz(tarFile{"qa-other-0.1.0/Cargo.toml", "x"}),
		"no Cargo.toml": tgz(tarFile{"qa-crate-mm-0.1.0/src/lib.rs", "x"}),
	}
	for name, crate := range cases {
		w := e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(publishBody(t, meta, crate)))
		if w.Code != 400 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := e.do("GET", "cargo-hosted", "qa/-c/qa-crate-mm", nil); w.Code != 404 {
		t.Fatalf("rejected publishes must be unindexed: %d", w.Code)
	}
	if w := e.do("GET", "cargo-hosted", "crates/qa-crate-mm/0.1.0/download", nil); w.Code != 404 {
		t.Fatalf("rejected publishes must have no asset: %d", w.Code)
	}
	good := publishBody(t, meta, testCrate("qa-crate-mm", "0.1.0", "[package]"))
	if w := e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(good)); w.Code != 200 {
		t.Fatalf("valid crate after rejects: %d %s", w.Code, w.Body.String())
	}
}

// rawTar writes one tar header block by hand (for types tar.Writer refuses).
func rawTar(name string, typ byte, size int64) []byte {
	blk := make([]byte, 512)
	copy(blk, name)
	copy(blk[100:], "0000644\x00")
	copy(blk[108:], "0000000\x00")
	copy(blk[116:], "0000000\x00")
	copy(blk[124:], fmt.Sprintf("%011o\x00", size))
	copy(blk[136:], "00000000000\x00")
	blk[156] = typ
	copy(blk[257:], "ustar  \x00")
	copy(blk[148:], "        ")
	var sum int
	for _, b := range blk {
		sum += int(b)
	}
	copy(blk[148:], fmt.Sprintf("%06o\x00 ", sum))
	return append(blk, make([]byte, 1024)...)
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func TestCheckCrateRejectsSparseAndLinks(t *testing.T) {
	cases := map[string][]byte{
		"gnu sparse": gz(rawTar("qa-1.0.0/Cargo.toml", 'S', 0)),
		"symlink":    gz(rawTar("qa-1.0.0/link", '2', 0)),
		"hardlink":   gz(rawTar("qa-1.0.0/link", '1', 0)),
	}
	cases["pax sparse"] = gz(paxSparse("qa-1.0.0/Cargo.toml"))
	start := time.Now()
	for name, b := range cases {
		if err := checkCrate(bytes.NewReader(b), "qa", "1.0.0"); repo.StatusOf(err) != http.StatusBadRequest {
			t.Errorf("%s: %v, want 400", name, err)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
}

// paxSparse is a PAX 1.0 sparse file whose 50 MiB real size is all hole.
func paxSparse(name string) []byte {
	rec := func(k, v string) string {
		n := len(k) + len(v) + 3
		for len(fmt.Sprint(n))+len(k)+len(v)+3 != n {
			n = len(fmt.Sprint(n)) + len(k) + len(v) + 3
		}
		return fmt.Sprintf("%d %s=%s\n", n, k, v)
	}
	recs := rec("GNU.sparse.major", "1") + rec("GNU.sparse.minor", "0") + rec("GNU.sparse.realsize", "52428800") + rec("GNU.sparse.name", name)
	pad := func(b []byte) []byte { return append(b, make([]byte, (512-len(b)%512)%512)...) }
	x := rawTar("PaxHeaders/x", 'x', int64(len(recs)))[:512]
	out := append([]byte{}, x...)
	out = append(out, pad([]byte(recs))...)
	out = append(out, rawTar(name, '0', 512)[:512]...)
	out = append(out, pad([]byte("0\n"))...)
	return append(out, make([]byte, 1024)...)
}
