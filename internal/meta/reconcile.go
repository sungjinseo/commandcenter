package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Recovery mode and the reconcile plans (the reconcile journal is
// journal.go's).

// Settings keys.
const (
	SettingRecoveryMode = "recoveryMode"         // RecoveryMode
	SettingJournalDays  = "reconcileJournalDays" // int: days the reconcile journal is kept
)

// DefaultJournalDays is how long the reconcile journal is kept unless
// SettingJournalDays says otherwise; MaxJournalDays bounds the setting.
const (
	DefaultJournalDays = 30
	MaxJournalDays     = 3650
)

// RecoveryTopic is the cluster notification a recovery mode change sends.
const RecoveryTopic = "recovery"

// RecoveryMode is the cluster-wide recovery mode: while it is on, clients
// may read but not upload or delete, and compaction, cleanup and the other
// tasks that delete or move content do not run, so that an administrator
// can restore the database and reconcile it with the blob stores.
type RecoveryMode struct {
	Enabled bool      `json:"enabled"`
	Since   time.Time `json:"since,omitzero"`    // when it was turned on
	By      string    `json:"by,omitempty"`      // who turned it on
	Message string    `json:"message,omitempty"` // the administrator's note, shown in the refusals
}

// Recovery returns the recovery mode as this node last read or set it
// (LoadRecoveryMode, SetRecoveryMode): cheap enough for every request.
func (d *DB) Recovery() RecoveryMode {
	if rm := d.recovery.Load(); rm != nil {
		return *rm
	}
	return RecoveryMode{}
}

// LoadRecoveryMode reads the stored recovery mode and caches it for
// Recovery.
func (d *DB) LoadRecoveryMode(ctx context.Context) (RecoveryMode, error) {
	var rm RecoveryMode
	if _, err := d.GetSettingJSON(ctx, SettingRecoveryMode, &rm); err != nil {
		return RecoveryMode{}, err
	}
	d.recovery.Store(&rm)
	return rm, nil
}

// SetRecoveryMode stores rm, caches it and tells the other nodes
// (RecoveryTopic), which read it again; a node the notification misses
// reads it within its next reload (app.follow).
func (d *DB) SetRecoveryMode(ctx context.Context, rm RecoveryMode) error {
	if len(rm.Message) > 500 {
		return fmt.Errorf("the recovery mode message takes at most 500 characters")
	}
	if err := d.SetSettingJSON(ctx, SettingRecoveryMode, rm); err != nil {
		return err
	}
	d.recovery.Store(&rm)
	d.Notify(context.WithoutCancel(ctx), RecoveryTopic) // best effort, see above
	return nil
}

// JournalDays returns how many days the reconcile journal is kept.
func (d *DB) JournalDays(ctx context.Context) (int, error) {
	n := DefaultJournalDays
	_, err := d.GetSettingJSON(ctx, SettingJournalDays, &n)
	return n, err
}

// SetJournalDays sets how many days the reconcile journal is kept.
func (d *DB) SetJournalDays(ctx context.Context, n int) error {
	if n < 1 || n > MaxJournalDays {
		return fmt.Errorf("journalRetentionDays must be between 1 and %d", MaxJournalDays)
	}
	return d.SetSettingJSON(ctx, SettingJournalDays, n)
}

// Plan and item states, and item actions (ItemAdd...).
const (
	PlanPlanned  = "PLANNED"  // made, not executed
	PlanExecuted = "EXECUTED" // every ADD and DELETE applied
	PlanFailed   = "FAILED"   // executed, some items failed: executing again retries them

	ItemPlanned = "PLANNED" // an ADD or DELETE not applied yet
	ItemDone    = "DONE"
	ItemFailed  = "FAILED"
	ItemSkipped = "SKIPPED" // nothing to do: a SKIP or CONFLICT, or changed since planned

	ItemAdd      = "ADD"
	ItemDelete   = "DELETE"
	ItemSkip     = "SKIP"
	ItemConflict = "CONFLICT"
)

// ReconcilePlan is one run of blobstore.reconcile-plan.
type ReconcilePlan struct {
	ID        string
	BlobStore string // "" for every blob store
	State     string
	Since     time.Time // the journal window's start; zero: the whole journal
	Created   time.Time
	CreatedBy string
	Executed  time.Time // the last execution's end
	Summary   string    // JSON counts
}

// ReconcileItem is one row a plan adds, deletes or leaves.
type ReconcileItem struct {
	PlanID     string
	Seq        int64 // execution order
	Repo       string
	Kind       string // ChangeAsset, ChangePackage or ChangeTag
	Label      string // Change.Label
	Action     string
	State      string
	Reason     string
	BlobSHA256 string    // an asset's blob
	JournalKey string    // the journal record the item comes from
	At         time.Time // that record's time
	Change     string    // the journaled Change, JSON
}

// CreateReconcilePlan stores a new plan.
func (d *DB) CreateReconcilePlan(ctx context.Context, p ReconcilePlan) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO reconcile_plans(id, blob_store, state, since, created_at, created_by, executed_at, summary)
VALUES(?,?,?,?,?,?,?,?)`, p.ID, p.BlobStore, p.State, toMs(p.Since), toMs(p.Created), p.CreatedBy, toMs(p.Executed), orJSON(p.Summary))
	return err
}

func orJSON(s string) string {
	if s == "" {
		return "{}"
	}
	return s
}

// UpdateReconcilePlan sets a plan's state, summary and execution time;
// ErrNotFound when the plan is gone.
func (d *DB) UpdateReconcilePlan(ctx context.Context, id, state, summary string, executed time.Time) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE reconcile_plans SET state = ?, summary = ?, executed_at = ? WHERE id = ?`,
		state, orJSON(summary), toMs(executed), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const planCols = `id, blob_store, state, since, created_at, created_by, executed_at, summary`

func scanPlan(r scanner) (ReconcilePlan, error) {
	var p ReconcilePlan
	var since, created, executed int64
	err := r.Scan(&p.ID, &p.BlobStore, &p.State, &since, &created, &p.CreatedBy, &executed, &p.Summary)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	p.Since, p.Created, p.Executed = fromMs(since), fromMs(created), fromMs(executed)
	return p, err
}

// GetReconcilePlan returns one plan; ErrNotFound when there is none.
func (d *DB) GetReconcilePlan(ctx context.Context, id string) (ReconcilePlan, error) {
	return scanPlan(d.sql.QueryRowContext(ctx, `SELECT `+planCols+` FROM reconcile_plans WHERE id = ?`, id))
}

// ErrBadToken refuses a continuation token no listing made.
var ErrBadToken = errors.New("invalid continuationToken")

// ListReconcilePlans returns up to limit plans, newest first, after the
// position token (from the previous page; "" for the first) and the token
// of the next page ("" on the last).
func (d *DB) ListReconcilePlans(ctx context.Context, token string, limit int) ([]ReconcilePlan, string, error) {
	q, args := `SELECT `+planCols+` FROM reconcile_plans`, []any{}
	if token != "" {
		ms, id, ok := strings.Cut(token, ".")
		at, err := strconv.ParseInt(ms, 10, 64)
		if !ok || err != nil {
			return nil, "", ErrBadToken
		}
		q += ` WHERE (created_at < ? OR created_at = ? AND id > ?)`
		args = append(args, at, at, id)
	}
	rows, err := d.sql.QueryContext(ctx, q+` ORDER BY created_at DESC, id LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []ReconcilePlan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[limit-1]
		next = strconv.FormatInt(toMs(last.Created), 10) + "." + last.ID
	}
	return out, next, nil
}

// DeleteReconcilePlan deletes a plan and its items; ErrNotFound when there
// is none.
func (d *DB) DeleteReconcilePlan(ctx context.Context, id string) error {
	return d.Tx(ctx, func(t *Tx) error {
		if _, err := t.q.ExecContext(ctx, `DELETE FROM reconcile_items WHERE plan_id = ?`, id); err != nil {
			return err
		}
		res, err := t.q.ExecContext(ctx, `DELETE FROM reconcile_plans WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

const itemCols = `plan_id, seq, repo, kind, label, action, state, reason, blob_sha256, journal_key, at, change`

// itemBatch is how many items one INSERT writes (12 parameters each).
const itemBatch = 400

// AddReconcileItems stores items, in transactions of itemBatch rows.
func (d *DB) AddReconcileItems(ctx context.Context, items []ReconcileItem) error {
	for len(items) > 0 {
		n := min(len(items), itemBatch)
		var b strings.Builder
		args := make([]any, 0, n*12)
		b.WriteString(`INSERT INTO reconcile_items(` + itemCols + `) VALUES`)
		for i, it := range items[:n] {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`(?,?,?,?,?,?,?,?,?,?,?,?)`)
			args = append(args, it.PlanID, it.Seq, it.Repo, it.Kind, it.Label, it.Action, it.State, it.Reason,
				it.BlobSHA256, it.JournalKey, toMs(it.At), orJSON(it.Change))
		}
		if _, err := d.sql.ExecContext(ctx, b.String(), args...); err != nil {
			return err
		}
		items = items[n:]
	}
	return nil
}

// ItemFilter selects a plan's items: an empty field matches any value.
type ItemFilter struct {
	Action, State string
	States        []string // any of these states (with State empty)
}

// ReconcileItems returns up to limit items of a plan with a seq above after,
// in seq order.
func (d *DB) ReconcileItems(ctx context.Context, planID string, after int64, limit int, f ItemFilter) ([]ReconcileItem, error) {
	q, args := `SELECT `+itemCols+` FROM reconcile_items WHERE plan_id = ? AND seq > ?`, []any{planID, after}
	if f.Action != "" {
		q += ` AND action = ?`
		args = append(args, f.Action)
	}
	if f.State != "" {
		q += ` AND state = ?`
		args = append(args, f.State)
	}
	if len(f.States) > 0 {
		q += ` AND state IN (?` + strings.Repeat(`,?`, len(f.States)-1) + `)`
		for _, s := range f.States {
			args = append(args, s)
		}
	}
	rows, err := d.sql.QueryContext(ctx, q+` ORDER BY seq LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReconcileItem
	for rows.Next() {
		var it ReconcileItem
		var at int64
		if err := rows.Scan(&it.PlanID, &it.Seq, &it.Repo, &it.Kind, &it.Label, &it.Action, &it.State, &it.Reason,
			&it.BlobSHA256, &it.JournalKey, &at, &it.Change); err != nil {
			return nil, err
		}
		it.At = fromMs(at)
		out = append(out, it)
	}
	return out, rows.Err()
}

// ItemResult is what executing an item came to.
type ItemResult struct {
	Seq           int64
	State, Reason string
}

// MarkReconcileItems sets the state and reason of items of a plan in one
// transaction.
func (d *DB) MarkReconcileItems(ctx context.Context, planID string, results []ItemResult) error {
	if len(results) == 0 {
		return nil
	}
	return d.Tx(ctx, func(t *Tx) error {
		for _, r := range results {
			if _, err := t.q.ExecContext(ctx, `UPDATE reconcile_items SET state = ?, reason = ? WHERE plan_id = ? AND seq = ?`,
				r.State, r.Reason, planID, r.Seq); err != nil {
				return err
			}
		}
		return nil
	})
}

// CountReconcileItems counts a plan's items by action and state
// ("ADD/DONE": 3).
func (d *DB) CountReconcileItems(ctx context.Context, planID string) (map[string]int64, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT action, state, COUNT(*) FROM reconcile_items WHERE plan_id = ? GROUP BY action, state`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var a, s string
		var n int64
		if err := rows.Scan(&a, &s, &n); err != nil {
			return nil, err
		}
		out[a+"/"+s] = n
	}
	return out, rows.Err()
}

// PendingReconcileBlobs returns the blobs that items still to be added name:
// compaction keeps them.
// ponytail: one set in memory; a join in the GC's in-use query if plans
// grow to millions of pending items.
func (d *DB) PendingReconcileBlobs(ctx context.Context) (map[string]bool, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT DISTINCT blob_sha256 FROM reconcile_items
WHERE state IN ('PLANNED', 'FAILED') AND action = 'ADD' AND blob_sha256 <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out[sha] = true
	}
	return out, rows.Err()
}

// GetPackage returns one package version row; ErrNotFound when there is none.
func (d *DB) GetPackage(ctx context.Context, repo, name, version string) (Package, error) {
	return getPackage(ctx, d.sql, repo, name, version)
}

// GetPackage is DB.GetPackage inside the transaction.
func (t *Tx) GetPackage(ctx context.Context, repo, name, version string) (Package, error) {
	return getPackage(ctx, t.q, repo, name, version)
}

func getPackage(ctx context.Context, q querier, repo, name, version string) (Package, error) {
	p := Package{Repo: repo, Name: name, Version: version}
	var yanked int
	var created int64
	err := q.QueryRowContext(ctx, `SELECT format, attrs, yanked, created_at FROM packages WHERE repo = ? AND name = ? AND version = ?`,
		repo, name, version).Scan(&p.Format, &p.Attrs, &yanked, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Package{}, ErrNotFound
	}
	p.Yanked, p.CreatedAt = yanked != 0, fromMs(created)
	return p, err
}
