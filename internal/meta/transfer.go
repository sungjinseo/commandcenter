package meta

import (
	"context"
	"time"
)

// Queries of repository export and import (internal/transfer).

// AssetPage returns up to limit assets of repo with a path greater than
// after ("" for the start), in path order; with a non-zero since, only
// those last modified after it.
func (d *DB) AssetPage(ctx context.Context, repo, after string, since time.Time, limit int) ([]Asset, error) {
	q := `SELECT ` + assetSelect + ` FROM assets WHERE repo = ? AND path > ?`
	args := []any{repo, after}
	if !since.IsZero() {
		q += ` AND last_modified > ?`
		args = append(args, toMs(since))
	}
	rows, err := d.sql.QueryContext(ctx, q+` ORDER BY path LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Asset, 0, limit)
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountPackages returns how many package rows repo has.
func (d *DB) CountPackages(ctx context.Context, repo string) (int64, error) {
	var n int64
	err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM packages WHERE repo = ?`, repo).Scan(&n)
	return n, err
}

// ImportAsset inserts or replaces an asset with every column as given:
// unlike PutAsset it keeps the given creation time and writes the download
// counters.
func (t *Tx) ImportAsset(ctx context.Context, a Asset) error {
	_, err := t.q.ExecContext(ctx, `INSERT INTO assets(`+assetSelect+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(repo, path) DO UPDATE SET
  blob_sha256 = excluded.blob_sha256, size = excluded.size, content_type = excluded.content_type,
  sha1 = excluded.sha1, md5 = excluded.md5, sha512 = excluded.sha512,
  created_at = excluded.created_at, last_modified = excluded.last_modified, remote_etag = excluded.remote_etag,
  remote_last_modified = excluded.remote_last_modified, checked_at = excluded.checked_at,
  downloads = excluded.downloads, last_downloaded = excluded.last_downloaded`,
		a.Repo, a.Path, a.BlobSHA256, a.Size, a.ContentType, a.SHA1, a.MD5, a.SHA512,
		toMs(a.CreatedAt), toMs(a.LastModified), a.RemoteETag, a.RemoteLastModified, toMs(a.CheckedAt),
		a.Downloads, toMs(a.LastDownloaded))
	return err
}

// ImportPackage writes a package row with every column as given: replace
// overwrites an existing row of the same name and version, otherwise an
// existing row is kept.
func (t *Tx) ImportPackage(ctx context.Context, p Package, replace bool) error {
	conflict := ` DO NOTHING`
	if replace {
		conflict = ` DO UPDATE SET format = excluded.format, attrs = excluded.attrs, yanked = excluded.yanked, created_at = excluded.created_at`
	}
	_, err := t.q.ExecContext(ctx, `INSERT INTO packages(repo, format, name, version, attrs, yanked, created_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(repo, name, version)`+conflict,
		p.Repo, p.Format, p.Name, p.Version, p.Attrs, boolInt(p.Yanked), toMs(p.CreatedAt))
	return err
}
