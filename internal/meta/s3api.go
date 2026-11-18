package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// The S3-compatible API's queries (internal/s3api) and its access keys.
//
// S3 access keys: the credentials of the S3-compatible API (internal/s3api).
// AWS Signature V4 needs the secret itself, so it cannot be hashed: it is
// sealed with the secret key like the other stored secrets (secrets.go).
// A key is a settings row, s3KeyPrefix + its id, holding JSON: no new table,
// so no schema change. An older build never reads the rows.

const s3KeyPrefix = "s3.accessKey."

// MaxS3KeysPerOwner bounds the access keys of one principal (AWS allows two
// per IAM user; a few more let a key be rotated per client).
const MaxS3KeysPerOwner = 10

// S3Key is an S3 access key. Owner is a principal's owner key
// (auth.Principal.Owner: "alice", "ldap:alice"). Secret is empty in lists.
type S3Key struct {
	ID, Owner, Secret string
	Created, LastUsed time.Time
}

type s3KeyRow struct {
	Owner    string `json:"owner"`
	Secret   string `json:"secret"`
	Created  int64  `json:"created"`
	LastUsed int64  `json:"lastUsed,omitempty"`
}

func (r s3KeyRow) key(id string) S3Key {
	return S3Key{ID: id, Owner: r.Owner, Secret: r.Secret, Created: fromMs(r.Created), LastUsed: fromMs(r.LastUsed)}
}

// ErrTooManyS3Keys refuses a key past MaxS3KeysPerOwner.
var ErrTooManyS3Keys = fmt.Errorf("a user may have at most %d S3 access keys", MaxS3KeysPerOwner)

// CreateS3Key stores k with its secret sealed. ErrExists when the id is
// taken, ErrTooManyS3Keys when the owner has MaxS3KeysPerOwner already.
func (d *DB) CreateS3Key(ctx context.Context, k S3Key) error {
	secret, err := d.box.seal(k.Secret)
	if err != nil {
		return err
	}
	b, err := json.Marshal(s3KeyRow{Owner: k.Owner, Secret: secret, Created: toMs(k.Created)})
	if err != nil {
		return err
	}
	return d.Tx(ctx, func(tx *Tx) error {
		// ponytail: two creates at once may both pass the bound by one; it
		// only keeps a user from piling up keys.
		var n int
		if err := tx.q.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key LIKE ? AND value::jsonb ->> 'owner' = ?`,
			s3KeyPrefix+"%", k.Owner).Scan(&n); err != nil {
			return err
		}
		if n >= MaxS3KeysPerOwner {
			return ErrTooManyS3Keys
		}
		res, err := tx.q.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO NOTHING`, s3KeyPrefix+k.ID, string(b))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrExists
		}
		return nil
	})
}

// GetS3Key returns the key with its secret opened; ErrNotFound if there is
// none.
func (d *DB) GetS3Key(ctx context.Context, id string) (S3Key, error) {
	var v string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, s3KeyPrefix+id).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return S3Key{}, ErrNotFound
	}
	if err != nil {
		return S3Key{}, err
	}
	var row s3KeyRow
	if err := json.Unmarshal([]byte(v), &row); err != nil {
		return S3Key{}, fmt.Errorf("S3 access key %q: %w", id, err)
	}
	if row.Secret, err = d.box.open(row.Secret); err != nil {
		return S3Key{}, fmt.Errorf("S3 access key %q: %w", id, err)
	}
	return row.key(id), nil
}

// ListS3Keys returns the keys of owner ("" for every owner) without their
// secrets, ordered by owner and id.
func (d *DB) ListS3Keys(ctx context.Context, owner string) ([]S3Key, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT key, value FROM settings WHERE key LIKE ? AND (? = '' OR value::jsonb ->> 'owner' = ?)
ORDER BY value::jsonb ->> 'owner', key`, s3KeyPrefix+"%", owner, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []S3Key{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		var row s3KeyRow
		if err := json.Unmarshal([]byte(v), &row); err != nil {
			return nil, fmt.Errorf("setting %q: %w", k, err)
		}
		row.Secret = ""
		out = append(out, row.key(k[len(s3KeyPrefix):]))
	}
	return out, rows.Err()
}

// DeleteS3Key removes a key; ErrNotFound if there is none.
func (d *DB) DeleteS3Key(ctx context.Context, id string) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, s3KeyPrefix+id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// deleteS3KeysOf removes the keys of owner in the transaction q.
func deleteS3KeysOf(ctx context.Context, q querier, owner string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM settings WHERE key LIKE ? AND value::jsonb ->> 'owner' = ?`, s3KeyPrefix+"%", owner)
	return err
}

// TouchS3Key records a successful use of the key, at most once per
// TokenTouchInterval.
func (d *DB) TouchS3Key(ctx context.Context, id string) {
	now := time.Now()
	_, err := d.sql.ExecContext(ctx, `UPDATE settings SET value = (value::jsonb || jsonb_build_object('lastUsed', ?::bigint))::text
WHERE key = ? AND COALESCE((value::jsonb ->> 'lastUsed')::bigint, 0) < ?`, toMs(now), s3KeyPrefix+id, toMs(now.Add(-TokenTouchInterval)))
	if err != nil {
		slog.Warn("could not record S3 access key use", "err", err)
	}
}

// rewriteS3KeySecrets is rewriteSecrets for the S3 access keys' secrets.
func rewriteS3KeySecrets(ctx context.Context, q querier, rewrite func(v *string, changed *bool) error) (int, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM settings WHERE key LIKE ?`, s3KeyPrefix+"%")
	if err != nil {
		return 0, err
	}
	keys := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return 0, err
		}
		keys[k] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for k, v := range keys {
		var m map[string]json.RawMessage // the other fields stay as stored
		var secret string
		if json.Unmarshal([]byte(v), &m) != nil || json.Unmarshal(m["secret"], &secret) != nil {
			return 0, fmt.Errorf("setting %q: decode the stored S3 access key", k)
		}
		changed := false
		if err := rewrite(&secret, &changed); err != nil {
			return 0, fmt.Errorf("setting %q: %w", k, err)
		}
		if !changed {
			continue
		}
		m["secret"], _ = json.Marshal(secret)
		b, _ := json.Marshal(m)
		if _, err := q.ExecContext(ctx, `UPDATE settings SET value = ? WHERE key = ?`, string(b), k); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// AssetsFrom returns up to limit assets of repo in path order from bound:
// paths at or after it with inclusive, else after it (the S3 listings).
func (d *DB) AssetsFrom(ctx context.Context, repo, bound string, inclusive bool, limit int) ([]Asset, error) {
	op := ">"
	if inclusive {
		op = ">="
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT `+assetSelect+` FROM assets WHERE repo = ? AND path `+op+` ? ORDER BY path LIMIT ?`, repo, bound, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RepositoriesCreated returns when each repository of format and type was
// created (S3's bucket creation dates).
func (d *DB) RepositoriesCreated(ctx context.Context, format, typ string) (map[string]time.Time, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT name, created_at FROM repositories WHERE format = ? AND type = ?`, format, typ)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var name string
		var ms int64
		if err := rows.Scan(&name, &ms); err != nil {
			return nil, err
		}
		out[name] = fromMs(ms)
	}
	return out, rows.Err()
}
