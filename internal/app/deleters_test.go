package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/format/cargo"
	"commandcenter/internal/format/maven"
	"commandcenter/internal/meta"
)

func TestDeletersWired(t *testing.T) {
	a, err := New(testConfig(t, `  - { name: cargo-hosted, format: cargo, type: hosted }
  - { name: npm-proxy, format: npm, type: proxy, remote: "https://registry.example.org" }
`), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	put, _ := http.NewRequest("PUT", ts.URL+"/repository/maven-releases/org/example/app/1.0/app-1.0.jar", strings.NewReader("jar"))
	put.SetBasicAuth("ci", "pw")
	if resp, err := http.DefaultClient.Do(put); err != nil || resp.StatusCode != 201 {
		t.Fatalf("PUT: %v %v", resp, err)
	}

	d, err := a.Deleters.For(a.Registry.Get("maven-releases"))
	if _, ok := d.(*maven.Handler); err != nil || !ok {
		t.Fatalf("maven hosted deleter: %T %v", d, err)
	}
	if d, err := a.Deleters.For(a.Registry.Get("npm-proxy")); err != nil {
		t.Fatalf("npm proxy deleter: %T %v", d, err)
	} else if _, ok := d.(*cleanup.ProxyDeleter); !ok {
		t.Fatalf("npm proxy deleter: %T", d)
	}
	// Cargo hosted versions can be deleted (the UI's delete; cleanup skips
	// them in Evaluate): a missing one is no error.
	if d, err := a.Deleters.For(a.Registry.Get("cargo-hosted")); err != nil {
		t.Fatalf("cargo hosted deleter: %T %v", d, err)
	} else if _, ok := d.(*cargo.Handler); !ok {
		t.Fatalf("cargo hosted deleter: %T", d)
	}
	c := component.Component{Format: "cargo", Name: "serde", Version: "1.0.0"}
	if res, err := a.Deleters.Delete(context.Background(), a.Registry.Get("cargo-hosted"), c, "system"); err != nil || res.Assets != 0 {
		t.Fatalf("cargo hosted: %+v %v", res, err)
	}
	if _, err := a.Deleters.For(a.Registry.Get("maven-public")); !errors.Is(err, cleanup.ErrNotSupported) {
		t.Fatalf("group: %v", err)
	}

	c = component.Component{Format: "maven", Group: "org.example", Name: "app", Version: "1.0"}
	if res, err := a.Deleters.Delete(context.Background(), a.Registry.Get("maven-releases"), c, "system"); err != nil || res != (cleanup.Result{Assets: 1, Bytes: 3}) {
		t.Fatalf("maven delete: %+v %v", res, err)
	}
	resp, err := http.Get(ts.URL + "/repository/maven-releases/org/example/app/1.0/app-1.0.jar")
	if err != nil || resp.StatusCode != 404 {
		t.Fatalf("GET after delete: %v %v", resp, err)
	}
}

// TestComponentDeletePermissions: every format's delete needs the delete
// privilege, end to end: npm unpublish and Go DELETE on the repository
// routes, PyPI and Cargo (which have no client delete) on the API. Add alone
// is refused with 403 (QA H-2, N-10, M-11).
func TestComponentDeletePermissions(t *testing.T) {
	a, err := New(testConfig(t, `  - { name: npm-hosted, format: npm, type: hosted }
  - { name: go-hosted, format: go, type: hosted }
  - { name: pypi-hosted, format: pypi, type: hosted }
  - { name: cargo-hosted, format: cargo, type: hosted }
`), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	for user, actions := range map[string][]string{
		"adder":   {meta.ActionBrowse, meta.ActionRead, meta.ActionAdd},
		"deleter": {meta.ActionBrowse, meta.ActionRead, meta.ActionAdd, meta.ActionDelete},
	} {
		role := meta.Role{ID: "role-" + user, Privileges: []meta.Privilege{{Target: "*", Actions: actions}}}
		if err := a.DB.CreateRole(ctx, role); err != nil {
			t.Fatal(err)
		}
		if err := a.DB.CreateUser(ctx, meta.User{ID: user, PasswordHash: string(hash), Roles: []string{role.ID}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Auth.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	do := func(method, path, user, body string) int {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if user != "" {
			req.SetBasicAuth(user, "pw")
		}
		req.Header.Set("X-Command-Center-UI", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, v := range []string{"1.0.0", "1.1.0"} {
		pub := `{"name":"pkg","versions":{"` + v + `":{"name":"pkg","version":"` + v + `"}},"_attachments":{"pkg-` + v + `.tgz":{"data":"` + npmTarball("pkg", v) + `"}}}`
		if code := do("PUT", "/repository/npm-hosted/pkg", "adder", pub); code != 201 {
			t.Fatalf("npm publish %s: %d", v, code)
		}
	}
	if _, err := a.Store.Put(ctx, "pypi-hosted", "packages/demo/demo-1.0.tar.gz", "", strings.NewReader("sdist")); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.Tx(ctx, func(tx *meta.Tx) error {
		if err := tx.InsertPackage(ctx, meta.Package{Repo: "pypi-hosted", Format: "pypi", Name: "demo", Version: "demo-1.0.tar.gz", Attrs: "{}"}); err != nil {
			return err
		}
		return tx.InsertPackage(ctx, meta.Package{Repo: "cargo-hosted", Format: "cargo", Name: "demo", Version: "1.0.0", Attrs: "{}"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.Put(ctx, "cargo-hosted", "crates/demo/1.0.0.crate", "", strings.NewReader("crate")); err != nil {
		t.Fatal(err)
	}

	api := "/-/api/repositories/"
	for _, c := range []struct{ method, path, body string }{
		{"DELETE", "/repository/npm-hosted/pkg/-/pkg-1.1.0.tgz/-rev/undefined", ""},
		{"PUT", "/repository/npm-hosted/pkg/-rev/undefined", `{"name":"pkg","versions":{}}`},
		{"DELETE", "/repository/npm-hosted/pkg/-rev/undefined", ""},
		{"DELETE", "/repository/go-hosted/example.com/m/@v/v1.0.0.zip", ""},
		{"DELETE", api + "pypi-hosted/components?name=demo&version=1.0", ""},
		{"DELETE", api + "cargo-hosted/components?name=demo&version=1.0.0", ""},
	} {
		if code := do(c.method, c.path, "", c.body); code != 401 {
			t.Errorf("anonymous %s %s: %d", c.method, c.path, code)
		}
		if code := do(c.method, c.path, "adder", c.body); code != 403 {
			t.Errorf("adder %s %s: %d", c.method, c.path, code)
		}
	}
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"DELETE", "/repository/npm-hosted/pkg/-/pkg-1.1.0.tgz/-rev/undefined", 200},
		{"DELETE", "/repository/npm-hosted/pkg/-rev/undefined", 200},
		{"DELETE", "/repository/go-hosted/example.com/m/@v/v1.0.0.zip", 404}, // reached the handler: nothing there
		{"DELETE", api + "pypi-hosted/components?name=demo&version=1.0", 200},
		{"DELETE", api + "cargo-hosted/components?name=demo&version=1.0.0", 200},
	} {
		if code := do(c.method, c.path, "deleter", ""); code != c.want {
			t.Errorf("deleter %s %s: %d, want %d", c.method, c.path, code, c.want)
		}
	}
	if code := do("GET", "/repository/npm-hosted/pkg", "deleter", ""); code != 404 {
		t.Fatalf("npm package after unpublish: %d", code)
	}
	for repoName, p := range map[string]string{"pypi-hosted": "packages/demo/demo-1.0.tar.gz", "cargo-hosted": "crates/demo/1.0.0.crate"} {
		if _, err := a.DB.GetAsset(ctx, repoName, p); err == nil {
			t.Errorf("%s: %s left", repoName, p)
		}
	}
}

// npmTarball is a minimal npm pack tarball, base64-encoded, which hosted npm
// publish checks against the publish document (QA L-15).
func npmTarball(name, version string) string {
	pj := []byte(`{"name":"` + name + `","version":"` + version + `"}`)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "package/package.json", Mode: 0o644, Size: int64(len(pj)), Typeflag: tar.TypeReg})
	tw.Write(pj)
	tw.Close()
	zw.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
