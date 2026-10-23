package maven

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/component"
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
