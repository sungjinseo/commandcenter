package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ---- blobstore.recalculate-size, blobstore.delete-temp-files ----

// storeNames returns the stores a blobstore task works on: the one its
// params name, or every one.
func storeNames(s *Service, t meta.Task) ([]string, error) {
	var p struct {
		BlobStore string `json:"blobStore,omitempty"`
	}
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return nil, fmt.Errorf("decode params: %w", err)
	}
	if s.Store == nil {
		return nil, errors.New("blob stores are not available")
	}
	if p.BlobStore == "" {
		return s.Store.StoreNames(), nil
	}
	if !s.Store.HasStore(p.BlobStore) {
		return nil, fmt.Errorf("blob store %q no longer exists: edit or delete this task", p.BlobStore)
	}
	return []string{p.BlobStore}, nil
}

// runRecalc counts the blobs each store holds and compares them with what
// the asset rows reference. The sizes the UI shows are computed live from
// the rows, so there is nothing to store: the task is a report.
func runRecalc(ctx context.Context, s *Service, t meta.Task, _ string, log *RunLog) (summary, error) {
	names, err := storeNames(s, t)
	if err != nil {
		return summary{msg: err.Error()}, err
	}
	snap := s.Repos.Snapshot()
	var total int64
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return summary{}, err
		}
		bs := s.Store.StoreByName(name)
		var stored int64
		if err := bs.Walk(func(string, time.Time) error { stored++; return ctx.Err() }); err != nil {
			return summary{}, fmt.Errorf("blob store %q: %w", name, err)
		}
		var used, bytes int64
		if name == repo.DefaultStore {
			used, bytes, err = s.DB.BlobStats(ctx)
		} else {
			used, bytes, err = s.DB.BlobStatsInRepos(ctx, repo.ReposForStore(snap, name))
		}
		if err != nil {
			return summary{}, fmt.Errorf("blob store %q: %w", name, err)
		}
		total += stored
		log.Infof("%s: %d blobs in the store, %d referenced by assets (%d bytes); %d would go at the next compaction", name, stored, used, bytes, max(stored-used, 0))
	}
	sum := summary{msg: fmt.Sprintf("%d blobs in %d blob stores; the sizes shown are computed live from the asset rows, nothing to recalculate", total, len(names))}
	log.Infof("%s", sum.msg)
	return sum, nil
}

// runTemp removes the stale temp files and abandoned upload sessions of
// each store's tmp/ (blob.Store.SweepTmp), as compaction does first.
func runTemp(ctx context.Context, s *Service, t meta.Task, _ string, log *RunLog) (summary, error) {
	names, err := storeNames(s, t)
	if err != nil {
		return summary{msg: err.Error()}, err
	}
	var removed int64
	var errs []error
	for _, name := range names {
		n, err := s.Store.StoreByName(name).SweepTmp()
		removed += int64(n)
		if err != nil {
			errs = append(errs, fmt.Errorf("blob store %q: %w", name, err))
			log.Errorf("%s: removed %d files, then: %v", name, n, err)
			continue
		}
		log.Infof("%s: removed %d stale temp files and upload sessions", name, n)
	}
	sum := summary{msg: fmt.Sprintf("removed %d stale temp files and upload sessions from %d blob stores", removed, len(names)), removed: removed}
	log.Infof("%s", sum.msg)
	return sum, errors.Join(errs...)
}
