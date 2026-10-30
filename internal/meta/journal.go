package meta

import (
	"context"
	"time"
)

// The reconcile journal: every committed write of a content row (an asset,
// a package version, a dist tag) is handed to DB.OnCommit after the
// commit, which records it in the blob store of the row's repository
// (repo.Journal). A database restored from a backup older than the blob
// stores is brought back in line from it (the blobstore.reconcile-plan and
// blobstore.reconcile-execute tasks).

// Change ops and kinds.
const (
	ChangePut    = "put"
	ChangeDelete = "delete"

	ChangeAsset   = "asset"
	ChangePackage = "package"
	ChangeTag     = "tag"
)

// Change is one content row a commit put or deleted.
type Change struct {
	Op      string      `json:"op"`   // ChangePut or ChangeDelete
	Kind    string      `json:"kind"` // ChangeAsset, ChangePackage or ChangeTag
	Repo    string      `json:"repository"`
	Format  string      `json:"format,omitempty"`  // the repository's (the journal fills it in)
	Path    string      `json:"path,omitempty"`    // an asset's
	Name    string      `json:"name,omitempty"`    // a package's or tag's
	Version string      `json:"version,omitempty"` // a package's; a tag's: the version it names
	Tag     string      `json:"tag,omitempty"`     // a tag's
	Asset   *AssetRow   `json:"asset,omitempty"`   // a put asset
	Package *PackageRow `json:"package,omitempty"` // a put package
}

// Key names the row a change is about within its repository: two changes
// with one key are versions of the same row.
func (c Change) Key() string {
	switch c.Kind {
	case ChangePackage:
		return c.Kind + "\x00" + c.Name + "\x00" + c.Version
	case ChangeTag:
		return c.Kind + "\x00" + c.Name + "\x00" + c.Tag
	}
	return c.Kind + "\x00" + c.Path
}

// Label is the row as people read it: an asset's path, name@version, or
// name:tag.
func (c Change) Label() string {
	switch c.Kind {
	case ChangePackage:
		return c.Name + "@" + c.Version
	case ChangeTag:
		return c.Name + ":" + c.Tag
	}
	return c.Path
}

// AssetRow is an asset row in the journal.
type AssetRow struct {
	SHA256             string    `json:"sha256"`
	SHA1               string    `json:"sha1,omitempty"`
	MD5                string    `json:"md5,omitempty"`
	SHA512             string    `json:"sha512,omitempty"`
	Size               int64     `json:"size"`
	ContentType        string    `json:"contentType,omitempty"`
	Created            time.Time `json:"created"`
	LastModified       time.Time `json:"lastModified"`
	Checked            time.Time `json:"checked,omitzero"`
	RemoteETag         string    `json:"remoteETag,omitempty"`
	RemoteLastModified string    `json:"remoteLastModified,omitempty"`
}

// Asset is the row as an Asset of repo at path.
func (r AssetRow) Asset(repo, path string) Asset {
	return Asset{Repo: repo, Path: path, BlobSHA256: r.SHA256, Size: r.Size, ContentType: r.ContentType,
		SHA1: r.SHA1, MD5: r.MD5, SHA512: r.SHA512, CreatedAt: r.Created, LastModified: r.LastModified,
		RemoteETag: r.RemoteETag, RemoteLastModified: r.RemoteLastModified, CheckedAt: r.Checked}
}

// PackageRow is a package version row in the journal: Attrs is the format's
// version document as stored.
type PackageRow struct {
	Format  string    `json:"format"`
	Attrs   string    `json:"attrs"`
	Yanked  bool      `json:"yanked,omitempty"`
	Created time.Time `json:"created"`
}

// Package is the row as a Package of repo.
func (c Change) PackageOf() Package {
	p := Package{Repo: c.Repo, Name: c.Name, Version: c.Version}
	if r := c.Package; r != nil {
		p.Format, p.Attrs, p.Yanked, p.CreatedAt = r.Format, r.Attrs, r.Yanked, r.Created
	}
	return p
}

func (t *Tx) putAsset(a Asset) {
	t.changes = append(t.changes, Change{Op: ChangePut, Kind: ChangeAsset, Repo: a.Repo, Path: a.Path, Asset: &AssetRow{
		SHA256: a.BlobSHA256, SHA1: a.SHA1, MD5: a.MD5, SHA512: a.SHA512, Size: a.Size, ContentType: a.ContentType,
		Created: a.CreatedAt, LastModified: a.LastModified, Checked: a.CheckedAt,
		RemoteETag: a.RemoteETag, RemoteLastModified: a.RemoteLastModified}})
}

func (t *Tx) putPackage(p Package) {
	t.changes = append(t.changes, Change{Op: ChangePut, Kind: ChangePackage, Repo: p.Repo, Name: p.Name, Version: p.Version,
		Package: &PackageRow{Format: p.Format, Attrs: p.Attrs, Yanked: p.Yanked, Created: p.CreatedAt}})
}

// writeRow runs a write that RETURNs one row into dest, in transaction q.
// Text the database cannot hold is ErrInvalidText, as ExecContext reports it.
func writeRow(ctx context.Context, q querier, query string, args []any, dest ...any) error {
	if !storable(args) {
		return ErrInvalidText
	}
	return q.QueryRowContext(ctx, query, args...).Scan(dest...)
}
