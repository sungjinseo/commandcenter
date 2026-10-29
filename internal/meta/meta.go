// Package meta stores repository metadata in SQLite or PostgreSQL.
package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

// Asset is one stored file in a repository.
type Asset struct {
	Repo, Path         string
	BlobSHA256         string
	Size               int64
	ContentType        string
	SHA1, MD5, SHA512  string
	CreatedAt          time.Time
	LastModified       time.Time
	RemoteETag         string // proxy: remote ETag; group: member fingerprint
	RemoteLastModified string
	CheckedAt          time.Time // proxy: last time the remote was consulted
	Downloads          int64     // successful GETs, flushed in batches (metrics.Recorder)
	LastDownloaded     time.Time // zero if never downloaded
}

type DB struct {
	sql  *pool
	path string // the SQLite file; "" on PostgreSQL
	box  sealer // seals the stored secrets; see SetSecretKey
}

// querier is implemented by *pool and txConn (dialect.go).
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) row
	postgres() bool
}

const schema = `
CREATE TABLE IF NOT EXISTS assets(
  repo TEXT NOT NULL, path TEXT NOT NULL,
  blob_sha256 TEXT NOT NULL, size INTEGER NOT NULL DEFAULT 0,
  content_type TEXT NOT NULL DEFAULT '',
  sha1 TEXT NOT NULL DEFAULT '', md5 TEXT NOT NULL DEFAULT '', sha512 TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT 0, last_modified INTEGER NOT NULL DEFAULT 0,
  remote_etag TEXT NOT NULL DEFAULT '', remote_last_modified TEXT NOT NULL DEFAULT '',
  checked_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(repo, path)) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS assets_blob ON assets(blob_sha256);
CREATE TABLE IF NOT EXISTS packages(
  repo TEXT NOT NULL, format TEXT NOT NULL, name TEXT NOT NULL, version TEXT NOT NULL,
  attrs TEXT NOT NULL DEFAULT '{}', yanked INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(repo, name, version)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS dist_tags(
  repo TEXT NOT NULL, name TEXT NOT NULL, tag TEXT NOT NULL, version TEXT NOT NULL,
  PRIMARY KEY(repo, name, tag)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS negative_cache(
  repo TEXT NOT NULL, path TEXT NOT NULL, expires_at INTEGER NOT NULL,
  PRIMARY KEY(repo, path)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS tokens(
  id TEXT PRIMARY KEY, user TEXT NOT NULL, hash TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL, last_used_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS repositories(
  name TEXT PRIMARY KEY, format TEXT NOT NULL, type TEXT NOT NULL, config TEXT NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS blob_stores(name TEXT PRIMARY KEY, path TEXT NOT NULL, created_at INTEGER NOT NULL,
  type TEXT NOT NULL DEFAULT 'file', config TEXT NOT NULL DEFAULT '{}');
CREATE TABLE IF NOT EXISTS users(
  id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', email TEXT NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT 'local',
  status TEXT NOT NULL DEFAULT 'active', roles TEXT NOT NULL DEFAULT '[]',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS roles(
  id TEXT PRIMARY KEY, description TEXT NOT NULL DEFAULT '', privileges TEXT NOT NULL DEFAULT '[]',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS request_stats(
  hour INTEGER NOT NULL, repo TEXT NOT NULL, requests INTEGER NOT NULL DEFAULT 0, bytes INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(hour, repo)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS events(
  id INTEGER PRIMARY KEY, at INTEGER NOT NULL, kind TEXT NOT NULL, actor TEXT NOT NULL DEFAULT '',
  repo TEXT NOT NULL DEFAULT '', subject TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS cleanup_policies(
  name TEXT PRIMARY KEY, format TEXT NOT NULL, config TEXT NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS tasks(
  id TEXT PRIMARY KEY, name TEXT NOT NULL, type TEXT NOT NULL, schedule TEXT NOT NULL,
  params TEXT NOT NULL DEFAULT '{}', enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  last_run INTEGER NOT NULL DEFAULT 0, last_result TEXT NOT NULL DEFAULT '',
  last_duration_ms INTEGER NOT NULL DEFAULT 0, last_message TEXT NOT NULL DEFAULT '',
  last_log TEXT NOT NULL DEFAULT '', next_run INTEGER NOT NULL DEFAULT 0,
  last_removed INTEGER NOT NULL DEFAULT 0, last_bytes INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS webhooks(
  id TEXT PRIMARY KEY, name TEXT NOT NULL, url TEXT NOT NULL, secret TEXT NOT NULL DEFAULT '',
  events TEXT NOT NULL DEFAULT '[]', repo TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS webhook_deliveries(
  id TEXT PRIMARY KEY, webhook_id TEXT NOT NULL, event TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
  attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
  next_at INTEGER NOT NULL DEFAULT 0, response_code INTEGER NOT NULL DEFAULT 0, payload TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS webhook_deliveries_hook ON webhook_deliveries(webhook_id, created_at);
CREATE TABLE IF NOT EXISTS routing_rules(
  name TEXT PRIMARY KEY, description TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL,
  matchers TEXT NOT NULL DEFAULT '[]', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS content_selectors(
  name TEXT PRIMARY KEY, description TEXT NOT NULL DEFAULT '', expression TEXT NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS user_tokens(
  owner TEXT PRIMARY KEY, name_code TEXT NOT NULL UNIQUE, hash TEXT NOT NULL,
  created_at INTEGER NOT NULL, last_used_at INTEGER NOT NULL DEFAULT 0);
`

// schemaVersion is the database layout this build reads and writes, kept in
// SQLite's PRAGMA user_version and in PostgreSQL's schema_version table
// (which starts at 1). Open refuses a database with a higher
// version: an older build must never run on data a newer one wrote (QA
// N-30). Bump it with every change an older build would mishandle (a table
// or column it would not maintain, a stored value it would misread). Builds
// before version 1 left it at 0 and do not check it.
//
//	1: users, roles and settings in the database, named blob stores,
//	   secrets sealed with the secret key (secrets.go).
//	2: blob_stores.type and config (S3 stores): an older build would open an
//	   S3 store's path as a directory and find nothing in it.
//	3: webhooks and webhook_deliveries. An older build would not misread
//	   them, but upgrade runs no DDL on a database already at its own
//	   version, so the tables are only created by a bump.
//	3: routing_rules and content_selectors: an older build would send a
//	   proxy's blocked paths to the remote and drop "selector:" privileges.
//	4: user_tokens: an older build would not create the table, and would
//	   keep a deleted user's user token row.
const schemaVersion = 4

// Open opens the metadata database and creates or upgrades its tables:
// PostgreSQL when target is a postgres:// or postgresql:// URL (see
// IsPostgresURL), else the SQLite file at target, created if needed. Errors
// never hold the URL.
func Open(target string) (*DB, error) {
	var p *pool
	var name string
	if IsPostgresURL(target) {
		db, n, err := openPostgres(target)
		if err != nil {
			return nil, err
		}
		p, name = &pool{db: db, pg: true}, n
	} else {
		dsn := "file:" + target +
			"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-4000)&_txlock=immediate"
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(4)
		p, name = &pool{db: db}, target
	}
	if err := upgrade(p, name); err != nil {
		p.db.Close()
		return nil, err
	}
	d := &DB{sql: p}
	if !p.pg {
		d.path = target
	}
	return d, nil
}

// SetMaxOpenConns sets the size of the connection pool (default 4 for
// SQLite, 10 for PostgreSQL). Call it before d is used.
func (d *DB) SetMaxOpenConns(n int) {
	d.sql.db.SetMaxOpenConns(n)
	if d.sql.pg {
		d.sql.db.SetMaxIdleConns(n)
	}
}

// Driver names the database d runs on: "sqlite" or "postgresql".
func (d *DB) Driver() string {
	if d.sql.pg {
		return "postgresql"
	}
	return "sqlite"
}

// SizeBytes returns the size of the database: of the SQLite file (without
// its WAL), or pg_database_size on PostgreSQL.
func (d *DB) SizeBytes(ctx context.Context) (int64, error) {
	if !d.sql.pg {
		st, err := os.Stat(d.path)
		if err != nil {
			return 0, err
		}
		return st.Size(), nil
	}
	var n int64
	err := d.sql.QueryRowContext(ctx, `SELECT pg_database_size(current_database())`).Scan(&n)
	return n, err
}

// upgrade creates the tables, adds the columns older databases lack and
// stamps schemaVersion in one transaction, so a crash leaves the database
// either as it was or migrated and stamped. A database a newer build wrote
// is refused before anything is written, and one at schemaVersion is left
// alone: no DDL runs, so a PostgreSQL role with DML rights alone opens it.
// On PostgreSQL the transaction holds the write lock, so two processes
// starting at once upgrade in turn.
func upgrade(p *pool, name string) error {
	ctx := context.Background()
	tx, err := p.begin(ctx, true)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := txConn{tx: tx, pg: p.pg}
	version, err := schemaVersionOf(ctx, q)
	if err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("%s was written by a newer version of Command Center (database schema %d; this version supports up to %d) "+
			"and this version would misread it: run the newer version again, or restore the backup taken before the upgrade "+
			"together with this version (operations guide, \"업그레이드\" and \"백업과 복원\")", name, version, schemaVersion)
	}
	if version == schemaVersion {
		return tx.Commit()
	}
	if _, err := q.ExecContext(ctx, pg(q, schema, pgSchema)); err != nil {
		return ddlError(err, name)
	}
	if !p.pg { // pgSchema has every column
		if err := migrate(ctx, q); err != nil {
			return err
		}
	}
	if err := stampSchemaVersion(ctx, q, schemaVersion); err != nil {
		return err
	}
	return tx.Commit()
}

// schemaVersionOf reads the stamped schema version: 0 for a new database or
// one from before the stamp. It runs no DDL (pgSchema creates the table).
func schemaVersionOf(ctx context.Context, q querier) (int, error) {
	var v int
	if !q.postgres() {
		err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
		return v, err
	}
	var stamped bool
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('schema_version') IS NOT NULL`).Scan(&stamped); err != nil || !stamped {
		return 0, err
	}
	err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v)
	return v, err
}

func stampSchemaVersion(ctx context.Context, q querier, v int) error {
	if !q.postgres() { // PRAGMA takes no bind parameters
		_, err := q.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v))
		return err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM schema_version`); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `INSERT INTO schema_version(version) VALUES(?)`, v)
	return err
}

// migrate adds columns that older SQLite databases lack. Each step checks
// PRAGMA table_info first, so it is safe on every start.
func migrate(ctx context.Context, db querier) error {
	for _, c := range []struct{ table, col, ddl string }{
		{"assets", "downloads", "downloads INTEGER NOT NULL DEFAULT 0"},
		{"assets", "last_downloaded", "last_downloaded INTEGER NOT NULL DEFAULT 0"},
		{"tasks", "notify_email", "notify_email TEXT NOT NULL DEFAULT '[]'"},
		{"blob_stores", "type", "type TEXT NOT NULL DEFAULT 'file'"},
		{"blob_stores", "config", "config TEXT NOT NULL DEFAULT '{}'"},
		{"request_stats", "errors", "errors INTEGER NOT NULL DEFAULT 0"},
		{"request_stats", "bytes_in", "bytes_in INTEGER NOT NULL DEFAULT 0"},
		{"request_stats", "ms", "ms INTEGER NOT NULL DEFAULT 0"},
		{"request_stats", "max_ms", "max_ms INTEGER NOT NULL DEFAULT 0"},
		{"request_stats", "upstream", "upstream INTEGER NOT NULL DEFAULT 0"},
		{"request_stats", "upstream_failures", "upstream_failures INTEGER NOT NULL DEFAULT 0"},
		{"request_stats", "upstream_ms", "upstream_ms INTEGER NOT NULL DEFAULT 0"},
		{"request_stats", "upstream_max_ms", "upstream_max_ms INTEGER NOT NULL DEFAULT 0"},
		{"request_stats", "cache_hits", "cache_hits INTEGER NOT NULL DEFAULT 0"},
		{"request_stats", "cache_misses", "cache_misses INTEGER NOT NULL DEFAULT 0"},
	} {
		has, err := hasColumn(ctx, db, c.table, c.col)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := db.ExecContext(ctx, "ALTER TABLE "+c.table+" ADD COLUMN "+c.ddl); err != nil {
			return err
		}
	}
	return nil
}

func hasColumn(ctx context.Context, db querier, table, col string) (bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (d *DB) Close() error { return d.sql.db.Close() }

const assetCols = `repo, path, blob_sha256, size, content_type, sha1, md5, sha512,
  created_at, last_modified, remote_etag, remote_last_modified, checked_at`

// assetSelect is assetCols plus the counters, which only the metrics flush writes.
const assetSelect = assetCols + `, downloads, last_downloaded`

func (d *DB) GetAsset(ctx context.Context, repo, path string) (Asset, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+assetSelect+` FROM assets WHERE repo = ? AND path = ?`, repo, path)
	return scanAsset(row)
}

// PutAsset inserts or replaces an asset; an existing CreatedAt is kept.
func (d *DB) PutAsset(ctx context.Context, a Asset) error { return putAsset(ctx, d.sql, a) }

func putAsset(ctx context.Context, q querier, a Asset) error {
	_, err := q.ExecContext(ctx, `INSERT INTO assets(`+assetCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(repo, path) DO UPDATE SET
  blob_sha256 = excluded.blob_sha256, size = excluded.size, content_type = excluded.content_type,
  sha1 = excluded.sha1, md5 = excluded.md5, sha512 = excluded.sha512,
  last_modified = excluded.last_modified, remote_etag = excluded.remote_etag,
  remote_last_modified = excluded.remote_last_modified, checked_at = excluded.checked_at`,
		a.Repo, a.Path, a.BlobSHA256, a.Size, a.ContentType, a.SHA1, a.MD5, a.SHA512,
		toMs(a.CreatedAt), toMs(a.LastModified), a.RemoteETag, a.RemoteLastModified, toMs(a.CheckedAt))
	return err
}

func (d *DB) DeleteAsset(ctx context.Context, repo, path string) error {
	return deleteAsset(ctx, d.sql, repo, path)
}

func deleteAsset(ctx context.Context, q querier, repo, path string) error {
	res, err := q.ExecContext(ctx, `DELETE FROM assets WHERE repo = ? AND path = ?`, repo, path)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListAssets returns the assets of repo whose path starts with prefix, ordered by path.
func (d *DB) ListAssets(ctx context.Context, repo, prefix string) ([]Asset, error) {
	return listAssets(ctx, d.sql, repo, prefix)
}

func listAssets(ctx context.Context, qr querier, repo, prefix string) ([]Asset, error) {
	q := `SELECT ` + assetSelect + ` FROM assets WHERE repo = ? AND path >= ?`
	args := []any{repo, prefix}
	if upper, ok := prefixUpper(prefix); ok {
		q += ` AND path < ?`
		args = append(args, upper)
	}
	rows, err := qr.QueryContext(ctx, q+` ORDER BY path`, args...)
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

// WalkAssetPaths calls fn with the path of each asset of repo whose path
// starts with prefix, in path order. Unlike ListAssets it holds one row at a
// time, so memory does not grow with the number of assets; fn runs while the
// query holds a connection.
func (d *DB) WalkAssetPaths(ctx context.Context, repo, prefix string, fn func(path string)) error {
	q := `SELECT path FROM assets WHERE repo = ? AND path >= ?`
	args := []any{repo, prefix}
	if upper, ok := prefixUpper(prefix); ok {
		q += ` AND path < ?`
		args = append(args, upper)
	}
	rows, err := d.sql.QueryContext(ctx, q+` ORDER BY path`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return err
		}
		fn(p)
	}
	return rows.Err()
}

func (d *DB) MarkChecked(ctx context.Context, repo, path string, t time.Time) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE assets SET checked_at = ? WHERE repo = ? AND path = ?`, toMs(t), repo, path)
	return err
}

func (d *DB) NegativeHit(ctx context.Context, repo, path string, now time.Time) (bool, error) {
	var exp int64
	err := d.sql.QueryRowContext(ctx, `SELECT expires_at FROM negative_cache WHERE repo = ? AND path = ?`, repo, path).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return exp > toMs(now), nil
}

func (d *DB) PutNegative(ctx context.Context, repo, path string, expires time.Time) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO negative_cache(repo, path, expires_at) VALUES(?,?,?)
ON CONFLICT(repo, path) DO UPDATE SET expires_at = excluded.expires_at`, repo, path, toMs(expires))
	return err
}

func (d *DB) ClearNegative(ctx context.Context, repo, path string) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM negative_cache WHERE repo = ? AND path = ?`, repo, path)
	return err
}

// PruneNegative deletes the negative-cache entries that can no longer be a
// hit at now (see NegativeHit) and returns how many rows went.
func (d *DB) PruneNegative(ctx context.Context, now time.Time) (int64, error) {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM negative_cache WHERE expires_at <= ?`, toMs(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) BlobInUse(ctx context.Context, sha string) (bool, error) {
	var one int
	err := d.sql.QueryRowContext(ctx, `SELECT 1 FROM assets WHERE blob_sha256 = ? LIMIT 1`, sha).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (d *DB) CreateToken(ctx context.Context, id, user, hash string, created time.Time) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO tokens(id, "user", hash, created_at) VALUES(?,?,?,?)`, id, user, hash, toMs(created))
	return err
}

// TokenTouchInterval bounds how often a token's last use is written.
var TokenTouchInterval = time.Minute

// TokenUser returns the user owning the token hash and when the token was last
// recorded as used (zero if never). It never writes: callers
// record a use with TouchToken once the token has actually authenticated.
func (d *DB) TokenUser(ctx context.Context, hash string) (string, time.Time, error) {
	var user string
	var last int64
	err := d.sql.QueryRowContext(ctx, `SELECT "user", last_used_at FROM tokens WHERE hash = ?`, hash).Scan(&user, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, ErrNotFound
	}
	if err != nil {
		return "", time.Time{}, err
	}
	return user, fromMs(last), nil
}

// TouchToken records a successful use of the token. The conditional UPDATE
// writes at most once per TokenTouchInterval, so busy clients do not turn
// every read into a database write.
func (d *DB) TouchToken(ctx context.Context, hash string) {
	now := time.Now()
	_, err := d.sql.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE hash = ? AND last_used_at < ?`,
		toMs(now), hash, toMs(now.Add(-TokenTouchInterval)))
	if err != nil {
		slog.Warn("could not record token use", "err", err)
	}
}

type scanner interface{ Scan(dest ...any) error }

func scanAsset(row scanner) (Asset, error) {
	var a Asset
	var created, modified, checked, lastDl int64
	err := row.Scan(&a.Repo, &a.Path, &a.BlobSHA256, &a.Size, &a.ContentType, &a.SHA1, &a.MD5, &a.SHA512,
		&created, &modified, &a.RemoteETag, &a.RemoteLastModified, &checked, &a.Downloads, &lastDl)
	if errors.Is(err, sql.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	if err != nil {
		return Asset{}, err
	}
	a.CreatedAt, a.LastModified, a.CheckedAt = fromMs(created), fromMs(modified), fromMs(checked)
	a.LastDownloaded = fromMs(lastDl)
	return a, nil
}

func toMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMs(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// prefixUpper returns the smallest string greater than every string with the prefix.
func prefixUpper(p string) (string, bool) {
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}

// Ping checks that the database answers a trivial query.
func (d *DB) Ping(ctx context.Context) error {
	var one int
	return d.sql.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}
