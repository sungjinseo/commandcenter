package auth

import (
	"errors"
	"sort"
	"strings"

	"commandcenter/internal/meta"
)

// actionSet is a bit set of privilege actions.
type actionSet uint8

const (
	actBrowse actionSet = 1 << iota
	actRead
	actAdd
	actEdit
	actDelete

	actAll = actBrowse | actRead | actAdd | actEdit | actDelete
)

var actionBits = map[string]actionSet{
	meta.ActionBrowse: actBrowse,
	meta.ActionRead:   actRead,
	meta.ActionAdd:    actAdd,
	meta.ActionEdit:   actEdit,
	meta.ActionDelete: actDelete,
}

// compiledRole is a role's privileges as bit sets per target kind.
type compiledRole struct {
	all     actionSet
	formats map[string]actionSet
	repos   map[string]actionSet
}

func compileRole(r meta.Role) *compiledRole {
	c := &compiledRole{formats: map[string]actionSet{}, repos: map[string]actionSet{}}
	if r.ID == meta.AdminRole {
		c.all = actAll // built in: grants everything whatever is stored
		return c
	}
	for _, p := range r.Privileges {
		var bits actionSet
		for _, a := range p.Actions {
			bits |= actionBits[a]
		}
		switch {
		case p.Target == "*":
			c.all |= bits
		case strings.HasPrefix(p.Target, "format:"):
			f := strings.TrimPrefix(p.Target, "format:")
			c.formats[f] |= bits
		case strings.HasPrefix(p.Target, "repo:"):
			n := strings.TrimPrefix(p.Target, "repo:")
			c.repos[n] |= bits
		}
	}
	return c
}

// Snapshot is an immutable view of users, roles, settings and realms. The
// Authenticator publishes a new one after every change (Reload); readers
// never see a half-applied edit.
type Snapshot struct {
	users    map[string]meta.User
	userIDs  []string // sorted
	roles    map[string]*compiledRole
	roleList []meta.Role // sorted by id
	settings meta.Settings
	realms   map[string]bool // enabled realm ids
}

// NewSnapshot builds a snapshot from stored records. The inputs are copied,
// so the caller may keep changing them. The built-in admin role is always
// present.
func NewSnapshot(users []meta.User, roles []meta.Role, settings meta.Settings) *Snapshot {
	s := &Snapshot{
		users:  make(map[string]meta.User, len(users)),
		roles:  make(map[string]*compiledRole, len(roles)+1),
		realms: map[string]bool{},
	}
	for _, u := range users {
		u.Roles = append([]string{}, u.Roles...)
		s.users[u.ID] = u
		s.userIDs = append(s.userIDs, u.ID)
	}
	sort.Strings(s.userIDs)
	hasAdmin := false
	for _, r := range roles {
		r = copyRole(r)
		if r.ID == meta.AdminRole {
			hasAdmin = true
		}
		s.roles[r.ID] = compileRole(r)
		s.roleList = append(s.roleList, r)
	}
	if !hasAdmin {
		r := meta.BuiltinAdminRole()
		s.roles[r.ID] = compileRole(r)
		s.roleList = append(s.roleList, r)
	}
	sort.Slice(s.roleList, func(i, j int) bool { return s.roleList[i].ID < s.roleList[j].ID })
	settings.Realms = append([]meta.Realm{}, settings.Realms...)
	settings.LDAP = settings.LDAP.Copy()
	s.settings = settings
	for _, r := range settings.Realms {
		if r.Enabled {
			s.realms[r.ID] = true
		}
	}
	return s
}

func copyRole(r meta.Role) meta.Role {
	privs := make([]meta.Privilege, len(r.Privileges))
	for i, p := range r.Privileges {
		privs[i] = meta.Privilege{Target: p.Target, Actions: append([]string{}, p.Actions...)}
	}
	r.Privileges = privs
	return r
}

// Users returns copies of every user, ordered by id, without password hashes.
func (s *Snapshot) Users() []meta.User {
	out := make([]meta.User, 0, len(s.userIDs))
	for _, id := range s.userIDs {
		u := s.users[id]
		u.Roles = append([]string{}, u.Roles...)
		u.PasswordHash = ""
		out = append(out, u)
	}
	return out
}

// User returns a copy of one user without its password hash.
func (s *Snapshot) User(id string) (meta.User, bool) {
	u, ok := s.users[id]
	if ok {
		u.Roles = append([]string{}, u.Roles...)
		u.PasswordHash = ""
	}
	return u, ok
}

// Roles returns copies of every role, ordered by id.
func (s *Snapshot) Roles() []meta.Role {
	out := make([]meta.Role, len(s.roleList))
	for i, r := range s.roleList {
		out[i] = copyRole(r)
	}
	return out
}

// Role returns a copy of one role.
func (s *Snapshot) Role(id string) (meta.Role, bool) {
	for _, r := range s.roleList {
		if r.ID == id {
			return copyRole(r), true
		}
	}
	return meta.Role{}, false
}

// Settings returns a copy of the settings.
func (s *Snapshot) Settings() meta.Settings {
	out := s.settings
	out.Realms = append([]meta.Realm{}, s.settings.Realms...)
	out.LDAP = s.settings.LDAP.Copy()
	return out
}

// RealmEnabled reports whether the realm id is in the list and enabled.
func (s *Snapshot) RealmEnabled(id string) bool { return s.realms[id] }

// canPasswordLogin reports whether u may sign in through the local realm:
// active, local with a password, and the local realm enabled. LDAP users
// sign in through the ldap realm (ldap.go), never with a stored row.
func (s *Snapshot) canPasswordLogin(u meta.User) bool {
	return u.Status == meta.StatusActive && u.Source == meta.SourceLocal && u.PasswordHash != "" && s.realms[meta.RealmLocal]
}

// has reports whether any of roles grants one of the actions in want on the
// repository named repo of format format ("" if unknown).
func (s *Snapshot) has(roles []string, repo, format string, want actionSet) bool {
	for _, id := range roles {
		r := s.roles[id]
		if r == nil {
			continue
		}
		if r.all&want != 0 || r.repos[repo]&want != 0 || (format != "" && r.formats[format]&want != 0) {
			return true
		}
	}
	return false
}

// ErrLockout reports a change that would leave nobody able to administer
// the system.
var ErrLockout = errors.New("the change would leave no enabled administrator who can log in")

// CheckLockout returns ErrLockout unless an admin can log in under s:
// (a) an active local user with a password and the admin role while the
// local realm is enabled, or (b) the ldap realm enabled with a complete
// configuration whose group mappings or default roles grant admin. A stored
// ldap-source row with the admin role does not count by itself. The APIs
// call it on the would-be snapshot before committing a change.
func CheckLockout(s *Snapshot) error {
	if s.realms[meta.RealmLocal] {
		for _, id := range s.userIDs {
			u := s.users[id]
			if hasRole(u.Roles, meta.AdminRole) && s.canPasswordLogin(u) {
				return nil
			}
		}
	}
	if s.realms[meta.RealmLDAP] && s.settings.LDAP.Complete() && s.settings.LDAP.GrantsAdmin() {
		return nil
	}
	return ErrLockout
}

// RestorableAdmins counts the active local users with a password and the
// admin role, whatever the realm state: the administrators who can sign in
// again once the local realm is re-enabled.
func RestorableAdmins(s *Snapshot) int {
	n := 0
	for _, id := range s.userIDs {
		u := s.users[id]
		if u.Status == meta.StatusActive && u.Source == meta.SourceLocal && u.PasswordHash != "" && hasRole(u.Roles, meta.AdminRole) {
			n++
		}
	}
	return n
}

func hasRole(roles []string, id string) bool {
	for _, r := range roles {
		if r == id {
			return true
		}
	}
	return false
}
