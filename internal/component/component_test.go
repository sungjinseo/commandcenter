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
		{"com/x/a/1.0-SNAPSHOT/a-1.0-20261019.1-1.jar", "com.x", "a", "1.0-SNAPSHOT", true},
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
		{"pypi", "packages/demo-pkg/demo_pkg-1.2.0.txt", "", "", "", false},
		{"pypi", "packages/demo-pkg/demo_pkg-1.2.0.tar.gz.sha256", "", "", "", false},
		{"pypi", "simple/demo-pkg/", "", "", "", false},
		{"pypi", "packages/demo-pkg", "", "", "", false},
		{"cargo", "crates/serde/1.0.0.crate", "", "serde", "1.0.0", true},
		{"cargo", "crates/serde/1.0.0", "", "", "", false},
		{"cargo", "index/se/rd/serde", "", "", "", false},
		{"go", "example.com/m/@v/v1.0.0.zip", "", "example.com/m", "v1.0.0", true},
		{"maven", "org/example/app/1.0/app-1.0.jar", "org.example", "app", "1.0", true},
		{"unknown", "a/b", "", "", "", false},
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
