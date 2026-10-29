package transfer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ExportParams are a repository.export run's.
type ExportParams struct {
	Repositories   []string  // names; "*" or none: every hosted repository (and proxy with IncludeProxies)
	Destination    string    // under the export root
	IncludeProxies bool      // "*" takes the proxies too
	Since          time.Time // non-zero: only the files changed after it
	HardLinks      bool      // hard-link blobs of a file store on the destination's filesystem
}

// exportPage is how many assets one query reads.
const exportPage = 500

// maxLogged bounds the problems a repository logs one by one.
const maxLogged = 20

// afterAsset is a test hook, called after each exported file.
var afterAsset = func(path string) {}

// Select resolves an export's repository list, refusing groups, unknown
// names and, without includeProxies, proxies.
func (s *Service) Select(names []string, includeProxies bool) ([]*repo.Repository, []string) {
	snap := s.Repos.Snapshot()
	if len(names) == 0 || slices.Contains(names, "*") {
		var out []*repo.Repository
		for _, rp := range snap.Sorted {
			if rp.Type == "hosted" || rp.Type == "proxy" && includeProxies {
				out = append(out, rp)
			}
		}
		return out, nil
	}
	var out []*repo.Repository
	var errs []string
	for _, name := range names {
		rp := snap.ByName[name]
		switch {
		case rp == nil:
			errs = append(errs, fmt.Sprintf("repository %q does not exist", name))
		case rp.Type == "group":
			errs = append(errs, fmt.Sprintf("repository %q is a group: export its members", name))
		case rp.Type == "proxy" && !includeProxies:
			errs = append(errs, fmt.Sprintf("repository %q is a proxy: set includeProxies to export its cache", name))
		default:
			out = append(out, rp)
		}
	}
	return out, errs
}

// Export writes the repositories p names to <destination>/<id>/. A run
// again with the same id rewrites the asset lists and skips the blobs
// already there with the right size and hash; manifest.json is removed
// first and written last.
func (s *Service) Export(ctx context.Context, id string, p ExportParams, log Logger) (Result, error) {
	var res Result
	repos, errs := s.Select(p.Repositories, p.IncludeProxies)
	if len(errs) > 0 {
		return res, errors.New(errs[0])
	}
	rel, err := s.rel(p.Destination)
	if err != nil {
		return res, err
	}
	root, err := os.OpenRoot(s.Root)
	if err != nil {
		return res, fmt.Errorf("export root: %w", err)
	}
	defer root.Close()
	dir := filepath.Join(rel, id)
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return res, fmt.Errorf("destination %q: %w", p.Destination, err)
	}
	exp, err := root.OpenRoot(dir)
	if err != nil {
		return res, fmt.Errorf("destination %q: %w", p.Destination, err)
	}
	defer exp.Close()
	if err := exp.Remove("manifest.json"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}
	m := Manifest{FormatVersion: FormatVersion, CommandCenterVersion: s.Version, ID: id,
		Created: s.now().UTC().Truncate(time.Millisecond), Since: p.Since.UTC(), Repositories: []RepoEntry{}}
	log.Infof("exporting %d repositories to %s", len(repos), filepath.Join(p.Destination, id))
	ex := &exporter{s: s, exp: exp, abs: filepath.Join(s.Root, dir), p: p, log: log, links: p.HardLinks}
	for _, rp := range repos {
		e, err := ex.repo(ctx, rp)
		res.Assets, res.Bytes = res.Assets+e.Assets, res.Bytes+e.Bytes
		if err != nil {
			if ctx.Err() != nil {
				return res, err
			}
			res.Failed++
			log.Errorf("%s: %v", rp.Name, err)
			continue
		}
		res.Repos++
		m.Repositories = append(m.Repositories, e)
	}
	res.Failed += ex.failed
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return res, err
	}
	if err := writeFile(exp, "manifest.json", b); err != nil {
		return res, err
	}
	log.Infof("exported %d repositories, %d files (%d bytes), to %s", res.Repos, res.Assets, res.Bytes, filepath.Join(p.Destination, id))
	if res.Failed > 0 {
		return res, fmt.Errorf("%d files or repositories could not be exported (see the log)", res.Failed)
	}
	return res, nil
}

// exporter is one export run.
type exporter struct {
	s      *Service
	exp    *os.Root // <destination>/<id>
	abs    string   // its path, for hard links
	p      ExportParams
	log    Logger
	links  bool  // still trying hard links
	failed int64 // files not exported
}

// repo writes rp's assets.jsonl and blobs.
func (ex *exporter) repo(ctx context.Context, rp *repo.Repository) (RepoEntry, error) {
	e := RepoEntry{Name: rp.Name, Format: rp.Format, Type: rp.Type, Config: strip(rp.Repository),
		AssetList: rp.Name + "/assets.jsonl"}
	bs, err := ex.s.Store.BlobStore(rp.Name)
	if err != nil {
		return e, err
	}
	if err := ex.exp.MkdirAll(filepath.Join(rp.Name, "blobs"), 0o755); err != nil {
		return e, err
	}
	tmp := e.AssetList + ".tmp"
	f, err := ex.exp.Create(tmp)
	if err != nil {
		return e, err
	}
	defer func() {
		if f != nil {
			f.Close()
			ex.exp.Remove(tmp)
		}
	}()
	h := sha256.New()
	bw := bufio.NewWriterSize(f, 64<<10)
	enc := json.NewEncoder(io.MultiWriter(bw, h))
	rows := rowCache{db: ex.s.DB, repo: rp.Name}
	var linked, copied, present, failed int64
	for after := ""; ; {
		page, err := ex.s.DB.AssetPage(ctx, rp.Name, after, ex.p.Since, exportPage)
		if err != nil {
			return e, err
		}
		for _, a := range page {
			if err := ctx.Err(); err != nil {
				return e, err
			}
			l, err := ex.line(ctx, rp.Format, a, &rows)
			if err != nil {
				return e, err
			}
			how, err := ex.blob(bs, rp.Name, a)
			if err != nil {
				if failed++; failed <= maxLogged {
					ex.log.Errorf("%s: %s not exported: %v", rp.Name, a.Path, err)
				}
				continue
			}
			switch how {
			case "linked":
				linked++
			case "copied":
				copied++
			default:
				present++
			}
			if err := enc.Encode(l); err != nil {
				return e, err
			}
			e.Assets, e.Bytes, e.Packages = e.Assets+1, e.Bytes+a.Size, e.Packages+int64(len(l.Packages))
			if e.Assets%10000 == 0 {
				ex.log.Infof("%s: %d files so far", rp.Name, e.Assets)
			}
			afterAsset(a.Path)
		}
		if len(page) < exportPage {
			break
		}
		after = page[len(page)-1].Path
	}
	if failed > maxLogged {
		ex.log.Errorf("%s: %d more files not exported", rp.Name, failed-maxLogged)
	}
	ex.failed += failed
	if err := bw.Flush(); err != nil {
		return e, err
	}
	if err := f.Close(); err != nil {
		return e, err
	}
	f = nil
	if err := ex.exp.Rename(tmp, e.AssetList); err != nil {
		return e, err
	}
	e.AssetListSHA256 = hex.EncodeToString(h.Sum(nil))
	ex.log.Infof("%s: exported %d files (%d bytes), %d package rows; blobs: %d hard-linked, %d copied, %d already there",
		rp.Name, e.Assets, e.Bytes, e.Packages, linked, copied, present)
	if ex.p.Since.IsZero() && failed == 0 {
		if n, err := ex.s.DB.CountPackages(ctx, rp.Name); err == nil && n > e.Packages {
			ex.log.Warnf("%s: %d package rows were not exported: the files they describe are missing (run repository.verify-blobs)", rp.Name, n-e.Packages)
		}
	}
	return e, nil
}

// line is the assets.jsonl line of a, with its package rows.
func (ex *exporter) line(ctx context.Context, format string, a meta.Asset, rows *rowCache) (Line, error) {
	l := Line{Path: a.Path, SHA256: a.BlobSHA256, Size: a.Size, ContentType: a.ContentType,
		Created: a.CreatedAt, LastUpdated: a.LastModified, LastDownloaded: a.LastDownloaded,
		Component: componentOf(format, a.Path),
		Attrs: AssetAttrs{SHA1: a.SHA1, MD5: a.MD5, SHA512: a.SHA512, RemoteETag: a.RemoteETag,
			RemoteLastModified: a.RemoteLastModified, Checked: a.CheckedAt, Downloads: a.Downloads}}
	name, ok := rowName(format, a.Path)
	if !ok {
		return l, nil
	}
	pkgs, tags, err := rows.get(ctx, name)
	if err != nil {
		return l, err
	}
	for _, p := range pkgs {
		if rowPath(format, p) != a.Path {
			continue
		}
		r := PackageRow{Name: p.Name, Version: p.Version, Attrs: p.Attrs, Yanked: p.Yanked, Created: p.CreatedAt}
		for _, t := range tags {
			if t.Version == p.Version {
				r.Tags = append(r.Tags, t.Tag)
			}
		}
		l.Packages = append(l.Packages, r)
	}
	return l, nil
}

// rowCache holds the rows and tags of the package read last: a package's
// files are adjacent in path order.
type rowCache struct {
	db   *meta.DB
	repo string
	name string
	pkgs []meta.Package
	tags []meta.DistTag
	ok   bool
}

func (c *rowCache) get(ctx context.Context, name string) ([]meta.Package, []meta.DistTag, error) {
	if c.ok && c.name == name {
		return c.pkgs, c.tags, nil
	}
	pkgs, err := c.db.ListPackages(ctx, c.repo, name)
	if err != nil {
		return nil, nil, err
	}
	tags, err := c.db.ListDistTags(ctx, c.repo, name)
	if err != nil {
		return nil, nil, err
	}
	c.name, c.pkgs, c.tags, c.ok = name, pkgs, tags, true
	return pkgs, tags, nil
}

// blob puts a's blob into the export: "present" when it is there already
// (the store's file itself, or a file of the right size and hash),
// "linked" when hard-linked from a file store, else "copied" (and hashed).
func (ex *exporter) blob(bs *blob.Store, repoName string, a meta.Asset) (string, error) {
	if !validSHA(a.BlobSHA256) {
		return "", fmt.Errorf("invalid blob id %q", a.BlobSHA256)
	}
	rel := blobPath(repoName, a.BlobSHA256)
	src, local := bs.LocalPath(a.BlobSHA256)
	if fi, err := ex.exp.Lstat(rel); err == nil {
		if fi.Mode().IsRegular() && fi.Size() == a.Size {
			if si, err := os.Stat(src); local && err == nil && os.SameFile(fi, si) {
				return "present", nil
			}
			if sum, n, err := hashFile(ex.exp, rel); err == nil && sum == a.BlobSHA256 && n == a.Size {
				return "present", nil
			}
		}
		if err := ex.exp.Remove(rel); err != nil {
			return "", err
		}
	}
	if err := ex.exp.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return "", err
	}
	if local && ex.links {
		err := os.Link(src, filepath.Join(ex.abs, rel))
		if err == nil {
			// The link is made by path: check it is where the root sees it.
			fi, err1 := ex.exp.Lstat(rel)
			si, err2 := os.Stat(src)
			if err1 != nil || err2 != nil || !os.SameFile(fi, si) {
				return "", errors.New("the hard link is not in the export directory")
			}
			return "linked", nil
		}
		if !errors.Is(err, fs.ErrNotExist) { // a missing blob is reported by the copy
			ex.links = false
			ex.log.Warnf("blobs cannot be hard-linked into the destination (%v): copying them", err)
		}
	}
	in, err := bs.Open(a.BlobSHA256)
	if errors.Is(err, blob.ErrNotFound) {
		return "", errors.New("its blob is missing from the blob store (run repository.verify-blobs)")
	}
	if err != nil {
		return "", err
	}
	defer in.Close()
	tmp := rel + ".tmp"
	out, err := ex.exp.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && (n != a.Size || hex.EncodeToString(h.Sum(nil)) != a.BlobSHA256) {
		err = fmt.Errorf("its blob in the blob store is corrupt (%d bytes, sha256 %s; run repository.verify-blobs)", n, hex.EncodeToString(h.Sum(nil)))
	}
	if err == nil {
		err = ex.exp.Rename(tmp, rel)
	}
	if err != nil {
		ex.exp.Remove(tmp)
		return "", err
	}
	return "copied", nil
}

// hashFile returns the sha256 and size of a file under root.
func hashFile(root *os.Root, name string) (string, int64, error) {
	f, err := root.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// writeFile writes b to name under root through a temp file, so name
// appears complete or not at all.
func writeFile(root *os.Root, name string, b []byte) error {
	tmp := name + ".tmp"
	if err := root.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		root.Remove(tmp)
		return err
	}
	return nil
}
