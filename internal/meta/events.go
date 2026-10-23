package meta

import (
	"context"
	"time"
)

// Event is one activity row (plan 11 Decision 3).
type Event struct {
	ID                                 int64 // 0 on insert
	At                                 time.Time
	Kind, Actor, Repo, Subject, Detail string
}

// InsertEvents stores the events in one transaction.
func (d *DB) InsertEvents(ctx context.Context, evs []Event) error {
	if len(evs) == 0 {
		return nil
	}
	return d.Tx(ctx, func(tx *Tx) error {
		for _, e := range evs {
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO events(at, kind, actor, repo, subject, detail) VALUES(?,?,?,?,?,?)`,
				toMs(e.At), e.Kind, e.Actor, e.Repo, e.Subject, e.Detail); err != nil {
				return err
			}
		}
		return nil
	})
}

// PruneEvents deletes everything but the newest keep events and returns how
// many rows it removed.
func (d *DB) PruneEvents(ctx context.Context, keep int) (int64, error) {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM events WHERE id <= (SELECT id FROM events ORDER BY id DESC LIMIT 1 OFFSET ?)`, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListEvents returns up to limit events, newest first. before > 0 returns only
// events with a smaller id (the next page); kind, when set, filters by kind.
func (d *DB) ListEvents(ctx context.Context, limit int, before int64, kind string) ([]Event, error) {
	q := `SELECT id, at, kind, actor, repo, subject, detail FROM events WHERE 1=1`
	var args []any
	if before > 0 {
		q += ` AND id < ?`
		args = append(args, before)
	}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Kind, &e.Actor, &e.Repo, &e.Subject, &e.Detail); err != nil {
			return nil, err
		}
		e.At = fromMs(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountEvents returns the number of stored events.
func (d *DB) CountEvents(ctx context.Context) (int64, error) {
	var n int64
	err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&n)
	return n, err
}
