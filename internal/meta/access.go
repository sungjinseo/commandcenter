package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"commandcenter/internal/config"
)

// Users and roles (plan 12 Decisions 1 and 2). The database is authoritative
// once config.yaml's users, roles and settings were imported (ImportAccess).

// User sources and statuses.
const (
	SourceLocal = "local"
	SourceLDAP  = "ldap"
	SourceOIDC  = "oidc"
	SourceCrowd = "crowd"

	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// AdminRole is the built-in role that grants everything. It cannot be edited
// or deleted (the API enforces that).
const AdminRole = "admin"

// Privilege actions (Decision 2).
const (
	ActionBrowse = "browse"
	ActionRead   = "read"
	ActionAdd    = "add"
	ActionEdit   = "edit"
	ActionDelete = "delete"
)

// Actions lists every privilege action in canonical order.
var Actions = []string{ActionBrowse, ActionRead, ActionAdd, ActionEdit, ActionDelete}

// ErrInUse reports that a record is still referenced (a role held by a user).
var ErrInUse = errors.New("in use")

// User is a stored user. Roles is never nil when read back.
type User struct {
	ID           string
	Name         string
	Email        string
	PasswordHash string `json:"-"` // never serialised
	Source       string // SourceLocal, SourceLDAP or SourceOIDC
	Status       string // StatusActive or StatusDisabled
	Roles        []string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Privilege grants actions on a target: "*", "format:<fmt>", "repo:<name>"
// or "selector:<content selector>". Repos scopes a selector target to
// repositories ("*" is any); it is empty for the other targets.
type Privilege struct {
	Target  string   `json:"target"`
	Actions []string `json:"actions"`
	Repos   []string `json:"repos,omitempty"`
}

// Role is a stored role. Privileges is never nil when read back.
type Role struct {
	ID          string
	Description string
	Privileges  []Privilege
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const userCols = `id, name, email, password_hash, source, status, roles, created_at, updated_at`

// ListUsers returns every stored user, ordered by id.
func (d *DB) ListUsers(ctx context.Context) ([]User, error) { return listUsers(ctx, d.sql) }

func listUsers(ctx context.Context, q querier) ([]User, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUser returns one user; ErrNotFound if there is none.
func (d *DB) GetUser(ctx context.Context, id string) (User, error) {
	return scanUser(d.sql.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// CreateUser stores a new user; ErrExists if the id is taken, also by an id
// that differs only in case (QA N-18). Empty source and status default to
// local and active; zero times to now.
func (d *DB) CreateUser(ctx context.Context, u User) error {
	return d.Tx(ctx, func(tx *Tx) error { return insertUser(ctx, tx.q, u) }) // the check and the insert under one write lock
}

// UpdateUser replaces the stored user u.ID (created_at is kept); ErrNotFound
// if there is none.
func (d *DB) UpdateUser(ctx context.Context, u User) error {
	u = userDefaults(u)
	roles, err := json.Marshal(u.Roles)
	if err != nil {
		return err
	}
	res, err := d.sql.ExecContext(ctx, `UPDATE users SET name = ?, email = ?, password_hash = ?, source = ?, status = ?,
roles = ?, updated_at = ? WHERE id = ?`, u.Name, u.Email, u.PasswordHash, u.Source, u.Status, string(roles), toMs(time.Now()), u.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes the user and, in the same transaction, its API tokens
// (Decision 1) and user token. ErrNotFound if there is no such user.
func (d *DB) DeleteUser(ctx context.Context, id string) error {
	return d.Tx(ctx, func(tx *Tx) error {
		res, err := tx.q.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err = tx.q.ExecContext(ctx, `DELETE FROM tokens WHERE "user" = ?`, id); err != nil {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `DELETE FROM user_tokens WHERE owner = ?`, id)
		return err
	})
}

func userDefaults(u User) User {
	if u.Source == "" {
		u.Source = SourceLocal
	}
	if u.Status == "" {
		u.Status = StatusActive
	}
	if u.Roles == nil {
		u.Roles = []string{}
	}
	return u
}

func insertUser(ctx context.Context, q querier, u User) error {
	u = userDefaults(u)
	roles, err := json.Marshal(u.Roles)
	if err != nil {
		return err
	}
	if err := checkFreeID(ctx, q, "users", u.ID); err != nil {
		return err
	}
	now := time.Now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	if u.UpdatedAt.IsZero() {
		u.UpdatedAt = now
	}
	_, err = q.ExecContext(ctx, `INSERT INTO users(`+userCols+`) VALUES(?,?,?,?,?,?,?,?,?)`,
		u.ID, u.Name, u.Email, u.PasswordHash, u.Source, u.Status, string(roles), toMs(u.CreatedAt), toMs(u.UpdatedAt))
	return err
}

// checkFreeID returns ErrExists when table (users or roles) has id, compared
// case-insensitively: login and role lookups are exact, so "Admin" next to
// "admin" would be a look-alike. Ids are ASCII, so NOCASE folds them fully
// (and lower() under the "C" collation on PostgreSQL, which folds ASCII as
// NOCASE does). Rows that already differ only in case are left alone.
func checkFreeID(ctx context.Context, q querier, table, id string) error {
	var got string
	err := q.QueryRowContext(ctx, pg(q, `SELECT id FROM `+table+` WHERE id = ? COLLATE NOCASE LIMIT 1`,
		`SELECT id FROM `+table+` WHERE lower(id) = lower(? COLLATE "C") LIMIT 1`), id).Scan(&got)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	case got != id:
		return fmt.Errorf("%w as %q", ErrExists, got)
	}
	return ErrExists
}

func scanUser(row scanner) (User, error) {
	var u User
	var roles string
	var created, updated int64
	err := row.Scan(&u.ID, &u.Name, &u.Email, &u.PasswordHash, &u.Source, &u.Status, &roles, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	if err := json.Unmarshal([]byte(roles), &u.Roles); err != nil {
		return User{}, fmt.Errorf("user %q: decode roles: %w", u.ID, err)
	}
	if u.Roles == nil {
		u.Roles = []string{}
	}
	u.CreatedAt, u.UpdatedAt = fromMs(created), fromMs(updated)
	return u, nil
}

const roleCols = `id, description, privileges, created_at, updated_at`

// ListRoles returns every stored role, ordered by id.
func (d *DB) ListRoles(ctx context.Context) ([]Role, error) { return listRoles(ctx, d.sql) }

func listRoles(ctx context.Context, q querier) ([]Role, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+roleCols+` FROM roles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Role
	for rows.Next() {
		r, err := scanRole(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRole returns one role; ErrNotFound if there is none.
func (d *DB) GetRole(ctx context.Context, id string) (Role, error) {
	return scanRole(d.sql.QueryRowContext(ctx, `SELECT `+roleCols+` FROM roles WHERE id = ?`, id))
}

// CreateRole stores a new role; ErrExists if the id is taken, also by an id
// that differs only in case (QA N-18).
func (d *DB) CreateRole(ctx context.Context, r Role) error {
	return d.Tx(ctx, func(tx *Tx) error { return insertRole(ctx, tx.q, r) })
}

// UpdateRole replaces the description and privileges of role r.ID;
// ErrNotFound if there is none.
func (d *DB) UpdateRole(ctx context.Context, r Role) error {
	if r.Privileges == nil {
		r.Privileges = []Privilege{}
	}
	privs, err := json.Marshal(r.Privileges)
	if err != nil {
		return err
	}
	res, err := d.sql.ExecContext(ctx, `UPDATE roles SET description = ?, privileges = ?, updated_at = ? WHERE id = ?`,
		r.Description, string(privs), toMs(time.Now()), r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteRole removes a role. It refuses with ErrInUse while a user holds the
// role (Decision 2); ErrNotFound if there is no such role.
func (d *DB) DeleteRole(ctx context.Context, id string) error {
	return d.Tx(ctx, func(tx *Tx) error {
		var one int
		err := tx.q.QueryRowContext(ctx, pg(tx.q, `SELECT 1 FROM users, json_each(users.roles) WHERE json_each.value = ? LIMIT 1`,
			`SELECT 1 FROM users, json_array_elements_text(users.roles::json) AS r(value) WHERE r.value = ? LIMIT 1`), id).Scan(&one)
		if err == nil {
			return ErrInUse
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		res, err := tx.q.ExecContext(ctx, `DELETE FROM roles WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func insertRole(ctx context.Context, q querier, r Role) error {
	if r.Privileges == nil {
		r.Privileges = []Privilege{}
	}
	privs, err := json.Marshal(r.Privileges)
	if err != nil {
		return err
	}
	if err := checkFreeID(ctx, q, "roles", r.ID); err != nil {
		return err
	}
	now := time.Now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = now
	}
	_, err = q.ExecContext(ctx, `INSERT INTO roles(`+roleCols+`) VALUES(?,?,?,?,?)`,
		r.ID, r.Description, string(privs), toMs(r.CreatedAt), toMs(r.UpdatedAt))
	return err
}

func scanRole(row scanner) (Role, error) {
	var r Role
	var privs string
	var created, updated int64
	err := row.Scan(&r.ID, &r.Description, &privs, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Role{}, ErrNotFound
	}
	if err != nil {
		return Role{}, err
	}
	if err := json.Unmarshal([]byte(privs), &r.Privileges); err != nil {
		return Role{}, fmt.Errorf("role %q: decode privileges: %w", r.ID, err)
	}
	if r.Privileges == nil {
		r.Privileges = []Privilege{}
	}
	r.CreatedAt, r.UpdatedAt = fromMs(created), fromMs(updated)
	return r, nil
}

// accessImportedKey marks that config.yaml's users, roles and settings were
// imported (plan 12). After that, the database is the only source of them.
const accessImportedKey = "access.imported"

// AccessImported reports whether ImportAccess has run.
func (d *DB) AccessImported(ctx context.Context) (bool, error) {
	_, err := d.GetSetting(ctx, accessImportedKey)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// ImportAccess stores cfg's users, roles (with the built-in admin role) and
// settings and marks the import done, in one transaction, unless an import
// already happened. It reports whether it imported.
func (d *DB) ImportAccess(ctx context.Context, cfg *config.Config) (bool, error) {
	imported := false
	err := d.Tx(ctx, func(tx *Tx) error {
		var v string
		err := tx.q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, accessImportedKey).Scan(&v)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// The ids the API would refuse are refused here too (final review M3):
		// ":" would make a stored id look like an LDAP owner key.
		for _, cu := range cfg.Users {
			if !ValidUserID(cu.Name) {
				return fmt.Errorf("user %q: invalid user id: use 1-64 letters, digits, '.', '_', '@' and '-', starting with a letter or digit (not \"create\")", cu.Name)
			}
		}
		for _, r := range RolesFromConfig(cfg) {
			if err := insertRole(ctx, tx.q, r); err != nil {
				return fmt.Errorf("role %q: %w", r.ID, err)
			}
		}
		for _, u := range UsersFromConfig(cfg) {
			if err := insertUser(ctx, tx.q, u); err != nil {
				return fmt.Errorf("user %q: %w", u.ID, err)
			}
		}
		if err := importSecurity(ctx, tx.q, cfg); err != nil {
			return err
		}
		if err := saveSettings(ctx, tx.q, SettingsFromConfig(cfg)); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?)`,
			accessImportedKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		imported = true
		return nil
	})
	return imported && err == nil, err
}

// BuiltinAdminRole is the stored form of the admin role: everything on "*".
func BuiltinAdminRole() Role {
	return Role{ID: AdminRole, Description: "Built-in administrator role: grants everything",
		Privileges: []Privilege{{Target: "*", Actions: append([]string(nil), Actions...)}}}
}

// UsersFromConfig maps config users to local, active users in file order.
func UsersFromConfig(cfg *config.Config) []User {
	out := make([]User, 0, len(cfg.Users))
	for _, cu := range cfg.Users {
		roles := append([]string{}, cu.Roles...)
		out = append(out, User{ID: cu.Name, PasswordHash: cu.PasswordHash, Source: SourceLocal, Status: StatusActive, Roles: roles})
	}
	return out
}

// RolesFromConfig maps config roles to stored roles, the built-in admin role
// first and the others by name. A `write` list becomes one privilege with
// every action per entry: "*" targets everything, any other name
// "repo:<name>" (Decision 2: today's write implies delete). A config role
// named admin is ignored; the built-in one wins.
func RolesFromConfig(cfg *config.Config) []Role {
	names := make([]string, 0, len(cfg.Roles))
	for name := range cfg.Roles {
		if name != AdminRole {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := []Role{BuiltinAdminRole()}
	for _, name := range names {
		privs := []Privilege{}
		seen := map[string]bool{}
		for _, w := range cfg.Roles[name].Write {
			target := "repo:" + w
			if w == "*" {
				target = "*"
			}
			if seen[target] {
				continue
			}
			seen[target] = true
			privs = append(privs, Privilege{Target: target, Actions: append([]string(nil), Actions...)})
		}
		out = append(out, Role{ID: name, Description: "Imported from the config file", Privileges: privs})
	}
	return out
}
