//go:build !unix

package datalock

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
)

// Lock is a held lock; Release frees it.
type Lock struct {
	once sync.Once
	path string
}

// TryAcquire creates the lock file exclusively, writing this process's PID:
// ErrHeld when it exists. A file left by a crashed process must be removed
// by hand (its PID says whose it was); the supported platform is Linux,
// which uses flock instead.
func TryAcquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		pid := "unknown"
		if b, rerr := os.ReadFile(path); rerr == nil {
			pid = strings.TrimSpace(string(b))
		}
		return nil, fmt.Errorf("%w: lock file %s holds pid %s (if no such process runs, the lock is stale: remove the file)", ErrHeld, path, pid)
	}
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Close()
	return &Lock{path: path}, nil
}

// Release removes the lock file; calling it again does nothing.
func (l *Lock) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() { os.Remove(l.path) })
}
