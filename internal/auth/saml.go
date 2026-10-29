package auth

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"

	"commandcenter/internal/ldap"
	"commandcenter/internal/meta"
)

// SAML login, the OIDC pattern (oidc.go) for the saml realm: the browser
// flow (ui/saml.go) verifies a Response and calls RecordSAML; sessions and
// tokens of "saml:<id>" resolve through the in-memory entry it leaves,
// failing closed after a restart or a changed SAML configuration. Roles
// come from the live snapshot on every use: the group mappings, the
// default roles and an override row (source=saml, same id) whose roles are
// added and which denies the user when disabled.

const (
	samlOwnerPrefix = "saml:"
	sourceSAML      = "saml"
)

// SAMLUser is an identity the IdP asserted, with its roles under the live
// snapshot (filled by SAMLUsers).
type SAMLUser = OIDCUser

// IsSAML reports whether p signed in through the saml realm.
func (p Principal) IsSAML() bool { return p.source == sourceSAML }

// ErrSAMLRefused reports an identity the realm does not admit: the realm is
// off or an override row disables the user.
var ErrSAMLRefused = errors.New("the user is not allowed to sign in")

func samlUsable(s *Snapshot) bool { return s.realms[meta.RealmSAML] && s.settings.SAML.Complete() }

func samlChanged(old, cur *Snapshot) bool {
	return old == nil || !reflect.DeepEqual(old.settings.SAML, cur.settings.SAML)
}

func (a *Authenticator) clearSAML() {
	a.samlMu.Lock()
	a.samlUsers = map[string]oidcEntry{}
	a.samlMu.Unlock()
}

func (a *Authenticator) dropSAML(id string) {
	a.samlMu.Lock()
	delete(a.samlUsers, id)
	a.samlMu.Unlock()
}

// RecordSAML notes that the IdP asserted u (u.ID already validated by the
// caller) and returns its principal, or ErrSAMLRefused.
func (a *Authenticator) RecordSAML(u SAMLUser) (Principal, error) {
	s := a.snap.Load()
	if !samlUsable(s) || !meta.ValidUserID(u.ID) {
		return Principal{}, ErrSAMLRefused
	}
	u.Groups, u.Roles = slices.Clone(u.Groups), nil
	p, ok := a.samlPrincipal(s, u)
	if !ok {
		return Principal{}, ErrSAMLRefused
	}
	a.samlMu.Lock()
	if a.samlUsers == nil || len(a.samlUsers) >= oidcMax {
		a.samlUsers = map[string]oidcEntry{}
	}
	a.samlUsers[u.ID] = oidcEntry{user: u, checkedAt: a.clock()}
	a.samlMu.Unlock()
	return p, nil
}

func (a *Authenticator) samlPrincipal(s *Snapshot, u SAMLUser) (Principal, bool) {
	roles, _ := ldap.MapRoles(s.settings.SAML.Mapping(), u.Groups)
	if row, ok := s.users[u.ID]; ok && row.Source == meta.SourceSAML {
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
	return Principal{User: u.ID, Admin: hasRole(roles, meta.AdminRole), roles: roles, source: sourceSAML}, true
}

// samlLookup returns the principal of SAML user id from the cache.
func (a *Authenticator) samlLookup(_ context.Context, id string) (Principal, bool) {
	s := a.snap.Load()
	if !samlUsable(s) {
		return Principal{}, false
	}
	a.samlMu.Lock()
	e, ok := a.samlUsers[id]
	a.samlMu.Unlock()
	if !ok {
		return Principal{}, false
	}
	return a.samlPrincipal(s, e.user)
}

// SAMLUsers returns the cached SAML users that are currently allowed, with
// their roles under the live snapshot, ordered by id.
func (a *Authenticator) SAMLUsers() []SAMLUser {
	s := a.snap.Load()
	if !samlUsable(s) {
		return nil
	}
	a.samlMu.Lock()
	var out []SAMLUser
	for _, e := range a.samlUsers {
		if p, ok := a.samlPrincipal(s, e.user); ok {
			u := e.user
			u.Roles = p.roles
			out = append(out, u)
		}
	}
	a.samlMu.Unlock()
	slices.SortFunc(out, func(x, y SAMLUser) int { return strings.Compare(x.ID, y.ID) })
	return out
}

func splitSAMLOwner(owner string) (id string, isSAML bool) {
	return strings.CutPrefix(owner, samlOwnerPrefix)
}
