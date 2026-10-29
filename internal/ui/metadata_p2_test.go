package ui

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/repo"
)

// TestComponentMetadataP2: a cached plugin's OSGi manifest gives the
// bundle name, vendor, license and Require-Bundle; a feature is found
// with nothing collected; a missing one is 404.
func TestComponentMetadataP2(t *testing.T) {
	e := newEnvWith(t, "http://repo.example", func(c *config.Config) {
		c.Repositories = append(c.Repositories, config.Repository{Name: "p2-proxy", Format: "p2", Type: "proxy", Remote: "https://download.eclipse.org/releases/2024-09/"})
	})
	ctx := context.Background()
	bs, err := blob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: bs}, DB: e.db, Now: time.Now}
	e.ui.Store = store
	jar := func(files map[string]string) string {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for name, body := range files {
			w, _ := zw.Create(name)
			w.Write([]byte(body))
		}
		zw.Close()
		return buf.String()
	}
	manifest := "Manifest-Version: 1.0\r\nBundle-SymbolicName: org.example.a;singleton:=true\r\nBundle-Version: 1.0.0.v20260101\r\nBundle-Name: Example\r\n Plugin\r\nBundle-Vendor: Example Inc.\r\nBundle-License: EPL-2.0\r\nRequire-Bundle: org.eclipse.core.runtime;bundle-version=\"[3.0.0,4.0.0)\",org.eclipse.ui;resolution:=optional\r\n\r\n"
	for p, body := range map[string]string{
		"plugins/org.example.a_1.0.0.v20260101.jar": jar(map[string]string{"META-INF/MANIFEST.MF": manifest}),
		"features/org.example.f_1.0.0.jar":          jar(map[string]string{"feature.xml": "<feature/>"}),
	} {
		if _, err := store.Put(ctx, "p2-proxy", p, "", strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	r := e.req(t, "GET", "/-/api/repositories/p2-proxy/components/metadata?group=plugins&name=org.example.a&version=1.0.0.v20260101", "", "admin", nil, false)
	var m componentMetadata
	if r.status != 200 || json.Unmarshal([]byte(r.body), &m) != nil || !m.Collected || *m.Description != "ExamplePlugin" || *m.Maintainer != "Example Inc." || *m.License != "EPL-2.0" || m.Homepage != nil {
		t.Fatalf("p2: %d %s", r.status, r.body)
	}
	var parts []string
	for _, d := range m.Dependencies {
		parts = append(parts, d.Scope+":"+d.Name+":"+d.Requirement)
	}
	if got := strings.Join(parts, " "); got != "require-bundle:org.eclipse.core.runtime:[3.0.0,4.0.0) require-bundle:org.eclipse.ui:" {
		t.Fatalf("p2 deps: %s", got)
	}
	r = e.req(t, "GET", "/-/api/repositories/p2-proxy/components/metadata?group=features&name=org.example.f&version=1.0.0", "", "admin", nil, false)
	if r.status != 200 || json.Unmarshal([]byte(r.body), &m) != nil || m.Collected {
		t.Fatalf("feature: %d %s", r.status, r.body)
	}
	for _, q := range []string{"group=plugins&name=org.example.a&version=2.0.0", "group=binary&name=org.example.a&version=1.0.0.v20260101", "group=other&name=org.example.a&version=1.0.0.v20260101", "name=org.example.a&version=1.0.0.v20260101"} {
		if r := e.req(t, "GET", "/-/api/repositories/p2-proxy/components/metadata?"+q, "", "admin", nil, false); r.status != 404 {
			t.Fatalf("%s: %d", q, r.status)
		}
	}
}
