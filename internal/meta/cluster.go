package meta

import (
	"context"
	"hash/fnv"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// Cluster primitives. On PostgreSQL every process on the database is a
// node, and two connections of its own, besides the pool, coordinate them:
//
//   - the lock connection holds the session advisory locks of DB.Lock;
//   - the listener runs LISTEN cc_changed for Subscribe, so a node learns
//     that another one changed what it caches (DB.Notify).
//
// Both open when first needed and reopen after a failure. On SQLite, one
// process per file, Lock is the in-process mutex alone and Notify does
// nothing.
//
// Lock order: a caller never takes a Lock inside a write transaction (Tx).
// A Lock waiting on another node while holding the global write lock would
// stall every write on every node until lock_timeout.

// lockClass is the first key of DB.Lock's advisory locks ("CC" 1). The
// two-int4 form has a lock-tag space of its own, apart from the bigint
// writeLock and migrateLock.
const lockClass int32 = 0x43430001

// notifyChannel carries DB.Notify's topics.
const notifyChannel = "cc_changed"

// lockStmtTimeout bounds one statement on the lock connection. It runs
// under a context of its own, never the caller's: pgx closes a connection
// whose context ends mid-statement, and with it every lock the node holds.
const lockStmtTimeout = 5 * time.Second

// cluster is a PostgreSQL DB's lock connection and listener.
type cluster struct {
	cfg *pgx.ConnConfig // the pool's, plus TCP keepalives

	mu   sync.Mutex // guards conn and gen: one statement at a time
	conn *pgx.Conn  // the lock connection; nil until needed or after it died
	gen  uint64     // counts lock connections: a lock taken on an older one ended with it

	subMu   sync.Mutex
	subs    map[string][]func(arg string)
	started bool
	stop    context.CancelFunc
	done    chan struct{}
	up      atomic.Bool   // the listener is connected and listening
	pid     atomic.Uint32 // the listener's backend (tests)
}

func newCluster(cfg *pgx.ConnConfig) *cluster {
	cfg = cfg.Copy()
	// A node cut off from the server loses its session, and so its advisory
	// locks, within about 30 + 3 × 10 s: the server's keepalives give up on
	// it. The dialer's do the same for a server that went away.
	cfg.RuntimeParams["tcp_keepalives_idle"] = "30"
	cfg.RuntimeParams["tcp_keepalives_interval"] = "10"
	cfg.RuntimeParams["tcp_keepalives_count"] = "3"
	dialer := &net.Dialer{Timeout: cfg.ConnectTimeout,
		KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: 30 * time.Second, Interval: 10 * time.Second, Count: 3}}
	cfg.DialFunc = dialer.DialContext
	return &cluster{cfg: cfg, subs: map[string][]func(string){}}
}

func (c *cluster) connect(ctx context.Context) (*pgx.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return pgx.ConnectConfig(ctx, c.cfg)
}

// ensure opens the lock connection if it is not open. c.mu is held.
func (c *cluster) ensure() error {
	if c.conn != nil && !c.conn.IsClosed() {
		return nil
	}
	conn, err := c.connect(context.Background())
	if err != nil {
		return err
	}
	c.conn, c.gen = conn, c.gen+1
	return nil
}

// tryLock asks once for the advisory lock (lockClass, key) and returns the
// connection generation that holds it. A connection the server ended (an
// administrator, a restart) is replaced and asked again once.
func (c *cluster) tryLock(key int32) (bool, uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	for range 2 {
		if err = c.ensure(); err != nil {
			return false, 0, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), lockStmtTimeout)
		var ok bool
		err = c.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, lockClass, key).Scan(&ok)
		cancel()
		if err == nil {
			return ok, c.gen, nil
		}
		if !c.conn.IsClosed() { // a statement error: the session and its locks live on
			return false, 0, err
		}
		c.conn = nil
	}
	return false, 0, err
}

// unlock releases a lock taken on connection generation gen. One taken on
// a connection that has since died ended with it.
func (c *cluster) unlock(gen uint64, key int32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil || c.conn.IsClosed() || c.gen != gen {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockStmtTimeout)
	defer cancel()
	if _, err := c.conn.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, lockClass, key); err != nil {
		// A lock that may still be held would block every node for good;
		// closing the session ends it (and this node's other locks).
		c.conn.Close(ctx)
		c.conn = nil
	}
}

// pingLock checks the lock connection, opening it if needed.
func (c *cluster) pingLock() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensure(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.conn.Ping(ctx)
	if err != nil && c.conn.IsClosed() {
		c.conn = nil
	}
	return err
}

// listen starts the listener once.
func (c *cluster) listen() {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if c.started {
		return
	}
	c.started = true
	ctx, cancel := context.WithCancel(context.Background())
	c.stop, c.done = cancel, make(chan struct{})
	go c.listenLoop(ctx)
}

// listenLoop keeps a LISTEN connection until ctx ends. After every
// (re)connect it calls every subscriber with "": notifications sent while
// it was not listening are lost, so each re-reads what it caches.
func (c *cluster) listenLoop(ctx context.Context) {
	defer close(c.done)
	wait := time.Second
	for ctx.Err() == nil {
		conn, err := c.connect(ctx)
		if err == nil {
			if _, err = conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
				conn.Close(context.Background())
			}
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(2*wait, 10*time.Second)
			continue
		}
		wait = time.Second
		c.pid.Store(conn.PgConn().PID())
		c.up.Store(true)
		c.dispatch("")
		for {
			n, err := conn.WaitForNotification(ctx)
			if err != nil {
				break
			}
			c.dispatch(n.Payload)
		}
		c.up.Store(false)
		conn.Close(context.Background())
	}
}

// dispatch calls the subscribers of payload's topic ("topic" or
// "topic:arg") with its arg, or every subscriber with "" for "".
func (c *cluster) dispatch(payload string) {
	topic, arg, _ := strings.Cut(payload, ":")
	c.subMu.Lock()
	var fns []func(string)
	if payload == "" {
		for _, l := range c.subs {
			fns = append(fns, l...)
		}
	} else {
		fns = c.subs[topic]
	}
	c.subMu.Unlock()
	for _, fn := range fns {
		fn(arg)
	}
}

func (c *cluster) close() {
	c.subMu.Lock()
	stop, done := c.stop, c.done
	c.subMu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		c.conn.Close(ctx)
		cancel()
		c.conn = nil
	}
}

// Lock takes the lock named key for this process and, on PostgreSQL, for
// every node on the database: first an in-process mutex, then the session
// advisory lock (lockClass, hash32(key)) on the lock connection, asked
// every 10 to 250 ms (jittered) until it is free or ctx ends. Two keys with
// the same hash only wait for each other. unlock releases both; calling it
// again does nothing.
//
// ponytail: a lock connection that dies releases every lock of the node at
// once, and the holders are not told (no fencing); a holder that must know
// would check pg_locks for the connection's pid inside its transaction.
// ponytail: waiters poll; wake them with a NOTIFY on unlock if waiting
// across nodes shows up in latency.
func (d *DB) Lock(ctx context.Context, key string) (unlock func(), err error) {
	local, err := d.keys.lock(ctx, key)
	if err != nil {
		return nil, err
	}
	if d.cl == nil {
		var once sync.Once
		return func() { once.Do(local) }, nil
	}
	h := hash32(key)
	backoff := 10 * time.Millisecond
	for {
		ok, gen, err := d.cl.tryLock(h)
		if err != nil {
			local()
			return nil, err
		}
		if ok {
			var once sync.Once
			return func() {
				once.Do(func() {
					d.cl.unlock(gen, h)
					local()
				})
			}, nil
		}
		select {
		case <-ctx.Done():
			local()
			return nil, ctx.Err()
		case <-time.After(backoff/2 + rand.N(backoff/2+1)):
		}
		backoff = min(2*backoff, 250*time.Millisecond)
	}
}

func hash32(key string) int32 {
	h := fnv.New32a()
	h.Write([]byte(key))
	return int32(h.Sum32())
}

// Subscribe calls fn when a node (this one too) sends Notify for topic, with
// the arg of "topic:arg" ("" for a bare topic), and with "" after the
// listener reconnected: then fn re-reads all it caches. fn runs on the
// listener's goroutine, so it must not block for long. On SQLite it does
// nothing: the one process applies its own changes.
func (d *DB) Subscribe(topic string, fn func(arg string)) {
	if d.cl == nil {
		return
	}
	d.cl.subMu.Lock()
	d.cl.subs[topic] = append(d.cl.subs[topic], fn)
	d.cl.subMu.Unlock()
	d.cl.listen()
}

// Notify tells every node's subscribers of topic ("topic" or "topic:arg")
// that what they cache changed. Send it after the change committed: the
// receivers re-read the database. It takes no write lock. On SQLite it does
// nothing.
func (d *DB) Notify(ctx context.Context, topic string) error {
	if d.cl == nil {
		return nil
	}
	_, err := d.sql.execFree(ctx, `SELECT pg_notify('`+notifyChannel+`', ?)`, topic)
	return err
}

// keyedLocks is the in-process half of Lock: a mutex per key, freed when
// no goroutine holds or waits for it, whose wait ends with the context.
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*keyedLock
}

type keyedLock struct {
	ch   chan struct{} // holds a token while locked
	refs int
}

func (k *keyedLocks) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyedLock{}
	}
	e := k.m[key]
	if e == nil {
		e = &keyedLock{ch: make(chan struct{}, 1)}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()
	release := func() {
		k.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
	select {
	case e.ch <- struct{}{}:
		return func() { <-e.ch; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}
