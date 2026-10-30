package meta

import (
	"reflect"
	"strings"
	"testing"

	"commandcenter/internal/config"
)

// The upgrade to schema 8 copies a tag of a Maven X-SNAPSHOT component to
// each timestamped build stored under it (a build is a component of its own
// now) and stamps the existing maven.purge-snapshots tasks with the scope
// they were made for, once: a later upgrade leaves untagged builds alone.
func TestUpgrade8MavenBuilds(t *testing.T) {
	db, target := openTestDB(t)
	for _, r := range []config.Repository{{Name: "snaps", Format: "maven", Type: "hosted"}, {Name: "npm", Format: "npm", Type: "hosted"}} {
		if err := db.CreateRepository(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	dir := "org/example/app/1.0-SNAPSHOT/"
	for _, p := range []string{dir + "app-1.0-20261014.120000-1.jar", dir + "app-1.0-20261014.120000-1.pom", dir + "app-1.0-20261015.120000-2-sources.jar",
		dir + "app-1.0-SNAPSHOT.jar", dir + "maven-metadata.xml", dir + "other-1.0-20261014.120000-1.jar", dir + "sub/app-1.0-20261016.120000-3.jar",
		"org/example/app/1.0-SNAPSHOTX/app-1.0-20261017.120000-4.jar"} {
		if err := db.PutAsset(ctx, Asset{Repo: "snaps", Path: p, BlobSHA256: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.CreateTag(ctx, "rc", `{}`); err != nil {
		t.Fatal(err)
	}
	if err := db.AssociateTag(ctx, "rc", []ComponentKey{{Repo: "snaps", Group: "org.example", Name: "app", Version: "1.0-SNAPSHOT"},
		{Repo: "npm", Group: "org.example", Name: "app", Version: "1.0-SNAPSHOT"}}); err != nil {
		t.Fatal(err)
	}
	for id, params := range map[string]string{"old": `{"repository":"snaps","minimumRetained":1,"snapshotRetentionDays":30}`,
		"new": `{"repository":"snaps","scope":"build","minimumRetained":1,"snapshotRetentionDays":30}`} {
		if err := db.CreateTask(ctx, Task{ID: id, Name: id, Type: "maven.purge-snapshots", Schedule: "manual", Params: params}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	raw := rawOpen(t, target)
	if err := stampSchemaVersion(ctx, raw, 7, 6); err != nil {
		t.Fatal(err)
	}
	reopen := func() *DB {
		t.Helper()
		db, err := Open(target)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	db = reopen()
	keys, err := db.TagMembers(ctx, "rc")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range keys {
		got = append(got, k.Repo+":"+k.Version)
	}
	want := []string{"npm:1.0-SNAPSHOT", "snaps:1.0-20261014.120000-1", "snaps:1.0-20261015.120000-2", "snaps:1.0-SNAPSHOT"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tagged: %q, want %q", got, want)
	}
	for id, want := range map[string]string{"old": `"scope": "version"`, "new": `"scope":"build"`} {
		tk, err := db.GetTask(ctx, id)
		if err != nil || !strings.Contains(tk.Params, want) {
			t.Fatalf("task %s: %s %v", id, tk.Params, err)
		}
	}
	// Once: an untagged build stays untagged when a later upgrade runs.
	if err := db.DissociateTag(ctx, "rc", []ComponentKey{{Repo: "snaps", Group: "org.example", Name: "app", Version: "1.0-20261014.120000-1"}}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := stampSchemaVersion(ctx, raw, 8, 7); err != nil {
		t.Fatal(err)
	}
	db = reopen()
	if keys, _ := db.TagMembers(ctx, "rc"); len(keys) != 3 {
		t.Fatalf("after a second open: %+v", keys)
	}
}
