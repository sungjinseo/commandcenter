package component_test

// The component package keeps private copies of golang.Unescape and
// pypi.Normalize because the format packages import it. This external test
// package may import both: export_test.go exposes the copies, and they are
// compared with the originals directly, error cases included.

import (
	"strings"
	"testing"

	"commandcenter/internal/component"
	"commandcenter/internal/format/golang"
	"commandcenter/internal/format/pypi"
	"commandcenter/internal/format/swift"
)

func TestGoUnescapeParity(t *testing.T) {
	for _, s := range []string{
		"", "plain", "v1.0.0", "v1.0.0-!r!c1", "github.com/!burnt!sushi/toml", "!a!b!c",
		"Upper", "v1.0.0-Upper", "bad!", "bad!1", "bad!!a", "bad!A", "!", "a!-b",
	} {
		want, wantErr := golang.Unescape(s)
		got, gotErr := component.GoUnescape(s)
		if got != want || (gotErr == nil) != (wantErr == nil) {
			t.Errorf("GoUnescape(%q) = %q, %v; golang.Unescape = %q, %v", s, got, gotErr, want, wantErr)
		}
	}
}

func TestPyPINormalizeParity(t *testing.T) {
	for _, s := range []string{
		"", "six", "Demo.Pkg", "demo__pkg", "A-_.b", "Zope.Interface", "--x--", "a...b", "MiXeD_Case-1.0", "_",
	} {
		if got, want := component.PyPINormalize(s), pypi.Normalize(s); got != want {
			t.Errorf("PyPINormalize(%q) = %q, pypi.Normalize = %q", s, got, want)
		}
	}
}

func TestDeriveUsesTheCopies(t *testing.T) {
	for _, s := range []string{"v1.0.0", "v1.0.0-!r!c1", "v1.0.0-Upper", "v1.0.0-bad!"} {
		want, err := golang.Unescape(s)
		if err != nil {
			want = s // DeriveGo keeps text that is not validly encoded
		}
		if _, got, ok := component.DeriveGo("m/@v/" + s + ".zip"); !ok || got != want {
			t.Errorf("DeriveGo version of %q = %q %v, want %q", s, got, ok, want)
		}
	}
	for _, spelling := range []string{"Demo.Pkg", "demo__pkg", "Zope.Interface"} {
		if got := component.PyPIDisplayVersion(pypi.Normalize(spelling), spelling+"-1.0.tar.gz"); got != "1.0" {
			t.Errorf("PyPIDisplayVersion for %q = %q, want 1.0", spelling, got)
		}
	}
}

// TestSwiftParity: the component package's copy of the swift version and
// identifier rules agrees with the format's.
func TestSwiftParity(t *testing.T) {
	for _, v := range []string{"1.0.0", "1.0.0-alpha.1", "1.0.0+build.5", "10.20.30-rc.1+x.y", "v1.0.0", "1.0", "01.0.0", "1.0.0-", "1.0.0-01", "1.0.0-0a", "1.0.0+", ""} {
		_, _, ok := component.SplitSwift("mona/linkedlist/" + v + "/source-archive.zip")
		if ok != swift.ValidVersion(v) {
			t.Errorf("SplitSwift version %q = %v, swift.ValidVersion = %v", v, ok, !ok)
		}
	}
	ids := []string{"mona.linkedlist", "a.b", "mo-na.linked-list", "mona.linked_list", "mo--na.x", "-mona.x", "mona.-x", "mona.x-", "mona._x", "mona.x_", "mona.x__y", "mona.x-_y", "mona_x.y", "mona.linked.list", ".x", "x.",
		strings.Repeat("a-", 19) + "a.x", strings.Repeat("a-", 20) + "a.x", "a." + strings.Repeat("b_", 49) + "bb", "a." + strings.Repeat("b_", 50) + "b"}
	for _, id := range ids {
		scope, name, ok := swift.SplitID(id)
		got, _, gotOK := component.SplitSwift(id[:strings.IndexByte(id+".", '.')] + "/" + id[strings.IndexByte(id+".", '.')+1:] + "/1.0.0/source-archive.zip")
		if gotOK != ok || (ok && got != scope+"."+name) {
			t.Errorf("SplitSwift identifier %q = %q %v, swift.SplitID = %q %q %v", id, got, gotOK, scope, name, ok)
		}
	}
}
