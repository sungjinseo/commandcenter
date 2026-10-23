package golang

import (
	"testing"

	"commandcenter/internal/repo"
)

func TestParseRoute(t *testing.T) {
	cases := []struct {
		in   string
		want Route
	}{
		{"rsc.io/quote/@v/list", Route{Kind: KindList, Module: "rsc.io/quote"}},
		{"rsc.io/quote/@v/v1.5.2.info", Route{Kind: KindInfo, Module: "rsc.io/quote", Version: "v1.5.2"}},
		{"rsc.io/quote/@v/v1.5.2.mod", Route{Kind: KindMod, Module: "rsc.io/quote", Version: "v1.5.2"}},
		{"github.com/!azure/azure-sdk/@v/v1.0.0-!r!c1.zip", Route{Kind: KindZip, Module: "github.com/!azure/azure-sdk", Version: "v1.0.0-!r!c1"}},
		{"rsc.io/quote/@latest", Route{Kind: KindLatest, Module: "rsc.io/quote"}},
		{"gopkg.in/yaml.v3/@v/v3.0.1+incompatible.mod", Route{Kind: KindMod, Module: "gopkg.in/yaml.v3", Version: "v3.0.1+incompatible"}},
		{"rsc.io/quote/@v/master.info", Route{Kind: KindInfo, Module: "rsc.io/quote", Version: "master", Query: true}},
	}
	for _, c := range cases {
		got, err := ParseRoute(c.in)
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v err=%v", c.in, got, err)
		}
	}
	for _, in := range []string{"sumdb/sum.golang.org/supported", "rsc.io/quote", "rsc.io/quote/@v/v1.0.0.txt", "rsc.io/quote/@v/"} {
		if _, err := ParseRoute(in); repo.StatusOf(err) != 404 {
			t.Errorf("%q: %v, want 404", in, err)
		}
	}
	for _, in := range []string{"Rsc.io/quote/@v/list", "rsc.io/../x/@v/list", "rsc.io//x/@v/list", "rsc.io/quote/@v/1.0.0.mod", "rsc.io/quote/@v/master.zip", "rsc.io/quote/@v/master.mod", "rsc.io/quote/@v/v1.0.mod", "a!/b/@v/list"} {
		if _, err := ParseRoute(in); repo.StatusOf(err) != 400 {
			t.Errorf("%q: %v, want 400", in, err)
		}
	}
}

func TestEscape(t *testing.T) {
	if got := Escape("github.com/Azure/SDK"); got != "github.com/!azure/!s!d!k" {
		t.Fatalf("Escape = %q", got)
	}
	if got, err := Unescape("github.com/!azure/!s!d!k"); err != nil || got != "github.com/Azure/SDK" {
		t.Fatalf("Unescape = %q %v", got, err)
	}
	for _, bad := range []string{"a!", "a!B", "a!1"} {
		if _, err := Unescape(bad); err == nil {
			t.Errorf("Unescape(%q) must fail", bad)
		}
	}
}
