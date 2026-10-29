package npm

import (
	"strings"
	"testing"

	"commandcenter/internal/repo"
)

func TestParseRoute(t *testing.T) {
	cases := []struct {
		in   string
		want Route
	}{
		{"lodash", Route{Kind: KindPackument, Package: "lodash"}},
		{"@types/node", Route{Kind: KindPackument, Package: "@types/node"}},
		{"lodash/4.17.21", Route{Kind: KindVersion, Package: "lodash", Version: "4.17.21"}},
		{"@types/node/latest", Route{Kind: KindVersion, Package: "@types/node", Version: "latest"}},
		{"lodash/-/lodash-4.17.21.tgz", Route{Kind: KindTarball, Package: "lodash", File: "lodash-4.17.21.tgz"}},
		{"@types/node/-/node-20.0.0.tgz", Route{Kind: KindTarball, Package: "@types/node", File: "node-20.0.0.tgz"}},
		{"-/package/lodash/dist-tags", Route{Kind: KindDistTags, Package: "lodash"}},
		{"-/package/@s/p/dist-tags/beta", Route{Kind: KindDistTag, Package: "@s/p", Tag: "beta"}},
		{"-/user/org.couchdb.user:ci", Route{Kind: KindLogin, User: "ci"}},
		{"-/v1/login", Route{Kind: KindWebLogin}},
		{"-/whoami", Route{Kind: KindWhoami}},
		{"-/ping", Route{Kind: KindPing}},
		{"-/npm/v1/security/advisories/bulk", Route{Kind: KindAudit, Path: "-/npm/v1/security/advisories/bulk"}},
		{"-/npm/v1/security/audits/quick", Route{Kind: KindAudit, Path: "-/npm/v1/security/audits/quick"}},
		{"-/package/foo/dist-tags/dist-tags", Route{Kind: KindDistTag, Package: "foo", Tag: "dist-tags"}},
		{"-/package/@s/p/dist-tags", Route{Kind: KindDistTags, Package: "@s/p"}},
		{"@s/p/1.0.0", Route{Kind: KindVersion, Package: "@s/p", Version: "1.0.0"}},
		{"pkg/-rev/undefined", Route{Kind: KindRev, Package: "pkg", Rev: "undefined"}},
		{"@s/p/-rev/3-abc", Route{Kind: KindRev, Package: "@s/p", Rev: "3-abc"}},
		{"pkg/-/pkg-1.0.0.tgz/-rev/x", Route{Kind: KindRev, Package: "pkg", File: "pkg-1.0.0.tgz", Rev: "x"}},
		{"@s/p/-/p-1.0.0.tgz/-rev/x", Route{Kind: KindRev, Package: "@s/p", File: "p-1.0.0.tgz", Rev: "x"}},
		{"@acme/_internal/-/_internal-1.0.0.tgz", Route{Kind: KindTarball, Package: "@acme/_internal", File: "_internal-1.0.0.tgz"}},
		{"-/user/token/cc_abc", Route{Kind: KindToken, Token: "cc_abc"}},
	}
	for _, c := range cases {
		got, err := ParseRoute(c.in)
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v err=%v, want %+v", c.in, got, err, c.want)
		}
	}
}

func TestParseRouteErrors(t *testing.T) {
	notFound := []string{"-/v1/search", "-/user/token/", "-/user/token/a/b", "lodash/-/x/y", "lodash/a/b", "lodash/-/readme.md", "-/package/lodash", "@scope"}
	for _, in := range notFound {
		if _, err := ParseRoute(in); repo.StatusOf(err) != 404 {
			t.Errorf("%s: err = %v, want 404", in, err)
		}
	}
	for _, in := range []string{"", ".hidden", "_private", "a b", "@/x", "@s/.x", "@s/.", "@s/..", "@./x"} {
		if _, err := ParseRoute(in); repo.StatusOf(err) != 400 {
			t.Errorf("%q: err = %v, want 400", in, err)
		}
	}
}

func TestParseRouteRejectsBadSegments(t *testing.T) {
	bad := []string{
		"lodash/..",
		"lodash/-/a\\b.tgz",
		"-/package/foo/dist-tags/..",
		"-/user/org.couchdb.user:a\x01",
	}
	for _, in := range bad {
		if _, err := ParseRoute(in); repo.StatusOf(err) != 400 {
			t.Errorf("%q: err = %v, want 400", in, err)
		}
	}
}

func TestNamesAndKeys(t *testing.T) {
	if TarballFile("@s/pkg", "1.0.0") != "pkg-1.0.0.tgz" || TarballFile("pkg", "2.0.0-beta.1") != "pkg-2.0.0-beta.1.tgz" {
		t.Fatal("TarballFile")
	}
	if PackumentKey("@s/pkg") != "@s/pkg/package.json" || TarballKey("@s/pkg", "pkg-1.0.0.tgz") != "@s/pkg/-/pkg-1.0.0.tgz" {
		t.Fatal("keys")
	}
	if !ValidName("Legacy-Upper") || ValidName("a/b") || ValidName(strings.Repeat("a", 215)) {
		t.Fatal("ValidName")
	}
	if !ValidName(strings.Repeat("a", 214)) {
		t.Fatal("214-character name must be valid")
	}
	// npm refuses a leading "." or "_" on the whole name only.
	for _, name := range []string{"@acme/_internal", "@_acme/x", "@s/x.y", "@s/x_"} {
		if !ValidName(name) {
			t.Errorf("%q must be valid", name)
		}
	}
	for _, name := range []string{"_x", ".x", "a..b", "@s/.x", "@.s/x", "@s/a..b", "@s/.", "@s/..", "@./x", "@../x", "@s/", "@s/a/b"} {
		if ValidName(name) {
			t.Errorf("%q must be invalid", name)
		}
	}
}
