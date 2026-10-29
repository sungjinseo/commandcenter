package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/tasks"
)

// Blob store groups (type "group"): a store whose blobs live in member file
// or S3 stores. POST /-/api/blobstores creates one with
// {"name","type":"group","members":[...],"fillPolicy"}; a member is
// {"name","readOnly"} or its bare name. A member belongs to one group, is no
// group itself, is not the default store and no repository names it (convert
// a store in use instead). Every change runs under the registry's write lock,
// like store create and delete.

func (u *UI) blobGroupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /-/api/blobstores/{name}/convert-to-group", u.convertBlobStore)
	mux.HandleFunc("GET /-/api/blobstores/{name}/health", u.blobStoreHealth)
}

// groupHolding returns the group among rows, other than except, that has
// name as a member; "" when none has.
func groupHolding(rows []meta.BlobStore, name, except string) string {
	for _, bs := range rows {
		if bs.Group == nil || bs.Name == except {
			continue
		}
		for _, m := range bs.Group.Members {
			if m.Name == name {
				return bs.Name
			}
		}
	}
	return ""
}

// checkGroupMembers checks cfg's members for the group named group against
// the stored rows and the repositories of snap; old is the group's current
// configuration on an update. A member it drops must hold no blob: the
// removal task moves them first.
func (u *UI) checkGroupMembers(ctx context.Context, snap *repo.Snapshot, group string, cfg blob.GroupConfig, old *blob.GroupConfig) error {
	rows, err := u.DB.ListBlobStores(ctx)
	if err != nil {
		return err
	}
	byName := map[string]meta.BlobStore{}
	for _, bs := range rows {
		byName[bs.Name] = bs
	}
	isOld := func(name string) bool {
		return old != nil && slices.ContainsFunc(old.Members, func(m blob.GroupMember) bool { return m.Name == name })
	}
	var errs []string
	for _, m := range cfg.Members {
		row, ok := byName[m.Name]
		switch {
		case m.Name == group:
			errs = append(errs, fmt.Sprintf("member %q: a group cannot be its own member", m.Name))
		case !ok:
			errs = append(errs, fmt.Sprintf("member %q: no such blob store", m.Name))
		case row.Type == meta.BlobStoreGroup:
			errs = append(errs, fmt.Sprintf("member %q: is a group, and groups do not nest", m.Name))
		case m.Name == repo.DefaultStore:
			errs = append(errs, fmt.Sprintf("member %q: the default store cannot be a member; convert it to a group instead", m.Name))
		case groupHolding(rows, m.Name, group) != "":
			errs = append(errs, fmt.Sprintf("member %q: already belongs to the group %q", m.Name, groupHolding(rows, m.Name, group)))
		case !isOld(m.Name) && len(repo.ReposForStore(snap, m.Name)) > 0:
			errs = append(errs, fmt.Sprintf("member %q: is used by repositories %s; convert it to a group instead",
				m.Name, strings.Join(repo.ReposForStore(snap, m.Name), ", ")))
		}
	}
	if old != nil {
		for _, m := range old.Members {
			if slices.ContainsFunc(cfg.Members, func(n blob.GroupMember) bool { return n.Name == m.Name }) {
				continue
			}
			bs := u.Store.StoreByName(m.Name)
			if bs == nil {
				continue
			}
			full, err := holdsBlobs(bs)
			if err != nil {
				return fmt.Errorf("member %q: %w", m.Name, err)
			}
			if full {
				errs = append(errs, fmt.Sprintf("member %q: holds blobs; remove it with the %s task, which moves them to the other members first",
					m.Name, tasks.TypeGroupRemove))
			}
		}
	}
	if len(errs) > 0 {
		return &repo.ValidationError{Errors: errs}
	}
	return nil
}

var errHasBlob = errors.New("holds a blob")

// holdsBlobs reports whether bs holds at least one blob.
func holdsBlobs(bs *blob.Store) (bool, error) {
	err := bs.Walk(func(string, time.Time) error { return errHasBlob })
	if errors.Is(err, errHasBlob) {
		return true, nil
	}
	return false, err
}

// refuseGroupMember refuses to delete or convert a store that a group has
// as a member (409).
func (u *UI) refuseGroupMember(ctx context.Context, name string) error {
	rows, err := u.DB.ListBlobStores(ctx)
	if err != nil {
		return err
	}
	if g := groupHolding(rows, name, ""); g != "" {
		return repo.Errorf(http.StatusConflict, "blob store %q is a member of the group %q: remove it from the group first (the %s task moves its blobs)",
			name, g, tasks.TypeGroupRemove)
	}
	return nil
}

// groupDetail is a group's event detail: members=a,b(read-only) fillPolicy=roundRobin.
func groupDetail(cfg blob.GroupConfig) string {
	names := make([]string, 0, len(cfg.Members))
	for _, m := range cfg.Members {
		if m.ReadOnly {
			names = append(names, m.Name+"(read-only)")
		} else {
			names = append(names, m.Name)
		}
	}
	return "members=" + strings.Join(names, ",") + " fillPolicy=" + cfg.FillPolicy
}

// setGroupMembers (PUT /-/api/blobstores/{name} with members/fillPolicy) replaces a
// group's members and fill policy (an empty fillPolicy keeps it). A file or
// S3 store's settings cannot change. When it answers, no write is still
// going to a member it made read-only or dropped.
func (u *UI) setGroupMembers(ctx context.Context, name string, body blobStoreRequest) (blob.GroupConfig, error) {
	var cfg blob.GroupConfig
	err := u.Repos.WithWriteLock(ctx, func(snap *repo.Snapshot) error {
		row, err := u.DB.GetBlobStore(ctx, name)
		if err != nil {
			return err
		}
		if row.Type != meta.BlobStoreGroup || row.Group == nil {
			return &repo.ValidationError{Errors: []string{"only a group's members and fill policy can change; a file or S3 store keeps its settings"}}
		}
		if body.Type != "" && body.Type != meta.BlobStoreGroup {
			return &repo.ValidationError{Errors: []string{"the type of a blob store cannot change"}}
		}
		cfg = blob.GroupConfig{Members: body.Members, FillPolicy: body.FillPolicy}
		if cfg.FillPolicy == "" {
			cfg.FillPolicy = row.Group.FillPolicy
		}
		if err := cfg.Validate(); err != nil {
			return &repo.ValidationError{Errors: []string{err.Error()}}
		}
		if err := u.checkGroupMembers(ctx, snap, name, cfg, row.Group); err != nil {
			return err
		}
		g := u.Store.StoreByName(name)
		if g == nil {
			return fmt.Errorf("blob store group %q is not open", name)
		}
		if err := u.DB.UpdateBlobStoreGroup(ctx, name, cfg); err != nil {
			return err
		}
		return g.SetGroup(cfg, u.Store.StoreByName)
	})
	return cfg, err
}

// POST /-/api/blobstores/{name}/convert-to-group {"newMemberName"}: the file
// or S3 store is renamed newMemberName, keeping its directory or bucket, and
// a group of the old name is created with it as its only member, so the
// repositories and tasks that name the store now name the group.
func (u *UI) convertBlobStore(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	if u.Store == nil {
		writeError(w, http.StatusNotImplemented, "blob stores are not available")
		return
	}
	var body struct {
		NewMemberName string `json:"newMemberName"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	name, member := r.PathValue("name"), body.NewMemberName
	if err := repo.CheckName("blob store", member); err != nil {
		u.writeBlobStoreError(w, r, err)
		return
	}
	err := u.Repos.WithWriteLock(r.Context(), func(*repo.Snapshot) error {
		ctx := r.Context()
		row, err := u.DB.GetBlobStore(ctx, name)
		if err != nil {
			return err
		}
		if row.Type == meta.BlobStoreGroup {
			return &repo.ValidationError{Errors: []string{fmt.Sprintf("blob store %q is a group already", name)}}
		}
		if err := u.refuseGroupMember(ctx, name); err != nil {
			return err
		}
		store := u.Store.StoreByName(name)
		if store == nil {
			return fmt.Errorf("blob store %q is not open", name)
		}
		cfg := blob.GroupConfig{Members: []blob.GroupMember{{Name: member}}, FillPolicy: blob.FillRoundRobin}
		g, err := repo.OpenGroup(u.DataDir, meta.BlobStore{Name: name, Path: row.Path, Type: meta.BlobStoreGroup, Group: &cfg},
			func(n string) *blob.Store {
				if n == member {
					return store
				}
				return nil
			})
		if err != nil {
			return err
		}
		// The member keeps the store's root: its path as stored, but for the
		// default store, whose root is the data directory whatever its row
		// says, "." (relative to the data directory, so it follows a move).
		memberPath := row.Path
		if name == repo.DefaultStore {
			memberPath = "."
		}
		if err := u.DB.ConvertBlobStoreToGroup(ctx, name, member, memberPath, cfg); err != nil {
			return err
		}
		u.Store.AddStore(member, store)
		u.Store.AddStore(name, g)
		return nil
	})
	if err != nil {
		u.writeBlobStoreError(w, r, err)
		return
	}
	u.notify(r.Context(), "blobstores")
	u.emit(r, "blobstore.convert", "", name, "member="+member)
	u.writeBlobStore(w, r, name, http.StatusOK)
}

// GET /-/api/blobstores/{name}/health (admin): {"name","healthy","checks":
// [{"target","ok","error"}]}, one check per directory, bucket (an S3 store's
// primary and each failover) or, for a group, per member's.
func (u *UI) blobStoreHealth(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	if u.Store == nil {
		writeError(w, http.StatusNotImplemented, "blob stores are not available")
		return
	}
	name := r.PathValue("name")
	bs := u.Store.StoreByName(name)
	if bs == nil {
		writeError(w, http.StatusNotFound, "blob store not found")
		return
	}
	checks := bs.Health()
	healthy := true
	for _, c := range checks {
		healthy = healthy && c.OK
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "healthy": healthy, "checks": checks})
}

// writeBlobStore answers the stored store's view with status.
func (u *UI) writeBlobStore(w http.ResponseWriter, r *http.Request, name string, status int) {
	stored, err := u.DB.GetBlobStore(r.Context(), name)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	v, err := u.blobStoreView(r.Context(), stored)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, status, v)
}
