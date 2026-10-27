package ui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/config"
	"commandcenter/internal/format/cargo"
	"commandcenter/internal/format/npm"
	"commandcenter/internal/format/pypi"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
)

type compResp struct {
	Components []struct {
		Repo, Format, Group, Name, Version, LastUpdated string
		Assets                                          []struct {
			Path string
			Size int64
		}
	}
	Truncated         bool
	ContinuationToken string
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
	for _, q := range []string{"", "format=maven", "name=a", "keyword=x&repo=maven-releases", "name=" + strings.Repeat("a", 201),
		"name=app&continuationToken=!!", "name=app&continuationToken=" + base64.RawURLEncoding.EncodeToString([]byte(`["a","b"]`))} {
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
	if len(c.Components) != 100 || !c.Truncated || c.ContinuationToken == "" {
		t.Fatalf("component cap: %d truncated=%v token=%q", len(c.Components), c.Truncated, c.ContinuationToken)
	}
	for i := 0; i < 51; i++ {
		putAsset(t, e.db, "maven-releases", fmt.Sprintf("fat/jar/one/1.0/one-1.0-c%02d.jar", i), 1)
	}
	c = searchComponents(t, e, "group=fat.jar", "admin", 200)
	if len(c.Components) != 1 || len(c.Components[0].Assets) != 50 || !c.Truncated {
		t.Fatalf("asset cap: %d comps, %d assets, truncated=%v", len(c.Components), len(c.Components[0].Assets), c.Truncated)
	}
}

// QA L-23: continuationToken pages through every component, 100 at a time
// in key order, without overlap or gaps, also when the scan meets them in
// another order (formats are scanned go before maven, but "zz-go" sorts last).
func TestComponentsPaging(t *testing.T) {
	e := newEnvWith(t, "http://x", func(c *config.Config) {
		c.Repositories = append(c.Repositories, config.Repository{Name: "zz-go", Format: "go", Type: "hosted"})
	})
	var want []string
	for i := 0; i < 150; i++ {
		putAsset(t, e.db, "maven-releases", fmt.Sprintf("page/many/lib%03d/1.0/lib%03d-1.0.jar", i, i), 1)
		want = append(want, fmt.Sprintf("maven-releases:page.many:lib%03d:1.0", i))
	}
	for i := 0; i < 60; i++ {
		putAsset(t, e.db, "zz-go", fmt.Sprintf("page/many/m%02d/@v/v1.0.0.zip", i), 1)
		want = append(want, fmt.Sprintf("zz-go::page/many/m%02d:v1.0.0", i))
	}
	var got []string
	tok, pages := "", 0
	for {
		q := "keyword=many"
		if tok != "" {
			q += "&continuationToken=" + url.QueryEscape(tok)
		}
		c := searchComponents(t, e, q, "admin", 200)
		pages++
		if n := names(c); n != "" {
			got = append(got, strings.Split(n, ",")...)
		}
		if c.ContinuationToken == "" {
			if c.Truncated {
				t.Fatalf("last page %d: truncated without a scan limit", pages)
			}
			break
		}
		if len(c.Components) != 100 || !c.Truncated {
			t.Fatalf("page %d: %d components truncated=%v, want a full page", pages, len(c.Components), c.Truncated)
		}
		if pages > 3 {
			t.Fatal("more pages than components")
		}
		tok = c.ContinuationToken
	}
	if pages != 3 || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%d pages:\n got  %v\n want %v", pages, got, want)
	}
}

// QA L-23 review: a page whose last key is not valid UTF-8 (a Maven name
// stored with a raw Latin-1 byte) resumes right after that key. With the key
// in JSON strings the byte became U+FFFD, and the next page skipped "b한",
// which sorts between the two.
func TestComponentsPagingInvalidUTF8(t *testing.T) {
	e := newEnv(t, "http://x")
	type comp struct{ name, ver string }
	var all []comp
	for i := 0; i < 99; i++ {
		all = append(all, comp{fmt.Sprintf("a%03d", i), fmt.Sprintf("1.%d", i)})
	}
	all = append(all,
		comp{"b\xe9", "2.0"},         // 100th in key order: Latin-1 e-acute, not UTF-8
		comp{"b\xed\x95\x9c", "3.0"}, // "b한": valid UTF-8, sorts after "b\xe9" and before "b" + U+FFFD
		comp{"c", "4.0"})
	for _, c := range all {
		putAsset(t, e.db, "maven-releases", "page/utf/"+c.name+"/"+c.ver+"/"+c.name+"-"+c.ver+".jar", 1)
	}
	seen := map[string]int{}
	tok := ""
	for pages := 1; ; pages++ {
		q := "group=page.utf"
		if tok != "" {
			q += "&continuationToken=" + url.QueryEscape(tok)
		}
		c := searchComponents(t, e, q, "admin", 200)
		for _, x := range c.Components {
			seen[x.Version]++
		}
		if c.ContinuationToken == "" || pages > 5 {
			break
		}
		tok = c.ContinuationToken
	}
	for _, c := range all {
		if n := seen[c.ver]; n != 1 {
			t.Errorf("component %q %s listed %d times across the pages, want once", c.name, c.ver, n)
		}
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

// TestDeleteComponentAPI: DELETE /-/api/repositories/{name}/components deletes
// a component version of a hosted repository with the format's deleter, for a
// caller with the delete privilege (npm, PyPI and Cargo had no delete means;
// QA H-2, M-11), and the repository list says where the UI may offer it.
func TestDeleteComponentAPI(t *testing.T) {
	e := newEnvWith(t, "http://repo.example", func(c *config.Config) {
		c.Repositories = append(c.Repositories,
			config.Repository{Name: "cargo-hosted", Format: "cargo", Type: "hosted"},
			config.Repository{Name: "pypi-ro", Format: "pypi", Type: "hosted", WritePolicy: "deny"})
		c.Roles["pydel"] = config.Role{Write: []string{"pypi-hosted", "cargo-hosted"}}
		c.Users = append(c.Users, config.User{Name: "deleter", PasswordHash: c.Users[0].PasswordHash, Roles: []string{"pydel"}})
	})
	ctx := context.Background()
	if r := e.req(t, "DELETE", "/-/api/repositories/pypi-hosted/components?name=my-pkg&version=1.0", "", "admin", nil, true); r.status != 501 {
		t.Fatalf("without deleters: %d %s", r.status, r.body)
	}
	bs, err := blob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: bs}, DB: e.db, Now: time.Now}
	sink := &repotest.Sink{}
	store.Events = sink
	baseURL := func() string { return "http://repo.example" }
	reg := cleanup.NewRegistry(store)
	reg.Register("npm", "hosted", npm.New(store, nil, e.srv.Auth, baseURL))
	reg.Register("pypi", "hosted", pypi.New(store, nil, baseURL))
	reg.Register("cargo", "hosted", cargo.New(store, nil, e.srv.Auth, baseURL))
	e.ui.Deleters = reg
	if _, err := store.Put(ctx, "pypi-hosted", "packages/my-pkg/my_pkg-1.0.tar.gz", "", strings.NewReader("sdist")); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Tx(ctx, func(tx *meta.Tx) error {
		return tx.InsertPackage(ctx, meta.Package{Repo: "cargo-hosted", Format: "cargo", Name: "qa-crate", Version: "0.1.0", Attrs: "{}", CreatedAt: t0})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, "cargo-hosted", "crates/qa-crate/0.1.0.crate", "", strings.NewReader("crate")); err != nil {
		t.Fatal(err)
	}

	del := func(user, repoName, query string, ui bool) resp {
		t.Helper()
		r := e.req(t, "DELETE", "/-/api/repositories/"+repoName+"/components?"+query, "", user, nil, ui)
		checkAPIHeaders(t, r)
		return r
	}
	for _, c := range []struct {
		user, repo, query string
		ui                bool
		want              int
	}{
		{"", "pypi-hosted", "name=my-pkg&version=1.0", true, 401},
		{"deleter", "pypi-hosted", "name=my-pkg&version=1.0", false, 403}, // no UI header
		{"reader", "pypi-hosted", "name=my-pkg&version=1.0", true, 403},
		{"ci", "pypi-hosted", "name=my-pkg&version=1.0", true, 403},
		{"deleter", "npm-hosted", "name=left-pad&version=1.0.0", true, 403},
		{"deleter", "ghost", "name=x&version=1", true, 404},
		{"admin", "npm-public", "name=left-pad&version=1.0.0", true, 400},
		{"admin", "maven-proxy", "group=org.example&name=app&version=1.0", true, 400},
		{"admin", "pypi-ro", "name=my-pkg&version=1.0", true, 400},
		{"admin", "maven-releases", "name=app&version=1.0", true, 400}, // no group
		{"deleter", "pypi-hosted", "name=my-pkg", true, 400},
		{"deleter", "pypi-hosted", "name=my-pkg&version=9.9", true, 404},
	} {
		if r := del(c.user, c.repo, c.query, c.ui); r.status != c.want {
			t.Errorf("%q %s %s: %d %s, want %d", c.user, c.repo, c.query, r.status, r.body, c.want)
		}
	}
	if len(sink.Events()) != 0 {
		t.Fatalf("refused deletes recorded events: %v", sink.Events())
	}

	files := func() float64 {
		return e.get(t, "/-/api/repositories/pypi-hosted", "deleter").json(t)["assets"].(float64)
	}
	if n := files(); n != 1 { // also caches the repository stats
		t.Fatalf("files before: %v", n)
	}
	r := del("deleter", "pypi-hosted", "name=My.Pkg&version=1.0", true)
	if m := r.json(t); r.status != 200 || m["assets"] != float64(1) || m["bytes"] != float64(len("sdist")) {
		t.Fatalf("pypi delete: %d %s", r.status, r.body)
	}
	if n := files(); n != 0 {
		t.Fatalf("files after the delete (stale stats): %v", n)
	}
	if pkgs, _ := e.db.ListPackages(ctx, "pypi-hosted", "my-pkg"); len(pkgs) != 0 {
		t.Fatalf("pypi rows left: %+v", pkgs)
	}
	if r := del("deleter", "cargo-hosted", "name=qa-crate&version=0.1.0", true); r.status != 200 {
		t.Fatalf("cargo delete: %d %s", r.status, r.body)
	}
	if _, err := e.db.GetAsset(ctx, "cargo-hosted", "crates/qa-crate/0.1.0.crate"); err == nil {
		t.Fatal("crate left")
	}
	if r := del("admin", "npm-hosted", "group=&name=left-pad&version=1.0.0", true); r.status != 200 {
		t.Fatalf("npm delete: %d %s", r.status, r.body)
	}
	got := sink.Events()
	if len(got) != 3 || got[0] != (repotest.Event{Kind: "delete", Actor: "deleter", Repo: "pypi-hosted", Subject: "My.Pkg@1.0", Detail: "assets=1"}) ||
		got[1].Subject != "qa-crate@0.1.0" || got[2] != (repotest.Event{Kind: "delete", Actor: "admin", Repo: "npm-hosted", Subject: "left-pad@1.0.0", Detail: "assets=1"}) {
		t.Fatalf("events: %v", got)
	}

	// canDelete: the delete privilege on an online, writable hosted repository.
	for user, want := range map[string]map[string]bool{
		"deleter": {"pypi-hosted": true, "cargo-hosted": true, "npm-hosted": false, "maven-releases": false},
		"admin":   {"npm-hosted": true, "maven-releases": true, "pypi-ro": false, "npm-public": false, "maven-proxy": false},
		"reader":  {"npm-hosted": false, "pypi-hosted": false},
	} {
		r := e.get(t, "/-/api/repositories", user)
		var body struct {
			Repositories []struct {
				Name      string
				CanDelete bool
			}
		}
		if err := json.Unmarshal([]byte(r.body), &body); err != nil {
			t.Fatal(err)
		}
		for _, rp := range body.Repositories {
			if w, ok := want[rp.Name]; ok && rp.CanDelete != w {
				t.Errorf("%s: %s canDelete = %v", user, rp.Name, rp.CanDelete)
			}
		}
	}
}
