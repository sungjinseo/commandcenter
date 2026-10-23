package maven

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"commandcenter/internal/config"
	"commandcenter/internal/repo"
)

func versionsOf(t *testing.T, e *env, repoName string) string {
	t.Helper()
	w := e.do("GET", repoName, "org/example/app/maven-metadata.xml", "")
	if w.Code != 200 {
		t.Fatalf("metadata: %d %s", w.Code, w.Body)
	}
	m, err := ParseMetadata(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(m.Versioning.Versions, ",") + " release=" + m.Versioning.Release
}

func TestPutAndGet(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "releases", "org/example/app/1.0/app-1.0.jar", "jar1"); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "releases", "org/example/app/1.0/app-1.0.jar", ""); w.Body.String() != "jar1" {
		t.Fatalf("GET after PUT: %q", w.Body)
	}
	if w := e.do("PUT", "releases", "org/example/app/1.0/app-1.0.jar.sha1", "ignored"); w.Code != 201 {
		t.Fatalf("PUT checksum: %d", w.Code)
	}
	if w := e.do("GET", "releases", "org/example/app/1.0/app-1.0.jar.sha1", ""); w.Body.String() != sha1hex("jar1") {
		t.Fatalf("checksum must be generated, got %q", w.Body)
	}
}

func TestWritePolicies(t *testing.T) {
	e := newEnv(t)
	e.do("PUT", "releases", "org/example/app/1.0/app-1.0.jar", "a")
	if w := e.do("PUT", "releases", "org/example/app/1.0/app-1.0.jar", "b"); w.Code != 400 {
		t.Fatalf("allow_once redeploy: %d", w.Code)
	}
	// allow: snapshots repo accepts redeploys
	path := "org/example/app/1.0-SNAPSHOT/app-1.0-SNAPSHOT.jar"
	e.do("PUT", "snapshots", path, "a")
	if w := e.do("PUT", "snapshots", path, "b"); w.Code != 201 {
		t.Fatalf("allow redeploy: %d", w.Code)
	}
}

func TestDenyPolicy(t *testing.T) {
	e := newEnv(t, configRepo("frozen", "deny"))
	if w := e.do("PUT", "frozen", "org/example/app/1.0/app-1.0.jar", "a"); w.Code != 400 {
		t.Fatalf("deny: %d", w.Code)
	}
	e.seed(t, "frozen", "org/example/app/1.0/app-1.0.jar", "kept")
	if w := e.do("DELETE", "frozen", "org/example/app/1.0/app-1.0.jar", ""); w.Code != 400 {
		t.Fatalf("deny DELETE: %d", w.Code)
	}
	if w := e.do("GET", "frozen", "org/example/app/1.0/app-1.0.jar", ""); w.Code != 200 || w.Body.String() != "kept" {
		t.Fatalf("asset must survive denied DELETE: %d %q", w.Code, w.Body)
	}
}

func TestAllowOnceConcurrent(t *testing.T) {
	e := newEnv(t)
	const n = 10
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes <- e.do("PUT", "releases", "org/example/app/1.0/app-1.0.jar", fmt.Sprintf("body-%d", i)).Code
		}(i)
	}
	wg.Wait()
	close(codes)
	created := 0
	for c := range codes {
		switch c {
		case 201:
			created++
		case 400:
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if created != 1 {
		t.Fatalf("allow_once: %d concurrent PUTs created, want exactly 1", created)
	}
}

func TestVersionPolicies(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "releases", "org/example/app/1.0-SNAPSHOT/app-1.0-SNAPSHOT.jar", "a"); w.Code != 400 {
		t.Fatalf("release repo accepted snapshot: %d", w.Code)
	}
	if w := e.do("PUT", "releases", "org/example/app/1.0-SNAPSHOT/maven-metadata.xml", "<metadata/>"); w.Code != 400 {
		t.Fatalf("release repo accepted snapshot metadata: %d", w.Code)
	}
	if w := e.do("PUT", "snapshots", "org/example/app/1.0/app-1.0.jar", "a"); w.Code != 400 {
		t.Fatalf("snapshot repo accepted release: %d", w.Code)
	}
	if w := e.do("PUT", "releases", "org/example/app/1.0/bogus.jar", "a"); w.Code != 400 {
		t.Fatalf("invalid artifact path: %d", w.Code)
	}
}

func TestMetadataMaintenance(t *testing.T) {
	e := newEnv(t)
	e.do("PUT", "releases", "org/example/app/1.0/app-1.0.pom", "p1")
	e.do("PUT", "releases", "org/example/app/2.0/app-2.0.pom", "p2")
	if got := versionsOf(t, e, "releases"); got != "1.0,2.0 release=2.0" {
		t.Fatalf("after puts: %s", got)
	}
	// a client-uploaded document is merged, never replacing known versions
	up := `<metadata><groupId>org.example</groupId><artifactId>app</artifactId>
<versioning><versions><version>0.5</version></versions><lastUpdated>20200101000000</lastUpdated></versioning></metadata>`
	if w := e.do("PUT", "releases", "org/example/app/maven-metadata.xml", up); w.Code != 201 {
		t.Fatalf("PUT metadata: %d %s", w.Code, w.Body)
	}
	if got := versionsOf(t, e, "releases"); got != "0.5,1.0,2.0 release=2.0" {
		t.Fatalf("after metadata put: %s", got)
	}
	if w := e.do("PUT", "releases", "org/example/app/maven-metadata.xml", "not xml"); w.Code != 400 {
		t.Fatalf("bad metadata: %d", w.Code)
	}
	// delete rebuilds from the remaining artifacts
	if w := e.do("DELETE", "releases", "org/example/app/2.0/app-2.0.pom", ""); w.Code != 204 {
		t.Fatalf("DELETE: %d", w.Code)
	}
	if got := versionsOf(t, e, "releases"); got != "1.0 release=1.0" {
		t.Fatalf("after delete: %s", got)
	}
	if w := e.do("DELETE", "releases", "org/example/app/2.0/app-2.0.pom", ""); w.Code != 404 {
		t.Fatalf("DELETE missing: %d", w.Code)
	}
}

func TestGroupAndProxyWrites(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "public", "org/example/app/3.0/app-3.0.jar", "g"); w.Code != 201 {
		t.Fatalf("group PUT: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "releases", "org/example/app/3.0/app-3.0.jar", ""); w.Body.String() != "g" {
		t.Fatalf("group PUT should land in writeMember: %q", w.Body)
	}
	if w := e.do("PUT", "readonly-group", "org/example/app/3.0/app-3.0.jar", "g"); w.Code != 405 {
		t.Fatalf("group without writeMember: %d", w.Code)
	}
	if w := e.do("PUT", "central", "org/example/app/3.0/app-3.0.jar", "g"); w.Code != 405 {
		t.Fatalf("proxy PUT: %d", w.Code)
	}
	if w := e.do("POST", "releases", "x/y/1/y-1.jar", "g"); w.Code != 405 {
		t.Fatalf("POST: %d", w.Code)
	}
}

func configRepo(name, writePolicy string) config.Repository {
	return config.Repository{Name: name, Format: "maven", Type: "hosted", VersionPolicy: "mixed", WritePolicy: writePolicy}
}

func TestPutMetadataOverGlobalCapIs413(t *testing.T) {
	e := newEnv(t)
	doc := "<metadata><groupId>g</groupId><artifactId>a</artifactId>" + strings.Repeat("<!-- x -->", 100) + "</metadata>"
	w := httptest.NewRecorder()
	body := http.MaxBytesReader(w, io.NopCloser(strings.NewReader(doc)), 100)
	err := e.h.putMetadata(context.Background(), e.repos["releases"], "org/example/app/maven-metadata.xml", body)
	if repo.StatusOf(err) != 413 {
		t.Fatalf("err = %v (status %d), want 413", err, repo.StatusOf(err))
	}
}

func TestDeleteLastArtifactsDropsVersionFromGAMetadata(t *testing.T) {
	e := newEnv(t)
	e.do("PUT", "releases", "org/example/app/1.0/app-1.0.jar", "j1")
	e.do("PUT", "releases", "org/example/app/1.0/app-1.0.pom", "p1")
	e.do("PUT", "releases", "org/example/app/2.0/app-2.0.pom", "p2")
	for _, f := range []string{"app-1.0.jar", "app-1.0.jar.sha1", "app-1.0.pom"} {
		if w := e.do("DELETE", "releases", "org/example/app/1.0/"+f, ""); w.Code != 204 {
			t.Fatalf("DELETE %s: %d", f, w.Code)
		}
	}
	if got := versionsOf(t, e, "releases"); got != "2.0 release=2.0" {
		t.Fatalf("after deleting 1.0: %s", got)
	}
}

func TestDeleteLastSnapshotFilesRemovesVersionMetadata(t *testing.T) {
	e := newEnv(t)
	base := "org/example/app/1.0-SNAPSHOT/"
	e.putAll(t, "snapshots", base+"app-1.0-20200101.000000-1.jar", base+"app-1.0-20200101.000000-1.pom")
	e.do("DELETE", "snapshots", base+"app-1.0-20200101.000000-1.jar", "")
	if w := e.do("GET", "snapshots", base+"maven-metadata.xml", ""); w.Code != 200 {
		t.Fatalf("version metadata must stay while artifacts remain: %d", w.Code)
	}
	e.do("DELETE", "snapshots", base+"app-1.0-20200101.000000-1.pom", "")
	if w := e.do("GET", "snapshots", base+"maven-metadata.xml", ""); w.Code != 404 {
		t.Fatalf("version metadata must be gone: %d", w.Code)
	}
	if w := e.do("GET", "snapshots", "org/example/app/maven-metadata.xml", ""); w.Code != 404 {
		t.Fatalf("GA metadata must be gone: %d", w.Code)
	}
}

// snapshotMeta reads the version-level maven-metadata.xml of verDir.
func snapshotMeta(t *testing.T, e *env, repoName, verDir string) *Metadata {
	t.Helper()
	w := e.do("GET", repoName, verDir+"/maven-metadata.xml", "")
	if w.Code != 200 {
		t.Fatalf("%s version metadata: %d %s", verDir, w.Code, w.Body)
	}
	m, err := ParseMetadata(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	if m.Versioning == nil || m.Versioning.Snapshot == nil {
		t.Fatalf("%s: no <snapshot>: %+v", verDir, m)
	}
	return m
}

// snapshotValues lists the <snapshotVersions> as classifier:extension=value.
func snapshotValues(m *Metadata) string {
	var out []string
	for _, sv := range m.Versioning.SnapshotVersions {
		out = append(out, sv.Classifier+":"+sv.Extension+"="+sv.Value)
	}
	return strings.Join(out, ",")
}

func clientVersionMeta(verDir, ts string, build int) string {
	return fmt.Sprintf(`<metadata><groupId>org.example</groupId><artifactId>app</artifactId><version>%s</version>
<versioning><snapshot><timestamp>%s</timestamp><buildNumber>%d</buildNumber></snapshot><lastUpdated>20991231000000</lastUpdated>
<snapshotVersions><snapshotVersion><extension>jar</extension><value>bogus-%d</value><updated>20991231000000</updated></snapshotVersion></snapshotVersions></versioning></metadata>`,
		verDir[strings.LastIndexByte(verDir, '/')+1:], ts, build, build)
}

// M-15 repro 1: deleting the newest build moves the version metadata back.
func TestSnapshotMetadataAfterDeleteOfLatestBuild(t *testing.T) {
	e := newEnv(t)
	verDir := "org/example/app/1.1.0-SNAPSHOT"
	for b := 1; b <= 3; b++ {
		ts := fmt.Sprintf("20261022.01000%d", b)
		e.putAll(t, "snapshots",
			fmt.Sprintf("%s/app-1.1.0-%s-%d.jar", verDir, ts, b),
			fmt.Sprintf("%s/app-1.1.0-%s-%d.pom", verDir, ts, b))
		// mvn deploy uploads its own version document after the files
		if w := e.do("PUT", "snapshots", verDir+"/maven-metadata.xml", clientVersionMeta(verDir, ts, b)); w.Code != 201 {
			t.Fatalf("client metadata PUT: %d %s", w.Code, w.Body)
		}
	}
	m := snapshotMeta(t, e, "snapshots", verDir)
	if m.Versioning.Snapshot.BuildNumber != 3 || m.Versioning.Snapshot.Timestamp != "20261022.010003" {
		t.Fatalf("before delete: %+v", m.Versioning.Snapshot)
	}
	for _, f := range []string{"app-1.1.0-20261022.010003-3.jar", "app-1.1.0-20261022.010003-3.pom"} {
		if w := e.do("DELETE", "snapshots", verDir+"/"+f, ""); w.Code != 204 {
			t.Fatalf("DELETE %s: %d", f, w.Code)
		}
	}
	m = snapshotMeta(t, e, "snapshots", verDir)
	if m.Version != "1.1.0-SNAPSHOT" || m.GroupID != "org.example" || m.ArtifactID != "app" {
		t.Fatalf("header: %+v", m)
	}
	if m.Versioning.Snapshot.BuildNumber != 2 || m.Versioning.Snapshot.Timestamp != "20261022.010002" {
		t.Fatalf("after delete: %+v", m.Versioning.Snapshot)
	}
	if got := snapshotValues(m); got != ":jar=1.1.0-20261022.010002-2,:pom=1.1.0-20261022.010002-2" {
		t.Fatalf("snapshotVersions: %s", got)
	}
}

// M-15 repro 2: plain PUTs of timestamped files (no client metadata).
func TestSnapshotMetadataCreatedForRawPuts(t *testing.T) {
	e := newEnv(t)
	verDir := "org/example/app/2.0-SNAPSHOT"
	e.putAll(t, "snapshots", verDir+"/app-2.0-20261022.020000-1.jar", verDir+"/app-2.0-20261022.020000-1.pom",
		verDir+"/app-2.0-20261022.020000-1-sources.jar")
	m := snapshotMeta(t, e, "snapshots", verDir)
	if m.Versioning.Snapshot.BuildNumber != 1 || m.Versioning.Snapshot.Timestamp != "20261022.020000" || m.Versioning.LastUpdated == "" {
		t.Fatalf("snapshot: %+v", m.Versioning)
	}
	if got := snapshotValues(m); got != ":jar=2.0-20261022.020000-1,:pom=2.0-20261022.020000-1,sources:jar=2.0-20261022.020000-1" {
		t.Fatalf("snapshotVersions: %s", got)
	}
	for _, sv := range m.Versioning.SnapshotVersions {
		if sv.Updated != "20261022020000" {
			t.Fatalf("updated: %+v", sv)
		}
	}
}

// Decision 6 / L-11: a client version document is accepted but not stored.
func TestClientVersionMetadataIgnored(t *testing.T) {
	e := newEnv(t)
	verDir := "org/example/app/3.0-SNAPSHOT"
	e.putAll(t, "snapshots", verDir+"/app-3.0-20261022.030000-1.jar")
	if w := e.do("PUT", "snapshots", verDir+"/maven-metadata.xml", clientVersionMeta(verDir, "20991231.000000", 9)); w.Code != 201 {
		t.Fatalf("client metadata PUT: %d %s", w.Code, w.Body)
	}
	m := snapshotMeta(t, e, "snapshots", verDir)
	if m.Versioning.Snapshot.BuildNumber != 1 || strings.Contains(snapshotValues(m), "bogus") {
		t.Fatalf("client document leaked: %+v %s", m.Versioning.Snapshot, snapshotValues(m))
	}
	// Without any timestamped file there is no server document to serve.
	other := "org/example/app/4.0-SNAPSHOT"
	if w := e.do("PUT", "snapshots", other+"/maven-metadata.xml", clientVersionMeta(other, "20991231.000000", 9)); w.Code != 201 {
		t.Fatalf("client metadata PUT: %d", w.Code)
	}
	if w := e.do("GET", "snapshots", other+"/maven-metadata.xml", ""); w.Code != 404 {
		t.Fatalf("client-only version metadata: %d %s", w.Code, w.Body)
	}
}

// L-14: a timestamped snapshot version is not a release.
func TestReleaseRepoRejectsTimestampedSnapshot(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{
		"org/example/app/1.4-20260101.000000-1/app-1.4-20260101.000000-1.jar",
		"org/example/app/1.4-20260101.000000-1/maven-metadata.xml",
	} {
		if w := e.do("PUT", "releases", p, "<metadata/>"); w.Code != 400 {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	if w := e.do("PUT", "releases", "org/example/app/1.4/app-1.4.jar", "ok"); w.Code != 201 {
		t.Fatalf("plain release: %d", w.Code)
	}
}

// doRaw is e.do for paths that would not survive URL parsing; the handler
// takes the repository path as an argument.
func (e *env) doRaw(method, repoName, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repoName+"/raw", strings.NewReader(body))
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

// L-13 applies to new uploads only: assets stored by older builds under
// such names stay readable, deletable and part of the metadata.
func TestLegacyOddNamesStillServed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.putAll(t, "releases", "org/example/app/1.0/app-1.0.jar")
	legacy := []string{
		"org/example/app/2.0/app-2.0.jar;jsessionid=1",
		"org/example/app/3.0/app-3.0.jar\x00.pom",
		"org/example/app/3.0/app-3.0.jar#frag",
	}
	for _, p := range legacy {
		e.seed(t, "releases", p, "legacy")
	}
	for _, p := range legacy {
		if w := e.doRaw("GET", "releases", p, ""); w.Code != 200 || w.Body.String() != "legacy" {
			t.Fatalf("GET %q: %d %q", p, w.Code, w.Body)
		}
		if w := e.doRaw("HEAD", "releases", p, ""); w.Code != 200 {
			t.Fatalf("HEAD %q: %d", p, w.Code)
		}
	}
	if _, err := e.h.RebuildMetadata(ctx, e.repos["releases"], 0, nil); err != nil {
		t.Fatal(err)
	}
	if got := versionsOf(t, e, "releases"); got != "1.0,2.0,3.0 release=3.0" {
		t.Fatalf("rebuild must count legacy files: %s", got)
	}
	if w := e.doRaw("DELETE", "releases", legacy[0], ""); w.Code != 204 {
		t.Fatalf("DELETE %q: %d %s", legacy[0], w.Code, w.Body)
	}
	if got := versionsOf(t, e, "releases"); got != "1.0,3.0 release=3.0" {
		t.Fatalf("after legacy delete: %s", got)
	}
	for _, p := range []string{"org/example/app/4.0/app-4.0.jar;x=1", "org/example/app/4.0/app-4.0.jar\x00.pom", "org/example/app/4.0/app-4.0.jar?q=1"} {
		if w := e.doRaw("PUT", "releases", p, "new"); w.Code != 400 {
			t.Fatalf("PUT %q: %d", p, w.Code)
		}
	}
}
