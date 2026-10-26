package cargo

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"commandcenter/internal/repo"
)

func publishBody(t *testing.T, meta string, crate []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(len(meta)))
	buf.WriteString(meta)
	binary.Write(&buf, binary.LittleEndian, uint32(len(crate)))
	buf.Write(crate)
	return buf.Bytes()
}

const helloMeta = `{"name":"smoke-hello","vers":"1.0.0","deps":[
 {"name":"itoa","version_req":"^1","features":[],"optional":false,"default_features":true,"target":null,"kind":"normal","registry":"https://github.com/rust-lang/crates.io-index","explicit_name_in_toml":null},
 {"name":"serde","version_req":"^1.0","features":["derive"],"optional":true,"default_features":false,"target":"cfg(unix)","kind":"normal","registry":null,"explicit_name_in_toml":"my_serde"}],
 "features":{"default":["std"],"std":[],"json":["dep:my_serde","my_serde?/derive"]},
 "authors":["x"],"description":"d","links":null,"rust_version":"1.70"}`

func TestReadPublishAndIndexLine(t *testing.T) {
	body := publishBody(t, helloMeta, []byte("crate-bytes"))
	meta, crate, n, err := readPublish(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(crate)
	if meta.Name != "smoke-hello" || meta.Vers != "1.0.0" || n != 11 || string(b) != "crate-bytes" {
		t.Fatalf("meta=%+v n=%d crate=%q", meta, n, b)
	}
	line := meta.indexLine("abc123")
	raw, err := line.marshal()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || bytes.Contains(raw, []byte("\n")) {
		t.Fatalf("line %s: %v", raw, err)
	}
	deps := m["deps"].([]any)
	d0, d1 := deps[0].(map[string]any), deps[1].(map[string]any)
	if d0["req"] != "^1" || d0["registry"] != "https://github.com/rust-lang/crates.io-index" || d0["kind"] != "normal" {
		t.Fatalf("dep 0 = %v", d0)
	}
	if d1["name"] != "my_serde" || d1["package"] != "serde" || d1["optional"] != true || d1["target"] != "cfg(unix)" {
		t.Fatalf("dep 1 = %v", d1)
	}
	if m["cksum"] != "abc123" || m["yanked"] != false || m["v"].(float64) != 2 || m["rust_version"] != "1.70" {
		t.Fatalf("line = %v", m)
	}
	feats, feats2 := m["features"].(map[string]any), m["features2"].(map[string]any)
	if feats["json"] != nil || feats2["json"] == nil || feats["std"] == nil {
		t.Fatalf("features=%v features2=%v", feats, feats2)
	}
	if v, err := lineVersion(raw); err != nil || v != "1.0.0" {
		t.Fatalf("lineVersion = %q %v", v, err)
	}
}

func TestReadPublishErrors(t *testing.T) {
	for name, body := range map[string][]byte{"truncated header": {1, 2}, "bad json": publishBody(t, "nope", []byte("x"))} {
		if _, _, _, err := readPublish(bytes.NewReader(body)); repo.StatusOf(err) != 400 {
			t.Errorf("%s: %v, want 400", name, err)
		}
	}
	full := publishBody(t, helloMeta, []byte("crate"))
	_, crate, n, err := readPublish(bytes.NewReader(full[:len(full)-2]))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(crate); int64(len(b)) == n {
		t.Fatal("a truncated crate must be shorter than its declared length (the caller checks it)")
	}
	var huge bytes.Buffer
	binary.Write(&huge, binary.LittleEndian, uint32(11<<20))
	if _, _, _, err := readPublish(&huge); repo.StatusOf(err) != 413 {
		t.Errorf("oversized metadata: %v", err)
	}
	if _, _, _, err := readPublish(bytes.NewReader(publishBody(t, `{"name":"9bad","vers":"1.0.0"}`, []byte("x")))); repo.StatusOf(err) != 400 {
		t.Errorf("invalid name: %v", err)
	}
	if _, _, _, err := readPublish(bytes.NewReader(publishBody(t, `{"name":"ok","vers":"1.0"}`, []byte("x")))); repo.StatusOf(err) != 400 {
		t.Errorf("invalid version: %v", err)
	}
}

// TestReadPublishValidatesIndexFields: dependency fields that cargo cannot
// read back from an index line are 400s (QA L-19).
func TestReadPublishValidatesIndexFields(t *testing.T) {
	read := func(name string, dep map[string]any) error {
		d := map[string]any{"name": "itoa", "version_req": "^1", "kind": "normal"}
		for k, v := range dep {
			d[k] = v
		}
		b, _ := json.Marshal(map[string]any{"name": name, "vers": "1.0.0", "deps": []any{d}})
		_, _, _, err := readPublish(bytes.NewReader(publishBody(t, string(b), []byte("x"))))
		return err
	}
	bad := map[string][]any{
		"name":                  {"", "9itoa", "it oa", "itoa!", "-itoa", strings.Repeat("a", 65)},
		"explicit_name_in_toml": {"my serde", "9x"},
		"version_req": {"", " ", "^", "1.0 2.0", ">=1.0 <2.0", "~>1", "v1", "1.0-alpha", "1.*.0", "*, >1", "01.0",
			"=1.0.0-01", "1.2.3.4", "^1,", "1.0,,2", ">=*", "\t1.0", "18446744073709551616"},
		"kind":     {"Normal", "runtime", "dev "},
		"target":   {"", "cfg(unix", "cfg()", "cfg(unix, windows)", "cfg(not(a, b))", "cfg(not())", "cfg(all)", "cfg(a = b)", "cfg(a = \"b)", "cfg(target-os)", "x86 64", "junk("},
		"registry": {"", "crates.io-index", "not a url"},
	}
	for field, values := range bad {
		for _, v := range values {
			if err := read("qa", map[string]any{field: v}); repo.StatusOf(err) != 400 {
				t.Errorf("%s %q: %v, want 400", field, v, err)
			}
		}
	}
	good := map[string][]any{
		"name":                  {"_private", "serde_json", "proc-macro2", "Inflector"},
		"explicit_name_in_toml": {"", "my_serde", "_x"},
		"version_req": {"^1", "^1.0", "~1.2.3", ">=1.0, <2.0", " >= 1.2 , < 2 ", "=1.2.3-rc.1", "*", "x", "1.*", "1.*.*",
			"1.2.*", "1.x", "<=0.4.0+build.01", "0.0.1", ">1.0.0-alpha.1"},
		"kind":     {"", "dev", "build"},
		"target":   {"cfg(unix)", `cfg(target_os = "linux")`, `cfg(all(unix, not(target_os = "macos")))`, "cfg(any())", `cfg(any(feature = "a b", windows,))`, "x86_64-unknown-linux-gnu", "thumbv7em-none-eabihf"},
		"registry": {"https://github.com/rust-lang/crates.io-index", "sparse+https://index.example.com/"},
	}
	for field, values := range good {
		for _, v := range values {
			if err := read("qa", map[string]any{field: v}); err != nil {
				t.Errorf("%s %q: %v", field, v, err)
			}
		}
	}
}

func TestDownloadURL(t *testing.T) {
	cases := []struct{ dl, want string }{
		{"https://static.crates.io/crates", "https://static.crates.io/crates/Serde/1.0.0/download"},
		{"https://x/{crate}/{version}.crate", "https://x/Serde/1.0.0.crate"},
		{"https://x/{prefix}/{lowerprefix}/{sha256-checksum}", "https://x/Se/rd/se/rd/abc"},
		{"https://x/{other}/b", "https://x/{other}/b/Serde/1.0.0/download"}, // unknown braces: plain base URL
	}
	for _, c := range cases {
		if got, err := downloadURL(c.dl, "Serde", "1.0.0", "abc"); err != nil || got != c.want {
			t.Errorf("%s: %q %v", c.dl, got, err)
		}
	}
	if _, err := downloadURL("ftp://x", "a", "1.0.0", ""); err == nil {
		t.Error("non-http dl must fail")
	}
}

func TestReadPublishBoundsMetadata(t *testing.T) {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint32(maxMeta+1))
	if _, _, _, err := readPublish(&b); repo.StatusOf(err) != 413 {
		t.Errorf("metadata one byte over the cap: %v", err)
	}
	// A declared length that the body cannot satisfy is a truncated body.
	b.Reset()
	binary.Write(&b, binary.LittleEndian, uint32(maxMeta))
	b.WriteString("{}")
	if _, _, _, err := readPublish(&b); repo.StatusOf(err) != 400 {
		t.Errorf("truncated metadata: %v", err)
	}
}

func TestReadPublishOverGlobalCapIs413(t *testing.T) {
	body := publishBody(t, helloMeta, []byte("crate"))
	w := httptest.NewRecorder()
	r := http.MaxBytesReader(w, io.NopCloser(bytes.NewReader(body)), 40)
	if _, _, _, err := readPublish(r); repo.StatusOf(err) != 413 {
		t.Fatalf("err = %v (status %d), want 413", err, repo.StatusOf(err))
	}
}
