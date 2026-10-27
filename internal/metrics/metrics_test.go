package metrics

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/meta"
)

type fakeDB struct {
	mu        sync.Mutex
	dlCalls   int
	rqCalls   int
	dl        []meta.DownloadDelta
	rq        []meta.RequestDelta
	pruned    []int64
	negPruned []time.Time
	failNext  bool
	failAll   bool
	failRq    bool
	attempts  int
	dlHook    func()
	rqHook    func()
	flushedCh chan struct{}
}

func (f *fakeDB) FlushDownloads(_ context.Context, b []meta.DownloadDelta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.dlHook != nil {
		f.dlHook()
	}
	if f.failAll {
		return errors.New("down")
	}
	if f.failNext {
		f.failNext = false
		return errors.New("boom")
	}
	f.dlCalls++
	f.dl = append(f.dl, b...)
	if f.flushedCh != nil {
		select {
		case f.flushedCh <- struct{}{}:
		default:
		}
	}
	return nil
}

func (f *fakeDB) FlushRequests(_ context.Context, b []meta.RequestDelta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rqHook != nil {
		f.rqHook()
	}
	if f.failRq {
		return errors.New("down")
	}
	f.rqCalls++
	f.rq = append(f.rq, b...)
	return nil
}

func (f *fakeDB) PruneRequestStats(_ context.Context, before int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruned = append(f.pruned, before)
	return 0, nil
}

func (f *fakeDB) PruneNegative(_ context.Context, now time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.negPruned = append(f.negPruned, now)
	return 0, nil
}

func TestNoDBWriteUntilFlushAndAggregation(t *testing.T) {
	db := &fakeDB{}
	r := New(db)
	t0 := time.Unix(1_700_000_000, 0)
	r.Now = func() time.Time { return t0 }
	for i := 0; i < 1000; i++ {
		r.Download("r", "a.jar")
		r.Request("r", 200, 0, 10, 0)
	}
	r.Download("r", "b.jar")
	if db.dlCalls != 0 || db.rqCalls != 0 {
		t.Fatalf("db written on the hot path: %d %d", db.dlCalls, db.rqCalls)
	}
	t1 := t0.Add(time.Minute)
	r.Now = func() time.Time { return t1 }
	r.Download("r", "a.jar")
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if db.dlCalls != 1 || db.rqCalls != 1 {
		t.Fatalf("want one call each, got %d %d", db.dlCalls, db.rqCalls)
	}
	got := map[string]meta.DownloadDelta{}
	for _, d := range db.dl {
		got[d.Path] = d
	}
	if len(db.dl) != 2 || got["a.jar"].N != 1001 || got["b.jar"].N != 1 || !got["a.jar"].Last.Equal(t1) {
		t.Fatalf("downloads %+v", db.dl)
	}
	hour := t0.Unix() / 3600 * 3600
	if len(db.rq) != 1 || db.rq[0] != (meta.RequestDelta{Hour: hour, Repo: "r", Requests: 1000, Bytes: 10000}) {
		t.Fatalf("requests %+v", db.rq)
	}
	// Nothing pending: a second flush writes nothing.
	r.Flush(context.Background())
	if db.dlCalls != 1 || db.rqCalls != 1 {
		t.Fatalf("empty flush wrote: %d %d", db.dlCalls, db.rqCalls)
	}
}

func TestRequestBucketsPerHour(t *testing.T) {
	db := &fakeDB{}
	r := New(db)
	t0 := time.Unix(1_700_000_000, 0)
	r.Now = func() time.Time { return t0 }
	r.Request("r", 200, 0, 1, 0)
	r.Now = func() time.Time { return t0.Add(time.Hour) }
	r.Request("r", 200, 0, 2, 0)
	r.Request("r", 200, 0, 3, 0)
	r.Flush(context.Background())
	if len(db.rq) != 2 {
		t.Fatalf("buckets %+v", db.rq)
	}
	for _, d := range db.rq {
		if d.Hour%3600 != 0 {
			t.Fatalf("hour not truncated: %+v", d)
		}
	}
}

func TestFlushFailureKeepsCounts(t *testing.T) {
	db := &fakeDB{failNext: true}
	r := New(db)
	r.Download("r", "a")
	if err := r.Flush(context.Background()); err == nil {
		t.Fatal("want error")
	}
	r.Download("r", "a")
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(db.dl) != 1 || db.dl[0].N != 2 {
		t.Fatalf("counts lost: %+v", db.dl)
	}
}

func TestFlushAtKeyThreshold(t *testing.T) {
	db := &fakeDB{flushedCh: make(chan struct{}, 1)}
	r := New(db)
	r.MaxKeys = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx, time.Hour); close(done) }()
	r.Download("r", "1")
	r.Download("r", "2")
	select {
	case <-db.flushedCh:
		t.Fatal("flushed below the threshold")
	case <-time.After(100 * time.Millisecond):
	}
	r.Download("r", "3")
	select {
	case <-db.flushedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("no flush at the threshold")
	}
	cancel()
	<-done
}

func TestRunFlushesOnInterval(t *testing.T) {
	db := &fakeDB{flushedCh: make(chan struct{}, 1)}
	r := New(db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx, 20*time.Millisecond)
	r.Download("r", "x")
	select {
	case <-db.flushedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("no periodic flush")
	}
}

func TestRunPrunesOlderThanRetention(t *testing.T) {
	db := &fakeDB{}
	r := New(db)
	now := time.Unix(1_700_000_000, 0)
	r.Now = func() time.Time { return now }
	r.prune(context.Background())
	want := now.Add(-90*24*time.Hour).Unix() / 3600 * 3600
	if len(db.pruned) != 1 || db.pruned[0] != want {
		t.Fatalf("pruned %v want %d", db.pruned, want)
	}
	// The same pass drops negative-cache entries that expired by now.
	if len(db.negPruned) != 1 || !db.negPruned[0].Equal(now) {
		t.Fatalf("negative cache pruned at %v, want once at %v", db.negPruned, now)
	}
}

func (f *fakeDB) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func TestNoRetryStormAfterFailure(t *testing.T) {
	db := &fakeDB{failAll: true, flushedCh: make(chan struct{}, 1)}
	r := New(db)
	var clock atomic.Int64 // Run reads it from its own goroutine
	clock.Store(1_700_000_000)
	r.Now = func() time.Time { return time.Unix(clock.Load(), 0) } // time stands still: always inside the backoff
	r.MaxKeys = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx, time.Hour); close(done) }()
	for _, p := range []string{"1", "2", "3"} {
		r.Download("r", p)
	}
	deadline := time.Now().Add(3 * time.Second)
	for db.attemptCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if db.attemptCount() != 1 {
		t.Fatalf("attempts %d, want 1", db.attemptCount())
	}
	// Many more calls, new keys and an explicit kick: the backoff holds them all.
	for i := 0; i < 200; i++ {
		r.Download("r", "k"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	r.signal()
	time.Sleep(150 * time.Millisecond)
	if n := db.attemptCount(); n != 1 {
		t.Fatalf("retry storm: %d attempts", n)
	}
	// After the backoff the threshold works again, and the failed counts were kept.
	clock.Add(int64((FailBackoff + time.Second) / time.Second))
	db.mu.Lock()
	db.failAll = false
	db.mu.Unlock()
	r.signal()
	select {
	case <-db.flushedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("no flush after the backoff")
	}
	cancel()
	<-done
}

func TestKickOnlyWhenCrossingThreshold(t *testing.T) {
	r := New(&fakeDB{})
	r.MaxKeys = 2
	r.Download("r", "a")
	r.Download("r", "b") // crosses
	if len(r.kick) != 1 {
		t.Fatal("no kick at the crossing")
	}
	<-r.kick
	r.Download("r", "c")
	r.Download("r", "a")
	r.Request("r", 200, 0, 1, 0)
	if len(r.kick) != 0 {
		t.Fatal("kicked again above the threshold")
	}
}

func TestMaxPendingDropsNewKeysAndCounts(t *testing.T) {
	r := New(&fakeDB{})
	r.MaxPending = 2
	r.Download("r", "a")
	r.Download("r", "b")
	r.Download("r", "c") // dropped
	r.Download("r", "a") // existing key still counts
	r.Request("r", 200, 0, 1, 0)
	r.Now = func() time.Time { return time.Unix(1_700_000_000+7200, 0) }
	r.Request("r", 200, 0, 1, 0)
	r.Now = func() time.Time { return time.Unix(1_700_000_000+14400, 0) }
	r.Request("r", 200, 0, 1, 0) // dropped
	if r.Dropped() != 2 {
		t.Fatalf("dropped %d, want 2", r.Dropped())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.dl) != 2 || r.dl[dlKey{"r", "a"}].n != 2 || len(r.rq) != 2 {
		t.Fatalf("pending: %+v %+v", r.dl, r.rq)
	}
}

func TestRestoreAfterFailedFlushMergesAndBounds(t *testing.T) {
	db := &fakeDB{failNext: true, failRq: true}
	r := New(db)
	r.MaxPending = 3
	hour := r.Now().Unix() / 3600 * 3600
	r.Download("r", "a")
	r.Download("r", "b")
	r.Request("r", 200, 0, 5, 0)
	// While the flush is failing, new traffic arrives: an existing key and
	// fresh keys that fill the map, so part of the restore must be dropped.
	db.dlHook = func() {
		r.Download("r", "a")
		r.Download("r", "x")
		r.Download("r", "y")
	}
	db.rqHook = func() {
		r.Request("r", 200, 0, 7, 0) // same hour: merged with the restored counts
		r.Now = func() time.Time { return time.Unix(hour+3600, 0) }
		r.Request("q", 200, 0, 1, 0)
		r.Request("q2", 200, 0, 1, 0)
		r.Now = func() time.Time { return time.Unix(hour+7200, 0) }
	}
	if err := r.Flush(context.Background()); err == nil {
		t.Fatal("want error")
	}
	r.mu.Lock()
	if len(r.dl) != 3 || r.dl[dlKey{"r", "a"}].n != 2 {
		t.Fatalf("downloads %+v", r.dl)
	}
	// Restoring a (merge, allowed) and b (new key, map full) -> b dropped.
	if _, ok := r.dl[dlKey{"r", "b"}]; ok {
		t.Fatal("b should have been dropped")
	}
	if got := r.rq[rqKey{hour, "r"}]; got.requests != 2 || got.bytes != 12 {
		t.Fatalf("requests not merged: %+v", r.rq)
	}
	r.mu.Unlock()
	if r.Dropped() != 1 {
		t.Fatalf("dropped %d, want 1", r.Dropped())
	}
}

func TestWarningsAreRateLimited(t *testing.T) {
	r := New(&fakeDB{})
	now := time.Unix(1_700_000_000, 0)
	r.Now = func() time.Time { return now }
	r.warn("k", "m")
	first := r.lastWarn["k"]
	now = now.Add(30 * time.Second)
	r.warn("k", "m")
	if !r.lastWarn["k"].Equal(first) {
		t.Fatal("warned again within a minute")
	}
	now = now.Add(31 * time.Second)
	r.warn("k", "m")
	if r.lastWarn["k"].Equal(first) {
		t.Fatal("did not warn after a minute")
	}
}

func TestRestoreRequestsDropsWhenFull(t *testing.T) {
	db := &fakeDB{failRq: true}
	r := New(db)
	r.MaxPending = 2
	r.Request("r", 200, 0, 5, 0)
	db.rqHook = func() { r.Request("q", 200, 0, 1, 0); r.Request("q2", 200, 0, 1, 0) }
	if err := r.Flush(context.Background()); err == nil {
		t.Fatal("want error")
	}
	r.mu.Lock()
	n := len(r.rq)
	r.mu.Unlock()
	if n != 2 || r.Dropped() != 1 {
		t.Fatalf("pending %d dropped %d, want 2 and 1", n, r.Dropped())
	}
}

func TestRequestErrorsUpstreamAndCacheCounters(t *testing.T) {
	db := &fakeDB{}
	r := New(db)
	r.Request("r", 200, 10, 100, 20*time.Millisecond)
	r.Request("r", 503, 0, 1, 5*time.Millisecond)
	r.Request("r", 429, 0, 1, 50*time.Millisecond)
	r.Upstream("r", true, 300*time.Millisecond)
	r.Upstream("r", false, 2*time.Second)
	r.Cache("r", true)
	r.Cache("r", false)
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	hour := r.Now().Unix() / 3600 * 3600
	want := meta.RequestDelta{Hour: hour, Repo: "r", Requests: 3, Bytes: 102, Errors: 2, BytesIn: 10, Ms: 75, MaxMs: 50,
		Upstream: 2, UpstreamFailures: 1, UpstreamMs: 2300, UpstreamMaxMs: 2000, CacheHits: 1, CacheMisses: 1}
	if len(db.rq) != 1 || db.rq[0] != want {
		t.Fatalf("got %+v\nwant %+v", db.rq, want)
	}
}
