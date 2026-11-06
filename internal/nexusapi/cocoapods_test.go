package nexusapi_test

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"commandcenter/internal/format/cocoapods"
)

// cocoapods-02, -05: an installed pod is a component whose one asset is its
// archive, at the path the client downloads it from, so the REST downloadUrl
// works; a podspec the client only read is no component.
func TestCocoapodsComponentsAndDownloadURL(t *testing.T) {
	up := newUpstream(t, http.StatusNotFound, map[string]string{})
	archive := up.URL + "/dl/one-1.0.zip"
	up.put("/dl/one-1.0.zip", "zip-bytes")
	up.put("/cdn/"+cocoapods.SpecPath("One", "1.0"), fmt.Sprintf(`{"name":"One","version":"1.0","source":{"http":%q}}`, archive))
	up.put("/cdn/"+cocoapods.SpecPath("One", "0.9"), fmt.Sprintf(`{"name":"One","version":"0.9","source":{"http":%q}}`, archive))
	up.put("/cdn/all_pods.txt", "One\n")
	e := startRepos(t, fmt.Sprintf(`
  - { name: pods, format: cocoapods, type: proxy, remote: %s/cdn/ }
`, up.URL))
	sum := sha1.Sum([]byte(archive))
	file := "pods/One/1.0/" + hex.EncodeToString(sum[:]) + ".zip"
	e.want("GET", "/repository/pods/all_pods.txt", "", "", 200)
	e.want("GET", "/repository/pods/"+cocoapods.SpecPath("One", "0.9"), "", "", 200)
	e.want("GET", "/repository/pods/"+cocoapods.SpecPath("One", "1.0"), "", "", 200)
	eq(t, "before the archive", e.items(v1+"/components?repository=pods", "admin", "version"))
	e.want("GET", "/repository/pods/"+file, "", "", 200)

	eq(t, "components", e.items(v1+"/components?repository=pods", "admin", "version"), "1.0")
	eq(t, "search", e.items(v1+"/search/assets?repository=pods&name=One", "admin", "path"), "/"+file)
	urls := e.items(v1+"/search/assets?repository=pods&name=One", "admin", "downloadUrl")
	if len(urls) != 1 {
		t.Fatalf("downloadUrl: %v", urls)
	}
	u, err := url.Parse(urls[0])
	if err != nil || u.Path != "/repository/pods/"+file {
		t.Fatalf("downloadUrl %q", urls[0])
	}
	if b := e.want("GET", u.Path, "", "", 200); string(b) != "zip-bytes" {
		t.Fatalf("download: %q", b)
	}
	// The rest of the cache is listed as files without a component.
	if ids := assetIDs(e, "pods"); len(ids) != 4 || ids["/"+file] == "" || ids["/"+cocoapods.SpecPath("One", "0.9")] == "" || ids["/all_pods.txt"] == "" {
		t.Fatalf("assets: %v", ids)
	}
}
