// Package meta stores repository metadata in PostgreSQL.
package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
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
	sql *pool
	box sealer // seals the stored secrets; see SetSecretKey

	keys keyedLocks             // Lock's in-process mutexes (cluster.go)
	cl   *cluster               // the lock connection and the listener
	self atomic.Pointer[Member] // this process's node, once Join ran (nodes.go)

	// OnCommit, if set, is called after each commit that wrote rows of
	// assets, packages or dist_tags, with the database's clock just before
	// the commit and those writes in order (the reconcile journal,
	// journal.go). Set it before d is shared; it must not block.
	OnCommit func(ctx context.Context, at time.Time, changes []Change)
	recovery atomic.Pointer[RecoveryMode] // the cached recovery mode (reconcile.go)
}

// querier is implemented by *pool and txConn (dialect.go).
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) row
}

// schemaVersion is the database layout this build reads and writes, kept in
// the schema_version table (which starts at 1). Open refuses a database
// with a higher version: an older build must never run on data a newer one
// wrote (QA N-30). Bump it with every change an older build would
// mishandle (a table or column it would not maintain, a stored value it
// would misread). Builds before version 1 left it at 0 and do not check it.
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
//	4: blob_stores rows of type "group" (blob store groups): an older build
//	   would open a group as an empty directory and compact its members as
//	   stores no repository uses, deleting every blob they hold.
//	4: cleanup_policies.config retain, exclude and pathPattern: an older
//	   build would ignore them and delete the versions they keep.
//	4: tags, component_tags and blob_store_moves: an older build would keep
//	   the tags of deleted components and read a repository that is moving
//	   to another blob store from its old store only (404 for what was
//	   uploaded during the move).
//	4: vuln_advisories, vuln_queries, vuln_findings, firewall_quarantine: an
//	   older build would serve what a proxy's firewall quarantines.
//	5: nodes, leases, schema_version.compat: an older build would start
//	   beside a running server without the single-writer guard (nodes.go)
//	   and ignore compat.
//	6: tasks.running_node and running_since, webhook_deliveries.claimed_by
//	   and claim_until (high availability, scheduling): expand-only, an older
//	   build ignores them and, alone on the database (the single-writer
//	   guard), needs neither the claims nor the marks.
//	6: sessions, login_states, saml_replay, auth_failures, sso_identities
//	   (authstate.go) and the settings rows registry.tokenKey and
//	   secretKey.check: an older build would keep its sessions in memory,
//	   leave a changed user's stored sessions for this one to accept again,
//	   and seal with another secret key unchecked.
//	6: repositories.gen, the incarnation (repositories.go): expand-only, an
//	   older build ignores it and keeps its in-process incarnations.
//	7: blob_stores rows of type "azure": an older build would open one as an
//	   empty directory (404s, uploads left on its disk), but no DDL: the
//	   first azure store's CreateBlobStore raises compat to 7 (raiseCompat),
//	   so the upgrade itself is expand-only and stays rolling.
//	7: blob_stores rows of type "google" (Google Cloud Storage stores): the
//	   same, through the same raiseCompat in CreateBlobStore.
//	7: reconcile_plans and reconcile_items (reconcile.go): expand-only, new
//	   tables an older build never reads; beside this one it writes no
//	   reconcile journal and its compaction does not keep the blobs a
//	   pending plan adds, so plans are made and run once every node runs
//	   this version.
//	7: repo_usage, usage_months, usage_seen and eval_components, and the
//	   triggers on assets and request_stats that keep the usage counters
//	   (usage.go): expand-only, an older build ignores the tables, and its
//	   asset and request writes still update the counters (the triggers run
//	   in the database).
const schemaVersion = 7

// schemaCompat is the oldest schemaVersion that may run on a database this
// build upgraded, stamped in schema_version.compat. A change older builds
// can ignore ("expand-only": new tables, or new columns with defaults, and
// no new meaning for stored values) sets it to schemaVersion-1, so the
// previous release still runs on the upgraded database: a rolling upgrade,
// or a one-release rollback. Any other change sets it to schemaVersion. The
// rule (N/N-1):
//
//   - Open refuses a database whose version and compat are both above
//     schemaVersion;
//   - an upgrade is refused while a live node of a schema below
//     schemaCompat is registered (the rollout stalls instead of breaking
//     that node);
//   - a running node re-reads both with every heartbeat and turns unready
//     when compat rises above its schemaVersion.
//
// TestSchemaCompat pins every version's decision, so a bump needs one.
const schemaCompat = 6

// Open opens the PostgreSQL database at url and creates or upgrades its
// tables. Errors never hold the URL.
func Open(url string) (*DB, error) {
	db, cfg, name, err := openPostgres(url)
	if err != nil {
		return nil, err
	}
	p := &pool{db: db}
	if err := upgrade(p, name); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{sql: p, cl: newCluster(cfg)}, nil
}

// SetMaxOpenConns sets the size of the connection pool (default 10). Call
// it before d is used.
func (d *DB) SetMaxOpenConns(n int) {
	d.sql.db.SetMaxOpenConns(n)
	d.sql.db.SetMaxIdleConns(n)
}

// SizeBytes returns the size of the database (pg_database_size).
func (d *DB) SizeBytes(ctx context.Context) (int64, error) {
	var n int64
	err := d.sql.QueryRowContext(ctx, `SELECT pg_database_size(current_database())`).Scan(&n)
	return n, err
}

// upgrade creates the tables, adds the columns older databases lack and
// stamps schemaVersion and schemaCompat in one transaction, so a crash
// leaves the database either as it was or migrated and stamped. A database
// a newer build wrote is refused before anything is written unless its
// compat lets this build run on it, and one at or above schemaVersion is
// left alone: no DDL runs, so a PostgreSQL role with DML rights alone opens
// it. The transaction first takes migrateLock without a lock
// or statement timeout, so nodes starting at once upgrade one after the
// other however long the DDL takes (the startup probe bounds the wait), and
// takes the write lock only for the DDL.
func upgrade(p *pool, name string) error {
	ctx := context.Background()
	tx, err := beginUpgrade(ctx, p)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := txConn{tx: tx}
	version, compat, err := readSchema(ctx, q)
	if err != nil {
		return err
	}
	if version > schemaVersion && compat > schemaVersion {
		return fmt.Errorf("%s was written by a newer version of Command Center (database schema %d, for builds of schema %d and newer; "+
			"this version supports up to %d) and this version would misread it: run the newer version again, or restore the backup taken "+
			"before the upgrade together with this version (operations guide, \"업그레이드\" and \"백업과 복원\")", name, version, compat, schemaVersion)
	}
	if version >= schemaVersion {
		return tx.Commit()
	}
	if err := refuseOlderNodes(ctx, q, name); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, writeLock); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, schema); err != nil {
		return ddlError(err, name)
	}
	if err := stampSchemaVersion(ctx, q, schemaVersion, schemaCompat); err != nil {
		return err
	}
	return tx.Commit()
}

// beginUpgrade starts upgrade's transaction, which holds migrateLock and
// waits without a timeout.
func beginUpgrade(ctx context.Context, p *pool) (*sql.Tx, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, q := range []string{`SET LOCAL lock_timeout = 0`, `SET LOCAL statement_timeout = 0`, migrateLock} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	return tx, nil
}

// refuseOlderNodes refuses an upgrade while a live node runs a build older
// than schemaCompat: the change would break it.
func refuseOlderNodes(ctx context.Context, q querier, name string) error {
	var has bool
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('nodes') IS NOT NULL`).Scan(&has); err != nil || !has {
		return err
	}
	var id string
	var s int
	err := q.QueryRowContext(ctx, `SELECT id, schema FROM nodes WHERE schema < ? AND heartbeat_at > `+dbNow+` - ? ORDER BY id LIMIT 1`,
		schemaCompat, NodeTTL.Milliseconds()).Scan(&id, &s)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%s cannot be upgraded to schema %d while node %s, a build of schema %d, is running on it (this upgrade needs every node "+
		"at schema %d or newer): stop the older nodes first (scale them down, or roll out with the Recreate strategy), then start this version",
		name, schemaVersion, id, s, schemaCompat)
}

// readSchema reads the stamped schema version and compat: 0 for a new
// database or one from before the stamp; compat is the version itself on
// databases stamped before compat existed. It runs no DDL (schema creates
// the table and the column).
func readSchema(ctx context.Context, q querier) (version, compat int, err error) {
	var stamped, hasCompat bool
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('schema_version') IS NOT NULL,
  EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('schema_version') AND attname = 'compat' AND NOT attisdropped)`).Scan(&stamped, &hasCompat); err != nil || !stamped {
		return 0, 0, err
	}
	if !hasCompat {
		err = q.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version)
		return version, version, err
	}
	err = q.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0), COALESCE(MAX(compat), 0) FROM schema_version`).Scan(&version, &compat)
	return version, compat, err
}

// schemaVersionOf reads the stamped schema version (readSchema).
func schemaVersionOf(ctx context.Context, q querier) (int, error) {
	v, _, err := readSchema(ctx, q)
	return v, err
}

// stampSchemaVersion stamps version v and compat.
func stampSchemaVersion(ctx context.Context, q querier, v, compat int) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM schema_version`); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `INSERT INTO schema_version(version, compat) VALUES(?, ?)`, v, compat)
	return err
}

// Close leaves the node registry if Join ran, closes the lock connection and
// the listener, then the database.
func (d *DB) Close() error {
	if m := d.self.Load(); m != nil {
		m.Leave(context.Background())
	}
	d.cl.close()
	return d.sql.db.Close()
}

const assetCols = `repo, path, blob_sha256, size, content_type, sha1, md5, sha512,
  created_at, last_modified, remote_etag, remote_last_modified, checked_at`

// assetSelect is assetCols plus the counters, which only the metrics flush writes.
const assetSelect = assetCols + `, downloads, last_downloaded`

func (d *DB) GetAsset(ctx context.Context, repo, path string) (Asset, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+assetSelect+` FROM assets WHERE repo = ? AND path = ?`, repo, path)
	return scanAsset(row)
}

// PutAsset inserts or replaces an asset; an existing CreatedAt is kept.
// It is Tx.PutAsset in a transaction of its own, so the reconcile journal
// records it (OnCommit).
func (d *DB) PutAsset(ctx context.Context, a Asset) error {
	return d.Tx(ctx, func(t *Tx) error { return t.PutAsset(ctx, a) })
}

// putAsset upserts a and returns the created_at the row keeps. q is a
// transaction: the RETURNING query takes no write lock of its own.
func putAsset(ctx context.Context, q querier, a Asset) (time.Time, error) {
	var created int64
	err := writeRow(ctx, q, `INSERT INTO assets(`+assetCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(repo, path) DO UPDATE SET
  blob_sha256 = excluded.blob_sha256, size = excluded.size, content_type = excluded.content_type,
  sha1 = excluded.sha1, md5 = excluded.md5, sha512 = excluded.sha512,
  last_modified = excluded.last_modified, remote_etag = excluded.remote_etag,
  remote_last_modified = excluded.remote_last_modified, checked_at = excluded.checked_at
RETURNING created_at`, []any{a.Repo, a.Path, a.BlobSHA256, a.Size, a.ContentType, a.SHA1, a.MD5, a.SHA512,
		toMs(a.CreatedAt), toMs(a.LastModified), a.RemoteETag, a.RemoteLastModified, toMs(a.CheckedAt)}, &created)
	return fromMs(created), err
}

// DeleteAsset is Tx.DeleteAsset in a transaction of its own.
func (d *DB) DeleteAsset(ctx context.Context, repo, path string) error {
	return d.Tx(ctx, func(t *Tx) error { _, err := t.DeleteAsset(ctx, repo, path); return err })
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

// AssetPathsWhere returns the paths of repo's assets whose checksums equal
// sums (md5, sha1, sha256 for the blob's, sha512; lower-case hex, stored
// sums compared without case)
// and, with a non-zero since, that were last modified at or after it.
func (d *DB) AssetPathsWhere(ctx context.Context, repo string, sums map[string]string, since time.Time) (map[string]bool, error) {
	q := `SELECT path FROM assets WHERE repo = ?`
	args := []any{repo}
	for _, col := range []string{"md5", "sha1", "sha256", "sha512"} {
		if v, ok := sums[col]; ok {
			if col == "sha256" {
				col = "blob_sha256"
			}
			q += ` AND lower(` + col + `) = ?`
			args = append(args, v)
		}
	}
	if !since.IsZero() {
		q += ` AND last_modified >= ?`
		args = append(args, toMs(since))
	}
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
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
// hit at now (see NegativeHit) and returns how many rows went. Only the
// leader prunes (Leader): elsewhere it does nothing.
func (d *DB) PruneNegative(ctx context.Context, now time.Time) (int64, error) {
	if !d.Leader() {
		return 0, nil
	}
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
