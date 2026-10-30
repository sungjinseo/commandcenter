// Package resolve finds assets across hosted, proxy and group repositories.
package resolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"commandcenter/internal/blob"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// MergeFunc combines member documents into one. Members can be re-read with Seek.
type MergeFunc func(ctx context.Context, members []io.ReadSeeker, w io.Writer) error

type Resolver struct {
	Store   *repo.Store
	Fetcher *proxy.Fetcher
}

func pathTarget(path string) proxy.Target { return proxy.Target{Key: path, Remote: path} }

// Lookup returns the asset at path. Groups return the first member that has it.
func (r *Resolver) Lookup(ctx context.Context, rp *repo.Repository, path string, kind proxy.Kind) (proxy.Result, error) {
	return r.LookupTarget(ctx, rp, pathTarget(path), kind)
}

// LookupTarget is Lookup for a target whose cache key and remote path differ.
// Groups skip offline members, and failing ones while a later member has the
// asset; Owner is the lookup that fails closed instead.
func (r *Resolver) LookupTarget(ctx context.Context, rp *repo.Repository, t proxy.Target, kind proxy.Kind) (proxy.Result, error) {
	switch rp.Type {
	case "hosted":
		a, err := r.Store.Get(ctx, rp.Name, t.Key)
		return proxy.Result{Asset: a}, err
	case "proxy":
		return r.Fetcher.FetchTarget(ctx, rp, t, kind)
	case "group":
		var firstErr error
		for _, m := range rp.OnlineMembers() {
			res, err := r.LookupTarget(ctx, m, t, kind)
			if err == nil {
				return res, nil
			}
			if repo.StatusOf(err) != http.StatusNotFound && firstErr == nil {
				firstErr = err
			}
		}
		if firstErr != nil {
			return proxy.Result{}, firstErr
		}
		return proxy.Result{}, repo.ErrNotFound
	}
	return proxy.Result{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// Owner returns the repository that answers for t in rp, and t from it: rp
// itself unless rp is a group, otherwise the first member in order (nested
// groups searched in place) that has t. A group that serves one member's
// package alone looks up the package document here. Unlike LookupTarget it
// fails closed: a member ahead of that one that is offline (503) or fails
// with anything but not found (5xx, see memberFailure) ends the search, so a
// later (public) member's same-named package never stands in for an earlier
// (internal) one (dependency confusion, QA M-4 and N-1).
func (r *Resolver) Owner(ctx context.Context, rp *repo.Repository, t proxy.Target, kind proxy.Kind) (*repo.Repository, proxy.Result, error) {
	if rp.Type != "group" {
		res, err := r.LookupTarget(ctx, rp, t, kind)
		return rp, res, err
	}
	for _, m := range rp.MemberRepos {
		if !m.IsOnline() {
			return nil, proxy.Result{}, repo.Errorf(http.StatusServiceUnavailable, "group member %q is offline", m.Name)
		}
		owner, res, err := r.Owner(ctx, m, t, kind)
		if err == nil {
			return owner, res, nil
		}
		if repo.StatusOf(err) != http.StatusNotFound {
			return nil, proxy.Result{}, memberFailure(rp.Name, m.Name, err)
		}
	}
	return nil, proxy.Result{}, repo.ErrNotFound
}

// OwnerFile looks up file, a file of the package whose document is doc (an
// npm tarball and its packument), under Owner's rule while reading doc as
// little as it can. A group asks its members in order for file, and the
// first that has it answers, from its cache or its upstream: a member with a
// file of the package has the package. A member without file is asked for
// doc (a nested group through Owner), and if it has the package it owns it:
// the answer is not found, never a later member's file (QA N-1). The last
// member is not asked, as no member follows it. So a cached file is served
// while its upstream is down or after the upstream dropped the package, and
// a lockfile install fetches no packuments. An offline member reached is a
// 503 and a failing one a 5xx, as in Owner.
func (r *Resolver) OwnerFile(ctx context.Context, rp *repo.Repository, file, doc proxy.Target) (proxy.Result, error) {
	if rp.Type != "group" {
		return r.LookupTarget(ctx, rp, file, proxy.Content)
	}
	for i, m := range rp.MemberRepos {
		if !m.IsOnline() {
			return proxy.Result{}, repo.Errorf(http.StatusServiceUnavailable, "group member %q is offline", m.Name)
		}
		res, err := r.OwnerFile(ctx, m, file, doc)
		if err == nil {
			return res, nil
		}
		if repo.StatusOf(err) == http.StatusNotFound && i < len(rp.MemberRepos)-1 {
			if _, _, err = r.Owner(ctx, m, doc, proxy.Metadata); err == nil {
				return proxy.Result{}, repo.ErrNotFound // m owns the package, without this file
			}
		}
		if repo.StatusOf(err) != http.StatusNotFound {
			return proxy.Result{}, memberFailure(rp.Name, m.Name, err)
		}
	}
	return proxy.Result{}, repo.ErrNotFound
}

// memberFailure is the error Owner and OwnerFile return for a failed
// member: the member's own status when it is a 5xx, otherwise 502.
func memberFailure(group, member string, err error) error {
	slog.Warn("group member lookup failed", "group", group, "member", member, "err", logsafe.Err(err))
	if repo.StatusOf(err) >= 500 {
		return err
	}
	return repo.Errorf(http.StatusBadGateway, "group member %q failed", member)
}

// All returns the asset at path from every leaf member of rp that has it.
func (r *Resolver) All(ctx context.Context, rp *repo.Repository, path string, kind proxy.Kind) ([]proxy.Result, error) {
	return r.AllTarget(ctx, rp, pathTarget(path), kind)
}

// AllTarget is All for a target whose cache key and remote path differ. A
// failing member is logged and skipped while another member has the asset.
func (r *Resolver) AllTarget(ctx context.Context, rp *repo.Repository, t proxy.Target, kind proxy.Kind) ([]proxy.Result, error) {
	var out []proxy.Result
	var firstErr error
	for _, m := range Leaves(rp) {
		res, err := r.LookupTarget(ctx, m, t, kind)
		if err == nil {
			out = append(out, res)
			continue
		}
		if repo.StatusOf(err) != http.StatusNotFound {
			slog.Warn("group member lookup failed", "group", rp.Name, "member", m.Name, "key", t.Key, "err", logsafe.Err(err))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// Merged returns the merge of path across rp's members. The result is stored
// in rp's own namespace and reused until a member's content changes.
func (r *Resolver) Merged(ctx context.Context, rp *repo.Repository, path, contentType string, merge MergeFunc) (meta.Asset, error) {
	return r.MergedTarget(ctx, rp, pathTarget(path), contentType, merge)
}

// MergedTarget is Merged for a target whose cache key and remote path differ.
func (r *Resolver) MergedTarget(ctx context.Context, rp *repo.Repository, t proxy.Target, contentType string, merge MergeFunc) (meta.Asset, error) {
	return r.MergedTargetSalted(ctx, rp, t, contentType, "", merge)
}

// MergedTargetSalted is MergedTarget with salt mixed into the stored copy's
// fingerprint. Changing a format's salt when its merge logic changes makes
// every merged copy stored by an older build re-merge once on its next read;
// the empty salt keeps the fingerprint MergedTarget has always stored.
func (r *Resolver) MergedTargetSalted(ctx context.Context, rp *repo.Repository, t proxy.Target, contentType, salt string, merge MergeFunc) (meta.Asset, error) {
	results, err := r.AllTarget(ctx, rp, t, proxy.Metadata)
	if err != nil {
		return meta.Asset{}, err
	}
	if len(results) == 0 {
		return meta.Asset{}, repo.ErrNotFound
	}
	fp := fingerprint(results, salt)
	stored, err := r.Store.Get(ctx, rp.Name, t.Key)
	if err == nil && stored.RemoteETag == fp {
		return stored, nil
	}
	if len(results) == 1 && err != nil {
		// Nothing to merge and no merged copy was ever served: serve the
		// member's document as it is. Once a merged copy exists, it is
		// re-stored instead, so Last-Modified never moves back to the
		// member's older time when a member drops out (plan 13 Decision 7).
		return results[0].Asset, nil
	}

	members := make([]io.ReadSeeker, 0, len(results))
	var files []blob.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	for _, res := range results {
		f, err := r.Store.Open(res.Asset)
		if err != nil {
			return meta.Asset{}, err
		}
		files = append(files, f)
		members = append(members, f)
	}

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("merge of %s in %s panicked: %v", t.Key, rp.Name, p)
				}
			}()
			return merge(ctx, members, pw)
		}()
		pw.CloseWithError(err)
	}()
	a, err := r.Store.Put(ctx, rp.Name, t.Key, contentType, pr, func(a *meta.Asset) { a.RemoteETag = fp })
	pr.Close() // unblocks the merger if Put stopped early
	<-done
	return a, err
}

func fingerprint(results []proxy.Result, salt string) string {
	h := sha256.New()
	for _, res := range results {
		io.WriteString(h, res.Asset.BlobSHA256)
		io.WriteString(h, "\n")
	}
	return "group:" + salt + hex.EncodeToString(h.Sum(nil))
}

// Leaves flattens nested groups into their online non-group members, each
// once, in order; a repository that is no group is its own leaf.
func Leaves(rp *repo.Repository) []*repo.Repository {
	var out []*repo.Repository
	seen := map[string]bool{}
	var walk func(*repo.Repository)
	walk = func(r *repo.Repository) {
		if seen[r.Name] {
			return
		}
		seen[r.Name] = true
		if r.Type != "group" {
			out = append(out, r)
			return
		}
		for _, m := range r.OnlineMembers() {
			walk(m)
		}
	}
	walk(rp)
	return out
}
