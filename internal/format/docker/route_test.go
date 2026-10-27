package docker

import "testing"

func TestParseRoute(t *testing.T) {
	cases := []struct {
		path string
		want Route
		ok   bool
	}{
		{"", Route{Kind: KindBase}, true},
		{"token", Route{Kind: KindToken}, true},
		{"_catalog", Route{Kind: KindCatalog}, true},
		{"dh/app/manifests/v1", Route{Kind: KindManifest, Repo: "dh", Image: "app", Reference: "v1"}, true},
		{"dh/team/app/manifests/sha256:ab", Route{Kind: KindManifest, Repo: "dh", Image: "team/app", Reference: "sha256:ab"}, true},
		{"dh/app/tags/list", Route{Kind: KindTags, Repo: "dh", Image: "app"}, true},
		{"dh/app/blobs/sha256:ab", Route{Kind: KindBlob, Repo: "dh", Image: "app", Digest: "sha256:ab"}, true},
		{"dh/app/blobs/uploads/", Route{Kind: KindUploads, Repo: "dh", Image: "app"}, true},
		{"dh/app/blobs/uploads/abc", Route{Kind: KindUpload, Repo: "dh", Image: "app", UUID: "abc"}, true},
		{"app/manifests/v1", Route{}, false}, // no image under the repository
		{"dh//manifests/v1", Route{}, false},
		{"dh/app/other", Route{}, false},
		{"dh/app/blobs/uploads", Route{Kind: KindBlob, Repo: "dh", Image: "app", Digest: "uploads"}, true}, // an invalid digest, not a session
	}
	for _, c := range cases {
		got, ok := ParseRoute(c.path)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%q: got %+v %v, want %+v %v", c.path, got, ok, c.want, c.ok)
		}
	}
	for name, ok := range map[string]bool{"dh/app": true, "dh/team/my-app_x.y": true, "dh/App": false, "dh": false, "dh/": false, "dh/a//b": false, "dh/-a": false} {
		if ValidName(name) != ok {
			t.Errorf("ValidName(%q) = %v", name, !ok)
		}
	}
	if s, ok := parseScope("repository:dh/app:pull,push"); !ok || s.Name != "dh/app" || len(s.Actions) != 2 {
		t.Errorf("scope = %+v %v", s, ok)
	}
	if start, end, ok := parseContentRange("bytes 5-9/20"); !ok || start != 5 || end != 9 {
		t.Errorf("content-range = %d %d %v", start, end, ok)
	}
}
