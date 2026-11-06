package nexusapi_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
)

func p2Jar(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	zw.Close()
	return buf.String()
}

// startP2 runs the app with p2-px, a proxy of a site with a plugin named in its
// manifest, one named through its properties, a feature and a binary.
func startP2(t *testing.T) (*env, *upstream) {
	t.Helper()
	up := newUpstream(t, http.StatusNotFound, map[string]string{
		"/a/p2.index":                "version=1\n",
		"/a/plugins/one_1.0.0.jar":   p2Jar(t, map[string]string{"META-INF/MANIFEST.MF": "Bundle-SymbolicName: one\nBundle-Name: The First\n"}),
		"/a/plugins/two_1.0.0.jar":   p2Jar(t, map[string]string{"META-INF/MANIFEST.MF": "Bundle-Name: %name\nBundle-Localization: plugin\n", "plugin.properties": "# names\nname = The Sec\\\n  ond\n"}),
		"/a/plugins/three_1.0.0.jar": p2Jar(t, map[string]string{"META-INF/MANIFEST.MF": "Bundle-Name: %name\n", "OSGI-INF/l10n/bundle.properties": "name=Third\n"}),
		"/a/features/f_1.0.0.jar":    p2Jar(t, map[string]string{"feature.xml": "<feature id='f' label='%fname' version='1.0.0'/>", "feature.properties": "fname=A Feature\n"}),
		"/a/binary/bin_1.0.0":        "binary",
		"/b/p2.index":                "version=1\nb\n",
		"/b/plugins/one_1.0.0.jar":   "another site's build",
	})
	return startRepos(t, fmt.Sprintf(`
  - { name: p2-px, format: p2, type: proxy, remote: %s/a/ }
`, up.URL)), up
}

// p2-02: changing a p2 proxy's remote deletes what was cached from the old
// one, so the next requests ask the new one, even for a plugin that never
// expires. Another setting and another format's proxy keep their cache.
func TestP2RemoteChangeDropsTheCache(t *testing.T) {
	e, up := startP2(t)
	const repo = "/repository/p2-px/"
	e.want("GET", repo+"p2.index", "", "", 200)
	e.want("GET", repo+"plugins/one_1.0.0.jar", "", "", 200)
	if ids := assetIDs(e, "p2-px"); len(ids) != 2 {
		t.Fatalf("cached: %v", ids)
	}
	body := func(remote string, age int) string {
		return fmt.Sprintf(`{"name":"p2-px","online":true,"storage":{"blobStoreName":"default"},"proxy":{"remoteUrl":%q,"contentMaxAge":-1,"metadataMaxAge":%d},
		  "httpClient":{"blocked":false,"autoBlock":true},"negativeCache":{"enabled":true,"timeToLive":10}}`, remote, age)
	}
	e.want("PUT", v1+"/repositories/p2/proxy/p2-px", "admin", body(up.URL+"/a/", 60), 204)
	if ids := assetIDs(e, "p2-px"); len(ids) != 2 {
		t.Fatalf("another setting dropped the cache: %v", ids)
	}
	e.want("PUT", v1+"/repositories/p2/proxy/p2-px", "admin", body(up.URL+"/b/", 60), 204)
	if ids := assetIDs(e, "p2-px"); len(ids) != 0 {
		t.Fatalf("still cached: %v", ids)
	}
	if b := e.want("GET", repo+"p2.index", "", "", 200); string(b) != "version=1\nb\n" {
		t.Fatalf("p2.index: %q", b)
	}
	if b := e.want("GET", repo+"plugins/one_1.0.0.jar", "", "", 200); string(b) != "another site's build" {
		t.Fatalf("plugin: %q", b)
	}
	e.want("GET", repo+"plugins/two_1.0.0.jar", "", "", 404) // only the old site has it

	raw := startRawProxy(t, e, up.URL)
	e.want("GET", "/repository/"+raw+"/a/p2.index", "", "", 200)
	e.want("PUT", v1+"/repositories/raw/proxy/"+raw, "admin", rawProxyBody(raw, up.URL+"/b/"), 204)
	if ids := assetIDs(e, raw); len(ids) != 1 {
		t.Fatalf("a raw proxy lost its cache: %v", ids)
	}
}

func rawProxyBody(name, remote string) string {
	return fmt.Sprintf(`{"name":%q,"online":true,"storage":{"blobStoreName":"default"},"proxy":{"remoteUrl":%q,"contentMaxAge":-1,"metadataMaxAge":60},
	  "httpClient":{"blocked":false,"autoBlock":true},"negativeCache":{"enabled":true,"timeToLive":10},"raw":{"contentDisposition":"ATTACHMENT"}}`, name, remote)
}

func startRawProxy(t *testing.T, e *env, remote string) string {
	t.Helper()
	e.want("POST", v1+"/repositories/raw/proxy", "admin", rawProxyBody("raw-px", remote+"/"), 201)
	return "raw-px"
}

// p2-05: p2.pluginName finds a bundle by the name in the jar (and implies the
// format); the JSON's group is null as in Nexus, and group= still takes the
// directory a component was found in.
func TestP2PluginNameSearch(t *testing.T) {
	e, _ := startP2(t)
	for _, p := range []string{"plugins/one_1.0.0.jar", "plugins/two_1.0.0.jar", "plugins/three_1.0.0.jar", "features/f_1.0.0.jar", "binary/bin_1.0.0"} {
		e.want("GET", "/repository/p2-px/"+p, "", "", 200)
	}
	for name, want := range map[string]string{`"The First"`: "one", `"The Second"`: "two", "Third": "three", `"A Feature"`: "f", "bin": "bin", "second": "two", "the*first": "one", "%name": ""} {
		got := e.items(v1+"/search?p2.pluginName="+url.QueryEscape(name), "admin", "name")
		if want == "" {
			eq(t, name, got)
			continue
		}
		eq(t, name, got, want)
	}
	eq(t, "alternatives", e.items(v1+"/search?sort=name&p2.pluginName="+url.QueryEscape("first third"), "admin", "name"), "one", "three")
	eq(t, "the attribute's own name", e.items(v1+"/search?attributes.p2.pluginName="+url.QueryEscape("Third"), "admin", "name"), "three")
	eq(t, "no such name", e.items(v1+"/search?p2.pluginName=none", "admin", "name"))
	eq(t, "asset search", e.items(v1+"/search/assets?p2.pluginName="+url.QueryEscape("feature"), "admin", "path"), "/features/f_1.0.0.jar")

	eq(t, "group", e.items(v1+"/search?repository=p2-px&name=one", "admin", "group"), "<nil>")
	eq(t, "group= the directory", e.items(v1+"/search?repository=p2-px&group=features", "admin", "name"), "f")
}
