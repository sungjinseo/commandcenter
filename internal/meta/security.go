package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"commandcenter/internal/config"
	"commandcenter/internal/csel"
)

// Routing rules and content selectors (Nexus "security" features). Both are
// imported from config.yaml with users and roles on the first start
// (ImportAccess) and managed in the web UI afterwards.

// Routing rule modes.
const (
	RoutingBlock = "BLOCK"
	RoutingAllow = "ALLOW"
)

// Routing rule limits.
const (
	MaxRoutingMatchers  = 50
	MaxRoutingMatcherLn = 256
)

// SelectorPrefix starts a privilege target that names a content selector.
const SelectorPrefix = "selector:"

// securityNameRE is a routing rule or content selector name.
var securityNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// RoutingRule is a stored routing rule.
type RoutingRule struct {
	Name, Description, Mode string
	Matchers                []string
	CreatedAt, UpdatedAt    time.Time
}

// ContentSelector is a stored content selector.
type ContentSelector struct {
	Name, Description, Expression string
	CreatedAt, UpdatedAt          time.Time
}

// RuleInUseError refuses to delete a routing rule that proxies still name.
type RuleInUseError struct {
	Rule  string
	Repos []string // sorted
}

func (e *RuleInUseError) Error() string {
	return fmt.Sprintf("routing rule %q is used by repositories: %s", e.Rule, strings.Join(e.Repos, ", "))
}

// SelectorInUseError refuses to delete a content selector that roles still name.
type SelectorInUseError struct {
	Selector string
	Roles    []string // sorted
}

func (e *SelectorInUseError) Error() string {
	return fmt.Sprintf("content selector %q is used by roles: %s", e.Selector, strings.Join(e.Roles, ", "))
}

// ValidateRoutingRule checks a rule's name, mode and matchers; the errors
// list every problem.
func ValidateRoutingRule(r RoutingRule) []string {
	var errs []string
	if !securityNameRE.MatchString(r.Name) {
		errs = append(errs, fmt.Sprintf("invalid routing rule name %q: use 1-64 letters, digits, '.', '_' and '-'", r.Name))
	}
	if r.Mode != RoutingBlock && r.Mode != RoutingAllow {
		errs = append(errs, fmt.Sprintf("mode must be %s or %s", RoutingBlock, RoutingAllow))
	}
	if err := checkDescription(r.Description); err != "" {
		errs = append(errs, err)
	}
	switch {
	case len(r.Matchers) == 0:
		errs = append(errs, "at least one matcher is required")
	case len(r.Matchers) > MaxRoutingMatchers:
		errs = append(errs, fmt.Sprintf("at most %d matchers are allowed", MaxRoutingMatchers))
	}
	for i, m := range r.Matchers {
		switch {
		case m == "":
			errs = append(errs, fmt.Sprintf("matcher %d is empty", i+1))
		case len(m) > MaxRoutingMatcherLn:
			errs = append(errs, fmt.Sprintf("matcher %d is longer than %d characters", i+1, MaxRoutingMatcherLn))
		default:
			if _, err := regexp.Compile(m); err != nil {
				errs = append(errs, fmt.Sprintf("matcher %d: invalid regular expression: %v", i+1, err))
			}
		}
	}
	return errs
}

// ValidateContentSelector checks a selector's name and expression.
func ValidateContentSelector(s ContentSelector) []string {
	var errs []string
	if !securityNameRE.MatchString(s.Name) {
		errs = append(errs, fmt.Sprintf("invalid content selector name %q: use 1-64 letters, digits, '.', '_' and '-'", s.Name))
	}
	if err := checkDescription(s.Description); err != "" {
		errs = append(errs, err)
	}
	if _, err := csel.Parse(s.Expression); err != nil {
		errs = append(errs, "expression: "+err.Error())
	}
	return errs
}

func checkDescription(s string) string {
	switch {
	case len(s) > 200:
		return "description is longer than 200 characters"
	case strings.ContainsFunc(s, unicode.IsControl):
		return "description must not contain control characters"
	}
	return ""
}

// ---- routing rules ----

const ruleCols = `name, description, mode, matchers, created_at, updated_at`

// ListRoutingRules returns every rule, ordered by name.
func (d *DB) ListRoutingRules(ctx context.Context) ([]RoutingRule, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+ruleCols+` FROM routing_rules ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoutingRule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRoutingRule returns one rule; ErrNotFound if there is none.
func (d *DB) GetRoutingRule(ctx context.Context, name string) (RoutingRule, error) {
	return scanRule(d.sql.QueryRowContext(ctx, `SELECT `+ruleCols+` FROM routing_rules WHERE name = ?`, name))
}

func scanRule(row scanner) (RoutingRule, error) {
	var r RoutingRule
	var matchers string
	var created, updated int64
	err := row.Scan(&r.Name, &r.Description, &r.Mode, &matchers, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return RoutingRule{}, ErrNotFound
	}
	if err != nil {
		return RoutingRule{}, err
	}
	if err := json.Unmarshal([]byte(matchers), &r.Matchers); err != nil {
		return RoutingRule{}, fmt.Errorf("routing rule %q: decode matchers: %w", r.Name, err)
	}
	r.CreatedAt, r.UpdatedAt = fromMs(created), fromMs(updated)
	return r, nil
}

// CreateRoutingRule stores a new rule; ErrExists if the name is taken.
func (d *DB) CreateRoutingRule(ctx context.Context, r RoutingRule) error {
	return d.Tx(ctx, func(tx *Tx) error { return insertRule(ctx, tx.q, r) })
}

func insertRule(ctx context.Context, q querier, r RoutingRule) error {
	matchers, err := json.Marshal(r.Matchers)
	if err != nil {
		return err
	}
	var one int
	err = q.QueryRowContext(ctx, `SELECT 1 FROM routing_rules WHERE name = ?`, r.Name).Scan(&one)
	if err == nil {
		return ErrExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := toMs(time.Now())
	_, err = q.ExecContext(ctx, `INSERT INTO routing_rules(`+ruleCols+`) VALUES(?,?,?,?,?,?)`,
		r.Name, r.Description, r.Mode, string(matchers), now, now)
	return err
}

// UpdateRoutingRule replaces the description, mode and matchers of rule
// r.Name; ErrNotFound if there is none.
func (d *DB) UpdateRoutingRule(ctx context.Context, r RoutingRule) error {
	matchers, err := json.Marshal(r.Matchers)
	if err != nil {
		return err
	}
	res, err := d.sql.ExecContext(ctx, `UPDATE routing_rules SET description = ?, mode = ?, matchers = ?, updated_at = ? WHERE name = ?`,
		r.Description, r.Mode, string(matchers), toMs(time.Now()), r.Name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteRoutingRule removes the rule unless a stored repository names it
// (*RuleInUseError), in one transaction. ErrNotFound if there is none.
func (d *DB) DeleteRoutingRule(ctx context.Context, name string) error {
	return d.Tx(ctx, func(tx *Tx) error {
		users, err := reposUsingRule(ctx, tx.q, name)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			return &RuleInUseError{Rule: name, Repos: users}
		}
		res, err := tx.q.ExecContext(ctx, `DELETE FROM routing_rules WHERE name = ?`, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RepositoriesUsingRoutingRule returns the names of the stored repositories
// whose definition names the rule, sorted.
func (d *DB) RepositoriesUsingRoutingRule(ctx context.Context, name string) ([]string, error) {
	return reposUsingRule(ctx, d.sql, name)
}

func reposUsingRule(ctx context.Context, q querier, name string) ([]string, error) {
	rows, err := q.QueryContext(ctx, pg(q, `SELECT name FROM repositories WHERE json_extract(config, '$.routingRule') = ? ORDER BY name`,
		`SELECT name FROM repositories WHERE config::json ->> 'routingRule' = ? ORDER BY name`), name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---- content selectors ----

const selectorCols = `name, description, expression, created_at, updated_at`

// ListContentSelectors returns every selector, ordered by name.
func (d *DB) ListContentSelectors(ctx context.Context) ([]ContentSelector, error) {
	return listContentSelectors(ctx, d.sql)
}

func listContentSelectors(ctx context.Context, q querier) ([]ContentSelector, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+selectorCols+` FROM content_selectors ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContentSelector
	for rows.Next() {
		s, err := scanSelector(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetContentSelector returns one selector; ErrNotFound if there is none.
func (d *DB) GetContentSelector(ctx context.Context, name string) (ContentSelector, error) {
	return scanSelector(d.sql.QueryRowContext(ctx, `SELECT `+selectorCols+` FROM content_selectors WHERE name = ?`, name))
}

func scanSelector(row scanner) (ContentSelector, error) {
	var s ContentSelector
	var created, updated int64
	err := row.Scan(&s.Name, &s.Description, &s.Expression, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return ContentSelector{}, ErrNotFound
	}
	if err != nil {
		return ContentSelector{}, err
	}
	s.CreatedAt, s.UpdatedAt = fromMs(created), fromMs(updated)
	return s, nil
}

// CreateContentSelector stores a new selector; ErrExists if the name is taken.
func (d *DB) CreateContentSelector(ctx context.Context, s ContentSelector) error {
	return d.Tx(ctx, func(tx *Tx) error { return insertSelector(ctx, tx.q, s) })
}

func insertSelector(ctx context.Context, q querier, s ContentSelector) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM content_selectors WHERE name = ?`, s.Name).Scan(&one)
	if err == nil {
		return ErrExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := toMs(time.Now())
	_, err = q.ExecContext(ctx, `INSERT INTO content_selectors(`+selectorCols+`) VALUES(?,?,?,?,?)`,
		s.Name, s.Description, s.Expression, now, now)
	return err
}

// UpdateContentSelector replaces the description and expression of
// selector s.Name; ErrNotFound if there is none.
func (d *DB) UpdateContentSelector(ctx context.Context, s ContentSelector) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE content_selectors SET description = ?, expression = ?, updated_at = ? WHERE name = ?`,
		s.Description, s.Expression, toMs(time.Now()), s.Name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteContentSelector removes the selector unless a stored role has a
// privilege on it (*SelectorInUseError), in one transaction. ErrNotFound if
// there is none.
func (d *DB) DeleteContentSelector(ctx context.Context, name string) error {
	return d.Tx(ctx, func(tx *Tx) error {
		users, err := rolesUsingSelector(ctx, tx.q, name)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			return &SelectorInUseError{Selector: name, Roles: users}
		}
		res, err := tx.q.ExecContext(ctx, `DELETE FROM content_selectors WHERE name = ?`, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RolesUsingSelector returns the ids of the stored roles with a privilege
// on the selector, sorted.
func (d *DB) RolesUsingSelector(ctx context.Context, name string) ([]string, error) {
	return rolesUsingSelector(ctx, d.sql, name)
}

func rolesUsingSelector(ctx context.Context, q querier, name string) ([]string, error) {
	// ponytail: a scan of every role's privileges JSON; roles are few.
	list, err := listRoles(ctx, q)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range list {
		for _, p := range r.Privileges {
			if p.Target == SelectorPrefix+name {
				out = append(out, r.ID)
				break
			}
		}
	}
	return out, nil
}

// importSecurity inserts cfg's routing rules and content selectors (the
// part of ImportAccess for them). A rule or selector the API would refuse
// fails the import.
func importSecurity(ctx context.Context, q querier, cfg *config.Config) error {
	for _, cr := range cfg.RoutingRules {
		r := RoutingRule{Name: cr.Name, Description: cr.Description, Mode: strings.ToUpper(cr.Mode), Matchers: cr.Matchers}
		if errs := ValidateRoutingRule(r); len(errs) > 0 {
			return fmt.Errorf("routing rule %q: %s", r.Name, strings.Join(errs, "; "))
		}
		if err := insertRule(ctx, q, r); err != nil {
			return fmt.Errorf("routing rule %q: %w", r.Name, err)
		}
	}
	for _, cs := range cfg.ContentSelectors {
		s := ContentSelector{Name: cs.Name, Description: cs.Description, Expression: cs.Expression}
		if errs := ValidateContentSelector(s); len(errs) > 0 {
			return fmt.Errorf("content selector %q: %s", s.Name, strings.Join(errs, "; "))
		}
		if err := insertSelector(ctx, q, s); err != nil {
			return fmt.Errorf("content selector %q: %w", s.Name, err)
		}
	}
	return nil
}

// SelectorsFromConfig maps the config file's content selectors to stored
// ones, in file order, without validation (auth.New; the import validates).
func SelectorsFromConfig(cfg *config.Config) []ContentSelector {
	out := make([]ContentSelector, 0, len(cfg.ContentSelectors))
	for _, cs := range cfg.ContentSelectors {
		out = append(out, ContentSelector{Name: cs.Name, Description: cs.Description, Expression: cs.Expression})
	}
	return out
}
