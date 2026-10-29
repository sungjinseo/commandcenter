package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// User tokens: each principal (owner key: "alice", "ldap:alice",
// "crowd:alice", ...) may hold one. Its name code is stored as is (the
// Basic user name that finds the row), its pass code only as a SHA-256 hash.

// SettingUserTokens is the settings key of the user token configuration.
const SettingUserTokens = "userTokens"

// UserTokens is the user token configuration. Required only applies while
// Enabled: repository endpoints then refuse a user name and password.
// ExpirationDays 0 means tokens never expire; otherwise a token expires that
// many days after it was created, whenever the setting changed.
type UserTokens struct {
	Enabled        bool `json:"enabled"`
	Required       bool `json:"required"`
	ExpirationDays int  `json:"expirationDays"`
}

// MaxUserTokenExpirationDays bounds UserTokens.ExpirationDays.
const MaxUserTokenExpirationDays = 3650

// ValidateUserTokens checks c.
func ValidateUserTokens(c UserTokens) error {
	if c.ExpirationDays < 0 || c.ExpirationDays > MaxUserTokenExpirationDays {
		return fmt.Errorf("userTokens expirationDays must be between 0 (never) and %d", MaxUserTokenExpirationDays)
	}
	return nil
}

// Expires returns when a token created at created expires under c, or the
// zero time when it never does.
func (c UserTokens) Expires(created time.Time) time.Time {
	if c.ExpirationDays <= 0 {
		return time.Time{}
	}
	return created.Add(time.Duration(c.ExpirationDays) * 24 * time.Hour)
}

// UserToken is a stored user token without its pass code hash.
type UserToken struct {
	Owner, NameCode   string
	Created, LastUsed time.Time
}

const userTokenCols = `owner, name_code, created_at, last_used_at`

func scanUserToken(row scanner, extra ...any) (UserToken, error) {
	var t UserToken
	var created, last int64
	err := row.Scan(append([]any{&t.Owner, &t.NameCode, &created, &last}, extra...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return UserToken{}, ErrNotFound
	}
	if err != nil {
		return UserToken{}, err
	}
	t.Created, t.LastUsed = fromMs(created), fromMs(last)
	return t, nil
}

// PutUserToken stores owner's token, replacing the one it had, in one
// transaction. ErrExists when another owner's token has the name code.
func (d *DB) PutUserToken(ctx context.Context, owner, nameCode, hash string, created time.Time) error {
	return d.Tx(ctx, func(tx *Tx) error {
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM user_tokens WHERE owner = ?`, owner); err != nil {
			return err
		}
		var one int
		err := tx.q.QueryRowContext(ctx, `SELECT 1 FROM user_tokens WHERE name_code = ?`, nameCode).Scan(&one)
		if err == nil {
			return ErrExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `INSERT INTO user_tokens(owner, name_code, hash, created_at) VALUES(?,?,?,?)`,
			owner, nameCode, hash, toMs(created))
		return err
	})
}

// UserTokenByNameCode returns the token with the name code and its pass
// code hash; ErrNotFound if there is none.
func (d *DB) UserTokenByNameCode(ctx context.Context, nameCode string) (UserToken, string, error) {
	var hash string
	t, err := scanUserToken(d.sql.QueryRowContext(ctx, `SELECT `+userTokenCols+`, hash FROM user_tokens WHERE name_code = ?`, nameCode), &hash)
	return t, hash, err
}

// GetUserToken returns owner's token; ErrNotFound if it has none.
func (d *DB) GetUserToken(ctx context.Context, owner string) (UserToken, error) {
	return scanUserToken(d.sql.QueryRowContext(ctx, `SELECT `+userTokenCols+` FROM user_tokens WHERE owner = ?`, owner))
}

// ListUserTokens returns every user token, ordered by owner (one per
// principal, so the list is as long as the user list at most).
func (d *DB) ListUserTokens(ctx context.Context) ([]UserToken, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+userTokenCols+` FROM user_tokens ORDER BY owner`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserToken{}
	for rows.Next() {
		t, err := scanUserToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteUserToken removes owner's token; ErrNotFound if it has none.
func (d *DB) DeleteUserToken(ctx context.Context, owner string) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM user_tokens WHERE owner = ?`, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUserTokensBefore removes the tokens created before cutoff and
// returns how many it removed.
func (d *DB) DeleteUserTokensBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM user_tokens WHERE created_at < ?`, toMs(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteAllUserTokens removes every user token and returns how many it removed.
func (d *DB) DeleteAllUserTokens(ctx context.Context) (int64, error) {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM user_tokens`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TouchUserToken records a successful use of owner's token, at most once
// per TokenTouchInterval.
func (d *DB) TouchUserToken(ctx context.Context, owner string) {
	now := time.Now()
	_, err := d.sql.ExecContext(ctx, `UPDATE user_tokens SET last_used_at = ? WHERE owner = ? AND last_used_at < ?`,
		toMs(now), owner, toMs(now.Add(-TokenTouchInterval)))
	if err != nil {
		slog.Warn("could not record user token use", "err", err)
	}
}
