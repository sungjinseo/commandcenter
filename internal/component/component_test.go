package component

import "testing"

func TestDeriveMaven(t *testing.T) {
	tests := []struct {
		path, group, name, version string
		ok                         bool
	}{
		{"org/example/app/1.0/app-1.0.jar", "org.example", "app", "1.0", true},
		{"org/example/app/1.0/app-1.0.jar.sha1", "org.example", "app", "1.0", true},
		{"org/example/app/1.0/app-1.0-sources.jar", "org.example", "app", "1.0", true},
		{"com/x/a/1.0-SNAPSHOT/a-1.0-20261019.1-1.jar", "com.x", "a", "1.0-20261019.1-1", true}, // a build is a component
		{"com/x/a/1.0-SNAPSHOT/a-1.0-20261019.120000-2-sources.jar.asc", "com.x", "a", "1.0-20261019.120000-2", true},
		{"com/x/a/1.0-SNAPSHOT/a-1.0-SNAPSHOT.jar", "com.x", "a", "1.0-SNAPSHOT", true},                            // non-unique
		{"com/x/a/1.0-20261019.120000-2/a-1.0-20261019.120000-2.jar", "com.x", "a", "1.0-20261019.120000-2", true}, // a release named so
		{"com/x/sbt-a_2.12_1.0/1.0/sbt-a_2.12_1.0-1.0.jar", "com.x", "sbt-a_2.12_1.0", "1.0", true},
		{"com/x/sbt-a_2.12_1.0/1.0/sbt-a-1.0-sources.jar", "com.x", "sbt-a_2.12_1.0", "1.0", true}, // sbt's legacy layout
		{"org/example/app/maven-metadata.xml", "", "", "", false},
		{"org/example/app/maven-metadata.xml.sha1", "", "", "", false},
		{"com/x/a/1.0-SNAPSHOT/maven-metadata.xml", "", "", "", false},
		{"a/1.0/a-1.0.jar", "", "", "", false}, // no group
		{"readme.txt", "", "", "", false},
	}
	for _, tc := range tests {
		g, n, v, ok := DeriveMaven(tc.path)
		if g != tc.group || n != tc.name || v != tc.version || ok != tc.ok {
			t.Errorf("DeriveMaven(%q) = %q %q %q %v, want %q %q %q %v", tc.path, g, n, v, ok, tc.group, tc.name, tc.version, tc.ok)
		}
	}
}

func TestDeriveNpm(t *testing.T) {
	tests := []struct {
		path, group, name, version string
		ok                         bool
	}{
		{"left-pad/-/left-pad-1.0.0.tgz", "", "left-pad", "1.0.0", true},
		{"@scope/util/-/util-2.0.0.tgz", "@scope", "util", "2.0.0", true},
		{"@scope/util/-/util-2.0.0-beta.1.tgz", "@scope", "util", "2.0.0-beta.1", true},
		{"left-pad", "", "", "", false},
		{"left-pad/-/other-1.0.0.tgz", "", "", "", false},
		{"left-pad/-/left-pad-1.0.0.zip", "", "", "", false},
	}
	for _, tc := range tests {
		g, n, v, ok := DeriveNpm(tc.path)
		if g != tc.group || n != tc.name || v != tc.version || ok != tc.ok {
			t.Errorf("DeriveNpm(%q) = %q %q %q %v, want %q %q %q %v", tc.path, g, n, v, ok, tc.group, tc.name, tc.version, tc.ok)
		}
	}
}

func TestDeriveGo(t *testing.T) {
	tests := []struct {
		path, name, version string
		ok                  bool
	}{
		{"example.com/m/v2/@v/v2.1.0.zip", "example.com/m/v2", "v2.1.0", true},
		{"example.com/m/@v/v1.0.0.mod", "example.com/m", "v1.0.0", true},
		{"example.com/m/@v/v1.0.0.info", "example.com/m", "v1.0.0", true},
		{"example.com/m/@v/list", "", "", false},
		{"example.com/m/@latest", "", "", false},
	}
	for _, tc := range tests {
		n, v, ok := DeriveGo(tc.path)
		if n != tc.name || v != tc.version || ok != tc.ok {
			t.Errorf("DeriveGo(%q) = %q %q %v, want %q %q %v", tc.path, n, v, ok, tc.name, tc.version, tc.ok)
		}
	}
}

func TestPyPIDisplayVersion(t *testing.T) {
	tests := []struct{ project, file, want string }{
		{"demo-pkg", "demo_pkg-1.2.0-py3-none-any.whl", "1.2.0"},
		{"demo-pkg", "demo-pkg-1.2.0.tar.gz", "1.2.0"},
		{"demo-pkg", "demo_pkg-1.2.0.tar.gz", "1.2.0"},
		{"my-pkg", "my_pkg-1.0.tar.gz", "1.0"},
		{"demo", "weird", "weird"},
	}
	for _, tc := range tests {
		if got := PyPIDisplayVersion(tc.project, tc.file); got != tc.want {
			t.Errorf("PyPIDisplayVersion(%q, %q) = %q, want %q", tc.project, tc.file, got, tc.want)
		}
	}
}

func TestDeriveMavenFileMatch(t *testing.T) {
	tests := []struct {
		path string
		ok   bool
	}{
		{"app/1.0/app-2.0.jar", false},
		{"g/app/1.0/app-2.0.jar", false},
		{"g/app/1.0/other-1.0.jar", false},
		{"g/app/1.0/app-1.0.0.jar", true}, // 1.0 then ".0": boundary after the version
		{"g/app/1.0/app-1.05.jar", false},
		{"g/a/1.0-SNAPSHOT/a-1.0-20261019.123456-7.jar", true},
		{"g/a/1.0-SNAPSHOT/a-1.0-20261019.123456-7-sources.jar", true},
		{"g/a/1.0-SNAPSHOT/a-1.0-SNAPSHOT.jar", true},
		{"g/a/1.0-SNAPSHOT/a-2.0-20261019.123456-7.jar", false},
		{"g/a/1.0-SNAPSHOT/a-1.0-notastamp.jar", false},
		{"g/sbt-a_2.12_1.0/1.0-SNAPSHOT/sbt-a-1.0-20261019.123456-7.jar", true},
		{"g/sbt-a_2.12_1.0/1.0/sbt-b-1.0.jar", false},
		{"g/sbt-a_2.12_1.0/1.0/sbt-a-2.0.jar", false},
		{"g/sbt-a_2.12/1.0/sbt-a-1.0.jar", false}, // a Scala suffix only
	}
	for _, tc := range tests {
		if _, _, _, ok := DeriveMaven(tc.path); ok != tc.ok {
			t.Errorf("DeriveMaven(%q) ok = %v, want %v", tc.path, ok, tc.ok)
		}
	}
}

func TestPyPIDisplayVersionSpellings(t *testing.T) {
	tests := []struct{ project, file, want string }{
		{"demo-pkg", "demo-pkg-1.2.0-py3-none-any.whl", "1.2.0"},
		{"demo-pkg", "demo_pkg-1.2.0.tar.gz", "1.2.0"},
		{"demo-pkg", "Demo.Pkg-1.2.0.zip", "1.2.0"},
		{"demo-pkg", "demo_pkg-1.2.0-py3-none-any.whl", "1.2.0"},
		{"demo-pkg", "demo-pkg-1.2.0.tar.bz2", "1.2.0"},
		{"demo-pkg", "demo_pkg-1.2.0-py3.8.egg", "1.2.0-py3.8"},
	}
	for _, tc := range tests {
		if got := PyPIDisplayVersion(tc.project, tc.file); got != tc.want {
			t.Errorf("PyPIDisplayVersion(%q, %q) = %q, want %q", tc.project, tc.file, got, tc.want)
		}
	}
}

func TestDeriveAsset(t *testing.T) {
	tests := []struct {
		format, path, group, name, version string
		ok                                 bool
	}{
		{"npm", "@scope/util/-/util-2.0.0.tgz", "@scope", "util", "2.0.0", true},
		{"npm", "left-pad/-/left-pad-1.0.0.tgz", "", "left-pad", "1.0.0", true},
		{"npm", "left-pad", "", "", "", false},
		{"pypi", "packages/demo-pkg/demo_pkg-1.2.0.tar.gz", "", "demo-pkg", "1.2.0", true},
		{"pypi", "packages/demo-pkg/1.2.0/demo_pkg-1.2.0-py3-none-any.whl", "", "demo-pkg", "1.2.0", true},
		{"pypi", "packages/demo-pkg/demo_pkg-1.2.0.zip", "", "demo-pkg", "1.2.0", true},
		{"pypi", "packages/demo-pkg/demo_pkg-1.2.0.egg", "", "demo-pkg", "1.2.0", true},
		{"pypi", "packages/demo-pkg/demo_pkg-1.2.0.tar.bz2", "", "demo-pkg", "1.2.0", true},
		{"pypi", "packages/demo-pkg/demo_pkg-1.2.0-py3-none-any.whl.metadata", "", "demo-pkg", "1.2.0", true},
		{"pypi", "packages/demo-pkg/demo_pkg-1.2.0.tar.gz.metadata", "", "demo-pkg", "1.2.0", true},
		{"pypi", "packages/demo-pkg/.metadata", "", "", "", false},
		{"pypi", "packages/demo-pkg/demo_pkg-1.2.0.txt", "", "", "", false},
		{"pypi", "packages/demo-pkg/demo_pkg-1.2.0.tar.gz.sha256", "", "", "", false},
		{"pypi", "simple/demo-pkg/", "", "", "", false},
		{"pypi", "packages/demo-pkg", "", "", "", false},
		{"cargo", "crates/serde/1.0.0.crate", "", "serde", "1.0.0", true},
		{"cargo", "crates/serde/1.0.0", "", "", "", false},
		{"cargo", "index/se/rd/serde", "", "", "", false},
		{"helm", "demo-1.0.0.tgz", "", "demo", "1.0.0", true},
		{"helm", "ingress-nginx-4.10.0-rc.1.tgz", "", "ingress-nginx", "4.10.0-rc.1", true}, // "-" in the name and the prerelease
		{"helm", "demo-1.0.0_build.2.tgz", "", "demo", "1.0.0+build.2", true},               // helm package writes "+" as "_"
		{"helm", "demo-1.0.tgz", "", "", "", false},                                         // not SemVer 2
		{"helm", "index.yaml", "", "", "", false},
		{"helm", "upstream/index.yaml", "", "", "", false},
		{"helm", "a/demo-1.0.0.tgz", "", "", "", false},
		{"yum", "demo-1.0-1.el9.x86_64.rpm", "", "demo", "1.0-1.el9.x86_64", true},
		{"yum", "el9/Packages/d/demo-lib-1.0-1.el9.aarch64.rpm", "", "el9/Packages/d/demo-lib", "1.0-1.el9.aarch64", true}, // "-" in the name; the directory qualifies it
		{"yum", "el9/x/beta-1.0-1.el9.noarch.rpm", "", "el9/x/beta", "1.0-1.el9.noarch", true},
		{"yum", "demo-1.0.rpm", "", "", "", false},        // no release
		{"yum", "demo-1.0-1.rpm", "", "", "", false},      // no arch
		{"yum", "-1.0-1.noarch.rpm", "", "", "", false},   // no name
		{"yum", "d/-1.0-1.noarch.rpm", "", "", "", false}, // no name
		{"yum", "repodata/repomd.xml", "", "", "", false}, // metadata
		{"yum", "repodata/a-1.0-1.noarch.rpm", "", "", "", false},
		{"yum", "el9/repodata/a-1.0-1.noarch.rpm", "", "", "", false},
		{"nuget", "demo.pkg/1.0.0-beta/demo.pkg.1.0.0-beta.nupkg", "", "demo.pkg", "1.0.0-beta", true},
		{"nuget", "demo.pkg/1.0.0/demo.pkg.1.0.0.nuspec", "", "demo.pkg", "1.0.0", true},
		{"nuget", "v3-flatcontainer/demo.pkg/1.0.0/demo.pkg.1.0.0.nupkg", "", "demo.pkg", "1.0.0", true}, // a proxy's cache
		{"nuget", "v3-flatcontainer/demo.pkg/index.json", "", "", "", false},
		{"nuget", "demo.pkg/1.0.0/other.1.0.0.nupkg", "", "", "", false},
		{"nuget", "upstream/index.json", "", "", "", false},
		{"apt", "pool/main/h/hello/hello_1.0_amd64.deb", "amd64", "hello", "1.0", true}, // the architecture is the group
		{"apt", "pool/main/libf/libfoo1/libfoo1_1%3a0.9-2_arm64.deb", "arm64", "libfoo1", "1:0.9-2", true},
		{"apt", "pool/main/h/hello/other_1.0_amd64.deb", "amd64", "other", "1.0", true}, // the file name decides, wherever it lies
		{"apt", "pool/main/h/hello/hello_1.0.deb", "", "", "", false},
		{"apt", "pool/h/hello/hello_1.0_amd64.deb", "amd64", "hello", "1.0", true}, // Nexus's pool layout
		{"apt", "binary/jenkins_2.4_all.deb", "all", "jenkins", "2.4", true},       // a flat remote
		{"apt", "pool/main/d/debian-installer/di-utils_1.1_amd64.udeb", "amd64", "di-utils", "1.1", true},
		{"apt", "pool/My_File_x.deb", "", "", "", false},
		{"apt", "dists/stable/Release", "", "", "", false},
		{"composer", "dist/acme/demo/1.0.0.0.zip", "", "acme/demo", "1.0.0.0", true},
		{"composer", "dist/acme/demo/dev-main.zip", "", "acme/demo", "dev-main", true},
		{"composer", "dist/acme/demo/1.0.0.0/0123456789abcdef0123456789abcdef01234567.zip", "", "acme/demo", "1.0.0.0", true}, // a proxy's cache
		{"composer", "dist/acme/demo/dev-feature/x/0123456789abcdef0123456789abcdef01234567.zip", "", "acme/demo", "dev-feature/x", true},
		{"composer", "dist/acme/demo/1.0.0.0/notahash.zip", "", "", "", false},
		{"composer", "dist/acme/demo.zip", "", "", "", false},
		{"composer", "p2/acme/demo.json", "", "", "", false},
		{"composer", "upstream/packages.json", "", "", "", false},
		{"conda", "noarch/foo-1.0-py_0.tar.bz2", "noarch", "foo", "1.0-py_0", true},                                               // the subdir is the group, the version <version>-<build>
		{"conda", "linux-64/scikit-learn-1.4.2-py312h1fcc3ea_0.conda", "linux-64", "scikit-learn", "1.4.2-py312h1fcc3ea_0", true}, // "-" in the name
		{"conda", "osx-arm64/foo-1.0-0.conda", "osx-arm64", "foo", "1.0-0", true},
		{"conda", "noarch/foo-1.0.tar.bz2", "", "", "", false}, // no build
		{"conda", "noarch/foo.conda", "", "", "", false},
		{"conda", "noarch/-1.0-0.conda", "", "", "", false}, // no name
		{"conda", "noarch/foo--0.conda", "", "", "", false}, // no version
		{"conda", "foo-1.0-0.conda", "", "", "", false},     // no subdir
		{"conda", "Noarch/foo-1.0-0.conda", "", "", "", false},
		{"conda", "noarch/a/foo-1.0-0.conda", "", "", "", false},
		{"conda", "noarch/repodata.json", "", "", "", false}, // metadata
		{"conda", "channeldata.json", "", "", "", false},
		{"r", "src/contrib/foo_1.0.0.tar.gz", "src/contrib", "foo", "1.0.0", true}, // the contrib directory is the group
		{"r", "bin/windows/contrib/4.4/data.table_1.15-2.zip", "bin/windows/contrib/4.4", "data.table", "1.15-2", true},
		{"r", "bin/macosx/big-sur-arm64/contrib/4.4/foo_1.0.0.tgz", "bin/macosx/big-sur-arm64/contrib/4.4", "foo", "1.0.0", true},
		{"r", "src/contrib/Archive/foo/foo_0.9.tar.gz", "src/contrib/Archive/foo", "foo", "0.9", true}, // a proxy's cached old version
		{"r", "src/contrib/foo_1.0.0.zip", "", "", "", false},                                          // the directory's extension
		{"r", "src/contrib/PACKAGES.rds", "", "", "", false},                                           // the index
		{"r", "foo_1.0.0.tar.gz", "", "", "", false},
		{"terraform", "modules/acme/vpc/aws/1.2.0.tgz", "", "acme/vpc/aws", "1.2.0", true},
		{"terraform", "modules/acme/vpc/aws/1.2.0-rc.1.zip", "", "acme/vpc/aws", "1.2.0-rc.1", true},
		{"terraform", "providers/acme/demo/2.0.0/terraform-provider-demo_2.0.0_linux_amd64.zip", "", "acme/demo", "2.0.0", true},
		{"terraform", "providers/acme/demo/2.0.0/terraform-provider-demo_2.0.0_SHA256SUMS.sig", "", "acme/demo", "2.0.0", true},
		{"terraform", "providers/acme/demo/2.0.0/download_linux_amd64.json", "", "acme/demo", "2.0.0", true}, // a proxy's cached document goes with its version
		{"terraform", "v1/modules/acme/vpc/aws/versions", "", "", "", false},                                 // version lists are not components
		{"terraform", "v1/modules/acme/vpc/aws/1.2.0/download", "", "", "", false},                           // nor a proxy's kept download source
		{"terraform", ".well-known/terraform.json", "", "", "", false},
		{"terraform", "modules/acme/vpc/aws/1.2.0.tar", "", "", "", false},
		{"go", "example.com/m/@v/v1.0.0.zip", "", "example.com/m", "v1.0.0", true},
		{"maven", "org/example/app/1.0/app-1.0.jar", "org.example", "app", "1.0", true},
		{"swift", "mona/linkedlist/1.1.1/source-archive.zip", "", "mona.linkedlist", "1.1.1", true}, // the version directory's files are the component
		{"swift", "mona/linkedlist/1.1.1/release.json", "", "mona.linkedlist", "1.1.1", true},
		{"swift", "mona/linkedlist/1.2.0-beta.1/Package@swift-5.9.swift", "", "mona.linkedlist", "1.2.0-beta.1", true},
		{"swift", "mona/linkedlist/releases.json", "", "", "", false}, // the release list
		{"swift", "mona/linkedlist/v1.1.1/source-archive.zip", "", "", "", false},
		{"swift", "Mona/linkedlist/1.1.1/source-archive.zip", "", "", "", false},
		{"swift", "identifiers/0123456789abcdef.json", "", "", "", false},
		{"unknown", "a/b", "", "", "", false},
		{"p2", "plugins/org.eclipse.core.runtime_3.31.0.v20240813-0735.jar", "plugins", "org.eclipse.core.runtime", "3.31.0.v20240813-0735", true},
		{"p2", "features/org.eclipse.platform_4.33.0.v20240903-0240.jar", "features", "org.eclipse.platform", "4.33.0.v20240903-0240", true},
		{"p2", "binary/org.eclipse.platform_root_4.33.0.v20240903-0240", "binary", "org.eclipse.platform_root", "4.33.0.v20240903-0240", true}, // "_" in the id
		{"p2", ".remote/da39a3ee5e6b4b0d3255bfef95601890afd80709/plugins/a_1.0.0.jar", "plugins", "a", "1.0.0", true},                          // a composite child's mirror
		{"p2", "plugins/a_1.0.0.jar.pack200.gz", "", "", "", false},
		{"p2", "plugins/sub/a_1.0.0.jar", "", "", "", false},
		{"p2", "plugins/_1.0.0.jar", "", "", "", false},
		{"p2", "plugins/a.jar", "", "", "", false},
		{"p2", "content.jar", "", "", "", false},
		{"p2", ".remote/index.json", "", "", "", false},
	}
	for _, tc := range tests {
		g, n, v, ok := DeriveAsset(tc.format, tc.path)
		if g != tc.group || n != tc.name || v != tc.version || ok != tc.ok {
			t.Errorf("DeriveAsset(%q, %q) = %q %q %q %v, want %q %q %q %v", tc.format, tc.path, g, n, v, ok, tc.group, tc.name, tc.version, tc.ok)
		}
	}
}

func TestDeriveGoUnescapes(t *testing.T) {
	n, v, ok := DeriveGo("github.com/!burnt!sushi/toml/@v/v1.0.0-!r!c1.zip")
	if !ok || n != "github.com/BurntSushi/toml" || v != "v1.0.0-RC1" {
		t.Fatalf("DeriveGo = %q %q %v", n, v, ok)
	}
	// an invalid escape keeps the raw value
	n, v, ok = DeriveGo("example.com/bad!/@v/v1.0.0.mod")
	if !ok || n != "example.com/bad!" || v != "v1.0.0" {
		t.Fatalf("invalid escape = %q %q %v", n, v, ok)
	}
}

func TestLabel(t *testing.T) {
	tests := []struct {
		c    Component
		want string
	}{
		{Component{Format: "maven", Group: "org.example", Name: "app", Version: "1.0"}, "org.example:app:1.0"},
		{Component{Format: "npm", Group: "@s", Name: "util", Version: "2.0.0"}, "@s/util@2.0.0"},
		{Component{Format: "npm", Name: "left-pad", Version: "1.0.0"}, "left-pad@1.0.0"},
		{Component{Format: "go", Name: "example.com/m", Version: "v1.0.0"}, "example.com/m@v1.0.0"},
	}
	for _, tc := range tests {
		if got := tc.c.Label(); got != tc.want {
			t.Errorf("Label(%+v) = %q, want %q", tc.c, got, tc.want)
		}
	}
}

func TestCheckTarget(t *testing.T) {
	c := Component{Format: "npm", Name: "a", Version: "1.0.0"}
	if err := c.CheckTarget("npm-hosted", "npm"); err != nil {
		t.Fatalf("empty Repo: %v", err)
	}
	c.Repo = "npm-hosted"
	if err := c.CheckTarget("npm-hosted", "npm"); err != nil {
		t.Fatalf("matching: %v", err)
	}
	if err := c.CheckTarget("npm-proxy", "npm"); err == nil {
		t.Fatal("other repository accepted")
	}
	if err := c.CheckTarget("npm-hosted", "maven"); err == nil {
		t.Fatal("other format accepted")
	}
	c.Repo, c.Format = "", ""
	if err := c.CheckTarget("npm-hosted", "npm"); err == nil {
		t.Fatal("empty format accepted")
	}
}

func TestResultAdd(t *testing.T) {
	var r Result
	r.Add(3)
	r.Add(0)
	if r != (Result{Assets: 2, Bytes: 3}) {
		t.Fatalf("r = %+v", r)
	}
}

func TestMavenBaseVersionAndParts(t *testing.T) {
	for v, want := range map[string]string{"1.0-20261014.120000-3": "1.0-SNAPSHOT", "1.0-SNAPSHOT": "1.0-SNAPSHOT", "1.0": "1.0",
		"2.0-rc1-20261014.1-12": "2.0-rc1-SNAPSHOT", "-20261014.120000-3": "-20261014.120000-3"} {
		if got := MavenBaseVersion(v); got != want {
			t.Errorf("MavenBaseVersion(%q) = %q, want %q", v, got, want)
		}
	}
	if got := MavenVersionDirs("1.0-20261014.120000-3"); len(got) != 2 || got[0] != "1.0-SNAPSHOT" {
		t.Errorf("MavenVersionDirs = %q", got)
	}
	for _, tc := range []struct{ file, version, cl, ext string }{
		{"app-1.0.jar", "1.0", "", "jar"},
		{"app-1.0-sources.jar", "1.0", "sources", "jar"},
		{"app-1.0-linux.x86_64.zip", "1.0", "linux.x86_64", "zip"},
		{"app-1.0-bin.tar.gz", "1.0", "bin", "tar.gz"},
		{"app-1.0.tar.gz", "1.0", "", "tar.gz"},
		{"app-1.0-bin.cpio.xz", "1.0", "bin", "cpio.xz"},
		{"app-1.0.7z", "1.0", "", "7z"},
		{"app-1.0.jar.asc", "1.0", "", "jar.asc"},
		{"app-1.0-linux.x86_64.zip.asc", "1.0", "linux.x86_64", "zip.asc"},
		{"app-1.0.jar.sha1", "1.0", "", "jar.sha1"},
		{"app-1.0.asc", "1.0", "", "asc"},
		{"app-1.0-20261014.120000-3-tests.jar", "1.0-20261014.120000-3", "tests", "jar"},
		{"app-1.0.1.jar", "1.0", "", ""}, // version 1.0.1, not a file of 1.0
		{"app-1.0.1", "1.0", "", ""},
		{"app-1.0-.jar", "1.0", "", ""},
		{"app-1.0", "1.0", "", ""},
	} {
		cl, ext, ok := MavenFileParts(tc.file, "app", tc.version)
		if cl != tc.cl || ext != tc.ext || ok != (tc.ext != "") {
			t.Errorf("MavenFileParts(%q, %q) = %q %q %v, want %q %q", tc.file, tc.version, cl, ext, ok, tc.cl, tc.ext)
		}
	}
}
