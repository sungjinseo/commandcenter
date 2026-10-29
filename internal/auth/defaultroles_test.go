package auth

import (
	"net/http/httptest"
	"slices"
	"testing"

	"commandcenter/internal/ldap"
	"commandcenter/internal/meta"
)

// The defaultRoles setting reaches every kind of signed-in principal and
// never the snapshot's own role lists.
func TestDefaultRolesEveryRealm(t *testing.T) {
	set := meta.DefaultSettings()
	set.DefaultRoles = []string{"base"}
	set.RUT = meta.RUT{Header: "REMOTE_USER", TrustedProxies: []string{"10.0.0.0/8"}}
	set.Realms = append(set.Realms, meta.Realm{ID: meta.RealmRUT, Enabled: true})
	users := []meta.User{{ID: "loc", Source: meta.SourceLocal, Status: meta.StatusActive, Roles: []string{"dev"}}}
	s := NewSnapshotWith(users, []meta.Role{{ID: "base"}, {ID: "dev"}}, set, nil)
	a := &Authenticator{}
	a.setSnapshot(s)
	lp, _ := a.ldapPrincipal(s, ldap.User{ID: "l"})
	op, _ := a.oidcPrincipal(s, OIDCUser{ID: "o"})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "[::ffff:10.1.2.3]:4242" // an IPv4-mapped address is the IPv4 one
	r.Header.Set("Remote_User", "Rut")
	rp, ok := a.rutPrincipal(r)
	if !ok || rp.User != "rut" || !rp.IsRUT() {
		t.Fatalf("rut: %+v %v", rp, ok)
	}
	for name, p := range map[string]Principal{"local": s.principal(s.users["loc"]), "ldap": lp, "oidc": op, "rut": rp} {
		if !slices.Contains(p.roles, "base") || p.Admin {
			t.Errorf("%s: roles %v", name, p.roles)
		}
	}
	if got := s.principal(s.users["loc"]).roles; !slices.Equal(got, []string{"dev", "base"}) || len(s.users["loc"].Roles) != 1 {
		t.Fatalf("local roles %v, snapshot %v", got, s.users["loc"].Roles)
	}
	r.RemoteAddr = "192.168.1.1:1"
	if _, ok := a.rutPrincipal(r); ok {
		t.Fatal("untrusted address accepted")
	}
}
