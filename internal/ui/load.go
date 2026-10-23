package ui

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"commandcenter/internal/meta"
)

const (
	// statsTTL is how long RepoStats and BlobStats results are reused.
	statsTTL = 30 * time.Second
	// heavySlots bounds the UI queries that can run at once. The database
	// has 4 connections; the rest stay free for package traffic.
	heavySlots = 2
	// statsTimeout bounds one stats refresh, which runs detached from the
	// caller that triggered it.
	statsTimeout = 30 * time.Second
)

// ttlCache holds one value for statsTTL and merges concurrent refreshes.
type ttlCache[T any] struct {
	mu  sync.Mutex
	val T
	exp time.Time
	gen int // bumped by reset; a load that began before it is not stored
	sf  singleflight.Group
}

// reset drops the cached value so the next get loads again; a load already
// running does not store its (older) result.
func (c *ttlCache[T]) reset() {
	c.mu.Lock()
	c.exp = time.Time{}
	c.gen++
	c.mu.Unlock()
}

// get returns the cached value, or loads it. The load runs once for all
// concurrent callers on a context detached from them, so one caller going
// away does not fail the others; each caller still stops waiting with ctx.
func (c *ttlCache[T]) get(ctx context.Context, now func() time.Time, load func(context.Context) (T, error)) (T, error) {
	c.mu.Lock()
	if now().Before(c.exp) {
		v := c.val
		c.mu.Unlock()
		return v, nil
	}
	gen := c.gen
	c.mu.Unlock()
	ch := c.sf.DoChan(strconv.Itoa(gen), func() (any, error) {
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statsTimeout)
		defer cancel()
		v, err := load(lctx)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.gen == gen {
			c.val, c.exp = v, now().Add(statsTTL)
		}
		c.mu.Unlock()
		return v, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			var zero T
			return zero, res.Err
		}
		return res.Val.(T), nil
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

type blobTotals struct{ blobs, bytes int64 }

func (u *UI) clock() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

// acquireHeavy waits for one of the heavy-query slots until ctx ends and
// returns the function that releases it.
func (u *UI) acquireHeavy(ctx context.Context) (func(), error) {
	u.slotsOnce.Do(func() { u.slots = make(chan struct{}, heavySlots) })
	select {
	case u.slots <- struct{}{}:
		return func() { <-u.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// repoStats returns per-repository asset counts, cached and slot-bounded.
func (u *UI) repoStats(ctx context.Context) (map[string]meta.RepoStat, error) {
	return u.repoCache.get(ctx, u.clock, func(ctx context.Context) (map[string]meta.RepoStat, error) {
		release, err := u.acquireHeavy(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		if u.repoStatsQuery != nil {
			return u.repoStatsQuery(ctx)
		}
		return u.DB.RepoStats(ctx)
	})
}

// blobStats returns the blob totals, cached and slot-bounded.
func (u *UI) blobStats(ctx context.Context) (blobs, bytes int64, err error) {
	t, err := u.blobCache.get(ctx, u.clock, func(ctx context.Context) (blobTotals, error) {
		release, err := u.acquireHeavy(ctx)
		if err != nil {
			return blobTotals{}, err
		}
		defer release()
		if u.blobStatsQuery != nil {
			b, n, err := u.blobStatsQuery(ctx)
			return blobTotals{b, n}, err
		}
		b, n, err := u.DB.BlobStats(ctx)
		return blobTotals{b, n}, err
	})
	return t.blobs, t.bytes, err
}

// ctxDone reports whether err is a cancelled or expired context.
func ctxDone(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
