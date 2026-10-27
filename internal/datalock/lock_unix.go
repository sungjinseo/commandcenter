//go:build unix

package datalock

import (
	"errors"
	"os"
	"sync"
	"syscall"
)

// Lock is a held lock; Release frees it.
type Lock struct {
	once sync.Once
	f    *os.File
}

// TryAcquire takes the lock at path without waiting: ErrHeld when it is held.
func TryAcquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrHeld
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Release frees the lock; calling it again does nothing.
func (l *Lock) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
		l.f.Close()
	})
}
