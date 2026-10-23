package meta

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"commandcenter/internal/config"
)

func TestCleanupPolicyCRUD(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	p := CleanupPolicy{Name: "old-snapshots", Format: "maven", Config: `{"componentAgeDays":30}`}
	if err := db.CreateCleanupPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateCleanupPolicy(ctx, p); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := db.GetCleanupPolicy(ctx, "old-snapshots")
	if err != nil || got.Format != "maven" || got.Config != p.Config || got.Created.IsZero() || got.Updated.IsZero() {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := db.GetCleanupPolicy(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}

	p.Format, p.Config = "all", `{"lastDownloadedDays":7}`
	if err := db.UpdateCleanupPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	upd, _ := db.GetCleanupPolicy(ctx, "old-snapshots")
	if upd.Format != "all" || upd.Config != p.Config || !upd.Created.Equal(got.Created) {
		t.Fatalf("update: %+v", upd)
	}
	if err := db.UpdateCleanupPolicy(ctx, CleanupPolicy{Name: "nope", Format: "all", Config: "{}"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}

	if err := db.CreateCleanupPolicy(ctx, CleanupPolicy{Name: "a-first", Format: "npm", Config: "{}"}); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListCleanupPolicies(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "a-first" || list[1].Name != "old-snapshots" {
		t.Fatalf("list: %+v %v", list, err)
	}

	if err := db.DeleteCleanupPolicy(ctx, "a-first"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteCleanupPolicy(ctx, "a-first"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestCleanupPolicyInUse(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateCleanupPolicy(ctx, CleanupPolicy{Name: "p", Format: "all", Config: "{}"}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []config.Repository{
		{Name: "b-repo", Format: "maven", Type: "hosted", CleanupPolicy: "p"},
		{Name: "a-repo", Format: "npm", Type: "proxy", Remote: "https://r.example/", CleanupPolicy: "p"},
		{Name: "other", Format: "npm", Type: "hosted", CleanupPolicy: "q"},
		{Name: "none", Format: "npm", Type: "hosted"},
	} {
		if err := db.CreateRepository(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	users, err := db.RepositoriesUsingPolicy(ctx, "p")
	if err != nil || !reflect.DeepEqual(users, []string{"a-repo", "b-repo"}) {
		t.Fatalf("users: %v %v", users, err)
	}
	err = db.DeleteCleanupPolicy(ctx, "p")
	var inUse *PolicyInUseError
	if !errors.As(err, &inUse) || !reflect.DeepEqual(inUse.Repos, []string{"a-repo", "b-repo"}) {
		t.Fatalf("delete in use: %v", err)
	}
	if _, err := db.GetCleanupPolicy(ctx, "p"); err != nil {
		t.Fatalf("policy was deleted: %v", err)
	}
}

func TestScanAssetsPages(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := db.PutAsset(ctx, Asset{Repo: "r", Path: fmt.Sprintf("p/%d", i), BlobSHA256: "x", Size: int64(i),
			CreatedAt: t0.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.PutAsset(ctx, Asset{Repo: "other", Path: "p/9", BlobSHA256: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := db.FlushDownloads(ctx, []DownloadDelta{{Repo: "r", Path: "p/1", N: 2, Last: t0.Add(48 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	page, err := db.ScanAssets(ctx, "r", "", 3)
	if err != nil || len(page) != 3 || page[0].Path != "p/0" || page[2].Path != "p/2" {
		t.Fatalf("page 1: %+v %v", page, err)
	}
	if page[1].Size != 1 || !page[1].Created.Equal(t0.Add(time.Hour)) || !page[1].LastDownloaded.Equal(t0.Add(48*time.Hour)) {
		t.Fatalf("row: %+v", page[1])
	}
	if !page[0].LastDownloaded.IsZero() {
		t.Fatalf("never downloaded: %+v", page[0])
	}
	page, err = db.ScanAssets(ctx, "r", "p/2", 3)
	if err != nil || len(page) != 2 || page[0].Path != "p/3" || page[1].Path != "p/4" {
		t.Fatalf("page 2: %+v %v", page, err)
	}
}

func TestCleanupPolicyCountCap(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	defer func(n int) { MaxCleanupPolicies = n }(MaxCleanupPolicies)
	MaxCleanupPolicies = 2
	for _, n := range []string{"a", "b"} {
		if err := db.CreateCleanupPolicy(ctx, CleanupPolicy{Name: n, Format: "all", Config: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateCleanupPolicy(ctx, CleanupPolicy{Name: "c", Format: "all", Config: "{}"}); !errors.Is(err, ErrPolicyLimit) {
		t.Fatalf("over the cap: %v", err)
	}
	if err := db.CreateCleanupPolicy(ctx, CleanupPolicy{Name: "a", Format: "all", Config: "{}"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate at the cap: %v", err)
	}
	if list, _ := db.ListCleanupPolicies(ctx); len(list) != 2 {
		t.Fatalf("list: %+v", list)
	}
}
