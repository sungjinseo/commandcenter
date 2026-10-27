package pypi

import (
	"testing"

	"commandcenter/internal/repo"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{"Django": "django", "python_dateutil": "python-dateutil", "zope.interface": "zope-interface", "A__B-.c": "a-b-c"}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
	if !ValidName("Django") || !ValidName("a") || ValidName("-bad") || ValidName("bad-") || ValidName("a b") || ValidName("") {
		t.Fatal("ValidName")
	}
}

func TestParseRoute(t *testing.T) {
	cases := []struct {
		in   string
		want Route
	}{
		{"", Route{Kind: KindUpload}},
		{"simple/", Route{Kind: KindRoot}},
		{"simple", Route{Kind: KindRoot}},
		{"simple/Python_DateUtil/", Route{Kind: KindProject, Name: "python-dateutil"}},
		{"simple/six", Route{Kind: KindProject, Name: "six"}},
		{"packages/six/six-1.16.0-py2.py3-none-any.whl", Route{Kind: KindFile, Name: "six", File: "six-1.16.0-py2.py3-none-any.whl"}},
		{"packages/six/1.16.0/six-1.16.0.tar.gz", Route{Kind: KindFile, Name: "six", File: "six-1.16.0.tar.gz"}},
		{"packages/six/six-1.16.0.tar.gz.metadata", Route{Kind: KindMetadata, Name: "six", File: "six-1.16.0.tar.gz"}},
		{"pypi/Six/json", Route{Kind: KindJSON, Name: "six"}},
		{"pypi/six/1.16.0/json", Route{Kind: KindJSON, Name: "six", Version: "1.16.0"}},
	}
	for _, c := range cases {
		got, err := ParseRoute(c.in)
		if err != nil || got != c.want {
			t.Errorf("%q: got %+v err=%v", c.in, got, err)
		}
	}
	for _, in := range []string{"pypi/six", "pypi/six/1.0/x", "pypi/six/1.0/2.0/json", "simple/a/b/", "packages/six", "other"} {
		if _, err := ParseRoute(in); repo.StatusOf(err) != 404 {
			t.Errorf("%q: %v, want 404", in, err)
		}
	}
	for _, in := range []string{"simple/-bad/", "packages/six/..", "packages/six/.hidden", "packages/a b/x.whl", "packages/six/a\\b.whl", "pypi/-bad/json", "pypi/six/.hidden/json", "packages/six/.metadata"} {
		if _, err := ParseRoute(in); repo.StatusOf(err) != 400 {
			t.Errorf("%q: %v, want 400", in, err)
		}
	}
}

func TestFileMatches(t *testing.T) {
	if !fileMatches("python-dateutil", "python_dateutil-2.8.2-py2.py3-none-any.whl") ||
		!fileMatches("python-dateutil", "python-dateutil-2.8.2.tar.gz") ||
		!fileMatches("smoke-hello", "smoke_hello-1.0.0.tar.gz") ||
		fileMatches("six", "sixteen-1.0.tar.gz") || fileMatches("six", "six.tar.gz") {
		t.Fatal("fileMatches")
	}
}
