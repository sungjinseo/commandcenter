// Package events records the activity timeline: uploads, deletes, repository
// and token changes, task runs. Emit never blocks a request: events go through
// a bounded channel to one writer goroutine that inserts them in batches.
package events

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
)

// System is the actor of events the server causes itself (scheduled tasks).
const System = "system"

// Store is the part of the metadata database the Recorder writes to.
type Store interface {
	InsertEvents(ctx context.Context, evs []meta.Event) error
	PruneEvents(ctx context.Context, keep int) (int64, error)
}

// closeTimeout bounds how long Close waits for the writer; a var for tests.
var closeTimeout = 10 * time.Second

const (
	// QueueSize is the capacity of the channel between Emit and the writer.
	QueueSize = 1000
	// Keep is the number of newest events that survive pruning.
	Keep = 10000
	// PruneEvery is the number of inserts after which the table is pruned.
	PruneEvery = 500
	// PruneInterval is the longest time between prunes while events arrive.
	PruneInterval = time.Minute

	maxBatch   = 100
	maxSubject = 512
	maxDetail  = 1024
	maxField   = 128 // actor, repo, kind
	warnEvery  = time.Minute
)

// Recorder queues events and writes them from a background goroutine.
type Recorder struct {
	db         Store
	keep       int
	pruneEvery int
	Now        func() time.Time // test hook; read only before the first Emit
	Node       string           // this process's node id, recorded with each event; set before the first Emit

	mu     sync.RWMutex // guards closed and sends on ch
	closed bool
	ch     chan meta.Event
	done   chan struct{}

	dropped  atomic.Int64
	lastWarn atomic.Int64 // unix nanos of the last queue-full warning
	abort    atomic.Bool  // set when Close gave up waiting: the writer discards what is left
}

// New starts a Recorder with the standard limits; Close stops it.
func New(db Store) *Recorder { return newWith(db, Keep, PruneEvery) }

func newWith(db Store, keep, pruneEvery int) *Recorder {
	r := &Recorder{db: db, keep: keep, pruneEvery: pruneEvery, Now: time.Now,
		ch: make(chan meta.Event, QueueSize), done: make(chan struct{})}
	go r.run()
	return r
}

// Dropped is the number of events lost: the queue was full, the recorder was
// closed or an insert failed.
func (r *Recorder) Dropped() int64 { return r.dropped.Load() }

// Emit queues an event. It never blocks: when the queue is full (or the
// recorder is closed) the event is dropped and counted. actor is the user name,
// "" for anonymous or System.
func (r *Recorder) Emit(kind, actor, repo, subject, detail string) {
	r.emit("", kind, actor, repo, subject, detail)
}

// EmitCtx is Emit for an event a request caused: the row names the
// client's address too (repo.CtxSink).
func (r *Recorder) EmitCtx(ctx context.Context, kind, actor, repo, subject, detail string) {
	r.emit(auth.ClientIP(ctx), kind, actor, repo, subject, detail)
}

func (r *Recorder) emit(ip, kind, actor, repo, subject, detail string) {
	e := meta.Event{At: r.Now(), Kind: clip(kind, maxField), Actor: clip(actor, maxField), Repo: clip(repo, maxField),
		Subject: clip(subject, maxSubject), Detail: clip(detail, maxDetail), IP: clip(ip, maxField), Node: r.Node}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		r.dropped.Add(1)
		return
	}
	select {
	case r.ch <- e:
	default:
		n := r.dropped.Add(1)
		now := time.Now().UnixNano()
		if last := r.lastWarn.Load(); now-last >= int64(warnEvery) && r.lastWarn.CompareAndSwap(last, now) {
			slog.Warn("activity event queue is full: events are being dropped", "dropped", n)
		}
	}
}

// Close stops accepting events, writes what is queued and waits for the writer.
// Call it before closing the database. It is safe to call more than once.
func (r *Recorder) Close() {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.ch)
	}
	r.mu.Unlock()
	select {
	case <-r.done:
	case <-time.After(closeTimeout):
		r.abort.Store(true)
		slog.Warn("activity events were not fully written before the shutdown deadline", "dropped", r.dropped.Load())
	}
}

func (r *Recorder) run() {
	defer close(r.done)
	lastPrune := time.Now()
	sinceFlush := 0
	r.prune()
	batch := make([]meta.Event, 0, maxBatch)
	lastWarn := time.Time{}
	for e := range r.ch {
		batch = append(batch[:0], e)
	fill:
		for len(batch) < maxBatch {
			select {
			case e, ok := <-r.ch:
				if !ok {
					break fill
				}
				batch = append(batch, e)
			default:
				break fill
			}
		}
		if r.abort.Load() {
			r.dropped.Add(int64(len(batch)))
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := r.db.InsertEvents(ctx, batch)
		cancel()
		if err != nil {
			r.dropped.Add(int64(len(batch)))
			if time.Since(lastWarn) >= warnEvery {
				lastWarn = time.Now()
				slog.Warn("could not store activity events", "err", logsafe.Err(err), "dropped", r.dropped.Load())
			}
			continue
		}
		sinceFlush += len(batch)
		if sinceFlush >= r.pruneEvery || time.Since(lastPrune) >= PruneInterval {
			r.prune()
			sinceFlush, lastPrune = 0, time.Now()
		}
	}
	if sinceFlush > 0 {
		r.prune()
	}
}

func (r *Recorder) prune() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := r.db.PruneEvents(ctx, r.keep); err != nil {
		slog.Warn("could not prune activity events", "err", logsafe.Err(err))
	}
}

// clip cuts s to at most n bytes, backing up at most utf8.UTFMax bytes to a rune start.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for i := 0; i < utf8.UTFMax && cut > 0 && !utf8.RuneStart(s[cut]); i++ {
		cut--
	}
	return s[:cut]
}
