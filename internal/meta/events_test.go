package meta

import (
	"strconv"
	"testing"
	"time"
)

func TestEventsInsertListPrune(t *testing.T) {
	db := openTest(t)
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	evs := []Event{{At: t0.Add(-time.Hour), Kind: "repo.update", Actor: "admin", Repo: "r", Subject: "r", IP: "10.0.0.1", Node: "n1"}}
	for i := 0; i < 12; i++ {
		kind := "upload"
		if i%2 == 1 {
			kind = "delete"
		}
		evs = append(evs, Event{At: t0.Add(time.Duration(i) * time.Second), Kind: kind, Actor: "a", Repo: "r", Subject: strconv.Itoa(i), Detail: "d"})
	}
	if err := db.InsertEvents(ctx, evs); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertEvents(ctx, nil); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListEvents(ctx, 5, 0, "")
	if err != nil || len(got) != 5 || got[0].Subject != "11" || got[4].Subject != "7" {
		t.Fatalf("list: %+v %v", got, err)
	}
	if !got[0].At.Equal(t0.Add(11*time.Second)) || got[0].ID == 0 {
		t.Fatalf("event %+v", got[0])
	}
	next, _ := db.ListEvents(ctx, 5, got[4].ID, "")
	if len(next) != 5 || next[0].Subject != "6" {
		t.Fatalf("next page: %+v", next)
	}
	del, _ := db.ListEvents(ctx, 50, 0, "delete")
	if len(del) != 6 {
		t.Fatalf("delete events: %d", len(del))
	}

	// Pruning keeps the newest N uploads and deletes and the newest N
	// others, and is a no-op when there are fewer.
	n, err := db.PruneEvents(ctx, 4)
	if err != nil || n != 8 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	left, _ := db.ListEvents(ctx, 50, 0, "")
	if len(left) != 5 || left[0].Subject != "11" || left[3].Subject != "8" || left[4].Kind != "repo.update" ||
		left[4].IP != "10.0.0.1" || left[4].Node != "n1" {
		t.Fatalf("left: %+v", left)
	}
	if n, err := db.PruneEvents(ctx, 100); err != nil || n != 0 {
		t.Fatalf("no-op prune: %d %v", n, err)
	}
	if c, _ := db.CountEvents(ctx); c != 5 {
		t.Fatalf("count %d", c)
	}
}
