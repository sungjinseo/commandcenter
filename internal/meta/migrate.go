package meta

import (
	"context"
	"fmt"
	"strings"
)

// copyTables lists every table with its columns, in the order Copy fills
// them. TestCopyTablesCoverSchema keeps it complete: a new table or column
// goes here too.
var copyTables = []struct{ name, cols string }{
	{"repositories", "name, format, type, config, created_at, updated_at"},
	{"blob_stores", "name, path, created_at, type, config"},
	{"users", "id, name, email, password_hash, source, status, roles, created_at, updated_at"},
	{"roles", "id, description, privileges, created_at, updated_at"},
	{"settings", "key, value"},
	{"assets", assetSelect},
	{"packages", "repo, format, name, version, attrs, yanked, created_at"},
	{"dist_tags", "repo, name, tag, version"},
	{"negative_cache", "repo, path, expires_at"},
	{"tokens", `id, "user", hash, created_at, last_used_at`},
	{"request_stats", "hour, repo, " + requestStatCols},
	{"events", "id, at, kind, actor, repo, subject, detail"},
	{"cleanup_policies", "name, format, config, created_at, updated_at"},
	{"tasks", "id, name, type, schedule, params, enabled, created_at, updated_at, last_run, last_result, " +
		"last_duration_ms, last_message, last_log, next_run, last_removed, last_bytes, notify_email"},
	{"webhooks", "id, name, url, secret, events, repo, enabled, created_at, updated_at"},
	{"webhook_deliveries", "id, webhook_id, event, status, attempts, last_error, created_at, next_at, response_code, payload"},
	{"routing_rules", "name, description, mode, matchers, created_at, updated_at"},
	{"content_selectors", "name, description, expression, created_at, updated_at"},
}

// copyBatchParams bounds the bind parameters of one INSERT (SQLite's
// historical limit is 999; PostgreSQL's 65535).
const copyBatchParams = 999

// TableCount is how many rows Copy wrote to one table.
type TableCount struct {
	Table string
	Rows  int64
}

// Copy copies every table of src into dst, which must be at the same schema
// version and empty (both as Open left them), in one transaction on dst and
// from one snapshot of src: ids, timestamps and text (JSON documents, sealed
// secrets) are copied as stored. It verifies the row count of every table
// before committing. Errors never hold a URL.
func Copy(ctx context.Context, src, dst *DB) ([]TableCount, error) {
	sv, err := schemaVersionOf(ctx, src.sql)
	if err != nil {
		return nil, err
	}
	dv, err := schemaVersionOf(ctx, dst.sql)
	if err != nil {
		return nil, err
	}
	if sv != dv {
		return nil, fmt.Errorf("schema version mismatch: source %d, target %d (open both with the same version)", sv, dv)
	}
	rtx, err := src.sql.begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer rtx.Rollback()
	rq := txConn{tx: rtx, pg: src.sql.pg}
	wtx, err := dst.sql.begin(ctx, true)
	if err != nil {
		return nil, err
	}
	defer wtx.Rollback()
	wq := txConn{tx: wtx, pg: dst.sql.pg}
	for _, t := range copyTables {
		if n, err := countRows(ctx, wq, t.name); err != nil {
			return nil, err
		} else if n > 0 {
			return nil, fmt.Errorf("the target is not empty: table %s has %d rows (migrate into a new, empty database)", t.name, n)
		}
	}
	var out []TableCount
	for _, t := range copyTables {
		n, err := copyTable(ctx, rq, wq, t.name, t.cols)
		if err != nil {
			return nil, fmt.Errorf("copy %s: %w", t.name, err)
		}
		want, err := countRows(ctx, rq, t.name)
		if err != nil {
			return nil, err
		}
		got, err := countRows(ctx, wq, t.name)
		if err != nil {
			return nil, err
		}
		if n != want || got != want {
			return nil, fmt.Errorf("verify %s: source has %d rows, copied %d, target has %d (nothing was committed)", t.name, want, n, got)
		}
		out = append(out, TableCount{t.name, n})
	}
	if wq.pg {
		// events.id came from the source; the identity sequence must continue after it.
		if _, err := wq.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence('events', 'id'), (SELECT MAX(id) FROM events)) WHERE EXISTS (SELECT 1 FROM events)`); err != nil {
			return nil, err
		}
	}
	return out, wtx.Commit()
}

func countRows(ctx context.Context, q querier, table string) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n)
	return n, err
}

// copyTable streams the rows of table from rq into wq in batched INSERTs
// and returns how many rows it inserted. Values pass through as the driver
// returns them (int64 for integers, string for text), so nothing is reformatted.
func copyTable(ctx context.Context, rq, wq txConn, table, cols string) (int64, error) {
	ncols := strings.Count(cols, ",") + 1
	per := max(1, copyBatchParams/ncols)
	rowPH := "(" + strings.Repeat("?,", ncols-1) + "?)"
	insert := func(args []any) (int64, error) {
		rows := len(args) / ncols
		q := `INSERT INTO ` + table + `(` + cols + `) VALUES ` + strings.Repeat(rowPH+",", rows-1) + rowPH
		res, err := wq.ExecContext(ctx, q, args...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	rows, err := rq.QueryContext(ctx, `SELECT `+cols+` FROM `+table)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var total int64
	args := make([]any, 0, per*ncols)
	vals := make([]any, ncols)
	ptrs := make([]any, ncols)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return 0, err
		}
		args = append(args, vals...)
		if len(args) == per*ncols {
			n, err := insert(args)
			if err != nil {
				return 0, err
			}
			total += n
			args = args[:0]
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(args) > 0 {
		n, err := insert(args)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}
