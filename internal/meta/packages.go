package meta

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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
	q   txConn
	box sealer
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
	if err := fn(&Tx{q: txConn{tx: tx, pg: d.sql.pg}, box: d.box}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (t *Tx) PutAsset(ctx context.Context, a Asset) error { return putAsset(ctx, t.q, a) }

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
	return size, nil
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
	return err
}

func (t *Tx) UpdatePackage(ctx context.Context, repo, name, version, attrs string) error {
	res, err := t.q.ExecContext(ctx, `UPDATE packages SET attrs = ? WHERE repo = ? AND name = ? AND version = ?`,
		attrs, repo, name, version)
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

func (t *Tx) SetDistTag(ctx context.Context, repo, name, tag, version string) error {
	_, err := t.q.ExecContext(ctx, `INSERT INTO dist_tags(repo, name, tag, version) VALUES(?,?,?,?)
ON CONFLICT(repo, name, tag) DO UPDATE SET version = excluded.version`, repo, name, tag, version)
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
	res, err := t.q.ExecContext(ctx, `UPDATE packages SET yanked = ? WHERE repo = ? AND name = ? AND version = ?`,
		boolInt(yanked), repo, name, version)
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
	return nil
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

// PublishedName returns the "name" in the attrs of one of a package's rows,
// or "" when it has none. Cargo stores a crate's name lowercased for lookups;
// its index lines in attrs keep the spelling it was published with.
func (d *DB) PublishedName(ctx context.Context, repo, name string) (string, error) {
	var n sql.NullString
	err := d.sql.QueryRowContext(ctx, pg(d.sql, `SELECT json_extract(attrs, '$.name') FROM packages
WHERE repo = ? AND name = ? LIMIT 1`, `SELECT attrs::json ->> 'name' FROM packages
WHERE repo = ? AND name = ? LIMIT 1`), repo, name).Scan(&n)
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
