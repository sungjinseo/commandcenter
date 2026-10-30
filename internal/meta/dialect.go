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

// The package's SQL is written with ? placeholders, which pool and txConn
// rebind to $1, $2, ... What the code relies on:
//
//   - One writer at a time: every write, a transaction or a single
//     statement, first takes the advisory lock writeLock, held until it
//     commits or rolls back. Reads take no lock and run beside a writer.
//   - A write waits at most 5 s for the lock (lock_timeout; a URL may set
//     another). The error is not mapped. Constraint errors are not mapped
//     either: ErrExists comes from a SELECT made under the write lock,
//     which the advisory lock keeps exact.
//   - Text compares, sorts and prefix-matches byte-wise: every text column
//     is COLLATE "C", whatever the database's locale. Case folding is ASCII
//     only: lower() under the "C" collation.
//   - JSON is stored as text, so documents read back byte for byte, and
//     read with ->> and json_array_elements_text.
//   - Integers, booleans (0/1) and times (Unix ms) are BIGINT; events.id is
//     an identity column. No query sorts a nullable column, so NULL
//     ordering does not arise.
//   - Text cannot hold invalid UTF-8 or NUL bytes (older builds stored such
//     Maven names). No stored row can match such a value, so a query with
//     one returns no rows without asking the server, and a write fails with
//     ErrInvalidText.

// writeLock takes the advisory lock of the one writer (key "ccmeta").
const writeLock = `SELECT pg_advisory_xact_lock(109278688277601)`

// migrateLock takes the advisory lock of the one upgrade (key "ccmigr"):
// nodes that start at once upgrade in turn, and the later ones find the
// database upgraded.
const migrateLock = `SELECT pg_advisory_xact_lock(109278688536434)`

// pool is the database handle: it rebinds and locks writes.
type pool struct {
	db *sql.DB
}

// txConn is a transaction on a pool.
type txConn struct {
	tx *sql.Tx
}

// ErrInvalidText refuses a write of text PostgreSQL cannot store: not valid
// UTF-8, or holding a NUL byte.
var ErrInvalidText = errors.New("text that is not valid UTF-8 or holds a NUL byte cannot be stored")

// storable reports whether the database can take every string in args.
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
	if !storable(args) {
		return p.db.QueryContext(ctx, noRows)
	}
	return p.db.QueryContext(ctx, rebind(q), args...)
}

func (p *pool) QueryRowContext(ctx context.Context, q string, args ...any) row {
	if !storable(args) {
		return row{err: sql.ErrNoRows}
	}
	return row{r: p.db.QueryRowContext(ctx, rebind(q), args...)}
}

// ExecContext runs a write: a transaction of its own that takes the write
// lock first.
func (p *pool) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
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

// execFree runs a single-statement write without the write lock, for the
// bookkeeping tables no transaction that
// checks an invariant reads: nodes, leases and the auth state (authstate.go).
// It keeps frequent small writes, such as heartbeats, from queueing behind
// the one writer.
func (p *pool) execFree(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return p.db.ExecContext(ctx, rebind(q), args...)
}

// begin starts a write transaction, which holds the write lock, or a read
// transaction, which sees one snapshot (REPEATABLE READ) and takes no lock.
func (p *pool) begin(ctx context.Context, write bool) (*sql.Tx, error) {
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
	if !storable(args) {
		return nil, ErrInvalidText
	}
	return t.tx.ExecContext(ctx, rebind(q), args...)
}

func (t txConn) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if !storable(args) {
		return t.tx.QueryContext(ctx, noRows)
	}
	return t.tx.QueryContext(ctx, rebind(q), args...)
}

func (t txConn) QueryRowContext(ctx context.Context, q string, args ...any) row {
	if !storable(args) {
		return row{err: sql.ErrNoRows}
	}
	return row{r: t.tx.QueryRowContext(ctx, rebind(q), args...)}
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

// IsPostgresURL reports whether target is a postgres:// or postgresql:// URL.
func IsPostgresURL(target string) bool {
	return strings.HasPrefix(target, "postgres://") || strings.HasPrefix(target, "postgresql://")
}

// errBadURL never holds the URL: it may hold a password.
var errBadURL = errors.New("the database URL does not parse: use postgres://user:password@host:5432/database?sslmode=... " +
	"(percent-encode special characters in the user name and password)")

// openPostgres connects to the database at url and returns its config too,
// for the cluster connections. The returned name says which database it is
// in messages: never the URL, which may hold a password.
func openPostgres(url string) (*sql.DB, *pgx.ConnConfig, string, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, nil, "", errBadURL
	}
	if _, ok := cfg.RuntimeParams["lock_timeout"]; !ok {
		cfg.RuntimeParams["lock_timeout"] = "5000" // ms
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(defaultPostgresConns)
	db.SetMaxIdleConns(defaultPostgresConns)
	return db, cfg, "PostgreSQL database " + strconv.Quote(cfg.Database) + " on " + cfg.Host, nil
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

// defaultPostgresConns is the pool size unless SetMaxOpenConns sets another.
const defaultPostgresConns = 10

// schema creates the tables at schemaVersion with every column. A later
// column goes into its CREATE TABLE and into an ALTER TABLE ... ADD COLUMN
// IF NOT EXISTS, for databases created before it.
const schema = `
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
  created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL, gen BIGINT NOT NULL DEFAULT 0);
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS gen BIGINT NOT NULL DEFAULT 0;
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
  notify_email TEXT COLLATE "C" NOT NULL DEFAULT '[]', running_node TEXT COLLATE "C" NOT NULL DEFAULT '',
  running_since BIGINT NOT NULL DEFAULT 0);
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS running_node TEXT COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS running_since BIGINT NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS webhooks(
  id TEXT COLLATE "C" PRIMARY KEY, name TEXT COLLATE "C" NOT NULL, url TEXT COLLATE "C" NOT NULL, secret TEXT COLLATE "C" NOT NULL DEFAULT '',
  events TEXT COLLATE "C" NOT NULL DEFAULT '[]', repo TEXT COLLATE "C" NOT NULL DEFAULT '', enabled BIGINT NOT NULL DEFAULT 1,
  created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS webhook_deliveries(
  id TEXT COLLATE "C" PRIMARY KEY, webhook_id TEXT COLLATE "C" NOT NULL, event TEXT COLLATE "C" NOT NULL, status TEXT COLLATE "C" NOT NULL DEFAULT 'pending',
  attempts BIGINT NOT NULL DEFAULT 0, last_error TEXT COLLATE "C" NOT NULL DEFAULT '', created_at BIGINT NOT NULL,
  next_at BIGINT NOT NULL DEFAULT 0, response_code BIGINT NOT NULL DEFAULT 0, payload TEXT COLLATE "C" NOT NULL DEFAULT '',
  claimed_by TEXT COLLATE "C" NOT NULL DEFAULT '', claim_until BIGINT NOT NULL DEFAULT 0);
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS claimed_by TEXT COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS claim_until BIGINT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS webhook_deliveries_hook ON webhook_deliveries(webhook_id, created_at);
CREATE INDEX IF NOT EXISTS webhook_deliveries_due ON webhook_deliveries(status, next_at);
CREATE TABLE IF NOT EXISTS schema_version(version BIGINT NOT NULL, compat BIGINT NOT NULL DEFAULT 0);
ALTER TABLE schema_version ADD COLUMN IF NOT EXISTS compat BIGINT NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS routing_rules(
  name TEXT COLLATE "C" PRIMARY KEY, description TEXT COLLATE "C" NOT NULL DEFAULT '', mode TEXT COLLATE "C" NOT NULL,
  matchers TEXT COLLATE "C" NOT NULL DEFAULT '[]', created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS content_selectors(
  name TEXT COLLATE "C" PRIMARY KEY, description TEXT COLLATE "C" NOT NULL DEFAULT '', expression TEXT COLLATE "C" NOT NULL,
  created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS user_tokens(
  owner TEXT COLLATE "C" PRIMARY KEY, name_code TEXT COLLATE "C" NOT NULL UNIQUE, hash TEXT COLLATE "C" NOT NULL,
  created_at BIGINT NOT NULL, last_used_at BIGINT NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS tags(
  name TEXT COLLATE "C" PRIMARY KEY, attributes TEXT COLLATE "C" NOT NULL DEFAULT '{}',
  created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS component_tags(
  tag TEXT COLLATE "C" NOT NULL, repo TEXT COLLATE "C" NOT NULL, grp TEXT COLLATE "C" NOT NULL,
  name TEXT COLLATE "C" NOT NULL, version TEXT COLLATE "C" NOT NULL,
  PRIMARY KEY(tag, repo, grp, name, version));
CREATE INDEX IF NOT EXISTS component_tags_component ON component_tags(repo, grp, name, version);
CREATE TABLE IF NOT EXISTS blob_store_moves(
  repo TEXT COLLATE "C" PRIMARY KEY, source TEXT COLLATE "C" NOT NULL, target TEXT COLLATE "C" NOT NULL,
  started_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS vuln_advisories(
  ecosystem TEXT COLLATE "C" NOT NULL, pkg TEXT COLLATE "C" NOT NULL, id TEXT COLLATE "C" NOT NULL,
  aliases TEXT COLLATE "C" NOT NULL DEFAULT '[]', summary TEXT COLLATE "C" NOT NULL DEFAULT '', severity TEXT COLLATE "C" NOT NULL DEFAULT '',
  score BIGINT NOT NULL DEFAULT -1, vector TEXT COLLATE "C" NOT NULL DEFAULT '', malicious BIGINT NOT NULL DEFAULT 0,
  affected TEXT COLLATE "C" NOT NULL DEFAULT '[]', published BIGINT NOT NULL DEFAULT 0, modified BIGINT NOT NULL DEFAULT 0,
  withdrawn BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY(ecosystem, pkg, id));
CREATE INDEX IF NOT EXISTS vuln_advisories_id ON vuln_advisories(id);
CREATE TABLE IF NOT EXISTS vuln_queries(
  ecosystem TEXT COLLATE "C" NOT NULL, pkg TEXT COLLATE "C" NOT NULL, version TEXT COLLATE "C" NOT NULL,
  ids TEXT COLLATE "C" NOT NULL DEFAULT '[]', checked_at BIGINT NOT NULL,
  PRIMARY KEY(ecosystem, pkg, version));
CREATE TABLE IF NOT EXISTS vuln_findings(
  repo TEXT COLLATE "C" NOT NULL, ecosystem TEXT COLLATE "C" NOT NULL, pkg TEXT COLLATE "C" NOT NULL, version TEXT COLLATE "C" NOT NULL,
  name TEXT COLLATE "C" NOT NULL DEFAULT '', ids TEXT COLLATE "C" NOT NULL DEFAULT '[]', vulns BIGINT NOT NULL DEFAULT 0,
  severity TEXT COLLATE "C" NOT NULL DEFAULT '', sev_rank BIGINT NOT NULL DEFAULT 0, malicious BIGINT NOT NULL DEFAULT 0,
  unchecked BIGINT NOT NULL DEFAULT 0, scanned_at BIGINT NOT NULL,
  PRIMARY KEY(repo, ecosystem, pkg, version));
CREATE TABLE IF NOT EXISTS firewall_quarantine(
  repo TEXT COLLATE "C" NOT NULL, name TEXT COLLATE "C" NOT NULL, version TEXT COLLATE "C" NOT NULL,
  ecosystem TEXT COLLATE "C" NOT NULL DEFAULT '', path TEXT COLLATE "C" NOT NULL DEFAULT '', ids TEXT COLLATE "C" NOT NULL DEFAULT '[]',
  severity TEXT COLLATE "C" NOT NULL DEFAULT '', malicious BIGINT NOT NULL DEFAULT 0,
  first_at BIGINT NOT NULL, last_at BIGINT NOT NULL, hits BIGINT NOT NULL DEFAULT 1,
  PRIMARY KEY(repo, name, version));
CREATE TABLE IF NOT EXISTS nodes(
  id TEXT COLLATE "C" PRIMARY KEY, hostname TEXT COLLATE "C" NOT NULL, version TEXT COLLATE "C" NOT NULL,
  schema BIGINT NOT NULL, started_at BIGINT NOT NULL, heartbeat_at BIGINT NOT NULL, draining BIGINT NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS leases(name TEXT COLLATE "C" PRIMARY KEY, holder TEXT COLLATE "C" NOT NULL, expires_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions(key TEXT COLLATE "C" PRIMARY KEY, owner TEXT COLLATE "C" NOT NULL, created_at BIGINT NOT NULL,
  last_seen BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS sessions_owner ON sessions(owner, last_seen);
CREATE TABLE IF NOT EXISTS login_states(state TEXT COLLATE "C" PRIMARY KEY, realm TEXT COLLATE "C" NOT NULL, nonce TEXT COLLATE "C" NOT NULL,
  verifier TEXT COLLATE "C" NOT NULL DEFAULT '', next TEXT COLLATE "C" NOT NULL DEFAULT '', created_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS saml_replay(id TEXT COLLATE "C" PRIMARY KEY, expires_at BIGINT NOT NULL);
CREATE UNLOGGED TABLE IF NOT EXISTS auth_failures(key TEXT COLLATE "C" PRIMARY KEY, fails BIGINT NOT NULL, last_fail BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS sso_identities(realm TEXT COLLATE "C" NOT NULL, id TEXT COLLATE "C" NOT NULL, conf TEXT COLLATE "C" NOT NULL,
  data TEXT COLLATE "C" NOT NULL, checked_at BIGINT NOT NULL, PRIMARY KEY(realm, id));
CREATE TABLE IF NOT EXISTS reconcile_plans(
  id TEXT COLLATE "C" PRIMARY KEY, blob_store TEXT COLLATE "C" NOT NULL DEFAULT '', state TEXT COLLATE "C" NOT NULL,
  since BIGINT NOT NULL DEFAULT 0, created_at BIGINT NOT NULL, created_by TEXT COLLATE "C" NOT NULL DEFAULT '',
  executed_at BIGINT NOT NULL DEFAULT 0, summary TEXT COLLATE "C" NOT NULL DEFAULT '{}');
CREATE TABLE IF NOT EXISTS reconcile_items(
  plan_id TEXT COLLATE "C" NOT NULL, seq BIGINT NOT NULL, repo TEXT COLLATE "C" NOT NULL, kind TEXT COLLATE "C" NOT NULL,
  label TEXT COLLATE "C" NOT NULL, action TEXT COLLATE "C" NOT NULL, state TEXT COLLATE "C" NOT NULL,
  reason TEXT COLLATE "C" NOT NULL DEFAULT '', blob_sha256 TEXT COLLATE "C" NOT NULL DEFAULT '',
  journal_key TEXT COLLATE "C" NOT NULL DEFAULT '', at BIGINT NOT NULL DEFAULT 0, change TEXT COLLATE "C" NOT NULL DEFAULT '{}',
  PRIMARY KEY(plan_id, seq));
CREATE INDEX IF NOT EXISTS reconcile_items_pending ON reconcile_items(blob_sha256) WHERE state IN ('PLANNED', 'FAILED');
`
