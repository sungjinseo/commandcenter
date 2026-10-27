package events

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
)

func openDB(t *testing.T) *meta.DB {
	t.Helper()
	db, err := meta.Open(metatest.Target(t, filepath.Join(t.TempDir(), "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestEmitInsertsAndCloseDrains(t *testing.T) {
	db := openDB(t)
	r := New(db)
	for i := 0; i < 50; i++ {
		r.Emit("upload", "alice", "maven", "a/b/"+strconv.Itoa(i), "d")
	}
	r.Close()
	got, err := db.ListEvents(context.Background(), 200, 0, "")
	if err != nil || len(got) != 50 {
		t.Fatalf("got %d events, err %v", len(got), err)
	}
	// newest first; fields intact
	if got[0].Subject != "a/b/49" || got[0].Kind != "upload" || got[0].Actor != "alice" || got[0].Repo != "maven" || got[0].Detail != "d" {
		t.Fatalf("newest: %+v", got[0])
	}
	if got[0].At.IsZero() {
		t.Fatal("no timestamp")
	}
	r.Emit("late", "", "", "", "") // after Close: dropped, no panic
	if r.Dropped() != 1 {
		t.Fatalf("dropped %d, want 1", r.Dropped())
	}
	r.Close() // idempotent
}

func TestPagingAndKindFilter(t *testing.T) {
	db := openDB(t)
	r := New(db)
	for i := 0; i < 10; i++ {
		k := "upload"
		if i%2 == 1 {
			k = "delete"
		}
		r.Emit(k, "", "r", strconv.Itoa(i), "")
	}
	r.Close()
	ctx := context.Background()
	p1, _ := db.ListEvents(ctx, 4, 0, "")
	p2, _ := db.ListEvents(ctx, 4, p1[len(p1)-1].ID, "")
	if len(p1) != 4 || len(p2) != 4 || p1[3].ID <= p2[0].ID || p2[0].Subject != "5" {
		t.Fatalf("p1 %+v p2 %+v", p1, p2)
	}
	del, _ := db.ListEvents(ctx, 50, 0, "delete")
	if len(del) != 5 {
		t.Fatalf("delete events: %d", len(del))
	}
}

func TestPruneKeepsNewest(t *testing.T) {
	db := openDB(t)
	r := newWith(db, 20, 10)
	for i := 0; i < 55; i++ {
		r.Emit("upload", "", "r", strconv.Itoa(i), "")
	}
	r.Close()
	n, _ := db.CountEvents(context.Background())
	// pruned in batches: never below Keep, never more than Keep + PruneEvery + a batch
	if n < 20 || n > 20+10+maxBatch {
		t.Fatalf("count %d", n)
	}
	got, _ := db.ListEvents(context.Background(), 1, 0, "")
	if got[0].Subject != "54" {
		t.Fatalf("newest %+v", got[0])
	}
	// The final prune on Close leaves exactly Keep rows.
	if n != 20 {
		t.Fatalf("after close count %d, want 20", n)
	}
}

// blockStore blocks InsertEvents until released.
type blockStore struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	n       int
}

func (b *blockStore) InsertEvents(ctx context.Context, evs []meta.Event) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	b.mu.Lock()
	b.n += len(evs)
	b.mu.Unlock()
	return nil
}
func (b *blockStore) PruneEvents(context.Context, int) (int64, error) { return 0, nil }

func TestFullChannelDropsWithoutBlocking(t *testing.T) {
	bs := &blockStore{entered: make(chan struct{}), release: make(chan struct{})}
	r := New(bs)
	r.Emit("upload", "", "r", "first", "")
	<-bs.entered // the writer holds one event and is stuck in the insert
	start := time.Now()
	for i := 0; i < QueueSize+7; i++ {
		r.Emit("upload", "", "r", "x", "")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Emit blocked for %v", d)
	}
	if r.Dropped() != 7 {
		t.Fatalf("dropped %d, want 7", r.Dropped())
	}
	close(bs.release)
	r.Close()
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.n != 1+QueueSize {
		t.Fatalf("stored %d, want %d", bs.n, 1+QueueSize)
	}
}

type failStore struct{}

func (failStore) InsertEvents(context.Context, []meta.Event) error { return errors.New("boom") }
func (failStore) PruneEvents(context.Context, int) (int64, error)  { return 0, nil }

func TestInsertFailureCountsDropped(t *testing.T) {
	r := New(failStore{})
	r.Emit("upload", "", "r", "x", "")
	r.Close()
	if r.Dropped() != 1 {
		t.Fatalf("dropped %d, want 1", r.Dropped())
	}
}

func TestFieldsAreTruncated(t *testing.T) {
	db := openDB(t)
	r := New(db)
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'a'
	}
	r.Emit("upload", "", "r", string(long), string(long))
	r.Close()
	got, _ := db.ListEvents(context.Background(), 1, 0, "")
	if len(got[0].Subject) > maxSubject || len(got[0].Detail) > maxDetail {
		t.Fatalf("lengths %d %d", len(got[0].Subject), len(got[0].Detail))
	}
}

func TestClipBacksUpAtMostUTFMax(t *testing.T) {
	// An invalid byte early on must not make clip walk the whole string.
	s := "\xff" + strings.Repeat("é", 5000)
	got := clip(s, 100)
	if len(got) > 100 || len(got) < 100-utf8.UTFMax {
		t.Fatalf("len %d", len(got))
	}
	if clip("short", 100) != "short" {
		t.Fatal("short string changed")
	}
	// A garbage tail: still bounded.
	if got := clip(strings.Repeat("\x80", 1000), 10); len(got) != 10-utf8.UTFMax {
		t.Fatalf("len %d, want %d", len(got), 10-utf8.UTFMax)
	}
}

func TestCloseGivesUpAfterDeadline(t *testing.T) {
	old := closeTimeout
	closeTimeout = 50 * time.Millisecond
	defer func() { closeTimeout = old }()
	bs := &blockStore{entered: make(chan struct{}), release: make(chan struct{})}
	r := New(bs)
	r.Emit("upload", "", "r", "x", "")
	<-bs.entered
	start := time.Now()
	r.Close() // the writer is stuck in the insert
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close waited %v", d)
	}
	close(bs.release)
	<-r.done
}
