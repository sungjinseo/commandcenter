package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/config"
	"commandcenter/internal/format/maven"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
)

// clock is a settable time source: the tests never sleep for a schedule.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

type harness struct {
	s     *Service
	db    *meta.DB
	store *repo.Store
	reg   *repo.Registry
	sink  *repotest.Sink
	clock *clock
	gcs   atomic.Int32
	mvn   *maven.Handler
	defs  []config.Repository
}

// t0 is a Tuesday morning, UTC.
var t0 = time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	blobs, err := blob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	for _, p := range []cleanup.Policy{
		{Name: "old", Format: "maven", Criteria: cleanup.Criteria{ComponentAgeDays: 30}},
		{Name: "npm-only", Format: "npm", Criteria: cleanup.Criteria{ComponentAgeDays: 30}},
	} {
		if err := cleanup.CreatePolicy(ctx, db, p); err != nil {
			t.Fatal(err)
		}
	}
	defs := []config.Repository{
		{Name: "mvn", Format: "maven", Type: "hosted", CleanupPolicy: "old"},
		{Name: "mvn-lost-policy", Format: "maven", Type: "hosted", CleanupPolicy: "gone"}, // imported, policy missing
		{Name: "mvn-none", Format: "maven", Type: "hosted"},
		{Name: "mvn-proxy", Format: "maven", Type: "proxy", Remote: "https://repo.example.org/maven2"},
		{Name: "mvn-group", Format: "maven", Type: "group", Members: []string{"mvn"}},
		{Name: "crates", Format: "cargo", Type: "hosted"},
		{Name: "mvn-npm-policy", Format: "maven", Type: "hosted", CleanupPolicy: "npm-only"}, // format mismatch
	}
	reg := repo.NewRegistry(db, defs)
	c := &clock{t: t0}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: blobs}, DB: db, Now: c.Now}
	store.Live = func(rp *repo.Repository) bool { cur := reg.Get(rp.Name); return cur != nil && cur.Gen == rp.Gen }
	sink := &repotest.Sink{}
	store.Events = sink
	mvn := maven.New(store, nil)
	mvn.Now = c.Now
	deleters := cleanup.NewRegistry(store)
	deleters.Register("maven", "hosted", mvn)
	h := &harness{db: db, store: store, reg: reg, sink: sink, clock: c, mvn: mvn, defs: defs}
	h.s = &Service{DB: db, Repos: reg, Deleters: deleters, Maven: mvn, Events: sink,
		Log: slog.New(slog.DiscardHandler), Now: c.Now, Loc: time.UTC,
		GC: func(context.Context, string) (int, int64, error) { h.gcs.Add(1); return 3, 30, nil }}
	return h
}

func (h *harness) create(t *testing.T, d Def) meta.Task {
	t.Helper()
	tk, err := h.s.Create(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func (h *harness) get(t *testing.T, id string) meta.Task {
	t.Helper()
	tk, err := h.s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func (h *harness) put(t *testing.T, repoName, path, body string) {
	t.Helper()
	if _, err := h.store.Put(context.Background(), repoName, path, "application/octet-stream", strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

func TestDueTaskRunsOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tk := h.create(t, Def{Name: "Nightly compact", Type: TypeCompact, Schedule: "daily:03:00"})
	if want := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC); !tk.NextRun.Equal(want) || !tk.Enabled {
		t.Fatalf("created: next %s enabled %v", tk.NextRun, tk.Enabled)
	}
	h.s.RunDue(ctx)
	if n := h.gcs.Load(); n != 0 {
		t.Fatalf("ran before due: %d", n)
	}
	h.clock.Set(time.Date(2026, 10, 20, 3, 0, 5, 0, time.UTC))
	h.s.RunDue(ctx)
	h.s.RunDue(ctx)
	if n := h.gcs.Load(); n != 1 {
		t.Fatalf("due task ran %d times, want 1", n)
	}
	got := h.get(t, tk.ID)
	if got.LastResult != ResultOK || got.LastRemoved != 3 || got.LastBytes != 30 || got.LastMessage != "removed 3 blobs (30 bytes)" || got.LastRun.IsZero() ||
		!got.NextRun.Equal(time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)) || !strings.Contains(got.LastLog, "removed 3 blobs (30 bytes)") {
		t.Fatalf("after run: %+v", got)
	}
	evs := h.sink.Events()
	if len(evs) != 1 || evs[0].Kind != "task.run" || evs[0].Actor != "system" || evs[0].Subject != tk.ID ||
		!strings.HasPrefix(evs[0].Detail, "OK: removed 3 blobs") {
		t.Fatalf("events: %v", evs)
	}

	// Disabled and manual tasks never run on schedule.
	h.create(t, Def{Name: "manual", Type: TypeCompact, Schedule: "manual"})
	off := false
	h.create(t, Def{Name: "off", Type: TypeCompact, Schedule: "hourly:00", Enabled: &off})
	h.clock.Set(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))
	h.s.RunDue(ctx)
	if n := h.gcs.Load(); n != 2 { // only the daily task, once for its missed runs
		t.Fatalf("runs: %d", n)
	}
}

func TestRunsAreSerialized(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var active, peak atomic.Int32
	entered, release := make(chan struct{}, 10), make(chan struct{})
	h.s.GC = func(context.Context, string) (int, int64, error) {
		n := active.Add(1)
		defer active.Add(-1)
		if n > peak.Load() {
			peak.Store(n)
		}
		entered <- struct{}{}
		<-release
		return 0, 0, nil
	}
	a := h.create(t, Def{Name: "a", Type: TypeCompact, Schedule: "daily:03:00"})
	b := h.create(t, Def{Name: "b", Type: TypeCompact, Schedule: "daily:03:00"})
	h.clock.Set(time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)) // both due

	first := make(chan error, 1)
	go func() { _, err := h.s.RunNow(a.ID, "admin"); first <- err }()
	<-entered
	if got := h.s.Running(); got != a.ID {
		t.Fatalf("running = %q", got)
	}
	// While a runs: run now is refused, the scheduler waits for the next tick.
	var busy *BusyError
	if _, err := h.s.RunNow(b.ID, "admin"); !errors.As(err, &busy) || busy.Running != a.ID {
		t.Fatalf("run now while running: %v", err)
	}
	if _, err := h.s.RunCompact("admin"); !errors.As(err, &busy) {
		t.Fatalf("gc alias while running: %v", err)
	}
	h.s.RunDue(ctx) // returns at once
	if err := h.s.Delete(ctx, a.ID); !errors.As(err, &busy) {
		t.Fatalf("delete while running: %v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	// The next tick runs both due tasks, one after the other.
	h.s.RunDue(ctx)
	if p := peak.Load(); p != 1 {
		t.Fatalf("peak concurrency %d", p)
	}
	if len(entered) != 2 { // a again (a run now leaves next_run alone) and b
		t.Fatalf("runs after the first: %d, want 2", len(entered))
	}
	if h.s.Running() != "" {
		t.Fatal("still running")
	}

	// Many concurrent callers: never more than one run.
	var wg sync.WaitGroup
	h.s.GC = func(context.Context, string) (int, int64, error) {
		if active.Add(1) > 1 {
			t.Error("two runs at once")
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
		return 0, 0, nil
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				h.s.RunNow(a.ID, "admin")
				h.s.RunCompact("admin")
			}
		}()
	}
	wg.Wait()
}

func TestRunNowUnknownTask(t *testing.T) {
	h := newHarness(t)
	if _, err := h.s.RunNow("nope", "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestCompactParamBlobStore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.db.CreateBlobStore(ctx, meta.BlobStore{Name: "fast", Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	// Empty param (all stores) canonicalises to {}.
	tk := h.create(t, Def{Name: "all", Type: TypeCompact, Schedule: "manual", Params: json.RawMessage(`{}`)})
	if tk.Params != "{}" {
		t.Fatalf("empty params = %q, want {}", tk.Params)
	}
	// A named, existing store is accepted and recorded.
	named := h.create(t, Def{Name: "one", Type: TypeCompact, Schedule: "manual", Params: json.RawMessage(`{"blobStore":"fast"}`)})
	if named.Params != `{"blobStore":"fast"}` {
		t.Fatalf("named params = %q", named.Params)
	}
	// An unknown store is a validation error.
	_, err := h.s.Create(ctx, Def{Name: "ghost", Type: TypeCompact, Schedule: "manual", Params: json.RawMessage(`{"blobStore":"ghost"}`)})
	var ve *repo.ValidationError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), `blob store "ghost" does not exist`) {
		t.Fatalf("expected unknown-store error, got %v", err)
	}
}

// TestCompactRunsNamedStore checks runCompact routes the param to the GC.
func TestCompactRunsNamedStore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.db.CreateBlobStore(ctx, meta.BlobStore{Name: "fast", Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	var gotStore string
	h.s.GC = func(_ context.Context, store string) (int, int64, error) { gotStore = store; return 1, 0, nil }
	tk := h.create(t, Def{Name: "one", Type: TypeCompact, Schedule: "manual", Params: json.RawMessage(`{"blobStore":"fast"}`)})
	if _, err := h.s.RunNow(tk.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if gotStore != "fast" {
		t.Fatalf("GC called with store %q, want fast", gotStore)
	}
	// Empty param compacts every store.
	all := h.create(t, Def{Name: "all", Type: TypeCompact, Schedule: "manual"})
	if _, err := h.s.RunNow(all.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if gotStore != "" {
		t.Fatalf("GC called with store %q, want empty (all)", gotStore)
	}
}

func TestRunFailureAndPanic(t *testing.T) {
	h := newHarness(t)
	tk := h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "manual"})
	h.s.GC = func(context.Context, string) (int, int64, error) { return 1, 10, errors.New("disk on fire") }
	out, err := h.s.RunNow(tk.ID, "admin")
	if err != nil || out.Result != ResultFailed || !strings.Contains(out.Message, "disk on fire") || out.Removed != 1 || out.Bytes != 10 {
		t.Fatalf("failed run: %+v %v", out, err)
	}
	if got := h.get(t, tk.ID); got.LastResult != ResultFailed || !strings.Contains(got.LastLog, "disk on fire") || !got.NextRun.IsZero() {
		t.Fatalf("recorded: %+v", got)
	}
	h.s.GC = func(context.Context, string) (int, int64, error) { panic("boom") }
	if out, err := h.s.RunNow(tk.ID, "admin"); err != nil || out.Result != ResultFailed || !strings.Contains(out.Message, "panic: boom") {
		t.Fatalf("panic run: %+v %v", out, err)
	}
	if h.s.Running() != "" {
		t.Fatal("still running after a panic")
	}
	h.s.GC = func(context.Context, string) (int, int64, error) { return 0, 0, nil }
	if out, err := h.s.RunNow(tk.ID, "admin"); err != nil || out.Result != ResultOK {
		t.Fatalf("after panic: %+v %v", out, err)
	}
	evs := h.sink.Events()
	if len(evs) != 3 || evs[0].Detail != "Failed: disk on fire (removed 1 blobs (10 bytes))" || !strings.HasPrefix(evs[1].Detail, "Failed: panic: boom") {
		t.Fatalf("events: %v", evs)
	}
}

func TestRunTimeout(t *testing.T) {
	h := newHarness(t)
	h.s.RunTimeout = 20 * time.Millisecond
	h.s.GC = func(ctx context.Context, _ string) (int, int64, error) { <-ctx.Done(); return 0, 0, ctx.Err() }
	tk := h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "manual"})
	out, _ := h.s.RunNow(tk.ID, "admin")
	if out.Result != ResultFailed || !strings.Contains(out.Message, "run timeout") {
		t.Fatalf("timeout: %+v", out)
	}
}

// The compact alias records on the compact task, or runs unrecorded when
// there is none.
func TestRunCompact(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	out, err := h.s.RunCompact("admin")
	if err != nil || out.TaskID != SeedID || out.Removed != 3 {
		t.Fatalf("alias: %+v %v", out, err)
	}
	if got := h.get(t, SeedID); got.LastResult != ResultOK || got.LastRemoved != 3 {
		t.Fatalf("seed task after alias: %+v", got)
	}
	if err := h.db.DeleteTask(ctx, SeedID); err != nil {
		t.Fatal(err)
	}
	out, err = h.s.RunCompact("admin")
	if err != nil || out.TaskID != TypeCompact || out.Result != ResultOK || h.gcs.Load() != 2 {
		t.Fatalf("alias without a task: %+v %v", out, err)
	}
}

func TestSeedOnFirstStartOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	tk := h.get(t, SeedID)
	if tk.Type != TypeCompact || tk.Schedule != "daily:03:00" || !tk.Enabled || !tk.NextRun.Equal(time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("seed: %+v", tk)
	}
	if err := h.db.DeleteTask(ctx, SeedID); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if list, _ := h.s.List(ctx); len(list) != 0 {
		t.Fatalf("seeded again: %+v", list)
	}
}

func TestCleanupTask(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// 1.0 is old (created 60 days ago), 2.0 is new.
	h.clock.Set(t0.AddDate(0, 0, -60))
	h.put(t, "mvn", "org/example/app/1.0/app-1.0.jar", "old-jar")
	h.put(t, "mvn", "org/example/app/1.0/app-1.0.pom", "old-pom!")
	h.put(t, "mvn-lost-policy", "org/example/app/1.0/app-1.0.jar", "kept")
	h.clock.Set(t0)
	h.put(t, "mvn", "org/example/app/2.0/app-2.0.jar", "new-jar")

	tk := h.create(t, Def{Name: "Cleanup", Type: TypeCleanup, Schedule: "manual"}) // every repository with a policy
	if tk.Params != `{"repos":[]}` {
		t.Fatalf("params: %s", tk.Params)
	}
	h.sink.Reset()
	out, err := h.s.RunNow(tk.ID, "admin")
	if err != nil || out.Result != ResultOK || out.Removed != 1 || out.Bytes != int64(len("old-jar")+len("old-pom!")) {
		t.Fatalf("cleanup: %+v %v", out, err)
	}
	as, _ := h.db.ListAssets(ctx, "mvn", "org/example/app/")
	var paths []string
	for _, a := range as {
		paths = append(paths, a.Path)
	}
	if got := strings.Join(paths, ","); got != "org/example/app/2.0/app-2.0.jar,org/example/app/maven-metadata.xml" {
		t.Fatalf("assets left: %s", got)
	}
	if _, err := h.db.GetAsset(ctx, "mvn-lost-policy", "org/example/app/1.0/app-1.0.jar"); err != nil {
		t.Fatalf("repository with a missing policy was cleaned: %v", err)
	}
	got := h.get(t, tk.ID)
	if got.LastRemoved != 1 || got.LastBytes != out.Bytes || !strings.Contains(got.LastLog, `cleanup policy "gone" does not exist`) {
		t.Fatalf("recorded: %+v", got)
	}
	var cleanupEv, runEv, deleteEv int
	for _, ev := range h.sink.Events() {
		switch ev.Kind {
		case "cleanup":
			cleanupEv++
			if ev.Actor != "admin" || ev.Subject != tk.ID || ev.Detail != fmt.Sprintf("components=1 assets=2 bytes=%d", out.Bytes) {
				t.Errorf("cleanup event: %+v", ev)
			}
		case "task.run":
			runEv++
		case "delete":
			deleteEv++
		}
	}
	if cleanupEv != 1 || runEv != 1 || deleteEv != 1 {
		t.Fatalf("events: %s", h.sink)
	}

	// The per-run limit: two old components, one per run.
	h.clock.Set(t0.AddDate(0, 0, -60))
	h.put(t, "mvn", "org/example/lib/1.0/lib-1.0.jar", "a")
	h.put(t, "mvn", "org/example/lib/1.1/lib-1.1.jar", "b")
	h.clock.Set(t0)
	h.s.MaxComponents = 1
	only := h.create(t, Def{Name: "Cleanup mvn", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn"]}`)})
	for i, wantLeft := range []int{2, 0} {
		out, _ := h.s.RunNow(only.ID, "admin")
		if out.Removed != 1 {
			t.Fatalf("run %d: %+v", i, out)
		}
		if as, _ := h.db.ListAssets(ctx, "mvn", "org/example/lib/"); len(as) != wantLeft {
			t.Fatalf("run %d: %d lib assets left", i, len(as))
		}
	}
}

func TestRebuildMetadataTask(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.put(t, "mvn", "org/example/app/1.0/app-1.0.jar", "jar")
	h.put(t, "mvn", "org/example/app/1.1/app-1.1.jar", "jar")
	// No maven-metadata.xml: it was removed (or never written).
	if _, err := h.db.GetAsset(ctx, "mvn", "org/example/app/maven-metadata.xml"); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("metadata before: %v", err)
	}
	tk := h.create(t, Def{Name: "Rebuild", Type: TypeRebuild, Schedule: "manual", Params: json.RawMessage(`{"repo":"mvn"}`)})
	out, err := h.s.RunNow(tk.ID, "admin")
	if err != nil || out.Result != ResultOK || !strings.Contains(out.Message, "rebuilt 1 GA") {
		t.Fatalf("rebuild: %+v %v", out, err)
	}
	a, err := h.db.GetAsset(ctx, "mvn", "org/example/app/maven-metadata.xml")
	if err != nil {
		t.Fatalf("metadata after: %v", err)
	}
	f, err := h.store.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := maven.ParseMetadata(f)
	if err != nil || strings.Join(m.Versioning.Versions, ",") != "1.0,1.1" || m.Versioning.Release != "1.1" {
		t.Fatalf("metadata: %+v %v", m, err)
	}
}

// QA L-12: a rebuild of a Maven proxy deletes the files older builds cached
// there at paths that are not Maven files, which leaves their blobs to
// blobstore.compact.
func TestRebuildMetadataTaskPurgesProxy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.put(t, "mvn-proxy", "org/up/lib/1.0", "<html>Index of /org/up/lib/1.0</html>")
	h.put(t, "mvn-proxy", ".index/nexus-maven-repository-index.gz", "index")
	h.put(t, "mvn-proxy", "org/up/lib/1.0/lib-1.0.jar", "jar")
	listing, err := h.db.GetAsset(ctx, "mvn-proxy", "org/up/lib/1.0")
	if err != nil {
		t.Fatal(err)
	}
	tk := h.create(t, Def{Name: "Purge", Type: TypeRebuild, Schedule: "manual", Params: json.RawMessage(`{"repo":"mvn-proxy"}`)})
	out, err := h.s.RunNow(tk.ID, "admin")
	if err != nil || out.Result != ResultOK || out.Removed != 2 || out.Message != "mvn-proxy: removed 2 cached files that are not Maven files" {
		t.Fatalf("run: %+v %v", out, err)
	}
	if as, err := h.db.ListAssets(ctx, "mvn-proxy", ""); err != nil || len(as) != 1 || as[0].Path != "org/up/lib/1.0/lib-1.0.jar" {
		t.Fatalf("left: %+v %v", as, err)
	}
	if used, err := h.db.BlobInUse(ctx, listing.BlobSHA256); err != nil || used {
		t.Fatalf("listing blob in use: %v %v", used, err)
	}
}

func TestValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cases := []struct {
		d    Def
		want string
	}{
		{Def{Type: TypeCompact, Schedule: "manual"}, "name is required"},
		{Def{Name: strings.Repeat("x", 101), Type: TypeCompact, Schedule: "manual"}, "at most 100"},
		{Def{Name: "x", Type: "nope", Schedule: "manual"}, "unknown type"},
		{Def{Name: "x", Type: TypeCompact, Schedule: "daily:25:00"}, "invalid schedule"},
		{Def{Name: "x", Type: TypeCompact, Schedule: "once:2026-10-19T00:00:00Z"}, "in the past"},
		{Def{Name: "x", Type: TypeCompact, Schedule: "manual", Params: json.RawMessage(`{"x":1}`)}, "invalid params"},
		{Def{Name: "x", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["missing"]}`)}, `"missing" does not exist`},
		{Def{Name: "x", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn-group"]}`)}, "is a group"},
		{Def{Name: "x", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["crates"]}`)}, "Cargo hosted"},
		{Def{Name: "x", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":"mvn"}`)}, "invalid params"},
		{Def{Name: "x", Type: TypeRebuild, Schedule: "manual"}, "params.repo is required"},
		{Def{Name: "x", Type: TypeRebuild, Schedule: "manual", Params: json.RawMessage(`{"repo":"missing"}`)}, "does not exist"},
		{Def{Name: "x", Type: TypeRebuild, Schedule: "manual", Params: json.RawMessage(`{"repo":"mvn-group"}`)}, "Maven hosted or proxy repository"},
	}
	for _, c := range cases {
		_, err := h.s.Create(ctx, c.d)
		var ve *repo.ValidationError
		if !errors.As(err, &ve) || !strings.Contains(strings.Join(ve.Errors, "; "), c.want) {
			t.Errorf("%+v: got %v, want ~%q", c.d, err, c.want)
		}
	}
	// Canonical forms are stored.
	tk := h.create(t, Def{Name: "  Clean  ", Type: TypeCleanup, Schedule: " weekly:SAT:4:30 ", Params: json.RawMessage(`{"repos":["mvn-proxy","mvn","mvn"]}`)})
	if tk.Name != "Clean" || tk.Schedule != "weekly:sat:04:30" || tk.Params != `{"repos":["mvn","mvn-proxy"]}` {
		t.Fatalf("canonical: %+v", tk)
	}
	// Update: the type is fixed, enabled is kept unless given, next run follows.
	if _, err := h.s.Update(ctx, tk.ID, Def{Name: "x", Type: TypeCompact, Schedule: "manual"}); err == nil {
		t.Fatal("type change accepted")
	}
	off := false
	up, err := h.s.Update(ctx, tk.ID, Def{Name: "Clean 2", Schedule: "daily:05:00", Params: json.RawMessage(`{"repos":["mvn"]}`), Enabled: &off})
	if err != nil || up.Enabled || !up.NextRun.IsZero() || up.Type != TypeCleanup {
		t.Fatalf("disable: %+v %v", up, err)
	}
	up, err = h.s.Update(ctx, tk.ID, Def{Name: "Clean 2", Schedule: "daily:05:00", Params: json.RawMessage(`{"repos":["mvn"]}`)})
	if err != nil || up.Enabled || !up.NextRun.IsZero() {
		t.Fatalf("enabled kept: %+v %v", up, err)
	}
	on := true
	up, err = h.s.Update(ctx, tk.ID, Def{Name: "Clean 2", Schedule: "daily:05:00", Params: json.RawMessage(`{"repos":["mvn"]}`), Enabled: &on})
	if err != nil || !up.Enabled || !up.NextRun.Equal(time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("enable: %+v %v", up, err)
	}
	if _, err := h.s.Update(ctx, "nope", Def{Name: "x", Schedule: "manual"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
}

func TestSchedulerLoop(t *testing.T) {
	h := newHarness(t)
	ticks := make(chan time.Time)
	h.s.Ticks = ticks
	ran := make(chan struct{}, 1)
	h.s.GC = func(context.Context, string) (int, int64, error) { ran <- struct{}{}; return 0, 0, nil }
	h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "hourly:30"})
	h.clock.Set(t0.Add(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.s.Start(ctx)
	ticks <- time.Time{}
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the tick did not run the due task")
	}
	h.s.Stop()
	select {
	case ticks <- time.Time{}:
		t.Fatal("the loop still runs after Stop")
	case <-time.After(50 * time.Millisecond):
	}
}

// Stop cancels a run in progress.
func TestStopCancelsRun(t *testing.T) {
	h := newHarness(t)
	h.s.Ticks = make(chan time.Time)
	entered := make(chan struct{})
	h.s.GC = func(ctx context.Context, _ string) (int, int64, error) {
		close(entered)
		<-ctx.Done()
		return 0, 0, ctx.Err()
	}
	tk := h.create(t, Def{Name: "c", Type: TypeCompact, Schedule: "manual"})
	h.s.Start(context.Background())
	done := make(chan Outcome)
	go func() { out, _ := h.s.RunNow(tk.ID, "admin"); done <- out }()
	<-entered
	h.s.Stop()
	if out := <-done; out.Result != ResultFailed || !strings.Contains(out.Message, "shutdown") {
		t.Fatalf("after stop: %+v", out)
	}
}

// countHandler counts the records slog receives.
type countHandler struct{ n atomic.Int32 }

func (c *countHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (c *countHandler) Handle(context.Context, slog.Record) error { c.n.Add(1); return nil }
func (c *countHandler) WithAttrs([]slog.Attr) slog.Handler        { return c }
func (c *countHandler) WithGroup(string) slog.Handler             { return c }
