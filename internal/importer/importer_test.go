package importer

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/importer/nexus"
)

func TestNormalize(t *testing.T) {
	now := time.Date(2026, 10, 27, 23, 0, 0, 0, time.UTC)
	p, errs := Params{URL: " https://Nexus.Example:8443/ctx/ ", ImportContent: true, Repositories: []string{"a", "a", " b ", ""}}.Normalize(now, nil)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if p.URL != "https://Nexus.Example:8443/ctx" || p.Job != "nexus.example-8443-20261027" || p.BlobStore != "default" ||
		p.Concurrency != DefaultConcurrency || strings.Join(p.Repositories, ",") != "a,b" {
		t.Fatalf("%+v", p)
	}
	for _, bad := range []Params{
		{URL: "ftp://x", ImportContent: true},
		{URL: "http://u:p@x", ImportContent: true},
		{URL: "http://x?q=1", ImportContent: true},
		{URL: "http://x"},
		{URL: "http://x", ImportContent: true, Concurrency: 17},
		{URL: "http://x", ImportContent: true, Concurrency: -1},
		{URL: "http://x", ImportContent: true, RenamePrefix: "a/b"},
		{URL: "http://x", ImportContent: true, Job: "../x"},
		{URL: "http://x", ImportContent: true, Username: "a:b"},
	} {
		if _, errs := bad.Normalize(now, nil); len(errs) == 0 {
			t.Errorf("%+v accepted", bad)
		}
	}
	if _, errs := (Params{URL: "http://x", ImportContent: true, BlobStore: "nope"}).Normalize(now, func(s string) bool { return s == "default" }); len(errs) != 1 {
		t.Fatalf("blob store: %v", errs)
	}
}

// Params have no password field, and Credentials never print or marshal.
func TestCredentialsRedacted(t *testing.T) {
	c := Credentials{Password: "hunter2"}
	b, _ := json.Marshal(struct {
		C Credentials
		P Params
	}{c, Params{URL: "http://x"}})
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s", c, c, c, c), string(b)} {
		if strings.Contains(s, "hunter2") {
			t.Fatalf("leaked: %s", s)
		}
	}
	if strings.Contains(strings.ToLower(string(b)), "password") {
		t.Fatalf("params have a password field: %s", b)
	}
}

func TestMappings(t *testing.T) {
	for in, want := range map[string]string{"ALLOW": "allow", "ALLOW_ONCE": "allow_once", "DENY": "deny"} {
		if got, ok := MapWritePolicy(in); !ok || got != want {
			t.Errorf("write %s: %s", in, got)
		}
	}
	for in, want := range map[string]string{"RELEASE": "release", "SNAPSHOT": "snapshot", "MIXED": "mixed"} {
		if got, ok := MapVersionPolicy(in); !ok || got != want {
			t.Errorf("version %s: %s", in, got)
		}
	}
	if _, ok := MapWritePolicy("SOMETIMES"); ok {
		t.Fatal("unknown write policy mapped")
	}
	for m, want := range map[int]string{-1: "-1", 0: "1s", 1: "1m", 60: "1h", 1440: "24h", 90: "90m"} {
		if got := MinutesToDuration(m).Text(); got != want {
			t.Errorf("%d minutes: %s, want %s", m, got, want)
		}
	}
	if d := MinutesToDuration(1 << 62); d <= 0 || d != MinutesToDuration(maxMinutes) {
		t.Fatalf("overflow: %v", d)
	}
	if MinutesToDuration(-1) != config.Never {
		t.Fatal("-1 is not never")
	}
	for in, want := range map[string]string{"maven2": "maven", "npm": "npm", "pypi": "pypi", "cargo": "cargo", "go": "go", "nuget": "", "raw": "", "docker": ""} {
		if got := mapFormat(in); got != want {
			t.Errorf("format %s: %q", in, got)
		}
	}
}

func TestAssetHelpers(t *testing.T) {
	a := nexus.Asset{Path: "crates/democrate/0.1.0/download"}
	if n, v, ok := crateOf(a); !ok || n != "democrate" || v != "0.1.0" {
		t.Fatalf("crate %s %s %v", n, v, ok)
	}
	a.Cargo = &nexus.NameVersion{Name: "Demo_Crate", Version: "0.1.0+b"}
	if n, v, _ := crateOf(a); n != "Demo_Crate" || v != "0.1.0+b" {
		t.Fatalf("crate attrs %s %s", n, v)
	}
	for _, p := range []string{"de/mo/democrate", "config.json", "crates/x/download"} {
		if _, _, ok := crateOf(nexus.Asset{Path: p}); ok {
			t.Errorf("%s is a crate", p)
		}
	}
	if n := npmPackage(nexus.Asset{Path: "@acme/scoped/-/scoped-2.0.0.tgz"}); n != "@acme/scoped" {
		t.Fatalf("npm %s", n)
	}
	if g := gaDir(nexus.Asset{Path: "com/ex/app/1.0/app-1.0.jar"}); g != "com/ex/app" {
		t.Fatalf("ga %s", g)
	}
	r := &repoRun{pl: &plan{format: "maven"}}
	for p, want := range map[string]assetKind{"com/ex/app/1.0/app-1.0.jar": kindIngest, "com/ex/app/1.0/app-1.0.jar.sha1": kindGenerated,
		"com/ex/app/maven-metadata.xml": kindGenerated, "com/ex/app/maven-metadata.xml.md5": kindGenerated, "com/ex/app/1.0/app-1.0.jar.asc": kindIngest,
		".index/nexus-maven-repository-index.gz": kindForeign, "archetype-catalog.xml": kindForeign} {
		if got := r.kind(nexus.Asset{Path: p}); got != want {
			t.Errorf("maven %s: %v", p, got)
		}
	}
	r.pl.format = "pypi"
	for p, want := range map[string]assetKind{"packages/x/1/x-1.whl": kindIngest, "packages/x/1/x-1.whl.metadata": kindGenerated, "simple/x/": kindGenerated} {
		if got := r.kind(nexus.Asset{Path: p}); got != want {
			t.Errorf("pypi %s: %v", p, got)
		}
	}
	r.pl.format = "npm"
	for p, want := range map[string]assetKind{"demo/-/demo-1.0.0.tgz": kindIngest, "demo": kindGenerated, "@a/b": kindGenerated} {
		if got := r.kind(nexus.Asset{Path: p}); got != want {
			t.Errorf("npm %s: %v", p, got)
		}
	}
}

// The expected name and version of an npm tarball: the asset's npm
// attributes, else the path "<name>/-/<base>-<version>.tgz" (scoped too).
func TestNpmNameVersion(t *testing.T) {
	for _, c := range []struct {
		a    nexus.Asset
		n, v string
	}{
		{nexus.Asset{Path: "demo/-/demo-1.0.0.tgz"}, "demo", "1.0.0"},
		{nexus.Asset{Path: "@acme/scoped/-/scoped-2.0.0-beta.1.tgz"}, "@acme/scoped", "2.0.0-beta.1"},
		{nexus.Asset{Path: "my-pkg/-/my-pkg-1.2.3+b.tgz"}, "my-pkg", "1.2.3+b"},
		{nexus.Asset{Path: "x/-/x-1.0.0.tgz", Npm: &nexus.NameVersion{Name: "y", Version: "9.9.9"}}, "y", "9.9.9"},
		{nexus.Asset{Path: "x/-/x-1.0.0.tgz", Npm: &nexus.NameVersion{Name: "x"}}, "x", "1.0.0"},
		{nexus.Asset{Path: "x/-/other.tgz"}, "x", ""},
	} {
		if n, v := npmNameVersion(c.a); n != c.n || v != c.v {
			t.Errorf("%s: %q %q, want %q %q", c.a.Path, n, v, c.n, c.v)
		}
	}
}

// Docker repositories are neither created nor imported, although Command
// Center has a docker format: the report says so and how to move images.
func TestPlanReportsDockerNotImported(t *testing.T) {
	e := &Engine{}
	rep := newReport("j", "http://x", false, time.Now())
	settings := []nexus.RepoSettings{
		{Name: "docker-hosted", Format: "docker", Type: "hosted"},
		{Name: "docker-proxy", Format: "docker", Type: "proxy"},
		{Name: "nuget-hosted", Format: "nuget", Type: "hosted"},
		{Name: "mvn", Format: "maven2", Type: "hosted"},
	}
	plans := e.plan(settings, Params{}, rep, discard{})
	if len(plans) != 1 || plans[0].src.Name != "mvn" {
		t.Fatalf("plans: %+v", plans)
	}
	if len(rep.Unsupported) != 2 || rep.Unsupported[0].Format != "docker" || rep.Unsupported[0].Count != 2 ||
		!strings.Contains(rep.Unsupported[0].Note, "docker push") || rep.Unsupported[1].Note != "" {
		t.Fatalf("unsupported: %+v", rep.Unsupported)
	}
}
