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

// manifest is a minimal Cargo.toml for name@vers.
func manifest(name, vers string) string {
	return fmt.Sprintf("[package]\nname = %q\nversion = %q\n", name, vers)
}

// testCrate is a minimal valid .crate; payload is a comment in its
// Cargo.toml, so different payloads give different crates.
func testCrate(name, vers, payload string) []byte {
	return tgz(tarFile{name + "-" + vers + "/Cargo.toml", manifest(name, vers) + "# " + payload + "\n"}, tarFile{name + "-" + vers + "/src/lib.rs", ""})
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
	ok := tarFile{"qa-1.0.0/Cargo.toml", manifest("qa", "1.0.0")}
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

// TestCheckCrateLimits: each limit rejects for its own reason (the fixtures
// carry a valid Cargo.toml so the manifest check does not fire first).
func TestCheckCrateLimits(t *testing.T) {
	oldE, oldS, oldT, oldM := maxCrateEntries, maxCrateEntry, maxCrateTotal, maxManifest
	defer func() { maxCrateEntries, maxCrateEntry, maxCrateTotal, maxManifest = oldE, oldS, oldT, oldM }()
	toml := manifest("qa", "1.0.0")
	check := func(what, want string, files ...tarFile) {
		t.Helper()
		err := checkCrate(bytes.NewReader(tgz(files...)), "qa", "1.0.0")
		if repo.StatusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want 400 %q", what, err, want)
		}
	}
	maxCrateEntries = 2
	check("too many entries", "more than 2 entries", tarFile{"qa-1.0.0/Cargo.toml", toml}, tarFile{"qa-1.0.0/a", "x"}, tarFile{"qa-1.0.0/b", "x"})
	maxCrateEntries = oldE
	maxCrateEntry = int64(len(toml))
	check("entry too big", `"qa-1.0.0/a" is larger than`, tarFile{"qa-1.0.0/Cargo.toml", toml}, tarFile{"qa-1.0.0/a", toml + "x"})
	maxCrateEntry = oldS
	maxCrateTotal = int64(len(toml)) + 3
	check("total too big", "uncompressed", tarFile{"qa-1.0.0/Cargo.toml", toml}, tarFile{"qa-1.0.0/a", "123456"})
	maxCrateTotal = oldT
	maxManifest = int64(len(toml)) - 1
	check("manifest too big", "Cargo.toml is larger than", tarFile{"qa-1.0.0/Cargo.toml", toml})
}

// TestCheckCrateManifest: the Cargo.toml must declare the published name and
// version (QA N-4); every spelling of its path is checked.
func TestCheckCrateManifest(t *testing.T) {
	crate := func(toml string) []byte { return tgz(tarFile{"qa-1.0.0/Cargo.toml", toml}) }
	cases := map[string]struct {
		crate []byte
		want  string
	}{
		"other version":     {crate(manifest("qa", "9.9.9")), `declares "qa" version "9.9.9", but the publish metadata says "qa" version "1.0.0"`},
		"other name":        {crate(manifest("qb", "1.0.0")), `declares "qb"`},
		"other case":        {crate(manifest("QA", "1.0.0")), `declares "QA"`},
		"workspace version": {crate("[package]\nname = \"qa\"\nversion = { workspace = true }\n"), "version is not a plain string"},
		"workspace dotted":  {crate("[package]\nname = \"qa\"\nversion.workspace = true\n"), "version is not a plain string"},
		"no version":        {crate("[package]\nname = \"qa\"\n"), "[package] has no version"},
		"no package":        {crate("[lib]\nname = \"qa\"\n"), "no [package] table"},
		"not toml":          {crate("hello crate"), "not valid TOML"},
		"other spelling": {tgz(tarFile{"qa-1.0.0/Cargo.toml", manifest("qa", "1.0.0")},
			tarFile{"qa-1.0.0/CARGO.TOML", manifest("qa", "9.9.9")}), `CARGO.TOML declares "qa" version "9.9.9"`},
	}
	for name, c := range cases {
		err := checkCrate(bytes.NewReader(c.crate), "qa", "1.0.0")
		if repo.StatusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want 400 %q", name, err, c.want)
		}
	}
	if err := checkCrate(bytes.NewReader(crate(manifest("qa", "1.0.0")+"[lib]\nname = \"other\"\n")), "qa", "1.0.0"); err != nil {
		t.Errorf("matching manifest: %v", err)
	}
}

// normalizedManifest is shaped like the Cargo.toml cargo publish writes,
// with the TOML forms a scanner could trip over.
const normalizedManifest = `# THIS FILE IS AUTOMATICALLY GENERATED BY CARGO
#
# When uploading crates to the registry Cargo will automatically
# "normalize" Cargo.toml files for maximal compatibility [package]

cargo-features = ["edition2024"]

[package]
edition = "2021"
rust-version = "1.56"
name = "Foo_Bar"
version = "1.0.210-rc.1+build.5"
authors = [
    "Erick <e@example.com>",
    'David "D" T',
]
autobins = false
description = """
A crate. [dependencies]
name = "evil"
\"""still inside\""" and ''' too ""
"""
keywords = ["a", "b", ]
categories = [
    "encoding", # a comment with "quotes" and [brackets]
    "no-std::no-alloc",
]
license = 'MIT OR Apache-2.0'
readme = '''
README "md" with """ inside'''

[package.metadata.docs.rs]
features = ["derive", "rc"]
targets = [["x86_64-unknown-linux-gnu"], []]

[lib]
name = "foo_bar"

[[bin]]
name = "tool"
path = "src/main.rs"

[dependencies.serde]
version = "1"
features = { a = 1, b = [1, 2], c = { d = "e" } }

[target."cfg(any(unix, windows))".dependencies.libc]
version = "0.2"

[ target . 'cfg(target_os = "linux")' . dependencies ]
nix = { version = "0.27", default-features = false }

[features]
default = ["std"]
"with-dash" = []

[lints.rust]
unexpected_cfgs = { level = "warn", check-cfg = ['cfg(foo)'] }
date = 1979-05-27 07:32:00Z
num = +1_000 # trailing
`

func TestManifestPackage(t *testing.T) {
	for _, toml := range []string{normalizedManifest, strings.ReplaceAll(normalizedManifest, "\n", "\r\n")} {
		if n, v, err := manifestPackage([]byte(toml)); err != nil || n != "Foo_Bar" || v != "1.0.210-rc.1+build.5" {
			t.Fatalf("normalized manifest: %q %q %v", n, v, err)
		}
	}
	good := map[string]string{
		// Keys inside strings, comments and other tables are not [package] keys.
		"fake keys before": "[package]\ndescription = \"\"\"\nname = \"evil\"\nversion = \"9.9.9\"\n\"\"\"\nname = \"qa\"\nversion = \"1.0.0\"\n",
		"fake table after": "[package]\nname = \"qa\"\nversion = \"1.0.0\"\nx = '''\n[package]\nname = \"evil\"\n'''\n",
		"quotes in string": "[package]\nx = '\"\"\"'\ny = \"\\\"\\\"\\\"\"\nname = \"qa\"\nversion = \"1.0.0\"\n",
		"nested arrays":    "[dependencies]\nx = [\n  [\"package\"],\n]\n[package]\nname = \"qa\"\nversion = \"1.0.0\"\n",
		"quoted keys":      "[\"package\"]\n'name' = 'qa'\n\"version\" = \"1.0.0\" # c\n",
		"sub-table first":  "[package.metadata]\nname = \"evil\"\n[package]\nname = \"qa\"\nversion = \"1.0.0\"\n",
	}
	for what, toml := range good {
		if n, v, err := manifestPackage([]byte(toml)); err != nil || n != "qa" || v != "1.0.0" {
			t.Errorf("%s: %q %q %v", what, n, v, err)
		}
	}
	bad := map[string]string{
		"package twice":      "[package]\nname = \"qa\"\n[package]\nversion = \"1.0.0\"\n",
		"name twice":         "[package]\nname = \"qa\"\nname = \"qa\"\nversion = \"1.0.0\"\n",
		"dotted package":     "package.name = \"qa\"\npackage.version = \"1.0.0\"\n",
		"inline package":     "package = { name = \"qa\", version = \"1.0.0\" }\n",
		"multi-line name":    "[package]\nname = \"\"\"qa\"\"\"\nversion = \"1.0.0\"\n",
		"unterminated":       "[package]\nname = \"qa\"\nversion = \"1.0.0\"\nx = \"\"\"\n",
		"trailing junk":      "[package]\nname = \"qa\" junk\nversion = \"1.0.0\"\n",
		"escaped newline":    "[package]\nx = \"a\\\nname = \"qa\"\nversion = \"1.0.0\"\n",
		"too deep":           "[package]\nname = \"qa\"\nversion = \"1.0.0\"\nx = " + strings.Repeat("[", 100) + strings.Repeat("]", 100) + "\n",
		"unclosed header":    "[package\nname = \"qa\"\nversion = \"1.0.0\"\n",
		"six closing quotes": "[package]\nname = \"qa\"\nversion = \"1.0.0\"\nx = \"\"\"a\"\"\"\"\"\"\n",
		"missing value":      "[package]\nname = \"qa\"\nversion = \"1.0.0\"\nx =\n",
		"unclosed array":     "[package]\nname = \"qa\"\nversion = \"1.0.0\"\nx = [1, 2\n",
	}
	for what, toml := range bad {
		if n, v, err := manifestPackage([]byte(toml)); err == nil {
			t.Errorf("%s: accepted as %q %q", what, n, v)
		}
	}
}

func TestPublishRejectsBadCrate(t *testing.T) {
	e := newEnv(t)
	meta := `{"name":"qa-crate-mm","vers":"0.1.0","deps":[],"features":{}}`
	cases := map[string][]byte{
		"not gzip":      []byte("plain bytes, not a crate"),
		"wrong top dir": tgz(tarFile{"qa-other-0.1.0/Cargo.toml", "x"}),
		"no Cargo.toml": tgz(tarFile{"qa-crate-mm-0.1.0/src/lib.rs", "x"}),
		// QA N-4: the manifest inside must match the publish metadata.
		"manifest version": tgz(tarFile{"qa-crate-mm-0.1.0/Cargo.toml", manifest("qa-crate-mm", "9.9.9")}),
		"manifest name":    tgz(tarFile{"qa-crate-mm-0.1.0/Cargo.toml", manifest("qa-crate-other", "0.1.0")}),
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
