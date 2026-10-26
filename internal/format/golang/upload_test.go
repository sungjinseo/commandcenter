package golang

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestUploadAndInstall(t *testing.T) {
	e := newEnv(t)
	mod := "module example.com/Hello\n\ngo 1.21\n"
	if w := e.do("PUT", "go-all", "example.com/!hello/@v/v1.0.0.mod", strings.NewReader(mod)); w.Code != 201 {
		t.Fatalf("mod via group: %d %s", w.Code, w.Body.String())
	}
	zipBytes := moduleZip(t, "example.com/Hello", "v1.0.0", mod)
	if w := e.do("PUT", "go-hosted", "example.com/!hello/@v/v1.0.0.zip", bytes.NewReader(zipBytes)); w.Code != 201 {
		t.Fatalf("zip: %d %s", w.Code, w.Body.String())
	}
	w := e.do("GET", "go-all", "example.com/!hello/@v/v1.0.0.info", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"Version":"v1.0.0"`) || !strings.Contains(w.Body.String(), `"Time":"`) {
		t.Fatalf("generated info: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("GET", "go-all", "example.com/!hello/@v/list", nil); w.Body.String() != "v1.0.0\n" {
		t.Fatalf("list: %q", w.Body.String())
	}
	if w := e.do("GET", "go-all", "example.com/!hello/@v/v1.0.0.zip", nil); !bytes.Equal(w.Body.Bytes(), zipBytes) {
		t.Fatal("zip round trip")
	}
	if w := e.do("PUT", "go-hosted", "example.com/!hello/@v/v1.0.0.zip", bytes.NewReader(zipBytes)); w.Code != 409 {
		t.Fatalf("re-upload: %d", w.Code)
	}
}

func TestUploadRejects(t *testing.T) {
	e := newEnv(t)
	cases := map[string]struct {
		path, body string
		want       int
	}{
		"mod declares other module": {"example.com/a/@v/v1.0.0.mod", "module example.com/b\n", 400},
		"mod without directive":     {"example.com/a/@v/v1.0.0.mod", "go 1.21\n", 400},
		"mod, unbalanced block":     {"example.com/a/@v/v1.0.0.mod", "module example.com/a\nrequire (\n", 400},
		"mod, unknown directive":    {"example.com/a/@v/v1.0.0.mod", "module example.com/a\nrequre example.com/b v1.0.0\n", 400},
		"zip, broken go.mod":        {"example.com/a/@v/v1.0.0.zip", string(moduleZip(t, "example.com/a", "v1.0.0", "module example.com/a\n)\n")), 400},
		"info for other version":    {"example.com/a/@v/v1.0.0.info", `{"Version":"v2.0.0"}`, 400},
		"info not json":             {"example.com/a/@v/v1.0.0.info", `nope`, 400},
		"info bad time":             {"example.com/a/@v/v1.0.0.info", `{"Version":"v1.0.0","Time":"yesterday"}`, 400},
		"zip not a zip":             {"example.com/a/@v/v1.0.0.zip", "nope", 400},
		"zip wrong prefix":          {"example.com/a/@v/v1.0.0.zip", string(moduleZip(t, "example.com/b", "v1.0.0", "module example.com/b\n")), 400},
		"put list":                  {"example.com/a/@v/list", "v1.0.0\n", 405},
		"put latest":                {"example.com/a/@latest", "{}", 405},
	}
	for name, c := range cases {
		if w := e.do("PUT", "go-hosted", c.path, strings.NewReader(c.body)); w.Code != c.want {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := e.do("GET", "go-hosted", "example.com/a/@v/list", nil); w.Code != 404 {
		t.Fatalf("rejected uploads must leave nothing: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("PUT", "go-proxy", "example.com/a/@v/v1.0.0.mod", strings.NewReader("module example.com/a\n")); w.Code != 405 {
		t.Fatalf("upload to proxy: %d", w.Code)
	}
}

func TestUploadWritePolicies(t *testing.T) {
	e := newEnv(t)
	e.repos["go-hosted"].WritePolicy = "allow"
	mod := "module example.com/a\n"
	for i := 0; i < 2; i++ {
		if w := e.do("PUT", "go-hosted", "example.com/a/@v/v1.0.0.mod", strings.NewReader(mod)); w.Code != 201 {
			t.Fatalf("allow re-upload %d: %d", i, w.Code)
		}
	}
	e.repos["go-hosted"].WritePolicy = "deny"
	if w := e.do("PUT", "go-hosted", "example.com/a/@v/v1.1.0.mod", strings.NewReader(mod)); w.Code != 400 {
		t.Fatalf("deny: %d", w.Code)
	}
}

func TestUploadSizeCap(t *testing.T) {
	e := newEnv(t)
	old := maxModUpload
	maxModUpload = 10
	defer func() { maxModUpload = old }()
	body := "module example.com/a\n\ngo 1.21\n"
	if w := e.do("PUT", "go-hosted", "example.com/a/@v/v1.0.0.mod", strings.NewReader(body)); w.Code != 413 {
		t.Fatalf("oversized mod: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "go-hosted", "example.com/a/@v/list", nil); w.Code != 404 {
		t.Fatalf("oversized upload left data: %d", w.Code)
	}
}

func TestUploadMajorVersionSuffix(t *testing.T) {
	cases := []struct {
		module, version string
		want            int
	}{
		{"example.com/hello", "v2.0.0", 400},
		{"example.com/hello", "v2.0.0+incompatible", 201},
		{"example.com/hello", "v1.0.0+incompatible", 201},
		{"example.com/hello/v2", "v1.0.0", 400},
		{"example.com/hello/v2", "v2.1.0", 201},
		{"example.com/hello/v2", "v2.1.0+incompatible", 400},
		{"gopkg.in/yaml.v3", "v3.0.1", 201},
		{"gopkg.in/yaml.v3", "v2.0.0", 400},
		{"gopkg.in/yaml.v0", "v0.1.0", 201},
		// QA L-22: like x/mod SplitPathVersion, only /v2 and up are suffixes
		{"example.com/hello/v1", "v1.0.0", 400},
		{"example.com/hello/v0", "v0.1.0", 400},
		{"example.com/hello/v01", "v1.0.0", 400},
		{"example.com/hello/v1.2", "v1.2.0", 400},
		{"example.com/hello/v10", "v10.0.0", 201},
		{"example.com/v1x", "v1.0.0", 201},
	}
	for _, c := range cases {
		e := newEnv(t)
		body := "module " + c.module + "\n"
		w := e.do("PUT", "go-hosted", Escape(c.module)+"/@v/"+c.version+".mod", strings.NewReader(body))
		if w.Code != c.want {
			t.Errorf("%s %s: %d %s", c.module, c.version, w.Code, w.Body.String())
		}
	}
}

func TestIncompatibleZipRejectsRootGoMod(t *testing.T) {
	e := newEnv(t)
	with := moduleZip(t, "example.com/hello", "v2.0.0+incompatible", "module example.com/hello\n")
	if w := e.do("PUT", "go-hosted", "example.com/hello/@v/v2.0.0+incompatible.zip", bytes.NewReader(with)); w.Code != 400 {
		t.Fatalf("incompatible zip with go.mod: %d %s", w.Code, w.Body.String())
	}
	z := makeZip(t, map[string]string{"example.com/hello@v2.0.0+incompatible/hello.go": "package hello\n"})
	if w := e.do("PUT", "go-hosted", "example.com/hello/@v/v2.0.0+incompatible.zip", z); w.Code != 201 {
		t.Fatalf("incompatible zip without go.mod: %d %s", w.Code, w.Body.String())
	}
}

func TestZipOnlyUploadDerivesMod(t *testing.T) {
	e := newEnv(t)
	gomod := "module example.com/zipped\n\ngo 1.21\n\nrequire example.com/dep v1.2.3\n"
	z := moduleZip(t, "example.com/zipped", "v1.0.0", gomod)
	if w := e.do("PUT", "go-hosted", "example.com/zipped/@v/v1.0.0.zip", bytes.NewReader(z)); w.Code != 201 {
		t.Fatalf("zip: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "go-hosted", "example.com/zipped/@v/v1.0.0.mod", nil); w.Code != 200 || w.Body.String() != gomod {
		t.Fatalf("derived mod: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("GET", "go-hosted", "example.com/zipped/@v/list", nil); w.Body.String() != "v1.0.0\n" {
		t.Fatalf("list: %q", w.Body.String())
	}
	// no go.mod in the zip: a module stub
	z2 := makeZip(t, map[string]string{"example.com/bare@v1.0.0/bare.go": "package bare\n"})
	if w := e.do("PUT", "go-hosted", "example.com/bare/@v/v1.0.0.zip", z2); w.Code != 201 {
		t.Fatalf("bare zip: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "go-hosted", "example.com/bare/@v/v1.0.0.mod", nil); w.Body.String() != "module example.com/bare\n" {
		t.Fatalf("stub mod: %q", w.Body.String())
	}
	// an existing, different .mod is left alone and the zip refused (QA L-22;
	// this used to store the zip and keep the mismatched .mod)
	e.do("PUT", "go-hosted", "example.com/kept/@v/v1.0.0.mod", strings.NewReader("module example.com/kept\n\n// mine\n"))
	z3 := moduleZip(t, "example.com/kept", "v1.0.0", "module example.com/kept\n")
	if w := e.do("PUT", "go-hosted", "example.com/kept/@v/v1.0.0.zip", bytes.NewReader(z3)); w.Code != 409 {
		t.Fatalf("zip with a different go.mod: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "go-hosted", "example.com/kept/@v/v1.0.0.mod", nil); !strings.Contains(w.Body.String(), "// mine") {
		t.Fatalf("existing mod replaced: %q", w.Body.String())
	}
}

// QA L-22: a zip uploaded after its .mod must carry the same go.mod, or no
// go.mod when the .mod is the stub the go command derives for such a zip.
func TestZipMustMatchUploadedMod(t *testing.T) {
	e := newEnv(t)
	put := func(path string, body io.Reader) int {
		t.Helper()
		return e.do("PUT", "go-hosted", path, body).Code
	}
	zipOf := func(module, version, gomod string) io.Reader {
		return bytes.NewReader(moduleZip(t, module, version, gomod))
	}
	mod := "module example.com/m\n\ngo 1.21\n"
	if c := put("example.com/m/@v/v1.0.0.mod", strings.NewReader(mod)); c != 201 {
		t.Fatalf("mod: %d", c)
	}
	if c := put("example.com/m/@v/v1.0.0.zip", zipOf("example.com/m", "v1.0.0", "module example.com/m\n\ngo 1.22\n")); c != 409 {
		t.Fatalf("zip with a different go.mod: %d", c)
	}
	if w := e.do("GET", "go-hosted", "example.com/m/@v/list", nil); w.Code != 404 {
		t.Fatalf("refused zip was stored: %d %q", w.Code, w.Body.String())
	}
	if c := put("example.com/m/@v/v1.0.0.zip", zipOf("example.com/m", "v1.0.0", mod)); c != 201 {
		t.Fatalf("zip with the same go.mod: %d", c)
	}
	put("example.com/s/@v/v1.0.0.mod", strings.NewReader("module example.com/s\n\ngo 1.21\n"))
	if c := put("example.com/s/@v/v1.0.0.zip", makeZip(t, map[string]string{"example.com/s@v1.0.0/s.go": "package s\n"})); c != 409 {
		t.Fatalf("zip without go.mod after a full .mod: %d", c)
	}
	put("example.com/s/@v/v1.1.0.mod", strings.NewReader("module example.com/s\n"))
	if c := put("example.com/s/@v/v1.1.0.zip", makeZip(t, map[string]string{"example.com/s@v1.1.0/s.go": "package s\n"})); c != 201 {
		t.Fatalf("zip without go.mod after the stub .mod: %d", c)
	}

	// the zip's go.mod is held to the .mod limit even when a .mod exists
	old := maxModUpload
	maxModUpload = 64
	put("example.com/big/@v/v1.0.0.mod", strings.NewReader("module example.com/big\n"))
	c := put("example.com/big/@v/v1.0.0.zip", zipOf("example.com/big", "v1.0.0", "module example.com/big\n"+strings.Repeat("// pad\n", 10)))
	maxModUpload = old
	if c != 400 {
		t.Fatalf("zip go.mod over the limit after a .mod: %d", c)
	}

	// writePolicy allow: the zip's go.mod replaces the .mod
	e.repos["go-hosted"].WritePolicy = "allow"
	gomod := "module example.com/m\n\ngo 1.23\n"
	put("example.com/m/@v/v1.1.0.mod", strings.NewReader(mod))
	if c := put("example.com/m/@v/v1.1.0.zip", zipOf("example.com/m", "v1.1.0", gomod)); c != 201 {
		t.Fatalf("allow, different go.mod: %d", c)
	}
	if w := e.do("GET", "go-hosted", "example.com/m/@v/v1.1.0.mod", nil); w.Body.String() != gomod {
		t.Fatalf("allow: .mod is %q, want the zip's", w.Body.String())
	}
}

func TestHostedListExcludesPseudoVersions(t *testing.T) {
	e := newEnv(t)
	pseudo := "v0.0.0-20200101000000-abcdefabcdef"
	for _, v := range []string{"v1.0.0", pseudo, "v1.2.4-0.20200101000000-abcdefabcdef", "v1.3.0-pre.0.20200101000000-abcdefabcdef", "v1.4.0-rc.1"} {
		z := moduleZip(t, "example.com/p", v, "module example.com/p\n")
		if w := e.do("PUT", "go-hosted", "example.com/p/@v/"+v+".zip", bytes.NewReader(z)); w.Code != 201 {
			t.Fatalf("%s: %d %s", v, w.Code, w.Body.String())
		}
	}
	if w := e.do("GET", "go-hosted", "example.com/p/@v/list", nil); w.Body.String() != "v1.0.0\nv1.4.0-rc.1\n" {
		t.Fatalf("list: %q", w.Body.String())
	}
	if w := e.do("GET", "go-hosted", "example.com/p/@v/"+pseudo+".zip", nil); w.Code != 200 {
		t.Fatalf("pseudo zip fetch: %d", w.Code)
	}
	if w := e.do("GET", "go-hosted", "example.com/p/@v/"+pseudo+".info", nil); w.Code != 200 {
		t.Fatalf("pseudo info fetch: %d", w.Code)
	}
}

func TestIsPseudoVersion(t *testing.T) {
	yes := []string{"v0.0.0-20191109021931-daa7c04131f5", "v1.2.3-pre.0.20191109021931-daa7c04131f5", "v1.2.4-0.20191109021931-daa7c04131f5", "v2.0.0-20191109021931-daa7c04131f5+incompatible"}
	no := []string{"v1.0.0", "v1.0.0-rc.1", "v1.0.0-20191109021931", "v0.0.0-2019110902193-daa7c04131f5", "v1.2.3-pre.20191109021931-daa7c04131f5"}
	for _, v := range yes {
		if !isPseudoVersion(v) {
			t.Errorf("%s should be pseudo", v)
		}
	}
	for _, v := range no {
		if isPseudoVersion(v) {
			t.Errorf("%s should not be pseudo", v)
		}
	}
}

func TestIdenticalModReuploadIsNoOp(t *testing.T) {
	e := newEnv(t)
	gomod := "module example.com/idem\n\ngo 1.21\n"
	z := moduleZip(t, "example.com/idem", "v1.0.0", gomod)
	if w := e.do("PUT", "go-hosted", "example.com/idem/@v/v1.0.0.zip", bytes.NewReader(z)); w.Code != 201 {
		t.Fatalf("zip: %d %s", w.Code, w.Body.String())
	}
	// the derived .mod is byte-identical to the explicit one: no-op, same status
	if w := e.do("PUT", "go-hosted", "example.com/idem/@v/v1.0.0.mod", strings.NewReader(gomod)); w.Code != 201 {
		t.Fatalf("identical mod: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("PUT", "go-hosted", "example.com/idem/@v/v1.0.0.mod", strings.NewReader(gomod+"// x\n")); w.Code != 409 {
		t.Fatalf("different mod: %d", w.Code)
	}
	if w := e.do("GET", "go-hosted", "example.com/idem/@v/v1.0.0.mod", nil); w.Body.String() != gomod {
		t.Fatalf("mod changed: %q", w.Body.String())
	}
	// zip and info re-uploads stay rejected, identical or not
	if w := e.do("PUT", "go-hosted", "example.com/idem/@v/v1.0.0.zip", bytes.NewReader(z)); w.Code != 409 {
		t.Fatalf("identical zip re-upload: %d", w.Code)
	}
	z2 := moduleZip(t, "example.com/idem", "v1.0.0", gomod+"// y\n")
	if w := e.do("PUT", "go-hosted", "example.com/idem/@v/v1.0.0.zip", bytes.NewReader(z2)); w.Code != 409 {
		t.Fatalf("different zip: %d", w.Code)
	}
	// info re-upload
	info := e.do("GET", "go-hosted", "example.com/idem/@v/v1.0.0.info", nil).Body.String()
	if w := e.do("PUT", "go-hosted", "example.com/idem/@v/v1.0.0.info", strings.NewReader(info)); w.Code != 409 {
		t.Fatalf("identical info re-upload: %d", w.Code)
	}
	if w := e.do("PUT", "go-hosted", "example.com/idem/@v/v1.0.0.info", strings.NewReader(`{"Version":"v1.0.0","Time":"2001-01-01T00:00:00Z"}`)); w.Code != 409 {
		t.Fatalf("different info: %d", w.Code)
	}
}

func TestLatestWithPseudoVersions(t *testing.T) {
	e := newEnv(t)
	put := func(module, v string) {
		t.Helper()
		z := moduleZip(t, module, v, "module "+module+"\n")
		if w := e.do("PUT", "go-hosted", module+"/@v/"+v+".zip", bytes.NewReader(z)); w.Code != 201 {
			t.Fatalf("%s %s: %d %s", module, v, w.Code, w.Body.String())
		}
	}
	older, newer := "v0.0.0-20200101000000-abcdefabcdef", "v0.0.0-20210101000000-abcdefabcdef"
	put("example.com/only", older)
	put("example.com/only", newer)
	w := e.do("GET", "go-hosted", "example.com/only/@latest", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), newer) {
		t.Fatalf("pseudo-only @latest: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "go-all", "example.com/only/@latest", nil); w.Code != 200 || !strings.Contains(w.Body.String(), newer) {
		t.Fatalf("pseudo-only @latest via group: %d %s", w.Code, w.Body.String())
	}
	put("example.com/mixed", newer)
	put("example.com/mixed", "v1.0.0")
	if w := e.do("GET", "go-hosted", "example.com/mixed/@latest", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"v1.0.0"`) {
		t.Fatalf("mixed @latest: %d %s", w.Code, w.Body.String())
	}
	put("example.com/pre", newer)
	put("example.com/pre", "v1.0.0-rc.1")
	if w := e.do("GET", "go-hosted", "example.com/pre/@latest", nil); !strings.Contains(w.Body.String(), "v1.0.0-rc.1") {
		t.Fatalf("prerelease beats pseudo: %s", w.Body.String())
	}
}

func TestDeriveModCorruptEntryIs400(t *testing.T) {
	e := newEnv(t)
	gomod := "module example.com/bad\n\ngo 1.21\n" + strings.Repeat("// padding line to make the entry big enough\n", 50)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// stored (uncompressed) entry, so flipping a data byte breaks only the CRC
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "example.com/bad@v1.0.0/go.mod", Method: zip.Store})
	io.WriteString(w, gomod)
	zw.Close()
	z := buf.Bytes()
	i := bytes.LastIndex(z, []byte("// padding"))
	if i < 0 {
		t.Fatal("stored data not found")
	}
	z[i+3] ^= 0x01
	resp := e.do("PUT", "go-hosted", "example.com/bad/@v/v1.0.0.zip", bytes.NewReader(z))
	if resp.Code != 400 {
		t.Fatalf("corrupt zip entry: %d %s", resp.Code, resp.Body.String())
	}
}

func TestModOnlyVersionNotListed(t *testing.T) {
	e := newEnv(t)
	mod := "module example.com/modonly\n\ngo 1.21\n"
	if w := e.do("PUT", "go-hosted", "example.com/modonly/@v/v1.0.0.mod", strings.NewReader(mod)); w.Code != 201 {
		t.Fatalf("mod: %d %s", w.Code, w.Body.String())
	}
	for _, repoName := range []string{"go-hosted", "go-all"} {
		if w := e.do("GET", repoName, "example.com/modonly/@v/list", nil); strings.Contains(w.Body.String(), "v1.0.0") {
			t.Fatalf("%s list shows a .mod-only version: %d %q", repoName, w.Code, w.Body.String())
		}
		if w := e.do("GET", repoName, "example.com/modonly/@latest", nil); w.Code == 200 {
			t.Fatalf("%s @latest serves a .mod-only version: %s", repoName, w.Body.String())
		}
		// QA N-15: nor its .info
		if w := e.do("GET", repoName, "example.com/modonly/@v/v1.0.0.info", nil); w.Code != 404 {
			t.Fatalf("%s .info of a .mod-only version: %d %s", repoName, w.Code, w.Body.String())
		}
	}
	z := moduleZip(t, "example.com/modonly", "v1.0.0", mod)
	if w := e.do("PUT", "go-hosted", "example.com/modonly/@v/v1.0.0.zip", bytes.NewReader(z)); w.Code != 201 {
		t.Fatalf("zip: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "go-hosted", "example.com/modonly/@v/list", nil); w.Body.String() != "v1.0.0\n" {
		t.Fatalf("list after zip: %q", w.Body.String())
	}
	if w := e.do("GET", "go-all", "example.com/modonly/@latest", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"v1.0.0"`) {
		t.Fatalf("@latest after zip: %d %s", w.Code, w.Body.String())
	}
	for _, repoName := range []string{"go-hosted", "go-all"} {
		if w := e.do("GET", repoName, "example.com/modonly/@v/v1.0.0.info", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"v1.0.0"`) {
			t.Fatalf("%s .info after zip: %d %s", repoName, w.Code, w.Body.String())
		}
	}
}

func TestLegacyModOnlyStillDownloadable(t *testing.T) {
	e := newEnv(t)
	mod := "module example.com/legacy\n"
	e.seed(t, "go-hosted", assetKey("example.com/legacy", "v1.0.0.mod"), mod)
	if w := e.do("GET", "go-hosted", "example.com/legacy/@v/v1.0.0.mod", nil); w.Code != 200 || w.Body.String() != mod {
		t.Fatalf("legacy .mod: %d %q", w.Code, w.Body.String())
	}
}
