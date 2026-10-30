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

// BlobStore is one named store: its blobs are under Path (Type "file"), in
// the bucket S3 describes (Type "s3"), in the member stores Group names
// (Type "group") or in the container Azure describes (Type "azure"); an S3 or
// azure store and a group keep only their temp files in Path.
type BlobStore struct {
	Name      string
	Path      string
	Type      string            // BlobStoreFile or BlobStoreS3; "" reads as file
	S3        *blob.S3Config    // for BlobStoreS3; its SecretKey is sealed in the row
	Quota     *Quota            // soft quota; nil is none
	Group     *blob.GroupConfig // for BlobStoreGroup
	Azure     *blob.AzureConfig // for BlobStoreAzure; its accountKey, sasToken and clientSecret are sealed in the row
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
	*blob.GroupConfig
	Quota *Quota            `json:"quota,omitempty"`
	Azure *blob.AzureConfig `json:"azure,omitempty"` // under its own key: its prefix and endpoint would clash with the S3 fields
}

const (
	BlobStoreFile  = "file"
	BlobStoreS3    = "s3"
	BlobStoreGroup = "group"
	BlobStoreAzure = "azure"
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
	if bs.Type == BlobStoreGroup {
		if bs.Group == nil {
			return "", "", fmt.Errorf("blob store %q: a group needs its members", bs.Name)
		}
		sc.GroupConfig = bs.Group
	}
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
	if bs.Type == BlobStoreAzure {
		if bs.Azure == nil {
			return "", "", fmt.Errorf("blob store %q: an azure store needs its azure configuration", bs.Name)
		}
		c := *bs.Azure
		for _, f := range azureSecrets(&c) {
			if *f.v, err = s.seal(*f.v); err != nil {
				return "", "", fmt.Errorf("blob store %q: %s: %w", bs.Name, f.name, err)
			}
		}
		sc.Azure = &c
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
		if typ == BlobStoreAzure {
			if err := raiseCompat(ctx, tx.q, schemaAzure); err != nil {
				return fmt.Errorf("blob store %q: %w", bs.Name, err)
			}
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

// UpdateBlobStoreGroup replaces a group's members and fill policy, keeping
// the rest of its stored configuration (the soft quota), or returns
// ErrNotFound when there is no group of that name.
func (d *DB) UpdateBlobStoreGroup(ctx context.Context, name string, cfg blob.GroupConfig) error {
	g, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	var gm map[string]json.RawMessage
	if err := json.Unmarshal(g, &gm); err != nil {
		return err
	}
	return d.Tx(ctx, func(tx *Tx) error {
		var config string
		err := tx.q.QueryRowContext(ctx, `SELECT config FROM blob_stores WHERE name = ? AND type = ?`, name, BlobStoreGroup).Scan(&config)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(config), &m); err != nil {
			return fmt.Errorf("blob store %q: decode stored configuration: %w", name, err)
		}
		if m == nil {
			m = map[string]json.RawMessage{}
		}
		for k, v := range gm {
			m[k] = v
		}
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `UPDATE blob_stores SET config = ? WHERE name = ?`, string(b), name)
		return err
	})
}

// ConvertBlobStoreToGroup turns the file or S3 store name into a group of
// that name in one transaction: the store's row is renamed member, with its
// path set to memberPath and the rest kept, and a group row named name, at the
// store's former path, is created with cfg. ErrNotFound if there is no such
// store, ErrExists if member is taken.
func (d *DB) ConvertBlobStoreToGroup(ctx context.Context, name, member, memberPath string, cfg blob.GroupConfig) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return d.Tx(ctx, func(tx *Tx) error {
		var path, typ string
		err := tx.q.QueryRowContext(ctx, `SELECT path, type FROM blob_stores WHERE name = ?`, name).Scan(&path, &typ)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if typ == BlobStoreGroup {
			return fmt.Errorf("blob store %q is a group already", name)
		}
		var one int
		err = tx.q.QueryRowContext(ctx, `SELECT 1 FROM blob_stores WHERE name = ?`, member).Scan(&one)
		if err == nil {
			return ErrExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `UPDATE blob_stores SET name = ?, path = ? WHERE name = ?`, member, memberPath, name); err != nil {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `INSERT INTO blob_stores(`+blobStoreCols+`) VALUES(?,?,?,?,?)`,
			name, path, BlobStoreGroup, string(b), toMs(time.Now()))
		return err
	})
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
	if bs.Type == BlobStoreGroup {
		bs.Group = &blob.GroupConfig{}
		if err := json.Unmarshal([]byte(config), bs.Group); err != nil {
			return BlobStore{}, fmt.Errorf("blob store %q: decode stored group: %w", bs.Name, err)
		}
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
	if bs.Type == BlobStoreAzure {
		var sc struct {
			Azure *blob.AzureConfig `json:"azure"`
		}
		if err := json.Unmarshal([]byte(config), &sc); err != nil || sc.Azure == nil {
			return BlobStore{}, fmt.Errorf("blob store %q: decode stored azure configuration: %v", bs.Name, err)
		}
		for _, f := range azureSecrets(sc.Azure) {
			if *f.v, err = d.box.open(*f.v); err != nil {
				return BlobStore{}, fmt.Errorf("blob store %q: %s: %w", bs.Name, f.name, err)
			}
		}
		bs.Azure = sc.Azure
	}
	return bs, nil
}

// schemaAzure is the first schema that opens azure blob stores. An older
// build would open one as an empty directory: its repositories would read
// nothing and their uploads would stay on the node's disk.
const schemaAzure = 7

// raiseCompat makes the database refuse builds below schema v from now on
// (schema_version.compat), for a stored value they would misread, and is
// refused while a live node of such a build is registered. A change that
// raises compat only once someone uses it leaves the upgrade to v rolling.
func raiseCompat(ctx context.Context, q querier, v int) error {
	var id string
	var s int
	err := q.QueryRowContext(ctx, `SELECT id, schema FROM nodes WHERE schema < ? AND heartbeat_at > `+dbNow+` - ? ORDER BY id LIMIT 1`,
		v, NodeTTL.Milliseconds()).Scan(&id, &s)
	if err == nil {
		return fmt.Errorf("node %s runs a build of schema %d, which cannot use it: upgrade every node first", id, s)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = q.ExecContext(ctx, `UPDATE schema_version SET compat = ? WHERE compat < ?`, v, v)
	return err
}

// azureSecrets are the sealed fields of an azure configuration.
func azureSecrets(c *blob.AzureConfig) []struct {
	name string
	v    *string
} {
	return []struct {
		name string
		v    *string
	}{{"accountKey", &c.AccountKey}, {"sasToken", &c.SASToken}, {"clientSecret", &c.ClientSecret}}
}

// UpdateBlobStoreAzure replaces an azure store's configuration (its
// credentials: the caller keeps the location), keeping the rest of its stored
// configuration (the soft quota), or returns ErrNotFound when there is no
// azure store of that name.
func (d *DB) UpdateBlobStoreAzure(ctx context.Context, name string, cfg blob.AzureConfig) error {
	var err error
	for _, f := range azureSecrets(&cfg) {
		if *f.v, err = d.box.seal(*f.v); err != nil {
			return fmt.Errorf("blob store %q: %s: %w", name, f.name, err)
		}
	}
	az, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return d.Tx(ctx, func(tx *Tx) error {
		var config string
		err := tx.q.QueryRowContext(ctx, `SELECT config FROM blob_stores WHERE name = ? AND type = ?`, name, BlobStoreAzure).Scan(&config)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return setAzureConfig(ctx, tx.q, name, config, az)
	})
}

// setAzureConfig writes az as the "azure" key of the stored config.
func setAzureConfig(ctx context.Context, q querier, name, config string, az []byte) error {
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(config), &m); err != nil {
		return fmt.Errorf("blob store %q: decode stored configuration: %w", name, err)
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	m["azure"] = az
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `UPDATE blob_stores SET config = ? WHERE name = ?`, string(b), name)
	return err
}

// rewriteAzureSecrets is rewriteSecrets' pass over the azure stores: it
// returns how many rows it rewrote.
func rewriteAzureSecrets(ctx context.Context, q querier, rewrite func(v *string, changed *bool) error) (int, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, config FROM blob_stores WHERE type = ?`, BlobStoreAzure)
	if err != nil {
		return 0, err
	}
	stores := map[string]string{}
	for rows.Next() {
		var name, js string
		if err := rows.Scan(&name, &js); err != nil {
			rows.Close()
			return 0, err
		}
		stores[name] = js
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for name, js := range stores {
		var sc struct {
			Azure *blob.AzureConfig `json:"azure"`
		}
		if err := json.Unmarshal([]byte(js), &sc); err != nil || sc.Azure == nil {
			return 0, fmt.Errorf("blob store %q: decode stored azure configuration: %v", name, err)
		}
		changed := false
		for _, f := range azureSecrets(sc.Azure) {
			if err := rewrite(f.v, &changed); err != nil {
				return 0, fmt.Errorf("blob store %q: %s: %w", name, f.name, err)
			}
		}
		if !changed {
			continue
		}
		az, err := json.Marshal(sc.Azure)
		if err != nil {
			return 0, err
		}
		if err := setAzureConfig(ctx, q, name, js, az); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}
