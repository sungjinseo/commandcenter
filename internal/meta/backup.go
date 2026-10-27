package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Online backup support (plan 14 Decision 3): a consistent copy of the live
// database, and the read-only queries a backup, its verify and its pool sweep
// run against that copy.

// Checkpoint runs a passive WAL checkpoint: it copies what it can from the
// WAL into the database file without waiting for readers or blocking writers.
func (d *DB) Checkpoint(ctx context.Context) error {
	var busy, logFrames, done int
	return d.sql.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &done)
}

// VacuumInto writes a consistent snapshot of the database to path, which must
// not exist, with VACUUM INTO: it reads in one read transaction, so writers
// continue meanwhile. The copy is switched to the rollback journal so that it
// is one self-contained file that opens read-only without -wal or -shm files.
func (d *DB) VacuumInto(ctx context.Context, path string) error {
	if _, err := d.sql.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return err
	}
	snap, err := sql.Open("sqlite", fileURI(path)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer snap.Close()
	var mode string
	if err := snap.QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&mode); err != nil {
		return fmt.Errorf("set the snapshot's journal mode: %w", err)
	}
	return snap.Close()
}

// OpenReadOnly opens an existing database file (a backup snapshot) read-only.
// It neither creates nor migrates anything.
func OpenReadOnly(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", path, ErrNotFound)
		}
		return nil, err
	}
	db, err := sql.Open("sqlite", fileURI(path)+"?mode=ro&_pragma=busy_timeout(5000)&_pragma=cache_size(-512)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{sql: &pool{db: db}, path: path}, nil
}

// CountAssets returns the number of asset rows.
func (d *DB) CountAssets(ctx context.Context) (int64, error) {
	var n int64
	err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM assets`).Scan(&n)
	return n, err
}

// EachAssetBlob calls fn for every asset row's (repo, blob, size), ordered by
// blob then repository, so the rows of one blob arrive together. It streams:
// nothing but the current row is held in memory.
func (d *DB) EachAssetBlob(ctx context.Context, fn func(repo, sha string, size int64) error) error {
	rows, err := d.sql.QueryContext(ctx, `SELECT repo, blob_sha256, size FROM assets ORDER BY blob_sha256, repo`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var repo, sha string
		var size int64
		if err := rows.Scan(&repo, &sha, &size); err != nil {
			return err
		}
		if err := fn(repo, sha, size); err != nil {
			return err
		}
	}
	return rows.Err()
}

// BlobRepos returns the repositories that have an asset on blob sha.
func (d *DB) BlobRepos(ctx context.Context, sha string) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT DISTINCT repo FROM assets WHERE blob_sha256 = ?`, sha)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QuickCheck runs PRAGMA quick_check and returns "ok" or the first problem.
func (d *DB) QuickCheck(ctx context.Context) (string, error) {
	var res string
	err := d.sql.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&res)
	return res, err
}

// fileURI turns a file path into an SQLite file: URI, percent-escaping the
// characters ('?', '#', '%', spaces) that a URI would otherwise interpret.
func fileURI(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // a Windows drive path: file:///C:/...
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// EachStoreBlob calls fn, in blob order, once per distinct blob that the
// assets of repos reference (notIn: of every repository but repos), with the
// largest size recorded for it. It streams over the assets_blob index.
func (d *DB) EachStoreBlob(ctx context.Context, repos []string, notIn bool, fn func(sha string, size int64) error) error {
	if len(repos) == 0 && !notIn {
		return nil
	}
	where := ""
	args := make([]any, 0, len(repos))
	if len(repos) > 0 {
		op := "IN"
		if notIn {
			op = "NOT IN"
		}
		where = ` WHERE repo ` + op + ` (` + strings.TrimSuffix(strings.Repeat("?,", len(repos)), ",") + `)`
		for _, r := range repos {
			args = append(args, r)
		}
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT blob_sha256, MAX(size) FROM assets`+where+` GROUP BY blob_sha256 ORDER BY blob_sha256`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sha string
		var size int64
		if err := rows.Scan(&sha, &size); err != nil {
			return err
		}
		if err := fn(sha, size); err != nil {
			return err
		}
	}
	return rows.Err()
}
