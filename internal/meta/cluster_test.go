package meta

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/meta/metatest"
)

// twoNodes opens one database twice, as two server processes would.
func twoNodes(t *testing.T) (a, b *DB, target string) {
	t.Helper()
	target = metatest.URL(t, t.TempDir())
	open := func() *DB {
		db, err := Open(target)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	return open(), open(), target
}

// shortNodeTimes makes heartbeats and leases fast for the test.
func shortNodeTimes(t *testing.T, heartbeat, ttl time.Duration) {
	t.Helper()
	hb, nt := HeartbeatEvery, NodeTTL
	HeartbeatEvery, NodeTTL = heartbeat, ttl
	t.Cleanup(func() { HeartbeatEvery, NodeTTL = hb, nt }) // registered first, runs after the handles closed
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// 50 goroutines on one key, alternating between two nodes: never two at once.
func TestLockExcludesAcrossNodes(t *testing.T) {
	a, b, _ := twoNodes(t)
	var cur, peak atomic.Int32
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := a
			if i%2 == 1 {
				d = b
			}
			unlock, err := d.Lock(ctx, "k")
			if err != nil {
				t.Error(err)
				return
			}
			n := cur.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			time.Sleep(time.Millisecond)
			cur.Add(-1)
			unlock()
		}()
	}
	wg.Wait()
	if p := peak.Load(); p != 1 {
		t.Fatalf("up to %d holders of one key at once, want 1", p)
	}
	// Another key does not wait for a held one.
	unlock, err := a.Lock(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	other, err := b.Lock(tctx, "other")
	if err != nil {
		t.Fatalf("a free key waited for a held one: %v", err)
	}
	other()
}

// LockAll takes overlapping key sets named in any order from two nodes
// without deadlocking, excludes the holders of each key, and frees every
// in-process entry afterwards.
func TestLockAll(t *testing.T) {
	a, b, _ := twoNodes(t)
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var cur, peak atomic.Int32
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, keys := a, []string{"x", "y", "z", "x"}
			if i%2 == 1 {
				d, keys = b, []string{"z", "y"}
			}
			unlock, err := d.LockAll(tctx, keys)
			if err != nil {
				t.Error(err)
				return
			}
			n := cur.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			time.Sleep(time.Millisecond)
			cur.Add(-1)
			unlock()
		}()
	}
	wg.Wait()
	if p := peak.Load(); p != 1 {
		t.Fatalf("up to %d holders of y at once, want 1", p)
	}
	for _, d := range []*DB{a, b} {
		d.keys.mu.Lock()
		n := len(d.keys.m)
		d.keys.mu.Unlock()
		if n != 0 {
			t.Fatalf("%d in-process lock entries left", n)
		}
	}
}

// A waiter whose context ends gives up, in the process and across nodes,
// and leaves the lock usable.
func TestLockCancelWhileWaiting(t *testing.T) {
	a, b, _ := twoNodes(t)
	unlock, err := a.Lock(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]*DB{"same process": a, "other node": b} {
		tctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		start := time.Now()
		_, err := d.Lock(tctx, "k")
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
			t.Fatalf("%s: Lock = %v after %v, want the deadline", name, err, time.Since(start))
		}
	}
	unlock()
	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	again, err := b.Lock(tctx, "k")
	if err != nil {
		t.Fatalf("after the cancelled waits: %v", err)
	}
	again()
}

// A second unlock does nothing: it does not release the lock of whoever
// took the key since.
func TestUnlockTwice(t *testing.T) {
	a, b, _ := twoNodes(t)
	first, err := a.Lock(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	first()
	first()
	second, err := a.Lock(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	first()
	tctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := b.Lock(tctx, "k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock after a stale unlock = %v, want it to wait for the holder", err)
	}
}

// pgBackendGone waits until pid has left pg_stat_activity.
func pgBackendGone(t *testing.T, d *DB, pid uint32) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var n int
		if err := d.sql.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = ?`, int64(pid)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %d is still there", pid)
		}
	}
}

func terminate(t *testing.T, d *DB, pid uint32) {
	t.Helper()
	var ok bool
	if err := d.sql.QueryRowContext(ctx, `SELECT pg_terminate_backend(?)`, int64(pid)).Scan(&ok); err != nil || !ok {
		t.Fatalf("pg_terminate_backend(%d) = %v, %v", pid, ok, err)
	}
	pgBackendGone(t, d, pid)
}

// The server ends the lock connection: its locks go with it (the
// documented ceiling), the node reconnects for the next Lock, and an unlock
// from the old connection does not release a lock taken since.
func TestLockConnectionTerminated(t *testing.T) {
	a, b, _ := twoNodes(t)
	stale, err := a.Lock(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	a.cl.mu.Lock()
	pid := a.cl.conn.PgConn().PID()
	a.cl.mu.Unlock()
	terminate(t, b, pid)

	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	held, err := b.Lock(tctx, "k1") // the session that held it is gone
	if err != nil {
		t.Fatalf("k1 after its session ended: %v", err)
	}
	unlock, err := a.Lock(tctx, "k2")
	if err != nil {
		t.Fatalf("Lock after the lock connection was terminated: %v", err)
	}
	unlock()
	stale() // releases the in-process mutex only
	short, cancelShort := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancelShort()
	if _, err := a.Lock(short, "k1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("k1 while the other node holds it: %v", err)
	}
	held()
	again, err := a.Lock(tctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	again()
}

// The scheduler lease moves to another node within NodeTTL of its holder's
// last renewal, and the old holder cannot renew it then.
func TestLeaseFailover(t *testing.T) {
	shortNodeTimes(t, 50*time.Millisecond, 500*time.Millisecond)
	a, b, _ := twoNodes(t)
	for _, c := range []struct {
		d      *DB
		holder string
		want   bool
	}{{a, "A", true}, {b, "B", false}, {a, "A", true}} {
		if got, err := c.d.acquireLease(ctx, schedulerLease, c.holder); err != nil || got != c.want {
			t.Fatalf("%s: %v, %v; want %v", c.holder, got, err, c.want)
		}
	}
	renewed := time.Now()
	for {
		got, err := b.acquireLease(ctx, schedulerLease, "B")
		if err != nil {
			t.Fatal(err)
		}
		if got {
			break
		}
		if time.Since(renewed) > NodeTTL+time.Second {
			t.Fatal("B did not get the lease after A stopped renewing it")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got, err := a.acquireLease(ctx, schedulerLease, "A"); err != nil || got {
		t.Fatalf("A renewed a lease B holds: %v, %v", got, err)
	}
}

// A notification reaches the subscribers of its topic on every node; after
// the listener is killed it reconnects and calls every subscriber with ""
// (re-read everything), then delivers again.
func TestNotifyAndResync(t *testing.T) {
	a, b, _ := twoNodes(t)
	got := make(chan string, 10)
	a.Subscribe("t", func(arg string) { got <- arg })
	next := func(want string) {
		t.Helper()
		select {
		case arg := <-got:
			if arg != want {
				t.Fatalf("subscriber got %q, want %q", arg, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no call, want %q", want)
		}
	}
	next("") // the first connect re-reads too: changes before it are not missed
	for _, topic := range []string{"other", "t:x"} {
		if err := b.Notify(ctx, topic); err != nil {
			t.Fatal(err)
		}
	}
	next("x")
	terminate(t, b, a.cl.pid.Load())
	next("")
	if err := b.Notify(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	next("")
	if !a.cl.up.Load() {
		t.Fatal("the listener is not up after the reconnect")
	}
}

// makeV4 turns a fresh database into one as schema 4 left it: no nodes,
// leases or compat.
func makeV4(t *testing.T, target string) {
	t.Helper()
	raw := rawOpen(t, target)
	for _, q := range []string{`DROP TABLE nodes`, `DROP TABLE leases`, `ALTER TABLE schema_version DROP COLUMN compat`,
		`DELETE FROM schema_version`, `INSERT INTO schema_version(version) VALUES(4)`} {
		if _, err := raw.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
}

// Nodes that start at once on an empty or an older database upgrade it one
// after the other, and all start.
func TestConcurrentOpen(t *testing.T) {
	for _, v4 := range []bool{false, true} {
		target := metatest.URL(t, t.TempDir())
		if v4 {
			db, err := Open(target)
			if err != nil {
				t.Fatal(err)
			}
			db.Close()
			makeV4(t, target)
		}
		const n = 4
		errs := make(chan error, n)
		start := make(chan struct{})
		for range n {
			go func() {
				<-start
				db, err := Open(target)
				if err == nil {
					db.Close()
				}
				errs <- err
			}()
		}
		close(start)
		for range n {
			if err := <-errs; err != nil {
				t.Fatalf("v4 %v: %v", v4, err)
			}
		}
		version, compat, err := readSchema(ctx, rawOpen(t, target))
		if err != nil || version != schemaVersion || compat != schemaCompat {
			t.Fatalf("v4 %v: schema %d compat %d (%v)", v4, version, compat, err)
		}
	}
}

// The upgrade waits for another node's upgrade however long it takes,
// whatever lock_timeout the URL sets for the server's writes.
func TestOpenWaitsForTheMigrateLock(t *testing.T) {
	target := metatest.URL(t, t.TempDir())
	raw := rawOpen(t, target)
	tx, err := raw.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(migrateLock); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		db, err := Open(target + "&lock_timeout=100")
		if err == nil {
			db.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Open did not wait for the upgrade in progress: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	tx.Commit()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Every schema version's compat decision (schemaCompat): bumping
// schemaVersion fails this test until a row says which builds may run on
// the new version. compat is the version (a full stop: older builds refuse
// it) or the version - 1 (expand-only: the previous release still runs).
func TestSchemaCompat(t *testing.T) {
	decisions := []struct {
		version, compat int
		note            string
	}{
		{1, 1, "users, roles and settings in the database, named blob stores, sealed secrets"},
		{2, 2, "S3 blob stores: an older build would read their paths as directories"},
		{3, 3, "webhooks, routing rules and content selectors"},
		{4, 4, "user tokens, blob store groups, cleanup criteria, tags, store moves, vulnerability data"},
		{5, 5, "nodes, leases, compat: a full stop, as the single-writer guard must refuse a second instance, which older builds would not"},
		{6, 6, "task run marks, webhook delivery claims and repositories.gen (expand-only) together with sessions and auth state in the database, " +
			"the shared token key and the secret key check: a full stop, as an older build would keep sessions in memory and leave " +
			"stored sessions of a changed user valid for this one"},
		{7, 6, "azure and google blob stores: no DDL, and the first such store raises compat to 7 (an older build would read it as an empty directory); reconcile_plans and reconcile_items: expand-only, new tables an older build never reads"},
	}
	for i, d := range decisions {
		if d.version != i+1 || d.compat < d.version-1 || d.compat > d.version || d.note == "" {
			t.Errorf("decision %+v: want version %d, compat version or version-1, and a note", d, i+1)
		}
	}
	if last := decisions[len(decisions)-1]; last.version != schemaVersion || last.compat != schemaCompat {
		t.Fatalf("the last decision is %+v; schemaVersion %d with schemaCompat %d needs its row", last, schemaVersion, schemaCompat)
	}
}

// Compat both ways: a build starts on a newer database only when its compat
// allows it, an upgrade waits for no live node of a build below compat, and
// a running node turns unready when compat rises above its build.
func TestCompatRefusals(t *testing.T) {
	shortNodeTimes(t, time.Hour, time.Minute) // beat by hand
	target := metatest.URL(t, t.TempDir())
	db, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := db.Join(ctx, "n1", "host", "test", discard())
	if err != nil {
		t.Fatal(err)
	}
	raw := rawOpen(t, target)
	stamp := func(v, c int) {
		t.Helper()
		if err := stampSchemaVersion(ctx, raw, v, c); err != nil {
			t.Fatal(err)
		}
		if err := m.beat(ctx); err != nil {
			t.Fatal(err)
		}
	}
	stamp(schemaVersion+1, schemaVersion+1)
	if err := db.Ready(ctx); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("Ready on a database a newer build may not share: %v", err)
	}
	if _, err := Open(target); err == nil {
		t.Fatal("Open of a database whose compat is above this build")
	}
	stamp(schemaVersion+1, schemaVersion)
	if err := db.Ready(ctx); err != nil {
		t.Fatalf("Ready on a newer, expand-only database: %v", err)
	}
	newer, err := Open(target)
	if err != nil {
		t.Fatalf("Open of a newer, expand-only database: %v", err)
	}
	newer.Close()
	if v, c, err := readSchema(ctx, raw); err != nil || v != schemaVersion+1 || c != schemaVersion {
		t.Fatalf("the older build changed the stamp: %d %d %v", v, c, err)
	}

	// An upgrade while a live node of an older build is registered.
	stamp(schemaCompat-1, schemaCompat-1)
	if _, err := raw.db.Exec(`INSERT INTO nodes(id, hostname, version, schema, started_at, heartbeat_at) VALUES('old-1', 'h', 'old', $1, 1, `+
		dbNow+`)`, schemaCompat-1); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(target); err == nil || !strings.Contains(err.Error(), "old-1") || !strings.Contains(err.Error(), "Recreate") {
		t.Fatalf("upgrade beside an older live node: %v", err)
	}
	if _, err := raw.db.Exec(`UPDATE nodes SET heartbeat_at = 1 WHERE id = 'old-1'`); err != nil { // it stopped
		t.Fatal(err)
	}
	up, err := Open(target)
	if err != nil {
		t.Fatalf("upgrade after the older node stopped: %v", err)
	}
	up.Close()
	if v, c, err := readSchema(ctx, raw); err != nil || v != schemaVersion || c != schemaCompat {
		t.Fatalf("after the upgrade: %d %d %v", v, c, err)
	}
}

// Nodes join beside each other at once (high availability): both alive, one the leader; a
// node that crashed stops counting as alive after NodeTTL and keeps no one
// waiting.
func TestJoinBesideOthers(t *testing.T) {
	shortNodeTimes(t, 100*time.Millisecond, 700*time.Millisecond)
	a, b, _ := twoNodes(t)
	start := time.Now()
	first, err := a.Join(ctx, "first", "host-a", "1", discard())
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Join(ctx, "second", "host-b", "1", discard())
	if err != nil {
		t.Fatalf("second Join beside a live node: %v", err)
	}
	if d := time.Since(start); d > NodeTTL/2 {
		t.Fatalf("the joins took %v", d)
	}
	alive := func() (map[string]bool, int) {
		t.Helper()
		nodes, err := a.Nodes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out, leaders := map[string]bool{}, 0
		for _, n := range nodes {
			out[n.ID] = n.Alive
			if n.Leader {
				leaders++
			}
		}
		return out, leaders
	}
	if live, leaders := alive(); !live["first"] || !live["second"] || leaders != 1 || first.Leader() == second.Leader() {
		t.Fatalf("nodes %v with %d leaders (first %v, second %v)", live, leaders, first.Leader(), second.Leader())
	}
	second.stopHeartbeat() // a crash: the row stays
	time.Sleep(NodeTTL + 2*HeartbeatEvery)
	if live, _ := alive(); !live["first"] || live["second"] {
		t.Fatalf("nodes after the crash: %v", live)
	}
	third, err := b.Join(ctx, "third", "host-b", "1", discard())
	if err != nil {
		t.Fatal(err)
	}
	first.Leave(ctx)
	third.Leave(ctx)
}

// Ready follows the node: ready once joined (the lock connection and the
// listener up), not ready while draining, and the row and
// the lease go on Leave.
func TestReadyDrainLeave(t *testing.T) {
	shortNodeTimes(t, 50*time.Millisecond, time.Second)
	db := openTest(t)
	if err := db.Ready(ctx); err != nil {
		t.Fatalf("Ready before Join: %v", err)
	}
	m, err := db.Join(ctx, "n1", "host", "v9", discard())
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); db.Ready(ctx) != nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("not ready after Join: %v", db.Ready(ctx))
		}
	}
	nodes, err := db.Nodes(ctx)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes %+v, %v", nodes, err)
	}
	n := nodes[0]
	if n.ID != "n1" || n.Hostname != "host" || n.Version != "v9" || n.Schema != schemaVersion || !n.Alive || !n.Leader || n.Draining ||
		n.StartedAt.IsZero() || n.HeartbeatAt.Before(n.StartedAt) || !m.Leader() {
		t.Fatalf("node %+v", n)
	}
	m.Drain(ctx)
	if err := db.Ready(ctx); err == nil || !strings.Contains(err.Error(), "draining") {
		t.Fatalf("Ready while draining: %v", err)
	}
	time.Sleep(3 * HeartbeatEvery) // the heartbeat keeps the flag
	if nodes, _ := db.Nodes(ctx); len(nodes) != 1 || !nodes[0].Draining || !nodes[0].Alive {
		t.Fatalf("draining node %+v", nodes)
	}
	m.Leave(ctx)
	m.Leave(ctx)
	var leases int
	if nodes, err := db.Nodes(ctx); err != nil || len(nodes) != 0 {
		t.Fatalf("nodes after Leave: %+v, %v", nodes, err)
	}
	if err := db.sql.QueryRowContext(ctx, `SELECT count(*) FROM leases`).Scan(&leases); err != nil || leases != 0 {
		t.Fatalf("%d leases after Leave (%v)", leases, err)
	}
	if err := db.Ready(ctx); err == nil {
		t.Fatal("ready after Leave")
	}
}

// joinAll joins each handle as a node (servers on one database, as high
// availability runs them); the first handle takes the scheduler lease.
func joinAll(t *testing.T, dbs ...*DB) {
	t.Helper()
	for i, d := range dbs {
		if _, err := d.Join(ctx, "node-"+string(rune('a'+i)), "host", "1", discard()); err != nil {
			t.Fatal(err)
		}
	}
}

// TryLock never waits: it fails while the key is held in this process or
// on another node, and succeeds once it is free.
func TestTryLock(t *testing.T) {
	a, b, _ := twoNodes(t)
	unlock, ok, err := a.TryLock("k")
	if err != nil || !ok {
		t.Fatalf("free key: %v, %v", ok, err)
	}
	for name, d := range map[string]*DB{"same process": a, "other node": b} {
		if again, ok, err := d.TryLock("k"); err != nil || ok {
			if again != nil {
				again()
			}
			t.Fatalf("%s took a held key: %v, %v", name, ok, err)
		}
	}
	unlock()
	unlock()
	again, ok, err := b.TryLock("k")
	if err != nil || !ok {
		t.Fatalf("after unlock: %v, %v", ok, err)
	}
	again()
}

// Only the leader (the scheduler lease) prunes request statistics, the
// negative cache and events; another node's prunes delete nothing.
func TestLeaderAlonePrunes(t *testing.T) {
	a, b, _ := twoNodes(t)
	joinAll(t, a, b)
	if !a.Leader() || b.Leader() {
		t.Fatalf("leaders: a %v, b %v; want a only", a.Leader(), b.Leader())
	}
	now := time.Now()
	old := now.Add(-48*time.Hour).Unix() / 3600 * 3600
	if err := a.FlushRequests(ctx, []RequestDelta{{Hour: old, Repo: "r", Requests: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := a.PutNegative(ctx, "r", "gone", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := a.InsertEvents(ctx, []Event{{At: now, Kind: "k"}, {At: now, Kind: "k"}, {At: now, Kind: "k"}}); err != nil {
		t.Fatal(err)
	}
	prune := func(d *DB) int64 {
		t.Helper()
		var total int64
		for _, f := range []func() (int64, error){
			func() (int64, error) { return d.PruneRequestStats(ctx, now.Add(-24*time.Hour).Unix()) },
			func() (int64, error) { return d.PruneNegative(ctx, now) },
			func() (int64, error) { return d.PruneEvents(ctx, 1) },
		} {
			n, err := f()
			if err != nil {
				t.Fatal(err)
			}
			total += n
		}
		return total
	}
	if n := prune(b); n != 0 {
		t.Fatalf("a node that is not the leader pruned %d rows", n)
	}
	if n := prune(a); n != 4 { // one statistics hour, one negative entry, two events
		t.Fatalf("the leader pruned %d rows, want 4", n)
	}
}
