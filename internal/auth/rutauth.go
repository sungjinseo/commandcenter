package auth

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"commandcenter/internal/ldap"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
)

// Remote User Token realm (meta.RUT). The gateway in front of Command
// Center authenticates every request itself, so there is no login here:
// a request that carries the configured header, comes from a trusted proxy
// (its RemoteAddr: a forwarded address is just another header) and has no
// Authorization is the named user, on every request. Tokens of such a user
// ("rutauth:<id>") resolve through the identity the last trusted request
// recorded (identity.go), as OIDC tokens do, and fail closed without one
// (after a restart without Shared) until the user comes through the
// gateway again.

const (
	rutOwnerPrefix = "rutauth:"
	sourceRUT      = "rutauth"

	// ViaRUT marks a principal the trusted proxy named; the UI treats it
	// as a signed-in session (requireInteractive).
	ViaRUT Via = "rutauth"
)

// IsRUT reports whether p was named by the trusted proxy's header.
func (p Principal) IsRUT() bool { return p.source == sourceRUT }

func rutUsable(s *Snapshot) bool { return s.realms[meta.RealmRUT] && s.settings.RUT.Complete() }

func rutChanged(old, cur *Snapshot) bool {
	return old == nil || !reflect.DeepEqual(old.settings.RUT, cur.settings.RUT)
}

func (a *Authenticator) clearRUT() { a.rutUsers.clear() }

func (a *Authenticator) dropRUT(id string) { a.rutUsers.drop(id) }

// rutPrincipal authenticates r through the realm; ok is false when r is not
// a trusted proxy's request for a valid user.
func (a *Authenticator) rutPrincipal(r *http.Request) (Principal, bool) {
	s := a.snap.Load()
	if !rutUsable(s) {
		return Principal{}, false
	}
	cfg := s.settings.RUT
	id := strings.ToLower(strings.TrimSpace(r.Header.Get(cfg.Header)))
	if id == "" || !cfg.Trusts(r.RemoteAddr) || !meta.ValidUserID(id) {
		return Principal{}, false
	}
	var groups []string
	if cfg.GroupHeader != "" {
		for _, g := range strings.Split(r.Header.Get(cfg.GroupHeader), ",") {
			if g = strings.TrimSpace(g); g != "" {
				groups = append(groups, g)
			}
		}
	}
	p, ok := a.rutBuild(s, id, groups)
	if !ok {
		return Principal{}, false
	}
	// The gateway asserts the user on every request: the shared identity is
	// written when it is new or changed, and otherwise once an hour.
	u, now := OIDCUser{ID: id, Groups: groups}, a.clock()
	e, seen := a.rutUsers.get(id)
	if a.Shared == nil || seen && slices.Equal(e.user.Groups, groups) && now.Sub(e.checkedAt) < identityRefresh {
		if !seen {
			e.checkedAt = now
		}
		a.rutUsers.put(id, ssoEntry{user: u, checkedAt: e.checkedAt, cachedAt: now})
	} else if err := a.recordSSO(r.Context(), s, &a.rutUsers, sourceRUT, u); err != nil {
		a.logger().Warn("could not record a gateway user for the other nodes; its tokens work here only", "user", id, "err", logsafe.Err(err))
	}
	return p, true
}

// rutBuild is the principal of id with groups under s; a disabled override
// row (source rutauth) denies it.
func (a *Authenticator) rutBuild(s *Snapshot, id string, groups []string) (Principal, bool) {
	roles, _ := ldap.MapRoles(s.settings.RUT.Mapping(), groups)
	if row, ok := s.users[id]; ok && row.Source == meta.SourceRUT {
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
	return Principal{User: id, Admin: s.isAdmin(roles), roles: roles, source: sourceRUT, Via: ViaRUT}, true
}

// rutLookup resolves a token owner "rutauth:<id>" from its recorded
// identity, failing closed when the user has not come through the gateway
// (since the start, without Shared).
func (a *Authenticator) rutLookup(ctx context.Context, id string) (Principal, bool) {
	s := a.snap.Load()
	if !rutUsable(s) {
		return Principal{}, false
	}
	u, ok := a.ssoUser(ctx, s, &a.rutUsers, sourceRUT, id)
	if !ok {
		return Principal{}, false
	}
	return a.rutBuild(s, id, u.Groups)
}

func splitRUTOwner(owner string) (id string, isRUT bool) {
	return strings.CutPrefix(owner, rutOwnerPrefix)
}
