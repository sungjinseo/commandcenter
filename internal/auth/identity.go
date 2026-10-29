package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
)

// The identities the SSO realms (oidc, saml, rutauth) asserted, which
// their sessions and tokens resolve through. With Shared (the app's) it is
// a row every node reads (meta.Identity), trusted for meta.SessionMaxAge after the
// login that asserted it and tied to a hash of the realm's configuration,
// so that no node uses it under another configuration; a node's own copy
// is read again once it is identityCache old, so that a row another node
// deleted stops working here too. Without Shared (unit tests) an identity
// lives in this process only.

const (
	identityCache = 30 * time.Second
	// identityRefresh is how often a gateway's (rutauth) repeated assertion
	// of an unchanged identity is written again.
	identityRefresh = time.Hour
)

type ssoEntry struct {
	user      OIDCUser
	checkedAt time.Time // the login that asserted it
	cachedAt  time.Time // when this node wrote or read it
}

// ssoUsers is one realm's identities on this node.
type ssoUsers struct {
	mu sync.Mutex
	m  map[string]ssoEntry
}

func (c *ssoUsers) get(id string) (ssoEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[id]
	return e, ok
}

func (c *ssoUsers) put(id string, e ssoEntry) {
	c.mu.Lock()
	if c.m == nil || len(c.m) >= oidcMax {
		c.m = map[string]ssoEntry{}
	}
	c.m[id] = e
	c.mu.Unlock()
}

func (c *ssoUsers) drop(id string) {
	c.mu.Lock()
	delete(c.m, id)
	c.mu.Unlock()
}

func (c *ssoUsers) clear() {
	c.mu.Lock()
	c.m = map[string]ssoEntry{}
	c.mu.Unlock()
}

func (c *ssoUsers) all() []OIDCUser {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]OIDCUser, 0, len(c.m))
	for _, e := range c.m {
		out = append(out, e.user)
	}
	return out
}

// identityJSON is meta.Identity's Data.
type identityJSON struct {
	Name   string   `json:"name,omitempty"`
	Email  string   `json:"email,omitempty"`
	Groups []string `json:"groups"`
}

func decodeIdentity(row meta.Identity) (OIDCUser, bool) {
	var x identityJSON
	if json.Unmarshal([]byte(row.Data), &x) != nil {
		return OIDCUser{}, false
	}
	return OIDCUser{ID: row.ID, Name: x.Name, Email: x.Email, Groups: x.Groups}, true
}

// realmConf is the hash an identity row of a realm is tied to: of the
// realm's configuration without its secret (the client secret, the
// service provider key or the application password), which says nothing
// about who signed in and must not be guessable from the row.
func realmConf(s *Snapshot, realm string) string {
	var v any
	switch realm {
	case sourceOIDC:
		c := s.settings.OIDC
		c.ClientSecret = ""
		v = c
	case sourceSAML:
		c := s.settings.SAML
		c.SPPrivateKey = ""
		v = c
	case sourceCrowd:
		c := s.settings.Crowd
		c.ApplicationPassword = ""
		v = c
	case sourceRUT:
		v = s.settings.RUT
	}
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(append([]byte(realm+"\x00"), b...))
	return hex.EncodeToString(sum[:16])
}

// shareIdentity stores u as realm's identity asserted at at, for the other
// nodes; without Shared it does nothing.
func (a *Authenticator) shareIdentity(ctx context.Context, s *Snapshot, realm string, u OIDCUser, at time.Time) error {
	if a.Shared == nil {
		return nil
	}
	b, _ := json.Marshal(identityJSON{Name: u.Name, Email: u.Email, Groups: u.Groups})
	return a.Shared.PutIdentity(ctx, realm, meta.Identity{ID: u.ID, Conf: realmConf(s, realm), Data: string(b), At: at})
}

// recordSSO keeps u as the identity realm asserted now, in c and shared.
func (a *Authenticator) recordSSO(ctx context.Context, s *Snapshot, c *ssoUsers, realm string, u OIDCUser) error {
	now := a.clock()
	if err := a.shareIdentity(ctx, s, realm, u, now); err != nil {
		return err
	}
	c.put(u.ID, ssoEntry{user: u, checkedAt: now, cachedAt: now})
	return nil
}

// ssoUser returns realm's identity id from c, or with Shared from its row
// once c's copy is identityCache old. It fails closed without one.
func (a *Authenticator) ssoUser(ctx context.Context, s *Snapshot, c *ssoUsers, realm, id string) (OIDCUser, bool) {
	e, ok := c.get(id)
	if a.Shared == nil {
		return e.user, ok
	}
	now := a.clock()
	if ok && now.Sub(e.cachedAt) < identityCache && now.Sub(e.checkedAt) < meta.SessionMaxAge {
		return e.user, true
	}
	row, err := a.Shared.GetIdentity(ctx, realm, id, realmConf(s, realm))
	if err == nil && now.Sub(row.At) >= meta.SessionMaxAge {
		err = meta.ErrNotFound
	}
	var u OIDCUser
	if err == nil {
		if u, ok = decodeIdentity(row); !ok {
			err = errors.New("the stored identity does not decode")
		}
	}
	if err != nil {
		c.drop(id)
		if !errors.Is(err, meta.ErrNotFound) {
			a.logger().Warn("could not read a signed-in identity: denying", "realm", realm, "user", id, "err", logsafe.Err(err))
		}
		return OIDCUser{}, false
	}
	c.put(id, ssoEntry{user: u, checkedAt: row.At, cachedAt: now})
	return u, true
}

// ssoList returns realm's identities: c's, or with Shared every node's.
func (a *Authenticator) ssoList(s *Snapshot, c *ssoUsers, realm string) []OIDCUser {
	if a.Shared == nil {
		return c.all()
	}
	rows, err := a.Shared.ListIdentities(context.Background(), realm, realmConf(s, realm), a.clock().Add(-meta.SessionMaxAge))
	if err != nil {
		a.logger().Warn("could not list the signed-in identities", "realm", realm, "err", logsafe.Err(err))
		return nil
	}
	var out []OIDCUser
	for _, row := range rows {
		if u, ok := decodeIdentity(row); ok {
			out = append(out, u)
		}
	}
	return out
}

// listed returns the users that p admits under s, with their roles, ordered by id.
func listed(s *Snapshot, users []OIDCUser, p func(*Snapshot, OIDCUser) (Principal, bool)) []OIDCUser {
	var out []OIDCUser
	for _, u := range users {
		if pr, ok := p(s, u); ok {
			u.Roles = pr.roles
			out = append(out, u)
		}
	}
	slices.SortFunc(out, func(x, y OIDCUser) int { return strings.Compare(x.ID, y.ID) })
	return out
}
