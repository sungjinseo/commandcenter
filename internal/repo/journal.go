package repo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
)

// The reconcile journal: after every commit that wrote content rows (assets,
// package versions, dist tags; meta.DB.OnCommit), Journal writes one record
// per blob store involved into that store, as the meta object
// journal/YYYY/MM/DD/HH/<unix-nanos>-<random>.json (UTC). Blobs are named by
// their SHA-256 alone; the journal is what says which repository path a blob
// was, so that a database restored from an older backup can be brought back
// in line with the blob stores (blobstore.reconcile-plan). The time is the
// database's clock at the commit, so the keys of every node sort in commit
// order and the random part keeps two nodes' keys apart.
//
// Writes are asynchronous: a bounded queue and a few writers, so an upload
// never waits for the journal; when the queue is full the committing
// request writes its record itself. A failed write is logged and counted
// (Stats, commandcenter_reconcile_journal_writes_total), never returned:
// the upload already committed.

// JournalPrefix is where the journal's meta objects live in a blob store.
const JournalPrefix = "journal/"

// JournalEntry is one journal record: the changes one commit made to the
// repositories of one blob store, in order.
type JournalEntry struct {
	V       int           `json:"v"` // 1
	At      time.Time     `json:"at"`
	Node    string        `json:"node,omitempty"`
	Actor   string        `json:"actor,omitempty"` // who uploaded or deleted ("" for a task)
	Changes []meta.Change `json:"changes"`
}

// Journal writes the reconcile journal of a Store.
type Journal struct {
	store *Store
	node  string
	log   *slog.Logger

	q      chan journalJob
	start  sync.Once // the writers start with the first record
	wg     sync.WaitGroup
	mu     sync.RWMutex // Record's read lock against Close
	closed bool

	written, failed atomic.Int64
	pending         atomic.Int64 // queued or being written by a writer
	logMu           sync.Mutex
	lastLog         time.Time // of a failure
	quiet           int       // failures not logged since
}

type journalJob struct {
	bs      *blob.Store
	store   string
	key     string
	data    []byte
	repo    string
	changes int
}

// Queue and writers; journalLogEvery bounds the failure log lines.
const (
	journalQueue    = 4096
	journalWorkers  = 4
	journalLogEvery = time.Minute
)

// NewJournal returns the journal of s; node names this process in the
// records. Its writers start with the first record; Close flushes it.
func NewJournal(s *Store, node string, log *slog.Logger) *Journal {
	return &Journal{store: s, node: node, log: log, q: make(chan journalJob, journalQueue)}
}

func (j *Journal) startWriters() {
	for range journalWorkers {
		j.wg.Add(1)
		go func() {
			defer j.wg.Done()
			for job := range j.q {
				j.write(job)
				j.pending.Add(-1)
			}
		}()
	}
}

// Record journals the changes one commit made at at (meta.DB.OnCommit).
func (j *Journal) Record(ctx context.Context, at time.Time, changes []meta.Change) {
	actor := auth.FromContext(ctx).Owner()
	var order []string
	byStore := map[string][]meta.Change{}
	for _, c := range changes {
		if rp := j.store.Definition(c.Repo); rp != nil {
			c.Format = rp.Format
		}
		name := j.store.storeName(c.Repo)
		if _, ok := byStore[name]; !ok {
			order = append(order, name)
		}
		byStore[name] = append(byStore[name], c)
	}
	for _, name := range order {
		cs := byStore[name]
		job := journalJob{store: name, key: JournalKey(at, randomSuffix()), repo: cs[0].Repo, changes: len(cs)}
		job.bs = j.store.StoreByName(name)
		var err error
		job.data, err = json.Marshal(JournalEntry{V: 1, At: at, Node: j.node, Actor: actor, Changes: cs})
		if job.bs == nil {
			err = fmt.Errorf("blob store %q does not exist", name)
		}
		if err != nil {
			j.fail(job, err)
			continue
		}
		j.mu.RLock()
		queued := false
		if !j.closed {
			j.start.Do(j.startWriters)
			j.pending.Add(1)
			select {
			case j.q <- job:
				queued = true
			default: // full: written here, its key keeps its place in the order
				j.pending.Add(-1)
			}
		}
		j.mu.RUnlock()
		if !queued {
			j.write(job)
		}
	}
}

func (j *Journal) write(job journalJob) {
	if err := job.bs.PutMeta(job.key, job.data); err != nil {
		j.fail(job, err)
		return
	}
	j.written.Add(1)
}

// fail counts a record that could not be written and logs it, at most once
// per journalLogEvery with the number left unlogged.
func (j *Journal) fail(job journalJob, err error) {
	j.failed.Add(1)
	j.logMu.Lock()
	defer j.logMu.Unlock()
	if time.Since(j.lastLog) < journalLogEvery {
		j.quiet++
		return
	}
	j.log.Error("could not write a reconcile journal record: what that commit wrote cannot be recovered with blobstore.reconcile-plan if the database is restored from an older backup",
		"blobStore", job.store, "repository", job.repo, "changes", job.changes, "key", job.key, "unloggedFailures", j.quiet, "err", logsafe.Err(err))
	j.lastLog, j.quiet = time.Now(), 0
}

// Close writes what is queued and stops the writers; a Record after it
// writes its record at once.
func (j *Journal) Close() {
	j.mu.Lock()
	if !j.closed {
		j.closed = true
		close(j.q)
	}
	j.mu.Unlock()
	j.wg.Wait()
}

// Flush waits until the records queued before it are written, or ctx ends.
func (j *Journal) Flush(ctx context.Context) error {
	for j.pending.Load() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
	return nil
}

// JournalStats are this node's journal counters since it started.
type JournalStats struct {
	Written int64 `json:"written"`
	Failed  int64 `json:"failed"`
	Queued  int64 `json:"queued"` // waiting or being written
}

// Stats returns the journal counters.
func (j *Journal) Stats() JournalStats {
	return JournalStats{Written: j.written.Load(), Failed: j.failed.Load(), Queued: j.pending.Load()}
}

func randomSuffix() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// JournalKey is the key of a record committed at at.
func JournalKey(at time.Time, suffix string) string {
	at = at.UTC()
	return fmt.Sprintf("%s%s/%019d-%s.json", JournalPrefix, at.Format("2006/01/02/15"), at.UnixNano(), suffix)
}

// JournalKeyTime returns the commit time a record's key names; ok is false
// for a key that is no journal record.
func JournalKeyTime(key string) (time.Time, bool) {
	if !strings.HasPrefix(key, JournalPrefix) || !strings.HasSuffix(key, ".json") {
		return time.Time{}, false
	}
	base := path.Base(key)
	if len(base) < 20 || base[19] != '-' {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(base[:19], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, n).UTC(), true
}

// ReadJournal calls fn with every record of bs committed at since or later
// (the whole journal for a zero since), in commit order. A record that does
// not decode is skipped and counted.
func ReadJournal(bs *blob.Store, since time.Time, fn func(key string, e JournalEntry) error) (skipped int, err error) {
	prefixes := []string{JournalPrefix}
	if !since.IsZero() {
		// One listing per day; a day past now covers a clock ahead of this one.
		prefixes = nil
		last := time.Now().UTC().Add(24 * time.Hour)
		for d := since.UTC().Truncate(24 * time.Hour); !d.After(last); d = d.Add(24 * time.Hour) {
			prefixes = append(prefixes, JournalPrefix+d.Format("2006/01/02/"))
		}
	}
	for _, p := range prefixes {
		err := bs.ListMeta(p, func(key string) error {
			at, ok := JournalKeyTime(key)
			if !ok || at.Before(since) {
				return nil
			}
			data, err := bs.GetMeta(key)
			if errors.Is(err, blob.ErrNotFound) { // pruned meanwhile
				return nil
			}
			if err != nil {
				return fmt.Errorf("journal record %s: %w", key, err)
			}
			var e JournalEntry
			if json.Unmarshal(data, &e) != nil || e.V != 1 {
				skipped++
				return nil
			}
			return fn(key, e)
		})
		if err != nil {
			return skipped, err
		}
	}
	return skipped, nil
}

// errPruned ends PruneJournal's listing at the first record it keeps.
var errPruned = errors.New("pruned")

// PruneJournal removes the records of bs committed before before and
// returns how many it removed.
func PruneJournal(bs *blob.Store, before time.Time) (int, error) {
	n := 0
	err := bs.ListMeta(JournalPrefix, func(key string) error {
		at, ok := JournalKeyTime(key)
		switch {
		case !ok:
			return nil
		case !at.Before(before): // keys sort by time: the rest are newer
			return errPruned
		}
		if err := bs.RemoveMeta(key); err != nil {
			return err
		}
		n++
		return nil
	})
	if errors.Is(err, errPruned) {
		err = nil
	}
	return n, err
}
