package meta

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// DirEntry is one child of a directory in a repository's asset tree.
type DirEntry struct {
	Name           string // segment name, without slashes
	Dir            bool
	Size           int64     // files only
	LastModified   time.Time // files only
	Downloads      int64     // files only
	LastDownloaded time.Time // files only; zero if never downloaded
}

// ListDir returns the immediate children of prefix ("" or ending in "/") in
// path order, at most limit entries; truncated reports more exist. Each step
// is an index seek, so cost is proportional to the entries returned, not to
// the size of the subtree.
func (d *DB) ListDir(ctx context.Context, repo, prefix string, limit int) (entries []DirEntry, truncated bool, err error) {
	upper, bounded := prefixUpper(prefix)
	cursor, inclusive := prefix, false
	for len(entries) <= limit {
		op := ">"
		if inclusive {
			op = ">="
		}
		q := `SELECT path, size, last_modified, downloads, last_downloaded FROM assets WHERE repo = ? AND path ` + op + ` ?`
		args := []any{repo, cursor}
		if bounded {
			q += ` AND path < ?`
			args = append(args, upper)
		}
		q += ` ORDER BY path LIMIT 1`
		var path string
		var size, lm, dl, ld int64
		err := d.sql.QueryRowContext(ctx, q, args...).Scan(&path, &size, &lm, &dl, &ld)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, false, err
		}
		rest := path[len(prefix):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			name := rest[:i]
			entries = append(entries, DirEntry{Name: name, Dir: true})
			// jump past the whole subdirectory
			next, ok := prefixUpper(prefix + name + "/")
			if !ok {
				break
			}
			cursor, inclusive = next, true
		} else {
			entries = append(entries, DirEntry{Name: rest, Size: size, LastModified: fromMs(lm), Downloads: dl, LastDownloaded: fromMs(ld)})
			cursor, inclusive = path, false
		}
	}
	if len(entries) > limit {
		entries, truncated = entries[:limit], true
	}
	return entries, truncated, nil
}

// AssetHit is one search result.
type AssetHit struct{ Repo, Path string }

// SearchAssets returns up to limit asset paths in repos containing q
// (case-insensitive), ordered by repo then path.
func (d *DB) SearchAssets(ctx context.Context, repos []string, q string, limit int) (hits []AssetHit, truncated bool, err error) {
	if len(repos) == 0 {
		return nil, false, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(repos)), ",")
	args := make([]any, 0, len(repos)+2)
	for _, r := range repos {
		args = append(args, r)
	}
	args = append(args, q, limit+1)
	rows, err := d.sql.QueryContext(ctx, `SELECT repo, path FROM assets WHERE repo IN (`+marks+`)
AND instr(lower(path), lower(?)) > 0 ORDER BY repo, path LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var h AssetHit
		if err := rows.Scan(&h.Repo, &h.Path); err != nil {
			return nil, false, err
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(hits) > limit {
		hits, truncated = hits[:limit], true
	}
	return hits, truncated, nil
}

// SearchPackageNames returns distinct package names in repo containing q
// (case-insensitive; "" matches all), at most limit.
func (d *DB) SearchPackageNames(ctx context.Context, repo, q string, limit int) ([]string, bool, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT DISTINCT name FROM packages WHERE repo = ?
AND instr(lower(name), lower(?)) > 0 ORDER BY name LIMIT ?`, repo, q, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, false, err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := false
	if len(names) > limit {
		names, truncated = names[:limit], true
	}
	return names, truncated, nil
}

// RepoStat is the asset count and total size of one repository.
type RepoStat struct{ Assets, Bytes int64 }

// RepoStats returns the number of assets and their total size per repository.
func (d *DB) RepoStats(ctx context.Context) (map[string]RepoStat, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT repo, COUNT(*), COALESCE(SUM(size),0) FROM assets GROUP BY repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RepoStat{}
	for rows.Next() {
		var repo string
		var s RepoStat
		if err := rows.Scan(&repo, &s.Assets, &s.Bytes); err != nil {
			return nil, err
		}
		out[repo] = s
	}
	return out, rows.Err()
}

// BlobStats returns the number of distinct blobs referenced by assets and their total size.
func (d *DB) BlobStats(ctx context.Context) (blobs, bytes int64, err error) {
	err = d.sql.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(size),0) FROM
(SELECT blob_sha256, MAX(size) AS size FROM assets GROUP BY blob_sha256)`).Scan(&blobs, &bytes)
	return blobs, bytes, err
}

// BlobStatsInRepos returns the distinct-blob count and total size for assets
// whose repo is in repos (a blob store's repositories). An empty list means no
// repository uses the store, so it holds no referenced blobs.
func (d *DB) BlobStatsInRepos(ctx context.Context, repos []string) (blobs, bytes int64, err error) {
	if len(repos) == 0 {
		return 0, 0, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(repos)), ",")
	args := make([]any, 0, len(repos))
	for _, r := range repos {
		args = append(args, r)
	}
	err = d.sql.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(size),0) FROM
(SELECT blob_sha256, MAX(size) AS size FROM assets WHERE repo IN (`+marks+`) GROUP BY blob_sha256)`, args...).
		Scan(&blobs, &bytes)
	return blobs, bytes, err
}

// BlobInUseInRepos reports whether any asset in repos references the blob. It
// scopes "in use" to a blob store's repositories, so per-store garbage
// collection never deletes a blob another store (with the same sha) still
// uses. An empty repo list means not in use.
func (d *DB) BlobInUseInRepos(ctx context.Context, sha string, repos []string) (bool, error) {
	if len(repos) == 0 {
		return false, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(repos)), ",")
	args := make([]any, 0, len(repos)+1)
	args = append(args, sha)
	for _, r := range repos {
		args = append(args, r)
	}
	var one int
	err := d.sql.QueryRowContext(ctx, `SELECT 1 FROM assets WHERE blob_sha256 = ? AND repo IN (`+marks+`) LIMIT 1`, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Token is a token's metadata (never its secret or hash).
type Token struct {
	ID, User          string
	Created, LastUsed time.Time
}

// ListTokens returns tokens (all users when user == ""), newest first, at most limit.
func (d *DB) ListTokens(ctx context.Context, user string, limit int) ([]Token, bool, error) {
	q := `SELECT id, user, created_at, last_used_at FROM tokens`
	var args []any
	if user != "" {
		q += ` WHERE user = ?`
		args = append(args, user)
	}
	q += ` ORDER BY created_at DESC, id LIMIT ?`
	args = append(args, limit+1)
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var created, last int64
		if err := rows.Scan(&t.ID, &t.User, &created, &last); err != nil {
			return nil, false, err
		}
		t.Created, t.LastUsed = fromMs(created), fromMs(last)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := false
	if len(out) > limit {
		out, truncated = out[:limit], true
	}
	return out, truncated, nil
}

// GetToken returns one token's metadata, or ErrNotFound.
func (d *DB) GetToken(ctx context.Context, id string) (Token, error) {
	var t Token
	var created, last int64
	err := d.sql.QueryRowContext(ctx, `SELECT id, user, created_at, last_used_at FROM tokens WHERE id = ?`, id).
		Scan(&t.ID, &t.User, &created, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, err
	}
	t.Created, t.LastUsed = fromMs(created), fromMs(last)
	return t, nil
}

// DeleteToken removes a token, or returns ErrNotFound.
func (d *DB) DeleteToken(ctx context.Context, id string) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteTokenByHash removes the token with the given hash (npm logout sends
// the token itself, not its id), or returns ErrNotFound.
func (d *DB) DeleteTokenByHash(ctx context.Context, hash string) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM tokens WHERE hash = ?`, hash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// VersionInfo is a package version without its attrs document.
type VersionInfo struct {
	Version string
	Yanked  bool
	Created time.Time
}

// ListVersions returns the versions of a package, newest first, at most
// limit; truncated reports more exist. Attrs are not read, so the cost stays
// bounded however large the version documents are.
func (d *DB) ListVersions(ctx context.Context, repo, name string, limit int) ([]VersionInfo, bool, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT version, yanked, created_at FROM packages
WHERE repo = ? AND name = ? ORDER BY created_at DESC, version DESC LIMIT ?`, repo, name, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []VersionInfo
	for rows.Next() {
		var v VersionInfo
		var yanked int
		var created int64
		if err := rows.Scan(&v.Version, &yanked, &created); err != nil {
			return nil, false, err
		}
		v.Yanked, v.Created = yanked != 0, fromMs(created)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := false
	if len(out) > limit {
		out, truncated = out[:limit], true
	}
	return out, truncated, nil
}

// AssetInfo is a search result with the asset's size and modification time.
type AssetInfo struct {
	Repo, Path     string
	Size           int64
	LastModified   time.Time
	Downloads      int64
	LastDownloaded time.Time // zero if never downloaded
}

// SearchAssetInfos is SearchAssets with the size and last-modified time of
// each hit: same matching, ordering and limit semantics.
func (d *DB) SearchAssetInfos(ctx context.Context, repos []string, q string, limit int) (hits []AssetInfo, truncated bool, err error) {
	return d.searchAssetInfos(ctx, repos, q, limit, "lower(path)")
}

// SearchGoAssetInfos is SearchAssetInfos for Go repositories, whose paths are
// case-encoded: the match runs on the path with "!" removed and lower-cased, so
// callers pass a lower-case query without "!" (burntsushi finds !burnt!sushi).
func (d *DB) SearchGoAssetInfos(ctx context.Context, repos []string, q string, limit int) (hits []AssetInfo, truncated bool, err error) {
	return d.searchAssetInfos(ctx, repos, q, limit, "replace(lower(path), '!', '')")
}

func (d *DB) searchAssetInfos(ctx context.Context, repos []string, q string, limit int, pathExpr string) (hits []AssetInfo, truncated bool, err error) {
	if len(repos) == 0 {
		return nil, false, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(repos)), ",")
	args := make([]any, 0, len(repos)+2)
	for _, r := range repos {
		args = append(args, r)
	}
	args = append(args, q, limit+1)
	rows, err := d.sql.QueryContext(ctx, `SELECT repo, path, size, last_modified, downloads, last_downloaded FROM assets WHERE repo IN (`+marks+`)
AND instr(`+pathExpr+`, lower(?)) > 0 ORDER BY repo, path LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var h AssetInfo
		var lm, ld int64
		if err := rows.Scan(&h.Repo, &h.Path, &h.Size, &lm, &h.Downloads, &ld); err != nil {
			return nil, false, err
		}
		h.LastModified, h.LastDownloaded = fromMs(lm), fromMs(ld)
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(hits) > limit {
		hits, truncated = hits[:limit], true
	}
	return hits, truncated, nil
}
