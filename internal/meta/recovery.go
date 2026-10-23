package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ResetLocalPassword is the offline break-glass recovery (final review I4,
// "commandcenter user reset-password"). In one transaction it
//   - sets id's password hash and status active, creating a local user if
//     there is none (an id naming a source=ldap override row is refused, so
//     a disabled directory identity is never re-enabled as a password login);
//   - with admin, adds the admin role;
//   - makes sure the local realm is in the realm list and enabled.
//
// It returns a description of every change, in that order.
func (d *DB) ResetLocalPassword(ctx context.Context, id, hash string, admin bool) ([]string, error) {
	if !ValidUserID(id) {
		return nil, fmt.Errorf("invalid user id %q", id)
	}
	if hash == "" {
		return nil, errors.New("empty password hash")
	}
	var changes []string
	err := d.Tx(ctx, func(tx *Tx) error {
		changes = nil
		u, err := scanUser(tx.q.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
		switch {
		case errors.Is(err, ErrNotFound):
			nu := User{ID: id, PasswordHash: hash, Source: SourceLocal, Status: StatusActive, Roles: []string{}}
			changes = append(changes, fmt.Sprintf("created local user %q", id))
			if admin {
				nu.Roles = []string{AdminRole}
				changes = append(changes, fmt.Sprintf("added the admin role to user %q", id))
			}
			if err := insertUser(ctx, tx.q, nu); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			changes = append(changes, fmt.Sprintf("set the password of user %q", id))
			u.PasswordHash = hash
			if u.Source != SourceLocal {
				return fmt.Errorf("user %q is an LDAP role-override record; choose another id for a local recovery account", id)
			}
			if u.Status != StatusActive {
				changes = append(changes, fmt.Sprintf("status of user %q: %s -> %s", id, u.Status, StatusActive))
				u.Status = StatusActive
			}
			if admin && !hasString(u.Roles, AdminRole) {
				u.Roles = append(u.Roles, AdminRole)
				changes = append(changes, fmt.Sprintf("added the admin role to user %q", id))
			}
			roles, err := json.Marshal(u.Roles)
			if err != nil {
				return err
			}
			if _, err := tx.q.ExecContext(ctx, `UPDATE users SET password_hash = ?, source = ?, status = ?, roles = ?, updated_at = ? WHERE id = ?`,
				u.PasswordHash, u.Source, u.Status, string(roles), toMs(time.Now()), id); err != nil {
				return err
			}
		}
		set, err := loadSettings(ctx, tx.q)
		if err != nil {
			return err
		}
		realms := append([]Realm{}, set.Realms...)
		found := false
		for i := range realms {
			if realms[i].ID == RealmLocal {
				found = true
				if !realms[i].Enabled {
					realms[i].Enabled = true
					changes = append(changes, "enabled the local realm")
				}
			}
		}
		if !found {
			realms = append([]Realm{{ID: RealmLocal, Enabled: true}}, realms...)
			changes = append(changes, "added the local realm (enabled) to the realm list")
		}
		return setSettingJSON(ctx, tx.q, SettingRealms, realms)
	})
	if err != nil {
		return nil, err
	}
	return changes, nil
}

func hasString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
