package cargo

import (
	"strings"
	"testing"

	"commandcenter/internal/repo"
)

func TestIndexPath(t *testing.T) {
	cases := map[string]string{"a": "1/a", "ab": "2/ab", "abc": "3/a/abc", "serde": "se/rd/serde", "Serde_JSON": "se/rd/serde_json"}
	for in, want := range cases {
		if got := IndexPath(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

func TestParseRoute(t *testing.T) {
	cases := []struct {
		in   string
		want Route
	}{
		{"config.json", Route{Kind: KindConfig}},
		{"1/a", Route{Kind: KindIndex, Name: "a"}},
		{"3/s/syn", Route{Kind: KindIndex, Name: "syn"}},
		{"se/rd/serde", Route{Kind: KindIndex, Name: "serde"}},
		{"crates/serde/1.0.200/download", Route{Kind: KindDownload, Name: "serde", Version: "1.0.200"}},
		{"crates/Serde/1.0.0-rc.1+build.5/download", Route{Kind: KindDownload, Name: "serde", Version: "1.0.0-rc.1+build.5"}},
		{"api/v1/crates/new", Route{Kind: KindPublish}},
		{"api/v1/crates/serde/1.0.0/yank", Route{Kind: KindYank, Name: "serde", Version: "1.0.0"}},
		{"api/v1/crates/serde/1.0.0/unyank", Route{Kind: KindUnyank, Name: "serde", Version: "1.0.0"}},
	}
	for _, c := range cases {
		got, err := ParseRoute(c.in)
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v err=%v", c.in, got, err)
		}
	}
	for _, in := range []string{"se/rd/syn", "2/abc", "1/ab", "api/v1/crates", "api/v1/me", "crates/serde/1.0/x", "index.json"} {
		if _, err := ParseRoute(in); repo.StatusOf(err) != 404 {
			t.Errorf("%q: %v, want 404", in, err)
		}
	}
	for _, in := range []string{"crates/serde/1.0/download", "crates/9bad/1.0.0/download", "api/v1/crates/serde/x/yank", "1/_"} {
		if _, err := ParseRoute(in); repo.StatusOf(err) != 400 {
			t.Errorf("%q: %v, want 400", in, err)
		}
	}
}

func TestParseRouteCrateFilePath(t *testing.T) {
	got, err := ParseRoute("crates/qa-crate/0.1.0.crate")
	if want := (Route{Kind: KindDownload, Name: "qa-crate", Version: "0.1.0"}); err != nil || got != want {
		t.Fatalf("got %+v err=%v", got, err)
	}
	got, err = ParseRoute("crates/Qa-Crate/1.0.0-rc.1+b.5.crate")
	if want := (Route{Kind: KindDownload, Name: "qa-crate", Version: "1.0.0-rc.1+b.5"}); err != nil || got != want {
		t.Fatalf("prerelease: %+v err=%v", got, err)
	}
	for in, want := range map[string]Route{
		"2/qa":           {Kind: KindIndex, Name: "qa"},
		"3/q/qaz":        {Kind: KindIndex, Name: "qaz"},
		"qa/-c/qa-crate": {Kind: KindIndex, Name: "qa-crate"},
	} {
		if got, err := ParseRoute(in); err != nil || got != want {
			t.Errorf("%s: %+v %v", in, got, err)
		}
	}
	for _, in := range []string{"crates/qa-crate/0.1.crate", "crates/qa-crate/.crate", "crates/9bad/0.1.0.crate"} {
		if _, err := ParseRoute(in); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}

func TestLegacyVersionStillServed(t *testing.T) {
	// A stored version that strict semver.Valid rejects (leading zero in a
	// numeric prerelease) must stay downloadable and indexed; only new
	// publishes are checked strictly.
	e := newEnv(t)
	e.seedHosted(t, "legacy", "1.0.0-01", "old crate")
	if w := e.do("GET", "cargo-hosted", "le/ga/legacy", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"1.0.0-01"`) {
		t.Fatalf("index: %d %s", w.Code, w.Body.String())
	}
	for _, p := range []string{"crates/legacy/1.0.0-01/download", "crates/legacy/1.0.0-01.crate"} {
		if w := e.do("GET", "cargo-hosted", p, nil); w.Code != 200 || w.Body.String() != "old crate" {
			t.Fatalf("%s: %d %q", p, w.Code, w.Body.String())
		}
	}
}
