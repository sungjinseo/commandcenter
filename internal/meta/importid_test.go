package meta

import (
	"context"
	"strings"
	"testing"

	"commandcenter/internal/config"
)

// M3 (final review): the one-time import refuses a config user id that the
// API would refuse (":" makes it look like an LDAP owner key), and imports
// nothing.
func TestImportAccessRejectsInvalidUserIDs(t *testing.T) {
	ctx := context.Background()
	for _, id := range []string{"ldap:alice", "with space", "-dash", "create", strings.Repeat("a", 65)} {
		db, _ := openTestDB(t)
		cfg := &config.Config{Users: []config.User{{Name: "admin", PasswordHash: "h", Roles: []string{"admin"}}, {Name: id, PasswordHash: "h"}}}
		imported, err := db.ImportAccess(ctx, cfg)
		if err == nil || imported || !strings.Contains(err.Error(), "invalid user id") || !strings.Contains(err.Error(), id) {
			t.Fatalf("%q: imported=%v err=%v", id, imported, err)
		}
		if done, _ := db.AccessImported(ctx); done {
			t.Fatalf("%q: import marked done", id)
		}
		if users, _ := db.ListUsers(ctx); len(users) != 0 {
			t.Fatalf("%q: users stored: %+v", id, users)
		}
	}
}
