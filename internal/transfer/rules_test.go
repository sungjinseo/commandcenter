package transfer

import (
	"testing"

	"commandcenter/internal/meta"
)

// Every format's rows are found from their file's path (rowName) and
// matched back to it (rowPath), so an export carries them.
func TestRowsFollowTheirFile(t *testing.T) {
	for _, c := range []struct{ format, path, name, version, attrs string }{
		{"npm", "demo/-/demo-1.0.0.tgz", "demo", "1.0.0", "{}"},
		{"npm", "@scope/demo/-/demo-1.0.0.tgz", "@scope/demo", "1.0.0", "{}"},
		{"pypi", "packages/demo/demo-1.0.tar.gz", "demo", "demo-1.0.tar.gz", "{}"},
		{"cargo", "crates/demo/1.0.0.crate", "demo", "1.0.0", "{}"},
		{"docker", "lib/app/manifests/sha256:" + sha64, "lib/app", "sha256:" + sha64, "{}"},
		{"helm", "demo-0.1.0.tgz", "demo", "0.1.0", "{}"},
		{"yum", "Packages/d/demo-1.0-1.el9.x86_64.rpm", "Packages/d/demo", "1.0-1.el9.x86_64", "{}"},
		{"nuget", "demo/1.0.0/demo.1.0.0.nupkg", "demo", "1.0.0", "{}"},
		{"composer", "dist/acme/demo/1.0.0.0.zip", "acme/demo", "1.0.0.0", "{}"},
		{"conda", "linux-64/demo-1.0-py_0.conda", "demo", "linux-64/demo-1.0-py_0.conda", "{}"},
		{"rubygems", "gems/demo-1.0.0-java.gem", "demo", "1.0.0-java", "{}"},
		{"r", "src/contrib/demo_1.0.tar.gz", "src/contrib/demo", "1.0", "{}"},
		{"conan", "v2/conans/zlib/1.3/_/_/revisions/abc/files/conanmanifest.txt", "zlib/1.3", "abc", "{}"},
		{"conan", "v2/conans/zlib/1.3/me/stable/revisions/abc/files/conanmanifest.txt", "zlib/1.3@me/stable", "abc", "{}"},
		{"apt", "pool/main/d/demo/demo_1.0_amd64.deb", "demo", "1.0_amd64", `{"filename":"pool/main/d/demo/demo_1.0_amd64.deb"}`},
	} {
		if name, ok := rowName(c.format, c.path); !ok || name != c.name {
			t.Errorf("rowName(%s, %s) = %q %v, want %q", c.format, c.path, name, ok, c.name)
		}
		if p := rowPath(c.format, meta.Package{Name: c.name, Version: c.version, Attrs: c.attrs}); p != c.path {
			t.Errorf("rowPath(%s, %s %s) = %q, want %q", c.format, c.name, c.version, p, c.path)
		}
	}
	for _, f := range []string{"maven", "raw", "go", "gitlfs"} {
		if _, ok := rowName(f, "a/b/c"); ok {
			t.Errorf("%s has no rows", f)
		}
	}
}

const sha64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestDerivedAndComponents(t *testing.T) {
	for _, c := range []struct {
		format, path string
		derived      bool
	}{
		{"maven", "org/example/app/maven-metadata.xml", true}, {"maven", "org/example/app/1.0/app-1.0.jar", false},
		{"npm", "demo/package.json", true}, {"npm", "@s/demo/package.json", true}, {"npm", "demo/-/package.json", false},
		{"helm", "index.yaml", true}, {"helm", "demo-0.1.0.tgz", false},
		{"yum", "repodata/repomd.xml", true}, {"yum", "el9/repodata/primary.xml.gz", true}, {"yum", "a.rpm", false},
		{"apt", "dists/stable/Release", true}, {"apt", "pool/main/d/demo/demo_1.0_amd64.deb", false},
		{"conda", "linux-64/repodata.json", true}, {"conda", "channeldata.json", true},
		{"r", "src/contrib/PACKAGES.gz", true}, {"rubygems", "versions", true}, {"rubygems", "gems/a-1.gem", false},
		{"raw", "index.yaml", false}, {"pypi", "packages/demo/demo-1.0.tar.gz", false},
	} {
		if got := derived(c.format, c.path); got != c.derived {
			t.Errorf("derived(%s, %s) = %v", c.format, c.path, got)
		}
	}
	if a, b := componentOf("maven", "g/a/1.0/a-1.0.jar"), componentOf("maven", "g/a/1.0/a-1.0.pom"); a == "" || a != b {
		t.Errorf("maven files of one version: %q %q", a, b)
	}
	if a, b := componentOf("pypi", "packages/demo/demo-1.0.tar.gz"), componentOf("pypi", "packages/demo/demo-1.0.tar.gz.metadata"); a == "" || a != b {
		t.Errorf("a pypi file and its sidecar: %q %q", a, b)
	}
	if componentOf("docker", "app/blobs/sha256:"+sha64) != "" || componentOf("maven", "g/a/maven-metadata.xml") != "" {
		t.Error("blobs and metadata are no components")
	}
}

func TestRel(t *testing.T) {
	s := &Service{Root: "/srv/export"}
	for dir, want := range map[string]string{"x": "x", "a/b/../c": "a/c", "/srv/export/x": "x", "/srv/export": ".", ".": "."} {
		if got, err := s.rel(dir); err != nil || got != want {
			t.Errorf("rel(%q) = %q %v, want %q", dir, got, err, want)
		}
	}
	for _, dir := range []string{"..", "../x", "/srv/exportx", "/srv", "/etc", "a/../../x"} {
		if got, err := s.rel(dir); err == nil {
			t.Errorf("rel(%q) = %q, want an error", dir, got)
		}
	}
	if _, err := (&Service{}).rel("x"); err != ErrNoRoot {
		t.Errorf("no root: %v", err)
	}
}
