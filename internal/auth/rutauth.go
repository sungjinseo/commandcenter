package auth

import (
	"context"
	"net/http"
	"reflect"
	"strings"

	"commandcenter/internal/ldap"
	"commandcenter/internal/meta"
)

// Remote User Token realm (meta.RUT). The gateway in front of Command
// Center authenticates every request itself, so there is no login here:
// a request that carries the configured header, comes from a trusted proxy
// (its RemoteAddr: a forwarded address is just another header) and has no
// Authorization is the named user, on every request. Tokens of such a user
// ("rutauth:<id>") resolve through an in-memory entry the last trusted
// request left, as OIDC tokens do, and fail closed after a restart until
// the user comes through the gateway again.

const (
	rutOwnerPrefix = "rutauth:"
	sourceRUT      = "rutauth"
	rutMax         = 1000

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

func (a *Authenticator) clearRUT() {
	a.rutMu.Lock()
	a.rutUsers = map[string][]string{}
	a.rutMu.Unlock()
}

func (a *Authenticator) dropRUT(id string) {
	a.rutMu.Lock()
	delete(a.rutUsers, id)
	a.rutMu.Unlock()
}

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
	a.rutMu.Lock()
	if a.rutUsers == nil || len(a.rutUsers) >= rutMax {
		a.rutUsers = map[string][]string{}
	}
	a.rutUsers[id] = groups
	a.rutMu.Unlock()
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
	return Principal{User: id, Admin: hasRole(roles, meta.AdminRole), roles: roles, source: sourceRUT, Via: ViaRUT}, true
}

// rutLookup resolves a token owner "rutauth:<id>" from the cache, failing
// closed when the user has not come through the gateway since the start.
func (a *Authenticator) rutLookup(_ context.Context, id string) (Principal, bool) {
	s := a.snap.Load()
	if !rutUsable(s) {
		return Principal{}, false
	}
	a.rutMu.Lock()
	groups, ok := a.rutUsers[id]
	a.rutMu.Unlock()
	if !ok {
		return Principal{}, false
	}
	return a.rutBuild(s, id, groups)
}

func splitRUTOwner(owner string) (id string, isRUT bool) {
	return strings.CutPrefix(owner, rutOwnerPrefix)
}
