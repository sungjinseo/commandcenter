package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"commandcenter/internal/config"
)

// importedKey marks that config.yaml's repositories were imported (plan 9
// Decision 1). After that, the database is the only source of definitions.
const importedKey = "repositories.imported"

// RepositoriesImported reports whether ImportRepositories has run.
func (d *DB) RepositoriesImported(ctx context.Context) (bool, error) {
	var v string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, importedKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ImportRepositories inserts defs and marks the import done, in one
// transaction, unless an import already happened. It reports whether it
// imported. An empty defs still marks the import done.
func (d *DB) ImportRepositories(ctx context.Context, defs []config.Repository) (bool, error) {
	imported := false
	err := d.Tx(ctx, func(tx *Tx) error {
		var v string
		err := tx.q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, importedKey).Scan(&v)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for _, def := range defs {
			def, err := d.box.sealRepo(def)
			if err != nil {
				return err
			}
			if err := insertRepository(ctx, tx.q, def); err != nil {
				return err
			}
		}
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?)`,
			importedKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		imported = true
		return nil
	})
	return imported && err == nil, err
}

// ListRepositories returns every stored definition, ordered by name.
func (d *DB) ListRepositories(ctx context.Context) ([]config.Repository, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT name, config FROM repositories ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []config.Repository
	for rows.Next() {
		var name, js string
		if err := rows.Scan(&name, &js); err != nil {
			return nil, err
		}
		var def config.Repository
		if err := json.Unmarshal([]byte(js), &def); err != nil {
			return nil, fmt.Errorf("repository %q: decode stored definition: %w", name, err)
		}
		if err := d.box.openRepo(&def); err != nil {
			return nil, err
		}
		out = append(out, def)
	}
	return out, rows.Err()
}

// CreateRepository stores a new definition; ErrExists if the name is taken.
func (d *DB) CreateRepository(ctx context.Context, def config.Repository) error {
	def, err := d.box.sealRepo(def)
	if err != nil {
		return err
	}
	return d.Tx(ctx, func(tx *Tx) error {
		var one int
		err := tx.q.QueryRowContext(ctx, `SELECT 1 FROM repositories WHERE name = ?`, def.Name).Scan(&one)
		if err == nil {
			return ErrExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// A write that was in flight while DeleteRepository purged this name
		// can land after the purge and leave orphan rows. Purge again here,
		// so a re-created repository never inherits the old one's content.
		// (ImportRepositories does not purge: rows written before the import
		// belong to the imported repositories.)
		if err := purgeRepository(ctx, tx.q, def.Name); err != nil {
			return err
		}
		return insertRepository(ctx, tx.q, def)
	})
}

// UpdateRepository replaces the stored definition named def.Name; ErrNotFound
// if there is none. Format and type are stored as given; callers enforce
// that they do not change.
func (d *DB) UpdateRepository(ctx context.Context, def config.Repository) error {
	sealed, err := d.box.sealRepo(def)
	if err != nil {
		return err
	}
	js, err := json.Marshal(sealed)
	if err != nil {
		return err
	}
	return d.Tx(ctx, func(tx *Tx) error {
		var old string
		err := tx.q.QueryRowContext(ctx, `SELECT config FROM repositories WHERE name = ?`, def.Name).Scan(&old)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var prev config.Repository
		if err := json.Unmarshal([]byte(old), &prev); err != nil {
			return fmt.Errorf("repository %q: decode stored definition: %w", def.Name, err)
		}
		if _, err := tx.q.ExecContext(ctx, `UPDATE repositories SET format = ?, type = ?, config = ?, updated_at = ? WHERE name = ?`,
			def.Format, def.Type, string(js), toMs(time.Now()), def.Name); err != nil {
			return err
		}
		// Misses recorded against the old remote say nothing about the new one.
		if prev.Remote != def.Remote {
			if _, err := tx.q.ExecContext(ctx, `DELETE FROM negative_cache WHERE repo = ?`, def.Name); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteRepository removes the definition and, in the same transaction, every
// assets, packages, dist_tags and negative_cache row of that repository
// (plan 9 Decision 4). Blobs are left for the GC. ErrNotFound if there is no
// such definition.
func (d *DB) DeleteRepository(ctx context.Context, name string) error {
	return d.Tx(ctx, func(tx *Tx) error {
		res, err := tx.q.ExecContext(ctx, `DELETE FROM repositories WHERE name = ?`, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return purgeRepository(ctx, tx.q, name)
	})
}

func purgeRepository(ctx context.Context, q querier, name string) error {
	for _, table := range []string{"assets", "packages", "dist_tags", "negative_cache"} {
		if _, err := q.ExecContext(ctx, `DELETE FROM `+table+` WHERE repo = ?`, name); err != nil {
			return fmt.Errorf("purge %s of %q: %w", table, name, err)
		}
	}
	return nil
}

func insertRepository(ctx context.Context, q querier, def config.Repository) error {
	js, err := json.Marshal(def)
	if err != nil {
		return err
	}
	now := toMs(time.Now())
	_, err = q.ExecContext(ctx, `INSERT INTO repositories(name, format, type, config, created_at, updated_at) VALUES(?,?,?,?,?,?)`,
		def.Name, def.Format, def.Type, string(js), now, now)
	return err
}
