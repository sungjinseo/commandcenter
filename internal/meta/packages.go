package meta

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Package is one published version of a package. Attrs holds the format's
// version document as JSON.
type Package struct {
	Repo, Format, Name, Version string
	Attrs                       string
	Yanked                      bool
	CreatedAt                   time.Time
}

// DistTag points a tag at a version.
type DistTag struct {
	Tag, Version string
}

var ErrExists = errors.New("already exists")

// Tx is a write transaction.
type Tx struct {
	q       txConn
	box     sealer
	changes []Change // the content rows written, for OnCommit
}

// Tx runs fn in a write transaction, the only one at a time (see
// pool.begin). An error or panic from fn rolls it back.
func (d *DB) Tx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := d.sql.begin(ctx, true)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
	}()
	t := &Tx{q: txConn{tx: tx}, box: d.box}
	if err := fn(t); err != nil {
		tx.Rollback()
		return err
	}
	var at time.Time
	if len(t.changes) > 0 && d.OnCommit != nil {
		// The database's clock, read under the write lock: the commits of
		// every node in one order, the order they commit in.
		var us int64
		if err := tx.QueryRowContext(ctx, `SELECT (extract(epoch FROM clock_timestamp()) * 1000000)::bigint`).Scan(&us); err != nil {
			tx.Rollback()
			return err
		}
		at = time.UnixMicro(us).UTC()
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if !at.IsZero() {
		d.OnCommit(ctx, at, t.changes)
	}
	return nil
}

func (t *Tx) PutAsset(ctx context.Context, a Asset) error {
	created, prev, err := putAsset(ctx, t.q, a)
	if err == nil {
		a.CreatedAt = created
		t.putAsset(a)
		t.changes[len(t.changes)-1].Prev = prev
	}
	return err
}

// DeleteAsset removes an asset record and returns its size; ErrNotFound when
// there is none.
func (t *Tx) DeleteAsset(ctx context.Context, repo, path string) (int64, error) {
	var size int64
	err := t.q.QueryRowContext(ctx, `SELECT size FROM assets WHERE repo = ? AND path = ?`, repo, path).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if err := deleteAsset(ctx, t.q, repo, path); err != nil {
		return 0, err
	}
	t.changes = append(t.changes, Change{Op: ChangeDelete, Kind: ChangeAsset, Repo: repo, Path: path})
	return size, nil
}

// DeleteUnusedAssets removes those of the assets paths of repo that were not
// downloaded, checked or written since before, and returns how many went and
// their size: one statement, as every other writer waits for this transaction.
// A path for which filesOf gives a prefix stays while an asset has that prefix;
// under the write lock no such asset commits unseen before this one does.
func (t *Tx) DeleteUnusedAssets(ctx context.Context, repo string, paths []string, before time.Time, filesOf func(string) (string, bool)) (n, size int64, err error) {
	if len(paths) == 0 {
		return 0, 0, nil
	}
	ms := toMs(before)
	args := make([]any, 0, 3*len(paths)+5)
	for _, p := range paths {
		var lo, hi any
		if prefix, ok := filesOf(p); ok {
			if upper, ok := prefixUpper(prefix); ok {
				lo, hi = prefix, upper
			}
		}
		args = append(args, p, lo, hi)
	}
	rows, err := t.q.QueryContext(ctx, `DELETE FROM assets a USING (VALUES (?::text, ?::text, ?::text)`+strings.Repeat(`, (?::text, ?::text, ?::text)`, len(paths)-1)+`) AS v(path, lo, hi)
WHERE a.repo = ? AND a.path = v.path AND a.last_downloaded < ? AND a.checked_at < ? AND a.created_at < ? AND a.last_modified < ?
  AND (v.lo IS NULL OR NOT EXISTS (SELECT 1 FROM assets f WHERE f.repo = a.repo AND f.path >= v.lo AND f.path < v.hi))
RETURNING a.path, a.size`, append(args, repo, ms, ms, ms, ms)...)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var s int64
		if err := rows.Scan(&p, &s); err != nil {
			return 0, 0, err
		}
		t.changes = append(t.changes, Change{Op: ChangeDelete, Kind: ChangeAsset, Repo: repo, Path: p})
		n, size = n+1, size+s
	}
	return n, size, rows.Err()
}

// GetAsset is DB.GetAsset inside the transaction; ErrNotFound when there is none.
func (t *Tx) GetAsset(ctx context.Context, repo, path string) (Asset, error) {
	return scanAsset(t.q.QueryRowContext(ctx, `SELECT `+assetSelect+` FROM assets WHERE repo = ? AND path = ?`, repo, path))
}

// ListAssets is DB.ListAssets inside the transaction.
func (t *Tx) ListAssets(ctx context.Context, repo, prefix string) ([]Asset, error) {
	return listAssets(ctx, t.q, repo, prefix)
}

// DeletePackage removes one package version row; ErrNotFound when there is none.
func (t *Tx) DeletePackage(ctx context.Context, repo, name, version string) error {
	res, err := t.q.ExecContext(ctx, `DELETE FROM packages WHERE repo = ? AND name = ? AND version = ?`, repo, name, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	t.changes = append(t.changes, Change{Op: ChangeDelete, Kind: ChangePackage, Repo: repo, Name: name, Version: version})
	return nil
}

func (t *Tx) InsertPackage(ctx context.Context, p Package) error {
	var one int
	err := t.q.QueryRowContext(ctx, `SELECT 1 FROM packages WHERE repo = ? AND name = ? AND version = ?`,
		p.Repo, p.Name, p.Version).Scan(&one)
	if err == nil {
		return ErrExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = t.q.ExecContext(ctx, `INSERT INTO packages(repo, format, name, version, attrs, yanked, created_at)
VALUES(?,?,?,?,?,?,?)`, p.Repo, p.Format, p.Name, p.Version, p.Attrs, boolInt(p.Yanked), toMs(p.CreatedAt))
	if err == nil {
		t.putPackage(p)
	}
	return err
}

func (t *Tx) UpdatePackage(ctx context.Context, repo, name, version, attrs string) error {
	return t.updatePackage(ctx, `attrs = ?`, attrs, repo, name, version)
}

// updatePackage sets one column of a package row and records the row as it
// is then; ErrNotFound when there is none.
func (t *Tx) updatePackage(ctx context.Context, set string, value any, repo, name, version string) error {
	p := Package{Repo: repo, Name: name, Version: version}
	var yanked int
	var created int64
	err := writeRow(ctx, t.q, `UPDATE packages SET `+set+` WHERE repo = ? AND name = ? AND version = ?
RETURNING format, attrs, yanked, created_at`, []any{value, repo, name, version}, &p.Format, &p.Attrs, &yanked, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	p.Yanked, p.CreatedAt = yanked != 0, fromMs(created)
	t.putPackage(p)
	return nil
}

func (t *Tx) ListPackages(ctx context.Context, repo, name string) ([]Package, error) {
	return listPackages(ctx, t.q, repo, name)
}

func (d *DB) ListPackages(ctx context.Context, repo, name string) ([]Package, error) {
	return listPackages(ctx, d.sql, repo, name)
}

func listPackages(ctx context.Context, q querier, repo, name string) ([]Package, error) {
	rows, err := q.QueryContext(ctx, `SELECT repo, format, name, version, attrs, yanked, created_at
FROM packages WHERE repo = ? AND name = ? ORDER BY created_at, version`, repo, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Package
	for rows.Next() {
		var p Package
		var yanked int
		var created int64
		if err := rows.Scan(&p.Repo, &p.Format, &p.Name, &p.Version, &p.Attrs, &yanked, &created); err != nil {
			return nil, err
		}
		p.Yanked, p.CreatedAt = yanked != 0, fromMs(created)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListPackageVersions returns the rows of a package without their attrs, for
// what needs no more of a package with thousands of versions (the attrs are
// megabytes per request), in ListPackages's order.
func (d *DB) ListPackageVersions(ctx context.Context, repo, name string) ([]Package, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT version, created_at FROM packages WHERE repo = ? AND name = ? ORDER BY created_at, version`, repo, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Package
	for rows.Next() {
		p := Package{Repo: repo, Name: name}
		var created int64
		if err := rows.Scan(&p.Version, &created); err != nil {
			return nil, err
		}
		p.CreatedAt = fromMs(created)
		out = append(out, p)
	}
	return out, rows.Err()
}

// EachPackage calls fn with the rows of the given versions of a package, in
// that order, reading them in batches: a document of a package with thousands
// of versions holds a batch at a time, and no connection while fn writes to a
// client. A version that is gone by then is skipped.
func (d *DB) EachPackage(ctx context.Context, repo, name string, versions []string, fn func(Package) error) error {
	const batch = 64
	for len(versions) > 0 {
		n := min(batch, len(versions))
		found, err := d.packagesIn(ctx, repo, name, versions[:n])
		if err != nil {
			return err
		}
		for _, v := range versions[:n] {
			if p, ok := found[v]; ok {
				if err := fn(p); err != nil {
					return err
				}
			}
		}
		versions = versions[n:]
	}
	return nil
}

func (d *DB) packagesIn(ctx context.Context, repo, name string, versions []string) (map[string]Package, error) {
	args := make([]any, 0, len(versions)+2)
	args = append(args, repo, name)
	for _, v := range versions {
		args = append(args, v)
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(versions)), ",")
	rows, err := d.sql.QueryContext(ctx, `SELECT format, version, attrs, yanked, created_at FROM packages
WHERE repo = ? AND name = ? AND version IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]Package, len(versions))
	for rows.Next() {
		p := Package{Repo: repo, Name: name}
		var yanked int
		var created int64
		if err := rows.Scan(&p.Format, &p.Version, &p.Attrs, &yanked, &created); err != nil {
			return nil, err
		}
		p.Yanked, p.CreatedAt = yanked != 0, fromMs(created)
		out[p.Version] = p
	}
	return out, rows.Err()
}

func (t *Tx) SetDistTag(ctx context.Context, repo, name, tag, version string) error {
	var prev string
	err := writeRow(ctx, t.q, `WITH prev AS (SELECT version FROM dist_tags WHERE repo = ? AND name = ? AND tag = ?)
INSERT INTO dist_tags(repo, name, tag, version) VALUES(?,?,?,?)
ON CONFLICT(repo, name, tag) DO UPDATE SET version = excluded.version
RETURNING COALESCE((SELECT version FROM prev), '')`, []any{repo, name, tag, repo, name, tag, version}, &prev)
	if err == nil {
		t.changes = append(t.changes, Change{Op: ChangePut, Kind: ChangeTag, Repo: repo, Name: name, Tag: tag, Version: version, Prev: prev})
	}
	return err
}

func (t *Tx) DeleteDistTag(ctx context.Context, repo, name, tag string) error {
	res, err := t.q.ExecContext(ctx, `DELETE FROM dist_tags WHERE repo = ? AND name = ? AND tag = ?`, repo, name, tag)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	t.changes = append(t.changes, Change{Op: ChangeDelete, Kind: ChangeTag, Repo: repo, Name: name, Tag: tag})
	return nil
}

func (t *Tx) ListDistTags(ctx context.Context, repo, name string) ([]DistTag, error) {
	return listDistTags(ctx, t.q, repo, name)
}

func (d *DB) ListDistTags(ctx context.Context, repo, name string) ([]DistTag, error) {
	return listDistTags(ctx, d.sql, repo, name)
}

func listDistTags(ctx context.Context, q querier, repo, name string) ([]DistTag, error) {
	rows, err := q.QueryContext(ctx, `SELECT tag, version FROM dist_tags WHERE repo = ? AND name = ? ORDER BY tag`, repo, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DistTag
	for rows.Next() {
		var d DistTag
		if err := rows.Scan(&d.Tag, &d.Version); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ListPackageNames returns the distinct package names of repo, sorted.
func (d *DB) ListPackageNames(ctx context.Context, repo string) ([]string, error) {
	return listPackageNames(ctx, d.sql, repo)
}

// ListPackageNames is DB.ListPackageNames inside the transaction (helm
// rebuilds index.yaml from every row in the transaction that changes one).
func (t *Tx) ListPackageNames(ctx context.Context, repo string) ([]string, error) {
	return listPackageNames(ctx, t.q, repo)
}

func listPackageNames(ctx context.Context, q querier, repo string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT name FROM packages WHERE repo = ? ORDER BY name`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SetYanked sets the yanked flag of one package version.
func (t *Tx) SetYanked(ctx context.Context, repo, name, version string, yanked bool) error {
	return t.updatePackage(ctx, `yanked = ?`, boolInt(yanked), repo, name, version)
}

// ListPackageNamesCanonical returns the distinct names of repo whose canonical
// form (lower case, `_` folded to `-`) equals canonical, sorted.
func (d *DB) ListPackageNamesCanonical(ctx context.Context, repo, canonical string) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT DISTINCT name FROM packages
WHERE repo = ? AND replace(lower(name), '_', '-') = ? ORDER BY name`, repo, canonical)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// HostedPackages returns which of names a hosted repository of format holds,
// and in which repositories. It is what keeps private names away from an
// upstream, so it looks at every hosted repository, online or not.
func (d *DB) HostedPackages(ctx context.Context, format string, names []string) (map[string][]string, error) {
	const batch = 256
	out := map[string][]string{}
	for len(names) > 0 {
		n := min(batch, len(names))
		args := make([]any, 0, n+1)
		args = append(args, format)
		for _, name := range names[:n] {
			args = append(args, name)
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", n), ",")
		rows, err := d.sql.QueryContext(ctx, `SELECT DISTINCT p.repo, p.name FROM repositories r JOIN packages p ON p.repo = r.name
WHERE r.format = ? AND r.type = 'hosted' AND p.name IN (`+marks+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var repo, name string
			if err := rows.Scan(&repo, &name); err != nil {
				rows.Close()
				return nil, err
			}
			out[name] = append(out[name], repo)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		names = names[n:]
	}
	return out, nil
}

// PublishedName returns the "name" in the attrs of one of a package's rows,
// or "" when it has none. Cargo stores a crate's name lowercased for lookups;
// its index lines in attrs keep the spelling it was published with.
func (d *DB) PublishedName(ctx context.Context, repo, name string) (string, error) {
	var n sql.NullString
	err := d.sql.QueryRowContext(ctx, `SELECT attrs::json ->> 'name' FROM packages
WHERE repo = ? AND name = ? LIMIT 1`, repo, name).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return n.String, err
}

// PackageStamp summarizes the versions of one package without reading attrs.
type PackageStamp struct {
	Count  int
	Newest time.Time // newest created_at
	Hash   string    // hex sha256 of every (version, yanked, created_at)
}

// PackageStamp streams (version, yanked, created_at) of a package's rows and
// hashes them, so a cheap validator exists whatever the size of the version
// documents. Count is 0 when the package has no rows.
func (d *DB) PackageStamp(ctx context.Context, repo, name string) (PackageStamp, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT version, yanked, created_at FROM packages
WHERE repo = ? AND name = ? ORDER BY created_at, version`, repo, name)
	if err != nil {
		return PackageStamp{}, err
	}
	defer rows.Close()
	h := sha256.New()
	var st PackageStamp
	var newest int64
	for rows.Next() {
		var v string
		var yanked int
		var created int64
		if err := rows.Scan(&v, &yanked, &created); err != nil {
			return PackageStamp{}, err
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", v, yanked, created)
		if created > newest {
			newest = created
		}
		st.Count++
	}
	if err := rows.Err(); err != nil {
		return PackageStamp{}, err
	}
	if st.Count > 0 {
		st.Newest, st.Hash = fromMs(newest), hex.EncodeToString(h.Sum(nil))
	}
	return st, nil
}
