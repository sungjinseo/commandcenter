package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// TestComponentMetadata: the metadata API reads what each format stored
// (POM, npm manifest, Cargo index line, go.mod), tells "nothing collected"
// apart from "no dependencies", and needs read access.
func TestComponentMetadata(t *testing.T) {
	e := newEnvWith(t, "http://repo.example", func(c *config.Config) {
		c.Repositories = append(c.Repositories,
			config.Repository{Name: "cargo-hosted", Format: "cargo", Type: "hosted"},
			config.Repository{Name: "helm-hosted", Format: "helm", Type: "hosted"},
			config.Repository{Name: "yum-hosted", Format: "yum", Type: "hosted"},
			config.Repository{Name: "nuget-hosted", Format: "nuget", Type: "hosted"},
			config.Repository{Name: "go-hosted", Format: "go", Type: "hosted"})
	})
	ctx := context.Background()
	bs, err := blob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: bs}, DB: e.db, Now: time.Now}
	e.ui.Store = store
	put := func(repoName, path, body string) {
		t.Helper()
		if _, err := store.Put(ctx, repoName, path, "", strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	pkg := func(repoName, format, name, version, attrs string) {
		t.Helper()
		if err := e.db.Tx(ctx, func(tx *meta.Tx) error {
			return tx.InsertPackage(ctx, meta.Package{Repo: repoName, Format: format, Name: name, Version: version, Attrs: attrs, CreatedAt: t0})
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("maven-releases", "org/example/app/1.0/app-1.0.pom", `<?xml version="1.0"?>
<project><modelVersion>4.0.0</modelVersion><groupId>org.example</groupId><artifactId>app</artifactId><version>1.0</version>
<description>An app</description><url>https://example.org/app</url>
<licenses><license><name>Apache-2.0</name></license><license><name>MIT</name></license></licenses>
<scm><url>https://git.example.org/app</url></scm>
<properties><guava.version>33.0.0-jre</guava.version></properties>
<dependencies>
  <dependency><groupId>com.google.guava</groupId><artifactId>guava</artifactId><version>${guava.version}</version></dependency>
  <dependency><groupId>junit</groupId><artifactId>junit</artifactId><version>4.13.2</version><scope>test</scope><optional>true</optional></dependency>
</dependencies>
<build><plugins><plugin><groupId>x</groupId><artifactId>y</artifactId><dependencies><dependency><groupId>not</groupId><artifactId>counted</artifactId><version>1</version></dependency></dependencies></plugin></plugins></build>
</project>`)
	put("maven-releases", "org/example/nopom/1.0/nopom-1.0.jar", "jar")
	pkg("npm-hosted", "npm", "web", "2.0.0", `{"name":"web","version":"2.0.0","description":"Web thing","license":{"type":"ISC"},"homepage":"https://example.org/web","repository":{"type":"git","url":"git+https://git.example.org/web.git"},"dependencies":{"express":"^4.18.0","lodash":"~4.17.21"},"devDependencies":{"jest":"^29"},"peerDependencies":{"react":">=18"},"optionalDependencies":{"fsevents":"^2"}}`)
	pkg("cargo-hosted", "cargo", "qa-crate", "0.1.0", `{"name":"qa-crate","vers":"0.1.0","deps":[{"name":"serde","req":"^1.0","features":[],"optional":false,"default_features":true,"target":null,"kind":"normal"},{"name":"rand_core","req":"^0.6","features":[],"optional":true,"default_features":true,"target":null,"kind":"dev","package":"rand-core"}],"cksum":"x","features":{},"yanked":false}`)
	put("go-hosted", "example.com/mod/@v/v1.2.0.mod", "module example.com/mod\n\ngo 1.22\n\nrequire github.com/google/uuid v1.6.0 // indirect\n\nrequire (\n\tgolang.org/x/text v0.21.0\n\tgopkg.in/yaml.v3 v3.0.1\n)\n")

	get := func(user, repoName, query string) (int, componentMetadata) {
		t.Helper()
		r := e.req(t, "GET", "/-/api/repositories/"+repoName+"/components/metadata?"+query, "", user, nil, false)
		var m componentMetadata
		if r.status == 200 {
			if err := json.Unmarshal([]byte(r.body), &m); err != nil {
				t.Fatalf("%s: %v: %s", query, err, r.body)
			}
		}
		return r.status, m
	}
	deps := func(m componentMetadata) string {
		var parts []string
		for _, d := range m.Dependencies {
			parts = append(parts, d.Scope+":"+d.Group+":"+d.Name+":"+d.Requirement+":"+map[bool]string{false: "req", true: "opt"}[d.Optional])
		}
		return strings.Join(parts, " ")
	}

	st, m := get("admin", "maven-releases", "group=org.example&name=app&version=1.0")
	if st != 200 || !m.Collected || m.Resolved || *m.Description != "An app" || *m.License != "Apache-2.0, MIT" || *m.Homepage != "https://example.org/app" || *m.RepositoryURL != "https://git.example.org/app" {
		t.Fatalf("maven: %d %+v", st, m)
	}
	if got := deps(m); got != "compile:com.google.guava:guava:33.0.0-jre:req test:junit:junit:4.13.2:opt" {
		t.Fatalf("maven deps: %s", got)
	}
	if st, m := get("admin", "maven-releases", "group=org.example&name=nopom&version=1.0"); st != 200 || m.Collected || len(m.Dependencies) != 0 {
		t.Fatalf("maven without a POM: %d %+v", st, m)
	}
	if st, _ := get("admin", "maven-releases", "group=org.example&name=app&version=9.9"); st != 404 {
		t.Fatalf("maven missing: %d", st)
	}
	if st, _ := get("admin", "maven-releases", "name=app&version=1.0"); st != 400 {
		t.Fatalf("maven without group: %d", st)
	}

	st, m = get("admin", "npm-hosted", "name=web&version=2.0.0")
	if st != 200 || !m.Collected || *m.License != "ISC" || *m.RepositoryURL != "git+https://git.example.org/web.git" || *m.Description != "Web thing" {
		t.Fatalf("npm: %d %+v", st, m)
	}
	if got := deps(m); got != "runtime::express:^4.18.0:req runtime::lodash:~4.17.21:req optional::fsevents:^2:opt peer::react:>=18:req dev::jest:^29:req" {
		t.Fatalf("npm deps: %s", got)
	}

	st, m = get("admin", "cargo-hosted", "name=QA-Crate&version=0.1.0")
	if st != 200 || !m.Collected || m.License != nil {
		t.Fatalf("cargo: %d %+v", st, m)
	}
	if got := deps(m); got != "normal::serde:^1.0:req dev::rand-core:^0.6:opt" {
		t.Fatalf("cargo deps: %s", got)
	}

	st, m = get("admin", "go-hosted", "name=example.com/mod&version=v1.2.0")
	if st != 200 || !m.Collected {
		t.Fatalf("go: %d %+v", st, m)
	}
	if got := deps(m); got != "runtime::github.com/google/uuid:v1.6.0:req runtime::golang.org/x/text:v0.21.0:req runtime::gopkg.in/yaml.v3:v3.0.1:req" {
		t.Fatalf("go deps: %s", got)
	}
	if st, _ := get("admin", "go-hosted", "name=example.com/mod&version=v9.0.0"); st != 404 {
		t.Fatalf("go missing: %d", st)
	}

	// helm: the index entry (Chart.yaml plus created, digest, urls)
	pkg("helm-hosted", "helm", "demo", "1.0.0", `{"apiVersion":"v2","name":"demo","version":"1.0.0","description":"A demo chart","home":"https://example.org/demo","appVersion":"1.16.0","dependencies":[{"name":"common","version":"2.x.x","repository":"https://charts.example.com","condition":"common.enabled"},{"name":"redis","version":"^18"}],"created":"2026-10-27T12:00:00Z","digest":"x","urls":["demo-1.0.0.tgz"]}`)
	st, m = get("admin", "helm-hosted", "name=demo&version=1.0.0")
	if st != 200 || !m.Collected || *m.Description != "A demo chart" || *m.Homepage != "https://example.org/demo" || m.License != nil {
		t.Fatalf("helm: %d %+v", st, m)
	}
	if got := deps(m); got != "chart:https://charts.example.com:common:2.x.x:opt chart::redis:^18:req" {
		t.Fatalf("helm deps: %s", got)
	}
	if st, _ := get("admin", "helm-hosted", "name=demo&version=2.0.0"); st != 404 {
		t.Fatalf("helm missing: %d", st)
	}

	// yum: the parsed RPM header; requires and provides are the dependencies
	pkg("yum-hosted", "yum", "el9/demo", "1.0-1.el9.x86_64", `{"name":"demo","epoch":"0","version":"1.0","release":"1.el9","arch":"x86_64","summary":"A demo","description":"Demo does nothing.","license":"MIT","url":"https://example.org/demo","provides":[{"name":"demo","flags":"EQ","epoch":"0","ver":"1.0","rel":"1.el9"}],"requires":[{"name":"/bin/sh","pre":true},{"name":"bar","flags":"GE","epoch":"1","ver":"2.0"},{"name":"baz","flags":"LT","epoch":"0","ver":"3","rel":"1"}]}`)
	st, m = get("admin", "yum-hosted", "name=el9/demo&version=1.0-1.el9.x86_64")
	if st != 200 || !m.Collected || *m.Description != "A demo\n\nDemo does nothing." || *m.License != "MIT" || *m.Homepage != "https://example.org/demo" {
		t.Fatalf("yum: %d %+v", st, m)
	}
	if got := deps(m); got != "requires::/bin/sh::req requires::bar:>= 1:2.0:req requires::baz:< 3-1:req provides::demo:= 1.0-1.el9:req" {
		t.Fatalf("yum deps: %s", got)
	}

	// nuget: the .nuspec fields the push stored; dependency scope is the target framework
	pkg("nuget-hosted", "nuget", "demo.pkg", "1.0.0", `{"id":"Demo.Pkg","version":"1.0.0","authors":"Jane","description":"A demo package","license":"MIT","licenseUrl":"https://licenses.nuget.org/MIT","projectUrl":"https://example.org/demo","repositoryUrl":"https://git.example.org/demo","tags":["demo"],"dependencyGroups":[{"targetFramework":"net6.0","dependencies":[{"id":"Newtonsoft.Json","range":"[13.0.1, )"}]},{"dependencies":[{"id":"System.Memory","range":"(, )"}]}],"listed":true,"published":"2026-10-27T12:00:00Z"}`)
	st, m = get("admin", "nuget-hosted", "name=Demo.Pkg&version=1.0.0")
	if st != 200 || !m.Collected || *m.Description != "A demo package" || *m.License != "MIT" || *m.Homepage != "https://example.org/demo" || *m.RepositoryURL != "https://git.example.org/demo" {
		t.Fatalf("nuget: %d %+v", st, m)
	}
	if got := deps(m); got != "net6.0::Newtonsoft.Json:[13.0.1, ):req any::System.Memory:(, ):req" {
		t.Fatalf("nuget deps: %s", got)
	}
	if st, _ := get("admin", "nuget-hosted", "name=demo.pkg&version=2.0.0"); st != 404 {
		t.Fatalf("nuget missing: %d", st)
	}

	// pypi: the file's PEP 658 sidecar holds the core metadata; a file without one is "not collected"
	pkg("pypi-hosted", "pypi", "meta-pkg", "meta_pkg-1.0.tar.gz", `{"filename":"meta_pkg-1.0.tar.gz","version":"1.0","sha256":"x"}`)
	if st, m := get("admin", "pypi-hosted", "name=Meta.Pkg&version=1.0"); st != 200 || m.Collected {
		t.Fatalf("pypi without a sidecar: %d %+v", st, m)
	}
	put("pypi-hosted", "packages/meta-pkg/meta_pkg-1.0.tar.gz.metadata", "Metadata-Version: 2.1\r\nName: meta-pkg\r\nVersion: 1.0\r\nSummary: A package\r\nLicense: MIT\r\nHome-page: https://example.org/mp\r\nProject-URL: Source, https://git.example.org/mp\r\nRequires-Dist: requests>=2.0\r\nRequires-Dist: pytest; extra == \"test\"\r\n\r\nLong description with Requires-Dist: nope\r\n")
	st, m = get("admin", "pypi-hosted", "name=Meta.Pkg&version=1.0")
	if st != 200 || !m.Collected || *m.Description != "A package" || *m.License != "MIT" || *m.Homepage != "https://example.org/mp" || *m.RepositoryURL != "https://git.example.org/mp" {
		t.Fatalf("pypi: %d %+v", st, m)
	}
	if got := deps(m); got != "runtime::requests:>=2.0:req optional::pytest:; extra == \"test\":opt" {
		t.Fatalf("pypi deps: %s", got)
	}

	// Read access is required. Signed-in users read every repository when
	// authenticatedRead is on (the default); anonymous callers only the ones
	// with anonymousRead, and a private one looks missing to them.
	if st, _ := get("reader", "cargo-hosted", "name=qa-crate&version=0.1.0"); st != 200 {
		t.Fatalf("signed-in reader: %d", st)
	}
	if st, _ := get("", "cargo-hosted", "name=qa-crate&version=0.1.0"); st != 401 && st != 403 && st != 404 {
		t.Fatalf("anonymous on a private repository: %d", st)
	}
	if st, _ := get("", "npm-hosted", "name=web&version=2.0.0"); st != 200 {
		t.Fatalf("anonymous on a public repository: %d", st)
	}
}

// fillCoreMetadata: the body (after the first blank line, CRLF included) and
// the continuation lines of an old-style folded Description are not fields,
// so README text cannot add a dependency or replace the license.
func TestFillCoreMetadataBodyAndFolding(t *testing.T) {
	for name, doc := range map[string]string{
		"crlf body":          "Metadata-Version: 2.1\r\nName: x\r\nVersion: 1.0\r\nLicense: MIT\r\nRequires-Dist: requests\r\n\r\n# README\r\nLicense: GPL-3.0 (see LICENSE)\r\nRequires-Dist: evil-package\r\n",
		"folded description": "Metadata-Version: 2.1\nName: x\nVersion: 1.0\nLicense: MIT\nDescription: My package\n        \n        License: GPL (see LICENSE)\n        Requires-Dist: evil-package\nRequires-Dist: requests\n\n",
	} {
		var m componentMetadata
		fillCoreMetadata(doc, &m)
		if m.License == nil || *m.License != "MIT" || len(m.Dependencies) != 1 || m.Dependencies[0].Name != "requests" {
			t.Fatalf("%s: license %v, deps %+v (want MIT, [requests])", name, m.License, m.Dependencies)
		}
	}
}

// apt: the control fields of the hosted row (the architecture is the group).
func TestComponentMetadataApt(t *testing.T) {
	e := newEnvWith(t, "http://repo.example", func(c *config.Config) {
		c.Repositories = append(c.Repositories, config.Repository{Name: "apt-hosted", Format: "apt", Type: "hosted"})
	})
	ctx := context.Background()
	attrs := `{"package":"hello","version":"1.0","architecture":"amd64","component":"main","filename":"pool/main/h/hello/hello_1.0_amd64.deb","size":3,"md5":"m","sha1":"s","sha256":"x",` +
		`"fields":[["Package","hello"],["Version","1.0"],["Architecture","amd64"],["Maintainer","QA <qa@example.test>"],["Depends","libc6 (>= 2.34), curl | wget"],["Recommends","less"],["Homepage","https://example.test/hello"],["Description","Greets you\n long text"]]}`
	if err := e.db.Tx(ctx, func(tx *meta.Tx) error {
		return tx.InsertPackage(ctx, meta.Package{Repo: "apt-hosted", Format: "apt", Name: "hello", Version: "1.0_amd64", Attrs: attrs, CreatedAt: t0})
	}); err != nil {
		t.Fatal(err)
	}
	r := e.req(t, "GET", "/-/api/repositories/apt-hosted/components/metadata?group=amd64&name=hello&version=1.0", "", "admin", nil, false)
	var m componentMetadata
	if r.status != 200 || json.Unmarshal([]byte(r.body), &m) != nil || !m.Collected || *m.Description != "Greets you" || *m.Homepage != "https://example.test/hello" || *m.Maintainer != "QA <qa@example.test>" || m.License != nil {
		t.Fatalf("apt: %d %s", r.status, r.body)
	}
	var parts []string
	for _, d := range m.Dependencies {
		parts = append(parts, d.Scope+":"+d.Name+":"+d.Requirement+":"+map[bool]string{false: "req", true: "opt"}[d.Optional])
	}
	if got := strings.Join(parts, " "); got != "depends:libc6:(>= 2.34):req depends:curl:| wget:req recommends:less::opt" {
		t.Fatalf("apt deps: %s", got)
	}
	for _, q := range []string{"group=arm64&name=hello&version=1.0", "name=hello&version=1.0"} {
		if r := e.req(t, "GET", "/-/api/repositories/apt-hosted/components/metadata?"+q, "", "admin", nil, false); r.status != 404 {
			t.Fatalf("%s: %d", q, r.status)
		}
	}
}
