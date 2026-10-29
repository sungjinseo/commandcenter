package meta

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"commandcenter/internal/logsafe"
)

// The node registry: every serving process has a row in nodes, renewed
// from the database clock, and the live nodes compete for leases. On
// PostgreSQL any number of nodes serve one database (high availability);
// SQLite is one process's file.

// HeartbeatEvery is how often a node renews its row; it counts as alive for
// NodeTTL after the last renewal, and a lease lasts NodeTTL too. Vars for
// tests.
var (
	HeartbeatEvery = 10 * time.Second
	NodeTTL        = 30 * time.Second
)

// nodePrune is the age of a heartbeat after which the leader deletes the row.
const nodePrune = 24 * time.Hour

// schedulerLease is the lease of the node that runs the singleton work
// (reported as "leader").
const schedulerLease = "scheduler"

// Node is a registered server process.
type Node struct {
	ID, Hostname, Version  string
	Schema                 int // the schemaVersion of its build
	StartedAt, HeartbeatAt time.Time
	Alive                  bool   // the heartbeat is newer than NodeTTL
	Draining               bool   // shutting down: readiness answers 503
	Leader                 bool   // holds the scheduler lease
	RunningTask            string // the task it marks as running (tasks.running_node), "" if none
}

// NewNodeID returns a node id: the host name and 4 random hex digits, new
// for every start.
func NewNodeID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "node"
	}
	b := make([]byte, 2)
	rand.Read(b)
	return host + "-" + hex.EncodeToString(b)
}

// dbNow is the database clock in Unix ms: leases and heartbeats never
// compare the clocks of two hosts.
func dbNow(q querier) string {
	return pg(q, `CAST(unixepoch('subsec') * 1000 AS INTEGER)`, `(extract(epoch from clock_timestamp()) * 1000)::bigint`)
}

// Member is this process's registration, from Join until Leave.
type Member struct {
	d                     *DB
	id, hostname, version string
	log                   *slog.Logger

	draining     atomic.Bool
	leader       atomic.Bool
	authPruned   time.Time              // the last pruneAuthState; beat runs on one goroutine at a time
	incompatible atomic.Pointer[string] // why the database schema is not this build's, or nil

	stop  context.CancelFunc
	done  chan struct{}
	leave sync.Once
}

// Join registers this process as node id beside any others on the
// database and renews its row every HeartbeatEvery until Leave; on
// PostgreSQL it then opens the lock connection and the listener. A server
// stopped while it starts still registers, then leaves.
func (d *DB) Join(ctx context.Context, id, hostname, version string, log *slog.Logger) (*Member, error) {
	m := &Member{d: d, id: id, hostname: hostname, version: version, log: log}
	dbctx, now := context.WithoutCancel(ctx), dbNow(d.sql)
	if _, err := d.sql.execFree(dbctx, `INSERT INTO nodes(id, hostname, version, schema, started_at, heartbeat_at, draining)
VALUES(?, ?, ?, ?, `+now+`, `+now+`, 0)`, id, hostname, version, schemaVersion); err != nil {
		return nil, fmt.Errorf("register node %s: %w", id, err)
	}
	if d.cl != nil {
		if err := d.cl.pingLock(); err != nil {
			m.unregister()
			return nil, fmt.Errorf("open the lock connection: %w", err)
		}
		d.cl.listen()
	}
	if err := m.beat(dbctx); err != nil { // the lease and the schema check, at once
		log.Warn("could not renew this node's registration", "err", logsafe.Err(err))
	}
	hctx, stop := context.WithCancel(context.Background())
	m.stop, m.done = stop, make(chan struct{})
	go m.run(hctx)
	d.self.Store(m)
	return m, nil
}

// run renews the registration every HeartbeatEvery until ctx ends. A
// failed renewal is logged once per run of failures.
func (m *Member) run(ctx context.Context) {
	defer close(m.done)
	t := time.NewTicker(HeartbeatEvery)
	defer t.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := m.beat(ctx)
		if err != nil && !failing && ctx.Err() == nil {
			m.log.Warn("could not renew this node's registration; other nodes see it as gone after "+NodeTTL.String(), "err", logsafe.Err(err))
		}
		failing = err != nil
	}
}

// beat renews the row (recreating it if it went), renews or takes the
// scheduler lease, prunes old rows as leader and re-reads the schema
// version and compat, turning the node unready when compat rose above this
// build's schemaVersion.
func (m *Member) beat(ctx context.Context) error {
	d := m.d
	now := dbNow(d.sql)
	draining := 0
	if m.draining.Load() {
		draining = 1
	}
	if _, err := d.sql.execFree(ctx, `INSERT INTO nodes(id, hostname, version, schema, started_at, heartbeat_at, draining)
VALUES(?, ?, ?, ?, `+now+`, `+now+`, ?)
ON CONFLICT(id) DO UPDATE SET heartbeat_at = excluded.heartbeat_at, draining = excluded.draining`,
		m.id, m.hostname, m.version, schemaVersion, draining); err != nil {
		return err
	}
	leader, err := d.acquireLease(ctx, schedulerLease, m.id)
	if err != nil {
		return err
	}
	m.leader.Store(leader)
	if leader {
		if _, err := d.sql.execFree(ctx, `DELETE FROM nodes WHERE heartbeat_at < `+now+` - ?`, nodePrune.Milliseconds()); err != nil {
			return err
		}
		if t := time.Now(); t.Sub(m.authPruned) >= authPruneEvery {
			if err := d.pruneAuthState(ctx, t); err != nil {
				return err
			}
			m.authPruned = t
		}
	}
	version, compat, err := readSchema(ctx, d.sql)
	if err != nil {
		return err
	}
	if compat > schemaVersion {
		why := fmt.Sprintf("the database is at schema %d, which builds of schema %d and newer run on; this build is schema %d: stop it and run the newer version", version, compat, schemaVersion)
		if old := m.incompatible.Swap(&why); old == nil {
			m.log.Error("the database was upgraded by a newer version this build cannot run beside; this node is no longer ready", "reason", why)
		}
	} else {
		m.incompatible.Store(nil)
	}
	return nil
}

// acquireLease takes or renews lease name for holder, for NodeTTL by the
// database clock, and reports whether holder has it.
func (d *DB) acquireLease(ctx context.Context, name, holder string) (bool, error) {
	now := dbNow(d.sql)
	var got string
	err := d.sql.QueryRowContext(ctx, `INSERT INTO leases(name, holder, expires_at) VALUES(?, ?, `+now+` + ?)
ON CONFLICT(name) DO UPDATE SET holder = excluded.holder, expires_at = excluded.expires_at
WHERE leases.holder = excluded.holder OR leases.expires_at < `+now+`
RETURNING holder`, name, holder, NodeTTL.Milliseconds()).Scan(&got) // a free write: no invariant reads leases
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Leader reports whether this node holds the scheduler lease.
func (m *Member) Leader() bool { return m.leader.Load() }

// Leader reports whether this process does the work one node does for all
// (scheduled task runs, prunes): on PostgreSQL, once it joined, while it
// holds the scheduler lease; always on SQLite (one process) and in a
// process that did not join (a CLI command, a test).
func (d *DB) Leader() bool {
	m := d.self.Load()
	return d.cl == nil || m == nil || m.Leader()
}

// NodeID is this process's node id once it joined, else "".
func (d *DB) NodeID() string {
	if m := d.self.Load(); m != nil {
		return m.id
	}
	return ""
}

// Drain marks the node as shutting down: Ready fails from now on, and the
// row says draining.
func (m *Member) Drain(ctx context.Context) {
	m.draining.Store(true)
	if _, err := m.d.sql.execFree(ctx, `UPDATE nodes SET draining = 1 WHERE id = ?`, m.id); err != nil {
		m.log.Warn("could not mark this node as draining", "err", logsafe.Err(err))
	}
}

// Leave stops the heartbeat, deletes the node's row and gives up its
// leases. The node stays draining. Calling it again does nothing.
func (m *Member) Leave(ctx context.Context) {
	m.leave.Do(func() {
		m.draining.Store(true)
		m.stopHeartbeat()
		m.unregister()
	})
}

func (m *Member) stopHeartbeat() {
	if m.stop != nil {
		m.stop()
		<-m.done
	}
}

func (m *Member) unregister() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := m.d.sql.execFree(ctx, `DELETE FROM nodes WHERE id = ?`, m.id)
	if err == nil {
		_, err = m.d.sql.execFree(ctx, `DELETE FROM leases WHERE holder = ?`, m.id)
	}
	if err != nil {
		m.log.Warn("could not remove this node's registration; it expires "+NodeTTL.String()+" after its last heartbeat", "err", logsafe.Err(err))
	}
}

// Ready reports whether this process should take requests (the readiness
// of GET /service/rest/v1/status/writable): the database answers and, once
// Join ran, the node is not draining, the schema is one this build runs
// on, and on PostgreSQL the lock connection and the listener are up.
func (d *DB) Ready(ctx context.Context) error {
	if err := d.Ping(ctx); err != nil {
		return err
	}
	m := d.self.Load()
	if m == nil {
		return nil
	}
	if m.draining.Load() {
		return errors.New("this node is draining (shutting down)")
	}
	if why := m.incompatible.Load(); why != nil {
		return errors.New(*why)
	}
	if d.cl != nil {
		if err := d.cl.pingLock(); err != nil {
			return fmt.Errorf("lock connection: %w", err)
		}
		if !d.cl.up.Load() {
			return errors.New("the change listener (LISTEN " + notifyChannel + ") is not connected")
		}
	}
	return nil
}

// Nodes returns the registered nodes, oldest first.
func (d *DB) Nodes(ctx context.Context) ([]Node, error) {
	now := dbNow(d.sql)
	rows, err := d.sql.QueryContext(ctx, `SELECT id, hostname, version, schema, started_at, heartbeat_at, draining, `+now+`,
  COALESCE((SELECT holder FROM leases WHERE name = ? AND expires_at > `+now+`), ''),
  COALESCE((SELECT t.id FROM tasks t WHERE t.running_node = nodes.id ORDER BY t.running_since DESC, t.id LIMIT 1), '')
FROM nodes ORDER BY started_at, id`, schedulerLease)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		var started, hb, at int64
		var draining int
		var leader string
		if err := rows.Scan(&n.ID, &n.Hostname, &n.Version, &n.Schema, &started, &hb, &draining, &at, &leader, &n.RunningTask); err != nil {
			return nil, err
		}
		n.StartedAt, n.HeartbeatAt = fromMs(started), fromMs(hb)
		n.Alive = at-hb < NodeTTL.Milliseconds()
		n.Draining, n.Leader = draining != 0, leader == n.ID
		out = append(out, n)
	}
	return out, rows.Err()
}
