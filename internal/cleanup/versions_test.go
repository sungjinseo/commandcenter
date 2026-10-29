package cleanup

import "testing"

// Each list is strictly increasing in its format's order: every pair is
// checked both ways. The lists follow the schemes' published examples
// (Maven's POM reference, PEP 440's ordering example, rpm's rpmvercmp
// tests, Debian Policy and dpkg's tests, conda's VersionOrder docs).
var increasing = map[string][][]string{
	"maven": {
		{"1-alpha2snapshot", "1-alpha2", "1-alpha-123", "1-beta-2", "1-beta123", "1-m2", "1-m11", "1-rc", "1-cr2",
			"1-rc123", "1-SNAPSHOT", "1", "1-sp", "1-sp2", "1-sp123", "1-abc", "1-def", "1-pom-1", "1-1-snapshot", "1-1", "1-2", "1-123"},
		{"1.0-alpha1", "1.0-beta1", "1.0-rc1", "1.0-SNAPSHOT", "1.0", "1.0-sp1", "1.0.1"},
		{"1", "1.1"},
		{"1-snapshot", "1", "1-sp"},
		{"1-foo2", "1-foo10"},
		{"1.foo", "1-1", "1.1"},
		{"1-ga", "1-sp"},
		{"1-ga.1", "1-sp.1"},
		{"1-sp-1", "1-ga-1"},
		{"2.0.9", "2.0.10", "2.1", "10.0"},
	},
	"pypi": {
		{"1.dev0", "1.0.dev456", "1.0a1", "1.0a2.dev456", "1.0a12.dev456", "1.0a12", "1.0b1.dev456", "1.0b2",
			"1.0b2.post345.dev456", "1.0b2.post345", "1.0rc1.dev456", "1.0rc1", "1.0", "1.0+abc.5", "1.0+abc.7", "1.0+5",
			"1.0.post456.dev34", "1.0.post456", "1.0.15", "1.1.dev1"},
		{"1.0.dev1", "1.0a1", "1.0", "1.0.post1", "1!0.1"},
		{"not a version", "0.1"},
	},
	"yum": {
		{"1.0-1", "1.0-1.el9", "1.0-2", "1.0.1-1", "1:0.5-1"},
		{"1.0~rc1-1", "1.0-1", "1.0^git1-1", "1.0.1-1"},
		{"1.0-1.el9.x86_64", "1.0-2.el9.x86_64", "1.1-1.el9.noarch"},
		{"2.0", "2.0.1", "2.0.1a"},
		{"5.5p1", "5.5p2", "5.5p10", "5.6p1", "6.5p1"},
		{"10xyz", "10.1xyz"},
		{"xyz.4", "2", "8"},
		{"6.0", "6.0.rc1"},
		{"10a2", "10b2"},
		{"1.0a", "1.0aa"},
		{"10.0001", "10.0039"},
		{"4.999.9", "5.0"},
		{"1.0~rc1~git123", "1.0~rc1", "1.0~rc1^git1", "1.0~rc2", "1.0"},
		{"1.0", "1.0^", "1.0^git1~pre", "1.0^git1", "1.0^git2", "1.01"},
		{"1.0^20160101", "1.0^20160101^git1", "1.0^20160102", "1.0.1"},
	},
	"apt": {
		{"1.0~~", "1.0~~a", "1.0~", "1.0", "1.0a", "1.0+", "1.0.1"},
		{"1.0~rc1", "1.0"},
		{"1.0-1", "1.0-2", "1.0-10"},
		{"1.2.3-1", "1.2.3-1ubuntu1", "1.2.3-2"},
		{"7.6-0", "7.6p2-4"},
		{"1.0-1", "1.0.3-3"},
		{"1.2.2", "1.2.2-2", "1.3"},
		{"0-pre", "0-pree"},
		{"1.1.6r-1", "1.1.6r2-2"},
		{"2.6b-2", "2.6b2-1"},
		{"98.1p5-1", "98.1-pre2-b6-2"},
		{"0.4-1", "0.4a6-2"},
		{"1:3.0.5-2", "1:3.0.5.1"},
		{"1.0pre7-2", "1.0.4-2"},
		{"9.9", "1:0.1", "2:0.0"},
	},
	"rubygems": {
		{"0.0.0", "1.8.2.a", "1.8.2.a9", "1.8.2.a10", "1.8.2.b", "1.8.2", "1.8.3"},
		{"5.a", "5.0.0.rc2", "5.0.0"},
		{"1.0.0.pre", "1.0.0.pre.1", "1.0.0", "1.0.1"},
		{"1.0.a.2", "1.0.b1"},
		{"1.0.0-java", "1.0.1-java", "1.1.0"},
	},
	"r": {
		{"0.9", "1.0", "1.0.0", "1.0-1", "1.0-2", "1.0.10", "1.1"},
	},
	"composer": {
		{"dev-feature", "dev-main", "1.0.0.0-dev", "1.0.0.0-alpha1", "1.0.0.0-beta1", "1.0.0.0-beta2", "1.0.0.0-RC1",
			"1.0.0.0", "1.0.0.0-patch1", "1.0.1.0", "1.0.9999999.9999999-dev", "1.1.0.0", "10.0.0.0"},
	},
	"conda": {
		{"0.4-0", "0.4.1.rc-0", "0.4.1-0", "0.5a1-0", "0.5b3-0", "0.5c1-0", "0.5-0", "0.9.6-0", "0.960923-0", "1.0-0",
			"1.1dev1-0", "1.1a1-0", "1.1.dev1-0", "1.1.a1-0", "1.1.0rc1-0", "1.1-0", "1.1.post1-0", "1.1post1-0",
			"1996.07.12-0", "1!0.4.1-0", "1!3.1.1.6-0", "2!0.4.1-0"},
		{"1.21.5-py39h7a5d4dd_0", "1.21.5-py39h7a5d4dd_1", "1.21.6-py39h7a5d4dd_0"},
	},
	"npm": {
		{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1",
			"1.0.0", "1.0.1", "1.1.0", "2.0.0", "10.0.0"},
	},
	"go": {
		{"v0.0.0-20200101000000-abcdefabcdef", "v0.0.1", "v1.2.3-pre", "v1.2.3", "v1.2.4-0.20200101000000-abcdefabcdef", "v1.2.4", "v2.0.0+incompatible", "v2.0.1"},
	},
	"nuget": {
		{"1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.10", "1.0.0", "1.0.0.1", "1.0.1", "1.1"},
	},
	"helm":  {{"0.1.0", "0.1.1-rc.1", "0.1.1", "1.0.0"}},
	"cargo": {{"0.9.9", "0.10.0", "1.0.0-rc.1", "1.0.0"}},
	"conan": {{"1.0#bbb", "1.2#aaa", "1.10#ccc", "2.0.0-rc1#aaa", "2.0.0#aaa"}},
}

// Pairs that compare equal.
var equal = map[string][][2]string{
	"maven":    {{"1.0", "1"}, {"1.ga", "1-ga"}, {"1-0", "1.0.0"}, {"1-final", "1"}, {"1-a1", "1-alpha-1"}, {"1-ga-1", "1-1"}, {"1.foo", "1-foo"}, {"1.0-RC1", "1.0-cr1"}, {"1.0.0-SNAPSHOT", "1.0-snapshot"}},
	"pypi":     {{"1.0", "1.0.0"}, {"1.0a1", "1.0.0alpha1"}, {"1.0rc1", "1.0c1"}, {"1.0-1", "1.0.post1"}, {"v1.0", "1.0"}},
	"yum":      {{"1.0-1", "0:1.0-1"}, {"10.0001-1", "10.1-1"}, {"2.0-1", "2_0-1"}, {"1.0-1.el9.x86_64", "1.0-1.el9.aarch64"}},
	"apt":      {{"1.0", "0:1.0"}, {"1.0-0", "1.0"}, {"1.0.01", "1.0.1"}},
	"rubygems": {{"1.0", "1.0.0"}, {"1.0.0.a.1.0", "1.0.0.a.1"}, {"1.0.0", "1.0.0-java"}},
	"r":        {{"1.0-1", "1.0.1"}},
	"conda":    {{"0.4-0", "0.4.0-0"}, {"0.4.1.rc-0", "0.4.1.RC-0"}, {"1.1.0dev1-0", "1.1.dev1-0"}, {"1.1.0-0", "1.1-0"}, {"1.1.0post1-0", "1.1.post1-0"}, {"1.0-py39_0", "1.0-py310_0"}},
	"npm":      {{"1.0.0+build.1", "1.0.0+build.2"}},
	"nuget":    {{"1.0", "1.0.0.0"}, {"1.0.0-BETA", "1.0.0-beta"}},
	"conan":    {{"1.0#aaa", "1.0#bbb"}},
	"docker":   {{"latest", "1.0"}},
	"raw":      {{"", ""}},
}

func TestCompareVersions(t *testing.T) {
	for format, lists := range increasing {
		for _, list := range lists {
			for i := range list {
				for j := range list {
					want := 0
					switch {
					case i < j:
						want = -1
					case i > j:
						want = 1
					}
					if got := CompareVersions(format, list[i], list[j]); got != want {
						t.Errorf("%s: CompareVersions(%q, %q) = %d, want %d", format, list[i], list[j], got, want)
					}
				}
			}
		}
	}
	for format, pairs := range equal {
		for _, p := range pairs {
			if got := CompareVersions(format, p[0], p[1]); got != 0 {
				t.Errorf("%s: CompareVersions(%q, %q) = %d, want 0", format, p[0], p[1], got)
			}
			if got := CompareVersions(format, p[1], p[0]); got != 0 {
				t.Errorf("%s: CompareVersions(%q, %q) = %d, want 0", format, p[1], p[0], got)
			}
		}
	}
	if HasVersionOrder("docker") || HasVersionOrder("raw") || !HasVersionOrder("maven") {
		t.Error("HasVersionOrder")
	}
}
