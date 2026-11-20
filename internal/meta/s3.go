package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// The S3 API (format/s3): access keys, the S3 headers of stored objects and
// multipart uploads in progress.
//
// An access key's secret is sealed like the other stored secrets (it must
// be read back: a SigV4 signature is an HMAC under it). An object's S3 row
// follows its asset: the foreign key drops it with the asset, and it
// counts only while its blob_sha256 is the asset's (another writer may
// have replaced the asset without it).

// schemaS3 is the first schema that knows the s3 format.
const schemaS3 = 10

// S3Key is a stored S3 access key without its secret.
type S3Key struct {
	AccessKeyID, Owner, Name   string
	Created, Expires, LastUsed time.Time // Expires zero: never
}

const s3KeyCols = `access_key_id, owner, name, created_at, expires_at, last_used_at`

func scanS3Key(row scanner, extra ...any) (S3Key, error) {
	var k S3Key
	var created, expires, last int64
	err := row.Scan(append([]any{&k.AccessKeyID, &k.Owner, &k.Name, &created, &expires, &last}, extra...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return S3Key{}, ErrNotFound
	}
	if err != nil {
		return S3Key{}, err
	}
	k.Created, k.Expires, k.LastUsed = fromMs(created), fromMs(expires), fromMs(last)
	return k, nil
}

// PutS3Key stores a new key with its secret sealed; ErrExists when the id
// is taken.
func (d *DB) PutS3Key(ctx context.Context, k S3Key, secret string) error {
	sealed, err := d.box.seal(secret)
	if err != nil {
		return err
	}
	return d.Tx(ctx, func(tx *Tx) error {
		var one int
		err := tx.q.QueryRowContext(ctx, `SELECT 1 FROM s3_keys WHERE access_key_id = ?`, k.AccessKeyID).Scan(&one)
		if err == nil {
			return ErrExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `INSERT INTO s3_keys(access_key_id, owner, name, secret, created_at, expires_at) VALUES(?,?,?,?,?,?)`,
			k.AccessKeyID, k.Owner, k.Name, sealed, toMs(k.Created), toMs(k.Expires))
		return err
	})
}

// S3KeyByID returns the key and its secret; ErrNotFound if there is none.
func (d *DB) S3KeyByID(ctx context.Context, id string) (S3Key, string, error) {
	if !utf8.ValidString(id) {
		return S3Key{}, "", ErrNotFound
	}
	var sealed string
	k, err := scanS3Key(d.sql.QueryRowContext(ctx, `SELECT `+s3KeyCols+`, secret FROM s3_keys WHERE access_key_id = ?`, id), &sealed)
	if err != nil {
		return S3Key{}, "", err
	}
	secret, err := d.box.open(sealed)
	if err != nil {
		return S3Key{}, "", fmt.Errorf("s3 key %s: %w", id, err)
	}
	return k, secret, nil
}

// ListS3Keys returns the keys of owner, or every key when owner is "",
// ordered by owner and creation.
func (d *DB) ListS3Keys(ctx context.Context, owner string) ([]S3Key, error) {
	q, args := `SELECT `+s3KeyCols+` FROM s3_keys ORDER BY owner, created_at, access_key_id`, []any{}
	if owner != "" {
		q, args = `SELECT `+s3KeyCols+` FROM s3_keys WHERE owner = ? ORDER BY created_at, access_key_id`, []any{owner}
	}
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []S3Key{}
	for rows.Next() {
		k, err := scanS3Key(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteS3Key removes a key and returns it; ErrNotFound if there is none.
func (d *DB) DeleteS3Key(ctx context.Context, id string) (S3Key, error) {
	var k S3Key
	err := d.Tx(ctx, func(tx *Tx) error {
		var err error
		k, err = scanS3Key(tx.q.QueryRowContext(ctx, `SELECT `+s3KeyCols+` FROM s3_keys WHERE access_key_id = ?`, id))
		if err != nil {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `DELETE FROM s3_keys WHERE access_key_id = ?`, id)
		return err
	})
	return k, err
}

// DeleteS3KeysOf removes every key of owner and returns how many.
func (d *DB) DeleteS3KeysOf(ctx context.Context, owner string) (int64, error) {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM s3_keys WHERE owner = ?`, owner)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TouchS3Key records a successful use; a failure is ignored (best effort,
// like TouchToken).
func (d *DB) TouchS3Key(ctx context.Context, id string, at time.Time) {
	d.sql.ExecContext(ctx, `UPDATE s3_keys SET last_used_at = ? WHERE access_key_id = ?`, toMs(at), id)
}

// rewriteS3KeySecrets passes the sealed secret of every key through
// rewrite (RotateSecretKey) and returns how many rows changed.
func rewriteS3KeySecrets(ctx context.Context, q querier, rewrite func(v *string, changed *bool) error) (int, error) {
	rows, err := q.QueryContext(ctx, `SELECT access_key_id, secret FROM s3_keys`)
	if err != nil {
		return 0, err
	}
	keys := map[string]string{}
	for rows.Next() {
		var id, secret string
		if err := rows.Scan(&id, &secret); err != nil {
			rows.Close()
			return 0, err
		}
		keys[id] = secret
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for id, secret := range keys {
		changed := false
		if err := rewrite(&secret, &changed); err != nil {
			return n, fmt.Errorf("s3 key %s: %w", id, err)
		}
		if !changed {
			continue
		}
		if _, err := q.ExecContext(ctx, `UPDATE s3_keys SET secret = ? WHERE access_key_id = ?`, secret, id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// S3ObjectMeta is what S3 keeps of an object beside its asset: the ETag
// of a multipart object ("" for the asset's MD5) and the headers the
// upload set (x-amz-meta-*, Cache-Control, Content-Disposition,
// Content-Encoding, Content-Language, Expires), keyed in lower case.
type S3ObjectMeta struct {
	BlobSHA256 string
	ETag       string
	Headers    map[string]string
}

// PutS3ObjectMeta stores the S3 row of repo/path; its asset must exist.
func (t *Tx) PutS3ObjectMeta(ctx context.Context, repo, path string, m S3ObjectMeta) error {
	js, err := json.Marshal(nonNil(m.Headers))
	if err != nil {
		return err
	}
	return execRow(ctx, t.q, `INSERT INTO s3_object_meta(repo, path, blob_sha256, etag, headers) VALUES(?,?,?,?,?)
ON CONFLICT(repo, path) DO UPDATE SET blob_sha256 = excluded.blob_sha256, etag = excluded.etag, headers = excluded.headers`,
		[]any{repo, path, m.BlobSHA256, m.ETag, string(js)})
}

// GetS3ObjectMeta returns the S3 row of repo/path; ErrNotFound if there is
// none.
func (d *DB) GetS3ObjectMeta(ctx context.Context, repo, path string) (S3ObjectMeta, error) {
	if !utf8.ValidString(path) {
		return S3ObjectMeta{}, ErrNotFound
	}
	var m S3ObjectMeta
	var js string
	err := d.sql.QueryRowContext(ctx, `SELECT blob_sha256, etag, headers FROM s3_object_meta WHERE repo = ? AND path = ?`, repo, path).
		Scan(&m.BlobSHA256, &m.ETag, &js)
	if errors.Is(err, sql.ErrNoRows) {
		return S3ObjectMeta{}, ErrNotFound
	}
	if err != nil {
		return S3ObjectMeta{}, err
	}
	if err := json.Unmarshal([]byte(js), &m.Headers); err != nil {
		return S3ObjectMeta{}, err
	}
	return m, nil
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// S3Upload is a multipart upload in progress. Its parts are files in the
// blob store's tmp directory (format/s3).
type S3Upload struct {
	ID, Repo, Path, Initiator, ContentType string
	Headers                                map[string]string
	Started, Updated                       time.Time
}

const s3UploadCols = `upload_id, repo, path, initiator, content_type, headers, started_at, updated_at`

func scanS3Upload(row scanner) (S3Upload, error) {
	var u S3Upload
	var js string
	var started, updated int64
	err := row.Scan(&u.ID, &u.Repo, &u.Path, &u.Initiator, &u.ContentType, &js, &started, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return S3Upload{}, ErrNotFound
	}
	if err != nil {
		return S3Upload{}, err
	}
	if err := json.Unmarshal([]byte(js), &u.Headers); err != nil {
		return S3Upload{}, err
	}
	u.Started, u.Updated = fromMs(started), fromMs(updated)
	return u, nil
}

// PutS3Upload stores a new upload.
func (d *DB) PutS3Upload(ctx context.Context, u S3Upload) error {
	js, err := json.Marshal(nonNil(u.Headers))
	if err != nil {
		return err
	}
	return d.Tx(ctx, func(tx *Tx) error {
		return execRow(ctx, tx.q, `INSERT INTO s3_uploads(`+s3UploadCols+`) VALUES(?,?,?,?,?,?,?,?)`,
			[]any{u.ID, u.Repo, u.Path, u.Initiator, u.ContentType, string(js), toMs(u.Started), toMs(u.Updated)})
	})
}

// GetS3Upload returns an upload; ErrNotFound if there is none.
func (d *DB) GetS3Upload(ctx context.Context, id string) (S3Upload, error) {
	if !utf8.ValidString(id) {
		return S3Upload{}, ErrNotFound
	}
	return scanS3Upload(d.sql.QueryRowContext(ctx, `SELECT `+s3UploadCols+` FROM s3_uploads WHERE upload_id = ?`, id))
}

// TouchS3Upload records that a part arrived, so the cleanup of stale
// uploads leaves it alone.
func (d *DB) TouchS3Upload(ctx context.Context, id string, at time.Time) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE s3_uploads SET updated_at = ? WHERE upload_id = ?`, toMs(at), id)
	return err
}

// DeleteS3Upload removes an upload; ErrNotFound if there is none.
func (d *DB) DeleteS3Upload(ctx context.Context, id string) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM s3_uploads WHERE upload_id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListS3Uploads returns the uploads of repo whose key starts with prefix,
// ordered by key and id, after the (key, id) pair afterKey/afterID, at
// most limit.
func (d *DB) ListS3Uploads(ctx context.Context, repo, prefix, afterKey, afterID string, limit int) ([]S3Upload, error) {
	if !utf8.ValidString(prefix) || !utf8.ValidString(afterKey) || !utf8.ValidString(afterID) {
		return []S3Upload{}, nil
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT `+s3UploadCols+` FROM s3_uploads
WHERE repo = ? AND starts_with(path, ?) AND (path > ? OR path = ? AND upload_id > ?)
ORDER BY path, upload_id LIMIT ?`, repo, prefix, afterKey, afterKey, afterID, limit)
	if err != nil {
		return nil, err
	}
	return collectS3Uploads(rows)
}

// StaleS3Uploads returns the uploads no part arrived for since before.
func (d *DB) StaleS3Uploads(ctx context.Context, before time.Time) ([]S3Upload, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+s3UploadCols+` FROM s3_uploads WHERE updated_at < ? ORDER BY upload_id`, toMs(before))
	if err != nil {
		return nil, err
	}
	return collectS3Uploads(rows)
}

func collectS3Uploads(rows *sql.Rows) ([]S3Upload, error) {
	defer rows.Close()
	out := []S3Upload{}
	for rows.Next() {
		u, err := scanS3Upload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListObjects returns at most limit assets of repo whose path starts with
// prefix and is at or after from (after it when exclusive), in byte order.
func (d *DB) ListObjects(ctx context.Context, repo, prefix, from string, exclusive bool, limit int) ([]Asset, error) {
	if !utf8.ValidString(prefix) || !utf8.ValidString(from) {
		return []Asset{}, nil
	}
	if from < prefix {
		from, exclusive = prefix, false
	}
	op := ">="
	if exclusive {
		op = ">"
	}
	q := `SELECT ` + assetSelect + ` FROM assets WHERE repo = ? AND path ` + op + ` ? AND starts_with(path, ?)`
	args := []any{repo, from, prefix}
	if end, ok := prefixEnd(prefix); ok {
		q += ` AND path < ?`
		args = append(args, end)
	}
	q += ` ORDER BY path LIMIT ?`
	args = append(args, limit)
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Asset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// prefixEnd is the least string above every string that starts with
// prefix, in byte (code point) order: prefix with its last code point
// raised by one, after dropping trailing U+10FFFF. ok is false when there
// is none (an empty prefix, or one of only U+10FFFF).
func prefixEnd(prefix string) (string, bool) {
	r := []rune(prefix)
	for len(r) > 0 {
		last := r[len(r)-1]
		r = r[:len(r)-1]
		if last == utf8.MaxRune {
			continue
		}
		next := last + 1
		if next >= 0xD800 && next <= 0xDFFF { // surrogates are not UTF-8
			next = 0xE000
		}
		return string(append(r, next)), true
	}
	return "", false
}

// RepoHasAssets reports whether repo holds any asset.
func (d *DB) RepoHasAssets(ctx context.Context, repo string) (bool, error) {
	var one int
	err := d.sql.QueryRowContext(ctx, `SELECT 1 FROM assets WHERE repo = ? LIMIT 1`, repo).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// execRow runs a write that returns no row; text the database cannot hold
// is ErrInvalidText.
func execRow(ctx context.Context, q querier, query string, args []any) error {
	if !storable(args) {
		return ErrInvalidText
	}
	_, err := q.ExecContext(ctx, query, args...)
	return err
}
