package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Users, roles and the self-service password change (plan 12 Task 2). Reads
// need an admin or the read action of the users or roles domain; every
// change needs an admin or the domain's action, from a password or session
// login, plus the UI header. Only an admin may grant, change or remove
// what makes someone an admin. The caller's own password change needs only
// an interactive login.

func (u *UI) accessRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /-/api/users", u.createUser)
	mux.HandleFunc("GET /-/api/users/{id}", u.getUser)
	mux.HandleFunc("PUT /-/api/users/{id}", u.updateUser)
	mux.HandleFunc("DELETE /-/api/users/{id}", u.deleteUser)
	mux.HandleFunc("PUT /-/api/users/{id}/password", u.resetPassword)
	mux.HandleFunc("DELETE /-/api/users/{id}/tokens", u.revokeUserTokens)
	mux.HandleFunc("GET /-/api/roles", u.listRoles)
	mux.HandleFunc("POST /-/api/roles", u.createRole)
	mux.HandleFunc("GET /-/api/roles/{id}", u.getRole)
	mux.HandleFunc("PUT /-/api/roles/{id}", u.updateRole)
	mux.HandleFunc("DELETE /-/api/roles/{id}", u.deleteRole)
	mux.HandleFunc("PUT /-/api/account/password", u.changeOwnPassword)
}

const (
	maxTextLen     = 200
	lockoutMessage = "this change would leave no admin who can log in"
)

// The UI matches these texts (static/access.js; TestAccessMessagesPinned).
const (
	roleInUseMessage     = "role is in use"
	wrongPasswordMessage = "the current password is incorrect"
	selfDeleteMessage    = "you cannot delete the user you are signed in as"
)

var (
	userIDRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*$`)
	accessFormats = []string{"maven", "npm", "pypi", "go", "cargo", "docker", "raw", "helm", "yum", "nuget", "apt", "composer", "conda", "rubygems", "gitlfs", "r", "conan", "p2", "swift", "cocoapods", "huggingface", "terraform"}
)

// errAdminOnly refuses a delegated caller a change of what makes someone an
// admin (403).
var errAdminOnly = errors.New("only an admin can change an administrator or grant the admin role")

// validation collects messages for the 400 answer.
type validation []string

func (v *validation) add(format string, args ...any) { *v = append(*v, fmt.Sprintf(format, args...)) }

func (v validation) err() error {
	if len(v) == 0 {
		return nil
	}
	return &repo.ValidationError{Errors: v}
}

// writeAccessError maps validation (400), not found (404), taken (409), in
// use (409) and lockout (409) errors; anything else is a 500.
func (u *UI) writeAccessError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *repo.ValidationError
	switch {
	case errors.As(err, &ve):
		first := "invalid request"
		if len(ve.Errors) > 0 {
			first = ve.Errors[0]
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": first, "errors": ve.Errors})
	case errors.Is(err, meta.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, meta.ErrExists):
		writeError(w, http.StatusConflict, err.Error()) // names an id that differs only in case
	case errors.Is(err, meta.ErrContained):
		_, by, _ := strings.Cut(err.Error(), ": contained in another role: ")
		writeError(w, http.StatusConflict, roleInUseMessage+" by role "+by)
	case errors.Is(err, meta.ErrInUse):
		writeError(w, http.StatusConflict, roleInUseMessage)
	case errors.Is(err, auth.ErrLockout):
		writeError(w, http.StatusConflict, lockoutMessage)
	case errors.Is(err, errAdminOnly):
		writeError(w, http.StatusForbidden, errAdminOnly.Error())
	default:
		u.fail(w, r, err)
	}
}

// ---- DTOs ----

type userDTO struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Email       string   `json:"email"`
	Source      string   `json:"source"`
	Status      string   `json:"status"`
	Roles       []string `json:"roles"`
	HasPassword bool     `json:"hasPassword"`
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt"`
}

func userJSON(x meta.User) userDTO {
	roles := x.Roles
	if roles == nil {
		roles = []string{}
	}
	return userDTO{ID: x.ID, Name: x.Name, Email: x.Email, Source: x.Source, Status: x.Status, Roles: roles,
		HasPassword: auth.HashUsable(x.PasswordHash), CreatedAt: rfc3339(x.CreatedAt), UpdatedAt: rfc3339(x.UpdatedAt)}
}

type privilegeDTO struct {
	Target  string   `json:"target"`
	Actions []string `json:"actions"`
	// Repos scopes a "selector:<name>" target to repositories ("*" for
	// any); absent for the other targets.
	Repos []string `json:"repos,omitempty"`
}

type roleDTO struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"` // "" shows the id
	Description string         `json:"description"`
	Privileges  []privilegeDTO `json:"privileges"`
	Roles       []string       `json:"roles"` // contained roles, transitively granted
	CreatedAt   string         `json:"createdAt"`
	UpdatedAt   string         `json:"updatedAt"`
}

func roleJSON(x meta.Role) roleDTO {
	privs := make([]privilegeDTO, 0, len(x.Privileges))
	for _, p := range x.Privileges {
		acts := p.Actions
		if acts == nil {
			acts = []string{}
		}
		privs = append(privs, privilegeDTO{Target: p.Target, Actions: acts, Repos: p.Repos})
	}
	roles := x.Roles
	if roles == nil {
		roles = []string{}
	}
	return roleDTO{ID: x.ID, Name: x.Name, Description: x.Description, Privileges: privs, Roles: roles,
		CreatedAt: rfc3339(x.CreatedAt), UpdatedAt: rfc3339(x.UpdatedAt)}
}

// ---- validation ----

func checkUserID(id string) error {
	switch {
	case id == "":
		return &repo.ValidationError{Errors: []string{"user id is required"}}
	case len(id) > 64:
		return &repo.ValidationError{Errors: []string{"user id is longer than 64 characters"}}
	case !userIDRE.MatchString(id):
		return &repo.ValidationError{Errors: []string{fmt.Sprintf("invalid user id %q: use letters, digits, '.', '_', '@' and '-', starting with a letter or digit", id)}}
	case id == "create":
		return &repo.ValidationError{Errors: []string{`user id "create" is reserved`}}
	}
	return nil
}

// checkPassword applies the password rules and the password policy
// setting to a new local password.
func (u *UI) checkPassword(pw string) error {
	err := auth.CheckNewPassword(pw)
	if err == nil {
		err = u.Auth.Snapshot().Settings().PasswordPolicy.Check(pw)
	}
	if err != nil {
		return &repo.ValidationError{Errors: []string{err.Error()}}
	}
	return nil
}

func checkText(v *validation, what, s string) {
	switch {
	case utf8.RuneCountInString(s) > maxTextLen:
		v.add("%s is longer than %d characters", what, maxTextLen)
	case strings.ContainsFunc(s, unicode.IsControl): // as task names (QA N-17)
		v.add("%s must not contain control characters", what)
	}
}

// checkEmail accepts an empty value or one bare address that net/mail
// parses: no display name, list or surrounding space (QA N-17).
func checkEmail(v *validation, s string) {
	if s == "" {
		return
	}
	if a, err := mail.ParseAddress(s); err != nil || a.Address != s {
		v.add("email is not a valid e-mail address")
	}
}

// checkRoleRefs reports every role id that is not stored.
func (u *UI) checkRoleRefs(ctx context.Context, v *validation, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	have := map[string]bool{meta.AdminRole: true}
	roles, err := u.DB.ListRoles(ctx)
	if err != nil {
		return err
	}
	for _, r := range roles {
		have[r.ID] = true
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			v.add("duplicate role %q", id)
		} else if !have[id] {
			v.add("unknown role %q", id)
		}
		seen[id] = true
	}
	return nil
}

// validatePrivileges checks a role's privileges. A "repo:" target must name
// an existing repository (QA N-25) unless the stored role (kept) already
// has it: a privilege an older version left from a deleted repository may
// stay while the role is edited. A "selector:" target must name a stored
// content selector and list the repositories it applies to ("*" for any);
// a repository the stored privilege already named may stay.
func (u *UI) validatePrivileges(v *validation, privs []privilegeDTO, kept []meta.Privilege) {
	valid := map[string]bool{}
	for _, a := range meta.Actions {
		valid[a] = true
	}
	var selectors map[string]bool // loaded on the first selector target
	targets := map[string]bool{}
	for i, p := range privs {
		at := fmt.Sprintf("privilege %d", i+1)
		key := p.Target
		switch {
		case strings.HasPrefix(p.Target, meta.SelectorPrefix):
			if selectors == nil {
				selectors = map[string]bool{}
				if list, err := u.DB.ListContentSelectors(context.Background()); err == nil {
					for _, s := range list {
						selectors[s.Name] = true
					}
				}
			}
			if name := strings.TrimPrefix(p.Target, meta.SelectorPrefix); !selectors[name] {
				v.add("%s: unknown content selector %q", at, name)
			}
			if len(p.Repos) == 0 {
				v.add(`%s: a selector privilege needs repos (repository names, or "*" for any)`, at)
			}
			wasKept := func(repo string) bool {
				return slices.ContainsFunc(kept, func(k meta.Privilege) bool { return k.Target == p.Target && slices.Contains(k.Repos, repo) })
			}
			for _, repo := range p.Repos {
				if f, ok := strings.CutPrefix(repo, "*-"); ok { // every repository of a format (Nexus "*-maven2")
					if !slices.ContainsFunc(accessFormats, func(x string) bool { return auth.NexusFormat(x) == f }) {
						v.add("%s: unknown format %q in %q", at, f, repo)
					}
					continue
				}
				if repo != "*" && u.Repos.Get(repo) == nil && !wasKept(repo) {
					v.add("%s: unknown repository %q", at, repo)
				}
			}
			key += "|" + strings.Join(p.Repos, ",")
		case len(p.Repos) > 0:
			v.add(`%s: repos is only valid with a "selector:<name>" target`, at)
		}
		valid := valid
		if strings.HasPrefix(p.Target, meta.AppTargetPrefix) {
			valid = map[string]bool{}
			for _, a := range appActionNames {
				valid[a] = true
			}
		}
		if targets[key] {
			v.add("%s: duplicate target %q", at, p.Target)
		}
		targets[key] = true
		target := p.Target
		if t, ok := strings.CutPrefix(target, meta.AdminTargetPrefix); ok {
			target = t
			if slices.Contains(p.Actions, meta.ActionAdd) && strings.HasPrefix(t, "repo:") {
				v.add(`%s: "add" creates repositories: grant it on "admin:*" or "admin:format:<format>"`, at)
			}
		}
		switch {
		case strings.HasPrefix(target, meta.AppTargetPrefix) && target == p.Target:
			d := strings.TrimPrefix(target, meta.AppTargetPrefix)
			if acts, ok := auth.AppDomains[d]; !ok {
				v.add("%s: unknown application domain %q", at, d)
			} else {
				for _, a := range p.Actions {
					if !slices.Contains(acts, a) && slices.Contains(appActionNames, a) {
						v.add("%s: the %q domain delegates only %s; the rest stays with administrators", at, d, strings.Join(acts, ", "))
						break
					}
				}
			}
		case strings.HasPrefix(target, meta.SelectorPrefix) && target == p.Target:
		case p.Target == auth.MetricsTarget:
			if len(p.Actions) != 1 || p.Actions[0] != meta.ActionRead {
				v.add(`%s: the %q target takes the action "read" only`, at, auth.MetricsTarget)
			}
		case target == "*":
		case strings.HasPrefix(target, "format:"):
			if f := strings.TrimPrefix(target, "format:"); !contains(accessFormats, f) {
				v.add("%s: unknown format %q", at, f)
			}
		case strings.HasPrefix(target, "repo:"):
			name := strings.TrimPrefix(target, "repo:")
			if err := repo.CheckName("repository", name); err != nil {
				v.add("%s: invalid repository name in %q", at, p.Target)
			} else if u.Repos.Get(name) == nil && !slices.ContainsFunc(kept, func(k meta.Privilege) bool { return k.Target == p.Target }) {
				v.add("%s: unknown repository %q", at, name)
			}
		default:
			v.add(`%s: target %q must be "*", "format:<format>", "repo:<name>", "selector:<name>", "metrics", "app:<domain>" or "admin:" before "*", "format:<format>" or "repo:<name>"`, at, p.Target)
		}
		if len(p.Actions) == 0 {
			v.add("%s: at least one action is required", at)
		}
		seen := map[string]bool{}
		for _, a := range p.Actions {
			switch {
			case !valid[a]:
				v.add("%s: unknown action %q", at, a)
			case seen[a]:
				v.add("%s: duplicate action %q", at, a)
			}
			seen[a] = true
		}
	}
}

// AccessFormats lists the formats a privilege may name ("format:<f>").
func AccessFormats() []string { return slices.Clone(accessFormats) }

// appActionNames are the actions any application domain may take; one a
// domain does not delegate is refused with a message saying so.
var appActionNames = []string{meta.ActionRead, meta.ActionAdd, meta.ActionEdit, meta.ActionDelete, meta.ActionRun}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- lockout ----

// checkLockout applies change to the stored users and refuses it
// (auth.ErrLockout) when
//   - it removes the last "restorable" admin (an active local user with a
//     password and the admin role, whatever the realm state), the way back
//     in through the local realm (plan 12 Task 6, I1) — this holds even in a
//     system that is already locked out or that has LDAP admins; or
//   - the system has an admin who can log in now and would have none after.
//
// A system with no restorable admin can still be changed, so an
// administrator can be restored.
func (u *UI) checkLockout(ctx context.Context, change func(users []meta.User) []meta.User) error {
	data, err := u.DB.LoadAccess(ctx)
	if err != nil {
		return err
	}
	return u.lockoutBetween(auth.NewSnapshot(data.Users, data.Roles, data.Settings),
		auth.NewSnapshot(change(append([]meta.User(nil), data.Users...)), data.Roles, data.Settings))
}

// lockoutBetween is checkLockout's rule for the change from before to after.
func (u *UI) lockoutBetween(before, after *auth.Snapshot) error {
	if auth.RestorableAdmins(before) > 0 && auth.RestorableAdmins(after) == 0 {
		return auth.ErrLockout
	}
	if auth.CheckLockout(before) == nil {
		if err := auth.CheckLockout(after); err != nil {
			return err
		}
	}
	if u.accessChecked != nil {
		u.accessChecked()
	}
	return nil
}

// applied reloads the live snapshot after a write; when invalidate is set it
// also ends the user's sessions and password cache. It reports false after
// having answered the request.
func (u *UI) applied(w http.ResponseWriter, r *http.Request, invalidate string) bool {
	if err := u.reloadAccess(r.Context()); err != nil {
		u.fail(w, r, err)
		return false
	}
	if invalidate != "" {
		if err := u.Auth.OnUserChanged(invalidate); err != nil {
			u.fail(w, r, err)
			return false
		}
	}
	return true
}

// ---- users ----
// GET /-/api/users is listUsers in info.go; it carries these fields too.

// storedAccess is a snapshot of the stored users, roles and settings: what
// a check under the access lock must see, even before this node reloads
// another node's change.
func (u *UI) storedAccess(ctx context.Context) (*auth.Snapshot, error) {
	data, err := u.DB.LoadAccess(ctx)
	if err != nil {
		return nil, err
	}
	return auth.NewSnapshot(data.Users, data.Roles, data.Settings), nil
}

// adminOnly answers 403 and reports true when a delegated (non-admin)
// caller would grant, change or remove what makes someone an admin: the
// stored roles (cur) or the new ones (next) of a user are, or contain, the
// admin role.
func (u *UI) adminOnly(w http.ResponseWriter, r *http.Request, p auth.Principal, cur, next []string) bool {
	if p.Admin {
		return false
	}
	s, err := u.storedAccess(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return true
	}
	if s.IsAdmin(cur...) || s.IsAdmin(next...) {
		writeError(w, http.StatusForbidden, errAdminOnly.Error())
		return true
	}
	return false
}

// localOnly answers 403 and reports true when a delegated (non-admin)
// caller would manage a record of source other than local: an override
// record can disable or empower a directory or SSO user whose roles here
// (an LDAP group mapped to admin) the caller cannot see.
func localOnly(w http.ResponseWriter, p auth.Principal, source string) bool {
	if p.Admin || source == meta.SourceLocal {
		return false
	}
	writeError(w, http.StatusForbidden, "only an admin can manage the records of LDAP, OIDC, Crowd, RUT and SAML users")
	return true
}

// checkLastName checks the Nexus API's lastName: the end of name.
func checkLastName(v *validation, name, last string) {
	if last != "" && name != last && !strings.HasSuffix(name, " "+last) {
		v.add("lastName must be the end of name")
	}
}

func (u *UI) getUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.requireApp(w, r, "users", meta.ActionRead); !ok {
		return
	}
	x, err := u.DB.GetUser(r.Context(), r.PathValue("id"))
	if err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, userJSON(x))
}

func (u *UI) createUser(w http.ResponseWriter, r *http.Request) {
	p, ok := u.requireAppInteractive(w, r, "users", meta.ActionAdd)
	if !ok {
		return
	}
	var body struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		LastName string   `json:"lastName"` // the end of name that is the family name (the Nexus API's lastName)
		Email    string   `json:"email"`
		Password string   `json:"password"`
		Source   string   `json:"source"`
		Status   string   `json:"status"`
		Roles    []string `json:"roles"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	unlock, ok := u.lockAccess(w, r) // load, lockout check, write and reload as one step (final review I3)
	if !ok {
		return
	}
	defer unlock()
	var v validation
	if err := checkUserID(body.ID); err != nil {
		v = append(v, err.(*repo.ValidationError).Errors...)
	}
	if body.Source == "" {
		body.Source = meta.SourceLocal
	}
	if body.Status == "" {
		body.Status = meta.StatusActive
	}
	if body.Source != meta.SourceLocal && body.Source != meta.SourceLDAP && body.Source != meta.SourceOIDC && body.Source != meta.SourceCrowd && body.Source != meta.SourceRUT && body.Source != meta.SourceSAML {
		v.add("source must be %q, %q, %q, %q, %q or %q", meta.SourceLocal, meta.SourceLDAP, meta.SourceOIDC, meta.SourceCrowd, meta.SourceRUT, meta.SourceSAML)
	}
	if body.Status != meta.StatusActive && body.Status != meta.StatusDisabled {
		v.add("status must be %q or %q", meta.StatusActive, meta.StatusDisabled)
	}
	checkText(&v, "name", body.Name)
	checkLastName(&v, body.Name, body.LastName)
	checkText(&v, "email", body.Email)
	checkEmail(&v, body.Email)
	hash := ""
	switch {
	case body.Source != meta.SourceLocal && body.Password != "":
		v.add("an LDAP, OIDC, Crowd, RUT or SAML user has no local password")
	case body.Source == meta.SourceLocal:
		if err := u.checkPassword(body.Password); err != nil {
			v = append(v, err.(*repo.ValidationError).Errors...)
		}
	}
	if err := u.checkRoleRefs(r.Context(), &v, body.Roles); err != nil {
		u.fail(w, r, err)
		return
	}
	if err := v.err(); err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if localOnly(w, p, body.Source) || u.adminOnly(w, r, p, nil, body.Roles) {
		return
	}
	if body.Source == meta.SourceLocal {
		var err error
		if hash, err = auth.HashPassword(body.Password); err != nil {
			u.fail(w, r, err)
			return
		}
	}
	roles := append([]string{}, body.Roles...)
	nu := meta.User{ID: body.ID, Name: body.Name, LastName: body.LastName, Email: body.Email, PasswordHash: hash,
		Source: body.Source, Status: body.Status, Roles: roles}
	if err := u.DB.CreateUser(r.Context(), nu); err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if !u.applied(w, r, "") {
		return
	}
	u.emit(r, "user.create", "", nu.ID, "")
	got, err := u.DB.GetUser(r.Context(), nu.ID)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, userJSON(got))
}

func (u *UI) updateUser(w http.ResponseWriter, r *http.Request) {
	p, ok := u.requireAppInteractive(w, r, "users", meta.ActionEdit)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var body struct {
		Name     string   `json:"name"`
		LastName *string  `json:"lastName"` // absent keeps the stored one
		Email    string   `json:"email"`
		Status   string   `json:"status"` // empty keeps the stored status
		Roles    []string `json:"roles"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	unlock, ok := u.lockAccess(w, r) // load, lockout check, write and reload as one step (final review I3)
	if !ok {
		return
	}
	defer unlock()
	cur, err := u.DB.GetUser(r.Context(), id)
	if err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	var v validation
	if body.Status == "" {
		body.Status = cur.Status
	}
	if body.Status != meta.StatusActive && body.Status != meta.StatusDisabled {
		v.add("status must be %q or %q", meta.StatusActive, meta.StatusDisabled)
	}
	// Values stored by an older version are not re-checked unless they
	// change, so such a user can still be disabled from the UI.
	if body.Name != cur.Name {
		checkText(&v, "name", body.Name)
	}
	last := cur.LastName
	if body.LastName != nil {
		last = *body.LastName
		checkLastName(&v, body.Name, last)
	}
	if body.Email != cur.Email {
		checkText(&v, "email", body.Email)
		checkEmail(&v, body.Email)
	}
	if err := u.checkRoleRefs(r.Context(), &v, body.Roles); err != nil {
		u.fail(w, r, err)
		return
	}
	if err := v.err(); err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if localOnly(w, p, cur.Source) || u.adminOnly(w, r, p, cur.Roles, body.Roles) {
		return
	}
	next := cur
	next.Name, next.LastName, next.Email, next.Status = body.Name, last, body.Email, body.Status
	next.Roles = append([]string{}, body.Roles...)
	err = u.checkLockout(r.Context(), func(users []meta.User) []meta.User {
		for i := range users {
			if users[i].ID == id {
				users[i] = next
			}
		}
		return users
	})
	if err == nil {
		err = u.DB.UpdateUser(r.Context(), next)
	}
	if err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	invalidate := ""
	if next.Status == meta.StatusDisabled && cur.Status != meta.StatusDisabled {
		invalidate = id
	}
	if !u.applied(w, r, invalidate) {
		return
	}
	u.emit(r, "user.update", "", id, "")
	got, err := u.DB.GetUser(r.Context(), id)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, userJSON(got))
}

func (u *UI) deleteUser(w http.ResponseWriter, r *http.Request) {
	p, ok := u.requireAppInteractive(w, r, "users", meta.ActionDelete)
	if !ok {
		return
	}
	id := r.PathValue("id")
	unlock, ok := u.lockAccess(w, r) // load, lockout check, write and reload as one step (final review I3)
	if !ok {
		return
	}
	defer unlock()
	cur, err := u.DB.GetUser(r.Context(), id)
	if err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if cur.Source != meta.SourceLocal && cur.Status == meta.StatusDisabled {
		// The disabled override row is what keeps the directory or provider
		// user out (QA N-20): deleting it would let them sign in again unannounced.
		writeError(w, http.StatusConflict, "this disabled record blocks "+strings.ToUpper(cur.Source)+" user "+cur.ID+"; deleting it lets them sign in again (set it active first)")
		return
	}
	if localOnly(w, p, cur.Source) || u.adminOnly(w, r, p, cur.Roles, nil) {
		return
	}
	err = u.checkLockout(r.Context(), func(users []meta.User) []meta.User {
		out := users[:0]
		for _, x := range users {
			if x.ID != id {
				out = append(out, x)
			}
		}
		return out
	})
	if err == nil && id == p.User {
		// As Nexus: nobody deletes the account they are signed in with.
		writeError(w, http.StatusConflict, selfDeleteMessage)
		return
	}
	if err == nil {
		err = u.DB.DeleteUser(r.Context(), id) // its tokens go in the same transaction
	}
	if err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if !u.applied(w, r, id) {
		return
	}
	u.emit(r, "user.delete", "", id, "")
	w.WriteHeader(http.StatusNoContent)
}

func (u *UI) resetPassword(w http.ResponseWriter, r *http.Request) {
	p, ok := u.requireAppInteractive(w, r, "users", meta.ActionEdit)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var body struct {
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	unlock, ok := u.lockAccess(w, r) // load, lockout check, write and reload as one step (final review I3)
	if !ok {
		return
	}
	defer unlock()
	cur, err := u.DB.GetUser(r.Context(), id)
	if err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if cur.Source != meta.SourceLocal {
		u.writeAccessError(w, r, &repo.ValidationError{Errors: []string{"only a local user has a password"}})
		return
	}
	if u.adminOnly(w, r, p, cur.Roles, nil) {
		return
	}
	if err := u.checkPassword(body.Password); err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	cur.PasswordHash = hash
	if err := u.DB.UpdateUser(r.Context(), cur); err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if !u.applied(w, r, id) {
		return
	}
	u.emit(r, "user.password", "", id, "reset")
	w.WriteHeader(http.StatusNoContent)
}

// revokeUserTokens deletes every API token of the user, local or LDAP
// (QA N-16). A password change or reset leaves tokens valid; after a
// suspected leak this ends them all. It answers how many it deleted.
func (u *UI) revokeUserTokens(w http.ResponseWriter, r *http.Request) {
	p, ok := u.requireAppInteractive(w, r, "users", meta.ActionEdit)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if cur, err := u.DB.GetUser(r.Context(), id); !p.Admin && err != nil {
		writeError(w, http.StatusForbidden, "only an admin can revoke the tokens of a user without a local record")
		return
	} else if err == nil && (localOnly(w, p, cur.Source) || u.adminOnly(w, r, p, cur.Roles, nil)) {
		return
	}
	n, err := u.Auth.RevokeUserTokens(r.Context(), id)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "token.revoke", "", id, fmt.Sprintf("all %d", n))
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n})
}

// changeOwnPassword lets any signed-in user (password or session, not a
// token) change their own local password after proving the current one.
func (u *UI) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	p, ok := requireInteractive(w, r)
	if !ok || !requireUIHeader(w, r) {
		return
	}
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if p.IsLDAP() {
		writeError(w, http.StatusBadRequest, "password is managed by LDAP")
		return
	}
	// The local realm only: a same-named LDAP user's password must not do.
	_, err := u.Auth.CheckLocalPassword(r.Context(), p.User, body.CurrentPassword)
	if wait := auth.RetryAfter(w.Header(), err); wait > 0 {
		writeThrottled(w, wait)
		return
	}
	if err != nil {
		writeError(w, http.StatusForbidden, wrongPasswordMessage)
		return
	}
	if err := u.checkPassword(body.NewPassword); err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if body.NewPassword == body.CurrentPassword { // QA N-21
		u.writeAccessError(w, r, &repo.ValidationError{Errors: []string{"the new password must differ from the current one"}})
		return
	}
	unlock, ok := u.lockAccess(w, r) // every access write is serialised (final review I3)
	if !ok {
		return
	}
	defer unlock()
	cur, err := u.DB.GetUser(r.Context(), p.User)
	if err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	hash, err := auth.HashPassword(body.NewPassword)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	cur.PasswordHash = hash
	if err := u.DB.UpdateUser(r.Context(), cur); err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if !u.applied(w, r, p.User) { // ends every session of the user, this one included
		return
	}
	if p.Via == auth.ViaSession && u.Auth.Sessions != nil {
		id, err := u.Auth.Sessions.Create(r.Context(), p.User)
		if err != nil {
			u.fail(w, r, err)
			return
		}
		http.SetCookie(w, u.cookie(id, sessionMaxAge))
	}
	u.emit(r, "user.password", "", p.User, "self")
	w.WriteHeader(http.StatusNoContent)
}

// ---- roles ----

// NexusPrivRolePrefix starts the id of a role the Nexus API's privilege
// endpoints manage (one privilege each, so Nexus role assignment works):
// "priv:<privilege name>". The rest of the id follows the role name rules.
const NexusPrivRolePrefix = "priv:"

func checkRoleID(id string) error {
	if rest, ok := strings.CutPrefix(id, NexusPrivRolePrefix); ok {
		return repo.CheckName("role", rest)
	}
	return repo.CheckName("role", id)
}

func (u *UI) listRoles(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.requireApp(w, r, "roles", meta.ActionRead); !ok {
		return
	}
	list, err := u.DB.ListRoles(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	out := make([]roleDTO, 0, len(list)+1)
	hasAdmin := false
	for _, x := range list {
		hasAdmin = hasAdmin || x.ID == meta.AdminRole
		out = append(out, roleJSON(x))
	}
	if !hasAdmin {
		out = append([]roleDTO{roleJSON(meta.BuiltinAdminRole())}, out...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": out})
}

func (u *UI) getRole(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.requireApp(w, r, "roles", meta.ActionRead); !ok {
		return
	}
	x, err := u.DB.GetRole(r.Context(), r.PathValue("id"))
	if err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, roleJSON(x))
}

func toPrivileges(in []privilegeDTO) []meta.Privilege {
	out := make([]meta.Privilege, 0, len(in))
	for _, p := range in {
		np := meta.Privilege{Target: p.Target, Actions: append([]string{}, p.Actions...)}
		if strings.HasPrefix(p.Target, meta.SelectorPrefix) {
			np.Repos = append([]string{}, p.Repos...)
		}
		out = append(out, np)
	}
	return out
}

// checkRoleChange applies the stored role cur (nil for a new one) becoming
// next and refuses it when a contained role is unknown, repeated, the role
// itself or leads back to it (validation, into v); when a delegated
// caller p would change what makes someone an admin (errAdminOnly); when a
// default role would grant admin (validation); or when no admin could sign
// in after it (auth.ErrLockout).
func (u *UI) checkRoleChange(ctx context.Context, v *validation, p auth.Principal, cur *meta.Role, next meta.Role) error {
	data, err := u.DB.LoadAccess(ctx)
	if err != nil {
		return err
	}
	before := auth.NewSnapshot(data.Users, data.Roles, data.Settings)
	have := map[string]bool{meta.AdminRole: true}
	roles := []meta.Role{next}
	for _, x := range data.Roles {
		have[x.ID] = true
		if x.ID != next.ID {
			roles = append(roles, x)
		}
	}
	seen := map[string]bool{}
	for _, id := range next.Roles {
		switch {
		case seen[id]:
			v.add("roles: duplicate role %q", id)
		case id == next.ID:
			v.add("roles: role %q cannot contain itself", id)
		case !have[id]:
			v.add("roles: unknown role %q", id)
		}
		seen[id] = true
	}
	if len(*v) > 0 {
		return v.err()
	}
	if before.ContainsCycle(next.ID, next.Roles) {
		v.add("roles: role %q cannot contain itself, directly or through the roles it contains", next.ID)
		return v.err()
	}
	after := auth.NewSnapshot(data.Users, roles, data.Settings)
	if !p.Admin && (after.IsAdmin(next.ID) || cur != nil && before.IsAdmin(cur.ID)) {
		return errAdminOnly
	}
	for _, d := range data.Settings.DefaultRoles {
		if after.IsAdmin(d) {
			v.add("roles: the default role %q would grant the admin role", d)
			return v.err()
		}
	}
	return u.lockoutBetween(before, after)
}

func (u *UI) createRole(w http.ResponseWriter, r *http.Request) {
	p, ok := u.requireAppInteractive(w, r, "roles", meta.ActionAdd)
	if !ok {
		return
	}
	var body struct {
		ID          string         `json:"id"`
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Privileges  []privilegeDTO `json:"privileges"`
		Roles       []string       `json:"roles"`
		Nexus       string         `json:"nexus"` // the Nexus API's form, kept for its reads
	}
	if !decodeBody(w, r, &body) {
		return
	}
	unlock, ok := u.lockAccess(w, r) // load, lockout check, write and reload as one step (final review I3)
	if !ok {
		return
	}
	defer unlock()
	var v validation
	if err := checkRoleID(body.ID); err != nil {
		v = append(v, err.(*repo.ValidationError).Errors...)
	} else if strings.EqualFold(body.ID, meta.AdminRole) {
		v.add("role id %q is reserved for the built-in administrator role", body.ID)
	}
	checkText(&v, "name", body.Name)
	checkText(&v, "description", body.Description)
	u.validatePrivileges(&v, body.Privileges, nil)
	if err := v.err(); err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	nr := meta.Role{ID: body.ID, Name: body.Name, Description: body.Description, Privileges: toPrivileges(body.Privileges),
		Roles: append([]string{}, body.Roles...), Nexus: body.Nexus}
	err := u.checkRoleChange(r.Context(), &v, p, nil, nr)
	if err == nil {
		err = u.DB.CreateRole(r.Context(), nr)
	}
	if err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if !u.applied(w, r, "") {
		return
	}
	u.emit(r, "role.create", "", nr.ID, "")
	got, err := u.DB.GetRole(r.Context(), nr.ID)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, roleJSON(got))
}

func (u *UI) updateRole(w http.ResponseWriter, r *http.Request) {
	p, ok := u.requireAppInteractive(w, r, "roles", meta.ActionEdit)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if id == meta.AdminRole {
		writeError(w, http.StatusConflict, "the admin role cannot be modified")
		return
	}
	var body struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Privileges  []privilegeDTO `json:"privileges"`
		Roles       []string       `json:"roles"` // the contained roles (absent: none)
		Nexus       string         `json:"nexus"` // the Nexus API's form; "" drops it
	}
	if !decodeBody(w, r, &body) {
		return
	}
	unlock, ok := u.lockAccess(w, r) // load, lockout check, write and reload as one step (final review I3)
	if !ok {
		return
	}
	defer unlock()
	cur, err := u.DB.GetRole(r.Context(), id)
	if err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	var v validation
	if body.Description != cur.Description { // a stored value is not re-checked
		checkText(&v, "description", body.Description)
	}
	nr := meta.Role{ID: id, Name: body.Name, Description: body.Description, Privileges: toPrivileges(body.Privileges),
		Roles: append([]string{}, body.Roles...), Nexus: body.Nexus}
	if nr.Name != cur.Name {
		checkText(&v, "name", nr.Name)
	}
	u.validatePrivileges(&v, body.Privileges, cur.Privileges)
	if err := v.err(); err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	err = u.checkRoleChange(r.Context(), &v, p, &cur, nr)
	if err == nil {
		err = u.DB.UpdateRole(r.Context(), nr)
	}
	if err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if !u.applied(w, r, "") {
		return
	}
	u.emit(r, "role.update", "", id, "")
	got, err := u.DB.GetRole(r.Context(), id)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, roleJSON(got))
}

func (u *UI) deleteRole(w http.ResponseWriter, r *http.Request) {
	p, ok := u.requireAppInteractive(w, r, "roles", meta.ActionDelete)
	if !ok {
		return
	}
	id := r.PathValue("id")
	unlock, ok := u.lockAccess(w, r) // load, lockout check, write and reload as one step (final review I3)
	if !ok {
		return
	}
	defer unlock()
	if id == meta.AdminRole {
		writeError(w, http.StatusConflict, "the admin role cannot be deleted")
		return
	}
	if u.adminOnly(w, r, p, []string{id}, nil) {
		return
	}
	// A role named by the LDAP settings would leave a dangling mapping
	// (final review M2); putLDAP takes the access lock too, so the check holds.
	set, err := u.DB.LoadSettings(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	if ref := ldapRoleRef(set.LDAP, id); ref != "" {
		writeError(w, http.StatusConflict, roleInUseMessage+" by the "+ref)
		return
	}
	if ref := ldapRoleRef(set.OIDC.Mapping(), id); ref != "" {
		writeError(w, http.StatusConflict, roleInUseMessage+" by the "+strings.Replace(ref, "LDAP", "OIDC", 1))
		return
	}
	if ref := ldapRoleRef(set.RUT.Mapping(), id); ref != "" {
		writeError(w, http.StatusConflict, roleInUseMessage+" by the "+strings.Replace(ref, "LDAP", "RUT", 1))
		return
	}
	if contains(set.DefaultRoles, id) {
		writeError(w, http.StatusConflict, roleInUseMessage+" by the defaultRoles setting")
		return
	}
	if ref := ldapRoleRef(set.Crowd.Mapping(), id); ref != "" {
		writeError(w, http.StatusConflict, roleInUseMessage+" by the "+strings.Replace(ref, "LDAP", "Crowd", 1))
		return
	}
	if ref := ldapRoleRef(set.SAML.Mapping(), id); ref != "" {
		writeError(w, http.StatusConflict, roleInUseMessage+" by the "+strings.Replace(ref, "LDAP", "SAML", 1))
		return
	}
	// A role held by a user or contained in another role is refused by the
	// store (ErrInUse), so deleting one never changes who is an admin.
	if err := u.DB.DeleteRole(r.Context(), id); err != nil {
		u.writeAccessError(w, r, err)
		return
	}
	if !u.applied(w, r, "") {
		return
	}
	u.emit(r, "role.delete", "", id, "")
	w.WriteHeader(http.StatusNoContent)
}

// ldapRoleRef names where the LDAP settings use role id ("" if nowhere):
// the first group mapping that grants it, or the default roles.
func ldapRoleRef(l meta.LDAP, id string) string {
	for _, m := range l.GroupMappings {
		if contains(m.Roles, id) {
			return fmt.Sprintf("LDAP group mapping %q", m.Group)
		}
	}
	if contains(l.DefaultRoles, id) {
		return "LDAP default roles"
	}
	return ""
}
