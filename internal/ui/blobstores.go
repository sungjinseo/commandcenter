package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/sysinfo"
)

// Blob-store admin API (plan 12 Decision 7). Reads need an admin; mutations
// need an interactive admin and the UI header. A store's path and bucket
// are immutable (no PUT). Deleting a store leaves its files on disk and its
// objects in the bucket.
//
// Create and delete run under the registry's write lock, which repository
// create and update hold while they check that a store exists: a store cannot
// be deleted between that check and the repository's commit, and two store
// creates cannot both pass the isolation check with overlapping paths.

func (u *UI) blobStoreRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/blobstores", u.listBlobStores)
	mux.HandleFunc("POST /-/api/blobstores", u.createBlobStore)
	mux.HandleFunc("GET /-/api/blobstores/{name}", u.getBlobStore)
	mux.HandleFunc("DELETE /-/api/blobstores/{name}", u.deleteBlobStore)
}

// blobStoreDeleteHook, if set, runs inside a delete after its in-use check,
// with the registry lock held (a test hook).
var blobStoreDeleteHook func()

// blobStoreRequest is the body of POST. Type is "file" (the default) or
// "s3", which takes the bucket in S3. An empty path puts the store (for an
// S3 store: its temp files) under <dataDir>/blobstores/<name> (recorded
// relative to the data directory); an absolute path is used as given; a
// relative one is refused (QA N-22).
type blobStoreRequest struct {
	Name string         `json:"name"`
	Path string         `json:"path"`
	Type string         `json:"type"`
	S3   *blob.S3Config `json:"s3"`
}

// blobStoreJSON is one store's wire shape. Path is the directory the store
// opens at. FsTotal/FsFree are null when the filesystem cannot be queried,
// and for an S3 store, whose bucket S3 describes (without the secret key).
// Warning is set when a file store lies outside the data directory (QA N-23).
type blobStoreJSON struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Path      string  `json:"path"`
	S3        *s3JSON `json:"s3,omitempty"`
	BlobCount int64   `json:"blobCount"`
	UsedBytes int64   `json:"usedBytes"`
	FsTotal   *uint64 `json:"fsTotal"`
	FsFree    *uint64 `json:"fsFree"`
	RepoCount int     `json:"repoCount"`
	Warning   string  `json:"warning,omitempty"`
}

// s3JSON is blob.S3Config without its secret: what GET shows.
type s3JSON struct {
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	Region    string `json:"region"`
	Endpoint  string `json:"endpoint"`
	PathStyle bool   `json:"pathStyle"`
	AccessKey string `json:"accessKeyId"`
	SecretSet bool   `json:"secretSet"` // the secret itself is never shown
	SSE       string `json:"sse"`
}

// blobStoreView builds the DTO for one stored row: usage scoped to the store's
// repositories (repo.ReposForStore, the rule routing and GC use) and the
// filesystem totals of its blob tree.
func (u *UI) blobStoreView(ctx context.Context, bs meta.BlobStore) (blobStoreJSON, error) {
	repos := repo.ReposForStore(u.Repos.Snapshot(), bs.Name)
	count, bytes, err := u.DB.BlobStatsInRepos(ctx, repos)
	if err != nil {
		return blobStoreJSON{}, err
	}
	root := repo.StoreRoot(u.DataDir, bs)
	out := blobStoreJSON{
		Name: bs.Name, Type: bs.Type, Path: root,
		BlobCount: count, UsedBytes: bytes, RepoCount: len(repos),
	}
	if out.Type == "" {
		out.Type = meta.BlobStoreFile
	}
	if bs.Type == meta.BlobStoreS3 && bs.S3 != nil {
		c := bs.S3
		out.S3 = &s3JSON{Bucket: c.Bucket, Prefix: c.Prefix, Region: c.Region, Endpoint: c.Endpoint,
			PathStyle: c.PathStyle, AccessKey: c.AccessKey, SecretSet: c.SecretKey != "", SSE: c.SSE}
		return out, nil
	}
	if total, free, ok := sysinfo.DiskUsage(filepath.Join(root, "blobs")); ok {
		out.FsTotal, out.FsFree = &total, &free
	}
	dd, err1 := realPath(u.DataDir)
	rt, err2 := realPath(root)
	if err1 == nil && err2 == nil && !isSubpath(dd, rt) {
		out.Warning = "outside the data directory " + u.DataDir + ": mount and back it up separately"
	}
	return out, nil
}

func (u *UI) listBlobStores(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	rows, err := u.DB.ListBlobStores(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	out := make([]blobStoreJSON, 0, len(rows))
	for _, bs := range rows {
		v, err := u.blobStoreView(r.Context(), bs)
		if err != nil {
			u.fail(w, r, err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"blobstores": out})
}

func (u *UI) getBlobStore(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	bs, err := u.DB.GetBlobStore(r.Context(), r.PathValue("name"))
	if err != nil {
		u.writeBlobStoreError(w, r, err)
		return
	}
	v, err := u.blobStoreView(r.Context(), bs)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (u *UI) createBlobStore(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	if u.Store == nil {
		writeError(w, http.StatusNotImplemented, "blob stores are not available")
		return
	}
	var body blobStoreRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if err := repo.CheckName("blob store", body.Name); err != nil {
		u.writeBlobStoreError(w, r, err)
		return
	}
	row := meta.BlobStore{Name: body.Name, Path: filepath.Join("blobstores", body.Name), Type: body.Type}
	switch {
	case filepath.IsAbs(body.Path):
		row.Path = filepath.Clean(body.Path)
	case body.Path != "":
		u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{
			"blob store path must be absolute; leave it empty for <dataDir>/blobstores/" + body.Name}})
		return
	}
	switch row.Type {
	case "":
		row.Type = meta.BlobStoreFile
	case meta.BlobStoreFile:
	case meta.BlobStoreS3:
		if body.S3 == nil {
			u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{"an s3 blob store needs \"s3\" with at least its bucket"}})
			return
		}
		if err := body.S3.Validate(); err != nil {
			u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{"s3: " + err.Error()}})
			return
		}
		row.S3 = body.S3
	default:
		u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{`blob store type must be "file" or "s3"`}})
		return
	}
	err := u.Repos.WithWriteLock(func(*repo.Snapshot) error {
		ctx := r.Context()
		// A duplicate name is a conflict, not an isolation problem.
		if _, err := u.DB.GetBlobStore(ctx, body.Name); err == nil {
			return meta.ErrExists
		} else if !errors.Is(err, meta.ErrNotFound) {
			return err
		}
		root := repo.StoreRoot(u.DataDir, row)
		if row.Type == meta.BlobStoreS3 {
			if err := u.checkS3Isolation(ctx, *row.S3); err != nil {
				return err
			}
		} else {
			if err := u.checkStoreIsolation(ctx, root); err != nil {
				return err
			}
			if err := checkNoBlobs(root); err != nil {
				return &repo.ValidationError{Errors: []string{fmt.Sprintf("blob store %q: %v", body.Name, err)}}
			}
		}
		bs, err := repo.OpenStore(u.DataDir, row)
		if err != nil {
			return &repo.ValidationError{Errors: []string{fmt.Sprintf("blob store %q: directory %s is not usable: %v", body.Name, root, err)}}
		}
		if err := probeWritable(root); err != nil {
			return &repo.ValidationError{Errors: []string{fmt.Sprintf("blob store %q: directory %s is not writable: %v", body.Name, root, err)}}
		}
		if row.Type == meta.BlobStoreS3 {
			if err := bs.Check(); err != nil {
				return &repo.ValidationError{Errors: []string{fmt.Sprintf("blob store %q: bucket %s is not usable: %v", body.Name, row.S3.Bucket, err)}}
			}
		}
		if err := u.DB.CreateBlobStore(ctx, row); err != nil {
			return err
		}
		u.Store.AddStore(body.Name, bs)
		return nil
	})
	if err != nil {
		u.writeBlobStoreError(w, r, err)
		return
	}
	stored, err := u.DB.GetBlobStore(r.Context(), body.Name)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	detail := repo.StoreRoot(u.DataDir, stored)
	if stored.Type == meta.BlobStoreS3 && stored.S3 != nil {
		detail = "s3://" + stored.S3.Bucket + "/" + stored.S3.Prefix
	}
	u.emit(r, "blobstore.create", "", body.Name, detail)
	v, err := u.blobStoreView(r.Context(), stored)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// errStoreInUse refuses deleting a store a repository uses.
type errStoreInUse struct {
	name  string
	repos []string
}

func (e *errStoreInUse) Error() string {
	return fmt.Sprintf("blob store %q is used by repositories: %s", e.name, strings.Join(e.repos, ", "))
}

func (u *UI) deleteBlobStore(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	name := r.PathValue("name")
	if name == repo.DefaultStore {
		writeError(w, http.StatusConflict, "the default blob store cannot be deleted")
		return
	}
	err := u.Repos.WithWriteLock(func(s *repo.Snapshot) error {
		if users := repo.ReposForStore(s, name); len(users) > 0 {
			return &errStoreInUse{name: name, repos: users}
		}
		if blobStoreDeleteHook != nil {
			blobStoreDeleteHook()
		}
		if err := u.DB.DeleteBlobStore(r.Context(), name); err != nil {
			return err
		}
		if u.Store != nil {
			u.Store.RemoveStore(name) // the directory is left on disk
		}
		return nil
	})
	var inUse *errStoreInUse
	if errors.As(err, &inUse) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": inUse.Error(), "repositories": inUse.repos})
		return
	}
	if err != nil {
		u.writeBlobStoreError(w, r, err)
		return
	}
	u.emit(r, "blobstore.delete", "", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// checkS3Isolation rejects a new S3 store on the same bucket and prefix (at
// the same endpoint) as an existing one: their GCs would delete each
// other's blobs.
func (u *UI) checkS3Isolation(ctx context.Context, c blob.S3Config) error {
	rows, err := u.DB.ListBlobStores(ctx)
	if err != nil {
		return err
	}
	for _, bs := range rows {
		if o := bs.S3; bs.Type == meta.BlobStoreS3 && o != nil && o.Location() == c.Location() {
			return &repo.ValidationError{Errors: []string{
				fmt.Sprintf("s3 bucket %s prefix %q is already the %q store's", c.Bucket, c.Prefix, bs.Name)}}
		}
	}
	return nil
}

// checkStoreIsolation rejects a new store whose blob tree equals, contains or
// is contained by an existing store's blob tree. Trees are compared on their
// /blobs subdirectories (not the roots, since the data directory contains
// every relative store) after resolving symlinks, so a link or an alias of
// another store's directory is caught too.
func (u *UI) checkStoreIsolation(ctx context.Context, root string) error {
	newTree, err := realPath(filepath.Join(root, "blobs"))
	if err != nil {
		return &repo.ValidationError{Errors: []string{"invalid blob store path"}}
	}
	rows, err := u.DB.ListBlobStores(ctx)
	if err != nil {
		return err
	}
	for _, bs := range rows {
		other, err := realPath(filepath.Join(repo.StoreRoot(u.DataDir, bs), "blobs"))
		if err != nil {
			// Fail closed (final review M1): a tree that cannot be resolved
			// (a dangling symlink) cannot be shown to be disjoint.
			return &repo.ValidationError{Errors: []string{
				fmt.Sprintf("cannot check the blob store path against the %q store at %s: its path cannot be resolved", bs.Name, repo.StoreRoot(u.DataDir, bs))}}
		}
		if treeConflicts(newTree, other) {
			return &repo.ValidationError{Errors: []string{
				fmt.Sprintf("blob store path conflicts with the %q store at %s", bs.Name, repo.StoreRoot(u.DataDir, bs))}}
		}
	}
	return nil
}

// realPath makes p absolute and resolves the symlinks of its longest
// existing prefix; the part that does not exist yet is appended as is.
func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	existing, rest := abs, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing { // reached the root without finding anything
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, rest), nil
}

// checkNoBlobs refuses a root whose blobs/sha256 directory already holds
// entries: a new store has no repositories, so its GC would delete them.
func checkNoBlobs(root string) error {
	d, err := os.Open(filepath.Join(root, "blobs", "sha256"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer d.Close()
	if _, err := d.Readdirnames(1); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("directory already contains blobs")
}

// treeConflicts reports whether two blob trees are the same, or one nests
// inside the other.
func treeConflicts(a, b string) bool {
	return isSubpath(a, b) || isSubpath(b, a)
}

// isSubpath reports whether child is at or below parent.
func isSubpath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// probeWritable confirms a store's tmp directory accepts a file, which is what
// blob writes use. blob.Open has already created root/tmp.
func probeWritable(root string) error {
	f, err := os.CreateTemp(filepath.Join(root, "tmp"), "probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// writeBlobStoreError maps blob-store errors: validation 400 with the list,
// a name taken 409, not found 404, the registry's HTTP errors as they are,
// anything else 500.
func (u *UI) writeBlobStoreError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *repo.ValidationError
	var he *repo.Error
	switch {
	case errors.As(err, &ve):
		first := "invalid blob store"
		if len(ve.Errors) > 0 {
			first = ve.Errors[0]
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": first, "errors": ve.Errors})
	case errors.Is(err, meta.ErrExists):
		writeError(w, http.StatusConflict, "a blob store with this name already exists")
	case errors.Is(err, meta.ErrNotFound):
		writeError(w, http.StatusNotFound, "blob store not found")
	case errors.As(err, &he):
		writeError(w, he.Status, he.Msg)
	default:
		u.fail(w, r, err)
	}
}
