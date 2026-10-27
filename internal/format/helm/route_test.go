package helm

import (
	"testing"

	"commandcenter/internal/repo"
)

func TestParseRoute(t *testing.T) {
	cases := map[string]Route{
		"index.yaml":                    {Kind: KindIndex},
		"api/charts":                    {Kind: KindAPI},
		"demo-1.0.0.tgz":                {Kind: KindChart, File: "demo-1.0.0.tgz"},
		"ingress-nginx-4.10.0-rc.1.tgz": {Kind: KindChart, File: "ingress-nginx-4.10.0-rc.1.tgz"},
		"demo-1.0.0+build.tgz":          {Kind: KindChart, File: "demo-1.0.0+build.tgz"},
	}
	for in, want := range cases {
		if got, err := ParseRoute(in); err != nil || got != want {
			t.Errorf("%s: got %+v err=%v", in, got, err)
		}
	}
	for _, in := range []string{"", "charts/demo-1.0.0.tgz", "upstream/index.yaml", "demo-1.0.0.tar.gz", ".tgz", "-x.tgz", "../x.tgz", "api/charts/demo/1.0.0"} {
		if _, err := ParseRoute(in); repo.StatusOf(err) != 404 {
			t.Errorf("%q: %v, want 404", in, err)
		}
	}
	if !sameFile("demo-1.0.0+build.2.tgz", "demo-1.0.0_build.2.tgz") || sameFile("demo-1.0.0.tgz", "demo-1.0.1.tgz") {
		t.Error("sameFile")
	}
}
