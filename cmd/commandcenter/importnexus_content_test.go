package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// No Nexus Repository runs here either: fakeNexus answers with the shapes of
// the reports' reading of its public source, and where that source is closed
// (nuget, gitlfs) with what its REST API documents; the importer must not
// depend on more than that.

func hostedBody(name, policy string, more map[string]any) map[string]any {
	b := map[string]any{"name": name, "online": true, "storage": map[string]any{"blobStoreName": "default", "strictContentTypeValidation": false, "writePolicy": policy}}
	for k, v := range more {
		b[k] = v
	}
	return b
}

// asc is what an armored signature starts with, for the target's check.
const asc = "-----BEGIN PGP SIGNATURE-----\n\nabc\n-----END PGP SIGNATURE-----\n"

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

func gzipOf(s string) string {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	io.WriteString(gz, s)
	gz.Close()
	return buf.String()
}

// gemOf is a .gem as gem build makes one.
func gemOf(name, version string) []byte {
	spec := "--- !ruby/object:Gem::Specification\nname: " + name + "\nversion: !ruby/object:Gem::Version\n  version: " + version +
		"\nplatform: ruby\nauthors:\n- Jane\nbindir: bin\ndate: 2026-10-27 00:00:00.000000000 Z\ndependencies: []\nfiles: []\nrequire_paths:\n- lib\n" +
		"required_ruby_version: !ruby/object:Gem::Requirement\n  requirements:\n  - - \">=\"\n    - !ruby/object:Gem::Version\n      version: '0'\n" +
		"required_rubygems_version: !ruby/object:Gem::Requirement\n  requirements:\n  - - \">=\"\n    - !ruby/object:Gem::Version\n      version: '0'\n" +
		"summary: demo\nspecification_version: 4\n"
	return plainTar(map[string]string{"metadata.gz": gzipOf(spec), "data.tar.gz": string(tarGz(nil, nil))})
}

// plainTar is a tar of files, uncompressed.
func plainTar(files map[string]string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range sortedKeys(files) {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg})
		io.WriteString(tw, files[n])
	}
	tw.Close()
	return buf.Bytes()
}

func provOf(file string, chart []byte) []byte {
	s := sha256.Sum256(chart)
	return []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n\nname: demo\n\n...\nfiles:\n  " + file + ": sha256:" + hex.EncodeToString(s[:]) +
		"\n-----BEGIN PGP SIGNATURE-----\n\nabc\n-----END PGP SIGNATURE-----\n")
}

// npmTarball is a package's tarball.
func npmTarball(t *testing.T, name, version string) []byte {
	t.Helper()
	pj, _ := json.Marshal(map[string]any{"name": name, "version": version, "description": "from the tarball"})
	return tgz(t, map[string]string{"package/package.json": string(pj)})
}

// nexusForFormats is a stand-in with a hosted repository of every format the
// import copies, and what its target refuses.
func nexusForFormats(t *testing.T) (*fakeNexus, *httptest.Server) {
	t.Helper()
	nx := &fakeNexus{comps: map[string][]nexusComponent{}, files: map[string][]byte{}, docs: map[string]string{}, assets: map[string][]nexusAsset{}}
	srv := httptest.NewServer(nx)
	t.Cleanup(srv.Close)
	nx.url = srv.URL
	for _, r := range [][2]string{{"maven-hosted", "maven2"}, {"npm-hosted", "npm"}, {"pypi-hosted", "pypi"}, {"helm-hosted", "helm"}, {"nuget-hosted", "nuget"},
		{"gems-hosted", "rubygems"}, {"r-hosted", "r"}, {"go-hosted", "go"}, {"lfs-hosted", "gitlfs"}, {"yum-hosted", "yum"}, {"conan-hosted", "conan"}, {"cargo-hosted", "cargo"}} {
		nx.repos = append(nx.repos, nexusRepo{r[0], r[1], "hosted"})
	}
	// Maven: a jar and its POM, and what Command Center makes itself.
	const pom = `<project><modelVersion>4.0.0</modelVersion><groupId>org.example</groupId><artifactId>app</artifactId><version>1.0</version></project>`
	pomAsset := nx.asset("maven-hosted", "org/example/app/1.0/app-1.0.pom", []byte(pom))
	pomAsset.Path = "/" + pomAsset.Path // Nexus 3.91 lists its paths so
	nx.comps["maven-hosted"] = []nexusComponent{{Group: "org.example", Name: "app", Version: "1.0", Assets: []nexusAsset{
		nx.asset("maven-hosted", "org/example/app/1.0/app-1.0.jar", []byte("jar")), pomAsset,
		nx.asset("maven-hosted", "org/example/app/1.0/app-1.0.jar.sha1", []byte("deadbeef"))}}}

	// npm: a scoped package with what was published of it; a package whose
	// document lists nothing; what the target refuses.
	util := func(v string) nexusAsset {
		return nx.asset("npm-hosted", "@acme/util/-/util-"+v+".tgz", npmTarball(t, "@acme/util", v))
	}
	u1, u2 := util("1.0.0"), util("1.1.0")
	nx.comps["npm-hosted"] = []nexusComponent{{Group: "acme", Name: "util", Version: "1.0.0", Assets: []nexusAsset{u1}}, {Group: "acme", Name: "util", Version: "1.1.0", Assets: []nexusAsset{u2}},
		{Name: "plain", Version: "2.0.0", Assets: []nexusAsset{nx.asset("npm-hosted", "plain/-/plain-2.0.0.tgz", npmTarball(t, "plain", "2.0.0"))}},
		{Name: "odd", Version: "1.0", Assets: []nexusAsset{nx.asset("npm-hosted", "odd/-/odd-1.0.tgz", npmTarball(t, "odd", "1.0"))}},
		{Name: "MixedCase", Version: "1.0.0", Assets: []nexusAsset{nx.asset("npm-hosted", "MixedCase/-/MixedCase-1.0.0.tgz", npmTarball(t, "MixedCase", "1.0.0"))}}}
	man := func(v, more string) string {
		return `{"name":"@acme/util","version":"` + v + `","description":"as published","gitHead":"abc123","_npmUser":{"name":"alice","email":"alice@example.org"}` + more + `}`
	}
	nx.docs["/repository/npm-hosted/@acme/util"] = `{"name":"@acme/util","dist-tags":{"latest":"1.1.0","beta":"1.0.0"},"versions":{"1.0.0":` + man("1.0.0", `,"deprecated":"use 1.1.0"`) + `,"1.1.0":` + man("1.1.0", "") + `},
		"time":{"created":"2020-01-02T03:04:05.000Z","modified":"2021-02-03T04:05:06.000Z","1.0.0":"2020-01-02T03:04:05.000Z","1.1.0":"2021-02-03T04:05:06.000Z"}}`
	nx.docs["/repository/npm-hosted/plain"] = `{"name":"plain","dist-tags":{"latest":"2.0.0","1.x":"2.0.0"}}`
	nx.docs["/repository/npm-hosted/MixedCase"] = `{"name":"MixedCase","dist-tags":{"latest":"1.0.0"}}`

	// PyPI: Nexus's version directory, a signature, a bad signature, an egg.
	sdist := tgz(t, map[string]string{"demo-1.0/PKG-INFO": "Metadata-Version: 2.1\nName: demo\nVersion: 1.0\n"})
	wheel := zipOf(t, map[string]string{"demo-1.0.dist-info/METADATA": "Metadata-Version: 2.1\nName: demo\nVersion: 1.0\n", "demo-1.0.dist-info/WHEEL": "Wheel-Version: 1.0\n"})
	nx.comps["pypi-hosted"] = []nexusComponent{{Name: "demo", Version: "1.0", Assets: []nexusAsset{
		nx.asset("pypi-hosted", "packages/demo/1.0/demo-1.0.tar.gz", sdist), nx.asset("pypi-hosted", "packages/demo/1.0/demo-1.0.tar.gz.asc", []byte(asc)),
		nx.asset("pypi-hosted", "packages/demo/1.0/demo-1.0-py3-none-any.whl", wheel), nx.asset("pypi-hosted", "packages/demo/1.0/demo-1.0-py3-none-any.whl.asc", []byte("not armored")),
		nx.asset("pypi-hosted", "packages/demo/1.0/demo-1.0-py2.7.egg", []byte("egg"))}}}

	// Helm: a chart under a name that is not <name>-<version>.tgz, signed; a
	// chart whose provenance file is another build's.
	chart := func(name, v string) []byte {
		return tgz(t, map[string]string{name + "/Chart.yaml": "apiVersion: v2\nname: " + name + "\nversion: " + v + "\n"})
	}
	demo, other := chart("demo", "1.0.0"), chart("other", "2.0.0")
	nx.comps["helm-hosted"] = []nexusComponent{{Name: "demo", Version: "1.0.0", Assets: []nexusAsset{
		nx.asset("helm-hosted", "charts/demo.tgz.prov", provOf("demo-1.0.0.tgz", demo)), nx.asset("helm-hosted", "charts/demo.tgz", demo)}},
		{Name: "other", Version: "2.0.0", Assets: []nexusAsset{nx.asset("helm-hosted", "other-2.0.0.tgz", other), nx.asset("helm-hosted", "other-2.0.0.tgz.prov", provOf("other-2.0.0.tgz", demo))}},
		// Two charts one file name: demo-1.0.0 version 2.3.4 and demo version 1.0.0-2.3.4.
		{Name: "demo-1.0.0", Version: "2.3.4", Assets: []nexusAsset{nx.asset("helm-hosted", "a/demo-1.0.0-2.3.4.tgz", chart("demo-1.0.0", "2.3.4"))}},
		{Name: "demo", Version: "1.0.0-2.3.4", Assets: []nexusAsset{nx.asset("helm-hosted", "b/demo-1.0.0-2.3.4.tgz", chart("demo", "1.0.0-2.3.4"))}}}

	// NuGet: a package with no extension in its path; a symbol package.
	nuspec := `<?xml version="1.0"?><package><metadata><id>Demo.Lib</id><version>1.2.0</version><authors>Jane</authors><description>demo</description></metadata></package>`
	nx.comps["nuget-hosted"] = []nexusComponent{{Name: "Demo.Lib", Version: "1.2.0", Assets: []nexusAsset{nx.asset("nuget-hosted", "Demo.Lib/1.2.0", zipOf(t, map[string]string{"Demo.Lib.nuspec": nuspec, "lib/a.dll": "MZ"}))}},
		{Name: "Demo.Lib", Version: "1.3.0", Assets: []nexusAsset{nx.asset("nuget-hosted", "Demo.Lib/1.3.0/Demo.Lib.1.3.0.snupkg", zipOf(t, map[string]string{"Demo.Lib.nuspec": strings.ReplaceAll(nuspec, "1.2.0", "1.3.0")}))}}}

	nx.comps["gems-hosted"] = []nexusComponent{{Name: "hello", Version: "1.0.0", Assets: []nexusAsset{
		nx.asset("gems-hosted", "gems/hello-1.0.0.gem", gemOf("hello", "1.0.0")), nx.asset("gems-hosted", "quick/Marshal.4.8/hello-1.0.0.gemspec.rz", []byte("rz"))}}}

	// R: as CRAN has it; a file name R's index cannot name.
	desc := func(v string) []byte {
		return tarGz([]string{"hello/DESCRIPTION"}, map[string]string{"hello/DESCRIPTION": "Package: hello\nVersion: " + v + "\nTitle: Hello\nDescription: Hello.\nLicense: MIT\n"})
	}
	nx.comps["r-hosted"] = []nexusComponent{{Group: "src/contrib", Name: "hello", Version: "1.0", Assets: []nexusAsset{nx.asset("r-hosted", "src/contrib/hello_1.0.tar.gz", desc("1.0"))}},
		{Group: "src/contrib", Name: "hello", Version: "2.0", Assets: []nexusAsset{nx.asset("r-hosted", "src/contrib/hello_2.0_R_x86_64-pc-linux-gnu.tar.gz", desc("2.0"))}}}

	// Go: the zip and its .mod; Nexus makes the .info on read.
	goZip := zipOf(t, map[string]string{"example.com/mod@v1.0.0/go.mod": "module example.com/mod\n", "example.com/mod@v1.0.0/x.go": "package mod\n"})
	nx.comps["go-hosted"] = []nexusComponent{{Name: "example.com/mod", Version: "v1.0.0", Assets: []nexusAsset{
		nx.asset("go-hosted", "example.com/mod/@v/v1.0.0.zip", goZip), nx.asset("go-hosted", "example.com/mod/@v/v1.0.0.mod", []byte("module other\n"))}},
		{Name: "example.com/bad", Version: "v2.0.0", Assets: []nexusAsset{nx.asset("go-hosted", "example.com/bad/@v/v2.0.0.zip", zipOf(t, map[string]string{"x": "y"}))}},
		{Name: "example.com/nomod", Version: "v1.0.0", Assets: []nexusAsset{nx.asset("go-hosted", "example.com/nomod/@v/v1.0.0.mod", []byte("module example.com/nomod\n"))}}}
	nx.docs["/repository/go-hosted/example.com/mod/@v/v1.0.0.info"] = `{"Version":"v1.0.0","Time":"2018-03-04T05:06:07Z"}`

	// Git LFS: the listing of assets, the object named by its oid or not.
	object, named := []byte("an lfs object"), []byte("another lfs object")
	sum := sha256.Sum256(named)
	a := nx.asset("lfs-hosted", "objects/x/"+hex.EncodeToString(sum[:]), named)
	a.Checksum.SHA256 = ""
	nx.assets["lfs-hosted"] = []nexusAsset{nx.asset("lfs-hosted", "some/path", object), a}

	// Yum: not an RPM at all (real RPMs are the smoke test's), and its package
	// group files, which only the list of assets has; one under another name,
	// with none beside it, is not guessed at.
	nx.comps["yum-hosted"] = []nexusComponent{{Name: "bad", Version: "1.0-1.noarch", Assets: []nexusAsset{nx.asset("yum-hosted", "Packages/bad-1.0-1.noarch.rpm", []byte("no rpm")), nx.asset("yum-hosted", "repodata/repomd.xml", []byte("<repomd/>"))}}}
	nx.assets["yum-hosted"] = []nexusAsset{nx.asset("yum-hosted", "repodata/comps.xml", []byte("<comps><group><id>demo</id></group></comps>")),
		nx.asset("yum-hosted", "repodata/comps.xml.gz", []byte(gzipOf("<comps/>"))), nx.asset("yum-hosted", "repodata/abc-comps.xml", []byte("<comps/>")),
		nx.asset("yum-hosted", "repodata/repomd.xml", []byte("<repomd/>")), nx.asset("yum-hosted", "other/repodata/abc-comps.xml.gz", []byte(gzipOf("<comps/>")))}

	nx.comps["conan-hosted"] = []nexusComponent{{Group: "lab/stable", Name: "zlib", Version: "1.3", Assets: []nexusAsset{nx.asset("conan-hosted", "v2/x", []byte("x"))}}}
	nx.comps["cargo-hosted"] = []nexusComponent{{Name: "hello", Version: "1.0.0", Assets: []nexusAsset{nx.asset("cargo-hosted", "crates/hello/1.0.0.crate", []byte("x"))}}}
	return nx, srv
}

// formatsTarget is a Command Center with those repositories.
func formatsTarget(t *testing.T) *ccServer {
	t.Helper()
	cc := newCC(t)
	for _, r := range []struct {
		format, name string
		more         map[string]any
	}{
		{"maven", "maven-hosted", map[string]any{"maven": map[string]any{"versionPolicy": "RELEASE", "layoutPolicy": "STRICT"}}},
		{"npm", "npm-hosted", nil}, {"pypi", "pypi-hosted", nil}, {"helm", "helm-hosted", nil}, {"nuget", "nuget-hosted", nil},
		{"rubygems", "gems-hosted", nil}, {"r", "r-hosted", nil}, {"go", "go-hosted", nil}, {"gitlfs", "lfs-hosted", nil}, {"yum", "yum-hosted", nil},
		{"conan", "conan-hosted", nil}, {"cargo", "cargo-hosted", nil},
	} {
		cc.must(t, "POST", nx+"/repositories/"+r.format+"/hosted", hostedBody(r.name, "ALLOW_ONCE", r.more))
	}
	return cc
}

func allRepos(nx *fakeNexus) []string {
	var out []string
	for _, r := range nx.repos {
		out = append(out, "-repo", r.Name)
	}
	return out
}

func TestImportNexusFormats(t *testing.T) {
	nexus, srv := nexusForFormats(t)
	cc := formatsTarget(t)
	dir := t.TempDir()
	pw := filepath.Join(dir, "nx.pw")
	os.WriteFile(pw, []byte("nx-secret\n"), 0o600)
	imp := func(extra ...string) (string, error) {
		var out bytes.Buffer
		args := append([]string{"import-nexus", "-from", srv.URL, "-from-user", "nx", "-from-password-file", pw, "-to", cc.url, "-to-user", "admin", "-to-token-file", cc.pwFile, "-concurrency", "3"}, allRepos(nexus)...)
		err := run(append(args, extra...), nil, &out)
		t.Log(out.String())
		return out.String(), err
	}
	has := func(what, out string, wants ...string) {
		t.Helper()
		for _, want := range wants {
			if !strings.Contains(out, want) {
				t.Errorf("%s lacks %q", what, want)
			}
		}
	}

	// A dry run names what npm and PyPI would refuse, without a download.
	out, err := imp("-dry-run")
	if err == nil {
		t.Errorf("a dry run with refusals must say so")
	}
	has("dry run", out,
		"npm-hosted\tnpm\t@acme/util\t1.0.0\t1\t", "would copy",
		"npm-hosted\tnpm\todd\t1.0\t0\t0\terror: would be refused: invalid version \"1.0\"",
		"npm-hosted\tnpm\tMixedCase\t1.0.0\t0\t0\terror: would be refused: invalid package name \"MixedCase\"",
		"npm-hosted\tnpm\tplain\tdist-tags\t0\t0\terror: tag 1.x would be refused: invalid dist-tag name",
		"demo-1.0-py2.7.egg: would be refused: unsupported file type",
		"conan-hosted\tconan\tzlib\t1.3\t0\t0\terror: not copied: no public source says how a current Nexus lays out hosted Conan",
		"cargo-hosted\tcargo\thello\t1.0.0\t0\t0\terror: not copied:")
	if nexus.downloads != 0 {
		t.Errorf("a dry run downloaded %d files", nexus.downloads)
	}

	out, err = imp()
	if err == nil {
		t.Errorf("a run with refusals must end in an error")
	}
	has("run", out,
		"maven-hosted -> maven-hosted (maven): 1 copied, 0 skipped, 0 failed",
		"npm-hosted\tnpm\t@acme/util\t1.0.0\t1\t",
		"npm-hosted\tnpm\todd\t1.0\t0\t0\terror: PUT /repository/npm-hosted/odd: HTTP 400: invalid version \"1.0\"",
		"npm-hosted\tnpm\tMixedCase\t1.0.0\t0\t0\terror: PUT /repository/npm-hosted/MixedCase: HTTP 400: invalid package name",
		"npm-hosted\tnpm\tplain\tdist-tags\t0\t0\terror: tag 1.x: PUT",
		// the wheel goes without its bad signature, and the line says so; the egg is the server's to refuse
		"left out demo-1.0-py3-none-any.whl.asc: not an ASCII-armored signature", "demo-1.0-py2.7.egg: POST /repository/pypi-hosted/: HTTP 400: unsupported file type",
		"helm-hosted\thelm\tother\t2.0.0\t1\t", "signs another build",
		"helm-hosted\thelm\tdemo\t1.0.0\t2\t",
		"nuget-hosted\tnuget\tDemo.Lib\t1.2.0\t1\t", "nuget-hosted\tnuget\tDemo.Lib\t1.3.0\t0\t0\terror: symbol packages (.snupkg) are not supported",
		"gems-hosted -> gems-hosted (rubygems): 1 copied, 0 skipped, 0 failed",
		"r-hosted\tr\thello\t1.0\t1\t", "hello_2.0_R_x86_64-pc-linux-gnu.tar.gz: PUT /repository/r-hosted/src/contrib/hello_2.0_R_x86_64-pc-linux-gnu.tar.gz: HTTP 400",
		"go-hosted\tgo\texample.com/mod\tv1.0.0\t2\t", "go-hosted\tgo\texample.com/nomod\tv1.0.0\t0\t0\terror: none of its 1 files",
		"lfs-hosted -> lfs-hosted (gitlfs): 2 copied, 0 skipped, 0 failed",
		"bad-1.0-1.noarch.rpm: PUT /repository/yum-hosted/Packages/bad-1.0-1.noarch.rpm: HTTP 400",
		"yum-hosted\tyum\trepodata/comps.xml\t\t1\t", "yum-hosted\tyum\trepodata/comps.xml.gz\t\t1\t",
		"yum-hosted\tyum\tother/repodata/abc-comps.xml.gz\t\t0\t0\terror: not copied: a package group file under another name",
		"conan-hosted\tconan\tzlib\t1.3\t0\t0\terror: not copied:",
		"publish times: kept")
	if !strings.Contains(out, "example.com/bad\tv2.0.0\t") || !strings.Contains(out, "v2.0.0.zip: PUT /repository/go-hosted/example.com/bad/@v/v2.0.0.zip: HTTP 400") {
		t.Errorf("a refused zip is not reported")
	}

	get := func(p string) (int, string) {
		t.Helper()
		st, b := cc.do(t, "", "", "GET", p, nil)
		return st, string(b)
	}
	want := func(p string, parts ...string) {
		t.Helper()
		st, b := get(p)
		if st != 200 {
			t.Errorf("GET %s: %d %s", p, st, b)
		}
		has(p, b, parts...)
	}
	stated := func(repo, p, when string) {
		t.Helper()
		l, _ := cc.get(t, nx+"/assets?repository="+repo).(obj)
		items, _ := l["items"].([]any)
		for _, x := range items {
			a, _ := x.(obj)
			if str(a, "path") == "/"+p {
				if !strings.HasPrefix(str(a, "blobCreated"), when) || !strings.HasPrefix(str(a, "lastModified"), when) {
					t.Errorf("%s %s: created %s, modified %s, want %s", repo, p, str(a, "blobCreated"), str(a, "lastModified"), when)
				}
				return
			}
		}
		t.Errorf("%s has no %s", repo, p)
	}

	// What the server makes keeps the time it made it: a Last-Modified from
	// 2019 on a new index would answer a client's copy 304.
	for repo, p := range map[string]string{"helm-hosted": "index.yaml", "yum-hosted": "repodata/repomd.xml", "r-hosted": "src/contrib/PACKAGES.gz"} {
		l, _ := cc.get(t, nx+"/assets?repository="+repo).(obj)
		items, _ := l["items"].([]any)
		seen := false
		for _, x := range items {
			a, _ := x.(obj)
			if str(a, "path") == "/"+p {
				seen = true
				if strings.HasPrefix(str(a, "lastModified"), "2019") {
					t.Errorf("%s %s carries the stated time: %s", repo, p, str(a, "lastModified"))
				}
			}
		}
		if !seen {
			t.Errorf("%s has no %s", repo, p)
		}
	}

	// npm: what was published, as it was published, and when.
	var doc struct {
		DistTags map[string]string `json:"dist-tags"`
		Time     map[string]string `json:"time"`
		Versions map[string]struct {
			Description string `json:"description"`
			Deprecated  string `json:"deprecated"`
			GitHead     string `json:"gitHead"`
			NpmUser     struct {
				Name string `json:"name"`
			} `json:"_npmUser"`
			Dist struct {
				Integrity string `json:"integrity"`
			} `json:"dist"`
		} `json:"versions"`
	}
	_, b := get("/repository/npm-hosted/@acme%2futil")
	if err := json.Unmarshal([]byte(b), &doc); err != nil {
		t.Fatal(err)
	}
	v := doc.Versions["1.0.0"]
	if v.Description != "as published" || v.Deprecated != "use 1.1.0" || v.GitHead != "abc123" || v.NpmUser.Name != "alice" || v.Dist.Integrity == "" {
		t.Errorf("npm manifest: %+v", v)
	}
	if doc.Time["1.0.0"] != "2020-01-02T03:04:05.000Z" || doc.Time["1.1.0"] != "2021-02-03T04:05:06.000Z" || doc.Time["created"] != "2020-01-02T03:04:05.000Z" || doc.Time["modified"] != "2021-02-03T04:05:06.000Z" {
		t.Errorf("npm time: %v", doc.Time)
	}
	if doc.DistTags["latest"] != "1.1.0" || doc.DistTags["beta"] != "1.0.0" {
		t.Errorf("npm tags: %v", doc.DistTags)
	}
	// Without a listed version: its tarball's package.json and its file's time.
	want("/repository/npm-hosted/plain", `"description":"from the tarball"`, `"2.0.0":"2019-05-06T07:08:09.000Z"`)
	stated("npm-hosted", "@acme/util/-/util-1.0.0.tgz", "2020-01-02T03:04:05")

	// PyPI: the file, its signature and the time; no signature that was not one.
	want("/repository/pypi-hosted/pypi/demo/json", `"upload_time":"2019-05-06T07:08:09"`)
	if st, b := get("/repository/pypi-hosted/packages/demo/demo-1.0.tar.gz.asc"); st != 200 || b != asc {
		t.Errorf("pypi signature: %d %q", st, b)
	}
	if st, _ := get("/repository/pypi-hosted/packages/demo/demo-1.0-py3-none-any.whl"); st != 200 {
		t.Errorf("pypi wheel: %d", st)
	}
	if st, _ := get("/repository/pypi-hosted/packages/demo/demo-1.0-py3-none-any.whl.asc"); st != 404 {
		t.Errorf("pypi bad signature: %d", st)
	}

	// Helm: the chart under its own name with its provenance file, created as in Nexus.
	want("/repository/helm-hosted/index.yaml", "created: \"2019-05-06T07:08:09Z\"", "demo-1.0.0.tgz")
	if st, _ := get("/repository/helm-hosted/demo-1.0.0.tgz.prov"); st != 200 {
		t.Errorf("helm provenance file: %d", st)
	}
	if st, _ := get("/repository/helm-hosted/other-2.0.0.tgz"); st != 200 {
		t.Errorf("a refused provenance file took its chart: %d", st)
	}
	if st, _ := get("/repository/helm-hosted/other-2.0.0.tgz.prov"); st != 404 {
		t.Errorf("a mismatched provenance file was stored: %d", st)
	}

	want("/repository/nuget-hosted/registration/demo.lib/1.2.0.json", `"published":"2019-05-06T07:08:09Z"`)
	if st, _ := get("/repository/nuget-hosted/v3-flatcontainer/demo.lib/1.3.0/demo.lib.1.3.0.nupkg"); st != 404 {
		t.Errorf("a symbol package was taken for a package: %d", st)
	}
	stated("gems-hosted", "gems/hello-1.0.0.gem", "2019-05-06T07:08:09")
	if st, _ := get("/repository/gems-hosted/quick/Marshal.4.8/hello-1.0.0.gemspec.rz"); st != 200 {
		t.Errorf("gemspec: %d", st)
	}
	want("/repository/r-hosted/src/contrib/PACKAGES", "Package: hello\nVersion: 1.0")
	stated("r-hosted", "src/contrib/hello_1.0.tar.gz", "2019-05-06T07:08:09")
	stated("maven-hosted", "org/example/app/1.0/app-1.0.jar", "2019-05-06T07:08:09")
	if st, b := get("/repository/maven-hosted/org/example/app/1.0/app-1.0.jar.sha1"); st != 200 || b == "deadbeef" {
		t.Errorf("maven checksum: %d %q", st, b)
	}

	// Yum: the package groups are in repomd.xml, the file comes as it was.
	want("/repository/yum-hosted/repodata/repomd.xml", `<data type="group">`, `<data type="group_gz">`)
	want("/repository/yum-hosted/repodata/comps.xml", "<id>demo</id>")
	if strings.Contains(out, "repodata/repomd.xml\t") || strings.Contains(out, "repodata/abc-comps.xml\t") {
		t.Errorf("a generated file was copied:\n%s", out)
	}

	// Go: the .info of Nexus, the .mod of the zip, not the one listed.
	want("/repository/go-hosted/example.com/mod/@v/v1.0.0.info", `"Time":"2018-03-04T05:06:07Z"`)
	if st, b := get("/repository/go-hosted/example.com/mod/@v/v1.0.0.mod"); st != 200 || b != "module example.com/mod\n" {
		t.Errorf("go mod: %d %q", st, b)
	}
	want("/repository/go-hosted/example.com/mod/@v/list", "v1.0.0")

	// Git LFS: both objects, one found by its name.
	s1 := sha256.Sum256([]byte("an lfs object"))
	s2 := sha256.Sum256([]byte("another lfs object"))
	for _, s := range [][32]byte{s1, s2} {
		if st, _ := get("/repository/lfs-hosted/info/lfs/objects/" + hex.EncodeToString(s[:])); st != 200 {
			t.Errorf("lfs object: %d", st)
		}
	}

	// A second run copies nothing, and what is refused stays reported.
	downloads := nexus.downloads
	out, err = imp()
	if err == nil {
		t.Errorf("second run: no error")
	}
	has("second run", out,
		"maven-hosted -> maven-hosted (maven): 0 copied, 1 skipped, 0 failed",
		"nuget-hosted\tnuget\tDemo.Lib\t1.2.0\t0\t0\tskipped",
		"gems-hosted -> gems-hosted (rubygems): 0 copied, 1 skipped, 0 failed",
		"go-hosted\tgo\texample.com/mod\tv1.0.0\t0\t0\tskipped",
		"lfs-hosted -> lfs-hosted (gitlfs): 0 copied, 2 skipped, 0 failed",
		"yum-hosted\tyum\trepodata/comps.xml\t\t0\t0\tskipped",
		"the target has the file without its signature")
	if strings.Contains(out, "\tok\n") {
		t.Errorf("second run copied something:\n%s", out)
	}
	if got := nexus.downloads - downloads; got > 12 { // refused files only: they are never on the target
		t.Errorf("second run downloaded %d files", got)
	}

	// Without the probes each format answers for itself: 409, 400 with a
	// message, npm's 403. All of that is "skipped", and a real refusal stays one.
	out, _ = imp("-skip-existing=false")
	has("run without probes", out,
		"maven-hosted -> maven-hosted (maven): 0 copied, 1 skipped, 0 failed",
		"nuget-hosted\tnuget\tDemo.Lib\t1.2.0\t0\t0\tskipped",
		"gems-hosted -> gems-hosted (rubygems): 0 copied, 1 skipped, 0 failed",
		"r-hosted\tr\thello\t1.0\t0\t0\tskipped",
		"go-hosted\tgo\texample.com/mod\tv1.0.0\t0\t0\tskipped",
		"npm-hosted\tnpm\t@acme/util\t1.0.0\t0\t0\tskipped",
		"yum-hosted\tyum\trepodata/comps.xml\t\t0\t0\tskipped",
		"demo-1.0-py2.7.egg: POST /repository/pypi-hosted/: HTTP 400: unsupported file type",
		"of another chart") // a 409 that is no duplicate is a refusal
}

// The groups Nexus merges and an import does not: one line each, until the
// operator has decided, and the setting is never turned on here.
func TestImportNexusGroupsThatMergeInNexus(t *testing.T) {
	hosted := func(name, format string) string {
		return `{"name":"` + name + `","format":"` + format + `","type":"hosted","url":"x","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":false,"writePolicy":"ALLOW_ONCE"},"cleanup":null,"component":{"proprietaryComponents":false}}`
	}
	group := func(name, format, member string) string {
		return `{"name":"` + name + `","format":"` + format + `","type":"group","url":"x","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":false},"group":{"memberNames":["` + member + `"],"writableMember":null}}`
	}
	nexus := httptest.NewServer(&nexusConfig{get: map[string]string{
		nx + "/blobstores": `[{"softQuota":null,"name":"default","type":"File"}]`,
		nx + "/repositorySettings": `[` + hosted("npm-h", "npm") + `,` + hosted("pypi-h", "pypi") + `,` + hosted("r-h", "r") + `,` + hosted("conda-h", "conda") + `,` + hosted("yum-h", "yum") + `,` + hosted("raw-h", "raw") + `,` +
			group("npm-all", "npm", "npm-h") + `,` + group("pypi-all", "pypi", "pypi-h") + `,` + group("r-all", "r", "r-h") + `,` + group("conda-all", "conda", "conda-h") + `,` + group("yum-all", "yum", "yum-h") + `,` + group("raw-all", "raw", "raw-h") + `]`,
	}})
	defer nexus.Close()
	cc := newCC(t)
	out, err := importConfig(t, nexus.URL, cc, "-config-only")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, name := range []string{"npm-all", "pypi-all", "r-all", "conda-all", "yum-all"} {
		if !strings.Contains(out, "needs manual action: repository "+name+": Nexus merges this group's members") || !strings.Contains(out, "mergeMembers") {
			t.Errorf("no line for %s:\n%s", name, out)
		}
		var cfg struct {
			MergeMembers bool `json:"mergeMembers"`
		}
		if json.Unmarshal(cc.must(t, "GET", native+"/repositories/"+name+"/config", nil), &cfg) != nil || cfg.MergeMembers {
			t.Errorf("%s: the import turned mergeMembers on", name)
		}
	}
	if strings.Contains(out, "repository raw-all: Nexus merges") {
		t.Errorf("a line for a format that has no such setting:\n%s", out)
	}
	// The decision made, the line goes; the others stay.
	cfg := map[string]any{}
	json.Unmarshal(cc.must(t, "GET", native+"/repositories/npm-all/config", nil), &cfg)
	cc.must(t, "PUT", native+"/repositories/npm-all", map[string]any{"name": "npm-all", "format": "npm", "type": "group", "members": cfg["members"], "mergeMembers": true, "online": true})
	out, _ = importConfig(t, nexus.URL, cc, "-config-only")
	if strings.Contains(out, "repository npm-all: Nexus merges") || !strings.Contains(out, "repository pypi-all: Nexus merges") {
		t.Errorf("after the decision:\n%s", out)
	}
}

// Without the right to state a time, the import says so and goes on: the
// versions carry the time of the copy and the name of who copied them.
func TestImportNexusWithoutPublishTimes(t *testing.T) {
	nexus, srv := nexusForFormats(t)
	nexus.repos = []nexusRepo{{"npm-hosted", "npm", "hosted"}}
	cc := formatsTarget(t)
	cc.must(t, "POST", nx+"/security/roles", obj{"id": "deployers", "name": "deployers", "privileges": []string{"nx-repository-view-*-*-*"}, "roles": []string{}})
	cc.must(t, "POST", nx+"/security/users", obj{"userId": "ci", "firstName": "C", "lastName": "I", "emailAddress": "ci@example.org", "password": "ci-password-1", "status": "active", "roles": []string{"deployers"}})
	dir := t.TempDir()
	pw, ci := filepath.Join(dir, "nx.pw"), filepath.Join(dir, "ci.pw")
	os.WriteFile(pw, []byte("nx-secret\n"), 0o600)
	os.WriteFile(ci, []byte("ci-password-1\n"), 0o600)
	var out bytes.Buffer
	before := time.Now().Add(-time.Minute)
	run([]string{"import-nexus", "-from", srv.URL, "-from-user", "nx", "-from-password-file", pw, "-to", cc.url, "-to-user", "ci", "-to-token-file", ci, "-repo", "npm-hosted"}, nil, &out)
	if !strings.Contains(out.String(), "publish times: NOT kept") || !strings.Contains(out.String(), "npm-hosted\tnpm\t@acme/util\t1.0.0\t1\t") {
		t.Errorf("output: %s", out.String())
	}
	var doc struct {
		Time     map[string]string `json:"time"`
		Versions map[string]struct {
			NpmUser struct {
				Name string `json:"name"`
			} `json:"_npmUser"`
		} `json:"versions"`
	}
	_, b := cc.do(t, "", "", "GET", "/repository/npm-hosted/@acme%2futil", nil)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	when, err := time.Parse(time.RFC3339, doc.Time["1.0.0"])
	if err != nil || when.Before(before) {
		t.Errorf("time %v: %v", doc.Time, err)
	}
	if got := doc.Versions["1.0.0"].NpmUser.Name; got != "ci" {
		t.Errorf("publisher %q", got)
	}
}

// The publish time is an administrator's to state, and nobody else's: other
// callers are refused and nothing is stored, a bad value is refused, and a
// format that records none says so rather than drop it.
func TestPublishTimeHeader(t *testing.T) {
	cc := newCC(t)
	for _, name := range []string{"raw-hosted", "raw-other"} {
		cc.must(t, "POST", nx+"/repositories/raw/hosted", hostedBody(name, "ALLOW_ONCE", nil))
	}
	cc.must(t, "POST", nx+"/repositories/npm/hosted", hostedBody("npm-hosted", "ALLOW_ONCE", nil))
	cc.must(t, "POST", nx+"/repositories/cargo/hosted", hostedBody("cargo-hosted", "ALLOW_ONCE", nil))
	cc.must(t, "POST", nx+"/repositories/docker/hosted", hostedBody("docker-hosted", "ALLOW_ONCE", nil))
	cc.must(t, "POST", nx+"/security/roles", obj{"id": "deployers", "name": "deployers", "privileges": []string{"nx-repository-view-*-*-*", "nx-repository-admin-*-*-*"}, "roles": []string{}})
	cc.must(t, "POST", nx+"/security/users", obj{"userId": "ci", "firstName": "C", "lastName": "I", "emailAddress": "ci@example.org", "password": "ci-password-1", "status": "active", "roles": []string{"deployers"}})
	ctx := context.Background()
	_, ciToken, err := cc.app.Auth.CreateToken(ctx, "ci")
	if err != nil {
		t.Fatal(err)
	}
	_, adminToken, err := cc.app.Auth.CreateToken(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	const past = "2020-01-02T03:04:05Z"
	// send uploads p with auth (user and password; none anonymous) stating when.
	send := func(user, password, method, p, when, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, cc.url+p, strings.NewReader(body))
		if user != "" {
			req.SetBasicAuth(user, password)
		}
		req.Header.Set("X-Command-Center-Publish-Time", when)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	exists := func(p string) bool {
		st, _ := cc.do(t, "", "", "GET", p, nil)
		return st == 200
	}
	for _, c := range []struct {
		who, user, password string
		status              int
	}{
		{"anonymous", "", "", 401},
		{"a deployer", "ci", "ci-password-1", 403},
		{"a deployer's token", "ci", ciToken, 403},
		{"an administrator's token", "admin", adminToken, 403},
	} {
		p := "/repository/raw-hosted/" + strings.ReplaceAll(c.who, " ", "-")
		if st, b := send(c.user, c.password, "PUT", p, past, "x"); st != c.status {
			t.Errorf("%s: %d %s, want %d", c.who, st, b, c.status)
		}
		if exists(p) {
			t.Errorf("%s: the file was stored", c.who)
		}
	}
	// The same callers without the header: the refusal was the header's.
	if st, b := send("ci", "ci-password-1", "PUT", "/repository/raw-hosted/without", "", "x"); st/100 != 2 {
		t.Errorf("a deployer without the header: %d %s", st, b)
	}
	// A read carries it to no effect, by anyone.
	if st, b := send("ci", "ci-password-1", "GET", "/repository/raw-hosted/without", "not a time", ""); st != 200 || b != "x" {
		t.Errorf("a read with the header: %d %s", st, b)
	}
	for what, when := range map[string]string{
		"not a time": "yesterday",
		"a date":     "2020-01-02",
		"the future": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"the epoch":  "1970-01-01T00:00:00Z",
		"long ago":   "0001-01-01T00:00:00Z",
	} {
		p := "/repository/raw-hosted/bad-" + strings.ReplaceAll(what, " ", "-")
		if st, b := send("admin", adminPassword, "PUT", p, when, "x"); st != 400 {
			t.Errorf("%s: %d %s, want 400", what, st, b)
		}
		if exists(p) {
			t.Errorf("%s: the file was stored", what)
		}
	}
	if st, b := send("admin", adminPassword, "POST", "/repository/cargo-hosted/api/v1/crates/new", past, "x"); st != 400 || !strings.Contains(b, "do not record") {
		t.Errorf("a format that records none: %d %s", st, b)
	}
	// The administrator: the file carries the time, its event says so, and
	// the write policy still holds.
	if st, b := send("admin", adminPassword, "PUT", "/repository/raw-hosted/dated", past, "x"); st/100 != 2 {
		t.Fatalf("administrator: %d %s", st, b)
	}
	if st, b := send("admin", adminPassword, "PUT", "/repository/raw-hosted/dated", past, "y"); st != 400 || !strings.Contains(b, "does not allow updating") {
		t.Errorf("a stated time passed the write policy: %d %s", st, b)
	}
	var assets struct {
		Items []obj `json:"items"`
	}
	json.Unmarshal(cc.must(t, "GET", nx+"/assets?repository=raw-hosted", nil), &assets)
	found := false
	for _, a := range assets.Items {
		switch str(a, "path") {
		case "/dated":
			found = true
			if !strings.HasPrefix(str(a, "blobCreated"), "2020-01-02T03:04:05") || !strings.HasPrefix(str(a, "lastModified"), "2020-01-02T03:04:05") {
				t.Errorf("stated time: %v", a)
			}
		case "/without":
			if strings.HasPrefix(str(a, "blobCreated"), "2020") {
				t.Errorf("no header but a time: %v", a)
			}
		}
	}
	if !found {
		t.Errorf("no asset /dated in %v", assets.Items)
	}
	// The component upload API is the same upload, under the same rule.
	upload := func(user, password, file string) (int, string) {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		mw.WriteField("raw.directory", "api")
		mw.WriteField("raw.asset1.filename", file)
		w, _ := mw.CreateFormFile("raw.asset1", file)
		io.WriteString(w, "x")
		mw.Close()
		req, _ := http.NewRequest("POST", cc.url+nx+"/components?repository=raw-hosted", &body)
		req.SetBasicAuth(user, password)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("X-Command-Center-Publish-Time", past)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if st, b := upload("ci", "ci-password-1", "by-ci.txt"); st != 403 || exists("/repository/raw-hosted/api/by-ci.txt") {
		t.Errorf("component upload by a deployer: %d %s", st, b)
	}
	if st, b := upload("admin", adminPassword, "by-admin.txt"); st != 204 {
		t.Errorf("component upload by an administrator: %d %s", st, b)
	}
	assets.Items = nil
	json.Unmarshal(cc.must(t, "GET", nx+"/assets?repository=raw-hosted", nil), &assets)
	found = false
	for _, a := range assets.Items {
		if str(a, "path") == "/api/by-admin.txt" {
			found = strings.HasPrefix(str(a, "blobCreated"), "2020-01-02T03:04:05")
		}
	}
	if !found {
		t.Errorf("the component upload did not carry the time: %v", assets.Items)
	}

	// /-/api/me says who may, as the import reads it.
	meOf := func(user, password string) bool {
		var me struct {
			StatesPublishTime bool `json:"statesPublishTime"`
		}
		st, b := cc.do(t, user, password, "GET", "/-/api/me", nil)
		if st != 200 || json.Unmarshal(b, &me) != nil {
			t.Fatalf("me %s: %d %s", user, st, b)
		}
		return me.StatesPublishTime
	}
	if !meOf("admin", adminPassword) || meOf("admin", adminToken) || meOf("ci", "ci-password-1") {
		t.Errorf("me: admin %v, admin's token %v, ci %v", meOf("admin", adminPassword), meOf("admin", adminToken), meOf("ci", "ci-password-1"))
	}
	// What a document claims is no way around it: an npm publish by a deployer
	// that names a time and a publisher gets the server's.
	tarball := npmTarball(t, "claimed", "1.0.0")
	doc, _ := json.Marshal(obj{"_id": "claimed", "name": "claimed", "time": obj{"1.0.0": past, "created": past},
		"versions":     obj{"1.0.0": obj{"name": "claimed", "version": "1.0.0", "_npmUser": obj{"name": "alice"}}},
		"_attachments": obj{"claimed-1.0.0.tgz": obj{"content_type": "application/octet-stream", "length": len(tarball), "data": tarball}}})
	if st, b := send("ci", "ci-password-1", "PUT", "/repository/npm-hosted/claimed", "", string(doc)); st/100 != 2 {
		t.Fatalf("npm publish: %d %s", st, b)
	}
	_, b := cc.do(t, "", "", "GET", "/repository/npm-hosted/claimed", nil)
	if strings.Contains(string(b), past[:10]) || !strings.Contains(string(b), `"name":"ci"`) || strings.Contains(string(b), "alice") {
		t.Errorf("a document's claims were taken: %s", b)
	}
}

// A repository Nexus has read-only: it stands closed from the moment it is
// made, is opened for its own copy alone, and a run with nothing to copy
// leaves it alone. A killed run leaves a marker with it, which the next run
// finishes by; without one, a repository that takes uploads is someone's
// choice, and the report says so.
func TestImportNexusReadOnlyRepository(t *testing.T) {
	content := &fakeNexus{comps: map[string][]nexusComponent{}, files: map[string][]byte{}}
	settings := func(name, policy, online string) string {
		return `{"name":"` + name + `","format":"raw","type":"hosted","url":"x","online":` + online + `,"storage":{"blobStoreName":"default",
			"strictContentTypeValidation":false,"writePolicy":"` + policy + `"},"cleanup":null,"component":{"proprietaryComponents":false},"raw":{"contentDisposition":"INLINE"}}`
	}
	nexus := &nexusConfig{fallback: content, get: map[string]string{
		nx + "/blobstores":         `[{"softQuota":null,"name":"default","type":"File"}]`,
		nx + "/repositorySettings": `[` + settings("raw-frozen", "DENY", "true") + `,` + settings("raw-open", "ALLOW_ONCE", "true") + `,` + settings("raw-offline", "ALLOW_ONCE", "false") + `,` + settings("raw-down", "DENY", "false") + `]`,
	}}
	src := httptest.NewServer(nexus)
	defer src.Close()
	content.url = src.URL
	content.repos = []nexusRepo{{"raw-frozen", "raw", "hosted"}, {"raw-open", "raw", "hosted"}, {"raw-offline", "raw", "hosted"}, {"raw-down", "raw", "hosted"}}
	content.comps["raw-frozen"] = []nexusComponent{{Name: "a.txt", Assets: []nexusAsset{content.asset("raw-frozen", "a.txt", []byte("a"))}}}
	content.comps["raw-open"] = []nexusComponent{{Name: "b.txt", Assets: []nexusAsset{content.asset("raw-open", "b.txt", []byte("b"))}}}
	content.comps["raw-offline"] = []nexusComponent{{Name: "c.txt", Assets: []nexusAsset{content.asset("raw-offline", "c.txt", []byte("c"))}}}
	content.comps["raw-down"] = []nexusComponent{{Name: "d.txt", Assets: []nexusAsset{content.asset("raw-down", "d.txt", []byte("d"))}}}

	cc := newCC(t)
	policyOf := func(name string) string {
		t.Helper()
		return str(sub(cc.named(t, nx+"/repositorySettings", "name", name), "storage"), "writePolicy")
	}
	const marker = nx + "/tags/import-nexus-open.raw-frozen"
	hasMarker := func() bool {
		st, _ := cc.do(t, "", "", "GET", marker, nil)
		return st == 200
	}
	served := func(p string) bool {
		st, _ := cc.do(t, "", "", "GET", p, nil)
		return st == 200
	}

	// The plan names it; nothing is made.
	out, err := importConfig(t, src.URL, cc, "-config", "-dry-run")
	if err == nil || !strings.Contains(out, "config\trepository\traw-frozen\twould stay read-only but for its copy") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}

	// The configuration alone: closed, as Nexus has it, with nothing in it.
	out, err = importConfig(t, src.URL, cc, "-config-only")
	if err != nil || policyOf("raw-frozen") != "DENY" || served("/repository/raw-frozen/a.txt") {
		t.Fatalf("-config-only: %v, %s\n%s", err, policyOf("raw-frozen"), out)
	}

	// The copy: read-only again, with its file, and what is offline on Nexus
	// said plainly; no file of it can be read, so nothing opens for it.
	out, err = importConfig(t, src.URL, cc, "-config")
	if err == nil || !strings.Contains(err.Error(), "2 errors") {
		t.Errorf("copy: %v", err)
	}
	for _, want := range []string{"config\trepository\traw-frozen\topening for its copy", "raw-frozen\traw\ta.txt\t\t1\t1\tok\n", "config\trepository\traw-frozen\tclosed: read-only, as Nexus has it\n",
		"raw-offline: error: it is offline on the Nexus server", "raw-down: error: it is offline on the Nexus server", "raw-open\traw\tb.txt\t\t1\t1\tok\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("copy lacks %q:\n%s", want, out)
		}
	}
	if policyOf("raw-frozen") != "DENY" || !served("/repository/raw-frozen/a.txt") || hasMarker() {
		t.Errorf("after the copy: %s, %v, marker %v", policyOf("raw-frozen"), served("/repository/raw-frozen/a.txt"), hasMarker())
	}
	if st, b := cc.do(t, "", "", "PUT", "/repository/raw-frozen/new.txt", "x"); st != 400 || !strings.Contains(string(b), "read-only") {
		t.Errorf("a closed repository took an upload: %d %s", st, b)
	}
	if s := cc.named(t, nx+"/repositorySettings", "name", "raw-down"); policyOf("raw-down") != "DENY" || s["online"] != false {
		t.Errorf("raw-down: %v", s)
	}

	// Nothing to copy: it never opens.
	out, _ = importConfig(t, src.URL, cc, "-config")
	if strings.Contains(out, "raw-frozen\topening") || !strings.Contains(out, "raw-frozen\traw\ta.txt\t\t0\t0\tskipped\n") || lineOf(out, "repository", "raw-frozen") != "unchanged" {
		t.Errorf("a run with nothing to copy:\n%s", out)
	}

	// Something to copy on a finished target: open, copied, closed.
	content.comps["raw-frozen"] = append(content.comps["raw-frozen"], nexusComponent{Name: "e.txt", Assets: []nexusAsset{content.asset("raw-frozen", "e.txt", []byte("e"))}})
	out, _ = importConfig(t, src.URL, cc, "-config")
	if !strings.Contains(out, "raw-frozen\topening for its copy") || !strings.Contains(out, "raw-frozen\traw\te.txt\t\t1\t1\tok\n") || !served("/repository/raw-frozen/e.txt") || policyOf("raw-frozen") != "DENY" {
		t.Errorf("a run with a file to copy: %s\n%s", policyOf("raw-frozen"), out)
	}

	// What a killed run leaves: open, with its marker. The next run says so,
	// copies, closes and removes the marker, without -update.
	open := hostedBody("raw-frozen", "ALLOW_ONCE", map[string]any{"raw": obj{"contentDisposition": "INLINE"}})
	leftOpen := func() {
		cc.must(t, "POST", nx+"/tags", obj{"name": "import-nexus-open.raw-frozen"})
		cc.must(t, "PUT", nx+"/repositories/raw/hosted/raw-frozen", open)
	}
	leftOpen()
	content.comps["raw-frozen"] = append(content.comps["raw-frozen"], nexusComponent{Name: "f.txt", Assets: []nexusAsset{content.asset("raw-frozen", "f.txt", []byte("f"))}})
	out, _ = importConfig(t, src.URL, cc, "-config")
	if lineOf(out, "repository", "raw-frozen") == "" || strings.Contains(out, "raw-frozen\tdiffers") || !strings.Contains(out, "raw-frozen\tan earlier run left it open") ||
		!strings.Contains(out, "raw-frozen\traw\tf.txt\t\t1\t1\tok\n") || !strings.Contains(out, "raw-frozen\tclosed: read-only") {
		t.Errorf("the run after a killed one:\n%s", out)
	}
	if policyOf("raw-frozen") != "DENY" || hasMarker() {
		t.Errorf("after an interrupted run: %s, marker %v", policyOf("raw-frozen"), hasMarker())
	}
	// A marker that outlived its closing (killed between the two) goes.
	cc.must(t, "POST", nx+"/tags", obj{"name": "import-nexus-open.raw-frozen"})
	out, _ = importConfig(t, src.URL, cc, "-config-only")
	if hasMarker() || policyOf("raw-frozen") != "DENY" || lineOf(out, "repository", "raw-frozen") != "unchanged" {
		t.Errorf("a stale marker: %v, %s\n%s", hasMarker(), policyOf("raw-frozen"), out)
	}
	// With no copy to run the end of the run closes it, and a dry run does not.
	leftOpen()
	if _, err := importConfig(t, src.URL, cc, "-config-only", "-dry-run"); err != nil || policyOf("raw-frozen") != "ALLOW_ONCE" {
		t.Errorf("dry run: %v, %s", err, policyOf("raw-frozen"))
	}
	out, err = importConfig(t, src.URL, cc, "-config-only")
	if err != nil || policyOf("raw-frozen") != "DENY" || hasMarker() {
		t.Errorf("-config-only after a killed run: %v, %s, marker %v\n%s", err, policyOf("raw-frozen"), hasMarker(), out)
	}

	// No marker: someone chose to. It stays, and the report says so; -update closes it.
	cc.must(t, "PUT", nx+"/repositories/raw/hosted/raw-frozen", open)
	out, _ = importConfig(t, src.URL, cc, "-config")
	if policyOf("raw-frozen") != "ALLOW_ONCE" || !strings.Contains(out, "raw-frozen\tdiffers: storage.writePolicy") ||
		!strings.Contains(out, "needs manual action: repository raw-frozen: it takes uploads here and is read-only on Nexus") {
		t.Errorf("a repository opened by hand: %s\n%s", policyOf("raw-frozen"), out)
	}
	out, _ = importConfig(t, src.URL, cc, "-config", "-update")
	if policyOf("raw-frozen") != "DENY" || strings.Contains(out, "raw-frozen: it takes uploads") {
		t.Errorf("-update: %s\n%s", policyOf("raw-frozen"), out)
	}

	// Target closed by hand, Nexus not: its files are refused one by one, with the reason.
	cc.must(t, "PUT", nx+"/repositories/raw/hosted/raw-open", hostedBody("raw-open", "DENY", map[string]any{"raw": obj{"contentDisposition": "INLINE"}}))
	content.comps["raw-open"] = append(content.comps["raw-open"], nexusComponent{Name: "g.txt", Assets: []nexusAsset{content.asset("raw-open", "g.txt", []byte("g"))}})
	out, _ = importConfig(t, src.URL, cc, "-config")
	if !strings.Contains(out, "raw-open\traw\tg.txt\t\t0\t0\terror: g.txt: PUT /repository/raw-open/g.txt: HTTP 400: repository \"raw-open\" is read-only") {
		t.Errorf("closed by hand:\n%s", out)
	}
}

// A docker repository without anonymous pull in Nexus has none after the
// move, whatever the target's global setting: by its forceBasicAuth, or by a
// Docker Bearer Token realm that is off.
func TestImportNexusDockerAnonymousPull(t *testing.T) {
	docker := func(name string, force bool) string {
		return fmt.Sprintf(`{"name":%q,"format":"docker","type":"hosted","url":"x","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW_ONCE"},
			"cleanup":null,"docker":{"v1Enabled":false,"forceBasicAuth":%v,"httpPort":null,"httpsPort":null,"subdomain":null}}`, name, force)
	}
	stand := func(realms string) *httptest.Server {
		srv := httptest.NewServer(&nexusConfig{get: map[string]string{
			nx + "/blobstores":             `[{"softQuota":null,"name":"default","type":"File"}]`,
			nx + "/repositorySettings":     `[` + docker("closed", true) + `,` + docker("open", false) + `]`,
			nx + "/security/realms/active": realms,
			nx + "/security/anonymous":     `{"enabled":true,"userId":"anonymous","realmName":"NexusAuthorizingRealm"}`,
		}})
		t.Cleanup(srv.Close)
		return srv
	}
	force := func(cc *ccServer, name string) any {
		return sub(cc.named(t, nx+"/repositorySettings", "name", name), "docker")["forceBasicAuth"]
	}

	on := stand(`["NexusAuthenticatingRealm","DockerToken"]`)
	cc := newCC(t)
	cc.must(t, "PUT", native+"/settings", obj{"anonymousRead": true}) // a global that would open what is not excluded
	out, err := importConfig(t, on.URL, cc, "-config-only", "-dry-run")
	if err != nil || !strings.Contains(out, "docker: anonymous pull would be off on 1 repositories") {
		t.Errorf("dry run: %v\n%s", err, out)
	}
	out, err = importConfig(t, on.URL, cc, "-config-only")
	if err != nil || force(cc, "closed") != true || force(cc, "open") != false {
		t.Errorf("realm on: %v, %v %v\n%s", err, force(cc, "closed"), force(cc, "open"), out)
	}
	if !strings.Contains(out, "docker: anonymous pull is off on 1 repositories, because Nexus does not allow it on them\n") ||
		!strings.Contains(out, "anonymous pull follows the anonymous read setting") {
		t.Errorf("report:\n%s", out)
	}
	// The same words on a run that changes nothing: the report is of the state.
	out, _ = importConfig(t, on.URL, cc, "-config-only")
	if !strings.Contains(out, "docker: anonymous pull is off on 1 repositories") || lineOf(out, "repository", "closed") != "unchanged" {
		t.Errorf("second run:\n%s", out)
	}
	if st, _ := anonymousGet(cc, "/v2/closed/x/tags/list"); st != 401 {
		t.Errorf("anonymous pull from a closed repository: %d", st)
	}

	// Without the realm no docker repository of Nexus has anonymous pull.
	off := stand(`["NexusAuthenticatingRealm"]`)
	cc2 := newCC(t)
	out, err = importConfig(t, off.URL, cc2, "-config-only")
	if err != nil || force(cc2, "closed") != true || force(cc2, "open") != true ||
		!strings.Contains(out, "docker: anonymous pull is off on 2 repositories") || !strings.Contains(out, "so is its Docker Bearer Token realm") {
		t.Errorf("realm off: %v\n%s", err, out)
	}

	// A target that has it open already: left as it is, and said; -update closes it.
	cc3 := newCC(t)
	cc3.must(t, "POST", nx+"/repositories/docker/hosted", hostedBody("closed", "ALLOW_ONCE", map[string]any{"docker": obj{"forceBasicAuth": false}}))
	out, _ = importConfig(t, on.URL, cc3, "-config-only")
	if force(cc3, "closed") != false || !strings.Contains(out, "differs: docker.forceBasicAuth") ||
		!strings.Contains(out, "needs manual action: repository closed: it allows anonymous pull here and Nexus does not") {
		t.Errorf("open on the target:\n%s", out)
	}
	out, _ = importConfig(t, on.URL, cc3, "-config-only", "-update")
	if force(cc3, "closed") != true || !strings.Contains(out, "docker: anonymous pull is off on 1 repositories") {
		t.Errorf("-update:\n%s", out)
	}
}

func anonymousGet(c *ccServer, p string) (int, string) {
	resp, err := http.Get(c.url + p)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// An interrupted copy still closes what it opened, says what it did not
// reach, and ends in an error.
func TestImportNexusInterruptedClosesWhatItOpened(t *testing.T) {
	content := &fakeNexus{comps: map[string][]nexusComponent{}, files: map[string][]byte{}}
	inCopy := make(chan struct{}, 1)
	settings := func(name, policy string) string {
		return `{"name":"` + name + `","format":"raw","type":"hosted","url":"x","online":true,"storage":{"blobStoreName":"default",
			"strictContentTypeValidation":false,"writePolicy":"` + policy + `"},"cleanup":null,"component":{"proprietaryComponents":false},"raw":{"contentDisposition":"INLINE"}}`
	}
	nexus := &nexusConfig{get: map[string]string{
		nx + "/blobstores":         `[{"softQuota":null,"name":"default","type":"File"}]`,
		nx + "/repositorySettings": `[` + settings("raw-frozen", "DENY") + `,` + settings("raw-later", "ALLOW_ONCE") + `]`,
	}, fallback: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repository/raw-frozen/slow.txt" { // the copy waits here for the signal
			inCopy <- struct{}{}
			<-r.Context().Done()
			return
		}
		content.ServeHTTP(w, r)
	})}
	src := httptest.NewServer(nexus)
	defer src.Close()
	content.url = src.URL
	content.repos = []nexusRepo{{"raw-frozen", "raw", "hosted"}, {"raw-later", "raw", "hosted"}}
	content.comps["raw-frozen"] = []nexusComponent{{Name: "slow.txt", Assets: []nexusAsset{content.asset("raw-frozen", "slow.txt", []byte("s"))}}}
	content.comps["raw-later"] = []nexusComponent{{Name: "b.txt", Assets: []nexusAsset{content.asset("raw-later", "b.txt", []byte("b"))}}}
	cc := newCC(t)
	type done struct {
		out string
		err error
	}
	res := make(chan done, 1)
	go func() {
		out, err := importConfig(t, src.URL, cc, "-config")
		res <- done{out, err}
	}()
	select {
	case <-inCopy:
	case d := <-res:
		t.Fatalf("the run ended before its copy: %v\n%s", d.err, d.out)
	case <-time.After(30 * time.Second):
		t.Fatal("the copy did not start")
	}
	settingsOf := func() string {
		return str(sub(cc.named(t, nx+"/repositorySettings", "name", "raw-frozen"), "storage"), "writePolicy")
	}
	if got := settingsOf(); got != "ALLOW_ONCE" {
		t.Errorf("during its copy: %s", got)
	}
	if st, _ := cc.do(t, "", "", "GET", nx+"/tags/import-nexus-open.raw-frozen", nil); st != 200 {
		t.Errorf("no marker during the copy: %d", st)
	}
	syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	d := <-res
	if d.err == nil || !strings.Contains(d.out, "raw-frozen\tclosed: read-only") || !strings.Contains(d.out, "interrupted: the repositories from raw-later on were not copied") {
		t.Errorf("the interrupted run: %v\n%s", d.err, d.out)
	}
	if got := settingsOf(); got != "DENY" {
		t.Errorf("after the interrupt: %s", got)
	}
	if st, _ := cc.do(t, "", "", "GET", nx+"/tags/import-nexus-open.raw-frozen", nil); st != 404 {
		t.Errorf("the marker stayed: %d", st)
	}
}

// A file's downloadUrl is where a client reads it, in the formats that store
// it elsewhere too: the import reads Command Center as a source through it.
func TestAssetDownloadURLsAreServed(t *testing.T) {
	cc := newCC(t)
	cc.must(t, "POST", nx+"/repositories/nuget/hosted", hostedBody("nuget-hosted", "ALLOW_ONCE", nil))
	cc.must(t, "POST", nx+"/repositories/gitlfs/hosted", hostedBody("lfs-hosted", "ALLOW_ONCE", nil))
	nuspec := `<?xml version="1.0"?><package><metadata><id>Demo.Lib</id><version>1.2.0</version><authors>Jane</authors><description>demo</description></metadata></package>`
	cc.must(t, "PUT", "/repository/nuget-hosted/api/v2/package", string(zipOf(t, map[string]string{"Demo.Lib.nuspec": nuspec, "lib/a.dll": "MZ"})))
	sum := sha256.Sum256([]byte("an object"))
	cc.must(t, "PUT", "/repository/lfs-hosted/info/lfs/objects/"+hex.EncodeToString(sum[:]), "an object")
	for repoName, want := range map[string]int{"nuget-hosted": 2, "lfs-hosted": 1} {
		var assets struct {
			Items []obj `json:"items"`
		}
		json.Unmarshal(cc.must(t, "GET", nx+"/assets?repository="+repoName, nil), &assets)
		if len(assets.Items) != want {
			t.Errorf("%s: %d assets, want %d", repoName, len(assets.Items), want)
		}
		for _, a := range assets.Items {
			u, err := url.Parse(str(a, "downloadUrl"))
			if err != nil {
				t.Fatal(err)
			}
			if st, b := cc.do(t, "", "", "GET", u.Path, nil); st != 200 {
				t.Errorf("%s: %d %s", u.Path, st, b)
			}
		}
	}
}

// Nexus's static groups come along with the roles they give. Settings that
// cannot work are the target's 400, which the line reports; with
// ldapGroupsAsRoles off Nexus reads no groups, and neither does the target.
func TestImportNexusLDAPStaticGroups(t *testing.T) {
	stand := func(group string) *httptest.Server {
		srv := httptest.NewServer(&nexusConfig{get: map[string]string{nx + "/security/ldap": `[{"id":"a","order":1,"name":"corp","protocol":"ldap","host":"ldap.example.org","port":389,
			"searchBase":"dc=example,dc=org","userBaseDn":"ou=people","userIdAttribute":"uid","userObjectClass":"inetOrgPerson","userSubtree":true,"authScheme":"NONE",` + group + `}]`}})
		t.Cleanup(srv.Close)
		return srv
	}
	const set = `"groupType":"static","groupBaseDn":"ou=groups","groupSubtree":true,"groupObjectClass":"groupOfNames","groupIdAttribute":"cn","groupMemberAttribute":"member"`

	on := stand(`"ldapGroupsAsRoles":true,` + set + `,"groupMemberFormat":"${dn}"`)
	cc := newCC(t)
	out, err := importConfig(t, on.URL, cc, "-config-only")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ldap, _ := cc.get(t, native+"/realms/ldap").(obj)
	for k, want := range map[string]any{"groupType": "static", "groupBaseDN": "ou=groups,dc=example,dc=org", "groupSubtree": true, "groupObjectClass": "groupOfNames",
		"groupIdAttr": "cn", "groupMemberAttr": "member", "groupMemberFormat": "${dn}"} {
		if ldap[k] != want {
			t.Errorf("%s: %v, want %v", k, ldap[k], want)
		}
	}
	if out, _ := importConfig(t, on.URL, cc, "-config-only"); lineOf(out, "ldap", "corp") != "unchanged" {
		t.Errorf("second run: %s\n%s", lineOf(out, "ldap", "corp"), out)
	}

	// A format that names nobody: the target's 400, on the line, and exit 1.
	cc2 := newCC(t)
	out, err = importConfig(t, stand(`"ldapGroupsAsRoles":true,`+set+`,"groupMemberFormat":"everyone"`).URL, cc2, "-config-only")
	if err == nil || !strings.Contains(lineOf(out, "ldap", "corp"), "groupMemberFormat must contain") {
		t.Errorf("a format without a placeholder: %v, %q\n%s", err, lineOf(out, "ldap", "corp"), out)
	}
	if ldap, _ := cc2.get(t, native+"/realms/ldap").(obj); str(ldap, "url") != "" {
		t.Errorf("the refused settings were stored: %v", ldap)
	}

	// The role switch off: not carried, and the line says so.
	cc3 := newCC(t)
	out, err = importConfig(t, stand(`"ldapGroupsAsRoles":false,`+set+`,"groupMemberFormat":"${dn}"`).URL, cc3, "-config-only")
	if err != nil || !strings.Contains(out, "its static group settings are not carried: ldapGroupsAsRoles is off") {
		t.Errorf("the role switch off: %v\n%s", err, out)
	}
	if ldap, _ := cc3.get(t, native+"/realms/ldap").(obj); str(ldap, "groupType") == "static" || str(ldap, "url") == "" {
		t.Errorf("the role switch off: %v", ldap)
	}
}
