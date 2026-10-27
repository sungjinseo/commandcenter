package meta

import (
	"context"
	"time"
)

// DownloadDelta is the number of downloads of one asset since the last flush.
type DownloadDelta struct {
	Repo, Path string
	N          int64
	Last       time.Time
}

// RequestDelta is the traffic of one repository in one UTC hour (Hour is unix
// seconds): requests with their response bytes and 5xx/429 errors, request
// body bytes, total and longest response times, the proxy's remote fetches
// (count, failures, total and longest time) and its cache hits and misses.
type RequestDelta struct {
	Hour                                                  int64
	Repo                                                  string
	Requests, Bytes, Errors, BytesIn, Ms, MaxMs           int64
	Upstream, UpstreamFailures, UpstreamMs, UpstreamMaxMs int64
	CacheHits, CacheMisses                                int64
}

// requestStatCols are the counters of request_stats, in RequestDelta order.
const requestStatCols = `requests, bytes, errors, bytes_in, ms, max_ms,
  upstream, upstream_failures, upstream_ms, upstream_max_ms, cache_hits, cache_misses`

// RepoTraffic is the traffic of one repository over a period (RequestDelta summed).
type RepoTraffic struct {
	Repo                                                  string
	Requests, Bytes, Errors, BytesIn, Ms, MaxMs           int64
	Upstream, UpstreamFailures, UpstreamMs, UpstreamMaxMs int64
	CacheHits, CacheMisses                                int64
}

// RequestBucket is the traffic in [T, T+step), T in unix seconds.
type RequestBucket struct {
	T               int64
	Requests, Bytes int64
}

// FlushDownloads adds the deltas to the assets' counters in one transaction.
// last_downloaded only moves forward; assets that no longer exist are skipped.
func (d *DB) FlushDownloads(ctx context.Context, batch []DownloadDelta) error {
	if len(batch) == 0 {
		return nil
	}
	return d.Tx(ctx, func(tx *Tx) error {
		for _, b := range batch {
			if _, err := tx.q.ExecContext(ctx, pg(tx.q, `UPDATE assets SET downloads = downloads + ?, last_downloaded = max(last_downloaded, ?)
WHERE repo = ? AND path = ?`, `UPDATE assets SET downloads = downloads + ?, last_downloaded = GREATEST(last_downloaded, ?)
WHERE repo = ? AND path = ?`), b.N, toMs(b.Last), b.Repo, b.Path); err != nil {
				return err
			}
		}
		return nil
	})
}

// FlushRequests adds the deltas to the hourly request statistics in one transaction.
func (d *DB) FlushRequests(ctx context.Context, batch []RequestDelta) error {
	if len(batch) == 0 {
		return nil
	}
	return d.Tx(ctx, func(tx *Tx) error {
		for _, b := range batch {
			const add = `requests = request_stats.requests + excluded.requests, bytes = request_stats.bytes + excluded.bytes,
errors = request_stats.errors + excluded.errors, bytes_in = request_stats.bytes_in + excluded.bytes_in,
ms = request_stats.ms + excluded.ms, upstream = request_stats.upstream + excluded.upstream,
upstream_failures = request_stats.upstream_failures + excluded.upstream_failures, upstream_ms = request_stats.upstream_ms + excluded.upstream_ms,
cache_hits = request_stats.cache_hits + excluded.cache_hits, cache_misses = request_stats.cache_misses + excluded.cache_misses, `
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO request_stats(hour, repo, `+requestStatCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(hour, repo) DO UPDATE SET `+add+pg(tx.q,
				`max_ms = max(request_stats.max_ms, excluded.max_ms), upstream_max_ms = max(request_stats.upstream_max_ms, excluded.upstream_max_ms)`,
				`max_ms = GREATEST(request_stats.max_ms, excluded.max_ms), upstream_max_ms = GREATEST(request_stats.upstream_max_ms, excluded.upstream_max_ms)`),
				b.Hour, b.Repo, b.Requests, b.Bytes, b.Errors, b.BytesIn, b.Ms, b.MaxMs,
				b.Upstream, b.UpstreamFailures, b.UpstreamMs, b.UpstreamMaxMs, b.CacheHits, b.CacheMisses); err != nil {
				return err
			}
		}
		return nil
	})
}

// RequestStats sums the statistics with from <= hour < to into buckets of step
// seconds (3600 or 86400), oldest first, for one repository or, when repo is
// empty, all of them. Buckets without traffic are not returned.
func (d *DB) RequestStats(ctx context.Context, from, to, step int64, repo string) ([]RequestBucket, error) {
	q := `SELECT (hour / ?) * ? AS t, SUM(requests), SUM(bytes) FROM request_stats WHERE hour >= ? AND hour < ?`
	args := []any{step, step, from, to}
	if repo != "" {
		q += ` AND repo = ?`
		args = append(args, repo)
	}
	rows, err := d.sql.QueryContext(ctx, q+` GROUP BY t ORDER BY t`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RequestBucket
	for rows.Next() {
		var b RequestBucket
		if err := rows.Scan(&b.T, &b.Requests, &b.Bytes); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RepoTrafficStats sums the statistics with from <= hour < to per repository, by
// name. Repositories without traffic are not returned.
func (d *DB) RepoTrafficStats(ctx context.Context, from, to int64) ([]RepoTraffic, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT repo, SUM(requests), SUM(bytes), SUM(errors), SUM(bytes_in), SUM(ms), MAX(max_ms),
SUM(upstream), SUM(upstream_failures), SUM(upstream_ms), MAX(upstream_max_ms), SUM(cache_hits), SUM(cache_misses)
FROM request_stats WHERE hour >= ? AND hour < ? GROUP BY repo ORDER BY repo`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoTraffic
	for rows.Next() {
		var s RepoTraffic
		if err := rows.Scan(&s.Repo, &s.Requests, &s.Bytes, &s.Errors, &s.BytesIn, &s.Ms, &s.MaxMs,
			&s.Upstream, &s.UpstreamFailures, &s.UpstreamMs, &s.UpstreamMaxMs, &s.CacheHits, &s.CacheMisses); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RepoTrafficPoint is one repository's requests in [T, T+step).
type RepoTrafficPoint struct {
	Repo        string
	T, Requests int64
}

// RepoTrafficSeries buckets the requests of every repository with from <=
// hour < to into step seconds (3600 or 86400), by repository then time.
// Buckets without traffic are not returned.
func (d *DB) RepoTrafficSeries(ctx context.Context, from, to, step int64) ([]RepoTrafficPoint, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT repo, (hour / ?) * ? AS t, SUM(requests) FROM request_stats
WHERE hour >= ? AND hour < ? GROUP BY repo, t ORDER BY repo, t`, step, step, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoTrafficPoint
	for rows.Next() {
		var p RepoTrafficPoint
		if err := rows.Scan(&p.Repo, &p.T, &p.Requests); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PruneRequestStats deletes the statistics of hours before the cutoff (unix
// seconds) and returns how many rows went.
func (d *DB) PruneRequestStats(ctx context.Context, before int64) (int64, error) {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM request_stats WHERE hour < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
