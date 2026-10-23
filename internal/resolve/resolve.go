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
	"os"

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
func (r *Resolver) LookupTarget(ctx context.Context, rp *repo.Repository, t proxy.Target, kind proxy.Kind) (proxy.Result, error) {
	return r.lookupTarget(ctx, rp, t, kind, false)
}

// LookupTargetStrict is LookupTarget for a group that fails closed: a member
// error (anything but not-found) ends the lookup with a 5xx instead of trying
// the next member, so a transient error of an internal member never serves a
// later member's same-named package (QA M-4, final review I1).
func (r *Resolver) LookupTargetStrict(ctx context.Context, rp *repo.Repository, t proxy.Target, kind proxy.Kind) (proxy.Result, error) {
	return r.lookupTarget(ctx, rp, t, kind, true)
}

// LookupStrict is LookupTargetStrict for a path.
func (r *Resolver) LookupStrict(ctx context.Context, rp *repo.Repository, path string, kind proxy.Kind) (proxy.Result, error) {
	return r.LookupTargetStrict(ctx, rp, pathTarget(path), kind)
}

// memberFailure is the error a strict group returns for a failed member: the
// member's own status when it is a 5xx, otherwise 502.
func memberFailure(group, member string, err error) error {
	slog.Warn("group member lookup failed", "group", group, "member", member, "err", logsafe.Err(err))
	if repo.StatusOf(err) >= 500 {
		return err
	}
	return repo.Errorf(http.StatusBadGateway, "group member %q failed", member)
}

func (r *Resolver) lookupTarget(ctx context.Context, rp *repo.Repository, t proxy.Target, kind proxy.Kind, strict bool) (proxy.Result, error) {
	switch rp.Type {
	case "hosted":
		a, err := r.Store.Get(ctx, rp.Name, t.Key)
		return proxy.Result{Asset: a}, err
	case "proxy":
		return r.Fetcher.FetchTarget(ctx, rp, t, kind)
	case "group":
		var firstErr error
		for _, m := range rp.OnlineMembers() {
			res, err := r.lookupTarget(ctx, m, t, kind, strict)
			if err == nil {
				return res, nil
			}
			if repo.StatusOf(err) != http.StatusNotFound {
				if strict {
					return proxy.Result{}, memberFailure(rp.Name, m.Name, err)
				}
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		if firstErr != nil {
			return proxy.Result{}, firstErr
		}
		return proxy.Result{}, repo.ErrNotFound
	}
	return proxy.Result{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// All returns the asset at path from every leaf member of rp that has it.
func (r *Resolver) All(ctx context.Context, rp *repo.Repository, path string, kind proxy.Kind) ([]proxy.Result, error) {
	return r.AllTarget(ctx, rp, pathTarget(path), kind)
}

// AllTarget is All for a target whose cache key and remote path differ.
func (r *Resolver) AllTarget(ctx context.Context, rp *repo.Repository, t proxy.Target, kind proxy.Kind) ([]proxy.Result, error) {
	return r.allTarget(ctx, rp, t, kind, false)
}

// allTarget is AllTarget; strict fails closed when a member ordered before
// the first member that answered errors (see LookupTargetStrict). A member
// failing after one that answered is logged and skipped in both modes.
func (r *Resolver) allTarget(ctx context.Context, rp *repo.Repository, t proxy.Target, kind proxy.Kind, strict bool) ([]proxy.Result, error) {
	var out []proxy.Result
	var firstErr error
	for _, m := range leaves(rp) {
		res, err := r.LookupTarget(ctx, m, t, kind)
		if err == nil {
			out = append(out, res)
			continue
		}
		if repo.StatusOf(err) != http.StatusNotFound {
			if strict && len(out) == 0 {
				return nil, memberFailure(rp.Name, m.Name, err)
			}
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
	return r.mergedTarget(ctx, rp, t, contentType, salt, merge, false)
}

// MergedTargetSaltedStrict is MergedTargetSalted for a group that fails
// closed: when a member ordered before the first member that has the document
// errors, the result is that error (5xx; see LookupTargetStrict) instead of a
// merge of the later members. npm uses it (QA M-4, final review I1).
func (r *Resolver) MergedTargetSaltedStrict(ctx context.Context, rp *repo.Repository, t proxy.Target, contentType, salt string, merge MergeFunc) (meta.Asset, error) {
	return r.mergedTarget(ctx, rp, t, contentType, salt, merge, true)
}

func (r *Resolver) mergedTarget(ctx context.Context, rp *repo.Repository, t proxy.Target, contentType, salt string, merge MergeFunc, strict bool) (meta.Asset, error) {
	results, err := r.allTarget(ctx, rp, t, proxy.Metadata, strict)
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
	var files []*os.File
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

// leaves flattens nested groups into their non-group members, each once, in order.
func leaves(rp *repo.Repository) []*repo.Repository {
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
