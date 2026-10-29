package meta

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/config"
)

func TestTags(t *testing.T) {
	db := openTest(t)
	if _, err := db.CreateTag(ctx, "rc-1", `{"jvm":"17"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTag(ctx, "rc-1", `{}`); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	db.CreateTag(ctx, "rc-2", `{}`)
	got, err := db.GetTag(ctx, "rc-1")
	if err != nil || got.Attributes != `{"jvm":"17"}` || got.Created.IsZero() || !got.Created.Equal(got.Updated) {
		t.Fatalf("get: %+v %v", got, err)
	}
	list, err := db.ListTags(ctx, "", 1)
	if err != nil || len(list) != 1 || list[0].Name != "rc-1" {
		t.Fatalf("page 1: %+v %v", list, err)
	}
	if list, _ = db.ListTags(ctx, "rc-1", 10); len(list) != 1 || list[0].Name != "rc-2" {
		t.Fatalf("page 2: %+v", list)
	}
	time.Sleep(2 * time.Millisecond)
	if up, err := db.UpdateTag(ctx, "rc-1", `{"jvm":"21"}`); err != nil || up.Attributes != `{"jvm":"21"}` || !up.Updated.After(up.Created) {
		t.Fatalf("update: %+v %v", up, err)
	}
	if _, err := db.UpdateTag(ctx, "nope", `{}`); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}

	a := ComponentKey{"maven-staging", "org.example", "app", "1.0"}
	b := ComponentKey{"maven-staging", "org.example", "app", "1.1"}
	if err := db.AssociateTag(ctx, "nope", []ComponentKey{a}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("associate unknown tag: %v", err)
	}
	if err := db.AssociateTag(ctx, "rc-1", []ComponentKey{a, b, a}); err != nil {
		t.Fatal(err)
	}
	db.AssociateTag(ctx, "rc-2", []ComponentKey{a})
	if m, err := db.TagMembers(ctx, "rc-1"); err != nil || !reflect.DeepEqual(m, []ComponentKey{a, b}) {
		t.Fatalf("members: %v %v", m, err)
	}
	if tags, err := db.ComponentTags(ctx, a); err != nil || !reflect.DeepEqual(tags, []string{"rc-1", "rc-2"}) {
		t.Fatalf("component tags: %v %v", tags, err)
	}
	if err := db.DissociateTag(ctx, "rc-1", []ComponentKey{b}); err != nil {
		t.Fatal(err)
	}
	if m, _ := db.TagMembers(ctx, "rc-1"); !reflect.DeepEqual(m, []ComponentKey{a}) {
		t.Fatalf("after dissociate: %v", m)
	}
	// A staging move hands the associations over; a delete drops them.
	if err := db.MoveComponentTags(ctx, a, "maven-releases"); err != nil {
		t.Fatal(err)
	}
	moved := ComponentKey{"maven-releases", "org.example", "app", "1.0"}
	if tags, _ := db.ComponentTags(ctx, a); len(tags) != 0 {
		t.Fatalf("source keeps tags: %v", tags)
	}
	if tags, _ := db.ComponentTags(ctx, moved); !reflect.DeepEqual(tags, []string{"rc-1", "rc-2"}) {
		t.Fatalf("moved tags: %v", tags)
	}
	if err := db.DropComponentTags(ctx, moved); err != nil {
		t.Fatal(err)
	}
	if m, _ := db.TagMembers(ctx, "rc-2"); len(m) != 0 {
		t.Fatalf("dropped: %v", m)
	}
	db.AssociateTag(ctx, "rc-2", []ComponentKey{a})
	if err := db.DeleteTag(ctx, "rc-2"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteTag(ctx, "rc-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete again: %v", err)
	}
	if tags, _ := db.ComponentTags(ctx, a); len(tags) != 0 {
		t.Fatalf("a deleted tag keeps associations: %v", tags)
	}
}

func TestTagsPurgedWithRepository(t *testing.T) {
	db := openTest(t)
	if err := db.CreateRepository(ctx, config.Repository{Name: "raw-a", Format: "raw", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	db.CreateTag(ctx, "t", `{}`)
	db.AssociateTag(ctx, "t", []ComponentKey{{"raw-a", "", "a.txt", ""}})
	db.PutBlobStoreMove(ctx, BlobStoreMove{Repo: "raw-a", Source: "default", Target: "s2", Started: time.Now()})
	if _, err := db.DeleteRepository(ctx, "raw-a"); err != nil {
		t.Fatal(err)
	}
	if m, _ := db.TagMembers(ctx, "t"); len(m) != 0 {
		t.Fatalf("associations of a deleted repository: %v", m)
	}
	if moves, _ := db.ListBlobStoreMoves(ctx); len(moves) != 0 {
		t.Fatalf("moves of a deleted repository: %v", moves)
	}
}

func TestBlobStoreMoveSwitch(t *testing.T) {
	db := openTest(t)
	def := config.Repository{Name: "raw-a", Format: "raw", Type: "hosted", BlobStore: "s1", WritePolicy: "allow_once"}
	if err := db.CreateRepository(ctx, def); err != nil {
		t.Fatal(err)
	}
	m := BlobStoreMove{Repo: "raw-a", Source: "s1", Target: "s2", Started: time.UnixMilli(1700000000000).UTC()}
	if err := db.PutBlobStoreMove(ctx, m); err != nil {
		t.Fatal(err)
	}
	if moves, err := db.ListBlobStoreMoves(ctx); err != nil || len(moves) != 1 || moves[0] != m {
		t.Fatalf("moves: %+v %v", moves, err)
	}
	db.PutAsset(ctx, Asset{Repo: "raw-a", Path: "a", BlobSHA256: strings.Repeat("a", 64), Size: 3})
	db.PutAsset(ctx, Asset{Repo: "raw-a", Path: "b", BlobSHA256: strings.Repeat("a", 64), Size: 3})
	db.PutAsset(ctx, Asset{Repo: "raw-a", Path: "c", BlobSHA256: strings.Repeat("b", 64), Size: 5})
	if blobs, err := db.RepoBlobs(ctx, "raw-a"); err != nil || len(blobs) != 2 || blobs[1].Size != 5 {
		t.Fatalf("blobs: %+v %v", blobs, err)
	}
	// A failing check changes nothing.
	stop := errors.New("not yet")
	if err := db.SwitchBlobStore(ctx, "raw-a", "s2", func(tx *Tx) error {
		blobs, err := tx.RepoBlobs(ctx, "raw-a")
		if err != nil || len(blobs) != 2 {
			t.Errorf("blobs in tx: %v %v", blobs, err)
		}
		return stop
	}); !errors.Is(err, stop) {
		t.Fatalf("switch: %v", err)
	}
	if moves, _ := db.ListBlobStoreMoves(ctx); len(moves) != 1 {
		t.Fatalf("a failed switch dropped the move: %v", moves)
	}
	if err := db.SwitchBlobStore(ctx, "raw-a", "s2", func(*Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	defs, err := db.ListRepositories(ctx)
	if err != nil || len(defs) != 1 || defs[0].BlobStore != "s2" || defs[0].WritePolicy != "allow_once" {
		t.Fatalf("stored definition: %+v %v", defs, err)
	}
	if moves, _ := db.ListBlobStoreMoves(ctx); len(moves) != 0 {
		t.Fatalf("move left: %v", moves)
	}
	if err := db.SwitchBlobStore(ctx, "nope", "s2", func(*Tx) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown repository: %v", err)
	}
	if err := db.DeleteBlobStoreMove(ctx, "raw-a"); err != nil {
		t.Fatal(err)
	}
}
