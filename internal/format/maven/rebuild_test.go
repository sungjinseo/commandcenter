package maven

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
)

func TestRebuildMetadata(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["releases"]
	e.putAll(t, "releases", "org/example/app/1.0/app-1.0.jar", "org/example/app/2.0/app-2.0.jar", "org/example/lib/1.0/lib-1.0.jar")
	// The GA metadata of app is lost, lib's is stale (claims 9.9), and an
	// orphan document is left for a GA without artifacts.
	if err := e.h.Store.Delete(ctx, "releases", "org/example/app/maven-metadata.xml"); err != nil {
		t.Fatal(err)
	}
	stale := `<metadata><groupId>org.example</groupId><artifactId>lib</artifactId><versioning><versions><version>9.9</version></versions></versioning></metadata>`
	e.seed(t, "releases", "org/example/lib/maven-metadata.xml", stale)
	orphan := `<metadata><groupId>org.gone</groupId><artifactId>old</artifactId><versioning><versions><version>1.0</version></versions></versioning></metadata>`
	e.seed(t, "releases", "org/gone/old/maven-metadata.xml", orphan)
	e.seed(t, "releases", "readme.txt", "not maven")

	var lines []string
	res, err := e.h.RebuildMetadata(ctx, rp, 0, func(f string, a ...any) { lines = append(lines, f) })
	if err != nil {
		t.Fatal(err)
	}
	if res.GAs != 2 || res.Removed != 1 || res.Truncated || res.Failed != 0 {
		t.Fatalf("result: %+v", res)
	}
	if got := versionsOf(t, e, "releases"); got != "1.0,2.0 release=2.0" {
		t.Fatalf("app metadata: %s", got)
	}
	w := e.do("GET", "releases", "org/example/lib/maven-metadata.xml", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "9.9") || !strings.Contains(w.Body.String(), "<version>1.0</version>") {
		t.Fatalf("lib metadata: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "releases", "org/gone/old/maven-metadata.xml", ""); w.Code != 404 {
		t.Fatalf("orphan metadata: %d", w.Code)
	}
}

func TestRebuildMetadataBounded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.putAll(t, "releases", "org/example/app/1.0/app-1.0.jar", "org/example/lib/1.0/lib-1.0.jar")
	// 4 assets (2 jars, 2 metadata): a budget of 2 reads only app's.
	res, err := e.h.RebuildMetadata(ctx, e.repos["releases"], 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.GAs != 1 {
		t.Fatalf("result: %+v", res)
	}
}

func TestRebuildMetadataRefusesOtherRepos(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"central", "public"} {
		if _, err := e.h.RebuildMetadata(context.Background(), e.repos[name], 0, nil); !errors.Is(err, component.ErrNotSupported) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// M-15: rebuild-metadata regenerates SNAPSHOT version documents too.
func TestRebuildFixesSnapshotMetadata(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	verDir := "org/example/app/1.1.0-SNAPSHOT"
	e.putAll(t, "snapshots", verDir+"/app-1.1.0-20261022.010001-1.jar", verDir+"/app-1.1.0-20261022.010002-2.jar")
	// Stale (claims build 3), missing, and orphan version documents.
	stale := `<metadata><groupId>org.example</groupId><artifactId>app</artifactId><version>1.1.0-SNAPSHOT</version>
<versioning><snapshot><timestamp>20261022.010003</timestamp><buildNumber>3</buildNumber></snapshot><lastUpdated>20261022010003</lastUpdated></versioning></metadata>`
	e.seed(t, "snapshots", verDir+"/maven-metadata.xml", stale)
	missing := "org/example/lib/2.0-SNAPSHOT"
	e.putAll(t, "snapshots", missing+"/lib-2.0-20261022.020000-1.pom")
	if err := e.h.Store.Delete(ctx, "snapshots", missing+"/maven-metadata.xml"); err != nil {
		t.Fatal(err)
	}
	orphan := "org/example/app/0.9-SNAPSHOT/maven-metadata.xml"
	e.seed(t, "snapshots", orphan, stale)

	res, err := e.h.RebuildMetadata(ctx, e.repos["snapshots"], 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Snapshots != 3 || res.Failed != 0 {
		t.Fatalf("result: %+v", res)
	}
	if m := snapshotMeta(t, e, "snapshots", verDir); m.Versioning.Snapshot.BuildNumber != 2 || snapshotValues(m) != ":jar=1.1.0-20261022.010002-2" {
		t.Fatalf("stale document not fixed: %+v %s", m.Versioning.Snapshot, snapshotValues(m))
	}
	if m := snapshotMeta(t, e, "snapshots", missing); m.Versioning.Snapshot.BuildNumber != 1 {
		t.Fatalf("missing document: %+v", m.Versioning.Snapshot)
	}
	if w := e.do("GET", "snapshots", orphan, ""); w.Code != 404 {
		t.Fatalf("orphan version document: %d", w.Code)
	}
}

// A rebuild that changes nothing but <lastUpdated> keeps the stored
// version document and its Last-Modified.
func TestSnapshotMetadataUnchangedIsKept(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	e.h.Now, e.h.Store.Now = clock, clock
	verDir := "org/example/app/1.1.0-SNAPSHOT"
	e.putAll(t, "snapshots", verDir+"/app-1.1.0-20261022.010001-1.jar", verDir+"/app-1.1.0-20261022.010001-1.pom")
	before, err := e.h.Store.Get(ctx, "snapshots", verDir+"/maven-metadata.xml")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if w := e.do("PUT", "snapshots", verDir+"/app-1.1.0-20261022.010001-1.jar", "again"); w.Code != 201 {
		t.Fatalf("redeploy: %d", w.Code)
	}
	if _, err := e.h.RebuildMetadata(ctx, e.repos["snapshots"], 0, nil); err != nil {
		t.Fatal(err)
	}
	after, err := e.h.Store.Get(ctx, "snapshots", verDir+"/maven-metadata.xml")
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastModified.Equal(before.LastModified) || after.BlobSHA256 != before.BlobSHA256 {
		t.Fatalf("unchanged document rewritten: %v -> %v", before.LastModified, after.LastModified)
	}
	// A real change (build 2) is stored with the new time.
	e.putAll(t, "snapshots", verDir+"/app-1.1.0-20261022.020000-2.jar")
	changed, _ := e.h.Store.Get(ctx, "snapshots", verDir+"/maven-metadata.xml")
	if !changed.LastModified.After(before.LastModified) {
		t.Fatalf("changed document not re-stored: %v", changed.LastModified)
	}
}

// The metadata rebuilds after an artifact PUT (GA and SNAPSHOT version) and
// after the GA maven-metadata.xml upload that ends mvn deploy stream the
// paths: a SNAPSHOT that CI has deployed to for years with no cleanup policy
// holds 10^5 files, whose asset rows held some 94 MiB at the shipped
// GOMEMLIMIT. The 30000 here (28 MiB that way, 3 MiB streamed) keep the test
// quick under -race.
func TestRebuildMetadataMemoryBounded(t *testing.T) {
	if raceEnabled {
		t.Skip("heap measurements are not meaningful under the race detector's instrumentation")
	}
	e := newEnv(t)
	ctx := context.Background()
	sums, err := e.h.Store.WriteBlob("snapshots", strings.NewReader("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	const builds = 6000 // x 5 files, all in 1.0-SNAPSHOT
	err = e.h.Store.DB.Tx(ctx, func(tx *meta.Tx) error {
		for b := range builds {
			base := fmt.Sprintf("org/example/app/1.0-SNAPSHOT/app-1.0-20260101.%06d-%d", b, b+1)
			for _, f := range []string{".pom", ".jar", "-sources.jar", "-javadoc.jar", ".module"} {
				if err := tx.PutAsset(ctx, e.h.Store.NewAsset("snapshots", base+f, "application/octet-stream", blob.Sums(sums))); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	defer debug.SetMemoryLimit(debug.SetMemoryLimit(80 << 20)) // the shipped GOMEMLIMIT
	for _, put := range [][2]string{
		{"org/example/app/maven-metadata.xml", `<metadata><groupId>org.example</groupId><artifactId>app</artifactId></metadata>`},
		{"org/example/app/1.0-SNAPSHOT/app-1.0-20260102.000000-1.jar", "jar"},
	} {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		var peak atomic.Uint64
		stop, sampled := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(sampled)
			for {
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				if ms.HeapInuse > peak.Load() {
					peak.Store(ms.HeapInuse)
				}
				select {
				case <-stop:
					return
				case <-time.After(time.Millisecond):
				}
			}
		}()
		start := time.Now()
		w := e.do("PUT", "snapshots", put[0], put[1])
		took := time.Since(start)
		close(stop)
		<-sampled
		grew := (max(peak.Load(), before.HeapInuse) - before.HeapInuse) >> 20
		t.Logf("PUT %s: %d in %v, heap in use grew by %d MiB", put[0], w.Code, took, grew)
		if w.Code != 201 || grew > 16 {
			t.Fatalf("PUT %s: %d %s, held %d MiB of heap", put[0], w.Code, w.Body, grew)
		}
	}
	if got := versionsOf(t, e, "snapshots"); got != "1.0-SNAPSHOT release=" {
		t.Fatalf("versions: %s", got)
	}
	if w := e.do("GET", "snapshots", "org/example/app/1.0-SNAPSHOT/maven-metadata.xml", ""); !strings.Contains(w.Body.String(), "<buildNumber>6000</buildNumber>") {
		t.Fatalf("version metadata: %d %s", w.Code, w.Body)
	}
}

// QA L-12: a purge deletes what older builds cached in a proxy at paths that
// are not Maven files, and keeps every Maven file even when it is text/html.
func TestPurgeUnfetchable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	defer func(n int) { rebuildPage = n }(rebuildPage)
	rebuildPage = 2 // several pages, deleted from while they are read
	gone := []string{".index/nexus-maven-repository-index.gz", ".index/nexus-maven-repository-index.properties",
		"org/up/lib/", "org/up/lib/1.0", "org/up/lib/1.0/"}
	kept := []string{".meta/prefixes.txt", "archetype-catalog.xml", "com/eed3si9n/sbt-assembly_2.12_1.0/2.1.1/sbt-assembly-2.1.1.jar",
		"org/up/lib/1.0/lib-1.0-report.html", "org/up/lib/1.0/lib-1.0.jar", "org/up/lib/1.0/lib-1.0.jar.asc",
		"org/up/lib/3.0/lib-3.0.jar", "org/up/lib/maven-metadata.xml"}
	for _, p := range append(gone, kept...) {
		if _, err := e.h.Store.Put(ctx, "central", p, "text/html", strings.NewReader("<html>"+p+"</html>")); err != nil {
			t.Fatal(err)
		}
	}
	e.seed(t, "releases", "org/up/lib/1.0", "not maven")
	var logged []string
	n, err := e.h.PurgeUnfetchable(ctx, e.repos["central"], func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
	if err != nil || n != len(gone) {
		t.Fatalf("purge: %d %v", n, err)
	}
	if got, want := assetPaths(t, e, "central", ""), strings.Join(kept, ","); got != want {
		t.Fatalf("left %s\nwant %s", got, want)
	}
	if len(logged) != len(gone) || logged[2] != `removed "org/up/lib/"` {
		t.Fatalf("log: %q", logged)
	}
	for _, name := range []string{"releases", "public"} {
		if _, err := e.h.PurgeUnfetchable(ctx, e.repos[name], nil); !errors.Is(err, component.ErrNotSupported) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := assetPaths(t, e, "releases", "org/up/"); got != "org/up/lib/1.0" {
		t.Fatalf("hosted: %s", got)
	}
}

// GA org:example and the plugins of groupId org.example share
// org/example/maven-metadata.xml. Rebuilding the GA's versioning from
// scratch, by rebuild-metadata, a DELETE or a component delete, keeps the
// <plugins>, also when the GA's last version goes.
func TestRebuildKeepsPluginsBesideGA(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const path = "org/example/maven-metadata.xml"
	plugin := `<metadata><plugins><plugin><name>a</name><prefix>a</prefix><artifactId>a-maven-plugin</artifactId></plugin></plugins></metadata>`
	if w := e.do("PUT", "releases", path, plugin); w.Code != 201 {
		t.Fatalf("PUT plugin-group metadata: %d %s", w.Code, w.Body)
	}
	e.putAll(t, "releases", "org/example/1.0/example-1.0.pom", "org/example/2.0/example-2.0.pom", "org/example/3.0/example-3.0.pom")
	doc := func() string {
		t.Helper()
		w := e.do("GET", "releases", path, "")
		m, err := ParseMetadata(w.Body)
		if w.Code != 200 || err != nil {
			t.Fatalf("metadata: %d %v %s", w.Code, err, w.Body)
		}
		var out []string
		for _, p := range m.Plugins {
			out = append(out, p.Prefix+"="+p.ArtifactID)
		}
		if m.Versioning != nil {
			out = append(out, m.GroupID+":"+m.ArtifactID+" "+strings.Join(m.Versioning.Versions, ","))
		}
		return strings.Join(out, " ")
	}
	if _, err := e.h.RebuildMetadata(ctx, e.repos["releases"], 0, nil); err != nil {
		t.Fatal(err)
	}
	if got := doc(); got != "a=a-maven-plugin org:example 1.0,2.0,3.0" {
		t.Fatalf("after rebuild-metadata: %s", got)
	}
	if w := e.do("DELETE", "releases", "org/example/3.0/example-3.0.pom", ""); w.Code != 204 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body)
	}
	if got := doc(); got != "a=a-maven-plugin org:example 1.0,2.0" {
		t.Fatalf("after a DELETE: %s", got)
	}
	c := component.Component{Format: "maven", Group: "org", Name: "example", Version: "2.0"}
	if _, err := e.h.DeleteComponent(ctx, e.repos["releases"], c, "system"); err != nil {
		t.Fatal(err)
	}
	if got := doc(); got != "a=a-maven-plugin org:example 1.0" {
		t.Fatalf("after a component delete: %s", got)
	}
	if w := e.do("DELETE", "releases", "org/example/1.0/example-1.0.pom", ""); w.Code != 204 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body)
	}
	if got := doc(); got != "a=a-maven-plugin" {
		t.Fatalf("after deleting the last version: %s", got)
	}
}

// An orphan GA document loses its versions whatever groupId and artifactId
// it names (older builds stored uploads as sent, so one may name no GA or
// another), also inside the directory of another GA. A plugin-group
// document keeps its <plugins>.
func TestRebuildRemovesOrphanVersions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.putAll(t, "releases", "org/example/1.0/example-1.0.pom", "org/example.app/1.0/example.app-1.0.pom",
		"org/plugins/p-maven-plugin/1.0/p-maven-plugin-1.0.jar")
	versions := `<versioning><versions><version>9.9</version></versions></versioning>`
	orphans := map[string]string{
		"org/example/app": `<metadata><groupId>org.example</groupId><artifactId>app</artifactId>` + versions + `</metadata>`, // under GA org:example
		"org/plain/lib":   `<metadata>` + versions + `</metadata>`,
		"org/foreign/lib": `<metadata><groupId>evil</groupId><artifactId>other</artifactId>` + versions + `</metadata>`,
		// The entry of p-maven-plugin, which has a version, stays; the one
		// of gone-maven-plugin, which has none, goes (maven-15).
		"org/plugins": `<metadata><plugins><plugin><prefix>p</prefix><artifactId>p-maven-plugin</artifactId></plugin>` +
			`<plugin><prefix>gone</prefix><artifactId>gone-maven-plugin</artifactId></plugin></plugins>` + versions + `</metadata>`,
	}
	for d, doc := range orphans {
		e.seed(t, "releases", d+"/maven-metadata.xml", doc)
	}
	// No GA has the directory org/example.app/1.0 (groupId org.example.app is
	// org/example/app): this document is not a GA's and stays.
	const notGA = "org/example.app/1.0/maven-metadata.xml"
	e.seed(t, "releases", notGA, `<metadata><groupId>org</groupId><artifactId>example.app</artifactId><version>1.0</version>`+versions+`</metadata>`)

	res, err := e.h.RebuildMetadata(ctx, e.repos["releases"], 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.GAs != 3 || res.Removed != len(orphans) || res.Failed != 0 {
		t.Fatalf("result: %+v", res)
	}
	for d := range orphans {
		w := e.do("GET", "releases", d+"/maven-metadata.xml", "")
		if d != "org/plugins" {
			if w.Code != 404 {
				t.Errorf("%s: %d %s", d, w.Code, w.Body)
			}
			continue
		}
		m, err := ParseMetadata(w.Body)
		if w.Code != 200 || err != nil || m.Versioning != nil || len(m.Plugins) != 1 || m.Plugins[0].Prefix != "p" {
			t.Errorf("%s: %d %v %s", d, w.Code, err, w.Body)
		}
	}
	if w := e.do("GET", "releases", notGA, ""); w.Code != 200 {
		t.Errorf("%s: %d", notGA, w.Code)
	}
	if w := e.do("GET", "releases", "org/example/maven-metadata.xml", ""); w.Code != 200 || strings.Contains(w.Body.String(), "9.9") {
		t.Errorf("GA org:example: %d %s", w.Code, w.Body)
	}
}
