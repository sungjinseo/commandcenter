package component_test

// The component package keeps private copies of golang.Unescape and
// pypi.Normalize because the format packages import it. This external test
// package may import both: export_test.go exposes the copies, and they are
// compared with the originals directly, error cases included.

import (
	"testing"

	"commandcenter/internal/component"
	"commandcenter/internal/format/golang"
	"commandcenter/internal/format/pypi"
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
