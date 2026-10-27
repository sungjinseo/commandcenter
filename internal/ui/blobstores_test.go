package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// withBlobStore gives e a live blob-store map (the default store rooted at the
// UI data directory) and routes repositories to their named store, the way the
// app wires it. It returns the data directory.
func withBlobStore(t *testing.T, e *env) string {
	t.Helper()
	ctx := context.Background()
	def, err := blob.Open(e.ui.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SeedBlobStore(ctx, repo.DefaultStore, e.ui.DataDir); err != nil {
		t.Fatal(err)
	}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: def}, DB: e.db, Now: time.Now}
	store.StoreFor = func(name string) string {
		if rp := e.ui.Repos.Get(name); rp != nil {
			return rp.BlobStore
		}
		return ""
	}
	e.ui.Store = store
	e.ui.Repos.HasStore = store.HasStore
	return e.ui.DataDir
}

// I2: a symlink to another store's root is the same tree.
func TestBlobStoreSymlinkRejected(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	dataDir := withBlobStore(t, e)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dataDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r := e.admin(t, "POST", "/-/api/blobstores", `{"name":"sym","path":"`+filepath.ToSlash(link)+`"}`)
	wantStatus(t, r, http.StatusBadRequest)
	if e.ui.Store.HasStore("sym") {
		t.Fatal("symlinked store was added")
	}
}

// I2: a directory that already holds blobs is refused.
func TestBlobStorePrepopulatedRejected(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	root := filepath.Join(t.TempDir(), "pre")
	leaf := filepath.Join(root, "blobs", "sha256", "ab", "cd")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leaf, "abcd"+strings.Repeat("0", 60)), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.admin(t, "POST", "/-/api/blobstores", `{"name":"pre","path":"`+filepath.ToSlash(root)+`"}`)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, "already contains blobs") {
		t.Fatalf("want 'already contains blobs': %s", r.body)
	}
}

// I3: a store created without an absolute path is recorded relative to the
// data directory, so it follows a moved dataDir.
func TestBlobStoreRelativePathStored(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores", `{"name":"fast"}`), http.StatusCreated)
	row, err := e.db.GetBlobStore(context.Background(), "fast")
	if err != nil {
		t.Fatal(err)
	}
	if row.Path != filepath.Join("blobstores", "fast") {
		t.Fatalf("stored path = %q, want blobstores/fast", row.Path)
	}
}

// C1.3: a store delete and a repository create naming that store are
// serialized, so a repository never ends up pointing at a deleted store.
func TestBlobStoreDeleteRacingRepoCreate(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores", `{"name":"doomed"}`), http.StatusCreated)

	type result struct{ err error }
	created := make(chan result, 1)
	finishedEarly := false
	blobStoreDeleteHook = func() {
		// Delete passed its in-use check and holds the lock: start a create
		// naming the store. It must not complete before the delete commits.
		go func() {
			_, err := e.ui.Repos.CreateDef(context.Background(),
				config.Repository{Name: "racer", Format: "maven", Type: "hosted", BlobStore: "doomed"})
			created <- result{err}
		}()
		select {
		case <-created:
			finishedEarly = true
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Cleanup(func() { blobStoreDeleteHook = nil })

	wantStatus(t, e.admin(t, "DELETE", "/-/api/blobstores/doomed", ""), http.StatusNoContent)
	if finishedEarly {
		t.Fatal("repository create ran while the store delete held the lock")
	}
	res := <-created
	var ve *repo.ValidationError
	if !errors.As(res.err, &ve) {
		t.Fatalf("create after delete: err = %v, want a validation error", res.err)
	}
	if e.ui.Repos.Get("racer") != nil {
		t.Fatal("a repository points at a deleted blob store")
	}
}

func TestBlobStoreCreateListDelete(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	dataDir := withBlobStore(t, e)

	// Create with an omitted path: lands under <dataDir>/blobstores/<name>.
	r := e.admin(t, "POST", "/-/api/blobstores", `{"name":"fast"}`)
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	wantPath := filepath.Join(dataDir, "blobstores", "fast")
	if m["name"] != "fast" || m["path"] != wantPath {
		t.Fatalf("create: %s (want path %s)", r.body, wantPath)
	}
	if _, err := os.Stat(filepath.Join(wantPath, "blobs", "sha256")); err != nil {
		t.Fatalf("blob tree not created: %v", err)
	}
	if !e.ui.Store.HasStore("fast") {
		t.Fatal("store not added to the live map")
	}

	if w := m["warning"]; w != nil {
		t.Fatalf("store under the data directory warned: %v", w)
	}

	// QA N-22: a relative path is refused (it used to be ignored silently).
	for _, rel := range []string{"ignored/here", "../escape", "bs1"} {
		r = e.admin(t, "POST", "/-/api/blobstores", `{"name":"rel","path":"`+rel+`"}`)
		if r.status != http.StatusBadRequest || !strings.Contains(r.body, "must be absolute") {
			t.Fatalf("relative path %q: %d %s", rel, r.status, r.body)
		}
	}
	if e.ui.Store.HasStore("rel") {
		t.Fatal("a refused store was added")
	}

	// An absolute path is used as given; QA N-23: outside the data
	// directory the answer carries a warning, inside it does not.
	abs := filepath.Join(t.TempDir(), "elsewhere")
	r = e.admin(t, "POST", "/-/api/blobstores", `{"name":"abs","path":"`+filepath.ToSlash(abs)+`"}`)
	wantStatus(t, r, http.StatusCreated)
	if m := r.json(t); m["path"] != abs || !strings.Contains(fmt.Sprint(m["warning"]), "outside the data directory") {
		t.Fatalf("absolute path outside: %s (want path %s and a warning)", r.body, abs)
	}
	if !strings.Contains(e.admin(t, "GET", "/-/api/blobstores/abs", "").body, `"warning"`) {
		t.Fatal("GET lost the warning")
	}
	inside := filepath.Join(dataDir, "custom", "inside")
	r = e.admin(t, "POST", "/-/api/blobstores", `{"name":"inside","path":"`+filepath.ToSlash(inside)+`"}`)
	wantStatus(t, r, http.StatusCreated)
	if m := r.json(t); m["path"] != inside || m["warning"] != nil {
		t.Fatalf("absolute path inside: %s", r.body)
	}

	// List includes default plus the three created.
	r = e.admin(t, "GET", "/-/api/blobstores", "")
	wantStatus(t, r, http.StatusOK)
	if n := strings.Count(r.body, `"name"`); n < 4 {
		t.Fatalf("list has %d stores: %s", n, r.body)
	}

	// Delete an unused store: 204 and the directory stays on disk.
	r = e.admin(t, "DELETE", "/-/api/blobstores/abs", "")
	wantStatus(t, r, http.StatusNoContent)
	if e.ui.Store.HasStore("abs") {
		t.Fatal("deleted store still in the live map")
	}
	if _, err := os.Stat(filepath.Join(abs, "blobs", "sha256")); err != nil {
		t.Fatalf("delete removed files on disk: %v", err)
	}
}

func TestBlobStoreDuplicateName(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores", `{"name":"dup"}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores", `{"name":"dup"}`), http.StatusConflict)
}

func TestBlobStoreReservedAndInvalidName(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores", `{"name":"create"}`), http.StatusBadRequest)
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores", `{"name":"has space"}`), http.StatusBadRequest)
}

func TestBlobStoreIsolationRejection(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	dataDir := withBlobStore(t, e)

	// Equal to the default store's blob tree (root == dataDir).
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores",
		`{"name":"same","path":"`+filepath.ToSlash(dataDir)+`"}`), http.StatusBadRequest)

	// Descendant of the default store's blob tree.
	nested := filepath.Join(dataDir, "blobs", "child")
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores",
		`{"name":"nested","path":"`+filepath.ToSlash(nested)+`"}`), http.StatusBadRequest)

	// Ancestor: s1's blob tree sits inside a would-be new store's blob tree.
	inner := filepath.Join(t.TempDir(), "outer", "blobs", "inner")
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores",
		`{"name":"s1","path":"`+filepath.ToSlash(inner)+`"}`), http.StatusCreated)
	outer := filepath.Dir(filepath.Dir(inner)) // .../outer
	r := e.admin(t, "POST", "/-/api/blobstores", `{"name":"anc","path":"`+filepath.ToSlash(outer)+`"}`)
	wantStatus(t, r, http.StatusBadRequest)
}

func TestBlobStoreNonWritablePath(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	// Point the store root at an existing file: MkdirAll of root/blobs fails.
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.admin(t, "POST", "/-/api/blobstores", `{"name":"bad","path":"`+filepath.ToSlash(file)+`"}`)
	wantStatus(t, r, http.StatusBadRequest)
}

func TestBlobStoreDeleteDefaultAndInUse(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)

	// The default store cannot be deleted.
	wantStatus(t, e.admin(t, "DELETE", "/-/api/blobstores/default", ""), http.StatusConflict)

	// A store a repository uses cannot be deleted.
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores", `{"name":"used"}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories",
		`{"name":"mvn-used","format":"maven","type":"hosted","blobStore":"used"}`), http.StatusCreated)
	r := e.admin(t, "DELETE", "/-/api/blobstores/used", "")
	wantStatus(t, r, http.StatusConflict)
	if !strings.Contains(r.body, "mvn-used") {
		t.Fatalf("409 should name the repository: %s", r.body)
	}
}

func TestBlobStoreUsageScoped(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	ctx := context.Background()
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores", `{"name":"fast"}`), http.StatusCreated)
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories",
		`{"name":"mvn-fast","format":"maven","type":"hosted","blobStore":"fast"}`), http.StatusCreated)

	for _, c := range []string{"alpha", "bravo"} {
		if _, err := e.ui.Store.Put(ctx, "mvn-fast", "org/x/"+c, "application/octet-stream", strings.NewReader(c)); err != nil {
			t.Fatal(err)
		}
	}
	r := e.admin(t, "GET", "/-/api/blobstores/fast", "")
	wantStatus(t, r, http.StatusOK)
	m := r.json(t)
	if m["blobCount"] != float64(2) {
		t.Fatalf("blobCount = %v, want 2: %s", m["blobCount"], r.body)
	}
	if m["usedBytes"] != float64(len("alpha")+len("bravo")) {
		t.Fatalf("usedBytes = %v: %s", m["usedBytes"], r.body)
	}
	if m["repoCount"] != float64(1) {
		t.Fatalf("repoCount = %v: %s", m["repoCount"], r.body)
	}
}

func TestBlobStoreAdminGatesAndReadAuth(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	// Reads require an admin.
	wantStatus(t, e.get(t, "/-/api/blobstores", ""), http.StatusUnauthorized)
	wantStatus(t, e.get(t, "/-/api/blobstores", "reader"), http.StatusForbidden)
	wantStatus(t, e.get(t, "/-/api/blobstores", "admin"), http.StatusOK)
	// Mutations need the UI header.
	wantStatus(t, e.req(t, "POST", "/-/api/blobstores", `{"name":"x"}`, "admin", nil, false), http.StatusForbidden)
}

func TestRepoCreateBlobStoreValidation(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)

	// Omitted store defaults to "default".
	r := e.admin(t, "POST", "/-/api/repositories", `{"name":"mvn-def","format":"maven","type":"hosted"}`)
	wantStatus(t, r, http.StatusCreated)
	if r.json(t)["blobStore"] != "default" {
		t.Fatalf("blobStore not defaulted: %s", r.body)
	}
	// Unknown store is rejected.
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories",
		`{"name":"mvn-ghost","format":"maven","type":"hosted","blobStore":"ghost"}`), http.StatusBadRequest)
	// A group rejects a blob store.
	wantStatus(t, e.admin(t, "POST", "/-/api/repositories",
		`{"name":"mvn-grp","format":"maven","type":"group","members":["maven-releases"],"blobStore":"default"}`), http.StatusBadRequest)
}

// M1 (final review): an existing store whose path cannot be resolved (a
// dangling symlink) is no reason to skip the isolation check: the new store
// is refused, naming the unresolvable one.
func TestBlobStoreIsolationFailsClosed(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	dir := t.TempDir()
	dangling := filepath.Join(dir, "gone")
	if err := os.Symlink(filepath.Join(dir, "missing-target"), dangling); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := e.db.CreateBlobStore(context.Background(), meta.BlobStore{Name: "broken", Path: dangling}); err != nil {
		t.Fatal(err)
	}
	r := e.admin(t, "POST", "/-/api/blobstores", `{"name":"fresh","path":"`+filepath.ToSlash(filepath.Join(t.TempDir(), "fresh"))+`"}`)
	wantStatus(t, r, http.StatusBadRequest)
	if !strings.Contains(r.body, `\"broken\"`) {
		t.Fatalf("refusal must name the broken store: %s", r.body)
	}
	if e.ui.Store.HasStore("fresh") {
		t.Fatal("store added despite an unresolvable existing store")
	}
}
