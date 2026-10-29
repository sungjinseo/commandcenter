package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// The package's SQL is written for SQLite, with ? placeholders. On
// PostgreSQL the same statements run through pool and txConn, which rebind
// the placeholders to $1, $2, ...; the few whose syntax differs pick their
// PostgreSQL form with pg. What the code relies on keeps its SQLite meaning:
//
//   - One writer at a time (SQLite's BEGIN IMMEDIATE): every PostgreSQL
//     write, a transaction or a single statement, first takes the advisory
//     lock writeLock, held until it commits or rolls back. Reads take no
//     lock and run beside a writer, as with WAL.
//   - A write waits at most 5 s for the lock, as busy_timeout(5000) waits
//     for SQLite's (lock_timeout; a URL may set another). Neither error is
//     mapped: both surface as errors, as SQLITE_BUSY always has. Constraint
//     errors are not mapped either: ErrExists comes from a SELECT made under
//     the write lock, which the advisory lock keeps exact.
//   - Text compares, sorts and prefix-matches byte-wise (SQLite's BINARY):
//     every text column is COLLATE "C", whatever the database's locale. Case
//     folding is ASCII only, as SQLite's lower(), LIKE and NOCASE: lower()
//     under the "C" collation.
//   - JSON is stored as text, so documents read back byte for byte;
//     json_extract and json_each become ->> and json_array_elements_text.
//   - Integers, booleans (0/1) and times (Unix ms) are BIGINT, as SQLite's
//     64-bit INTEGER; events.id is an identity column where SQLite has its
//     rowid alias. No query sorts a nullable column, so NULL ordering does
//     not arise.
//   - PostgreSQL text cannot hold what SQLite's can: invalid UTF-8 and NUL
//     bytes (older builds stored such Maven names). No stored row can match
//     such a value, so a query with one returns no rows without asking the
//     server, and a write fails with ErrInvalidText.

// writeLock takes the advisory lock of the one writer (key "ccmeta").
const writeLock = `SELECT pg_advisory_xact_lock(109278688277601)`

// pool is the database handle; on PostgreSQL it rebinds and locks writes.
type pool struct {
	db *sql.DB
	pg bool
}

// txConn is a transaction on a pool.
type txConn struct {
	tx *sql.Tx
	pg bool
}

func (p *pool) postgres() bool  { return p.pg }
func (t txConn) postgres() bool { return t.pg }

// pg returns the PostgreSQL form of a statement on PostgreSQL, else the
// SQLite form.
func pg(q querier, sqlite, postgres string) string {
	if q.postgres() {
		return postgres
	}
	return sqlite
}

func (p *pool) bind(q string) string {
	if p.pg {
		return rebind(q)
	}
	return q
}

func (t txConn) bind(q string) string {
	if t.pg {
		return rebind(q)
	}
	return q
}

// ErrInvalidText refuses a write of text PostgreSQL cannot store: not valid
// UTF-8, or holding a NUL byte.
var ErrInvalidText = errors.New("text that is not valid UTF-8 or holds a NUL byte cannot be stored")

// storable reports whether PostgreSQL can take every string in args.
func storable(args []any) bool {
	for _, a := range args {
		if s, ok := a.(string); ok && (!utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0) {
			return false
		}
	}
	return true
}

// noRows is a query that returns no rows, for a value no row can match.
const noRows = `SELECT 1 WHERE false`

// row is what QueryRowContext returns: a *sql.Row, or the error of a query
// that did not run.
type row struct {
	r   *sql.Row
	err error
}

func (r row) Scan(dest ...any) error {
	if r.r == nil {
		return r.err
	}
	return r.r.Scan(dest...)
}

func (p *pool) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if p.pg && !storable(args) {
		return p.db.QueryContext(ctx, noRows)
	}
	return p.db.QueryContext(ctx, p.bind(q), args...)
}

func (p *pool) QueryRowContext(ctx context.Context, q string, args ...any) row {
	if p.pg && !storable(args) {
		return row{err: sql.ErrNoRows}
	}
	return row{r: p.db.QueryRowContext(ctx, p.bind(q), args...)}
}

// ExecContext runs a write. On PostgreSQL it is a transaction of its own
// that takes the write lock first, as SQLite's implicit one does.
func (p *pool) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if !p.pg {
		return p.db.ExecContext(ctx, q, args...)
	}
	if !storable(args) {
		return nil, ErrInvalidText
	}
	tx, err := p.begin(ctx, true)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, rebind(q), args...)
	if err != nil {
		return nil, err
	}
	return res, tx.Commit()
}

// begin starts a write transaction, which holds the write lock, or a read
// transaction, which sees one snapshot (SQLite's deferred BEGIN in WAL mode;
// REPEATABLE READ on PostgreSQL) and takes no lock.
func (p *pool) begin(ctx context.Context, write bool) (*sql.Tx, error) {
	if !p.pg {
		if write {
			return p.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE (the DSN's _txlock)
		}
		// ReadOnly makes the driver issue a plain (deferred) BEGIN, which does
		// not take the write lock.
		return p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	}
	if !write {
		return p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, writeLock); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func (t txConn) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if t.pg && !storable(args) {
		return nil, ErrInvalidText
	}
	return t.tx.ExecContext(ctx, t.bind(q), args...)
}

func (t txConn) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if t.pg && !storable(args) {
		return t.tx.QueryContext(ctx, noRows)
	}
	return t.tx.QueryContext(ctx, t.bind(q), args...)
}

func (t txConn) QueryRowContext(ctx context.Context, q string, args ...any) row {
	if t.pg && !storable(args) {
		return row{err: sql.ErrNoRows}
	}
	return row{r: t.tx.QueryRowContext(ctx, t.bind(q), args...)}
}

// rebind turns the ? placeholders outside quotes into $1, $2, ...
func rebind(q string) string {
	var b strings.Builder
	n := 0
	var quote byte
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '?':
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// IsPostgresURL reports whether target names a PostgreSQL database rather
// than an SQLite file.
func IsPostgresURL(target string) bool {
	return strings.HasPrefix(target, "postgres://") || strings.HasPrefix(target, "postgresql://")
}

// errBadURL never holds the URL: it may hold a password.
var errBadURL = errors.New("the database URL does not parse: use postgres://user:password@host:5432/database?sslmode=... " +
	"(percent-encode special characters in the user name and password)")

// openPostgres connects to the database at url. The returned name says
// which database it is in messages: never the URL, which may hold a password.
func openPostgres(url string) (*sql.DB, string, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, "", errBadURL
	}
	if _, ok := cfg.RuntimeParams["lock_timeout"]; !ok {
		cfg.RuntimeParams["lock_timeout"] = "5000" // ms, as SQLite's busy_timeout(5000)
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(defaultPostgresConns)
	db.SetMaxIdleConns(defaultPostgresConns)
	return db, "PostgreSQL database " + strconv.Quote(cfg.Database) + " on " + cfg.Host, nil
}

// ddlError explains a CREATE or ALTER that PostgreSQL refused for lack of
// privilege: creating and upgrading the tables takes the owner of the
// database and its tables, while an up-to-date database needs DML only.
func ddlError(err error, name string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" { // insufficient_privilege
		return fmt.Errorf("creating or upgrading the tables of %s: %w (the server's role must own the database and its tables, "+
			"CREATE DATABASE ... OWNER <role>; or run this version once as their owner)", name, err)
	}
	return err
}

// defaultPostgresConns is the PostgreSQL pool size unless SetMaxOpenConns
// sets another; SQLite's is 4.
const defaultPostgresConns = 10

// pgSchema is schema for PostgreSQL, at schemaVersion with every column, so
// it needs no migrate step. A later column goes into both: into schema plus
// migrate for SQLite, and here as ALTER TABLE ... ADD COLUMN IF NOT EXISTS.
const pgSchema = `
CREATE TABLE IF NOT EXISTS assets(
  repo TEXT COLLATE "C" NOT NULL, path TEXT COLLATE "C" NOT NULL,
  blob_sha256 TEXT COLLATE "C" NOT NULL, size BIGINT NOT NULL DEFAULT 0,
  content_type TEXT COLLATE "C" NOT NULL DEFAULT '',
  sha1 TEXT COLLATE "C" NOT NULL DEFAULT '', md5 TEXT COLLATE "C" NOT NULL DEFAULT '', sha512 TEXT COLLATE "C" NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL DEFAULT 0, last_modified BIGINT NOT NULL DEFAULT 0,
  remote_etag TEXT COLLATE "C" NOT NULL DEFAULT '', remote_last_modified TEXT COLLATE "C" NOT NULL DEFAULT '',
  checked_at BIGINT NOT NULL DEFAULT 0,
  downloads BIGINT NOT NULL DEFAULT 0, last_downloaded BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY(repo, path));
CREATE INDEX IF NOT EXISTS assets_blob ON assets(blob_sha256);
CREATE TABLE IF NOT EXISTS packages(
  repo TEXT COLLATE "C" NOT NULL, format TEXT COLLATE "C" NOT NULL, name TEXT COLLATE "C" NOT NULL, version TEXT COLLATE "C" NOT NULL,
  attrs TEXT COLLATE "C" NOT NULL DEFAULT '{}', yanked BIGINT NOT NULL DEFAULT 0, created_at BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY(repo, name, version));
CREATE TABLE IF NOT EXISTS dist_tags(
  repo TEXT COLLATE "C" NOT NULL, name TEXT COLLATE "C" NOT NULL, tag TEXT COLLATE "C" NOT NULL, version TEXT COLLATE "C" NOT NULL,
  PRIMARY KEY(repo, name, tag));
CREATE TABLE IF NOT EXISTS negative_cache(
  repo TEXT COLLATE "C" NOT NULL, path TEXT COLLATE "C" NOT NULL, expires_at BIGINT NOT NULL,
  PRIMARY KEY(repo, path));
CREATE TABLE IF NOT EXISTS tokens(
  id TEXT COLLATE "C" PRIMARY KEY, "user" TEXT COLLATE "C" NOT NULL, hash TEXT COLLATE "C" NOT NULL UNIQUE,
  created_at BIGINT NOT NULL, last_used_at BIGINT NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS repositories(
  name TEXT COLLATE "C" PRIMARY KEY, format TEXT COLLATE "C" NOT NULL, type TEXT COLLATE "C" NOT NULL, config TEXT COLLATE "C" NOT NULL,
  created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS settings(key TEXT COLLATE "C" PRIMARY KEY, value TEXT COLLATE "C" NOT NULL);
CREATE TABLE IF NOT EXISTS blob_stores(name TEXT COLLATE "C" PRIMARY KEY, path TEXT COLLATE "C" NOT NULL, created_at BIGINT NOT NULL,
  type TEXT COLLATE "C" NOT NULL DEFAULT 'file', config TEXT COLLATE "C" NOT NULL DEFAULT '{}');
ALTER TABLE blob_stores ADD COLUMN IF NOT EXISTS type TEXT COLLATE "C" NOT NULL DEFAULT 'file';
ALTER TABLE blob_stores ADD COLUMN IF NOT EXISTS config TEXT COLLATE "C" NOT NULL DEFAULT '{}';
CREATE TABLE IF NOT EXISTS users(
  id TEXT COLLATE "C" PRIMARY KEY, name TEXT COLLATE "C" NOT NULL DEFAULT '', email TEXT COLLATE "C" NOT NULL DEFAULT '',
  password_hash TEXT COLLATE "C" NOT NULL DEFAULT '', source TEXT COLLATE "C" NOT NULL DEFAULT 'local',
  status TEXT COLLATE "C" NOT NULL DEFAULT 'active', roles TEXT COLLATE "C" NOT NULL DEFAULT '[]',
  created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS roles(
  id TEXT COLLATE "C" PRIMARY KEY, description TEXT COLLATE "C" NOT NULL DEFAULT '', privileges TEXT COLLATE "C" NOT NULL DEFAULT '[]',
  created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS request_stats(
  hour BIGINT NOT NULL, repo TEXT COLLATE "C" NOT NULL, requests BIGINT NOT NULL DEFAULT 0, bytes BIGINT NOT NULL DEFAULT 0,
  errors BIGINT NOT NULL DEFAULT 0, bytes_in BIGINT NOT NULL DEFAULT 0, ms BIGINT NOT NULL DEFAULT 0, max_ms BIGINT NOT NULL DEFAULT 0,
  upstream BIGINT NOT NULL DEFAULT 0, upstream_failures BIGINT NOT NULL DEFAULT 0, upstream_ms BIGINT NOT NULL DEFAULT 0,
  upstream_max_ms BIGINT NOT NULL DEFAULT 0, cache_hits BIGINT NOT NULL DEFAULT 0, cache_misses BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY(hour, repo));
ALTER TABLE request_stats ADD COLUMN IF NOT EXISTS errors BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_stats ADD COLUMN IF NOT EXISTS bytes_in BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_stats ADD COLUMN IF NOT EXISTS ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_stats ADD COLUMN IF NOT EXISTS max_ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_stats ADD COLUMN IF NOT EXISTS upstream BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_stats ADD COLUMN IF NOT EXISTS upstream_failures BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_stats ADD COLUMN IF NOT EXISTS upstream_ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_stats ADD COLUMN IF NOT EXISTS upstream_max_ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_stats ADD COLUMN IF NOT EXISTS cache_hits BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_stats ADD COLUMN IF NOT EXISTS cache_misses BIGINT NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS events(
  id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, at BIGINT NOT NULL, kind TEXT COLLATE "C" NOT NULL,
  actor TEXT COLLATE "C" NOT NULL DEFAULT '', repo TEXT COLLATE "C" NOT NULL DEFAULT '',
  subject TEXT COLLATE "C" NOT NULL DEFAULT '', detail TEXT COLLATE "C" NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS cleanup_policies(
  name TEXT COLLATE "C" PRIMARY KEY, format TEXT COLLATE "C" NOT NULL, config TEXT COLLATE "C" NOT NULL,
  created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS tasks(
  id TEXT COLLATE "C" PRIMARY KEY, name TEXT COLLATE "C" NOT NULL, type TEXT COLLATE "C" NOT NULL, schedule TEXT COLLATE "C" NOT NULL,
  params TEXT COLLATE "C" NOT NULL DEFAULT '{}', enabled BIGINT NOT NULL DEFAULT 1,
  created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL,
  last_run BIGINT NOT NULL DEFAULT 0, last_result TEXT COLLATE "C" NOT NULL DEFAULT '',
  last_duration_ms BIGINT NOT NULL DEFAULT 0, last_message TEXT COLLATE "C" NOT NULL DEFAULT '',
  last_log TEXT COLLATE "C" NOT NULL DEFAULT '', next_run BIGINT NOT NULL DEFAULT 0,
  last_removed BIGINT NOT NULL DEFAULT 0, last_bytes BIGINT NOT NULL DEFAULT 0,
  notify_email TEXT COLLATE "C" NOT NULL DEFAULT '[]');
CREATE TABLE IF NOT EXISTS webhooks(
  id TEXT COLLATE "C" PRIMARY KEY, name TEXT COLLATE "C" NOT NULL, url TEXT COLLATE "C" NOT NULL, secret TEXT COLLATE "C" NOT NULL DEFAULT '',
  events TEXT COLLATE "C" NOT NULL DEFAULT '[]', repo TEXT COLLATE "C" NOT NULL DEFAULT '', enabled BIGINT NOT NULL DEFAULT 1,
  created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS webhook_deliveries(
  id TEXT COLLATE "C" PRIMARY KEY, webhook_id TEXT COLLATE "C" NOT NULL, event TEXT COLLATE "C" NOT NULL, status TEXT COLLATE "C" NOT NULL DEFAULT 'pending',
  attempts BIGINT NOT NULL DEFAULT 0, last_error TEXT COLLATE "C" NOT NULL DEFAULT '', created_at BIGINT NOT NULL,
  next_at BIGINT NOT NULL DEFAULT 0, response_code BIGINT NOT NULL DEFAULT 0, payload TEXT COLLATE "C" NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS webhook_deliveries_hook ON webhook_deliveries(webhook_id, created_at);
CREATE TABLE IF NOT EXISTS schema_version(version BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS routing_rules(
  name TEXT COLLATE "C" PRIMARY KEY, description TEXT COLLATE "C" NOT NULL DEFAULT '', mode TEXT COLLATE "C" NOT NULL,
  matchers TEXT COLLATE "C" NOT NULL DEFAULT '[]', created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS content_selectors(
  name TEXT COLLATE "C" PRIMARY KEY, description TEXT COLLATE "C" NOT NULL DEFAULT '', expression TEXT COLLATE "C" NOT NULL,
  created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL);
`
