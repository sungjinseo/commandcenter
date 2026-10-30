package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Compaction and the reconcile journal (repo.Journal). After the database
// is restored from an older backup, the blobs uploaded since are referenced
// by no asset row: compaction would delete exactly what a reconcile plan
// can recover. So compaction does not run at all in recovery mode, and
// otherwise keeps
//
//   - the blobs that the ADD items of a plan not executed yet name, and
//   - the blobs whose row's latest journal record of the last
//     minAge+gcJournalWindow puts them while the database holds no such
//     row: an upload in that window whose row the restore lost. A row
//     deleted in the window has a later delete record, and one that moved
//     to another store is in the database, so neither is kept.
//
// A restore older than that window must turn recovery mode on before the
// next compaction (the seeded task runs daily) to keep the older blobs.

// gcJournalWindow is how far back past the compaction's minimum age the
// journal protects blobs.
const gcJournalWindow = 24 * time.Hour

var errRecoveryGC = errors.New("recovery mode is on: compaction does not run until it is turned off (a restored database " +
	"references none of the blobs uploaded after its backup; reconcile it first)")

// reconcileKeep returns the blobs compaction keeps in bs although no asset
// references them (see above).
func (a *App) reconcileKeep(ctx context.Context, bs *blob.Store, minAge time.Duration) (map[string]bool, error) {
	// This node's queued records first, so a delete just made here counts.
	// Bounded: under steady uploads the queue may never empty, and what is
	// still queued (here or on another node) only makes compaction keep more.
	if a.Store.Journal != nil {
		fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		a.Store.Journal.Flush(fctx)
		cancel()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	keep, err := a.DB.PendingReconcileBlobs(ctx)
	if err != nil {
		return nil, err
	}
	latest := map[string]string{} // repository, path -> the blob put last, "" when deleted last
	_, err = repo.ReadJournal(bs, time.Now().Add(-minAge-gcJournalWindow), func(_ string, e repo.JournalEntry) error {
		for _, c := range e.Changes {
			if c.Kind != meta.ChangeAsset {
				continue
			}
			sha := ""
			if c.Op == meta.ChangePut && c.Asset != nil {
				sha = c.Asset.SHA256
			}
			latest[c.Repo+"\x00"+c.Path] = sha
		}
		return ctx.Err()
	})
	if err != nil && !errors.Is(err, blob.ErrNoMeta) {
		return nil, err
	}
	// ponytail: one query per row of the window; batch them if compaction
	// of a busy store spends noticeable time here.
	for k, sha := range latest {
		if sha == "" || keep[sha] {
			continue
		}
		repoName, path, _ := strings.Cut(k, "\x00")
		cur, err := a.DB.GetAsset(ctx, repoName, path)
		if err != nil && !errors.Is(err, meta.ErrNotFound) {
			return nil, err
		}
		if err != nil || cur.BlobSHA256 != sha {
			keep[sha] = true
		}
	}
	return keep, nil
}

// pruneJournal removes the journal records of bs older than the retention
// (meta.DB.JournalDays); compaction runs it, so the journal stops growing
// with it. Failures are logged: compaction itself succeeded.
func (a *App) pruneJournal(ctx context.Context, name string, bs *blob.Store) {
	days, err := a.DB.JournalDays(ctx)
	if err == nil {
		var n int
		n, err = repo.PruneJournal(bs, time.Now().Add(-time.Duration(days)*24*time.Hour))
		if n > 0 {
			a.log.Info("removed reconcile journal records past their retention", "blobStore", name, "removed", n, "days", days)
		}
	}
	if err != nil && !errors.Is(err, blob.ErrNoMeta) && ctx.Err() == nil {
		a.log.Warn("could not prune the reconcile journal", "blobStore", name, "err", logsafe.Err(err))
	}
}

// recoveryRefusal is what an upload or delete is told in recovery mode, ""
// when it is off.
func recoveryRefusal(rm meta.RecoveryMode) string {
	if !rm.Enabled {
		return ""
	}
	msg := "Command Center is in recovery mode (since " + rm.Since.UTC().Format(time.RFC3339) + "): it serves downloads, " +
		"but uploads and deletes are refused until an administrator turns recovery mode off"
	if rm.Message != "" {
		msg += ": " + rm.Message
	}
	return msg
}
