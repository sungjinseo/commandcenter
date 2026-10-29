package repo

import (
	"slices"
	"sync"
)

// KeyedMutex is a set of mutexes addressed by string keys.
// Entries are freed when no goroutine holds or waits for them.
type KeyedMutex struct {
	mu sync.Mutex
	m  map[string]*keyedEntry
}

type keyedEntry struct {
	mu   sync.Mutex
	refs int
}

// Lock acquires the mutex for key and returns its unlock function.
func (k *KeyedMutex) Lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyedEntry{}
	}
	e := k.m[key]
	if e == nil {
		e = &keyedEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// LockAll locks every distinct key, in sorted order so that two callers
// never deadlock, and returns one function that unlocks them all.
func (k *KeyedMutex) LockAll(keys []string) (unlock func()) {
	keys = slices.Compact(slices.Sorted(slices.Values(keys)))
	unlocks := make([]func(), len(keys))
	for i, key := range keys {
		unlocks[i] = k.Lock(key)
	}
	return func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
}
