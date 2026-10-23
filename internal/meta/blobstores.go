package meta

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Blob stores (plan 12 Decision 7). Each is a named directory that holds a
// blob.Store; a repository routes its blob I/O to the store it names. The
// "default" row is seeded on startup with the data directory as its path.

// BlobStore is one named file store.
type BlobStore struct {
	Name      string
	Path      string
	CreatedAt time.Time
}

const blobStoreCols = `name, path, created_at`

// ListBlobStores returns every blob store, ordered by name.
func (d *DB) ListBlobStores(ctx context.Context) ([]BlobStore, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+blobStoreCols+` FROM blob_stores ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BlobStore
	for rows.Next() {
		bs, err := scanBlobStore(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, bs)
	}
	return out, rows.Err()
}

// GetBlobStore returns one blob store, or ErrNotFound.
func (d *DB) GetBlobStore(ctx context.Context, name string) (BlobStore, error) {
	return scanBlobStore(d.sql.QueryRowContext(ctx, `SELECT `+blobStoreCols+` FROM blob_stores WHERE name = ?`, name))
}

// CreateBlobStore stores a new blob store; ErrExists if the name is taken. A
// zero CreatedAt defaults to now.
func (d *DB) CreateBlobStore(ctx context.Context, bs BlobStore) error {
	var one int
	err := d.sql.QueryRowContext(ctx, `SELECT 1 FROM blob_stores WHERE name = ?`, bs.Name).Scan(&one)
	if err == nil {
		return ErrExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if bs.CreatedAt.IsZero() {
		bs.CreatedAt = time.Now()
	}
	_, err = d.sql.ExecContext(ctx, `INSERT INTO blob_stores(`+blobStoreCols+`) VALUES(?,?,?)`,
		bs.Name, bs.Path, toMs(bs.CreatedAt))
	return err
}

// SeedBlobStore inserts the named store, or refreshes the path of an existing
// row (its created_at is kept). It is used on every start for the "default"
// store, whose recorded path tracks the current data directory.
func (d *DB) SeedBlobStore(ctx context.Context, name, path string) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO blob_stores(`+blobStoreCols+`) VALUES(?,?,?)
ON CONFLICT(name) DO UPDATE SET path = excluded.path`, name, path, toMs(time.Now()))
	return err
}

// DeleteBlobStore removes a blob store, or returns ErrNotFound.
func (d *DB) DeleteBlobStore(ctx context.Context, name string) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM blob_stores WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanBlobStore(row scanner) (BlobStore, error) {
	var bs BlobStore
	var created int64
	err := row.Scan(&bs.Name, &bs.Path, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return BlobStore{}, ErrNotFound
	}
	if err != nil {
		return BlobStore{}, err
	}
	bs.CreatedAt = fromMs(created)
	return bs, nil
}
