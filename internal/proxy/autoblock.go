package proxy

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/repo"
)

// Auto-block (Nexus "auto block"): after autoBlockAfter consecutive remote
// failures (a connection error or a 5xx; a 404, 401, 403 or 429 is an
// answer, not a failure) a proxy stops asking its remote for a back-off that
// starts at autoBlockMin and doubles up to autoBlockMax on every further
// block; meanwhile cached content is served stale and the rest is 502 at
// once. A successful answer, or the back-off expiring, lets requests through
// again. The manual blocked setting never asks the remote.
const (
	autoBlockAfter = 3
	autoBlockMin   = 30 * time.Second
	autoBlockMax   = 5 * time.Minute
)

var errBlocked = errors.New("remote is blocked")

type remoteState struct {
	fails   int
	until   time.Time
	backoff time.Duration
	lastErr string
}

// ponytail: entries of deleted repositories stay in the map (a few bytes each).
func (f *Fetcher) state(rp *repo.Repository) *remoteState {
	key := stateKey(rp)
	if f.remotes == nil {
		f.remotes = map[string]*remoteState{}
	}
	st := f.remotes[key]
	if st == nil {
		st = &remoteState{}
		f.remotes[key] = st
	}
	return st
}

// blocked reports why no remote request is made for rp now, or nil.
func (f *Fetcher) blocked(rp *repo.Repository, now time.Time) error {
	if rp.Blocked {
		return fmt.Errorf("%w: blocked by its configuration", errBlocked)
	}
	if !rp.AutoBlocks() {
		return nil
	}
	f.blockMu.Lock()
	defer f.blockMu.Unlock()
	st := f.state(rp)
	if now.Before(st.until) {
		return fmt.Errorf("%w: auto-blocked until %s after %d failures (%s)", errBlocked, st.until.UTC().Format(time.RFC3339), st.fails, st.lastErr)
	}
	return nil
}

// noteFailure counts a failed remote request (noteSuccess an answer).
func (f *Fetcher) noteFailure(rp *repo.Repository, cause error) {
	if !rp.AutoBlocks() {
		return
	}
	now := f.Now()
	f.blockMu.Lock()
	defer f.blockMu.Unlock()
	st := f.state(rp)
	st.fails++
	st.lastErr = failure(cause)
	if st.fails < autoBlockAfter || now.Before(st.until) {
		return
	}
	st.backoff = min(max(2*st.backoff, autoBlockMin), autoBlockMax)
	st.until = now.Add(st.backoff)
}

func (f *Fetcher) noteSuccess(rp *repo.Repository) {
	f.blockMu.Lock()
	defer f.blockMu.Unlock()
	if st := f.remotes[stateKey(rp)]; st != nil {
		*st = remoteState{}
	}
}

// Remote is the live remote state of rp for the status API.
func (f *Fetcher) Remote(rp *repo.Repository) repo.RemoteStatus {
	if rp == nil || rp.Type != "proxy" {
		return repo.RemoteStatus{}
	}
	if rp.Blocked {
		return repo.RemoteStatus{Blocked: true}
	}
	if !rp.AutoBlocks() {
		return repo.RemoteStatus{}
	}
	now := f.Now()
	f.blockMu.Lock()
	defer f.blockMu.Unlock()
	st := f.remotes[stateKey(rp)]
	switch {
	case st == nil:
		return repo.RemoteStatus{}
	case now.Before(st.until):
		return repo.RemoteStatus{Blocked: true, Auto: true, Until: st.until, LastError: st.lastErr}
	}
	return repo.RemoteStatus{LastError: st.lastErr}
}

// stateKey names rp's remote state (and cookie jar): a change of how the
// remote is reached (its credentials, connection settings or block
// switches) starts afresh, as a new remote (Gen) does.
func stateKey(rp *repo.Repository) string {
	b, _ := json.Marshal([]any{rp.RemoteAuth, rp.Connection, rp.Blocked, rp.AutoBlock})
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%s\x00%d\x00%x", rp.Name, rp.Gen, sum[:8])
}

// failure is the text of a remote failure that any signed-in user may see:
// the innermost error, since a *url.Error and a dial error name the remote.
func failure(err error) string {
	for next := errors.Unwrap(err); next != nil; next = errors.Unwrap(err) {
		err = next
	}
	return logsafe.Err(err)
}
