package auth

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"commandcenter/internal/ldap"
	"commandcenter/internal/meta"
)

// OIDC login. The browser flow (ui/sso.go) verifies an ID token and calls
// RecordOIDC with the identity; sessions and tokens of the user
// ("oidc:<id>") resolve through the identity it records (identity.go).
// There is no re-check against the provider: an identity lasts until a
// changed OIDC configuration, or a restart without Shared, or on
// PostgreSQL meta.SessionMaxAge after the login; then the browser simply
// signs in again and a token fails closed until it does. Roles are
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

func (a *Authenticator) clearOIDC() { a.oidcUsers.clear() }

func (a *Authenticator) dropOIDC(id string) { a.oidcUsers.drop(id) }

// ErrOIDCRefused reports an identity the realm does not admit: the realm is
// off or an override row disables the user.
var ErrOIDCRefused = errors.New("the user is not allowed to sign in")

// RecordOIDC notes that the provider asserted u (u.ID already validated by
// the caller) and returns its principal, or ErrOIDCRefused.
func (a *Authenticator) RecordOIDC(ctx context.Context, u OIDCUser) (Principal, error) {
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
	if err := a.recordSSO(ctx, s, &a.oidcUsers, sourceOIDC, u); err != nil {
		return Principal{}, err
	}
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

// oidcLookup returns the principal of OIDC user id from its recorded
// identity, failing closed without one (as after a restart without Shared).
func (a *Authenticator) oidcLookup(ctx context.Context, id string) (Principal, bool) {
	s := a.snap.Load()
	if !oidcUsable(s) {
		return Principal{}, false
	}
	u, ok := a.ssoUser(ctx, s, &a.oidcUsers, sourceOIDC, id)
	if !ok {
		return Principal{}, false
	}
	return a.oidcPrincipal(s, u)
}

// OIDCUsers returns the signed-in OIDC users that are currently allowed,
// with their roles under the live snapshot, ordered by id.
func (a *Authenticator) OIDCUsers() []OIDCUser {
	s := a.snap.Load()
	if !oidcUsable(s) {
		return nil
	}
	return listed(s, a.ssoList(s, &a.oidcUsers, sourceOIDC), a.oidcPrincipal)
}

// splitOIDCOwner splits a token owner or session key into an OIDC id.
func splitOIDCOwner(owner string) (id string, isOIDC bool) {
	return strings.CutPrefix(owner, oidcOwnerPrefix)
}
