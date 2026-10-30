package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Usage counters (schema 7): the repository dashboard and the monthly
// usage metrics.
//
//   - repo_usage: one row per repository with assets. assets, bytes,
//     unique_bytes (the sizes of its distinct blobs), last_upload and changes
//     (asset writes) are kept by triggers on assets, in the transaction of
//     every asset write whichever node or build makes it, so they are exact
//     without scanning the table; last_download is moved by FlushDownloads.
//     components is counted from the asset paths outside the database (the
//     component model lives in Go: SetComponentCount) and is current while
//     counted_changes equals changes. The eval_ columns are the last
//     repository.evaluate of the repository.
//   - usage_months: one row per UTC calendar month (month: its first second,
//     Unix). uploads are the files stored in hosted repositories (new or
//     replaced content), dated by the asset's last_modified (the trigger);
//     downloads are added by FlushDownloads; requests, errors, egress and
//     ingress bytes are summed from request_stats and the storage, asset and
//     component columns sampled by SampleUsage.
//   - usage_seen: the distinct users and client addresses of each month, as
//     hashes (FlushSeen).
//   - eval_components: what repository.evaluate found per component of a
//     monitored repository, clean ones included.
//
// The triggers rely on what the package guarantees: one writer at a time
// (writeLock), an asset update never changing repo or path (the key), and
// one asset per INSERT or UPDATE statement (a statement writing two new
// assets of one blob would count the blob in neither). Deletes are counted
// per statement, so purging a repository's assets costs one update; the
// blob check is a probe per distinct blob (LATERAL ... LIMIT 1, so the
// plan does not depend on the table's statistics).
const usageSchema = `
CREATE TABLE IF NOT EXISTS repo_usage(
  repo TEXT COLLATE "C" PRIMARY KEY, assets BIGINT NOT NULL DEFAULT 0, bytes BIGINT NOT NULL DEFAULT 0,
  unique_bytes BIGINT NOT NULL DEFAULT 0,
  last_upload BIGINT NOT NULL DEFAULT 0, last_download BIGINT NOT NULL DEFAULT 0, changes BIGINT NOT NULL DEFAULT 0,
  components BIGINT NOT NULL DEFAULT -1, counted_changes BIGINT NOT NULL DEFAULT -1, counted_at BIGINT NOT NULL DEFAULT 0,
  eval_at BIGINT NOT NULL DEFAULT 0, eval_components BIGINT NOT NULL DEFAULT 0, eval_vulnerable BIGINT NOT NULL DEFAULT 0,
  eval_malicious BIGINT NOT NULL DEFAULT 0, eval_critical BIGINT NOT NULL DEFAULT 0, eval_high BIGINT NOT NULL DEFAULT 0,
  eval_medium BIGINT NOT NULL DEFAULT 0, eval_low BIGINT NOT NULL DEFAULT 0, eval_unknown BIGINT NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS usage_months(
  month BIGINT PRIMARY KEY, requests BIGINT NOT NULL DEFAULT 0, errors BIGINT NOT NULL DEFAULT 0,
  egress_bytes BIGINT NOT NULL DEFAULT 0, ingress_bytes BIGINT NOT NULL DEFAULT 0, downloads BIGINT NOT NULL DEFAULT 0,
  uploads BIGINT NOT NULL DEFAULT 0, upload_bytes BIGINT NOT NULL DEFAULT 0,
  storage_bytes BIGINT NOT NULL DEFAULT 0, peak_storage_bytes BIGINT NOT NULL DEFAULT 0, assets BIGINT NOT NULL DEFAULT 0,
  components BIGINT NOT NULL DEFAULT 0, peak_components BIGINT NOT NULL DEFAULT 0, sampled_at BIGINT NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS usage_seen(
  month BIGINT NOT NULL, kind TEXT COLLATE "C" NOT NULL, hash TEXT COLLATE "C" NOT NULL, PRIMARY KEY(month, kind, hash));
CREATE TABLE IF NOT EXISTS eval_components(
  repo TEXT COLLATE "C" NOT NULL, grp TEXT COLLATE "C" NOT NULL, name TEXT COLLATE "C" NOT NULL, version TEXT COLLATE "C" NOT NULL,
  ecosystem TEXT COLLATE "C" NOT NULL, pkg TEXT COLLATE "C" NOT NULL, pkg_version TEXT COLLATE "C" NOT NULL,
  gen TEXT COLLATE "C" NOT NULL DEFAULT '', evaluated_at BIGINT NOT NULL, ids TEXT COLLATE "C" NOT NULL DEFAULT '[]',
  vulns BIGINT NOT NULL DEFAULT 0, sev_rank BIGINT NOT NULL DEFAULT 0, severity TEXT COLLATE "C" NOT NULL DEFAULT '',
  malicious BIGINT NOT NULL DEFAULT 0, unchecked BIGINT NOT NULL DEFAULT 0, advisories TEXT COLLATE "C" NOT NULL DEFAULT '[]',
  PRIMARY KEY(repo, grp, name, version));
CREATE OR REPLACE FUNCTION cc_month(ms BIGINT) RETURNS BIGINT LANGUAGE sql IMMUTABLE AS
  $$ SELECT EXTRACT(EPOCH FROM date_trunc('month', to_timestamp(ms / 1000.0) AT TIME ZONE 'UTC'))::BIGINT $$;
CREATE OR REPLACE FUNCTION cc_asset_written() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  n BIGINT := 0;
  d BIGINT := NEW.size;
  u BIGINT := 0;
BEGIN
  IF TG_OP = 'INSERT' THEN
    n := 1;
  ELSE
    d := NEW.size - OLD.size;
  END IF;
  IF (TG_OP = 'INSERT' OR NEW.blob_sha256 <> OLD.blob_sha256) AND NOT EXISTS
      (SELECT 1 FROM assets a WHERE a.blob_sha256 = NEW.blob_sha256 AND a.repo = NEW.repo AND a.path <> NEW.path) THEN
    u := NEW.size;
  END IF;
  IF TG_OP = 'UPDATE' AND NEW.blob_sha256 <> OLD.blob_sha256 AND NOT EXISTS
      (SELECT 1 FROM assets a WHERE a.blob_sha256 = OLD.blob_sha256 AND a.repo = OLD.repo) THEN
    u := u - OLD.size;
  END IF;
  INSERT INTO repo_usage AS r(repo, assets, bytes, unique_bytes, last_upload, last_download, changes)
    VALUES(NEW.repo, n, d, u, NEW.last_modified, NEW.last_downloaded, 1)
  ON CONFLICT(repo) DO UPDATE SET assets = r.assets + n, bytes = r.bytes + d, unique_bytes = r.unique_bytes + u,
    last_upload = GREATEST(r.last_upload, NEW.last_modified), last_download = GREATEST(r.last_download, NEW.last_downloaded),
    changes = r.changes + 1;
  INSERT INTO usage_months AS m(month, uploads, upload_bytes)
    SELECT cc_month(NEW.last_modified), 1, NEW.size FROM repositories WHERE name = NEW.repo AND type = 'hosted'
  ON CONFLICT(month) DO UPDATE SET uploads = m.uploads + 1, upload_bytes = m.upload_bytes + excluded.upload_bytes;
  RETURN NULL;
END $$;
CREATE OR REPLACE FUNCTION cc_assets_deleted() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE repo_usage u SET assets = u.assets - d.n, bytes = u.bytes - d.b, changes = u.changes + d.n
    FROM (SELECT repo, COUNT(*) AS n, SUM(size) AS b FROM cc_old GROUP BY repo) d WHERE u.repo = d.repo;
  UPDATE repo_usage u SET unique_bytes = u.unique_bytes - d.b FROM (
    SELECT k.repo, SUM(k.size) AS b FROM (SELECT repo, blob_sha256, MAX(size) AS size FROM cc_old GROUP BY repo, blob_sha256) k
    LEFT JOIN LATERAL (SELECT 1 AS hit FROM assets a WHERE a.blob_sha256 = k.blob_sha256 AND a.repo = k.repo LIMIT 1) x ON true
    WHERE x.hit IS NULL GROUP BY k.repo) d WHERE u.repo = d.repo;
  RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS assets_usage_ins ON assets;
CREATE TRIGGER assets_usage_ins AFTER INSERT ON assets FOR EACH ROW EXECUTE FUNCTION cc_asset_written();
DROP TRIGGER IF EXISTS assets_usage_upd ON assets;
CREATE TRIGGER assets_usage_upd AFTER UPDATE ON assets FOR EACH ROW
  WHEN (OLD.blob_sha256 IS DISTINCT FROM NEW.blob_sha256 OR OLD.size IS DISTINCT FROM NEW.size OR OLD.last_modified IS DISTINCT FROM NEW.last_modified)
  EXECUTE FUNCTION cc_asset_written();
DROP TRIGGER IF EXISTS assets_usage_del ON assets;
CREATE TRIGGER assets_usage_del AFTER DELETE ON assets REFERENCING OLD TABLE AS cc_old FOR EACH STATEMENT EXECUTE FUNCTION cc_assets_deleted();
INSERT INTO repo_usage(repo, assets, bytes, unique_bytes, last_upload, last_download)
  SELECT repo, SUM(n), SUM(b), SUM(sz), MAX(up), MAX(ld) FROM (SELECT repo, blob_sha256, COUNT(*) AS n, SUM(size) AS b, MAX(size) AS sz,
    MAX(last_modified) AS up, MAX(last_downloaded) AS ld FROM assets GROUP BY repo, blob_sha256) x GROUP BY repo
ON CONFLICT(repo) DO NOTHING;
INSERT INTO usage_months(month, requests, errors, egress_bytes, ingress_bytes)
  SELECT cc_month(hour * 1000), SUM(requests), SUM(errors), SUM(bytes), SUM(bytes_in) FROM request_stats GROUP BY 1
ON CONFLICT(month) DO NOTHING;
`

// RepoUsage is one repository's row of the dashboard counters.
type RepoUsage struct {
	Repo                       string
	Assets, Bytes, UniqueBytes int64
	LastUpload, LastDownload   time.Time
	Changes                    int64 // asset writes so far: a component count is current while CountedChanges equals it
	Components, CountedChanges int64 // -1 before the first count
	CountedAt                  time.Time
	Eval                       EvalSummary
}

// EvalSummary is a repository's last evaluation: its components and the
// vulnerable ones by their highest severity.
type EvalSummary struct {
	At                                   time.Time // zero: never evaluated
	Components, Vulnerable, Malicious    int64
	Critical, High, Medium, Low, Unknown int64
}

const usageCols = `repo, assets, bytes, unique_bytes, last_upload, last_download, changes, components, counted_changes, counted_at,
  eval_at, eval_components, eval_vulnerable, eval_malicious, eval_critical, eval_high, eval_medium, eval_low, eval_unknown`

// RepoUsages returns every repository's counters by name; a repository
// that never had an asset has no row.
func (d *DB) RepoUsages(ctx context.Context) (map[string]RepoUsage, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+usageCols+` FROM repo_usage`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RepoUsage{}
	for rows.Next() {
		var u RepoUsage
		var up, dl, counted, evalAt int64
		e := &u.Eval
		if err := rows.Scan(&u.Repo, &u.Assets, &u.Bytes, &u.UniqueBytes, &up, &dl, &u.Changes, &u.Components, &u.CountedChanges,
			&counted, &evalAt, &e.Components, &e.Vulnerable, &e.Malicious, &e.Critical, &e.High, &e.Medium, &e.Low, &e.Unknown); err != nil {
			return nil, err
		}
		u.LastUpload, u.LastDownload, u.CountedAt, e.At = fromMs(up), fromMs(dl), fromMs(counted), fromMs(evalAt)
		out[u.Repo] = u
	}
	return out, rows.Err()
}

// SetComponentCount stores repo's component count, counted after changes
// asset writes (RepoUsage.Changes when the count began). A count that began
// before the stored one is dropped.
func (d *DB) SetComponentCount(ctx context.Context, repo string, n, changes int64, at time.Time) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO repo_usage AS u(repo, components, counted_changes, counted_at) VALUES(?,?,?,?)
ON CONFLICT(repo) DO UPDATE SET components = excluded.components, counted_changes = excluded.counted_changes, counted_at = excluded.counted_at
WHERE u.counted_changes <= excluded.counted_changes`, repo, n, changes, toMs(at))
	return err
}

// MonthStart is the first instant of t's UTC calendar month.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// SampleUsage records the current storage, asset and component totals in
// the month of now: storage_bytes and components are the latest sample,
// the peaks the highest one, a month's first sample counting the previous
// month's last (what the month started with). It runs under the write
// lock, so samples taken by several nodes land in order.
//
// The request totals of the month of now and the one before are summed
// again from request_stats, which keeps 90 days: every node's flushes count,
// and a month keeps its highest sum once its first hours are pruned.
func (d *DB) SampleUsage(ctx context.Context, now time.Time) error {
	month := MonthStart(now).Unix()
	return d.Tx(ctx, func(tx *Tx) error {
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO usage_months AS m(month, requests, errors, egress_bytes, ingress_bytes)
SELECT cc_month(hour * 1000), SUM(requests), SUM(errors), SUM(bytes), SUM(bytes_in) FROM request_stats WHERE hour >= ? GROUP BY 1
ON CONFLICT(month) DO UPDATE SET requests = GREATEST(m.requests, excluded.requests), errors = GREATEST(m.errors, excluded.errors),
  egress_bytes = GREATEST(m.egress_bytes, excluded.egress_bytes), ingress_bytes = GREATEST(m.ingress_bytes, excluded.ingress_bytes)`,
			MonthStart(MonthStart(now).Add(-time.Hour)).Unix()); err != nil {
			return err
		}
		_, err := tx.q.ExecContext(ctx, `WITH t AS (SELECT COALESCE(SUM(bytes), 0) AS b, COALESCE(SUM(assets), 0) AS a,
  COALESCE(SUM(GREATEST(components, 0)), 0) AS c FROM repo_usage),
p AS (SELECT COALESCE(MAX(storage_bytes), 0) AS b, COALESCE(MAX(components), 0) AS c FROM
  (SELECT storage_bytes, components FROM usage_months WHERE month < ? AND sampled_at > 0 ORDER BY month DESC LIMIT 1) l)
INSERT INTO usage_months AS m(month, storage_bytes, peak_storage_bytes, assets, components, peak_components, sampled_at)
SELECT ?, t.b, GREATEST(t.b, p.b), t.a, t.c, GREATEST(t.c, p.c), ? FROM t, p
ON CONFLICT(month) DO UPDATE SET storage_bytes = excluded.storage_bytes, assets = excluded.assets, components = excluded.components,
  peak_storage_bytes = GREATEST(m.peak_storage_bytes, excluded.peak_storage_bytes),
  peak_components = GREATEST(m.peak_components, excluded.peak_components), sampled_at = excluded.sampled_at`,
			month, month, toMs(now))
		return err
	})
}

// MonthUsage is one month of usage_months, with its distinct users and
// client addresses.
type MonthUsage struct {
	Month                                                  time.Time
	Requests, Errors, EgressBytes, IngressBytes, Downloads int64
	Uploads, UploadBytes                                   int64
	StorageBytes, PeakStorageBytes, Assets                 int64
	Components, PeakComponents                             int64
	Sampled                                                time.Time // zero: never sampled
	Users, Addresses                                       int64
}

// MonthlyUsage returns the stored months with from <= month < to, oldest
// first; months without a row are left out.
func (d *DB) MonthlyUsage(ctx context.Context, from, to time.Time) ([]MonthUsage, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT m.month, requests, errors, egress_bytes, ingress_bytes, downloads, uploads, upload_bytes,
  storage_bytes, peak_storage_bytes, assets, components, peak_components, sampled_at,
  (SELECT COUNT(*) FROM usage_seen s WHERE s.month = m.month AND s.kind = 'user'),
  (SELECT COUNT(*) FROM usage_seen s WHERE s.month = m.month AND s.kind = 'addr')
FROM usage_months m WHERE m.month >= ? AND m.month < ? ORDER BY m.month`, MonthStart(from).Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MonthUsage
	for rows.Next() {
		var u MonthUsage
		var month, sampled int64
		if err := rows.Scan(&month, &u.Requests, &u.Errors, &u.EgressBytes, &u.IngressBytes, &u.Downloads, &u.Uploads, &u.UploadBytes,
			&u.StorageBytes, &u.PeakStorageBytes, &u.Assets, &u.Components, &u.PeakComponents, &sampled, &u.Users, &u.Addresses); err != nil {
			return nil, err
		}
		u.Month, u.Sampled = time.Unix(month, 0).UTC(), fromMs(sampled)
		out = append(out, u)
	}
	return out, rows.Err()
}

// SeenKey is one distinct user ("user") or client address ("addr") of a
// month, as a hash.
type SeenKey struct {
	Month      int64 // MonthStart, Unix
	Kind, Hash string
}

// FlushSeen stores the keys; ones already stored are kept.
func (d *DB) FlushSeen(ctx context.Context, keys []SeenKey) error {
	if len(keys) == 0 {
		return nil
	}
	return d.Tx(ctx, func(tx *Tx) error {
		for _, k := range keys {
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO usage_seen(month, kind, hash) VALUES(?,?,?) ON CONFLICT DO NOTHING`,
				k.Month, k.Kind, k.Hash); err != nil {
				return err
			}
		}
		return nil
	})
}

// PruneUsage deletes the months before the one of before. Only the leader
// prunes (Leader): elsewhere it does nothing.
func (d *DB) PruneUsage(ctx context.Context, before time.Time) error {
	if !d.Leader() {
		return nil
	}
	m := MonthStart(before).Unix()
	return d.Tx(ctx, func(tx *Tx) error {
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM usage_months WHERE month < ?`, m); err != nil {
			return err
		}
		_, err := tx.q.ExecContext(ctx, `DELETE FROM usage_seen WHERE month < ?`, m)
		return err
	})
}

// ---- repository evaluation ----

// EvalComponent is what repository.evaluate found for one component.
type EvalComponent struct {
	Repo, Group, Name, Version         string // the component as the repository shows it
	Ecosystem, Package, PackageVersion string // its OSV package
	Gen                                string // the advisory data it was evaluated against
	Evaluated                          time.Time
	IDs                                []string // the advisories affecting it
	Severity                           string   // the highest; "" when none
	Rank                               int      // Severity's rank: critical 4 ... low 1, otherwise 0
	Malicious, Unchecked               bool     // Unchecked: the online lookup failed, local data only
	Advisories                         string   // JSON: the advisories' details
}

const evalCols = `grp, name, version, ecosystem, pkg, pkg_version, gen, evaluated_at, ids, severity, sev_rank, malicious, unchecked`

func scanEval(row scanner, repo string, advisories bool) (EvalComponent, error) {
	c := EvalComponent{Repo: repo}
	var at int64
	var ids string
	var mal, un int
	dest := []any{&c.Group, &c.Name, &c.Version, &c.Ecosystem, &c.Package, &c.PackageVersion, &c.Gen, &at, &ids, &c.Severity, &c.Rank, &mal, &un}
	if advisories {
		dest = append(dest, &c.Advisories)
	}
	if err := row.Scan(dest...); err != nil {
		return c, err
	}
	_ = json.Unmarshal([]byte(ids), &c.IDs)
	c.Evaluated, c.Malicious, c.Unchecked = fromMs(at), mal != 0, un != 0
	return c, nil
}

// WalkEvalComponents calls fn with every stored component of repo, without
// its advisories, holding one row at a time.
func (d *DB) WalkEvalComponents(ctx context.Context, repo string, fn func(EvalComponent)) error {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+evalCols+` FROM eval_components WHERE repo = ?`, repo)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanEval(rows, repo, false)
		if err != nil {
			return err
		}
		fn(c)
	}
	return rows.Err()
}

// PutEvalComponents stores evaluation results in one transaction.
func (d *DB) PutEvalComponents(ctx context.Context, cs []EvalComponent) error {
	if len(cs) == 0 {
		return nil
	}
	return d.Tx(ctx, func(tx *Tx) error {
		for _, c := range cs {
			adv := c.Advisories
			if adv == "" {
				adv = "[]"
			}
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO eval_components(repo, `+evalCols+`, vulns, advisories) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(repo, grp, name, version) DO UPDATE SET ecosystem = excluded.ecosystem, pkg = excluded.pkg, pkg_version = excluded.pkg_version,
  gen = excluded.gen, evaluated_at = excluded.evaluated_at, ids = excluded.ids, severity = excluded.severity, sev_rank = excluded.sev_rank,
  malicious = excluded.malicious, unchecked = excluded.unchecked, vulns = excluded.vulns, advisories = excluded.advisories`,
				c.Repo, c.Group, c.Name, c.Version, c.Ecosystem, c.Package, c.PackageVersion, c.Gen, toMs(c.Evaluated), jsonList(c.IDs),
				c.Severity, c.Rank, boolInt(c.Malicious), boolInt(c.Unchecked), len(c.IDs), adv); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteEvalComponents removes components of repo (Group, Name and
// Version of each) in one transaction.
func (d *DB) DeleteEvalComponents(ctx context.Context, repo string, cs []EvalComponent) error {
	if len(cs) == 0 {
		return nil
	}
	return d.Tx(ctx, func(tx *Tx) error {
		for _, c := range cs {
			if _, err := tx.q.ExecContext(ctx, `DELETE FROM eval_components WHERE repo = ? AND grp = ? AND name = ? AND version = ?`,
				repo, c.Group, c.Name, c.Version); err != nil {
				return err
			}
		}
		return nil
	})
}

// SummarizeEvaluation sums repo's stored components into its eval_
// counters, stamped at, and returns them.
func (d *DB) SummarizeEvaluation(ctx context.Context, repo string, at time.Time) (EvalSummary, error) {
	s := EvalSummary{At: at.UTC().Truncate(time.Millisecond)}
	err := d.Tx(ctx, func(tx *Tx) error {
		err := tx.q.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE vulns > 0), COUNT(*) FILTER (WHERE malicious = 1),
  COUNT(*) FILTER (WHERE vulns > 0 AND sev_rank = 4), COUNT(*) FILTER (WHERE vulns > 0 AND sev_rank = 3),
  COUNT(*) FILTER (WHERE vulns > 0 AND sev_rank = 2), COUNT(*) FILTER (WHERE vulns > 0 AND sev_rank = 1),
  COUNT(*) FILTER (WHERE vulns > 0 AND sev_rank = 0) FROM eval_components WHERE repo = ?`, repo).
			Scan(&s.Components, &s.Vulnerable, &s.Malicious, &s.Critical, &s.High, &s.Medium, &s.Low, &s.Unknown)
		if err != nil {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `INSERT INTO repo_usage AS u(repo, eval_at, eval_components, eval_vulnerable, eval_malicious,
  eval_critical, eval_high, eval_medium, eval_low, eval_unknown) VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(repo) DO UPDATE SET eval_at = excluded.eval_at, eval_components = excluded.eval_components, eval_vulnerable = excluded.eval_vulnerable,
  eval_malicious = excluded.eval_malicious, eval_critical = excluded.eval_critical, eval_high = excluded.eval_high,
  eval_medium = excluded.eval_medium, eval_low = excluded.eval_low, eval_unknown = excluded.eval_unknown`,
			repo, toMs(s.At), s.Components, s.Vulnerable, s.Malicious, s.Critical, s.High, s.Medium, s.Low, s.Unknown)
		return err
	})
	return s, err
}

// ClearEvaluation drops repo's evaluation: a repository no longer
// monitored. It reports whether there was one.
func (d *DB) ClearEvaluation(ctx context.Context, repo string) (bool, error) {
	var had bool
	err := d.Tx(ctx, func(tx *Tx) error {
		var at int64
		err := tx.q.QueryRowContext(ctx, `SELECT eval_at FROM repo_usage WHERE repo = ?`, repo).Scan(&at)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		res, err := tx.q.ExecContext(ctx, `DELETE FROM eval_components WHERE repo = ?`, repo)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		had = at > 0 || n > 0
		_, err = tx.q.ExecContext(ctx, `UPDATE repo_usage SET eval_at = 0, eval_components = 0, eval_vulnerable = 0, eval_malicious = 0,
  eval_critical = 0, eval_high = 0, eval_medium = 0, eval_low = 0, eval_unknown = 0 WHERE repo = ? AND eval_at > 0`, repo)
		return err
	})
	return had, err
}

// VulnerableComponents returns a page of repo's vulnerable components with
// their advisories, malicious ones first, then by severity and name, and
// how many there are.
func (d *DB) VulnerableComponents(ctx context.Context, repo string, minRank, limit, offset int) ([]EvalComponent, int64, error) {
	var total int64
	if err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM eval_components WHERE repo = ? AND vulns > 0 AND (sev_rank >= ? OR malicious = 1)`,
		repo, minRank).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT `+evalCols+`, advisories FROM eval_components WHERE repo = ? AND vulns > 0 AND (sev_rank >= ? OR malicious = 1)
ORDER BY malicious DESC, sev_rank DESC, vulns DESC, name, grp, version LIMIT ? OFFSET ?`, repo, minRank, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []EvalComponent
	for rows.Next() {
		c, err := scanEval(rows, repo, true)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// WalkPackageVersions calls fn with the name and version of each package
// row of repo, holding one row at a time.
func (d *DB) WalkPackageVersions(ctx context.Context, repo string, fn func(name, version string)) error {
	rows, err := d.sql.QueryContext(ctx, `SELECT name, version FROM packages WHERE repo = ?`, repo)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n, v string
		if err := rows.Scan(&n, &v); err != nil {
			return err
		}
		fn(n, v)
	}
	return rows.Err()
}
