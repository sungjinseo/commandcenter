package auth

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"commandcenter/internal/ldap"
	"commandcenter/internal/meta"
)

// OIDC login. The browser flow (ui/sso.go) verifies an ID token and calls
// RecordOIDC with the identity; sessions and tokens of the user
// ("oidc:<id>") resolve through the in-memory entry it leaves. There is no
// re-check against the provider: an entry lives until the cache is cleared
// (a restart, or a changed OIDC configuration), after which the browser
// simply signs in again and a token fails closed until it does. Roles are
// computed on every use from the live snapshot, as for LDAP: the group
// mappings, the default roles and an optional override row (source=oidc,
// same id) whose roles are added and which denies the user when disabled.

const (
	oidcOwnerPrefix = "oidc:"
	sourceOIDC      = "oidc"
	oidcMax         = 1000
)

// OIDCUser is an identity the provider asserted, with the roles it maps to
// under the live snapshot (filled by OIDCUsers).
type OIDCUser struct {
	ID, Name, Email string
	Groups          []string
	Roles           []string
}

type oidcEntry struct {
	user      OIDCUser
	checkedAt time.Time
}

// IsOIDC reports whether p signed in through the oidc realm.
func (p Principal) IsOIDC() bool { return p.source == sourceOIDC }

// oidcUsable reports whether the oidc realm is on and configured.
func oidcUsable(s *Snapshot) bool {
	return s.realms[meta.RealmOIDC] && s.settings.OIDC.Complete()
}

// oidcChanged reports whether the OIDC configuration differs between snapshots.
func oidcChanged(old, cur *Snapshot) bool {
	return old == nil || !reflect.DeepEqual(old.settings.OIDC, cur.settings.OIDC)
}

func (a *Authenticator) clearOIDC() {
	a.oidcMu.Lock()
	a.oidcUsers = map[string]oidcEntry{}
	a.oidcMu.Unlock()
}

func (a *Authenticator) dropOIDC(id string) {
	a.oidcMu.Lock()
	delete(a.oidcUsers, id)
	a.oidcMu.Unlock()
}

// ErrOIDCRefused reports an identity the realm does not admit: the realm is
// off or an override row disables the user.
var ErrOIDCRefused = errors.New("the user is not allowed to sign in")

// RecordOIDC notes that the provider asserted u (u.ID already validated by
// the caller) and returns its principal, or ErrOIDCRefused.
func (a *Authenticator) RecordOIDC(u OIDCUser) (Principal, error) {
	s := a.snap.Load()
	if !oidcUsable(s) || !meta.ValidUserID(u.ID) {
		return Principal{}, ErrOIDCRefused
	}
	u.Groups = append([]string{}, u.Groups...)
	u.Roles = nil
	p, ok := a.oidcPrincipal(s, u)
	if !ok {
		return Principal{}, ErrOIDCRefused
	}
	a.oidcMu.Lock()
	if a.oidcUsers == nil || len(a.oidcUsers) >= oidcMax {
		a.oidcUsers = map[string]oidcEntry{}
	}
	a.oidcUsers[u.ID] = oidcEntry{user: u, checkedAt: a.clock()}
	a.oidcMu.Unlock()
	return p, nil
}

// oidcPrincipal builds the principal of u under s. A disabled override row
// denies it.
func (a *Authenticator) oidcPrincipal(s *Snapshot, u OIDCUser) (Principal, bool) {
	roles, _ := ldap.MapRoles(s.settings.OIDC.Mapping(), u.Groups)
	if row, ok := s.users[u.ID]; ok && row.Source == meta.SourceOIDC {
		if row.Status != meta.StatusActive {
			return Principal{}, false
		}
		for _, r := range row.Roles {
			if !hasRole(roles, r) {
				roles = append(roles, r)
			}
		}
	}
	roles = s.grant(roles)
	return Principal{User: u.ID, Admin: hasRole(roles, meta.AdminRole), roles: roles, source: sourceOIDC}, true
}

// oidcLookup returns the principal of OIDC user id from the cache, failing
// closed when it is not there (as after a restart).
func (a *Authenticator) oidcLookup(_ context.Context, id string) (Principal, bool) {
	s := a.snap.Load()
	if !oidcUsable(s) {
		return Principal{}, false
	}
	a.oidcMu.Lock()
	e, ok := a.oidcUsers[id]
	a.oidcMu.Unlock()
	if !ok {
		return Principal{}, false
	}
	return a.oidcPrincipal(s, e.user)
}

// OIDCUsers returns the cached OIDC users that are currently allowed, with
// their roles under the live snapshot, ordered by id.
func (a *Authenticator) OIDCUsers() []OIDCUser {
	s := a.snap.Load()
	if !oidcUsable(s) {
		return nil
	}
	a.oidcMu.Lock()
	entries := make([]oidcEntry, 0, len(a.oidcUsers))
	for _, e := range a.oidcUsers {
		entries = append(entries, e)
	}
	a.oidcMu.Unlock()
	var out []OIDCUser
	for _, e := range entries {
		if p, ok := a.oidcPrincipal(s, e.user); ok {
			u := e.user
			u.Roles = p.roles
			out = append(out, u)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// splitOIDCOwner splits a token owner or session key into an OIDC id.
func splitOIDCOwner(owner string) (id string, isOIDC bool) {
	return strings.CutPrefix(owner, oidcOwnerPrefix)
}
