package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Task is one scheduled task (plan 11 Decision 7). Type, Schedule and
// Params are owned and validated by internal/tasks.
type Task struct {
	ID, Name, Type, Schedule string
	Params                   string   // JSON
	NotifyEmail              []string // addresses mailed when a run fails
	Enabled                  bool
	Created, Updated         time.Time

	// The last run; LastLog is empty in ListTasks results.
	LastRun        time.Time
	LastResult     string // "", "OK" or "Failed"
	LastDurationMs int64
	LastMessage    string
	LastLog        string
	LastRemoved    int64 // blobs (compact) or components (cleanup) removed
	LastBytes      int64 // bytes removed (cleanup)

	NextRun time.Time // zero: not scheduled
}

// TaskRun is the outcome of one run, stored by RecordTaskRun.
type TaskRun struct {
	Run            time.Time
	Result         string
	DurationMs     int64
	Message, Log   string
	Removed, Bytes int64
}

// MaxTasks is how many tasks may exist: CreateTask refuses more
// (ErrTaskLimit). A variable so tests can lower it.
var MaxTasks = 100

// ErrTaskLimit is returned when MaxTasks tasks exist already.
var ErrTaskLimit = errors.New("too many tasks")

// taskSelect selects every task column; without withLog, last_log reads ”.
func taskSelect(withLog bool) string {
	logCol := `''`
	if withLog {
		logCol = `last_log`
	}
	return `SELECT id, name, type, schedule, params, enabled, created_at, updated_at,
  last_run, last_result, last_duration_ms, last_message, ` + logCol + `, last_removed, last_bytes, next_run, notify_email FROM tasks`
}

func scanTask(row scanner) (Task, error) {
	var t Task
	var enabled int
	var created, updated, lastRun, nextRun int64
	var notify string
	err := row.Scan(&t.ID, &t.Name, &t.Type, &t.Schedule, &t.Params, &enabled, &created, &updated,
		&lastRun, &t.LastResult, &t.LastDurationMs, &t.LastMessage, &t.LastLog, &t.LastRemoved, &t.LastBytes, &nextRun, &notify)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, err
	}
	t.Enabled = enabled != 0
	t.NotifyEmail = []string{}
	if notify != "" {
		if err := json.Unmarshal([]byte(notify), &t.NotifyEmail); err != nil || t.NotifyEmail == nil {
			t.NotifyEmail = []string{}
		}
	}
	t.Created, t.Updated, t.LastRun, t.NextRun = fromMs(created), fromMs(updated), fromMs(lastRun), fromMs(nextRun)
	return t, nil
}

func queryTasks(ctx context.Context, q querier, query string, args ...any) ([]Task, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListTasks returns every task ordered by name and id, without LastLog.
func (d *DB) ListTasks(ctx context.Context) ([]Task, error) {
	return queryTasks(ctx, d.sql, taskSelect(false)+` ORDER BY name, id LIMIT ?`, MaxTasks)
}

// DueTasks returns the enabled tasks whose next run is set and not after
// now, oldest first, without LastLog.
func (d *DB) DueTasks(ctx context.Context, now time.Time) ([]Task, error) {
	return queryTasks(ctx, d.sql, taskSelect(false)+` WHERE enabled = 1 AND next_run > 0 AND next_run <= ?
ORDER BY next_run, id LIMIT ?`, toMs(now), MaxTasks)
}

// GetTask returns the task with its last log, or ErrNotFound.
func (d *DB) GetTask(ctx context.Context, id string) (Task, error) {
	return scanTask(d.sql.QueryRowContext(ctx, taskSelect(true)+` WHERE id = ?`, id))
}

func notifyJSON(addrs []string) string {
	if addrs == nil {
		addrs = []string{}
	}
	b, _ := json.Marshal(addrs)
	return string(b)
}

func insertTask(ctx context.Context, q querier, t Task) error {
	now := toMs(time.Now())
	_, err := q.ExecContext(ctx, `INSERT INTO tasks(id, name, type, schedule, params, enabled, created_at, updated_at, next_run, notify_email)
VALUES(?,?,?,?,?,?,?,?,?,?)`, t.ID, t.Name, t.Type, t.Schedule, t.Params, boolInt(t.Enabled), now, now, toMs(t.NextRun), notifyJSON(t.NotifyEmail))
	return err
}

// CreateTask stores a new task; ErrExists if the id is taken, ErrTaskLimit
// when MaxTasks exist. The last-run fields start empty.
func (d *DB) CreateTask(ctx context.Context, t Task) error {
	return d.Tx(ctx, func(tx *Tx) error {
		var n, exists int
		if err := tx.q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(id = ?), 0) FROM tasks`, t.ID).Scan(&n, &exists); err != nil {
			return err
		}
		switch {
		case exists > 0:
			return ErrExists
		case n >= MaxTasks:
			return ErrTaskLimit
		}
		return insertTask(ctx, tx.q, t)
	})
}

// UpdateTask replaces the name, schedule, params, enabled flag and next run
// of the task t.ID; ErrNotFound if there is none. The type and the last run
// are kept.
func (d *DB) UpdateTask(ctx context.Context, t Task) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE tasks SET name = ?, schedule = ?, params = ?, enabled = ?, next_run = ?, notify_email = ?, updated_at = ?
WHERE id = ?`, t.Name, t.Schedule, t.Params, boolInt(t.Enabled), toMs(t.NextRun), notifyJSON(t.NotifyEmail), toMs(time.Now()), t.ID)
	return affected(res, err)
}

// RecordTaskRun stores the outcome of a run; ErrNotFound if the task was
// deleted meanwhile. With next set, the next run is recomputed in the same
// transaction from the task as stored now, so an edit made during the run
// is not overwritten; next returning false (and a nil next) keeps next_run.
func (d *DB) RecordTaskRun(ctx context.Context, id string, r TaskRun, next func(Task) (time.Time, bool)) error {
	return d.Tx(ctx, func(tx *Tx) error {
		cur, err := scanTask(tx.q.QueryRowContext(ctx, taskSelect(false)+` WHERE id = ?`, id))
		if err != nil {
			return err
		}
		nextRun := cur.NextRun
		if next != nil {
			if t, ok := next(cur); ok {
				nextRun = t
			}
		}
		res, err := tx.q.ExecContext(ctx, `UPDATE tasks SET last_run = ?, last_result = ?, last_duration_ms = ?, last_message = ?,
  last_log = ?, last_removed = ?, last_bytes = ?, next_run = ? WHERE id = ?`,
			toMs(r.Run), r.Result, r.DurationMs, r.Message, r.Log, r.Removed, r.Bytes, toMs(nextRun), id)
		return affected(res, err)
	})
}

// DeleteTask removes the task; ErrNotFound if there is none.
func (d *DB) DeleteTask(ctx context.Context, id string) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id)
	return affected(res, err)
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SeedTask inserts t once per database, in one transaction: only when the
// settings key flag is not set and no task exists. The flag is set either
// way, so a seed task the admin deleted does not come back. It reports
// whether it inserted t.
func (d *DB) SeedTask(ctx context.Context, flag string, t Task) (bool, error) {
	seeded := false
	err := d.Tx(ctx, func(tx *Tx) error {
		var v string
		err := tx.q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, flag).Scan(&v)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var n int
		if err := tx.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if err := insertTask(ctx, tx.q, t); err != nil {
				return err
			}
			seeded = true
		}
		_, err = tx.q.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?)`, flag, time.Now().UTC().Format(time.RFC3339))
		return err
	})
	return seeded && err == nil, err
}
