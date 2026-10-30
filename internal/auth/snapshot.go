package auth

import (
	"errors"
	"sort"
	"strings"

	"commandcenter/internal/csel"
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
	all       actionSet
	formats   map[string]actionSet
	repos     map[string]actionSet
	selectors []selectorGrant
	metrics   actionSet // the MetricsTarget privilege
}

// MetricsTarget is the privilege target of the instance-wide usage data
// (Nexus nexus:metrics:read): its read action grants the repository
// dashboard, every repository's evaluation results and the monthly usage
// metrics.
const MetricsTarget = "metrics"

// selectorGrant is a "selector:<name>" privilege: bits on the paths the
// selector matches, in the repositories named by repos ("*" for any).
type selectorGrant struct {
	name  string
	repos map[string]bool
	bits  actionSet
}

func (g selectorGrant) inScope(repo string) bool { return g.repos["*"] || g.repos[repo] }

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
		case p.Target == MetricsTarget:
			c.metrics |= bits
		case strings.HasPrefix(p.Target, meta.SelectorPrefix):
			g := selectorGrant{name: strings.TrimPrefix(p.Target, meta.SelectorPrefix), repos: map[string]bool{}, bits: bits}
			for _, r := range p.Repos {
				g.repos[r] = true
			}
			c.selectors = append(c.selectors, g)
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
	// selectors are the parsed content selectors by name; a "selector:"
	// privilege on a name that is missing here (deleted, or an expression
	// this build cannot parse) grants nothing.
	selectors map[string]*csel.Expr
}

// NewSnapshot is NewSnapshotWith without content selectors.
func NewSnapshot(users []meta.User, roles []meta.Role, settings meta.Settings) *Snapshot {
	return NewSnapshotWith(users, roles, settings, nil)
}

// NewSnapshotWith builds a snapshot from stored records. The inputs are
// copied, so the caller may keep changing them. The built-in admin role is
// always present.
func NewSnapshotWith(users []meta.User, roles []meta.Role, settings meta.Settings, selectors []meta.ContentSelector) *Snapshot {
	s := &Snapshot{
		users:     make(map[string]meta.User, len(users)),
		roles:     make(map[string]*compiledRole, len(roles)+1),
		realms:    map[string]bool{},
		selectors: make(map[string]*csel.Expr, len(selectors)),
	}
	for _, sel := range selectors {
		if e, err := csel.Parse(sel.Expression); err == nil {
			s.selectors[sel.Name] = e
		}
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
	settings.OIDC = settings.OIDC.Copy()
	settings.Crowd = settings.Crowd.Copy()
	settings.RUT = settings.RUT.Copy()
	settings.DefaultRoles = append([]string{}, settings.DefaultRoles...)
	settings.SAML = settings.SAML.Copy()
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
		if p.Repos != nil {
			privs[i].Repos = append([]string{}, p.Repos...)
		}
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
	out.OIDC = s.settings.OIDC.Copy()
	out.RUT = s.settings.RUT.Copy()
	out.HTTP = s.settings.HTTP.Copy()
	out.TrustedCerts = append([]string{}, s.settings.TrustedCerts...)
	out.Crowd = s.settings.Crowd.Copy()
	out.DefaultRoles = append([]string{}, s.settings.DefaultRoles...)
	out.SAML = s.settings.SAML.Copy()
	return out
}

// RealmEnabled reports whether the realm id is in the list and enabled.
func (s *Snapshot) RealmEnabled(id string) bool { return s.realms[id] }

// canPasswordLogin reports whether u may sign in through the local realm:
// active, local with a password (HashUsable), and the local realm enabled.
// LDAP users sign in through the ldap realm (ldap.go), never with a stored
// row.
func (s *Snapshot) canPasswordLogin(u meta.User) bool {
	return u.Status == meta.StatusActive && u.Source == meta.SourceLocal && HashUsable(u.PasswordHash) && s.realms[meta.RealmLocal]
}

// has reports whether any of roles grants one of the actions in want on the
// repository named repo of format format ("" if unknown). A selector
// privilege counts when the repository is in its scope: it could match some
// path there, so listings work; hasPath is the exact check.
func (s *Snapshot) has(roles []string, repo, format string, want actionSet) bool {
	return s.hasPath(roles, repo, format, want, "", false)
}

// hasPath is has for one path (the part after /repository/<name>/, or a
// docker image name): a selector privilege counts only when the repository
// is in its scope and its expression matches (format in Nexus spelling,
// "/"+path). With exact false the expression is not consulted.
func (s *Snapshot) hasPath(roles []string, repo, format string, want actionSet, path string, exact bool) bool {
	for _, id := range roles {
		r := s.roles[id]
		if r == nil {
			continue
		}
		if r.all&want != 0 || r.repos[repo]&want != 0 || (format != "" && r.formats[format]&want != 0) {
			return true
		}
		for _, g := range r.selectors {
			if g.bits&want == 0 || !g.inScope(repo) {
				continue
			}
			e := s.selectors[g.name]
			if e == nil {
				continue
			}
			if !exact || e.Match(NexusFormat(format), "/"+strings.TrimPrefix(path, "/")) {
				return true
			}
		}
	}
	return false
}

// NexusFormat is the format name content selectors see: Nexus's spelling,
// "maven2" for maven and the others unchanged.
func NexusFormat(format string) string {
	if format == "maven" {
		return "maven2"
	}
	return format
}

// ErrLockout reports a change that would leave nobody able to administer
// the system.
var ErrLockout = errors.New("the change would leave no enabled administrator who can log in")

// CheckLockout returns ErrLockout unless an admin can log in under s:
// (a) an active local user with a password and the admin role while the
// local realm is enabled, or (b) the ldap realm enabled with a complete
// configuration whose group mappings or default roles grant admin, or (c)
// the same for the crowd realm. A stored ldap-source row with the admin
// role does not count by itself. The APIs
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
	if crowdUsable(s) && s.settings.Crowd.GrantsAdmin() {
		return nil
	}
	return ErrLockout
}

// RestorableAdmins counts the active local users with a password
// (HashUsable) and the admin role, whatever the realm state: the
// administrators who can sign in again once the local realm is re-enabled.
func RestorableAdmins(s *Snapshot) int {
	n := 0
	for _, id := range s.userIDs {
		u := s.users[id]
		if u.Status == meta.StatusActive && u.Source == meta.SourceLocal && HashUsable(u.PasswordHash) && hasRole(u.Roles, meta.AdminRole) {
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
