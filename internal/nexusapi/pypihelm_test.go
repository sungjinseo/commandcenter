package nexusapi_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// A PyPI release as Nexus's REST API shows it: its core metadata files are no
// assets, so a release of one wheel is one asset that search/assets/download
// finds; an asset has name and version; a project is found by any spelling of
// its name and by its metadata; one file can be deleted.
func TestPyPIComponentModel(t *testing.T) {
	e := startUploads(t)
	whl := zipOf("demo_pkg-1.0.0.dist-info/METADATA", "Metadata-Version: 2.1\nName: demo-pkg\nVersion: 1.0.0\nSummary: A demo for searching\nKeywords: alpha,beta\nClassifier: Topic :: Testing\n\nThe long description.\n")
	sdist := tgzOf("demo-pkg-1.1/PKG-INFO", "Metadata-Version: 2.1\nName: demo-pkg\nVersion: 1.1\nSummary: Something else\n")
	const wheel = "demo_pkg-1.0.0-py3-none-any.whl"
	e.wantUpload(204, "ci", "pypi-hosted", fil("pypi.asset", wheel, whl))
	e.wantUpload(204, "ci", "pypi-hosted", fil("pypi.asset", "demo-pkg-1.1.tar.gz", sdist))
	e.want("PUT", "/repository/raw-files/demo-pkg", "ci", "another format", 201)

	var comps page[comp]
	e.get(v1+"/search?repository=pypi-hosted&name=demo-pkg&version=1.0.0", "ci", &comps)
	if len(comps.Items) != 1 || len(comps.Items[0].Assets) != 1 || comps.Items[0].Assets[0].Path != "/packages/demo-pkg/"+wheel {
		t.Fatalf("a release of one wheel: %+v", comps.Items)
	}
	st, _, hdr := e.do("GET", v1+"/search/assets/download?repository=pypi-hosted&name=demo-pkg&version=1.0.0", "ci", "")
	if st != 302 || !strings.HasSuffix(hdr.Get("Location"), "/repository/pypi-hosted/packages/demo-pkg/"+wheel) {
		t.Fatalf("search/assets/download: %d %q", st, hdr.Get("Location"))
	}
	eq(t, "the assets of the search", e.items(v1+"/search/assets?repository=pypi-hosted&name=demo-pkg&version=1.0.0", "ci", "path"), "/packages/demo-pkg/"+wheel)

	var assets page[map[string]any]
	e.get(v1+"/search/assets?repository=pypi-hosted&name=demo-pkg&version=1.0.0", "ci", &assets)
	if b, _ := json.Marshal(assets.Items[0]["pypi"]); string(b) != `{"name":"demo-pkg","version":"1.0.0"}` {
		t.Fatalf("pypi attributes: %s", b)
	}

	for _, name := range []string{"demo-pkg", "demo_pkg", "demo.pkg", "DEMO_PKG", "demo_*"} {
		if got := e.items(v1+"/search?repository=pypi-hosted&version=1.0.0&name="+name, "ci", "name"); len(got) != 1 || got[0] != "demo-pkg" {
			t.Errorf("name=%s: %q", name, got)
		}
	}
	for _, c := range []struct {
		query string
		want  []string
	}{
		{"pypi.summary=searching", []string{"1.0.0"}},
		{"pypi.summary=SOMETHING", []string{"1.1"}},
		{"pypi.summary=nomatch", nil},
		{"pypi.keywords=alpha", []string{"1.0.0"}},
		{"pypi.classifiers=Topic", []string{"1.0.0"}},
		{"pypi.description=long+description", []string{"1.0.0"}},
		{"pypi.summary=demo&pypi.keywords=nomatch", nil},
		{"pypi.summary=", []string{"1.0.0", "1.1"}}, // given empty: ignored, as a component parameter is
	} {
		got := e.items(v1+"/search?sort=version&direction=asc&format=pypi&"+c.query, "ci", "version")
		eq(t, c.query, got, c.want...)
	}
	// a format-qualified parameter implies the format
	eq(t, "implied format", e.items(v1+"/search?name=demo-pkg&pypi.summary=demo", "ci", "format"), "pypi")

	// the core metadata stays in the assets list, which pull replication reads, and is not deleted alone
	e.get(v1+"/assets?repository=pypi-hosted", "ci", &assets)
	var ids = map[string]string{}
	for _, a := range assets.Items {
		ids[a["path"].(string)] = a["id"].(string)
	}
	if len(ids) != 4 {
		t.Fatalf("assets: %v", ids)
	}
	e.want("DELETE", v1+"/assets/"+ids["/packages/demo-pkg/"+wheel+".metadata"], "ci", "", 400)
	e.want("DELETE", v1+"/assets/"+ids["/packages/demo-pkg/"+wheel], "viewer", "", 403)
	e.want("DELETE", v1+"/assets/"+ids["/packages/demo-pkg/"+wheel], "ci", "", 204)
	page2 := e.body("/repository/pypi-hosted/simple/demo-pkg/")
	if strings.Contains(page2, wheel) || !strings.Contains(page2, "demo-pkg-1.1.tar.gz") {
		t.Fatalf("project page: %s", page2)
	}
	eq(t, "assets after the delete", e.items(v1+"/assets?repository=pypi-hosted", "ci", "path"), "/packages/demo-pkg/demo-pkg-1.1.tar.gz", "/packages/demo-pkg/demo-pkg-1.1.tar.gz.metadata")
}

// A chart's provenance file through the component upload API, as an asset of
// the chart's component. DELETE of the chart's asset takes both; of the
// provenance file, it alone; index.yaml is generated.
func TestHelmProvenanceAssets(t *testing.T) {
	e := startUploads(t)
	chart := tgzOf("demo/Chart.yaml", "apiVersion: v2\nname: demo\nversion: v1.2.3\n")
	const file = "demo-v1.2.3.tgz"
	digest := sha256.Sum256(chart)
	prov := "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n\nname: demo\n\n...\nfiles:\n  " + file + ": sha256:" + hex.EncodeToString(digest[:]) + "\n-----BEGIN PGP SIGNATURE-----\n\nabcd\n-----END PGP SIGNATURE-----\n"
	e.wantInvalid("*", "upload the chart before", "ci", "helm-hosted", fil("helm.asset", file+".prov", []byte(prov)))
	e.wantUpload(204, "ci", "helm-hosted", fil("helm.asset", file, chart))
	e.wantUpload(204, "ci", "helm-hosted", fil("helm.asset", file+".prov", []byte(prov)))
	eq(t, "the component's assets", e.items(v1+"/search/assets?repository=helm-hosted&name=demo&version=v1.2.3", "ci", "path"), "/"+file, "/"+file+".prov")

	var assets page[map[string]any]
	id := func(path string) string {
		t.Helper()
		e.get(v1+"/assets?repository=helm-hosted", "ci", &assets)
		for _, a := range assets.Items {
			if a["path"] == path {
				return a["id"].(string)
			}
		}
		t.Fatalf("no asset %s", path)
		return ""
	}
	e.want("DELETE", v1+"/assets/"+id("/index.yaml"), "ci", "", 400)
	e.want("DELETE", v1+"/assets/"+id("/"+file+".prov"), "ci", "", 204)
	if b := e.body("/repository/helm-hosted/index.yaml"); !strings.Contains(b, file) {
		t.Fatalf("the chart of a deleted provenance file left the index: %s", b)
	}
	e.wantUpload(204, "ci", "helm-hosted", fil("helm.asset", file+".prov", []byte(prov)))
	e.want("DELETE", v1+"/assets/"+id("/"+file), "ci", "", 204)
	eq(t, "assets after the chart", e.items(v1+"/assets?repository=helm-hosted", "ci", "path"), "/index.yaml")
	if b := e.body("/repository/helm-hosted/index.yaml"); strings.Contains(b, file) {
		t.Fatalf("a deleted chart in the index: %s", b)
	}
}
