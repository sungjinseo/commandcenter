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

// RequestDelta is the traffic of one repository in one UTC hour (Hour is unix seconds).
type RequestDelta struct {
	Hour            int64
	Repo            string
	Requests, Bytes int64
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
			if _, err := tx.q.ExecContext(ctx, `UPDATE assets SET downloads = downloads + ?, last_downloaded = max(last_downloaded, ?)
WHERE repo = ? AND path = ?`, b.N, toMs(b.Last), b.Repo, b.Path); err != nil {
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
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO request_stats(hour, repo, requests, bytes) VALUES(?,?,?,?)
ON CONFLICT(hour, repo) DO UPDATE SET requests = requests + excluded.requests, bytes = bytes + excluded.bytes`,
				b.Hour, b.Repo, b.Requests, b.Bytes); err != nil {
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

// PruneRequestStats deletes the statistics of hours before the cutoff (unix
// seconds) and returns how many rows went.
func (d *DB) PruneRequestStats(ctx context.Context, before int64) (int64, error) {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM request_stats WHERE hour < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
