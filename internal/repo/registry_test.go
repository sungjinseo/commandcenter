package repo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

func newRegistry(t *testing.T, defs ...config.Repository) (*Registry, *meta.DB) {
	t.Helper()
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for i := range defs {
		config.ApplyRepositoryDefaults(&defs[i])
	}
	if _, err := db.ImportRepositories(context.Background(), defs); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return NewRegistry(db, loaded), db
}

func baseDefs() []config.Repository {
	return []config.Repository{
		{Name: "h", Format: "maven", Type: "hosted"},
		{Name: "p", Format: "maven", Type: "proxy", Remote: "https://x.example/maven2"},
		{Name: "g", Format: "maven", Type: "group", Members: []string{"h", "p"}, WriteMember: "h"},
	}
}

func TestRegistrySnapshotResolves(t *testing.T) {
	reg, _ := newRegistry(t, baseDefs()...)
	s := reg.Snapshot()
	if len(s.Sorted) != 3 || s.Sorted[0].Name != "g" || s.Sorted[1].Name != "h" || s.Sorted[2].Name != "p" {
		t.Fatalf("sorted = %v", names(s.Sorted))
	}
	g := reg.Get("g")
	if g.MemberRepos[0] != s.ByName["h"] || g.WriteTarget != s.ByName["h"] || s.ByName["p"].RemoteURL.String() != "https://x.example/maven2/" {
		t.Fatalf("group not resolved: %+v", g)
	}
	if reg.Get("missing") != nil {
		t.Fatal("Get(missing) != nil")
	}
}

func TestRegistryBlobStoreImmutable(t *testing.T) {
	reg, _ := newRegistry(t, baseDefs()...)
	ctx := context.Background()
	if err := reg.Create(ctx, config.Repository{Name: "fast-repo", Format: "maven", Type: "hosted", BlobStore: "fast"}); err != nil {
		t.Fatal(err)
	}
	if got := reg.Get("fast-repo").BlobStore; got != "fast" {
		t.Fatalf("created blobStore = %q, want fast", got)
	}
	// An update that repeats the store is fine; omitting it keeps the stored one.
	if err := reg.Update(ctx, "fast-repo", config.Repository{WritePolicy: "deny"}, ClearSecrets{}); err != nil {
		t.Fatalf("update keeping blobStore: %v", err)
	}
	if got := reg.Get("fast-repo").BlobStore; got != "fast" {
		t.Fatalf("blobStore not kept on update: %q", got)
	}
	// Changing the store is rejected.
	var ve *ValidationError
	err := reg.Update(ctx, "fast-repo", config.Repository{BlobStore: "slow"}, ClearSecrets{})
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "blobStore cannot be changed") {
		t.Fatalf("expected immutability error, got %v", err)
	}
}

func TestRegistryRejectsUnknownBlobStore(t *testing.T) {
	reg, _ := newRegistry(t, baseDefs()...)
	reg.HasStore = func(name string) bool { return name == DefaultStore || name == "fast" }
	ctx := context.Background()
	var ve *ValidationError
	err := reg.Create(ctx, config.Repository{Name: "n1", Format: "maven", Type: "hosted", BlobStore: "ghost"})
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), `unknown blob store "ghost"`) {
		t.Fatalf("create with unknown store: %v", err)
	}
	if err := reg.Create(ctx, config.Repository{Name: "n2", Format: "maven", Type: "hosted", BlobStore: "fast"}); err != nil {
		t.Fatalf("create with known store: %v", err)
	}
	// The store disappears (it cannot through the API; a direct DB edit can):
	// an update of the repository is refused rather than persisting it.
	reg.HasStore = func(name string) bool { return name == DefaultStore }
	err = reg.Update(ctx, "n2", config.Repository{WritePolicy: "deny"}, ClearSecrets{})
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), `unknown blob store "fast"`) {
		t.Fatalf("update with unknown store: %v", err)
	}
}

func TestStoreOf(t *testing.T) {
	for _, c := range []struct {
		def  config.Repository
		want string
	}{
		{config.Repository{Type: "hosted"}, DefaultStore},
		{config.Repository{Type: "hosted", BlobStore: "fast"}, "fast"},
		{config.Repository{Type: "group"}, DefaultStore},
		{config.Repository{Type: "group", BlobStore: "fast"}, DefaultStore},
	} {
		if got := StoreOf(c.def); got != c.want {
			t.Errorf("StoreOf(%+v) = %q, want %q", c.def, got, c.want)
		}
	}
}

func TestBlobStoreUnknownNameErrors(t *testing.T) {
	s := newStore(t)
	s.StoreFor = func(string) string { return "ghost" }
	if _, err := s.BlobStore("r"); err == nil {
		t.Fatal("BlobStore resolved a missing store")
	}
	if _, err := s.WriteBlob("r", strings.NewReader("x"), nil); err == nil {
		t.Fatal("WriteBlob wrote through a missing store")
	}
}

func TestRegistryWritesSwapSnapshot(t *testing.T) {
	reg, db := newRegistry(t, baseDefs()...)
	ctx := context.Background()
	before := reg.Snapshot()

	if err := reg.Create(ctx, config.Repository{Name: "h2", Format: "maven", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	afterCreate := reg.Snapshot()
	if before.ByName["h2"] != nil || len(before.Sorted) != 3 {
		t.Fatal("old snapshot changed by Create")
	}
	h2 := afterCreate.ByName["h2"]
	if h2 == nil || h2.WritePolicy != "allow_once" || h2.VersionPolicy != "release" {
		t.Fatalf("created repo (defaults applied) = %+v", h2)
	}

	upd := config.Repository{Type: "group", Members: []string{"h2", "h", "p"}, WriteMember: "h2"}
	if err := reg.Update(ctx, "g", upd, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	afterUpdate := reg.Snapshot()
	if names(afterCreate.ByName["g"].MemberRepos) != "h,p" {
		t.Fatal("old snapshot changed by Update")
	}
	g := afterUpdate.ByName["g"]
	if names(g.MemberRepos) != "h2,h,p" || g.WriteTarget != afterUpdate.ByName["h2"] || g.Format != "maven" {
		t.Fatalf("updated group = %+v", g)
	}

	if err := reg.Update(ctx, "g", config.Repository{Members: []string{"h", "p"}}, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Delete(ctx, "h2"); err != nil {
		t.Fatal(err)
	}
	if reg.Get("h2") != nil || afterUpdate.ByName["h2"] == nil {
		t.Fatal("Delete did not swap the snapshot, or changed the old one")
	}

	// Every write reached the database.
	defs, err := db.ListRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 3 {
		t.Fatalf("db = %+v", defs)
	}
	for _, d := range defs {
		if d.Name == "g" && (strings.Join(d.Members, ",") != "h,p" || d.WriteMember != "") {
			t.Fatalf("db group = %+v", d)
		}
	}
}

func TestRegistryNotFound(t *testing.T) {
	reg, _ := newRegistry(t, baseDefs()...)
	ctx := context.Background()
	if err := reg.Update(ctx, "nope", config.Repository{}, ClearSecrets{}); StatusOf(err) != http.StatusNotFound {
		t.Fatalf("update missing: %v", err)
	}
	if err := reg.Delete(ctx, "nope"); StatusOf(err) != http.StatusNotFound {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestRegistryValidationErrors(t *testing.T) {
	reg, db := newRegistry(t, baseDefs()...)
	ctx := context.Background()
	before := reg.Snapshot()

	err := reg.Create(ctx, config.Repository{Name: "x", Format: "docker", Type: "virtual"})
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Errors) != 2 || StatusOf(err) != http.StatusBadRequest {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(strings.Join(ve.Errors, "\n"), `repository "x": unknown format "docker"`) {
		t.Fatalf("errors = %q", ve.Errors)
	}
	if err := reg.Create(ctx, config.Repository{Name: "h", Format: "maven", Type: "hosted"}); !errors.As(err, &ve) ||
		ve.Errors[0] != `duplicate repository name "h"` {
		t.Fatalf("duplicate: %v", err)
	}
	if err := reg.Update(ctx, "g", config.Repository{Members: []string{"h", "missing"}}, ClearSecrets{}); !errors.As(err, &ve) ||
		ve.Errors[0] != `repository "g": unknown member "missing"` {
		t.Fatalf("update: %v", err)
	}
	if reg.Snapshot() != before {
		t.Fatal("a rejected write swapped the snapshot")
	}
	if defs, _ := db.ListRepositories(ctx); len(defs) != 3 {
		t.Fatalf("a rejected write reached the db: %+v", defs)
	}
}

func TestRegistryImmutableFields(t *testing.T) {
	reg, _ := newRegistry(t, baseDefs()...)
	ctx := context.Background()
	for _, def := range []config.Repository{
		{Name: "other", Format: "maven", Type: "hosted"},
		{Name: "h", Format: "npm", Type: "hosted"},
		{Name: "h", Format: "maven", Type: "proxy", Remote: "https://y.example/"},
	} {
		err := reg.Update(ctx, "h", def, ClearSecrets{})
		var ve *ValidationError
		if !errors.As(err, &ve) || !strings.Contains(ve.Errors[0], "cannot be changed") {
			t.Fatalf("update %+v: %v", def, err)
		}
	}
	// Omitted (empty) name, format and type keep the stored values.
	if err := reg.Update(ctx, "h", config.Repository{WritePolicy: "allow"}, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	if h := reg.Get("h"); h.Name != "h" || h.Format != "maven" || h.Type != "hosted" || h.WritePolicy != "allow" {
		t.Fatalf("h = %+v", h)
	}
}

// remoteAuthOf returns the stored remoteAuth of the named repository,
// checking that the snapshot and the database agree.
func remoteAuthOf(t *testing.T, reg *Registry, db *meta.DB, name string) config.RemoteAuth {
	t.Helper()
	defs, err := db.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var stored *config.RemoteAuth
	for _, d := range defs {
		if d.Name == name {
			stored = d.RemoteAuth
		}
	}
	live := reg.Get(name).RemoteAuth
	if (live == nil) != (stored == nil) || (live != nil && *live != *stored) {
		t.Fatalf("snapshot %+v != db %+v", live, stored)
	}
	if live == nil {
		return config.RemoteAuth{}
	}
	return *live
}

func TestRegistrySecrets(t *testing.T) {
	reg, db := newRegistry(t)
	ctx := context.Background()
	p := config.Repository{Name: "p", Format: "npm", Type: "proxy", Remote: "https://r.example/",
		RemoteAuth: &config.RemoteAuth{Username: "u", Password: "pw", BearerToken: "tok"}}
	if err := reg.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	auth := func() config.RemoteAuth { return remoteAuthOf(t, reg, db, "p") }
	update := func(def config.Repository, clear ClearSecrets) {
		t.Helper()
		if err := reg.Update(ctx, "p", def, clear); err != nil {
			t.Fatal(err)
		}
	}
	const remote = "https://r.example/v2/" // same scheme and host: secrets may follow

	// Empty password and bearer token keep the stored values.
	update(config.Repository{Remote: remote, RemoteAuth: &config.RemoteAuth{Username: "u2"}}, ClearSecrets{})
	if a := auth(); a != (config.RemoteAuth{Username: "u2", Password: "pw", BearerToken: "tok"}) {
		t.Fatalf("after keep: %+v", a)
	}
	if reg.Get("p").RemoteURL.Path != "/v2/" {
		t.Fatal("remote not updated")
	}
	// A nil remoteAuth keeps the stored auth as a whole, username included.
	update(config.Repository{Remote: remote}, ClearSecrets{})
	if a := auth(); a != (config.RemoteAuth{Username: "u2", Password: "pw", BearerToken: "tok"}) {
		t.Fatalf("after nil remoteAuth: %+v", a)
	}
	// A new value replaces.
	update(config.Repository{Remote: remote, RemoteAuth: &config.RemoteAuth{Username: "u2", Password: "pw2"}}, ClearSecrets{})
	if a := auth(); a != (config.RemoteAuth{Username: "u2", Password: "pw2", BearerToken: "tok"}) {
		t.Fatalf("after replace: %+v", a)
	}
	// Clear flags clear, also with a nil remoteAuth.
	update(config.Repository{Remote: remote, RemoteAuth: &config.RemoteAuth{Username: "u2"}}, ClearSecrets{Password: true})
	if a := auth(); a != (config.RemoteAuth{Username: "u2", BearerToken: "tok"}) {
		t.Fatalf("after clear password: %+v", a)
	}
	update(config.Repository{Remote: remote}, ClearSecrets{BearerToken: true})
	if a := auth(); a != (config.RemoteAuth{Username: "u2"}) {
		t.Fatalf("after clear bearer token: %+v", a)
	}
	// An explicit empty remoteAuth with nothing left to keep becomes nil.
	update(config.Repository{Remote: remote, RemoteAuth: &config.RemoteAuth{}}, ClearSecrets{})
	if reg.Get("p").RemoteAuth != nil {
		t.Fatalf("empty remoteAuth must be nil, got %+v", reg.Get("p").RemoteAuth)
	}
}

func TestRegistryPasswordNeedsUsername(t *testing.T) {
	reg, db := newRegistry(t)
	ctx := context.Background()
	if err := reg.Create(ctx, config.Repository{Name: "p", Format: "npm", Type: "proxy", Remote: "https://r.example/",
		RemoteAuth: &config.RemoteAuth{Username: "u", Password: "pw"}}); err != nil {
		t.Fatal(err)
	}
	// An explicit remoteAuth without a username would keep a password with no user.
	err := reg.Update(ctx, "p", config.Repository{Remote: "https://r.example/", RemoteAuth: &config.RemoteAuth{BearerToken: "tok"}}, ClearSecrets{})
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(ve.Errors[0], "username required with password") {
		t.Fatalf("err = %v", err)
	}
	if a := remoteAuthOf(t, reg, db, "p"); a != (config.RemoteAuth{Username: "u", Password: "pw"}) {
		t.Fatalf("rejected update changed auth: %+v", a)
	}
	// Clearing the password at the same time is fine.
	if err := reg.Update(ctx, "p", config.Repository{Remote: "https://r.example/", RemoteAuth: &config.RemoteAuth{BearerToken: "tok"}},
		ClearSecrets{Password: true}); err != nil {
		t.Fatal(err)
	}
	if a := remoteAuthOf(t, reg, db, "p"); a != (config.RemoteAuth{BearerToken: "tok"}) {
		t.Fatalf("bearer only: %+v", a)
	}
}

func TestRegistryCredentialsDoNotFollowHostChange(t *testing.T) {
	reg, db := newRegistry(t)
	ctx := context.Background()
	creds := config.RemoteAuth{Username: "u", Password: "pw", BearerToken: "tok"}
	reset := func() {
		t.Helper()
		if err := reg.Delete(ctx, "p"); err != nil && StatusOf(err) != http.StatusNotFound {
			t.Fatal(err)
		}
		c := creds
		if err := reg.Create(ctx, config.Repository{Name: "p", Format: "npm", Type: "proxy", Remote: "https://r.example/", RemoteAuth: &c}); err != nil {
			t.Fatal(err)
		}
	}
	reset()

	// Rejected: a new scheme or host with stored secrets carried over.
	for _, def := range []config.Repository{
		{Remote: "https://evil.example/", RemoteAuth: &config.RemoteAuth{Username: "u"}},
		{Remote: "https://evil.example/"}, // nil remoteAuth keeps everything
		{Remote: "http://r.example/"},     // scheme change
		{Remote: "https://r.example:8443/"},
		// A new password alone would still carry the old bearer token.
		{Remote: "https://evil.example/", RemoteAuth: &config.RemoteAuth{Username: "u", Password: "new"}},
	} {
		before := reg.Snapshot()
		err := reg.Update(ctx, "p", def, ClearSecrets{})
		var ve *ValidationError
		if !errors.As(err, &ve) || !strings.Contains(ve.Errors[0], "remote host changed: re-enter the remote credentials or clear them") {
			t.Fatalf("update to %s: %v", def.Remote, err)
		}
		if reg.Snapshot() != before {
			t.Fatal("rejected update swapped the snapshot")
		}
	}
	if a := remoteAuthOf(t, reg, db, "p"); a != creds || reg.Get("p").Remote != "https://r.example/" {
		t.Fatalf("rejected updates changed the repository: %+v", a)
	}

	// Accepted: new credentials for the new host.
	if err := reg.Update(ctx, "p", config.Repository{Remote: "https://new.example/",
		RemoteAuth: &config.RemoteAuth{Username: "u", Password: "pw2", BearerToken: "tok2"}}, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	if a := remoteAuthOf(t, reg, db, "p"); a != (config.RemoteAuth{Username: "u", Password: "pw2", BearerToken: "tok2"}) {
		t.Fatalf("new credentials: %+v", a)
	}
	// Accepted: a new password with the bearer token cleared.
	reset()
	if err := reg.Update(ctx, "p", config.Repository{Remote: "https://new.example/",
		RemoteAuth: &config.RemoteAuth{Username: "u", Password: "pw2"}}, ClearSecrets{BearerToken: true}); err != nil {
		t.Fatal(err)
	}
	if a := remoteAuthOf(t, reg, db, "p"); a != (config.RemoteAuth{Username: "u", Password: "pw2"}) {
		t.Fatalf("new password, bearer cleared: %+v", a)
	}
	// Accepted: clearing everything.
	reset()
	if err := reg.Update(ctx, "p", config.Repository{Remote: "https://other.example/", RemoteAuth: &config.RemoteAuth{}},
		ClearSecrets{Password: true, BearerToken: true}); err != nil {
		t.Fatal(err)
	}
	if reg.Get("p").RemoteAuth != nil {
		t.Fatalf("cleared: %+v", reg.Get("p").RemoteAuth)
	}
	// Accepted: a repository without secrets may change host freely.
	if err := reg.Update(ctx, "p", config.Repository{Remote: "https://third.example/"}, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryDeleteGroupMemberConflicts(t *testing.T) {
	defs := append(baseDefs(), config.Repository{Name: "g2", Format: "maven", Type: "group", Members: []string{"p"}})
	reg, db := newRegistry(t, defs...)
	ctx := context.Background()
	err := reg.Delete(ctx, "p")
	if StatusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "g, g2") {
		t.Fatalf("delete member: %v", err)
	}
	if err := reg.Delete(ctx, "h"); StatusOf(err) != http.StatusConflict || !strings.HasSuffix(err.Error(), "group repositories: g") {
		t.Fatalf("delete writeMember: %v", err)
	}
	if reg.Get("p") == nil {
		t.Fatal("rejected delete removed the repository")
	}
	// Deleting the groups first frees the members, and purges their rows and
	// the role privileges on them (QA N-25).
	if err := db.PutAsset(ctx, meta.Asset{Repo: "p", Path: "a.jar", BlobSHA256: "s"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"r1", "r2"} {
		if err := db.CreateRole(ctx, meta.Role{ID: id, Privileges: []meta.Privilege{{Target: "repo:p", Actions: []string{"read"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	var deleted []string
	reg.OnDelete = func(_ context.Context, name string, roles []string) {
		if reg.mu.TryLock() { // the app applies the role changes before a create can run
			reg.mu.Unlock()
			t.Error("OnDelete ran without the write lock")
		}
		deleted = append(deleted, name+"["+strings.Join(roles, " ")+"]")
	}
	for _, n := range []string{"g", "g2", "p"} {
		if err := reg.Delete(ctx, n); err != nil {
			t.Fatalf("delete %s: %v", n, err)
		}
	}
	if strings.Join(deleted, ",") != "g[],g2[],p[r1 r2]" {
		t.Fatalf("OnDelete calls = %v", deleted)
	}
	if _, err := db.GetAsset(ctx, "p", "a.jar"); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("asset of deleted repo: %v", err)
	}
}

func TestRegistryDrivesAnonymousRead(t *testing.T) {
	reg, _ := newRegistry(t, baseDefs()...)
	ctx := context.Background()
	authn := auth.New(&config.Config{AnonymousRead: true}, nil)
	authn.AnonymousRead = reg.AnonymousRead
	anon := auth.Principal{}
	if !authn.Can(anon, "h", auth.Read) {
		t.Fatal("inherit: anonymous read denied")
	}
	no := false
	if err := reg.Update(ctx, "h", config.Repository{AnonymousRead: &no}, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	if authn.Can(anon, "h", auth.Read) {
		t.Fatal("anonymous read still allowed after the registry update")
	}
	if err := reg.Create(ctx, config.Repository{Name: "n", Format: "npm", Type: "hosted", AnonymousRead: &no}); err != nil {
		t.Fatal(err)
	}
	if authn.Can(anon, "n", auth.Read) {
		t.Fatal("created repository ignores anonymousRead")
	}
}

// TestRegistryConcurrentReaders is meant for go test -race: readers must
// always see an internally consistent snapshot while writers swap it.
func TestRegistryConcurrentReaders(t *testing.T) {
	reg, _ := newRegistry(t, baseDefs()...)
	ctx := context.Background()
	var stop atomic.Bool
	var wg sync.WaitGroup
	var bad atomic.Value
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				s := reg.Snapshot()
				if len(s.Sorted) != len(s.ByName) {
					bad.Store(fmt.Sprintf("sorted %d != byName %d", len(s.Sorted), len(s.ByName)))
				}
				for _, rp := range s.Sorted {
					if s.ByName[rp.Name] != rp {
						bad.Store("sorted entry not in ByName: " + rp.Name)
					}
					for i, m := range rp.MemberRepos {
						if m == nil || s.ByName[rp.Members[i]] != m {
							bad.Store("member of " + rp.Name + " not from the same snapshot")
						}
					}
				}
				_ = reg.Get("g")
				_ = reg.AnonymousRead("h")
			}
		}()
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for i := 0; time.Now().Before(deadline); i++ {
		name := fmt.Sprintf("h%d", i)
		if err := reg.Create(ctx, config.Repository{Name: name, Format: "maven", Type: "hosted"}); err != nil {
			t.Fatal(err)
		}
		if err := reg.Update(ctx, "g", config.Repository{Members: []string{name, "h", "p"}}, ClearSecrets{}); err != nil {
			t.Fatal(err)
		}
		if err := reg.Update(ctx, "g", config.Repository{Members: []string{"h", "p"}}, ClearSecrets{}); err != nil {
			t.Fatal(err)
		}
		if err := reg.Delete(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	wg.Wait()
	if v := bad.Load(); v != nil {
		t.Fatal(v)
	}
}

func names(rs []*Repository) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return strings.Join(out, ",")
}

func TestRegistryGenChangesOnRecreateAndRemoteChange(t *testing.T) {
	reg, _ := newRegistry(t, baseDefs()...)
	ctx := context.Background()
	gen := func(n string) uint64 { return reg.Get(n).Gen }
	h1, p1 := gen("h"), gen("p")
	if h1 == 0 || p1 == 0 || h1 == p1 {
		t.Fatalf("every repository needs its own non-zero Gen: %d %d", h1, p1)
	}
	if err := reg.Create(ctx, config.Repository{Name: "other", Format: "maven", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	if gen("h") != h1 {
		t.Fatal("an unrelated create must not change a repository's Gen")
	}
	if err := reg.Delete(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Create(ctx, config.Repository{Name: "other", Format: "maven", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Update(ctx, "p", config.Repository{Remote: "https://y.example/maven2"}, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	if gen("p") == p1 {
		t.Fatal("a remote change must change Gen")
	}
	p2 := gen("p")
	if err := reg.Update(ctx, "p", config.Repository{Remote: "https://y.example/maven2", MetadataMaxAge: config.Duration(time.Hour)}, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	if gen("p") != p2 {
		t.Fatal("an update that keeps the remote must keep Gen")
	}
}

func TestStoreTxRefusesReplacedRepository(t *testing.T) {
	reg, db := newRegistry(t, baseDefs()...)
	s := &Store{DB: db, Now: time.Now, Live: func(rp *Repository) bool {
		cur := reg.Get(rp.Name)
		return cur != nil && cur.Gen == rp.Gen
	}}
	if err := reg.Create(context.Background(), config.Repository{Name: "other", Format: "maven", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	old := reg.Get("other")
	ran := false
	if err := s.Tx(context.Background(), old, func(*meta.Tx) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("live repository: ran=%v err=%v", ran, err)
	}
	if err := reg.Delete(context.Background(), "other"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Create(context.Background(), config.Repository{Name: "other", Format: "maven", Type: "hosted"}); err != nil {
		t.Fatal(err)
	}
	ran = false
	err := s.Tx(context.Background(), old, func(*meta.Tx) error { ran = true; return nil })
	if ran || StatusOf(err) != http.StatusServiceUnavailable {
		t.Fatalf("replaced repository: ran=%v err=%v", ran, err)
	}
}

func TestRegistryCreateNameRules(t *testing.T) {
	reg, _ := newRegistry(t, baseDefs()...)
	ctx := context.Background()
	long := strings.Repeat("a", 65)
	for _, name := range []string{"..", ".", "a b", "create", "a#b", "-x", "_x", ".x", "a/b", "", long, "ä"} {
		err := reg.Create(ctx, config.Repository{Name: name, Format: "maven", Type: "hosted"})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("Create(%q): err = %v, want a ValidationError", name, err)
		}
		if reg.Get(name) != nil {
			t.Errorf("rejected name %q was created", name)
		}
	}
	for _, name := range []string{"a", "A1", "my.repo_2-x", strings.Repeat("a", 64)} {
		if err := reg.Create(ctx, config.Repository{Name: name, Format: "maven", Type: "hosted"}); err != nil {
			t.Errorf("Create(%q): %v", name, err)
		}
	}
}

func TestRegistryCreateNormalizesRemoteAuth(t *testing.T) {
	reg, _ := newRegistry(t, baseDefs()...)
	ctx := context.Background()
	px := func(name string, a *config.RemoteAuth) config.Repository {
		return config.Repository{Name: name, Format: "maven", Type: "proxy", Remote: "https://r.example/", RemoteAuth: a}
	}
	def, err := reg.CreateDef(ctx, px("empty-auth", &config.RemoteAuth{}))
	if err != nil || def.RemoteAuth != nil || reg.Get("empty-auth").RemoteAuth != nil {
		t.Fatalf("an empty remoteAuth must become nil: %+v, %v", def.RemoteAuth, err)
	}
	err = reg.Create(ctx, px("pw-only", &config.RemoteAuth{Password: "x"}))
	var ve *ValidationError
	if !errors.As(err, &ve) || reg.Get("pw-only") != nil {
		t.Fatalf("a password without a username must be a ValidationError: %v", err)
	}
	if err := reg.Create(ctx, px("ok-auth", &config.RemoteAuth{Username: "u", Password: "x"})); err != nil {
		t.Fatal(err)
	}
}

// M7 (final review): a legacy row stored with an empty blobStore lives in the
// default store, so an update that says "default" explicitly is no change.
func TestRegistryBlobStoreLegacyEmptyAcceptsDefault(t *testing.T) {
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	legacy := config.Repository{Name: "old", Format: "maven", Type: "hosted"}
	config.ApplyRepositoryDefaults(&legacy)
	legacy.BlobStore = "" // as stored before blob stores existed
	if _, err := db.ImportRepositories(ctx, []config.Repository{legacy}); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.ListRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded[0].BlobStore != "" {
		t.Fatalf("setup: stored blobStore = %q", loaded[0].BlobStore)
	}
	reg := NewRegistry(db, loaded)
	if err := reg.Update(ctx, "old", config.Repository{BlobStore: DefaultStore}, ClearSecrets{}); err != nil {
		t.Fatalf("explicit default on a legacy row: %v", err)
	}
	var ve *ValidationError
	if err := reg.Update(ctx, "old", config.Repository{BlobStore: "fast"}, ClearSecrets{}); !errors.As(err, &ve) {
		t.Fatalf("a real change must still be refused: %v", err)
	}
}
