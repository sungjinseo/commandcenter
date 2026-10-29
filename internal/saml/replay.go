package saml

import (
	"sync"
	"time"
)

// ReplayCache remembers assertion IDs until they expire, so an assertion is
// accepted once. It lives in this process: a second replica would need the
// IDs in a shared store.
type ReplayCache struct {
	mu sync.Mutex
	m  map[string]time.Time
}

// replayMax bounds the cache; only assertions the identity provider signed
// get in, so filling it takes that many real logins within their lifetime.
const replayMax = 50000

// Add records id until exp and reports whether it is new. An ID already
// there is a replay; a cache full of live IDs refuses too (fail closed).
func (c *ReplayCache) Add(id string, exp, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]time.Time{}
	}
	if t, ok := c.m[id]; ok && now.Before(t) {
		return false
	}
	if len(c.m) >= replayMax {
		for k, t := range c.m {
			if !now.Before(t) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= replayMax {
			return false
		}
	}
	c.m[id] = exp
	return true
}
