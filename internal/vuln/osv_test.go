package vuln

import (
	"encoding/json"
	"slices"
	"testing"

	"commandcenter/internal/config"
)

// The base scores of the examples in FIRST's "CVSS v3.1 Examples".
func TestCVSS3BaseScore(t *testing.T) {
	for _, c := range []struct {
		cve, vector string
		want        float64
	}{
		{"CVE-2013-1937", "CVSS:3.1/AV:N/AC:L/PR:N/UI:R/S:C/C:L/I:L/A:N", 6.1},
		{"CVE-2013-0375", "CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:C/C:L/I:L/A:N", 6.4},
		{"CVE-2014-3566", "CVSS:3.1/AV:N/AC:H/PR:N/UI:R/S:U/C:L/I:N/A:N", 3.1},
		{"CVE-2012-1516", "CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:C/C:H/I:H/A:H", 9.9},
		{"CVE-2009-0783", "CVSS:3.1/AV:L/AC:L/PR:H/UI:N/S:U/C:L/I:L/A:L", 4.2},
		{"CVE-2012-0384", "CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:H", 8.8},
		{"CVE-2015-1098", "CVSS:3.1/AV:L/AC:L/PR:N/UI:R/S:U/C:H/I:H/A:H", 7.8},
		{"CVE-2014-0160", "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:N/A:N", 7.5},
		{"CVE-2014-6271", "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 9.8},
		{"CVE-2008-1447", "CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:C/C:N/I:H/A:N", 6.8},
		{"CVE-2014-2005", "CVSS:3.1/AV:P/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 6.8},
		{"CVE-2010-0467", "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:C/C:L/I:N/A:N", 5.8},
		{"CVE-2012-1342", "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:C/C:N/I:L/A:N", 5.8},
		{"CVE-2013-6014", "CVSS:3.1/AV:A/AC:L/PR:N/UI:N/S:C/C:H/I:N/A:H", 9.3},
		{"CVE-2014-9253", "CVSS:3.1/AV:N/AC:L/PR:L/UI:R/S:C/C:L/I:L/A:N", 5.4},
		{"CVE-2016-1645", "CVSS:3.1/AV:N/AC:L/PR:N/UI:R/S:U/C:H/I:H/A:H", 8.8},
		{"CVE-2016-0128", "CVSS:3.1/AV:N/AC:H/PR:N/UI:R/S:U/C:H/I:H/A:N", 6.8},
		{"CVE-2014-0224", "CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:H/I:H/A:N", 7.4},
		{"CVE-2012-5376", "CVSS:3.1/AV:N/AC:L/PR:N/UI:R/S:C/C:H/I:H/A:H", 9.6},
		{"CVE-2016-5729", "CVSS:3.1/AV:L/AC:L/PR:H/UI:N/S:C/C:H/I:H/A:H", 8.2},
		{"no impact", "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:N", 0},
		{"maximum", "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:C/C:H/I:H/A:H", 10},
		{"3.0 vector, temporal metrics", "CVSS:3.0/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H/E:P/RL:O", 9.8},
	} {
		got, ok := CVSS3(c.vector)
		if !ok || got != c.want {
			t.Errorf("%s %s = %v %v, want %v", c.cve, c.vector, got, ok, c.want)
		}
	}
	for _, bad := range []string{"", "CVSS:2.0/AV:N", "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H", "CVSS:3.1/AV:X/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
		"CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:H/VI:H/VA:H/SC:N/SI:N/SA:N"} {
		if _, ok := CVSS3(bad); ok {
			t.Errorf("CVSS3(%q) accepted", bad)
		}
	}
}

func entry(t *testing.T, js string) Entry {
	t.Helper()
	var e Entry
	if err := json.Unmarshal([]byte(js), &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSeverityAndRows(t *testing.T) {
	for _, c := range []struct {
		name, js string
		label    string
		score10  int
		vector   string
		mal      bool
	}{
		{"cvss3", `{"id":"GHSA-1","severity":[{"type":"CVSS_V3","score":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}]}`,
			"critical", 98, "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", false},
		{"highest of two", `{"id":"GHSA-2","severity":[{"type":"CVSS_V3","score":"CVSS:3.1/AV:N/AC:H/PR:N/UI:R/S:U/C:L/I:N/A:N"}],
			"affected":[{"package":{"ecosystem":"npm","name":"a"},"severity":[{"type":"CVSS_V3","score":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:N/A:N"}]}]}`,
			"high", 75, "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:N/A:N", false},
		{"cvss4 with a given score", `{"id":"GHSA-3","severity":[{"type":"CVSS_V4","score":"CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:H/VI:H/VA:H/SC:N/SI:N/SA:N"}],
			"database_specific":{"cvss_score":9.3}}`, "critical", 93, "CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:H/VI:H/VA:H/SC:N/SI:N/SA:N", false},
		{"cvss4 and a label", `{"id":"GHSA-4","severity":[{"type":"CVSS_V4","score":"CVSS:4.0/AV:N"}],"database_specific":{"severity":"MODERATE"}}`,
			"medium", -1, "CVSS:4.0/AV:N", false},
		{"label on the affected entry", `{"id":"PYSEC-1","affected":[{"package":{"ecosystem":"PyPI","name":"x"},"ecosystem_specific":{"severity":"LOW"}}]}`,
			"low", -1, "", false},
		{"malware", `{"id":"MAL-2024-1","summary":"Malicious code in evil (npm)"}`, "unknown", -1, "", true},
		{"malware alias", `{"id":"GHSA-5","aliases":["MAL-2024-2"]}`, "unknown", -1, "", true},
	} {
		e := entry(t, c.js)
		label, score, vector := e.severity()
		if label != c.label || score != c.score10 || vector != c.vector || e.Malicious() != c.mal {
			t.Errorf("%s: %s %d %q %v, want %s %d %q %v", c.name, label, score, vector, e.Malicious(), c.label, c.score10, c.vector, c.mal)
		}
	}

	e := entry(t, `{"id":"PYSEC-2","modified":"2024-01-02T03:04:05Z","details":"First line.\nMore.",
		"affected":[{"package":{"ecosystem":"PyPI","name":"Django_Rest"},"ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"2.0"}]}]},
		{"package":{"ecosystem":"PyPI","name":"django-rest"},"versions":["3.0"]},
		{"package":{"ecosystem":"Debian:12","name":"python-django"}},
		{"package":{"ecosystem":"Maven","name":"org.x:y"},"versions":["1"]}]}`)
	rows := e.Rows()
	if len(rows) != 2 || rows[0].Ecosystem != "PyPI" || rows[0].Package != "django-rest" || rows[1].Package != "org.x:y" ||
		rows[0].Summary != "First line." || rows[0].Modified.Year() != 2024 {
		t.Fatalf("rows: %+v", rows)
	}
	if hit, _, _ := Affects("PyPI", "3.0", rows[0].Affected); !hit { // both entries of the package are kept
		t.Fatalf("second affected entry lost: %s", rows[0].Affected)
	}
}

func TestAffects(t *testing.T) {
	semver := func(events string) string {
		return `[{"ranges":[{"type":"SEMVER","events":` + events + `}]}]`
	}
	eco := func(events string) string {
		return `[{"ranges":[{"type":"ECOSYSTEM","events":` + events + `}]}]`
	}
	for _, c := range []struct {
		name, ecosystem, affected, version string
		hit                                bool
		fixedIn                            []string
	}{
		{"before fix", "npm", semver(`[{"introduced":"0"},{"fixed":"1.2.3"}]`), "1.2.2", true, []string{"1.2.3"}},
		{"at fix", "npm", semver(`[{"introduced":"0"},{"fixed":"1.2.3"}]`), "1.2.3", false, nil},
		{"after fix", "npm", semver(`[{"introduced":"0"},{"fixed":"1.2.3"}]`), "2.0.0", false, nil},
		{"prerelease of the fix", "npm", semver(`[{"introduced":"0"},{"fixed":"1.2.3"}]`), "1.2.3-rc.1", true, []string{"1.2.3"}},
		{"before introduced", "npm", semver(`[{"introduced":"1.0.0"},{"fixed":"1.5.0"}]`), "0.9.0", false, nil},
		{"two intervals, between", "npm", semver(`[{"introduced":"1.0.0"},{"fixed":"1.5.0"},{"introduced":"2.0.0"},{"fixed":"2.5.0"}]`), "1.7.0", false, nil},
		{"two intervals, second", "npm", semver(`[{"introduced":"1.0.0"},{"fixed":"1.5.0"},{"introduced":"2.0.0"},{"fixed":"2.5.0"}]`), "2.1.0", true, []string{"2.5.0"}},
		{"events out of order", "crates.io", semver(`[{"fixed":"2.5.0"},{"introduced":"2.0.0"},{"fixed":"1.5.0"},{"introduced":"1.0.0"}]`), "1.2.0", true, []string{"1.5.0", "2.5.0"}},
		{"last_affected, at", "npm", semver(`[{"introduced":"0"},{"last_affected":"1.4.0"}]`), "1.4.0", true, nil},
		{"last_affected, above", "npm", semver(`[{"introduced":"0"},{"last_affected":"1.4.0"}]`), "1.4.1", false, nil},
		{"open range", "npm", semver(`[{"introduced":"3.0.0"}]`), "9.9.9", true, nil},
		{"limit ignored", "npm", semver(`[{"introduced":"0"},{"limit":"1.0.0"}]`), "2.0.0", true, nil},
		{"explicit versions", "npm", `[{"versions":["0.9.1","0.9.3"]}]`, "0.9.1", true, nil},
		{"explicit versions, other", "npm", `[{"versions":["0.9.1","0.9.3"]}]`, "0.9.2", false, nil},
		{"git ranges are not evaluated", "Go", `[{"ranges":[{"type":"GIT","events":[{"introduced":"0"}]}]}]`, "1.0.0", false, nil},
		{"go", "Go", semver(`[{"introduced":"0"},{"fixed":"0.17.0"}]`), "0.0.0-20220101000000-abcdefabcdef", true, []string{"0.17.0"}},
		{"pypi pre-release fix", "PyPI", eco(`[{"introduced":"0"},{"fixed":"2.0rc1"}]`), "2.0b1", true, []string{"2.0rc1"}},
		{"pypi at the fix", "PyPI", eco(`[{"introduced":"0"},{"fixed":"2.0rc1"}]`), "2.0rc1", false, nil},
		{"pypi post release", "PyPI", eco(`[{"introduced":"1.9"},{"fixed":"2.0"}]`), "1.9.post1", true, []string{"2.0"}},
		{"pypi spelling", "PyPI", `[{"versions":["1.0"]}]`, "1.0.0", true, nil},
		{"maven", "Maven", eco(`[{"introduced":"2.0-beta9"},{"fixed":"2.15.0"}]`), "2.14.1", true, []string{"2.15.0"}},
		{"maven before introduced", "Maven", eco(`[{"introduced":"2.0-beta9"},{"fixed":"2.15.0"}]`), "2.0-alpha1", false, nil},
		{"maven numeric order", "Maven", eco(`[{"introduced":"0"},{"fixed":"2.9.10.8"}]`), "2.9.10.7", true, []string{"2.9.10.8"}},
		{"nuget", "NuGet", eco(`[{"introduced":"0"},{"fixed":"13.0.1"}]`), "12.0.3", true, []string{"13.0.1"}},
		{"nuget four parts", "NuGet", eco(`[{"introduced":"0"},{"fixed":"4.5.4"}]`), "4.5.4.0", false, nil},
		{"rubygems", "RubyGems", eco(`[{"introduced":"0"},{"fixed":"6.1.7.1"}]`), "6.1.7", true, []string{"6.1.7.1"}},
		{"rubygems pre-release", "RubyGems", eco(`[{"introduced":"7.0.0.alpha"},{"fixed":"7.0.4.1"}]`), "7.0.0.rc1", true, []string{"7.0.4.1"}},
		{"packagist", "Packagist", eco(`[{"introduced":"5.0.0"},{"fixed":"5.4.31"}]`), "5.4.30.0", true, []string{"5.4.31"}},
		{"packagist v prefix", "Packagist", eco(`[{"introduced":"5.0.0"},{"fixed":"5.4.31"}]`), "v5.4.31", false, nil},
		{"cran", "CRAN", eco(`[{"introduced":"0"},{"fixed":"1.2-10"}]`), "1.2-9", true, []string{"1.2-10"}},
		{"conan", "ConanCenter", eco(`[{"introduced":"1.2.0"},{"fixed":"1.2.13"}]`), "1.2.11", true, []string{"1.2.13"}},
	} {
		hit, fixed, err := Affects(c.ecosystem, c.version, c.affected)
		if err != nil || hit != c.hit || !slices.Equal(fixed, c.fixedIn) {
			t.Errorf("%s: Affects(%s, %s) = %v %v %v, want %v %v", c.name, c.ecosystem, c.version, hit, fixed, err, c.hit, c.fixedIn)
		}
	}
}

func TestFromComponent(t *testing.T) {
	for _, c := range []struct {
		format, group, name, version string
		want                         Package
	}{
		{"maven", "org.apache.logging.log4j", "log4j-core", "2.14.1", Package{"Maven", "org.apache.logging.log4j:log4j-core", "2.14.1"}},
		{"npm", "@babel", "core", "7.0.0", Package{"npm", "@babel/core", "7.0.0"}},
		{"npm", "", "lodash", "4.17.20", Package{"npm", "lodash", "4.17.20"}},
		{"go", "", "golang.org/x/net", "v0.7.0", Package{"Go", "golang.org/x/net", "0.7.0"}},
		{"rubygems", "", "nokogiri", "1.13.0-java", Package{"RubyGems", "nokogiri", "1.13.0"}},
		{"conan", "_/_", "zlib", "1.2.11#abc", Package{"ConanCenter", "zlib", "1.2.11"}},
		{"r", "src/contrib", "jsonlite", "1.8.0", Package{"CRAN", "jsonlite", "1.8.0"}},
	} {
		got, ok := FromComponent(c.format, c.group, c.name, c.version)
		if !ok || got != c.want {
			t.Errorf("FromComponent(%s, %s, %s, %s) = %+v %v", c.format, c.group, c.name, c.version, got, ok)
		}
	}
	for _, f := range []string{"docker", "conda", "apt", "yum", "helm", "raw", "gitlfs"} {
		if _, ok := FromComponent(f, "", "x", "1"); ok {
			t.Errorf("%s is covered", f)
		}
	}
}

// config.FirewallFormats must be the formats with data.
func TestFirewallFormats(t *testing.T) {
	for f := range Ecosystems {
		if !config.FirewallFormats[f] {
			t.Errorf("config.FirewallFormats lacks %s", f)
		}
	}
	if len(config.FirewallFormats) != len(Ecosystems) {
		t.Errorf("config.FirewallFormats has formats without data: %v", config.FirewallFormats)
	}
}

func TestBlocking(t *testing.T) {
	no := false
	res := Result{Vulnerabilities: []Vulnerability{
		{ID: "MAL-1", Severity: "unknown", Malicious: true},
		{ID: "GHSA-c", Severity: "critical"}, {ID: "GHSA-m", Severity: "medium"}, {ID: "GHSA-u", Severity: "unknown"},
	}}
	ids := func(vs []Vulnerability) (out []string) {
		for _, v := range vs {
			out = append(out, v.ID)
		}
		return out
	}
	for _, c := range []struct {
		fw   config.Firewall
		want []string
	}{
		{config.Firewall{Enabled: true}, []string{"MAL-1"}},
		{config.Firewall{Enabled: true, BlockSeverityAtLeast: "none"}, []string{"MAL-1"}},
		{config.Firewall{Enabled: true, BlockSeverityAtLeast: "high"}, []string{"MAL-1", "GHSA-c"}},
		{config.Firewall{Enabled: true, BlockSeverityAtLeast: "low"}, []string{"MAL-1", "GHSA-c", "GHSA-m"}},
		{config.Firewall{Enabled: true, BlockMalicious: &no, BlockSeverityAtLeast: "critical"}, []string{"GHSA-c"}},
		{config.Firewall{Enabled: true, BlockMalicious: &no}, nil},
	} {
		if got := ids(Blocking(&c.fw, res)); !slices.Equal(got, c.want) {
			t.Errorf("%+v: %v, want %v", c.fw, got, c.want)
		}
	}
	if !allowListed([]string{`^left-pad@1\.0\.0$`, `(`}, "left-pad@1.0.0") || allowListed([]string{`^left-pad@1\.0\.0$`}, "left-pad@1.0.1") {
		t.Fatal("allow list")
	}
	if e := AllowEntry("@scope/a.b", "1.0.0+x"); e != `^@scope/a\.b@1\.0\.0\+x$` || !allowListed([]string{e}, "@scope/a.b@1.0.0+x") {
		t.Fatalf("AllowEntry: %s", e)
	}
}
