package app

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/tasks"
)

// security.purge-api-keys deletes the tokens of owners that no longer
// resolve (core-43): a stored id with no local user, or an LDAP-only
// override row's plain id; it keeps a live user's, a disabled user's, and
// those it cannot check (an SSO owner, LDAP without a directory).
func TestPurgeOrphanedAPIKeys(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg, err := config.Parse([]byte(`
dataDir: ` + t.TempDir() + `
users:
  - { name: root, passwordHash: "` + string(h) + `", roles: [admin] }
  - { name: dev, passwordHash: "` + string(h) + `", roles: [] }
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.URL = metatest.URL(t, cfg.DataDir)
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	ctx := context.Background()
	if st, b := utCall(t, "POST", ts.URL+"/-/api/users", "root", "pw", `{"id":"dir","source":"ldap","roles":[]}`, true); st != 201 {
		t.Fatalf("ldap row: %d %s", st, b)
	}
	if st, b := utCall(t, "PUT", ts.URL+"/-/api/users/dev", "root", "pw", `{"roles":[],"status":"disabled"}`, true); st != 200 {
		t.Fatalf("disable dev: %d %s", st, b)
	}
	old := time.Now().Add(-time.Hour)
	for i, owner := range []string{"root", "dev", "ghost", "dir", "ldap:someone", "oidc:sso-user"} {
		if err := a.DB.CreateToken(ctx, "t"+string(rune('a'+i)), owner, "hash-"+owner, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.DB.PutUserToken(ctx, "ghost", "ghostnam", "h", old); err != nil {
		t.Fatal(err)
	}
	st, b := utCall(t, "POST", ts.URL+"/-/api/tasks", "root", "pw", `{"name":"purge","type":"`+tasks.TypePurgeAPIKeys+`","schedule":"manual","params":{}}`, true)
	if st != 201 {
		t.Fatalf("create task: %d %s", st, b)
	}
	out, err := a.Tasks.RunNow(utJSON(t, b)["id"].(string), "root")
	if err != nil || out.Result != tasks.ResultOK || out.Removed != 3 { // ghost's token and user token, dir's token
		t.Fatalf("purge: %+v %v", out, err)
	}
	owners, err := a.DB.TokenOwners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(owners); got != 4 || owners[0] != "dev" || owners[1] != "ldap:someone" || owners[2] != "oidc:sso-user" || owners[3] != "root" {
		t.Fatalf("owners left = %v", owners)
	}
}
