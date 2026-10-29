package meta

import (
	"errors"
	"testing"

	"commandcenter/internal/blob"
)

// A group row keeps its members in order with their read-only flags; an
// update replaces them; converting a store renames its row to the member,
// with the new path, and creates the group at the store's old path, in one
// transaction that a taken member name leaves untouched.
func TestBlobStoreGroupRows(t *testing.T) {
	db := openTest(t)
	cfg := blob.GroupConfig{Members: []blob.GroupMember{{Name: "a"}, {Name: "b", ReadOnly: true}}, FillPolicy: blob.FillWriteToFirst}
	for _, bs := range []BlobStore{{Name: "a", Path: "/a"}, {Name: "b", Path: "/b"}, {Name: "g", Path: "blobstores/g", Type: BlobStoreGroup, Group: &cfg}} {
		if err := db.CreateBlobStore(ctx, bs); err != nil {
			t.Fatal(err)
		}
	}
	g, err := db.GetBlobStore(ctx, "g")
	if err != nil || g.Type != BlobStoreGroup || g.Group == nil || g.S3 != nil || len(g.Group.Members) != 2 ||
		g.Group.Members[1] != (blob.GroupMember{Name: "b", ReadOnly: true}) || g.Group.FillPolicy != blob.FillWriteToFirst {
		t.Fatalf("group row: %+v %+v, %v", g, g.Group, err)
	}
	cfg.Members = cfg.Members[:1]
	if err := db.UpdateBlobStoreGroup(ctx, "g", cfg); err != nil {
		t.Fatal(err)
	}
	if g, _ := db.GetBlobStore(ctx, "g"); len(g.Group.Members) != 1 {
		t.Fatalf("after update: %+v", g.Group)
	}
	if err := db.UpdateBlobStoreGroup(ctx, "a", cfg); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a file store: %v", err)
	}
	if err := db.CreateBlobStore(ctx, BlobStore{Name: "bad", Type: BlobStoreGroup}); err == nil {
		t.Fatal("a group without members was stored")
	}

	one := blob.GroupConfig{Members: []blob.GroupMember{{Name: "b-member"}}, FillPolicy: blob.FillRoundRobin}
	if err := db.ConvertBlobStoreToGroup(ctx, "b", "a", "/b", one); !errors.Is(err, ErrExists) {
		t.Fatalf("convert onto a taken name: %v", err)
	}
	if bs, _ := db.GetBlobStore(ctx, "b"); bs.Type != BlobStoreFile {
		t.Fatalf("a failed convert changed the store: %+v", bs)
	}
	if err := db.ConvertBlobStoreToGroup(ctx, "b", "b-member", "/b", one); err != nil {
		t.Fatal(err)
	}
	m, err := db.GetBlobStore(ctx, "b-member")
	if err != nil || m.Type != BlobStoreFile || m.Path != "/b" {
		t.Fatalf("member row: %+v, %v", m, err)
	}
	if g, err := db.GetBlobStore(ctx, "b"); err != nil || g.Type != BlobStoreGroup || g.Path != "/b" || g.Group.Members[0].Name != "b-member" {
		t.Fatalf("new group row: %+v, %v", g, err)
	}
	if err := db.ConvertBlobStoreToGroup(ctx, "b", "x", "/b", one); err == nil {
		t.Fatal("a group was converted again")
	}
	if err := db.ConvertBlobStoreToGroup(ctx, "nope", "x", "/x", one); !errors.Is(err, ErrNotFound) {
		t.Fatalf("convert of a missing store: %v", err)
	}
}
