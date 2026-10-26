// Package metrics counts downloads and requests in memory and writes them to
// the metadata database in batches, so the request path never touches the DB.
package metrics

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
)

// Flusher is the part of the metadata database the Recorder writes to.
type Flusher interface {
	FlushDownloads(ctx context.Context, batch []meta.DownloadDelta) error
	FlushRequests(ctx context.Context, batch []meta.RequestDelta) error
	PruneRequestStats(ctx context.Context, before int64) (int64, error)
	PruneNegative(ctx context.Context, now time.Time) (int64, error)
}

const (
	// DefaultMaxKeys is the number of pending keys that triggers an early flush.
	DefaultMaxKeys = 10000
	// DefaultMaxPending bounds memory when the database keeps failing: counts
	// for new keys beyond this many keys (per kind) are dropped.
	DefaultMaxPending = 4 * DefaultMaxKeys
	// Retention is how long request statistics are kept.
	Retention = 90 * 24 * time.Hour
	// FailBackoff is how long after a failed flush the key threshold is ignored;
	// the interval ticker keeps retrying.
	FailBackoff = 30 * time.Second
	// warnEvery rate-limits the flush-failure and drop warnings.
	warnEvery = time.Minute
)

type dlKey struct{ repo, path string }
type dlVal struct {
	n    int64
	last time.Time
}
type rqKey struct {
	hour int64
	repo string
}
type rqVal struct{ requests, bytes int64 }

// Recorder accumulates counters; see Flush and Run.
type Recorder struct {
	db         Flusher
	MaxKeys    int              // pending keys that trigger a flush; DefaultMaxKeys
	MaxPending int              // pending keys per kind beyond which new keys are dropped
	Now        func() time.Time // test hook

	mu       sync.Mutex
	dl       map[dlKey]dlVal
	rq       map[rqKey]rqVal
	kick     chan struct{}
	lastFail time.Time // of the last failed flush; zero after a success
	lastWarn map[string]time.Time

	dropped atomic.Int64
	flushMu sync.Mutex // one flush at a time
}

func New(db Flusher) *Recorder {
	return &Recorder{db: db, MaxKeys: DefaultMaxKeys, MaxPending: DefaultMaxPending, Now: time.Now,
		dl: map[dlKey]dlVal{}, rq: map[rqKey]rqVal{}, kick: make(chan struct{}, 1),
		lastWarn: map[string]time.Time{}}
}

// Dropped is the number of counts lost because MaxPending keys were pending.
func (r *Recorder) Dropped() int64 { return r.dropped.Load() }

// warn logs at most once per warnEvery for each kind. Callers hold no lock.
func (r *Recorder) warn(kind, msg string, args ...any) {
	now := r.Now()
	r.mu.Lock()
	if t, ok := r.lastWarn[kind]; ok && now.Sub(t) < warnEvery {
		r.mu.Unlock()
		return
	}
	r.lastWarn[kind] = now
	r.mu.Unlock()
	slog.Warn(msg, args...)
}

func (r *Recorder) drop(n int64) {
	r.dropped.Add(n)
	r.warn("drop", "metrics counters dropped: too many pending keys (is the database failing?)", "dropped", r.dropped.Load())
}

// Download counts one successful download of repo/path. ServeAsset reports it
// when the 2xx headers are sent, so a transfer the client aborts still counts.
func (r *Recorder) Download(repo, path string) {
	now := r.Now()
	r.mu.Lock()
	k := dlKey{repo, path}
	v, ok := r.dl[k]
	if !ok && len(r.dl) >= r.MaxPending {
		r.mu.Unlock()
		r.drop(1)
		return
	}
	v.n++
	if now.After(v.last) {
		v.last = now
	}
	r.dl[k] = v
	// Signal only when the pending keys cross the threshold, not on every call above it.
	crossed := !ok && len(r.dl)+len(r.rq) == r.MaxKeys
	r.mu.Unlock()
	if crossed {
		r.signal()
	}
}

// Request counts one finished request to repo with its response bytes. Callers
// pass only names the registry knows, so the key space stays small.
func (r *Recorder) Request(repo string, bytes int64) {
	hour := r.Now().Unix() / 3600 * 3600
	r.mu.Lock()
	k := rqKey{hour, repo}
	v, ok := r.rq[k]
	if !ok && len(r.rq) >= r.MaxPending {
		r.mu.Unlock()
		r.drop(1)
		return
	}
	v.requests++
	v.bytes += bytes
	r.rq[k] = v
	crossed := !ok && len(r.dl)+len(r.rq) == r.MaxKeys
	r.mu.Unlock()
	if crossed {
		r.signal()
	}
}

func (r *Recorder) signal() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// inBackoff reports whether a flush failed less than FailBackoff ago.
func (r *Recorder) inBackoff() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.lastFail.IsZero() && r.Now().Sub(r.lastFail) < FailBackoff
}

// Flush writes everything pending, each kind in one transaction. On failure the
// counts are put back (and retried by the next flush).
func (r *Recorder) Flush(ctx context.Context) error {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	r.mu.Lock()
	dl, rq := r.dl, r.rq
	r.dl, r.rq = map[dlKey]dlVal{}, map[rqKey]rqVal{}
	r.mu.Unlock()

	var firstErr error
	if len(dl) > 0 {
		batch := make([]meta.DownloadDelta, 0, len(dl))
		for k, v := range dl {
			batch = append(batch, meta.DownloadDelta{Repo: k.repo, Path: k.path, N: v.n, Last: v.last})
		}
		if err := r.db.FlushDownloads(ctx, batch); err != nil {
			firstErr = err
			r.restoreDownloads(dl)
		}
	}
	if len(rq) > 0 {
		batch := make([]meta.RequestDelta, 0, len(rq))
		for k, v := range rq {
			batch = append(batch, meta.RequestDelta{Hour: k.hour, Repo: k.repo, Requests: v.requests, Bytes: v.bytes})
		}
		if err := r.db.FlushRequests(ctx, batch); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			r.restoreRequests(rq)
		}
	}
	r.mu.Lock()
	if firstErr != nil {
		r.lastFail = r.Now()
	} else {
		r.lastFail = time.Time{}
	}
	r.mu.Unlock()
	return firstErr
}

func (r *Recorder) restoreDownloads(dl map[dlKey]dlVal) {
	var dropped int64
	r.mu.Lock()
	for k, v := range dl {
		cur, ok := r.dl[k]
		if !ok && len(r.dl) >= r.MaxPending {
			dropped++
			continue
		}
		cur.n += v.n
		if v.last.After(cur.last) {
			cur.last = v.last
		}
		r.dl[k] = cur
	}
	r.mu.Unlock()
	if dropped > 0 {
		r.drop(dropped)
	}
}

func (r *Recorder) restoreRequests(rq map[rqKey]rqVal) {
	var dropped int64
	r.mu.Lock()
	for k, v := range rq {
		cur, ok := r.rq[k]
		if !ok && len(r.rq) >= r.MaxPending {
			dropped++
			continue
		}
		cur.requests += v.requests
		cur.bytes += v.bytes
		r.rq[k] = cur
	}
	r.mu.Unlock()
	if dropped > 0 {
		r.drop(dropped)
	}
}

// Run flushes every interval and whenever MaxKeys keys are pending (not within
// FailBackoff of a failed flush; the ticker retries then), and prunes old
// request statistics and expired negative-cache entries at start and then
// daily, until ctx ends. The caller does the final Flush after its server has
// drained.
func (r *Recorder) Run(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	day := time.NewTicker(24 * time.Hour)
	defer day.Stop()
	r.prune(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-r.kick:
			if r.inBackoff() {
				continue
			}
		case <-day.C:
			r.prune(ctx)
			continue
		}
		if err := r.Flush(ctx); err != nil && ctx.Err() == nil {
			r.warn("flush", "could not flush metrics", "err", logsafe.Err(err))
		}
	}
}

func (r *Recorder) prune(ctx context.Context) {
	now := r.Now()
	before := now.Add(-Retention).Unix() / 3600 * 3600
	if _, err := r.db.PruneRequestStats(ctx, before); err != nil && ctx.Err() == nil {
		r.warn("prune", "could not prune request statistics", "err", logsafe.Err(err))
	}
	// Proxies record one row per missing path; nothing else deletes the expired ones.
	if _, err := r.db.PruneNegative(ctx, now); err != nil && ctx.Err() == nil {
		r.warn("prune-negative", "could not prune expired negative-cache entries", "err", logsafe.Err(err))
	}
}
