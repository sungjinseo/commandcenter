package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

type compResp struct {
	Components []struct {
		Repo, Format, Group, Name, Version, LastUpdated string
		Assets                                          []struct {
			Path string
			Size int64
		}
	}
	Truncated bool
}

func searchComponents(t *testing.T, e *env, query, user string, want int) compResp {
	t.Helper()
	r := e.get(t, "/-/api/search/components?"+query, user)
	wantStatus(t, r, want)
	checkAPIHeaders(t, r)
	var out compResp
	if want == 200 {
		if strings.Contains(r.body, `"components":null`) {
			t.Fatalf("components is null: %s", r.body)
		}
		if err := json.Unmarshal([]byte(r.body), &out); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func putAsset(t *testing.T, db *meta.DB, repo, path string, size int64) {
	t.Helper()
	err := db.PutAsset(context.Background(), meta.Asset{Repo: repo, Path: path, BlobSHA256: "sha256-" + repo + path,
		Size: size, CreatedAt: t0, LastModified: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
}

func names(c compResp) string {
	var parts []string
	for _, x := range c.Components {
		parts = append(parts, x.Repo+":"+x.Group+":"+x.Name+":"+x.Version)
	}
	return strings.Join(parts, ",")
}

func TestComponentsMavenSearch(t *testing.T) {
	e := newEnv(t, "http://x")
	c := searchComponents(t, e, "group=org.example", "admin", 200)
	if got, want := names(c), "maven-releases:org.example:app:1.0,maven-releases:org.example:pad-util:1.0"; got != want {
		t.Fatalf("components = %s, want %s", got, want)
	}
	app := c.Components[0]
	if app.Format != "maven" || app.LastUpdated != "2026-10-01T13:00:00Z" || len(app.Assets) != 2 {
		t.Fatalf("app = %+v", app)
	}
	if app.Assets[0].Path != "org/example/app/1.0/app-1.0.jar" || app.Assets[0].Size != 3 || app.Assets[1].Size != 5 {
		t.Fatalf("assets = %+v", app.Assets)
	}
	if c.Truncated {
		t.Fatal("truncated")
	}
	// name substring, case-insensitive
	c = searchComponents(t, e, "name=PAD&format=maven", "admin", 200)
	if got, want := names(c), "maven-releases:org.example:pad-util:1.0"; got != want {
		t.Fatalf("name search = %s, want %s", got, want)
	}
	// keyword matches group, name or version
	c = searchComponents(t, e, "keyword=org.exam", "admin", 200)
	if len(c.Components) != 2 {
		t.Fatalf("keyword group: %s", names(c))
	}
	c = searchComponents(t, e, "keyword=pad-util", "admin", 200)
	if len(c.Components) != 1 {
		t.Fatalf("keyword name: %s", names(c))
	}
	// no match: empty array, never null
	c = searchComponents(t, e, "name=nomatchatall", "admin", 200)
	if len(c.Components) != 0 || c.Truncated {
		t.Fatalf("no match: %+v", c)
	}
}

func TestComponentsVersionExactAndGlob(t *testing.T) {
	e := newEnv(t, "http://x")
	for _, v := range []string{"1.0.1", "1.1", "2.0"} {
		putAsset(t, e.db, "maven-releases", "org/example/app/"+v+"/app-"+v+".jar", 1)
	}
	c := searchComponents(t, e, "name=app&version=1.0", "admin", 200)
	if got, want := names(c), "maven-releases:org.example:app:1.0"; got != want {
		t.Fatalf("exact = %s, want %s", got, want)
	}
	c = searchComponents(t, e, "name=app&version="+url.QueryEscape("1.*"), "admin", 200)
	if got, want := names(c), "maven-releases:org.example:app:1.0,maven-releases:org.example:app:1.0.1,maven-releases:org.example:app:1.1"; got != want {
		t.Fatalf("glob = %s, want %s", got, want)
	}
	// version alone is a valid criterion
	c = searchComponents(t, e, "version=2.0", "admin", 200)
	if got, want := names(c), "maven-releases:org.example:app:2.0"; got != want {
		t.Fatalf("version only = %s, want %s", got, want)
	}
}

func TestComponentsClassifierExtension(t *testing.T) {
	e := newEnv(t, "http://x")
	putAsset(t, e.db, "maven-releases", "org/example/app/1.0/app-1.0-sources.jar", 9)
	c := searchComponents(t, e, "name=app&classifier=sources", "admin", 200)
	if len(c.Components) != 1 || len(c.Components[0].Assets) != 1 || c.Components[0].Assets[0].Size != 9 {
		t.Fatalf("classifier: %+v", c)
	}
	c = searchComponents(t, e, "name=app&extension=pom", "admin", 200)
	if len(c.Components) != 1 || len(c.Components[0].Assets) != 1 || !strings.HasSuffix(c.Components[0].Assets[0].Path, ".pom") {
		t.Fatalf("extension: %+v", c)
	}
}

func TestComponentsNpmAndPypi(t *testing.T) {
	e := newEnv(t, "http://x")
	putAsset(t, e.db, "npm-hosted", "@scope/util/-/util-2.0.0.tgz", 21)
	c := searchComponents(t, e, "group=@scope", "admin", 200)
	if got, want := names(c), "npm-hosted:@scope:util:2.0.0"; got != want {
		t.Fatalf("scope = %s, want %s", got, want)
	}
	if a := c.Components[0].Assets; len(a) != 1 || a[0].Path != "@scope/util/-/util-2.0.0.tgz" || a[0].Size != 21 {
		t.Fatalf("scope assets = %+v", a)
	}
	c = searchComponents(t, e, "keyword=left", "admin", 200)
	if len(c.Components) != 2 {
		t.Fatalf("left-pad: %s", names(c))
	}
	for _, x := range c.Components {
		if x.Version == "1.0.0" && (len(x.Assets) != 1 || x.Assets[0].Size != 17) {
			t.Fatalf("left-pad 1.0.0 assets = %+v", x.Assets)
		}
		if x.Version == "1.1.0" && x.Assets == nil {
			t.Fatal("assets must be [] not null")
		}
	}
	c = searchComponents(t, e, "name=my-pkg", "admin", 200)
	if got, want := names(c), "pypi-hosted::my-pkg:1.0"; got != want {
		t.Fatalf("pypi = %s, want %s", got, want)
	}
	putAsset(t, e.db, "pypi-hosted", "packages/my-pkg/my_pkg-1.0.tar.gz", 31)
	c = searchComponents(t, e, "name=my-pkg", "admin", 200)
	if a := c.Components[0].Assets; len(a) != 1 || a[0].Path != "packages/my-pkg/my_pkg-1.0.tar.gz" || a[0].Size != 31 {
		t.Fatalf("pypi assets = %+v", a)
	}
}

func TestComponentsFormatRepoFilters(t *testing.T) {
	e := newEnv(t, "http://x")
	putAsset(t, e.db, "maven-proxy", "org/example/app/3.0/app-3.0.jar", 1)
	c := searchComponents(t, e, "name=app&format=maven", "admin", 200)
	if len(c.Components) != 2 {
		t.Fatalf("format maven: %s", names(c))
	}
	c = searchComponents(t, e, "name=app&format=npm", "admin", 200)
	if len(c.Components) != 0 {
		t.Fatalf("format npm: %s", names(c))
	}
	c = searchComponents(t, e, "name=app&repo=maven-proxy", "admin", 200)
	if got, want := names(c), "maven-proxy:org.example:app:3.0"; got != want {
		t.Fatalf("repo = %s, want %s", got, want)
	}
	searchComponents(t, e, "name=app&repo=nosuch", "admin", 404)
}

func TestComponentsHiddenAndGroupRepos(t *testing.T) {
	e := newEnv(t, "http://x")
	putAsset(t, e.db, "maven-public", "org/example/grp/1.0/grp-1.0.jar", 1) // group repos are skipped
	c := searchComponents(t, e, "group=org.example", "admin", 200)
	if strings.Contains(names(c), "grp") {
		t.Fatalf("group repo searched: %s", names(c))
	}
	// anonymous may only read npm-hosted: maven repos are hidden.
	c = searchComponents(t, e, "group=org.example", "", 200)
	if len(c.Components) != 0 {
		t.Fatalf("hidden repo leaked: %s", names(c))
	}
	searchComponents(t, e, "name=app&repo=maven-releases", "", 404)
	c = searchComponents(t, e, "keyword=left", "", 200)
	if len(c.Components) != 2 {
		t.Fatalf("anonymous npm: %s", names(c))
	}
}

func TestComponentsValidation(t *testing.T) {
	e := newEnv(t, "http://x")
	for _, q := range []string{"", "format=maven", "name=a", "keyword=x&repo=maven-releases", "name=" + strings.Repeat("a", 201)} {
		searchComponents(t, e, q, "admin", 400)
	}
	r := e.req(t, "POST", "/-/api/search/components?name=app", "", "admin", nil, true)
	if r.status != 405 {
		t.Fatalf("POST status %d", r.status)
	}
}

func TestComponentsCaps(t *testing.T) {
	e := newEnv(t, "http://x")
	for i := 0; i < 101; i++ {
		putAsset(t, e.db, "maven-releases", fmt.Sprintf("big/many/lib%03d/1.0/lib%03d-1.0.jar", i, i), 1)
	}
	c := searchComponents(t, e, "group=big.many", "admin", 200)
	if len(c.Components) != 100 || !c.Truncated {
		t.Fatalf("component cap: %d truncated=%v", len(c.Components), c.Truncated)
	}
	for i := 0; i < 51; i++ {
		putAsset(t, e.db, "maven-releases", fmt.Sprintf("fat/jar/one/1.0/one-1.0-c%02d.jar", i), 1)
	}
	c = searchComponents(t, e, "group=fat.jar", "admin", 200)
	if len(c.Components) != 1 || len(c.Components[0].Assets) != 50 || !c.Truncated {
		t.Fatalf("asset cap: %d comps, %d assets, truncated=%v", len(c.Components), len(c.Components[0].Assets), c.Truncated)
	}
}

func TestComponentsGo(t *testing.T) {
	e := newEnvWith(t, "http://x", func(c *config.Config) {
		c.Repositories = append(c.Repositories, config.Repository{Name: "go-hosted", Format: "go", Type: "hosted"})
	})
	for _, p := range []string{"example.com/m/v2/@v/v2.1.0.zip", "example.com/m/v2/@v/v2.1.0.mod", "example.com/m/v2/@v/list"} {
		putAsset(t, e.db, "go-hosted", p, 4)
	}
	c := searchComponents(t, e, "name=example.com", "admin", 200)
	if got, want := names(c), "go-hosted::example.com/m/v2:v2.1.0"; got != want {
		t.Fatalf("go = %s, want %s", got, want)
	}
	if len(c.Components[0].Assets) != 2 {
		t.Fatalf("go assets = %+v", c.Components[0].Assets)
	}
}

func TestComponentsProxyRepos(t *testing.T) {
	e := newEnvWith(t, "http://x", func(c *config.Config) {
		c.Repositories = append(c.Repositories,
			config.Repository{Name: "pypi-proxy", Format: "pypi", Type: "proxy", Remote: "https://pypi.example.org"},
			config.Repository{Name: "cargo-proxy", Format: "cargo", Type: "proxy", Remote: "https://crates.example.org"},
			config.Repository{Name: "npm-proxy", Format: "npm", Type: "proxy", Remote: "https://npm.example.org"})
	})
	putAsset(t, e.db, "pypi-proxy", "packages/demo-pkg/demo_pkg-1.2.0.tar.gz", 5)
	putAsset(t, e.db, "pypi-proxy", "packages/demo-pkg/demo_pkg-1.2.0-py3-none-any.whl", 6)
	putAsset(t, e.db, "cargo-proxy", "crates/serde/1.0.0.crate", 7)
	putAsset(t, e.db, "cargo-proxy", "index/se/rd/serde", 8)
	putAsset(t, e.db, "npm-proxy", "@types/node/-/node-20.1.0.tgz", 9)

	// name given with underscore, paths hold the normalized project
	for _, q := range []string{"name=demo_pkg", "name=Demo.Pkg", "keyword=demo_pkg", "keyword=1.2.0&format=pypi"} {
		c := searchComponents(t, e, q, "admin", 200)
		if got, want := names(c), "pypi-proxy::demo-pkg:1.2.0"; got != want {
			t.Fatalf("%s: %s, want %s", q, got, want)
		}
		if len(c.Components[0].Assets) != 2 {
			t.Fatalf("%s: assets = %+v", q, c.Components[0].Assets)
		}
	}
	c := searchComponents(t, e, "name=serde", "admin", 200)
	if got, want := names(c), "cargo-proxy::serde:1.0.0"; got != want {
		t.Fatalf("cargo = %s, want %s", got, want)
	}
	if a := c.Components[0].Assets; len(a) != 1 || a[0].Size != 7 {
		t.Fatalf("cargo assets = %+v", a)
	}
	c = searchComponents(t, e, "group=@types", "admin", 200)
	if got, want := names(c), "npm-proxy:@types:node:20.1.0"; got != want {
		t.Fatalf("npm proxy = %s, want %s", got, want)
	}
}

func TestComponentsScanCapTruncates(t *testing.T) {
	old := scanLimit
	scanLimit = 3
	t.Cleanup(func() { scanLimit = old })
	e := newEnv(t, "http://x")
	for i := 0; i < 5; i++ {
		putAsset(t, e.db, "maven-releases", fmt.Sprintf("scan/cap/lib%d/1.0/lib%d-1.0.jar", i, i), 1)
	}
	c := searchComponents(t, e, "group=scan.cap", "admin", 200)
	if !c.Truncated || len(c.Components) != 3 {
		t.Fatalf("maven scan cap: %d components truncated=%v", len(c.Components), c.Truncated)
	}
	// package scan: left-pad has 2 versions, @scope/util 1; cap 3 is not exceeded
	// until a fourth version appears.
	err := e.db.Tx(context.Background(), func(tx *meta.Tx) error {
		return tx.InsertPackage(context.Background(), meta.Package{Repo: "npm-hosted", Format: "npm", Name: "left-pad", Version: "1.2.0", Attrs: "{}", CreatedAt: t0})
	})
	if err != nil {
		t.Fatal(err)
	}
	c = searchComponents(t, e, "keyword=.0&format=npm", "admin", 200)
	if !c.Truncated {
		t.Fatalf("package scan cap not truncated: %s", names(c))
	}
}

func TestComponentsTrimsCriteria(t *testing.T) {
	e := newEnv(t, "http://x")
	searchComponents(t, e, "name="+url.QueryEscape(" a "), "admin", 400)
	c := searchComponents(t, e, "name="+url.QueryEscape(" app "), "admin", 200)
	if len(c.Components) != 1 {
		t.Fatalf("trimmed name: %s", names(c))
	}
}

func TestComponentsGoCaseEncoded(t *testing.T) {
	e := newEnvWith(t, "http://x", func(c *config.Config) {
		c.Repositories = append(c.Repositories, config.Repository{Name: "go-hosted", Format: "go", Type: "hosted"})
	})
	putAsset(t, e.db, "go-hosted", "github.com/!burnt!sushi/toml/@v/v1.0.0.zip", 4)
	putAsset(t, e.db, "go-hosted", "github.com/!burnt!sushi/toml/@v/v1.0.0.mod", 4)
	for _, q := range []string{"name=BurntSushi", "keyword=BurntSushi", "name=github.com/BurntSushi/toml", "name=burnt", "name=burntsushi", "keyword=burntsushi", "name=github.com/burntsushi/toml"} {
		c := searchComponents(t, e, q, "admin", 200)
		if got, want := names(c), "go-hosted::github.com/BurntSushi/toml:v1.0.0"; got != want {
			t.Fatalf("%s: got %q, want %q", q, got, want)
		}
		if len(c.Components[0].Assets) != 2 {
			t.Fatalf("%s: assets %+v", q, c.Components[0].Assets)
		}
	}
}

func TestComponentsKeywordRecallPackageRepos(t *testing.T) {
	old := scanLimit
	scanLimit = 100
	t.Cleanup(func() { scanLimit = old })
	e := newEnv(t, "http://x")
	err := e.db.Tx(context.Background(), func(tx *meta.Tx) error {
		for i := 0; i < 300; i++ {
			name := fmt.Sprintf("pkg-%03d", i)
			if i == 299 {
				name = "zz-last"
			}
			for _, v := range []string{"1.0.0", "2.0.0"} {
				if err := tx.InsertPackage(context.Background(), meta.Package{Repo: "npm-hosted", Format: "npm", Name: name, Version: v, Attrs: "{}", CreatedAt: t0}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := searchComponents(t, e, "keyword=zz-last&format=npm", "admin", 200)
	if got, want := names(c), "npm-hosted::zz-last:1.0.0,npm-hosted::zz-last:2.0.0"; got != want {
		t.Fatalf("keyword recall = %q, want %q", got, want)
	}
}

func TestComponentsDeadlineReturnsPartial(t *testing.T) {
	old := componentsTimeout
	componentsTimeout = time.Nanosecond
	t.Cleanup(func() { componentsTimeout = old })
	e := newEnv(t, "http://x")
	putAsset(t, e.db, "maven-releases", "dl/line/app/1.0/app-1.0.jar", 1)
	c := searchComponents(t, e, "group=dl.line", "admin", 200)
	if !c.Truncated {
		t.Fatalf("deadline: truncated=%v components=%s", c.Truncated, names(c))
	}
}

// A hosted PyPI version with a wheel and an sdist is one component with both
// files, whether found by name or by a bare keyword (two-pass scan).
func TestHostedPypiComponentListsAllFiles(t *testing.T) {
	e := newEnv(t, "http://x")
	err := e.db.Tx(context.Background(), func(tx *meta.Tx) error {
		for _, f := range []string{"demo_pkg-1.2.0.tar.gz", "demo_pkg-1.2.0-py3-none-any.whl"} {
			if err := tx.InsertPackage(context.Background(), meta.Package{Repo: "pypi-hosted", Format: "pypi", Name: "demo-pkg", Version: f, Attrs: "{}", CreatedAt: t0}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	putAsset(t, e.db, "pypi-hosted", "packages/demo-pkg/demo_pkg-1.2.0.tar.gz", 5)
	putAsset(t, e.db, "pypi-hosted", "packages/demo-pkg/demo_pkg-1.2.0-py3-none-any.whl", 6)
	for _, q := range []string{"name=demo-pkg", "keyword=demo"} {
		c := searchComponents(t, e, q, "admin", 200)
		if len(c.Components) != 1 || len(c.Components[0].Assets) != 2 {
			t.Fatalf("%s: want one component with 2 assets, got %+v", q, c.Components)
		}
	}
}
