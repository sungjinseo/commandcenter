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
	Quota     *Quota         // soft quota; nil is none
	CreatedAt time.Time
}

// Quota is a blob store's soft quota (Nexus "soft quota"): a limit on the
// bytes the store's repositories use (QuotaSpaceUsed) or on the bytes left on
// its filesystem (QuotaSpaceRemaining). Writes are never refused; the status
// checks report a violation and the blob store API shows it.
type Quota struct {
	Type       string `json:"type"`
	LimitBytes int64  `json:"limitBytes"`
}

const (
	QuotaSpaceUsed      = "spaceUsedBytes"
	QuotaSpaceRemaining = "spaceRemainingBytes"
)

// ValidateQuota checks q (nil is fine).
func ValidateQuota(q *Quota) error {
	switch {
	case q == nil:
		return nil
	case q.Type != QuotaSpaceUsed && q.Type != QuotaSpaceRemaining:
		return fmt.Errorf("quota type must be %q or %q", QuotaSpaceUsed, QuotaSpaceRemaining)
	case q.LimitBytes <= 0:
		return fmt.Errorf("quota limitBytes must be positive")
	}
	return nil
}

// storedConfig is the config column: the S3 configuration (sealed secret)
// and the quota under one key.
type storedConfig struct {
	*blob.S3Config
	Quota *Quota `json:"quota,omitempty"`
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
	if err := ValidateQuota(bs.Quota); err != nil {
		return "", "", fmt.Errorf("blob store %q: %w", bs.Name, err)
	}
	sc := storedConfig{Quota: bs.Quota}
	if bs.Type == BlobStoreS3 {
		if bs.S3 == nil {
			return "", "", fmt.Errorf("blob store %q: an s3 store needs its s3 configuration", bs.Name)
		}
		c := *bs.S3
		if c.SecretKey, err = s.seal(c.SecretKey); err != nil {
			return "", "", fmt.Errorf("blob store %q: secretKey: %w", bs.Name, err)
		}
		sc.S3Config = &c
	}
	b, err := json.Marshal(sc)
	if err != nil {
		return "", "", err
	}
	return bs.Type, string(b), nil
}

// SetBlobStoreQuota replaces the quota of the named store (nil removes it),
// or returns ErrNotFound.
func (d *DB) SetBlobStoreQuota(ctx context.Context, name string, q *Quota) error {
	if err := ValidateQuota(q); err != nil {
		return fmt.Errorf("blob store %q: %w", name, err)
	}
	return d.Tx(ctx, func(tx *Tx) error {
		var config string
		err := tx.q.QueryRowContext(ctx, `SELECT config FROM blob_stores WHERE name = ?`, name).Scan(&config)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var m map[string]json.RawMessage // the S3 fields stay as stored (sealed)
		if err := json.Unmarshal([]byte(config), &m); err != nil {
			return fmt.Errorf("blob store %q: decode stored configuration: %w", name, err)
		}
		if m == nil {
			m = map[string]json.RawMessage{}
		}
		delete(m, "quota")
		if q != nil {
			m["quota"], _ = json.Marshal(q)
		}
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `UPDATE blob_stores SET config = ? WHERE name = ?`, string(b), name)
		return err
	})
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
	var sc struct {
		Quota *Quota `json:"quota"`
	}
	if json.Unmarshal([]byte(config), &sc) == nil && ValidateQuota(sc.Quota) == nil {
		bs.Quota = sc.Quota
	}
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
