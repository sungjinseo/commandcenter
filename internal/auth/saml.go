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
// tokens of "saml:<id>" resolve through the identity it records
// (identity.go), failing closed after a changed SAML configuration, or a
// restart without Shared. Roles
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

func (a *Authenticator) clearSAML() { a.samlUsers.clear() }

func (a *Authenticator) dropSAML(id string) { a.samlUsers.drop(id) }

// RecordSAML notes that the IdP asserted u (u.ID already validated by the
// caller) and returns its principal, or ErrSAMLRefused.
func (a *Authenticator) RecordSAML(ctx context.Context, u SAMLUser) (Principal, error) {
	s := a.snap.Load()
	if !samlUsable(s) || !meta.ValidUserID(u.ID) {
		return Principal{}, ErrSAMLRefused
	}
	u.Groups, u.Roles = slices.Clone(u.Groups), nil
	p, ok := a.samlPrincipal(s, u)
	if !ok {
		return Principal{}, ErrSAMLRefused
	}
	if err := a.recordSSO(ctx, s, &a.samlUsers, sourceSAML, u); err != nil {
		return Principal{}, err
	}
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

// samlLookup returns the principal of SAML user id from its recorded identity.
func (a *Authenticator) samlLookup(ctx context.Context, id string) (Principal, bool) {
	s := a.snap.Load()
	if !samlUsable(s) {
		return Principal{}, false
	}
	u, ok := a.ssoUser(ctx, s, &a.samlUsers, sourceSAML, id)
	if !ok {
		return Principal{}, false
	}
	return a.samlPrincipal(s, u)
}

// SAMLUsers returns the signed-in SAML users that are currently allowed,
// with their roles under the live snapshot, ordered by id.
func (a *Authenticator) SAMLUsers() []SAMLUser {
	s := a.snap.Load()
	if !samlUsable(s) {
		return nil
	}
	return listed(s, a.ssoList(s, &a.samlUsers, sourceSAML), a.samlPrincipal)
}

func splitSAMLOwner(owner string) (id string, isSAML bool) {
	return strings.CutPrefix(owner, samlOwnerPrefix)
}
