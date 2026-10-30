package meta

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every write of a content row reaches OnCommit after the commit, with the
// row as stored (the created_at an upsert keeps, a package's whole row after
// a yank), in order and stamped with the database's clock; a rolled back
// transaction reaches it with nothing.
func TestOnCommitJournalsContentWrites(t *testing.T) {
	db := openTest(t)
	var mu sync.Mutex
	type commit struct {
		at      time.Time
		changes []Change
	}
	var got []commit
	db.OnCommit = func(_ context.Context, at time.Time, cs []Change) {
		mu.Lock()
		got = append(got, commit{at, cs})
		mu.Unlock()
	}
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	if err := db.PutAsset(ctx, Asset{Repo: "r", Path: "a", BlobSHA256: "s1", Size: 1, CreatedAt: t0, LastModified: t0}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutAsset(ctx, Asset{Repo: "r", Path: "a", BlobSHA256: "s2", Size: 2, CreatedAt: t0.Add(time.Hour), LastModified: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	err := db.Tx(ctx, func(tx *Tx) error {
		if err := tx.InsertPackage(ctx, Package{Repo: "r", Format: "npm", Name: "n", Version: "1", Attrs: `{"v":1}`, CreatedAt: t0}); err != nil {
			return err
		}
		if err := tx.SetYanked(ctx, "r", "n", "1", true); err != nil {
			return err
		}
		if err := tx.SetDistTag(ctx, "r", "n", "latest", "1"); err != nil {
			return err
		}
		if err := tx.ImportPackage(ctx, Package{Repo: "r", Format: "npm", Name: "n", Version: "1", Attrs: `{}`}, false); err != nil {
			return err // kept: no change
		}
		return tx.DeleteDistTag(ctx, "r", "n", "latest")
	})
	if err != nil {
		t.Fatal(err)
	}
	db.Tx(ctx, func(tx *Tx) error {
		tx.DeletePackage(ctx, "r", "n", "1")
		return errors.New("rolled back")
	})
	if err := db.DeleteAsset(ctx, "r", "a"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAsset(ctx, "r", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("%d commits journaled, want 4: %+v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if !got[i].at.After(got[i-1].at) || time.Since(got[i].at) > time.Minute {
			t.Fatalf("commit times %v then %v: want increasing database times", got[i-1].at, got[i].at)
		}
	}
	if c := got[1].changes[0]; c.Op != ChangePut || c.Asset.SHA256 != "s2" || !c.Asset.Created.Equal(t0) {
		t.Fatalf("an upsert journals the created_at it keeps: %+v %+v", c, c.Asset)
	}
	cs := got[2].changes
	var kinds []string
	for _, c := range cs {
		kinds = append(kinds, c.Op+" "+c.Kind+" "+c.Label())
	}
	if want := "put package n@1,put package n@1,put tag n:latest,delete tag n:latest"; strings.Join(kinds, ",") != want {
		t.Fatalf("changes %q, want %q", strings.Join(kinds, ","), want)
	}
	if p := cs[1].Package; p == nil || !p.Yanked || p.Attrs != `{"v":1}` || p.Format != "npm" || !p.Created.Equal(t0) {
		t.Fatalf("a yank journals the whole row: %+v", p)
	}
	if c := got[3].changes[0]; c.Op != ChangeDelete || c.Kind != ChangeAsset || c.Path != "a" {
		t.Fatalf("delete: %+v", c)
	}
}

func TestReconcilePlanStore(t *testing.T) {
	db := openTest(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i, id := range []string{"p1", "p2", "p3"} {
		if err := db.CreateReconcilePlan(ctx, ReconcilePlan{ID: id, State: PlanPlanned, Created: now.Add(time.Duration(i) * time.Second), CreatedBy: "admin"}); err != nil {
			t.Fatal(err)
		}
	}
	page, next, err := db.ListReconcilePlans(ctx, "", 2)
	if err != nil || len(page) != 2 || page[0].ID != "p3" || page[1].ID != "p2" || next == "" {
		t.Fatalf("first page %+v %q %v", page, next, err)
	}
	page, next, err = db.ListReconcilePlans(ctx, next, 2)
	if err != nil || len(page) != 1 || page[0].ID != "p1" || next != "" {
		t.Fatalf("second page %+v %q %v", page, next, err)
	}
	if _, _, err := db.ListReconcilePlans(ctx, "nope", 2); !errors.Is(err, ErrBadToken) {
		t.Fatalf("bad token: %v", err)
	}
	var items []ReconcileItem
	for i := 1; i <= itemBatch+50; i++ {
		it := ReconcileItem{PlanID: "p1", Seq: int64(i), Repo: "r", Kind: ChangeAsset, Label: fmt.Sprint("f", i), Action: ItemAdd, State: ItemPlanned,
			BlobSHA256: fmt.Sprintf("%064d", i%3), At: now}
		if i%10 == 0 {
			it.Action, it.State = ItemSkip, ItemSkipped
		}
		items = append(items, it)
	}
	if err := db.AddReconcileItems(ctx, items); err != nil {
		t.Fatal(err)
	}
	got, err := db.ReconcileItems(ctx, "p1", 400, 1000, ItemFilter{States: []string{ItemPlanned, ItemFailed}})
	if err != nil || len(got) != 45 || got[0].Seq != 401 {
		t.Fatalf("items after 400: %d (first %+v), %v", len(got), got[0], err)
	}
	if err := db.MarkReconcileItems(ctx, "p1", []ItemResult{{Seq: 1, State: ItemDone}, {Seq: 2, State: ItemFailed, Reason: "boom"}}); err != nil {
		t.Fatal(err)
	}
	counts, err := db.CountReconcileItems(ctx, "p1")
	if err != nil || counts["ADD/DONE"] != 1 || counts["ADD/FAILED"] != 1 || counts["SKIP/SKIPPED"] != 45 || counts["ADD/PLANNED"] != 403 {
		t.Fatalf("counts %v %v", counts, err)
	}
	pending, err := db.PendingReconcileBlobs(ctx)
	if err != nil || len(pending) != 3 {
		t.Fatalf("pending blobs %v %v", pending, err)
	}
	if err := db.UpdateReconcilePlan(ctx, "p1", PlanFailed, `{"failed":1}`, now); err != nil {
		t.Fatal(err)
	}
	if p, err := db.GetReconcilePlan(ctx, "p1"); err != nil || p.State != PlanFailed || !p.Executed.Equal(now) || p.Summary != `{"failed":1}` {
		t.Fatalf("plan %+v %v", p, err)
	}
	if err := db.DeleteReconcilePlan(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if pending, _ := db.PendingReconcileBlobs(ctx); len(pending) != 0 {
		t.Fatalf("a deleted plan keeps blobs: %v", pending)
	}
	if err := db.DeleteReconcilePlan(ctx, "p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestRecoveryModeSetting(t *testing.T) {
	db, url := openTestDB(t)
	if rm, err := db.LoadRecoveryMode(ctx); err != nil || rm.Enabled || db.Recovery().Enabled {
		t.Fatalf("default %+v %v", rm, err)
	}
	on := RecoveryMode{Enabled: true, Since: time.Now().UTC().Truncate(time.Second), By: "admin", Message: "restoring"}
	if err := db.SetRecoveryMode(ctx, on); err != nil {
		t.Fatal(err)
	}
	if db.Recovery() != on {
		t.Fatalf("cached %+v", db.Recovery())
	}
	other, err := Open(url)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if rm, err := other.LoadRecoveryMode(ctx); err != nil || rm != on {
		t.Fatalf("another node reads %+v %v", rm, err)
	}
	if n, err := db.JournalDays(ctx); err != nil || n != DefaultJournalDays {
		t.Fatalf("journal days %d %v", n, err)
	}
	if err := db.SetJournalDays(ctx, 0); err == nil {
		t.Fatal("0 days accepted")
	}
	if err := db.SetJournalDays(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if n, _ := other.JournalDays(ctx); n != 7 {
		t.Fatalf("journal days %d", n)
	}
}
