package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CleanupPolicy is one stored cleanup policy (plan 11 Decision 5). Config is
// the criteria as JSON; internal/cleanup owns its shape and validation.
type CleanupPolicy struct {
	Name, Format, Config string
	Created, Updated     time.Time
}

// PolicyInUseError refuses to delete a policy that repositories still use.
type PolicyInUseError struct {
	Policy string
	Repos  []string // sorted
}

func (e *PolicyInUseError) Error() string {
	return fmt.Sprintf("cleanup policy %q is used by repositories: %s", e.Policy, strings.Join(e.Repos, ", "))
}

// MaxCleanupPolicies is how many policies may exist: CreateCleanupPolicy
// refuses more (ErrPolicyLimit), so ListCleanupPolicies always returns all
// of them. A variable so tests can lower it.
var MaxCleanupPolicies = 1000

// ErrPolicyLimit is returned when MaxCleanupPolicies policies exist already.
var ErrPolicyLimit = errors.New("too many cleanup policies")

// ListCleanupPolicies returns every stored policy, ordered by name.
func (d *DB) ListCleanupPolicies(ctx context.Context) ([]CleanupPolicy, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT name, format, config, created_at, updated_at FROM cleanup_policies
ORDER BY name LIMIT ?`, MaxCleanupPolicies)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CleanupPolicy
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetCleanupPolicy returns the named policy, or ErrNotFound.
func (d *DB) GetCleanupPolicy(ctx context.Context, name string) (CleanupPolicy, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT name, format, config, created_at, updated_at FROM cleanup_policies WHERE name = ?`, name)
	return scanPolicy(row)
}

func scanPolicy(row scanner) (CleanupPolicy, error) {
	var p CleanupPolicy
	var created, updated int64
	err := row.Scan(&p.Name, &p.Format, &p.Config, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return CleanupPolicy{}, ErrNotFound
	}
	if err != nil {
		return CleanupPolicy{}, err
	}
	p.Created, p.Updated = fromMs(created), fromMs(updated)
	return p, nil
}

// CreateCleanupPolicy stores a new policy; ErrExists if the name is taken.
func (d *DB) CreateCleanupPolicy(ctx context.Context, p CleanupPolicy) error {
	return d.Tx(ctx, func(tx *Tx) error {
		var exists, n int
		if err := tx.q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN name = ? THEN 1 ELSE 0 END), 0) FROM cleanup_policies`, p.Name).
			Scan(&n, &exists); err != nil {
			return err
		}
		switch {
		case exists > 0:
			return ErrExists
		case n >= MaxCleanupPolicies:
			return ErrPolicyLimit
		}
		now := toMs(time.Now())
		_, err := tx.q.ExecContext(ctx, `INSERT INTO cleanup_policies(name, format, config, created_at, updated_at)
VALUES(?,?,?,?,?)`, p.Name, p.Format, p.Config, now, now)
		return err
	})
}

// UpdateCleanupPolicy replaces the format and config of the named policy;
// ErrNotFound if there is none. created_at is kept.
func (d *DB) UpdateCleanupPolicy(ctx context.Context, p CleanupPolicy) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE cleanup_policies SET format = ?, config = ?, updated_at = ? WHERE name = ?`,
		p.Format, p.Config, toMs(time.Now()), p.Name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteCleanupPolicy removes the named policy unless a stored repository
// definition names it (a *PolicyInUseError listing them), in one transaction.
// ErrNotFound if there is no such policy.
func (d *DB) DeleteCleanupPolicy(ctx context.Context, name string) error {
	return d.Tx(ctx, func(tx *Tx) error {
		users, err := reposUsingPolicy(ctx, tx.q, name)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			return &PolicyInUseError{Policy: name, Repos: users}
		}
		res, err := tx.q.ExecContext(ctx, `DELETE FROM cleanup_policies WHERE name = ?`, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RepositoriesUsingPolicy returns the names of the stored repositories whose
// definition names the policy, sorted.
func (d *DB) RepositoriesUsingPolicy(ctx context.Context, name string) ([]string, error) {
	return reposUsingPolicy(ctx, d.sql, name)
}

func reposUsingPolicy(ctx context.Context, q querier, name string) ([]string, error) {
	rows, err := q.QueryContext(ctx, pg(q, `SELECT name FROM repositories WHERE json_extract(config, '$.cleanupPolicy') = ? ORDER BY name`,
		`SELECT name FROM repositories WHERE config::json ->> 'cleanupPolicy' = ? ORDER BY name`), name)
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

// ScanAsset is what cleanup evaluation reads of an asset.
type ScanAsset struct {
	Path           string
	Size           int64
	Created        time.Time
	Modified       time.Time // last_modified
	LastDownloaded time.Time // zero if never downloaded
	BlobSHA256     string
}

// ScanAssets returns up to limit assets of repo with a path greater than
// after ("" for the start), in path order: one page of a bounded scan.
func (d *DB) ScanAssets(ctx context.Context, repo, after string, limit int) ([]ScanAsset, error) {
	return d.scanAssets(ctx, `path > ?`, repo, after, limit)
}

// ScanAssetsFrom is ScanAssets starting at from itself: the assets with a
// path greater than or equal to from.
func (d *DB) ScanAssetsFrom(ctx context.Context, repo, from string, limit int) ([]ScanAsset, error) {
	return d.scanAssets(ctx, `path >= ?`, repo, from, limit)
}

func (d *DB) scanAssets(ctx context.Context, cond, repo, bound string, limit int) ([]ScanAsset, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT path, size, created_at, last_modified, last_downloaded, blob_sha256 FROM assets
WHERE repo = ? AND `+cond+` ORDER BY path LIMIT ?`, repo, bound, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ScanAsset, 0, limit)
	for rows.Next() {
		var a ScanAsset
		var created, modified, lastDl int64
		if err := rows.Scan(&a.Path, &a.Size, &created, &modified, &lastDl, &a.BlobSHA256); err != nil {
			return nil, err
		}
		a.Created, a.Modified, a.LastDownloaded = fromMs(created), fromMs(modified), fromMs(lastDl)
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetSetting returns the value of a settings key; ErrNotFound if unset.
func (d *DB) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetSetting stores a settings key; an empty value deletes it.
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	if value == "" {
		_, err := d.sql.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
		return err
	}
	_, err := d.sql.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// DeleteSettings deletes every settings key that starts with prefix.
func (d *DB) DeleteSettings(ctx context.Context, prefix string) error {
	q, args := `DELETE FROM settings WHERE key >= ?`, []any{prefix}
	if upper, ok := prefixUpper(prefix); ok {
		q += ` AND key < ?`
		args = append(args, upper)
	}
	_, err := d.sql.ExecContext(ctx, q, args...)
	return err
}
