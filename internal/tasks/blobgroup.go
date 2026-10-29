package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ---- blobstore.group-remove-member ----

// TypeGroupRemove removes a member from a blob store group without losing a
// blob: the member is made read-only in the group (no write goes to it once
// that is stored), each of its blobs that no other member holds is copied
// through the group to the writable members and its SHA-256 checked, and then
// it leaves the group. Reads go through the group all along, from the member
// until the copy is made. The store itself is kept, outside the group: no
// repository names it, so its next compaction deletes its copies.
const TypeGroupRemove = "blobstore.group-remove-member"

func init() {
	types[TypeGroupRemove] = typeDef{name: "Remove a member from a blob store group", params: groupRemoveParamsOf, run: runGroupRemove,
		fields: []ParamSpec{
			{Name: "group", Type: "blobStore", Required: true, Help: "the blob store group"},
			{Name: "member", Type: "blobStore", Required: true, Help: "the member to move out; another member must be writable"},
		}}
}

type groupRemoveParams struct {
	Group  string `json:"group"`
	Member string `json:"member"`
}

// groupCopyHook, if set, runs after each blob the removal copies (a test
// hook).
var groupCopyHook func()

// groupRemoveCheck reports what keeps the member from being removed from the
// group's stored configuration cfg.
func groupRemoveCheck(cfg blob.GroupConfig, p groupRemoveParams) error {
	i := slices.IndexFunc(cfg.Members, func(m blob.GroupMember) bool { return m.Name == p.Member })
	if i < 0 {
		return fmt.Errorf("blob store %q is not a member of the group %q", p.Member, p.Group)
	}
	for j, m := range cfg.Members {
		if j != i && !m.ReadOnly {
			return nil
		}
	}
	return fmt.Errorf("the group %q has no other writable member to move the blobs of %q to", p.Group, p.Member)
}

func groupRemoveParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	var p groupRemoveParams
	if err := decodeParams(raw, &p); err != nil {
		return "", []string{err.Error() + " (blobstore.group-remove-member takes group and member)"}
	}
	switch {
	case p.Group == "" || p.Member == "":
		return "", []string{"params.group and params.member are required"}
	case !check:
		return encodeParams(p), nil
	}
	row, err := s.DB.GetBlobStore(context.Background(), p.Group)
	switch {
	case errors.Is(err, meta.ErrNotFound):
		return "", []string{fmt.Sprintf("blob store %q does not exist", p.Group)}
	case err != nil:
		return "", []string{err.Error()}
	case row.Group == nil:
		return "", []string{fmt.Sprintf("blob store %q is not a group", p.Group)}
	}
	if err := groupRemoveCheck(*row.Group, p); err != nil {
		return "", []string{err.Error()}
	}
	return encodeParams(p), nil
}

// editGroup applies edit to the group's stored configuration, then to the
// live group, under the registry's lock (as the blob-store API does), and
// tells the other nodes.
func editGroup(ctx context.Context, s *Service, name string, edit func(*blob.GroupConfig) error) error {
	defer s.DB.Notify(context.WithoutCancel(ctx), "blobstores")
	return s.Repos.WithWriteLock(ctx, func(*repo.Snapshot) error {
		row, err := s.DB.GetBlobStore(ctx, name)
		if err != nil {
			return err
		}
		g := s.Store.StoreByName(name)
		if row.Group == nil || g == nil {
			return fmt.Errorf("blob store %q is not a group", name)
		}
		cfg := *row.Group
		cfg.Members = slices.Clone(cfg.Members)
		if err := edit(&cfg); err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
		if err := s.DB.UpdateBlobStoreGroup(ctx, name, cfg); err != nil {
			return err
		}
		return g.SetGroup(cfg, s.Store.StoreByName)
	})
}

func runGroupRemove(ctx context.Context, s *Service, t meta.Task, _ string, log *RunLog) (summary, error) {
	var p groupRemoveParams
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %w", err)
	}
	if s.Store == nil || s.Repos == nil {
		return summary{msg: "blob stores are not available"}, errors.New("blob stores are not available")
	}
	// 1. No more writes to the member.
	err := editGroup(ctx, s, p.Group, func(cfg *blob.GroupConfig) error {
		if err := groupRemoveCheck(*cfg, p); err != nil {
			return err
		}
		for i := range cfg.Members {
			if cfg.Members[i].Name == p.Member {
				cfg.Members[i].ReadOnly = true
			}
		}
		return nil
	})
	if err != nil {
		return summary{msg: err.Error()}, err
	}
	log.Infof("%s: %s is read-only in the group: new blobs go to the other members", p.Group, p.Member)

	// 2. Copy what only the member holds.
	g, ms := s.Store.StoreByName(p.Group), s.Store.StoreByName(p.Member)
	if g == nil || ms == nil {
		err := fmt.Errorf("blob store %q or %q is gone", p.Group, p.Member)
		return summary{msg: err.Error()}, err
	}
	cfg, _ := g.Group()
	var copied, kept, bytes int64
	err = ms.Walk(func(sha string, _ time.Time) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, m := range cfg.Members {
			if o := s.Store.StoreByName(m.Name); m.Name != p.Member && o != nil {
				if _, err := o.Stat(sha); err == nil {
					kept++
					return nil
				}
			}
		}
		f, err := ms.Open(sha)
		if errors.Is(err, blob.ErrNotFound) { // compacted since the walk saw it
			return nil
		}
		if err != nil {
			return fmt.Errorf("blob %s: %w", sha, err)
		}
		sums, err := g.Write(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("blob %s: copy: %w", sha, err)
		}
		if sums.SHA256 != sha {
			return fmt.Errorf("blob %s: the member's copy has the SHA-256 %s: it is corrupt; repair or delete the assets that use it", sha, sums.SHA256)
		}
		copied, bytes = copied+1, bytes+sums.Size
		if groupCopyHook != nil {
			groupCopyHook()
		}
		return nil
	})
	sum := summary{msg: fmt.Sprintf("copied %d blobs (%d bytes) of %q to the other members of %q; %d were there already", copied, bytes, p.Member, p.Group, kept),
		removed: copied, bytes: bytes}
	if err != nil {
		log.Errorf("%s; then: %v; %s stays read-only in the group, run the task again", sum.msg, err, p.Member)
		return sum, err
	}
	log.Infof("%s", sum.msg)

	// 3. Out of the group.
	err = editGroup(ctx, s, p.Group, func(cfg *blob.GroupConfig) error {
		i := slices.IndexFunc(cfg.Members, func(m blob.GroupMember) bool { return m.Name == p.Member })
		switch {
		case i < 0:
			return fmt.Errorf("blob store %q left the group %q during the run", p.Member, p.Group)
		case !cfg.Members[i].ReadOnly:
			return fmt.Errorf("blob store %q was made writable again during the run; run the task again", p.Member)
		}
		cfg.Members = slices.Delete(cfg.Members, i, i+1)
		return nil
	})
	if err != nil {
		log.Errorf("%v", err)
		return sum, err
	}
	sum.msg += fmt.Sprintf("; %q left the group", p.Member)
	log.Infof("%s: %s left the group; it stays a blob store of its own, which no repository uses", p.Group, p.Member)
	return sum, nil
}
