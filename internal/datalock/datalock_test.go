package datalock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Two handles of the lock file stand in for two processes: flock locks
// belong to the open file description, so they conflict even in one process.
func TestTwoHandlesExclude(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, err := TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryAcquire(path); !errors.Is(err, ErrHeld) {
		t.Fatalf("second handle: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Acquire(ctx, path); err == nil || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("Acquire while held: %v after %s", err, time.Since(start))
	}
	// Released, the waiter gets it.
	got := make(chan error, 1)
	go func() {
		l, err := Acquire(context.Background(), path)
		if err == nil {
			l.Release()
		}
		got <- err
	}()
	time.Sleep(50 * time.Millisecond)
	a.Release()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter did not get the released lock")
	}
	b, err := TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	b.Release()
	b.Release() // idempotent
}
