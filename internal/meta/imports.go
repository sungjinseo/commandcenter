package meta

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ImportJob is one import job (plan 15 Decision 4): its parameters (never a
// password), its state and, once finished, its report (JSON).
type ImportJob struct {
	Job, Source, Params, Status, Actor string
	Started, Finished                  time.Time
	Report                             string // empty in ListImportJobs results
}

// Import asset states.
const (
	ImportDone    = "done"
	ImportSkipped = "skipped"
	ImportFailed  = "failed"
)

// PutImportJob inserts or replaces the job row.
func (d *DB) PutImportJob(ctx context.Context, j ImportJob) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO import_jobs(job, source, params, status, actor, started, finished, report)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(job) DO UPDATE SET source = excluded.source, params = excluded.params, status = excluded.status,
  actor = excluded.actor, started = excluded.started, finished = excluded.finished, report = excluded.report`,
		j.Job, j.Source, j.Params, j.Status, j.Actor, toMs(j.Started), toMs(j.Finished), j.Report)
	return err
}

func scanImportJob(row scanner) (ImportJob, error) {
	var j ImportJob
	var started, finished int64
	err := row.Scan(&j.Job, &j.Source, &j.Params, &j.Status, &j.Actor, &started, &finished, &j.Report)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportJob{}, ErrNotFound
	}
	j.Started, j.Finished = fromMs(started), fromMs(finished)
	return j, err
}

// GetImportJob returns the job with its report, or ErrNotFound.
func (d *DB) GetImportJob(ctx context.Context, job string) (ImportJob, error) {
	return scanImportJob(d.sql.QueryRowContext(ctx, `SELECT job, source, params, status, actor, started, finished, report
FROM import_jobs WHERE job = ?`, job))
}

// ListImportJobs returns at most limit jobs, newest first, without reports.
func (d *DB) ListImportJobs(ctx context.Context, limit int) ([]ImportJob, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT job, source, params, status, actor, started, finished, ''
FROM import_jobs ORDER BY started DESC, job LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImportJob
	for rows.Next() {
		j, err := scanImportJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ImportState returns the recorded state of one source asset of a job:
// its status (ImportDone, ImportSkipped or ImportFailed) and the source sha1
// it had; ErrNotFound when the asset has no state yet.
func (d *DB) ImportState(ctx context.Context, job, repo, path string) (status, sha1 string, err error) {
	err = d.sql.QueryRowContext(ctx, `SELECT status, sha1 FROM import_state WHERE job = ? AND repo = ? AND path = ?`,
		job, repo, path).Scan(&status, &sha1)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return status, sha1, err
}

// SetImportState records the state of one source asset of a job.
func (d *DB) SetImportState(ctx context.Context, job, repo, path, sha1, status, errText string) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO import_state(job, repo, path, sha1, status, error) VALUES(?,?,?,?,?,?)
ON CONFLICT(job, repo, path) DO UPDATE SET sha1 = excluded.sha1, status = excluded.status, error = excluded.error`,
		job, repo, path, sha1, status, errText)
	return err
}

// ImportStateCounts returns how many assets of the job are in each state.
func (d *DB) ImportStateCounts(ctx context.Context, job string) (map[string]int64, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT status, COUNT(*) FROM import_state WHERE job = ? GROUP BY status`, job)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// ImportStateRow is one recorded source asset of a job.
type ImportStateRow struct {
	Repo, Path, SHA1, Status, Error string
}

// ListImportState returns at most limit asset states of the job, by
// repository and path.
func (d *DB) ListImportState(ctx context.Context, job string, limit int) ([]ImportStateRow, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT repo, path, sha1, status, error FROM import_state WHERE job = ?
ORDER BY repo, path LIMIT ?`, job, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImportStateRow
	for rows.Next() {
		var r ImportStateRow
		if err := rows.Scan(&r.Repo, &r.Path, &r.SHA1, &r.Status, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
