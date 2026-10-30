package nexusapi_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/app"
	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
)

// mvnEnv is the whole app with Maven repositories and an upstream that
// records what it is asked.
type mvnEnv struct {
	*env
	mu   sync.Mutex
	hits []string
}

func (m *mvnEnv) hitList() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.hits...)
}

const upstreamCatalog = `<?xml version="1.0" encoding="UTF-8"?>
<archetype-catalog xmlns="http://maven.apache.org/plugins/maven-archetype-plugin/archetype-catalog/1.0.0">
  <archetypes>
    <archetype><groupId>org.up</groupId><artifactId>arch-b</artifactId><version>1.0</version><repository>https://x</repository></archetype>
    <archetype><groupId>org.up</groupId><artifactId>arch-a</artifactId><version>2.0</version><description>A</description></archetype>
  </archetypes>
</archetype-catalog>`

func startMaven(t *testing.T) *mvnEnv {
	t.Helper()
	m := &mvnEnv{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits = append(m.hits, r.URL.Path)
		m.mu.Unlock()
		switch r.URL.Path {
		case "/maven2/org/up/lib/1.0/lib-1.0.jar", "/maven2/org/up/lib/1.0-SNAPSHOT/lib-1.0-SNAPSHOT.jar":
			io.WriteString(w, "upstream-jar")
		case "/maven2/archetype-catalog.xml":
			io.WriteString(w, upstreamCatalog)
		case "/maven2/docs/readme.txt":
			io.WriteString(w, "upstream readme")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
dataDir: %s
baseURL: http://nexus.example.com
anonymousRead: true
users:
  - { name: admin, passwordHash: %q, roles: [admin] }
  - { name: ci, passwordHash: %q, roles: [deployer] }
roles:
  deployer: { write: [releases, snapshots, loose] }
repositories:
  - { name: releases, format: maven, type: hosted, versionPolicy: release, writePolicy: allow_once }
  - { name: snapshots, format: maven, type: hosted, versionPolicy: snapshot, writePolicy: allow }
  - { name: loose, format: maven, type: hosted, versionPolicy: mixed, writePolicy: allow, layoutPolicy: permissive, contentDisposition: attachment }
  - { name: rel-proxy, format: maven, type: proxy, versionPolicy: release, remote: %s/maven2/ }
  - { name: snap-proxy, format: maven, type: proxy, versionPolicy: snapshot, remote: %s/maven2/ }
  - { name: perm-proxy, format: maven, type: proxy, layoutPolicy: permissive, remote: %s/maven2/ }
  - { name: public, format: maven, type: group, members: [releases, snapshots, rel-proxy, snap-proxy] }
`, t.TempDir(), string(h), string(h), up.URL, up.URL, up.URL)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.URL = metatest.URL(t, cfg.DataDir)
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(func() { ts.Close(); a.Close() })
	m.env = &env{t, ts}
	return m
}

func pom(g, a, v, packaging string) string {
	return fmt.Sprintf(`<project><modelVersion>4.0.0</modelVersion><groupId>%s</groupId><artifactId>%s</artifactId><version>%s</version><packaging>%s</packaging><name>%s name</name></project>`,
		g, a, v, packaging, a)
}

// deploy uploads files as mvn deploy does: each file, then its sha1 and
// md5, the POM last; then the version document of a SNAPSHOT and the GA
// document, with their checksums.
func (m *mvnEnv) deploy(repoName, g, a, base, version string, files map[string]string) {
	m.t.Helper()
	dir := strings.ReplaceAll(g, ".", "/") + "/" + a + "/" + base + "/"
	put := func(p, body string) {
		m.t.Helper()
		m.want("PUT", "/repository/"+repoName+"/"+p, "ci", body, 201)
	}
	var poms []string
	for name, body := range files {
		if strings.HasSuffix(name, ".pom") {
			poms = append(poms, name)
			continue
		}
		put(dir+name, body)
		put(dir+name+".sha1", "0000")
		put(dir+name+".md5", "0000")
	}
	for _, name := range poms {
		put(dir+name, files[name])
		put(dir+name+".sha1", "0000")
	}
	if strings.HasSuffix(base, "-SNAPSHOT") {
		stamp := strings.TrimPrefix(version, strings.TrimSuffix(base, "SNAPSHOT"))
		ts, n, _ := strings.Cut(stamp, "-")
		put(dir+"maven-metadata.xml", fmt.Sprintf(`<metadata><groupId>%s</groupId><artifactId>%s</artifactId><version>%s</version>
<versioning><snapshot><timestamp>%s</timestamp><buildNumber>%s</buildNumber></snapshot></versioning></metadata>`, g, a, base, ts, n))
		put(dir+"maven-metadata.xml.sha1", "0000")
	}
	ga := strings.ReplaceAll(g, ".", "/") + "/" + a + "/maven-metadata.xml"
	put(ga, fmt.Sprintf(`<metadata><groupId>%s</groupId><artifactId>%s</artifactId><versioning><versions><version>%s</version></versions></versioning></metadata>`, g, a, base))
	put(ga+".sha1", "0000")
}

// deployBuild deploys build n (a jar, a sources jar and the POM) of
// org.example:app:1.0-SNAPSHOT to snapshots.
func (m *mvnEnv) deployBuild(n int) string {
	v := fmt.Sprintf("1.0-20261014.12000%d-%d", n, n)
	m.deploy("snapshots", "org.example", "app", "1.0-SNAPSHOT", v, map[string]string{
		"app-" + v + ".jar": "jar " + v, "app-" + v + "-sources.jar": "sources " + v, "app-" + v + ".pom": pom("org.example", "app", "1.0-SNAPSHOT", "jar")})
	return v
}

type compItem struct {
	ID, Version string
	Assets      []struct {
		Path   string
		Maven2 map[string]string
	}
}

func (m *mvnEnv) items(path string) []compItem {
	m.t.Helper()
	var page struct{ Items []compItem }
	m.get(path, "admin", &page)
	return page.Items
}

// maven-01..03, core-18: each timestamped build is a component, found and
// deleted alone, with the maven2 attributes of its files; the "latest
// snapshot" download picks the newest build's main jar.
func TestMavenSnapshotBuildsAreComponents(t *testing.T) {
	m := startMaven(t)
	b1, b2, b3 := m.deployBuild(1), m.deployBuild(2), m.deployBuild(3)
	items := m.items(v1 + "/components?repository=snapshots")
	if len(items) != 3 || items[0].Version != b1 || items[2].Version != b3 {
		t.Fatalf("components: %+v", items)
	}
	attrs := map[string]map[string]string{}
	for _, a := range items[1].Assets {
		attrs[a.Path] = a.Maven2
	}
	src := attrs["/org/example/app/1.0-SNAPSHOT/app-"+b2+"-sources.jar"]
	if len(items[1].Assets) != 3 || src["classifier"] != "sources" || src["extension"] != "jar" || src["version"] != b2 || src["groupId"] != "org.example" {
		t.Fatalf("assets of %s: %+v", b2, items[1].Assets)
	}
	if main := attrs["/org/example/app/1.0-SNAPSHOT/app-"+b2+".jar"]; main["classifier"] != "" || main["extension"] != "jar" {
		t.Fatalf("main jar: %+v", main)
	}
	if n := len(m.items(v1 + "/search?repository=snapshots&maven.baseVersion=1.0-SNAPSHOT")); n != 3 {
		t.Fatalf("search by baseVersion: %d", n)
	}
	if it := m.items(v1 + "/search?repository=snapshots&version=" + b1); len(it) != 1 || it[0].Version != b1 {
		t.Fatalf("search by version: %+v", it)
	}
	download := func(q string) string {
		t.Helper()
		st, b, hdr := m.do("GET", v1+"/search/assets/download?repository=snapshots&maven.groupId=org.example&maven.artifactId=app&maven.baseVersion=1.0-SNAPSHOT"+q, "admin", "")
		if st != 302 {
			t.Fatalf("download %s: %d %s", q, st, b)
		}
		return hdr.Get("Location")
	}
	if loc := download("&maven.extension=jar&sort=version"); !strings.HasSuffix(loc, "/app-"+b3+".jar") {
		t.Fatalf("latest jar: %s", loc)
	}
	if loc := download("&maven.extension=jar&maven.classifier=sources&sort=version"); !strings.HasSuffix(loc, "/app-"+b3+"-sources.jar") {
		t.Fatalf("latest sources: %s", loc)
	}
	if loc := download("&maven.extension=jar&sort=version&direction=asc"); !strings.HasSuffix(loc, "/app-"+b1+".jar") {
		t.Fatalf("oldest jar: %s", loc)
	}

	// Deleting a build deletes only its files; mvn still resolves the others.
	m.want("DELETE", v1+"/components/"+items[2].ID, "admin", "", 204)
	if left := m.items(v1 + "/components?repository=snapshots"); len(left) != 2 || left[1].Version != b2 {
		t.Fatalf("after delete: %+v", left)
	}
	b := m.want("GET", "/repository/snapshots/org/example/app/1.0-SNAPSHOT/maven-metadata.xml", "", "", 200)
	if !strings.Contains(string(b), "<value>"+b2+"</value>") || strings.Contains(string(b), b3) {
		t.Fatalf("version document: %s", b)
	}
	if got := m.want("GET", "/repository/public/org/example/app/1.0-SNAPSHOT/app-"+b2+".jar", "", "", 200); string(got) != "jar "+b2 {
		t.Fatalf("resolve: %s", got)
	}
	if b := m.want("GET", "/repository/snapshots/org/example/app/maven-metadata.xml", "", "", 200); !strings.Contains(string(b), "<version>1.0-SNAPSHOT</version>") {
		t.Fatalf("GA document: %s", b)
	}
	// A non-unique SNAPSHOT is the component X-SNAPSHOT, with a version
	// document in the non-unique form (maven-17).
	m.want("PUT", "/repository/snapshots/org/example/old/2.0-SNAPSHOT/old-2.0-SNAPSHOT.jar", "ci", "old", 201)
	if b := m.want("GET", "/repository/snapshots/org/example/old/2.0-SNAPSHOT/maven-metadata.xml", "", "", 200); !strings.Contains(string(b), "<buildNumber>1</buildNumber>") || strings.Contains(string(b), "<timestamp>") {
		t.Fatalf("non-unique version document: %s", b)
	}
}

// sort=version orders Maven versions as Maven does (maven.CompareVersions).
func TestMavenSortVersion(t *testing.T) {
	m := startMaven(t)
	for _, v := range []string{"1.0", "1.0-rc1", "1.0-alpha-2", "1.10", "1.9"} {
		m.want("PUT", "/repository/releases/org/example/lib/"+v+"/lib-"+v+".jar", "ci", "jar", 201)
	}
	var got []string
	for _, it := range m.items(v1 + "/search?repository=releases&name=lib&sort=version&direction=asc") {
		got = append(got, it.Version)
	}
	if strings.Join(got, " ") != "1.0-alpha-2 1.0-rc1 1.0 1.9 1.10" {
		t.Fatalf("order: %s", got)
	}
}

type mavenBlock struct {
	VersionPolicy, LayoutPolicy, ContentDisposition string
}

// maven-08..10: layoutPolicy and contentDisposition are real settings,
// reported as stored; a group has no maven object.
func TestMavenRepositoryPolicies(t *testing.T) {
	m := startMaven(t)
	settings := func(typ, name string) map[string]json.RawMessage {
		t.Helper()
		var s map[string]json.RawMessage
		m.get(v1+"/repositories/maven/"+typ+"/"+name, "admin", &s)
		return s
	}
	block := func(typ, name string) mavenBlock {
		t.Helper()
		var b mavenBlock
		if err := json.Unmarshal(settings(typ, name)["maven"], &b); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return b
	}
	if b := block("hosted", "releases"); b != (mavenBlock{"RELEASE", "STRICT", "INLINE"}) {
		t.Fatalf("releases: %+v", b)
	}
	if b := block("hosted", "loose"); b != (mavenBlock{"MIXED", "PERMISSIVE", "ATTACHMENT"}) {
		t.Fatalf("loose: %+v", b)
	}
	if _, ok := settings("group", "public")["maven"]; ok {
		t.Fatal("a group has a maven object")
	}
	m.want("POST", v1+"/repositories/maven/hosted", "admin", `{"name":"made","online":true,"storage":{"blobStoreName":"default","writePolicy":"ALLOW"},
"maven":{"versionPolicy":"MIXED","layoutPolicy":"PERMISSIVE","contentDisposition":"ATTACHMENT"}}`, 201)
	if b := block("hosted", "made"); b != (mavenBlock{"MIXED", "PERMISSIVE", "ATTACHMENT"}) {
		t.Fatalf("created: %+v", b)
	}
	// An update without layoutPolicy or contentDisposition keeps them; one with STRICT sets it.
	m.want("PUT", v1+"/repositories/maven/hosted/made", "admin", `{"name":"made","online":true,"storage":{"blobStoreName":"default","writePolicy":"ALLOW"},"maven":{"versionPolicy":"MIXED"}}`, 204)
	if b := block("hosted", "made"); b != (mavenBlock{"MIXED", "PERMISSIVE", "ATTACHMENT"}) {
		t.Fatalf("updated: %+v", b)
	}
	m.want("PUT", v1+"/repositories/maven/hosted/made", "admin", `{"name":"made","online":true,"storage":{"blobStoreName":"default","writePolicy":"ALLOW"},"maven":{"versionPolicy":"MIXED","layoutPolicy":"STRICT"}}`, 204)
	if b := block("hosted", "made"); b.LayoutPolicy != "STRICT" {
		t.Fatalf("strict again: %+v", b)
	}
	// A Terraform group body carries a maven object, which is ignored.
	m.want("POST", v1+"/repositories/maven/group", "admin", `{"name":"grp","online":true,"storage":{"blobStoreName":"default"},
"group":{"memberNames":["releases"]},"maven":{"versionPolicy":"MIXED","layoutPolicy":"STRICT"}}`, 201)

	// PERMISSIVE stores any path; STRICT refuses it.
	m.want("PUT", "/repository/loose/docs/readme.txt", "ci", "read me", 201)
	m.want("PUT", "/repository/snapshots/docs/readme.txt", "ci", "read me", 400)
	m.want("PUT", "/repository/loose/.index/nexus-maven-repository-index.gz", "ci", "x", 400) // no indexer, whatever the policy
	// A permissive proxy fetches any path; a strict one does not ask.
	if b := m.want("GET", "/repository/perm-proxy/docs/readme.txt", "", "", 200); string(b) != "upstream readme" {
		t.Fatalf("permissive proxy: %s", b)
	}
	m.want("GET", "/repository/rel-proxy/docs/readme.txt", "", "", 404)
	if n := strings.Count(strings.Join(m.hitList(), " "), "/docs/readme.txt"); n != 1 {
		t.Fatalf("upstream asked %d times for docs/readme.txt", n)
	}

	// Content-Disposition: attachment where set, inline by default, none on metadata.
	m.want("PUT", "/repository/loose/org/example/app/1.0/app-1.0.jar", "ci", "jar", 201)
	m.want("PUT", "/repository/releases/org/example/app/1.0/app-1.0.jar", "ci", "jar", 201)
	disposition := func(p string) string {
		t.Helper()
		st, b, hdr := m.do("GET", p, "", "")
		if st != 200 {
			t.Fatalf("GET %s: %d %s", p, st, b)
		}
		return hdr.Get("Content-Disposition")
	}
	if d := disposition("/repository/loose/org/example/app/1.0/app-1.0.jar"); d != `attachment; filename=app-1.0.jar` {
		t.Fatalf("attachment: %q", d)
	}
	if d := disposition("/repository/releases/org/example/app/1.0/app-1.0.jar"); d != "inline" {
		t.Fatalf("inline: %q", d)
	}
	if d := disposition("/repository/releases/org/example/app/maven-metadata.xml"); d != "" {
		t.Fatalf("metadata: %q", d)
	}
}

// maven-11: the version policy applies to reads of hosted and proxy
// repositories, so a proxy is not asked for what its policy refuses, and a
// group asks only the members that allow the path.
func TestMavenVersionPolicyOnReads(t *testing.T) {
	m := startMaven(t)
	snap := "org/up/lib/1.0-SNAPSHOT/lib-1.0-SNAPSHOT.jar"
	m.want("GET", "/repository/rel-proxy/"+snap, "", "", 404)
	m.want("HEAD", "/repository/rel-proxy/"+snap, "", "", 404)
	m.want("GET", "/repository/rel-proxy/org/up/lib/1.0-SNAPSHOT/maven-metadata.xml", "", "", 404)
	m.want("GET", "/repository/snap-proxy/org/up/lib/1.0/lib-1.0.jar", "", "", 404)
	if h := m.hitList(); len(h) != 0 {
		t.Fatalf("upstream asked: %v", h)
	}
	// Through the group: the release comes from rel-proxy, and snap-proxy is
	// never asked for it; the snapshot from snap-proxy alone.
	if b := m.want("GET", "/repository/public/org/up/lib/1.0/lib-1.0.jar", "", "", 200); string(b) != "upstream-jar" {
		t.Fatalf("release: %s", b)
	}
	if b := m.want("GET", "/repository/public/"+snap, "", "", 200); string(b) != "upstream-jar" {
		t.Fatalf("snapshot: %s", b)
	}
	if h := m.hitList(); len(h) != 2 {
		t.Fatalf("upstream asked %d times: %v", len(h), h)
	}
	// A hosted RELEASE repository answers 400 to the DELETE of a snapshot path.
	m.want("DELETE", "/repository/releases/"+snap, "admin", "", 400)
}

// maven-13: a hosted repository lists its archetypes; a group merges its
// members' catalogs.
func TestMavenArchetypeCatalog(t *testing.T) {
	m := startMaven(t)
	m.want("GET", "/repository/releases/archetype-catalog.xml", "", "", 404)
	m.deploy("releases", "org.example", "quick-archetype", "1.0", "1.0", map[string]string{
		"quick-archetype-1.0.jar": "jar", "quick-archetype-1.0.pom": pom("org.example", "quick-archetype", "1.0", "maven-archetype")})
	m.deploy("releases", "org.example", "lib", "1.0", "1.0", map[string]string{"lib-1.0.jar": "jar", "lib-1.0.pom": pom("org.example", "lib", "1.0", "jar")})
	b := m.want("GET", "/repository/releases/archetype-catalog.xml", "", "", 200)
	if !strings.Contains(string(b), "<artifactId>quick-archetype</artifactId>") || strings.Contains(string(b), "<artifactId>lib</artifactId>") {
		t.Fatalf("hosted catalog: %s", b)
	}
	m.want("GET", "/repository/releases/archetype-catalog.xml.sha1", "", "", 200)
	b = m.want("GET", "/repository/public/archetype-catalog.xml", "", "", 200)
	a, bb, q := strings.Index(string(b), "arch-a"), strings.Index(string(b), "arch-b"), strings.Index(string(b), "quick-archetype")
	if a < 0 || bb < 0 || q < 0 || !(q < a && a < bb) || strings.Contains(string(b), "https://x") {
		t.Fatalf("merged catalog (org.example before org.up, sorted, no repository): %s", b)
	}
	// Deleting the archetype takes it out of the catalog.
	var items []compItem
	for _, it := range m.items(v1 + "/search?repository=releases&name=quick-archetype") {
		items = append(items, it)
	}
	if len(items) != 1 {
		t.Fatalf("search: %+v", items)
	}
	m.want("DELETE", v1+"/components/"+items[0].ID, "admin", "", 204)
	m.want("GET", "/repository/releases/archetype-catalog.xml", "", "", 404)
}

// maven-14, maven-20: under allow_once a signature may be redeployed, the
// signed file not; a signature is served as application/pgp-signature.
func TestMavenSignatureRedeploy(t *testing.T) {
	m := startMaven(t)
	p := "/repository/releases/org/example/app/1.0/app-1.0.jar"
	m.want("PUT", p, "ci", "jar", 201)
	m.want("PUT", p+".asc", "ci", "sig 1", 201)
	m.want("PUT", p+".asc", "admin", "sig 2", 201) // an overwrite needs Edit, which the admin has
	m.want("PUT", p, "admin", "jar 2", 400)
	st, b, hdr := m.do("GET", p+".asc", "", "")
	if st != 200 || string(b) != "sig 2" || hdr.Get("Content-Type") != "application/pgp-signature" {
		t.Fatalf("signature: %d %q %s", st, b, hdr.Get("Content-Type"))
	}
}

// maven-16: a classifier may hold dots and an extension start with a digit.
func TestMavenDottedClassifier(t *testing.T) {
	m := startMaven(t)
	m.want("PUT", "/repository/releases/org/example/app/1.0/app-1.0-linux.x86_64.zip", "ci", "zip", 201)
	m.want("PUT", "/repository/releases/org/example/app/1.0/app-1.0.7z", "ci", "7z", 201)
	var page struct {
		Items []struct {
			Path   string
			Maven2 map[string]string
		}
	}
	m.get(v1+"/search/assets?repository=releases&maven.classifier=linux.x86_64", "admin", &page)
	if len(page.Items) != 1 || page.Items[0].Maven2["extension"] != "zip" || page.Items[0].Maven2["classifier"] != "linux.x86_64" {
		t.Fatalf("dotted classifier: %+v", page.Items)
	}
	page.Items = nil
	m.get(v1+"/search/assets?repository=releases&maven.extension=7z", "admin", &page)
	if len(page.Items) != 1 || page.Items[0].Maven2["classifier"] != "" {
		t.Fatalf("7z: %+v", page.Items)
	}
}

func pluginJar(t *testing.T, prefix string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("META-INF/maven/plugin.xml")
	fmt.Fprintf(f, `<?xml version="1.0"?><plugin><name>x</name><groupId>org.example.plugins</groupId><artifactId>tool-maven-plugin</artifactId>
<goalPrefix>%s</goalPrefix><mojos><mojo><goal>run</goal></mojo></mojos></plugin>`, prefix)
	zw.Close()
	return buf.String()
}

// runTask creates a task through the Nexus API, runs it and waits for its result.
func (m *mvnEnv) runTask(typ string, props map[string]string) map[string]any {
	m.t.Helper()
	pb, _ := json.Marshal(props)
	var created map[string]any
	json.Unmarshal(m.want("POST", v1+"/tasks", "admin", fmt.Sprintf(`{"type":%q,"name":"t","frequency":{"schedule":"manual"},"properties":%s}`, typ, pb), 201), &created)
	id := created["id"].(string)
	m.want("POST", v1+"/tasks/"+id+"/run", "admin", "", 204)
	for i := 0; i < 200; i++ {
		var item map[string]any
		m.get(v1+"/tasks/"+id, "admin", &item)
		if item["lastRunResult"] != nil && item["currentState"] != "RUNNING" {
			return item
		}
		time.Sleep(50 * time.Millisecond)
	}
	m.t.Fatalf("task %s did not finish", typ)
	return nil
}

// maven-15, maven-07: rebuild-metadata takes Nexus's properties, and builds
// the plugin-group document from the plugin's jar; deleting the plugin
// drops its prefix.
func TestMavenPluginGroupRebuild(t *testing.T) {
	m := startMaven(t)
	dir := "/repository/releases/org/example/plugins/tool-maven-plugin/1.0/"
	m.want("PUT", dir+"tool-maven-plugin-1.0.jar", "ci", pluginJar(t, "tool"), 201)
	m.want("PUT", dir+"tool-maven-plugin-1.0.pom", "ci", pom("org.example.plugins", "tool-maven-plugin", "1.0", "maven-plugin"), 201)
	m.want("GET", "/repository/releases/org/example/plugins/maven-metadata.xml", "", "", 404) // imported without the G document
	item := m.runTask("repository.maven.rebuild-metadata", map[string]string{"repositoryName": "releases", "groupId": "org.example.plugins",
		"rebuildChecksums": "true", "cascadeRebuild": "true"})
	if item["lastRunResult"] != "OK" {
		t.Fatalf("rebuild: %+v", item)
	}
	b := m.want("GET", "/repository/releases/org/example/plugins/maven-metadata.xml", "", "", 200)
	if !strings.Contains(string(b), "<prefix>tool</prefix>") || !strings.Contains(string(b), "<artifactId>tool-maven-plugin</artifactId>") ||
		!strings.Contains(string(b), "<name>tool-maven-plugin name</name>") {
		t.Fatalf("plugin-group document: %s", b)
	}
	items := m.items(v1 + "/search?repository=releases&name=tool-maven-plugin")
	if len(items) != 1 {
		t.Fatalf("search: %+v", items)
	}
	m.want("DELETE", v1+"/components/"+items[0].ID, "admin", "", 204)
	m.want("GET", "/repository/releases/org/example/plugins/maven-metadata.xml", "", "", 404)
	// Unknown levels are refused, as Nexus's form does.
	m.want("POST", v1+"/tasks", "admin", `{"type":"repository.maven.rebuild-metadata","name":"t","frequency":{"schedule":"manual"},"properties":{"repositoryName":"releases","artifactId":"x"}}`, 400)
}

// maven-04..06: the snapshot tasks take Nexus's ids and properties, "*"
// and groups; minimumRetained -1 removes nothing by age.
func TestMavenSnapshotTasks(t *testing.T) {
	m := startMaven(t)
	for n := 1; n <= 3; n++ {
		m.deployBuild(n)
	}
	item := m.runTask("repository.maven.remove-snapshots", map[string]string{"repositoryName": "*", "minimumRetained": "-1",
		"snapshotRetentionDays": "0", "removeIfReleased": "false", "gracePeriodInDays": ""})
	if item["lastRunResult"] != "OK" || len(m.items(v1+"/components?repository=snapshots")) != 3 {
		t.Fatalf("minimumRetained -1: %+v", item)
	}
	item = m.runTask("repository.maven.remove-snapshots", map[string]string{"repositoryName": "public", "minimumRetained": "1", "snapshotRetentionDays": "0"})
	left := m.items(v1 + "/components?repository=snapshots")
	if item["lastRunResult"] != "OK" || len(left) != 1 || left[0].Version != "1.0-20261014.120003-3" {
		t.Fatalf("minimumRetained 1 through the group: %+v %+v", item, left)
	}
	if b := m.want("GET", "/repository/snapshots/org/example/app/1.0-SNAPSHOT/maven-metadata.xml", "", "", 200); !strings.Contains(string(b), "<buildNumber>3</buildNumber>") {
		t.Fatalf("version document: %s", b)
	}
	// A release in another repository removes the snapshot (removeIfReleased).
	m.want("PUT", "/repository/releases/org/example/app/1.0/app-1.0.jar", "ci", "jar", 201)
	item = m.runTask("repository.maven.remove-snapshots", map[string]string{"repositoryName": "snapshots", "minimumRetained": "1",
		"snapshotRetentionDays": "30", "removeIfReleased": "true", "gracePeriodInDays": "0"})
	if item["lastRunResult"] != "OK" || len(m.items(v1+"/components?repository=snapshots")) != 0 {
		t.Fatalf("removeIfReleased: %+v", item)
	}
	// Delete unused SNAPSHOT: a fresh build was used today.
	m.deployBuild(4)
	if item := m.runTask("repository.maven.purge-unused-snapshots", map[string]string{"repositoryName": "snapshots", "lastUsed": "1"}); item["lastRunResult"] != "OK" ||
		len(m.items(v1+"/components?repository=snapshots")) != 1 {
		t.Fatalf("purge unused: %+v", item)
	}
	m.want("POST", v1+"/tasks", "admin", `{"type":"repository.maven.purge-unused-snapshots","name":"t","frequency":{"schedule":"manual"},"properties":{"repositoryName":"releases","lastUsed":"1"}}`, 400)
}
