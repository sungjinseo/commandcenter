// Package cleanup removes components from repositories (plan 11, Decisions 5
// and 6). This file holds the per-format deleters: hosted repositories use
// their format package's DeleteComponent, proxies of every format use one
// generic deleter that drops the cached asset rows.
//
// Cleanup ignores writePolicy: a read-only (deny) or
// allow_once repository is cleaned like any other.
package cleanup

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"sync"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ErrNotSupported is returned for repositories whose components cannot be
// deleted: Cargo hosted, groups, and formats or types without a deleter.
var ErrNotSupported = component.ErrNotSupported

// Result is what one component delete removed (component.Result).
type Result = component.Result

// ComponentDeleter deletes one component of a repository and reports the
// asset rows it actually deleted. Every delete runs in repo.Store.Tx, so a
// repository that was deleted or re-created meanwhile gets no writes. A
// component whose Repo (when set) or Format does not match rp is refused.
// actor is recorded on the delete event ("system" from scheduled cleanup).
// A component evaluated by a policy (Component.Evaluated) is skipped with
// ErrChanged, nothing deleted, when it changed since (CheckUnchanged, called
// inside the transaction). Blobs are reclaimed later by the compact task.
type ComponentDeleter interface {
	DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (Result, error)
}

type deleterKey struct{ format, typ string }

// Registry maps (format, repository type) to a deleter.
type Registry struct {
	mu    sync.RWMutex
	proxy ComponentDeleter
	byKey map[deleterKey]ComponentDeleter
}

// NewRegistry returns a registry whose proxies of every format use the
// generic proxy deleter. Hosted deleters are added with Register.
func NewRegistry(store *repo.Store) *Registry {
	return &Registry{proxy: &ProxyDeleter{Store: store}, byKey: map[deleterKey]ComponentDeleter{}}
}

// Register sets the deleter for repositories of format and type typ.
func (r *Registry) Register(format, typ string, d ComponentDeleter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byKey[deleterKey{format, typ}] = d
}

// For returns the deleter for rp: a registered one, else the generic proxy
// deleter for proxies, else an ErrNotSupported error (groups, for example).
func (r *Registry) For(rp *repo.Repository) (ComponentDeleter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if d := r.byKey[deleterKey{rp.Format, rp.Type}]; d != nil {
		return d, nil
	}
	if rp.Type == "proxy" {
		return r.proxy, nil
	}
	return nil, fmt.Errorf("%s %s repository %q: %w", rp.Format, rp.Type, rp.Name, ErrNotSupported)
}

// Delete deletes c from rp with the deleter For returns.
func (r *Registry) Delete(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (Result, error) {
	d, err := r.For(rp)
	if err != nil {
		return Result{}, err
	}
	return d.DeleteComponent(ctx, rp, c, actor)
}

// ProxyDeleter removes the cached asset rows of a proxy component; the
// content is fetched again from the remote on demand.
type ProxyDeleter struct {
	Store *repo.Store
}

// DeleteComponent deletes, in one transaction, the files listed in c.Assets
// by exact path, then scans each of their directories once for remaining
// files of the same component (sidecars, files past the UI's per-component
// cap). Files of other components are left alone, and a listed path that does
// not derive to c is an error. The files found are checked with
// CheckUnchanged before any is deleted. Only rows actually deleted are counted; a
// component with nothing cached is not an error.
func (d *ProxyDeleter) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (Result, error) {
	if rp.Type != "proxy" {
		return Result{}, fmt.Errorf("proxy delete in %s repository %q: %w", rp.Type, rp.Name, ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return Result{}, err
	}
	belongs := func(p string) bool {
		g, name, v, ok := component.DeriveAsset(rp.Format, p)
		return ok && g == c.Group && name == c.Name && v == c.Version
	}
	var listed []string
	seen := map[string]bool{}
	dirSet := map[string]bool{}
	for _, a := range c.Assets {
		if seen[a.Path] {
			continue
		}
		if !belongs(a.Path) {
			return Result{}, fmt.Errorf("asset %q is not part of component %s", a.Path, c.Label())
		}
		seen[a.Path] = true
		listed = append(listed, a.Path)
		dirSet[path.Dir(a.Path)] = true
	}
	dirs := make([]string, 0, len(dirSet))
	for dir := range dirSet {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	var res Result
	err := d.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		res = Result{}
		// The component's cached files now: the listed ones that exist and
		// the others found in their directories.
		var current []meta.Asset
		found := map[string]bool{}
		for _, p := range listed {
			a, err := tx.GetAsset(ctx, rp.Name, p)
			switch {
			case err == nil:
				current = append(current, a)
				found[p] = true
			case !errors.Is(err, meta.ErrNotFound):
				return err
			}
		}
		for _, dir := range dirs {
			assets, err := tx.ListAssets(ctx, rp.Name, dir+"/")
			if err != nil {
				return err
			}
			for _, a := range assets {
				if !found[a.Path] && path.Dir(a.Path) == dir && belongs(a.Path) {
					current = append(current, a)
					found[a.Path] = true
				}
			}
		}
		if err := CheckUnchanged(c, current); err != nil {
			return err
		}
		for _, a := range current {
			size, err := tx.DeleteAsset(ctx, rp.Name, a.Path)
			if err != nil {
				return err
			}
			res.Add(size)
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if res.Assets > 0 {
		d.Store.EmitAs("delete", actor, rp.Name, c.Label(), fmt.Sprintf("assets=%d", res.Assets))
	}
	return res, nil
}
