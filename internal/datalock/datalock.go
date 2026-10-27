// Package datalock is the cross-process blob-delete lock of a data directory
// (plan 14 fix round 1): <dataDir>/.gc.lock. The server takes it for every
// blob store compaction and every backup run; the offline `commandcenter gc`
// takes it too, so a gc started next to a running server cannot delete blobs
// a backup is copying. On Unix it is an flock(2) on the file (released by the
// kernel when the process dies); elsewhere a lock file created exclusively
// that holds the owner's PID.
package datalock

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// FileName is the lock file's name in the data directory.
const FileName = ".gc.lock"

// ServerFileName is the lock a running `commandcenter serve` holds for its
// whole life, so an offline restore can tell that a server (even an idle one,
// which does not hold FileName) uses the data directory.
const ServerFileName = ".server.lock"

// ErrHeld is returned by TryAcquire while another holder has the lock.
var ErrHeld = errors.New("the data directory lock is held by another process or run")

// pollEvery is how often Acquire retries.
const pollEvery = 100 * time.Millisecond

// Acquire takes the lock at path, waiting until it is free or ctx is done;
// then the error wraps ctx.Err() and names why the lock was held.
func Acquire(ctx context.Context, path string) (*Lock, error) {
	for {
		l, err := TryAcquire(path)
		if !errors.Is(err, ErrHeld) {
			return l, err
		}
		t := time.NewTimer(pollEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("%w (%v)", ctx.Err(), err)
		case <-t.C:
		}
	}
}
