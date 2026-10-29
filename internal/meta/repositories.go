package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
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
			if err := insertRepository(ctx, tx.q, def, newGen()); err != nil {
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
	defs, _, err := d.ListRepositoriesGen(ctx)
	return defs, err
}

// ListRepositoriesGen is ListRepositories with each repository's gen, from
// the same query.
func (d *DB) ListRepositoriesGen(ctx context.Context) ([]config.Repository, map[string]uint64, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT name, config, gen FROM repositories ORDER BY name`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []config.Repository
	gens := map[string]uint64{}
	for rows.Next() {
		var name, js string
		var gen int64
		if err := rows.Scan(&name, &js, &gen); err != nil {
			return nil, nil, err
		}
		var def config.Repository
		if err := json.Unmarshal([]byte(js), &def); err != nil {
			return nil, nil, fmt.Errorf("repository %q: decode stored definition: %w", name, err)
		}
		if err := d.box.openRepo(&def); err != nil {
			return nil, nil, err
		}
		out = append(out, def)
		gens[def.Name] = uint64(gen)
	}
	return out, gens, rows.Err()
}

// A repository's gen names its incarnation: a random number set when it is
// created and when its remote changes, never otherwise (0 for a row stored
// before schema 6). A write that read the repository's definition at the
// start of a request checks it inside its transaction (Tx.RepositoryGen),
// so it fails, instead of writing into the new repository, when another
// node deleted and re-created the name meanwhile.
func newGen() int64 { return rand.Int64N(math.MaxInt64) + 1 }

// RepositoryGen returns the named repository's gen; ErrNotFound if there is
// no such repository.
func (d *DB) RepositoryGen(ctx context.Context, name string) (uint64, error) {
	return repositoryGen(ctx, d.sql, name)
}

// RepositoryGen is DB.RepositoryGen inside the transaction.
func (t *Tx) RepositoryGen(ctx context.Context, name string) (uint64, error) {
	return repositoryGen(ctx, t.q, name)
}

func repositoryGen(ctx context.Context, q querier, name string) (uint64, error) {
	var gen int64
	err := q.QueryRowContext(ctx, `SELECT gen FROM repositories WHERE name = ?`, name).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return uint64(gen), err
}

// CreateRepository stores a new definition; ErrExists if the name is taken.
func (d *DB) CreateRepository(ctx context.Context, def config.Repository) error {
	_, err := d.CreateRepositoryGen(ctx, def)
	return err
}

// CreateRepositoryGen is CreateRepository, returning the new repository's gen.
func (d *DB) CreateRepositoryGen(ctx context.Context, def config.Repository) (uint64, error) {
	def, err := d.box.sealRepo(def)
	if err != nil {
		return 0, err
	}
	gen := newGen()
	return uint64(gen), d.Tx(ctx, func(tx *Tx) error {
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
		return insertRepository(ctx, tx.q, def, gen)
	})
}

// UpdateRepository replaces the stored definition named def.Name; ErrNotFound
// if there is none. Format and type are stored as given; callers enforce
// that they do not change.
func (d *DB) UpdateRepository(ctx context.Context, def config.Repository) error {
	_, err := d.UpdateRepositoryGen(ctx, def)
	return err
}

// UpdateRepositoryGen is UpdateRepository, returning the repository's gen:
// a new one when the remote changed.
func (d *DB) UpdateRepositoryGen(ctx context.Context, def config.Repository) (uint64, error) {
	sealed, err := d.box.sealRepo(def)
	if err != nil {
		return 0, err
	}
	js, err := json.Marshal(sealed)
	if err != nil {
		return 0, err
	}
	var gen int64
	err = d.Tx(ctx, func(tx *Tx) error {
		var old string
		err := tx.q.QueryRowContext(ctx, `SELECT config, gen FROM repositories WHERE name = ?`, def.Name).Scan(&old, &gen)
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
		// Misses recorded against the old remote say nothing about the new
		// one, and what was fetched from it is another incarnation's.
		if prev.Remote != def.Remote {
			if _, err := tx.q.ExecContext(ctx, `DELETE FROM negative_cache WHERE repo = ?`, def.Name); err != nil {
				return err
			}
			gen = newGen()
			if _, err := tx.q.ExecContext(ctx, `UPDATE repositories SET gen = ? WHERE name = ?`, gen, def.Name); err != nil {
				return err
			}
		}
		return nil
	})
	return uint64(gen), err
}

// DeleteRepository removes the definition and, in the same transaction, every
// assets, packages, dist_tags, negative_cache, component_tags and
// blob_store_moves row of that repository (plan 9 Decision 4) and every role privilege whose target is exactly
// "repo:<name>", so a repository created later under the name does not get
// them (QA N-25). It returns the ids of the roles it changed, ordered by id.
// Blobs are left for the GC. ErrNotFound if there is no such definition.
func (d *DB) DeleteRepository(ctx context.Context, name string) ([]string, error) {
	var roles []string
	err := d.Tx(ctx, func(tx *Tx) error {
		res, err := tx.q.ExecContext(ctx, `DELETE FROM repositories WHERE name = ?`, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if err := purgeRepository(ctx, tx.q, name); err != nil {
			return err
		}
		roles, err = dropRepoPrivileges(ctx, tx.q, name)
		return err
	})
	if err != nil {
		return nil, err
	}
	return roles, nil
}

// dropRepoPrivileges removes the privileges on exactly "repo:<name>" from
// every stored role and returns the ids of the roles it changed.
func dropRepoPrivileges(ctx context.Context, q querier, name string) ([]string, error) {
	list, err := listRoles(ctx, q)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, r := range list {
		n := len(r.Privileges)
		kept := slices.DeleteFunc(r.Privileges, func(p Privilege) bool { return p.Target == "repo:"+name })
		if len(kept) == n {
			continue
		}
		privs, err := json.Marshal(kept)
		if err != nil {
			return nil, err
		}
		if _, err := q.ExecContext(ctx, `UPDATE roles SET privileges = ?, updated_at = ? WHERE id = ?`,
			string(privs), toMs(time.Now()), r.ID); err != nil {
			return nil, err
		}
		changed = append(changed, r.ID)
	}
	return changed, nil
}

func purgeRepository(ctx context.Context, q querier, name string) error {
	for _, table := range []string{"assets", "packages", "dist_tags", "negative_cache", "component_tags", "blob_store_moves", "vuln_findings", "firewall_quarantine"} {
		if _, err := q.ExecContext(ctx, `DELETE FROM `+table+` WHERE repo = ?`, name); err != nil {
			return fmt.Errorf("purge %s of %q: %w", table, name, err)
		}
	}
	return nil
}

func insertRepository(ctx context.Context, q querier, def config.Repository, gen int64) error {
	js, err := json.Marshal(def)
	if err != nil {
		return err
	}
	now := toMs(time.Now())
	_, err = q.ExecContext(ctx, `INSERT INTO repositories(name, format, type, config, created_at, updated_at, gen) VALUES(?,?,?,?,?,?,?)`,
		def.Name, def.Format, def.Type, string(js), now, now, gen)
	return err
}
