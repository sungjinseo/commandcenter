package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Vulnerability data (OSV advisories) and the firewall's quarantine.
//
//   - vuln_advisories: one row per advisory and affected package, keyed by
//     the OSV ecosystem and the package name as matched (internal/vuln
//     normalizes case-insensitive names); affected holds that package's
//     OSV affected entries (ranges and versions) as JSON.
//   - vuln_queries: the online API's answer per package version (the ids
//     that affect it), reused for the cacheHours setting.
//   - vuln_findings: what the vulnerability.scan task found per component
//     of a repository, one row per scanned component, clean ones included.
//   - firewall_quarantine: the components a proxy's firewall refused.

// VulnAdvisory is one advisory for one package.
type VulnAdvisory struct {
	ID, Ecosystem, Package         string
	Aliases                        []string
	Summary                        string
	Severity                       string // critical, high, medium, low, none or unknown
	Score10                        int    // base score times ten; -1 when unknown
	Vector                         string // the CVSS vector the score is from, or a CVSS 4 vector
	Malicious                      bool
	Affected                       string // JSON: the package's OSV affected entries
	Published, Modified, Withdrawn time.Time
}

const vulnCols = `ecosystem, pkg, id, aliases, summary, severity, score, vector, malicious, affected, published, modified, withdrawn`

func scanVulnAdvisory(row scanner) (VulnAdvisory, error) {
	var v VulnAdvisory
	var aliases string
	var mal int
	var pub, mod, wd int64
	err := row.Scan(&v.Ecosystem, &v.Package, &v.ID, &aliases, &v.Summary, &v.Severity, &v.Score10, &v.Vector, &mal, &v.Affected, &pub, &mod, &wd)
	if errors.Is(err, sql.ErrNoRows) {
		return VulnAdvisory{}, ErrNotFound
	}
	if err != nil {
		return VulnAdvisory{}, err
	}
	_ = json.Unmarshal([]byte(aliases), &v.Aliases)
	v.Malicious = mal != 0
	v.Published, v.Modified, v.Withdrawn = fromMs(pub), fromMs(mod), fromMs(wd)
	return v, nil
}

func jsonList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// ReplaceVulnAdvisories stores advisories in one transaction: every stored
// row of each id in ids goes, then rows are inserted, so an advisory that
// no longer affects a package loses its row.
func (d *DB) ReplaceVulnAdvisories(ctx context.Context, ids []string, rows []VulnAdvisory) error {
	return d.Tx(ctx, func(tx *Tx) error {
		for _, id := range ids {
			if _, err := tx.q.ExecContext(ctx, `DELETE FROM vuln_advisories WHERE id = ?`, id); err != nil {
				return err
			}
		}
		for _, v := range rows {
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO vuln_advisories(`+vulnCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(ecosystem, pkg, id) DO UPDATE SET aliases = excluded.aliases, summary = excluded.summary,
  severity = excluded.severity, score = excluded.score, vector = excluded.vector, malicious = excluded.malicious,
  affected = excluded.affected, published = excluded.published, modified = excluded.modified, withdrawn = excluded.withdrawn`,
				v.Ecosystem, v.Package, v.ID, jsonList(v.Aliases), v.Summary, v.Severity, v.Score10, v.Vector, boolInt(v.Malicious),
				v.Affected, toMs(v.Published), toMs(v.Modified), toMs(v.Withdrawn)); err != nil {
				return err
			}
		}
		return nil
	})
}

// VulnAdvisoriesFor returns the advisories stored for one package.
func (d *DB) VulnAdvisoriesFor(ctx context.Context, ecosystem, pkg string) ([]VulnAdvisory, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+vulnCols+` FROM vuln_advisories WHERE ecosystem = ? AND pkg = ? ORDER BY id`, ecosystem, pkg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VulnAdvisory
	for rows.Next() {
		v, err := scanVulnAdvisory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// VulnAdvisory returns one stored row of advisory id, preferring the row of
// ecosystem and pkg; ErrNotFound when the advisory is not stored.
func (d *DB) VulnAdvisory(ctx context.Context, id, ecosystem, pkg string) (VulnAdvisory, error) {
	return scanVulnAdvisory(d.sql.QueryRowContext(ctx, `SELECT `+vulnCols+` FROM vuln_advisories WHERE id = ?
ORDER BY CASE WHEN ecosystem = ? AND pkg = ? THEN 0 ELSE 1 END LIMIT 1`, id, ecosystem, pkg))
}

// WalkVulnPackages calls fn with every (ecosystem, package) that has a
// stored advisory, holding one row at a time.
func (d *DB) WalkVulnPackages(ctx context.Context, fn func(ecosystem, pkg string)) error {
	rows, err := d.sql.QueryContext(ctx, `SELECT DISTINCT ecosystem, pkg FROM vuln_advisories`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e, p string
		if err := rows.Scan(&e, &p); err != nil {
			return err
		}
		fn(e, p)
	}
	return rows.Err()
}

// VulnStats counts the stored advisories per ecosystem and the malicious
// ones (MAL- ids) among them.
func (d *DB) VulnStats(ctx context.Context) (map[string]int64, int64, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT ecosystem, COUNT(DISTINCT id), COUNT(DISTINCT CASE WHEN malicious = 1 THEN id END)
FROM vuln_advisories GROUP BY ecosystem`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := map[string]int64{}
	var mal int64
	for rows.Next() {
		var e string
		var n, m int64
		if err := rows.Scan(&e, &n, &m); err != nil {
			return nil, 0, err
		}
		out[e], mal = n, mal+m
	}
	return out, mal, rows.Err()
}

// VulnQuery is the online API's answer for one package version.
type VulnQuery struct {
	Ecosystem, Package, Version string
	IDs                         []string
	Checked                     time.Time
}

// GetVulnQuery returns the stored answer for a package version; false when
// there is none.
func (d *DB) GetVulnQuery(ctx context.Context, ecosystem, pkg, version string) (VulnQuery, bool, error) {
	q := VulnQuery{Ecosystem: ecosystem, Package: pkg, Version: version}
	var ids string
	var at int64
	err := d.sql.QueryRowContext(ctx, `SELECT ids, checked_at FROM vuln_queries WHERE ecosystem = ? AND pkg = ? AND version = ?`,
		ecosystem, pkg, version).Scan(&ids, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return q, false, nil
	}
	if err != nil {
		return q, false, err
	}
	_ = json.Unmarshal([]byte(ids), &q.IDs)
	q.Checked = fromMs(at)
	return q, true, nil
}

// PutVulnQueries stores online answers in one transaction.
func (d *DB) PutVulnQueries(ctx context.Context, qs []VulnQuery) error {
	return d.Tx(ctx, func(tx *Tx) error {
		for _, q := range qs {
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO vuln_queries(ecosystem, pkg, version, ids, checked_at) VALUES(?,?,?,?,?)
ON CONFLICT(ecosystem, pkg, version) DO UPDATE SET ids = excluded.ids, checked_at = excluded.checked_at`,
				q.Ecosystem, q.Package, q.Version, jsonList(q.IDs), toMs(q.Checked)); err != nil {
				return err
			}
		}
		return nil
	})
}

// VulnFinding is what a scan found for one component of a repository.
type VulnFinding struct {
	Repo, Ecosystem, Package, Version string
	Name                              string // the component as name@version shows it
	IDs                               []string
	Severity                          string // the highest of its advisories; "" when none
	Rank                              int    // Severity's rank: critical 4 ... low 1, otherwise 0
	Malicious                         bool
	Unchecked                         bool // the online lookup failed: local data only
	Scanned                           time.Time
}

// PutVulnFindings stores scan results in one transaction.
func (d *DB) PutVulnFindings(ctx context.Context, fs []VulnFinding) error {
	return d.Tx(ctx, func(tx *Tx) error {
		for _, f := range fs {
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO vuln_findings(repo, ecosystem, pkg, version, name, ids, vulns, severity, sev_rank, malicious, unchecked, scanned_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(repo, ecosystem, pkg, version) DO UPDATE SET name = excluded.name, ids = excluded.ids, vulns = excluded.vulns,
  severity = excluded.severity, sev_rank = excluded.sev_rank, malicious = excluded.malicious, unchecked = excluded.unchecked,
  scanned_at = excluded.scanned_at`,
				f.Repo, f.Ecosystem, f.Package, f.Version, f.Name, jsonList(f.IDs), len(f.IDs), f.Severity, f.Rank,
				boolInt(f.Malicious), boolInt(f.Unchecked), toMs(f.Scanned)); err != nil {
				return err
			}
		}
		return nil
	})
}

// PruneVulnFindings deletes the findings of repo scanned before t: the
// components a finished scan no longer saw.
func (d *DB) PruneVulnFindings(ctx context.Context, repo string, before time.Time) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM vuln_findings WHERE repo = ? AND scanned_at < ?`, repo, toMs(before))
	return err
}

// VulnCounts sums a repository's findings.
type VulnCounts struct {
	Components, Vulnerable, Malicious, Unchecked int64
	Critical, High, Medium, Low, Other           int64 // vulnerable components by their highest severity
	Scanned                                      time.Time
}

// VulnCountsByRepo sums the findings of every scanned repository, or of
// repo alone unless it is "".
func (d *DB) VulnCountsByRepo(ctx context.Context, repo string) (map[string]VulnCounts, error) {
	where, args := "", []any{}
	if repo != "" {
		where, args = "WHERE repo = ? ", []any{repo}
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT repo, COUNT(*),
  SUM(CASE WHEN vulns > 0 THEN 1 ELSE 0 END), SUM(malicious), SUM(unchecked),
  SUM(CASE WHEN vulns > 0 AND sev_rank = 4 THEN 1 ELSE 0 END), SUM(CASE WHEN vulns > 0 AND sev_rank = 3 THEN 1 ELSE 0 END),
  SUM(CASE WHEN vulns > 0 AND sev_rank = 2 THEN 1 ELSE 0 END), SUM(CASE WHEN vulns > 0 AND sev_rank = 1 THEN 1 ELSE 0 END),
  SUM(CASE WHEN vulns > 0 AND sev_rank = 0 THEN 1 ELSE 0 END), MAX(scanned_at)
FROM vuln_findings `+where+`GROUP BY repo`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]VulnCounts{}
	for rows.Next() {
		var repo string
		var c VulnCounts
		var at int64
		if err := rows.Scan(&repo, &c.Components, &c.Vulnerable, &c.Malicious, &c.Unchecked,
			&c.Critical, &c.High, &c.Medium, &c.Low, &c.Other, &at); err != nil {
			return nil, err
		}
		c.Scanned = fromMs(at)
		out[repo] = c
	}
	return out, rows.Err()
}

// TopVulnFindings returns repo's vulnerable components, malicious ones
// first, then by severity and number of advisories.
func (d *DB) TopVulnFindings(ctx context.Context, repo string, limit int) ([]VulnFinding, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT ecosystem, pkg, version, name, ids, severity, sev_rank, malicious, unchecked, scanned_at
FROM vuln_findings WHERE repo = ? AND vulns > 0 ORDER BY malicious DESC, sev_rank DESC, vulns DESC, name LIMIT ?`, repo, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VulnFinding
	for rows.Next() {
		f := VulnFinding{Repo: repo}
		var ids string
		var mal, un int
		var at int64
		if err := rows.Scan(&f.Ecosystem, &f.Package, &f.Version, &f.Name, &ids, &f.Severity, &f.Rank, &mal, &un, &at); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(ids), &f.IDs)
		f.Malicious, f.Unchecked, f.Scanned = mal != 0, un != 0, fromMs(at)
		out = append(out, f)
	}
	return out, rows.Err()
}

// QuarantineItem is a component a proxy's firewall refused.
type QuarantineItem struct {
	Repo, Name, Version string
	Ecosystem, Path     string // Path: the first file refused
	IDs                 []string
	Severity            string
	Malicious           bool
	First, Last         time.Time
	Count               int64
}

// Quarantine records a refusal: a new row, or the existing one's time,
// count and advisories updated. created reports a new row.
func (d *DB) Quarantine(ctx context.Context, it QuarantineItem) (created bool, err error) {
	err = d.Tx(ctx, func(tx *Tx) error {
		var one int
		err := tx.q.QueryRowContext(ctx, `SELECT 1 FROM firewall_quarantine WHERE repo = ? AND name = ? AND version = ?`,
			it.Repo, it.Name, it.Version).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			created = true
			_, err = tx.q.ExecContext(ctx, `INSERT INTO firewall_quarantine(repo, name, version, ecosystem, path, ids, severity, malicious, first_at, last_at, hits)
VALUES(?,?,?,?,?,?,?,?,?,?,1)`, it.Repo, it.Name, it.Version, it.Ecosystem, it.Path, jsonList(it.IDs), it.Severity,
				boolInt(it.Malicious), toMs(it.Last), toMs(it.Last))
			return err
		}
		if err != nil {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `UPDATE firewall_quarantine SET ids = ?, severity = ?, malicious = ?, last_at = ?, hits = hits + 1
WHERE repo = ? AND name = ? AND version = ?`, jsonList(it.IDs), it.Severity, boolInt(it.Malicious), toMs(it.Last), it.Repo, it.Name, it.Version)
		return err
	})
	return created, err
}

// ListQuarantine returns the quarantined components, of repo only unless
// it is "", most recently refused first, at most limit.
func (d *DB) ListQuarantine(ctx context.Context, repo string, limit int) ([]QuarantineItem, error) {
	q := `SELECT repo, name, version, ecosystem, path, ids, severity, malicious, first_at, last_at, hits FROM firewall_quarantine`
	args := []any{}
	if repo != "" {
		q += ` WHERE repo = ?`
		args = append(args, repo)
	}
	rows, err := d.sql.QueryContext(ctx, q+` ORDER BY last_at DESC, repo, name, version LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuarantineItem
	for rows.Next() {
		var it QuarantineItem
		var ids string
		var mal int
		var first, last int64
		if err := rows.Scan(&it.Repo, &it.Name, &it.Version, &it.Ecosystem, &it.Path, &ids, &it.Severity, &mal, &first, &last, &it.Count); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(ids), &it.IDs)
		it.Malicious, it.First, it.Last = mal != 0, fromMs(first), fromMs(last)
		out = append(out, it)
	}
	return out, rows.Err()
}

// DeleteQuarantine removes one quarantined component; ErrNotFound when it
// is not quarantined.
func (d *DB) DeleteQuarantine(ctx context.Context, repo, name, version string) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM firewall_quarantine WHERE repo = ? AND name = ? AND version = ?`, repo, name, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
