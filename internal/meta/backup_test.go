package meta

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The snapshot path may contain characters that mean something in a file:
// URI; the read-only handle really is read-only.
func TestSnapshotURIEscaping(t *testing.T) {
	name := "we ird%20?#x"
	if runtime.GOOS == "windows" {
		name = "we ird%20#x"
	}
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.PutAsset(ctx, Asset{Repo: "r", Path: "p", BlobSHA256: "s", Size: 1}); err != nil {
		t.Fatal(err)
	}
	snapPath := filepath.Join(dir, "meta.db")
	if err := db.VacuumInto(ctx, snapPath); err != nil {
		t.Fatal(err)
	}
	snap, err := OpenReadOnly(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if n, err := snap.CountAssets(ctx); err != nil || n != 1 {
		t.Fatalf("count %d %v", n, err)
	}
	if _, err := snap.sql.ExecContext(ctx, `DELETE FROM assets`); err == nil {
		t.Fatal("the read-only snapshot accepted a write")
	}
	entries, _ := os.ReadDir(filepath.Dir(dir))
	if len(entries) != 1 {
		t.Fatalf("files outside the snapshot dir: %v", entries)
	}
}
