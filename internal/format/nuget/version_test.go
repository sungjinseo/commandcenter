package nuget

import (
	"sort"
	"strings"
	"testing"
)

// TestVersionNormalization: NuGet's normalized form (NuGetVersion
// .ToNormalizedString): leading zeros dropped, three parts at least, a zero
// fourth part dropped, release labels kept as written, build metadata not
// part of the identity.
func TestVersionNormalization(t *testing.T) {
	cases := []struct{ in, want, key string }{
		{"1.0.0", "1.0.0", "1.0.0"},
		{"1.0", "1.0.0", "1.0.0"},
		{"1", "1.0.0", "1.0.0"},
		{"1.0.0.0", "1.0.0", "1.0.0"},
		{"1.0.0.1", "1.0.0.1", "1.0.0.1"},
		{"01.02.003", "1.2.3", "1.2.3"},
		{"1.0.0-beta", "1.0.0-beta", "1.0.0-beta"},
		{"1.0.0-Beta.1", "1.0.0-Beta.1", "1.0.0-beta.1"},
		{"1.0.0-rc.1+build.5", "1.0.0-rc.1", "1.0.0-rc.1"},
		{"1.0.0+sha.abc", "1.0.0", "1.0.0"},
		{"2.1.0.0-preview1", "2.1.0-preview1", "2.1.0-preview1"},
		{" 1.2.3 ", "1.2.3", "1.2.3"},
	}
	for _, c := range cases {
		v, ok := parseVersion(c.in)
		if !ok || v.String() != c.want || v.key() != c.key {
			t.Errorf("%q: %v %q %q, want %q %q", c.in, ok, v.String(), v.key(), c.want, c.key)
		}
	}
	for _, bad := range []string{"", "1.0.0.0.0", "a.b", "1..0", "1.0-", "1.0.0-beta!", "-1.0", "1.0.0+", "4294967296.0.0", "1.0.0 beta"} {
		if _, ok := parseVersion(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestVersionOrder(t *testing.T) {
	in := []string{"1.0.0", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-alpha", "0.9.0", "1.0.0.1", "1.0.1-rc", "1.0.0-Beta.2", "1.0.0-beta", "10.0.0"}
	vs := make([]version, len(in))
	for i, s := range in {
		vs[i], _ = parseVersion(s)
	}
	sort.Slice(vs, func(i, j int) bool { return compareVersions(vs[i], vs[j]) < 0 })
	var got []string
	for _, v := range vs {
		got = append(got, v.String())
	}
	want := "0.9.0,1.0.0-alpha,1.0.0-beta,1.0.0-beta.2,1.0.0-Beta.2,1.0.0-beta.11,1.0.0,1.0.0.1,1.0.1-rc,10.0.0"
	if s := strings.Join(got, ","); s != want {
		t.Fatalf("order:\n got %s\nwant %s", s, want)
	}
	a, _ := parseVersion("1.0.0-beta.2")
	b, _ := parseVersion("1.0.0-BETA.2")
	if compareVersions(a, b) != 0 {
		t.Fatal("release labels compare without case")
	}
}

func TestVersionRange(t *testing.T) {
	for in, want := range map[string]string{"1.0.0": "[1.0.0, )", "1.0": "[1.0.0, )", "": "(, )", "[1.0.0, 2.0.0)": "[1.0.0, 2.0.0)", "(1.0,)": "(1.0,)"} {
		if got := versionRange(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestParseRoute(t *testing.T) {
	cases := []struct {
		path string
		want Route
		ok   bool
	}{
		{"index.json", Route{Kind: KindIndex}, true},
		{"query", Route{Kind: KindSearch}, true},
		{"api/v2/package", Route{Kind: KindPublish}, true},
		{"api/v2/package/Demo.Pkg/1.0.0.0", Route{Kind: KindPublish, ID: "demo.pkg", Version: "1.0.0"}, true},
		{"api/v2/symbolpackage", Route{Kind: KindSymbols}, true},
		{"v3-flatcontainer/Demo.Pkg/index.json", Route{Kind: KindFlatIndex, ID: "demo.pkg"}, true},
		{"v3-flatcontainer/demo.pkg/1.0.0-Beta/demo.pkg.1.0.0-beta.nupkg", Route{Kind: KindFlatFile, ID: "demo.pkg", Version: "1.0.0-beta", File: "demo.pkg.1.0.0-beta.nupkg"}, true},
		{"v3-flatcontainer/demo.pkg/1.0.0/demo.pkg.1.0.0.nuspec", Route{Kind: KindFlatFile, ID: "demo.pkg", Version: "1.0.0", File: "demo.pkg.1.0.0.nuspec"}, true},
		{"registration/Demo.Pkg/index.json", Route{Kind: KindRegistration, ID: "demo.pkg", Rest: "index.json"}, true},
		{"registration/demo.pkg/1.0.0.0.json", Route{Kind: KindRegistration, ID: "demo.pkg", Version: "1.0.0", Rest: "1.0.0.json"}, true},
		{"registration/demo.pkg/page/1.0.0/2.0.0.json", Route{Kind: KindRegistration, ID: "demo.pkg", Rest: "page/1.0.0/2.0.0.json"}, true},
		{"v3-flatcontainer/demo.pkg/1.0.0/other.1.0.0.nupkg", Route{}, false},
		{"v3-flatcontainer/demo.pkg/1.0.0/demo.pkg.1.0.0.dll", Route{}, false},
		{"v3-flatcontainer/../x/index.json", Route{}, false},
		{"registration/demo.pkg/../x.json", Route{}, false},
		{"registration/demo.pkg/index.html", Route{}, false},
		{"api/v2/package/demo/", Route{}, false},
		{"", Route{}, false},
		{"nuget/index.json", Route{}, false},
	}
	for _, c := range cases {
		got, err := ParseRoute(c.path)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("%q: %+v %v, want %+v ok=%v", c.path, got, err, c.want, c.ok)
		}
	}
	if _, err := ParseRoute("api/v2/package/demo/not-a-version!"); err == nil || !strings.Contains(err.Error(), "invalid version") {
		t.Fatalf("bad version: %v", err)
	}
}
