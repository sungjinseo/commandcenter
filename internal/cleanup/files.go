package cleanup

import (
	"context"
	"fmt"
	"strings"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// A proxy's cached files that belong to no component (the documents and
// indexes of a format, whatever component.DeriveAsset does not know) are
// fetched again on demand, so repository.purge-unused removes those no one had
// a use for, as Nexus does; otherwise a proxy's disk use only grows. The rest
// of this file is what it must not remove.

// fileUsed is when a cached file was last of use, as near as its row tells:
// read (ServeAsset, repo.Store.Used), found current at the remote, or written.
func fileUsed(a meta.ScanAsset) time.Time {
	used := assetWritten(a)
	if a.LastDownloaded.After(used) {
		used = a.LastDownloaded
	}
	if a.Checked.After(used) {
		used = a.Checked
	}
	return used
}

// keptFile reports whether a proxy keeps file p however old its row looks: it is
// the proxy's own record rather than a copy of the remote's file, and the row
// does not tell when it was last used (a read through Store.Get).
func keptFile(format, p string) bool {
	switch format {
	case "p2": // the table of mirrored children, which every request below .remote/ reads
		return p == ".remote/index.json"
	case "huggingface": // a listing's next page, which a 304 of the listing keeps without touching it
		return strings.HasSuffix(p, "~next")
	case "terraform": // a module's last download answer, which stands in while the remote fails
		return strings.HasPrefix(p, "v1/modules/") && strings.HasSuffix(p, "/download")
	}
	return false
}

// OwnerRule reports whether a group of format answers for a package from the
// first member that has it, alone, a proxy having it when its document is
// cached (resolve.Owner and its like in the format packages). That document
// outlives its remote's 404 so that a later member cannot stand in for the
// package (dependency confusion): deleting it hands the package on.
func OwnerRule(format string) bool {
	switch format {
	case "npm", "pypi", "composer", "conan", "terraform", "rubygems", "nuget", "swift", "helm", "cargo":
		return true
	}
	return false
}

// FilesOf returns the path prefix of the cached files that the document p of
// format lists, for a document that stays while any is cached: an npm document
// holds the versions its remote dropped and nothing else does (only their
// tarballs would be left), and a PyPI project page is how a client finds
// the files of a project its remote dropped.
func FilesOf(format, p string) (string, bool) {
	switch format {
	case "npm":
		if name, ok := strings.CutSuffix(p, "/package.json"); ok {
			return name + "/-/", true
		}
	case "pypi":
		if rest, ok := strings.CutPrefix(p, "simple/"); ok {
			if project, ok := strings.CutSuffix(rest, "/index"); ok {
				return "packages/" + project + "/", true
			}
		}
	}
	return "", false
}

// DeleteFiles deletes, in one transaction, those of the cached files paths of
// proxy rp that are still unused since before: one read or fetched since it
// was evaluated stays. The blobs are reclaimed later by the compact task.
func (r *Registry) DeleteFiles(ctx context.Context, rp *repo.Repository, paths []string, before time.Time, actor string) (Result, error) {
	if rp.Type != "proxy" {
		return Result{}, fmt.Errorf("file delete in %s repository %q: %w", rp.Type, rp.Name, ErrNotSupported)
	}
	var n, size int64
	err := r.store.Tx(ctx, rp, func(tx *meta.Tx) (err error) {
		n, size, err = tx.DeleteUnusedAssets(ctx, rp.Name, paths, before)
		return err
	})
	if err != nil {
		return Result{}, err
	}
	if n > 0 {
		r.store.EmitAs("delete", actor, rp.Name, "cached files without a component", fmt.Sprintf("assets=%d", n))
	}
	return Result{Assets: int(n), Bytes: size}, nil
}
