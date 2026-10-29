package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"commandcenter/internal/blob"
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
		if g := s.Store.GroupOf(name); g != "" {
			if len(names) > 1 {
				continue // counted with its group
			}
			n, b, err := bs.Usage(ctx)
			if err != nil {
				return summary{}, fmt.Errorf("blob store %q: %w", name, err)
			}
			total += n
			log.Infof("%s: %d blobs (%d bytes) in the store, a member of the group %q, whose repositories it serves", name, n, b, g)
			continue
		}
		stored, err := groupUsage(ctx, s, name, bs, log)
		if err != nil {
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

// groupUsage counts the blobs bs holds; for a group it logs each member's
// blobs and bytes, whose sums are the group's.
func groupUsage(ctx context.Context, s *Service, name string, bs *blob.Store, log *RunLog) (int64, error) {
	cfg, ok := bs.Group()
	if !ok {
		var stored int64
		err := bs.Walk(func(string, time.Time) error { stored++; return ctx.Err() })
		return stored, err
	}
	var blobs, bytes int64
	for _, m := range cfg.Members {
		ms := s.Store.StoreByName(m.Name)
		if ms == nil {
			return 0, fmt.Errorf("member %q is not open", m.Name)
		}
		n, b, err := ms.Usage(ctx)
		if err != nil {
			return 0, fmt.Errorf("member %q: %w", m.Name, err)
		}
		blobs, bytes = blobs+n, bytes+b
		log.Infof("%s: member %s holds %d blobs (%d bytes)", name, m.Name, n, b)
	}
	log.Infof("%s: the group holds %d blobs (%d bytes) in %d members; a blob copied to two members counts twice", name, blobs, bytes, len(cfg.Members))
	return blobs, nil
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
