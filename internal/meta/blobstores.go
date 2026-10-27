package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"commandcenter/internal/blob"
)

// Blob stores (plan 12 Decision 7). Each is a named directory that holds a
// blob.Store, or an S3 bucket with a directory for uploads in progress; a
// repository routes its blob I/O to the store it names. The "default" row
// is seeded on startup with the data directory as its path.

// BlobStore is one named store: its blobs are under Path (Type "file") or
// in the bucket S3 describes (Type "s3"), with Path holding its temp files.
type BlobStore struct {
	Name      string
	Path      string
	Type      string         // BlobStoreFile or BlobStoreS3; "" reads as file
	S3        *blob.S3Config // for BlobStoreS3; its SecretKey is sealed in the row
	CreatedAt time.Time
}

const (
	BlobStoreFile = "file"
	BlobStoreS3   = "s3"
)

const blobStoreCols = `name, path, type, config, created_at`

// row returns what CreateBlobStore stores for bs: its type, and for an S3
// store its configuration as JSON with the secret key sealed.
func (s sealer) blobStoreRow(bs BlobStore) (typ, config string, err error) {
	if bs.Type == "" {
		bs.Type = BlobStoreFile
	}
	if bs.Type != BlobStoreS3 {
		return bs.Type, "{}", nil
	}
	if bs.S3 == nil {
		return "", "", fmt.Errorf("blob store %q: an s3 store needs its s3 configuration", bs.Name)
	}
	c := *bs.S3
	if c.SecretKey, err = s.seal(c.SecretKey); err != nil {
		return "", "", fmt.Errorf("blob store %q: secretKey: %w", bs.Name, err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", "", err
	}
	return bs.Type, string(b), nil
}

// ListBlobStores returns every blob store, ordered by name.
func (d *DB) ListBlobStores(ctx context.Context) ([]BlobStore, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+blobStoreCols+` FROM blob_stores ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BlobStore
	for rows.Next() {
		bs, err := d.scanBlobStore(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, bs)
	}
	return out, rows.Err()
}

// GetBlobStore returns one blob store, or ErrNotFound.
func (d *DB) GetBlobStore(ctx context.Context, name string) (BlobStore, error) {
	return d.scanBlobStore(d.sql.QueryRowContext(ctx, `SELECT `+blobStoreCols+` FROM blob_stores WHERE name = ?`, name))
}

// CreateBlobStore stores a new blob store; ErrExists if the name is taken. A
// zero CreatedAt defaults to now.
func (d *DB) CreateBlobStore(ctx context.Context, bs BlobStore) error {
	if bs.CreatedAt.IsZero() {
		bs.CreatedAt = time.Now()
	}
	typ, config, err := d.box.blobStoreRow(bs)
	if err != nil {
		return err
	}
	return d.Tx(ctx, func(tx *Tx) error { // the check and the insert under one write lock
		var one int
		err := tx.q.QueryRowContext(ctx, `SELECT 1 FROM blob_stores WHERE name = ?`, bs.Name).Scan(&one)
		if err == nil {
			return ErrExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `INSERT INTO blob_stores(`+blobStoreCols+`) VALUES(?,?,?,?,?)`,
			bs.Name, bs.Path, typ, config, toMs(bs.CreatedAt))
		return err
	})
}

// SeedBlobStore inserts the named store, or refreshes the path of an existing
// row (its created_at is kept). It is used on every start for the "default"
// store, whose recorded path tracks the current data directory.
func (d *DB) SeedBlobStore(ctx context.Context, name, path string) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO blob_stores(`+blobStoreCols+`) VALUES(?,?,?,?,?)
ON CONFLICT(name) DO UPDATE SET path = excluded.path`, name, path, BlobStoreFile, "{}", toMs(time.Now()))
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

func (d *DB) scanBlobStore(row scanner) (BlobStore, error) {
	var bs BlobStore
	var created int64
	var config string
	err := row.Scan(&bs.Name, &bs.Path, &bs.Type, &config, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return BlobStore{}, ErrNotFound
	}
	if err != nil {
		return BlobStore{}, err
	}
	bs.CreatedAt = fromMs(created)
	if bs.Type == BlobStoreS3 {
		bs.S3 = &blob.S3Config{}
		if err := json.Unmarshal([]byte(config), bs.S3); err != nil {
			return BlobStore{}, fmt.Errorf("blob store %q: decode stored configuration: %w", bs.Name, err)
		}
		if bs.S3.SecretKey, err = d.box.open(bs.S3.SecretKey); err != nil {
			return BlobStore{}, fmt.Errorf("blob store %q: secretKey: %w", bs.Name, err)
		}
	}
	return bs, nil
}
