package repo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

// Snapshot is an immutable view of every repository, with references
// resolved. It is never modified after it is published, so readers may keep
// and use it, and the *Repository values in it, without locking.
type Snapshot struct {
	ByName map[string]*Repository
	Sorted []*Repository // by name
}

// Registry holds the live repository definitions (plan 9 Decision 5). Reads
// load the current Snapshot; writes are serialized by the cluster lock
// "registry" (meta.DB.Lock), validated against the whole would-be list,
// persisted, then published as a new Snapshot. Each repository's Gen is its
// stored gen (meta.DB.RepositoryGen).
type Registry struct {
	snap atomic.Pointer[Snapshot]
	db   *meta.DB
	mu   sync.Mutex // WithWriteLock's lock without a db (a read-only registry)

	// routes are the compiled routing rules by name (routing.go).
	routes atomic.Pointer[map[string]*Route]

	sf singleflight.Group // Lookup's resyncs

	// Other nodes write the database too: a write first re-reads the stored
	// repositories and routing rules, and runs Refresh, under the registry
	// lock, so it validates against what every node wrote; afterwards it
	// sends Notify("registry"). Lookup reads the database for a name the
	// snapshot lacks.
	//
	// Refresh, if set, re-reads under the registry lock of a write what
	// else the write validates against: the app's blob stores and auth
	// snapshot.
	Refresh func(ctx context.Context) error

	// OnDelete, if set, is called after a repository is deleted, with the
	// registry lock held, the delete's ctx and the ids of the roles whose
	// privileges on it the delete removed (QA N-25). The app uses it to apply
	// those role changes before a repository of that name can be created
	// again.
	OnDelete func(ctx context.Context, name string, roles []string)

	// Policies looks up a cleanup policy's format ("all" or a format) by
	// name; ok is false when there is no such policy (cleanup.PolicyLookup).
	// Create and Update call it, under the registry lock, for a definition that
	// names a policy. With a nil Policies no policy can be named.
	Policies func(ctx context.Context, name string) (format string, ok bool, err error)

	// HasStore reports whether a blob store exists. Create and Update check,
	// under the registry lock, that a non-group definition's store exists, so
	// a repository never names a store routing cannot resolve. Blob-store
	// create and delete take the same lock (WithWriteLock). A nil HasStore
	// skips the check (tests without stores).
	HasStore func(name string) bool
}

// checkStore refuses a definition whose blob store does not exist.
func (r *Registry) checkStore(def config.Repository) error {
	if r.HasStore == nil || def.Type == "group" {
		return nil
	}
	if name := StoreOf(def); !r.HasStore(name) {
		return &ValidationError{Errors: []string{fmt.Sprintf("repository %q: unknown blob store %q", def.Name, name)}}
	}
	return nil
}

// ValidationError lists every problem with a rejected write. It maps to 400.
type ValidationError struct {
	Errors []string
}

func (e *ValidationError) Error() string { return strings.Join(e.Errors, "; ") }

// ClearSecrets holds Update's options. Password and BearerToken ask it to
// drop the stored remote credentials; without them, an empty password or
// bearer token keeps the stored value. KeepCleanupPolicy keeps the stored
// cleanupPolicy, whatever def says (the API's absent field); it is applied
// under the write lock, so a concurrent change is not undone.
type ClearSecrets struct {
	Password          bool
	BearerToken       bool
	SigningKey        bool // drop a stored apt signingKey; otherwise an empty one keeps it
	KeepCleanupPolicy bool
	KeepFirewall      bool // keep the stored firewall, whatever def says (the API's absent field)
}

// NewRegistry publishes the first snapshot from defs, which must already be
// validated with defaults applied, each with Gen 0; Reload reads them, with
// their gens, from db instead. db receives the writes; with a nil db the
// registry is read-only (tests).
func NewRegistry(db *meta.DB, defs []config.Repository) *Registry {
	r := &Registry{db: db}
	r.snap.Store(newSnapshot(defs, nil))
	return r
}

// registryLock is the cluster lock of the registry's writes. Lock order
// (meta/cluster.go): "access", then "registry", then the formats' keys, then
// a write transaction.
const registryLock = "registry"

// lock takes the registry lock for a write, then re-reads what the write
// validates against.
func (r *Registry) lock(ctx context.Context) (func(), error) {
	if err := r.store(); err != nil {
		return nil, err
	}
	unlock, err := r.db.Lock(ctx, registryLock)
	if err != nil {
		return nil, err
	}
	if err := r.refresh(ctx); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// refresh re-reads the stored repositories and routing rules and runs
// Refresh. The registry lock is held, so no node writes them meanwhile.
func (r *Registry) refresh(ctx context.Context) error {
	if err := r.Reload(ctx); err != nil {
		return err
	}
	if err := r.ReloadRoutes(ctx); err != nil {
		return err
	}
	if r.Refresh != nil {
		return r.Refresh(ctx)
	}
	return nil
}

// Resync takes the registry lock to re-read the repositories,
// routing rules and what Refresh reads, as a write does first: a node
// applies the changes of the others so (Notify, and a periodic safety net).
func (r *Registry) Resync(ctx context.Context) error {
	unlock, err := r.lock(ctx)
	if err != nil {
		return err
	}
	unlock()
	return nil
}

// written tells the other nodes of a committed registry write.
func (r *Registry) written(ctx context.Context) {
	r.db.Notify(context.WithoutCancel(ctx), registryLock) // missed, it is the 5 s reload's
}

// Reload replaces the snapshot with the stored repositories, normalized as
// at start (config.NormalizeStored), and their gens. Call it with the
// registry lock held (Resync), or before the registry is shared.
func (r *Registry) Reload(ctx context.Context) error {
	if err := r.store(); err != nil {
		return err
	}
	defs, gens, err := r.db.ListRepositoriesGen(ctx)
	if err != nil {
		return err
	}
	for i := range defs {
		config.NormalizeStored(&defs[i])
	}
	if err := config.ValidateRepositories(defs); err != nil {
		return fmt.Errorf("stored repositories are invalid: %w", err)
	}
	r.snap.Store(newSnapshot(defs, gens))
	return nil
}

// Lookup is Get for a request naming a repository: a name the snapshot
// lacks is looked up in the database, and when another node created it
// this node resyncs at once (Resync, one at a time), so the repository,
// and the blob store and routing rule it names, are served right away.
func (r *Registry) Lookup(ctx context.Context, name string) *Repository {
	if rp := r.Get(name); rp != nil || r.db == nil {
		return rp
	}
	if _, err := r.db.RepositoryGen(ctx, name); err != nil {
		return nil
	}
	r.sf.Do("resync", func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lookupWait)
		defer cancel()
		return nil, r.Resync(ctx)
	})
	return r.Get(name)
}

// lookupWait bounds Lookup's wait for the registry lock.
const lookupWait = 10 * time.Second

// gens returns the Gen of every repository of the current snapshot.
func (r *Registry) gens() map[string]uint64 {
	s := r.snap.Load()
	out := make(map[string]uint64, len(s.Sorted))
	for _, rp := range s.Sorted {
		out[rp.Name] = rp.Gen
	}
	return out
}

func newSnapshot(defs []config.Repository, gens map[string]uint64) *Snapshot {
	m := buildMap(defs)
	sorted := make([]*Repository, 0, len(m))
	for _, rp := range m {
		rp.Gen = gens[rp.Name]
		sorted = append(sorted, rp)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	return &Snapshot{ByName: m, Sorted: sorted}
}

// Snapshot returns the current snapshot.
func (r *Registry) Snapshot() *Snapshot { return r.snap.Load() }

// Get returns the named repository from the current snapshot, or nil.
func (r *Registry) Get(name string) *Repository { return r.snap.Load().ByName[name] }

// AnonymousRead returns the repository's anonymousRead override, or nil to
// inherit (also for unknown repositories). It suits auth.Authenticator.
func (r *Registry) AnonymousRead(name string) *bool {
	if rp := r.Get(name); rp != nil {
		return rp.AnonymousRead
	}
	return nil
}

// definitions returns a fresh copy of the current definition list.
func (r *Registry) definitions() []config.Repository {
	s := r.snap.Load()
	out := make([]config.Repository, 0, len(s.Sorted))
	for _, rp := range s.Sorted {
		out = append(out, rp.Repository)
	}
	return out
}

func (r *Registry) store() error {
	if r.db == nil {
		return errors.New("repository registry is read-only")
	}
	return nil
}

// Create validates and stores a new repository, then publishes it.
// Create is CreateDef without the stored definition.
func (r *Registry) Create(ctx context.Context, def config.Repository) error {
	_, err := r.CreateDef(ctx, def)
	return err
}

// CreateDef is Create, returning the stored definition (defaults applied),
// read under the write lock so a concurrent Delete cannot invalidate it.
func (r *Registry) CreateDef(ctx context.Context, def config.Repository) (config.Repository, error) {
	unlock, err := r.lock(ctx)
	if err != nil {
		return config.Repository{}, err
	}
	defer unlock()
	def = cloneDef(def)
	if err := validateNewName(def.Name); err != nil {
		return config.Repository{}, err
	}
	ra, err := mergeAuth(def.Name, "", def.Remote, nil, def.RemoteAuth, ClearSecrets{})
	if err != nil {
		return config.Repository{}, err
	}
	def.RemoteAuth = ra
	config.ApplyRepositoryDefaults(&def)
	list := append(r.definitions(), def)
	if err := errors.Join(config.ValidateDefinition(def), config.ValidateRepositories(list)); err != nil {
		return config.Repository{}, validationError(err)
	}
	if err := r.checkStore(def); err != nil {
		return config.Repository{}, err
	}
	if err := r.checkPolicy(ctx, def); err != nil {
		return config.Repository{}, err
	}
	if err := r.checkRoute(def); err != nil {
		return config.Repository{}, err
	}
	gen, err := r.db.CreateRepositoryGen(ctx, def)
	if err != nil {
		if errors.Is(err, meta.ErrExists) {
			return config.Repository{}, &ValidationError{Errors: []string{fmt.Sprintf("duplicate repository name %q", def.Name)}}
		}
		return config.Repository{}, err
	}
	gens := r.gens()
	gens[def.Name] = gen
	r.snap.Store(newSnapshot(list, gens))
	r.written(ctx)
	return cloneDef(def), nil
}

// Update replaces the editable fields of the named repository with def's
// (plan 9 Decision 3). Name, format and type must be empty or unchanged. An
// empty remoteAuth password or bearer token keeps the stored value unless
// clear asks to drop it.
// Update is UpdateDef without the stored definition.
func (r *Registry) Update(ctx context.Context, name string, def config.Repository, clear ClearSecrets) error {
	_, err := r.UpdateDef(ctx, name, def, clear)
	return err
}

// UpdateDef is Update, returning the stored definition under the write lock.
func (r *Registry) UpdateDef(ctx context.Context, name string, def config.Repository, clear ClearSecrets) (config.Repository, error) {
	unlock, err := r.lock(ctx)
	if err != nil {
		return config.Repository{}, err
	}
	defer unlock()
	cur := r.Get(name)
	if cur == nil {
		return config.Repository{}, Errorf(http.StatusNotFound, "repository %q not found", name)
	}
	def = cloneDef(def)
	var errs []string
	keep := func(field string, got *string, want string) {
		switch *got {
		case "":
			*got = want
		case want:
		default:
			errs = append(errs, fmt.Sprintf("repository %q: %s cannot be changed", name, field))
		}
	}
	keep("name", &def.Name, cur.Name)
	keep("format", &def.Format, cur.Format)
	keep("type", &def.Type, cur.Type)
	// blobStore compares the stores StoreOf assigns, so a legacy row stored
	// with "" accepts an explicit "default" (final review M7).
	if def.BlobStore != "" && StoreOf(config.Repository{BlobStore: def.BlobStore}) != StoreOf(config.Repository{BlobStore: cur.BlobStore}) {
		errs = append(errs, fmt.Sprintf("repository %q: blobStore cannot be changed", name))
	}
	def.BlobStore = cur.BlobStore
	if len(errs) > 0 {
		return config.Repository{}, &ValidationError{Errors: errs}
	}
	if def.Online == nil { // absent keeps the stored state
		def.Online = cur.Online
	}
	if def.AutoBlock == nil {
		def.AutoBlock = cur.AutoBlock
	}
	if clear.KeepCleanupPolicy {
		def.CleanupPolicy = cur.CleanupPolicy
	}
	if clear.KeepFirewall {
		def.Firewall = cloneDef(cur.Repository).Firewall
	}
	ra, err := mergeAuth(name, cur.Remote, def.Remote, cur.RemoteAuth, def.RemoteAuth, clear)
	if err != nil {
		return config.Repository{}, err
	}
	def.RemoteAuth = ra
	if def.SigningKey == "" && !clear.SigningKey {
		def.SigningKey = cur.SigningKey
	}
	config.ApplyRepositoryDefaults(&def)

	list := r.definitions()
	for i := range list {
		if list[i].Name == name {
			list[i] = def
		}
	}
	if err := errors.Join(config.ValidateDefinition(def), config.ValidateRepositories(list)); err != nil {
		return config.Repository{}, validationError(err)
	}
	if err := r.checkStore(def); err != nil {
		return config.Repository{}, err
	}
	if err := r.checkPolicy(ctx, def); err != nil {
		return config.Repository{}, err
	}
	if err := r.checkRoute(def); err != nil {
		return config.Repository{}, err
	}
	gen, err := r.db.UpdateRepositoryGen(ctx, def) // a new one when the remote changed
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return config.Repository{}, Errorf(http.StatusNotFound, "repository %q not found", name)
		}
		return config.Repository{}, err
	}
	gens := r.gens()
	if def.Remote != cur.Remote {
		gens[name] = gen
	}
	r.snap.Store(newSnapshot(list, gens))
	r.written(ctx)
	return cloneDef(def), nil
}

// Delete removes the named repository, purges its rows (plan 9 Decision 4)
// and removes the role privileges on it (QA N-25). A repository that is a
// member or writeMember of a group is refused with 409, naming the groups.
func (r *Registry) Delete(ctx context.Context, name string) error {
	unlock, err := r.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	s := r.snap.Load()
	if s.ByName[name] == nil {
		return Errorf(http.StatusNotFound, "repository %q not found", name)
	}
	var groups []string
	for _, rp := range s.Sorted {
		if rp.Type == "group" && (slices.Contains(rp.Members, name) || rp.WriteMember == name) {
			groups = append(groups, rp.Name)
		}
	}
	if len(groups) > 0 {
		return Errorf(http.StatusConflict, "repository %q is used by group repositories: %s", name, strings.Join(groups, ", "))
	}
	roles, err := r.db.DeleteRepository(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return Errorf(http.StatusNotFound, "repository %q not found", name)
		}
		return err
	}
	list := r.definitions()
	list = slices.DeleteFunc(list, func(d config.Repository) bool { return d.Name == name })
	r.snap.Store(newSnapshot(list, r.gens()))
	r.written(ctx)
	if r.OnDelete != nil {
		r.OnDelete(ctx, name, roles)
	}
	return nil
}

// checkPolicy checks the cleanup policy that def names, if any: it must
// exist and be for all formats or def's format. The static rules (hosted or
// proxy, not Cargo hosted) are config.ValidateRepositories'.
func (r *Registry) checkPolicy(ctx context.Context, def config.Repository) error {
	name := def.CleanupPolicy
	if name == "" {
		return nil
	}
	if r.Policies == nil {
		return &ValidationError{Errors: []string{fmt.Sprintf("repository %q: cleanup policies are not available", def.Name)}}
	}
	format, ok, err := r.Policies(ctx, name)
	switch {
	case err != nil:
		return err
	case !ok:
		return &ValidationError{Errors: []string{fmt.Sprintf("repository %q: unknown cleanup policy %q", def.Name, name)}}
	case format != "all" && format != def.Format:
		return &ValidationError{Errors: []string{fmt.Sprintf("repository %q: cleanup policy %q is for %s repositories", def.Name, name, format)}}
	}
	return nil
}

// WithWriteLock runs fn with the registry lock held and the current
// snapshot (re-read first), so no repository is created,
// updated or deleted meanwhile on any node. Cleanup policy, routing rule
// and blob store changes use it: a policy delete or format change must not
// race with a repository being connected to that policy. fn's caller tells
// the other nodes of what it changed.
func (r *Registry) WithWriteLock(ctx context.Context, fn func(*Snapshot) error) error {
	if r.db == nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		return fn(r.snap.Load())
	}
	unlock, err := r.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return fn(r.snap.Load())
}

// newNameRE is what the API accepts for a new repository: a letter or digit,
// then letters, digits, dot, underscore or hyphen. Names read from the config
// file or the database are validated more loosely (config.ValidateRepositories).
var newNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const maxNewNameLen = 64

// validateNewName applies the API's create-only name rules. "create" is
// reserved for the UI route repositories:create.
func validateNewName(name string) error { return CheckName("repository", name) }

// CheckName applies the API's rules for a new name of the given kind
// ("repository", "policy"): at most 64 characters, letters, digits, '.', '_'
// and '-', starting with a letter or digit, and not "create" (reserved for
// the UI's create routes).
func CheckName(kind, name string) error {
	switch {
	case len(name) > maxNewNameLen:
		return &ValidationError{Errors: []string{fmt.Sprintf("%s name is longer than %d characters", kind, maxNewNameLen)}}
	case !newNameRE.MatchString(name):
		return &ValidationError{Errors: []string{fmt.Sprintf("invalid %s name %q: use letters, digits, '.', '_' and '-', starting with a letter or digit", kind, name)}}
	case name == "create":
		return &ValidationError{Errors: []string{fmt.Sprintf("%s name %q is reserved", kind, name)}}
	}
	return nil
}

// mergeAuth applies the secret-keeping rules to an update's remoteAuth:
//   - a nil upd keeps the stored auth as a whole, username included;
//   - an empty password or bearer token keeps the stored one unless clear
//     drops it;
//   - a stored secret never follows a change of the remote's scheme or host
//     (it would be sent to a different server): the update must supply new
//     credentials or clear the old ones;
//   - a password needs a username.
func mergeAuth(name, oldRemote, newRemote string, old, upd *config.RemoteAuth, clear ClearSecrets) (*config.RemoteAuth, error) {
	var out config.RemoteAuth
	switch {
	case upd != nil:
		out = *upd
	case old != nil:
		out.Username = old.Username
	}
	carried := false
	if old != nil {
		if out.Password == "" && !clear.Password && old.Password != "" {
			out.Password, carried = old.Password, true
		}
		if out.BearerToken == "" && !clear.BearerToken && old.BearerToken != "" {
			out.BearerToken, carried = old.BearerToken, true
		}
	}
	if carried && !sameOrigin(oldRemote, newRemote) {
		return nil, &ValidationError{Errors: []string{
			fmt.Sprintf("repository %q: remote host changed: re-enter the remote credentials or clear them", name)}}
	}
	if out.Password != "" && out.Username == "" {
		return nil, &ValidationError{Errors: []string{fmt.Sprintf("repository %q: username required with password", name)}}
	}
	if out == (config.RemoteAuth{}) {
		return nil, nil
	}
	return &out, nil
}

// sameOrigin reports whether two remotes share scheme and host (with port).
func sameOrigin(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

// cloneDef copies the parts of def that are shared by reference, so the
// caller cannot change a published snapshot afterwards.
func cloneDef(def config.Repository) config.Repository {
	def.Members = slices.Clone(def.Members)
	if def.RemoteAuth != nil {
		a := *def.RemoteAuth
		def.RemoteAuth = &a
	}
	if def.Online != nil {
		v := *def.Online
		def.Online = &v
	}
	if def.AnonymousRead != nil {
		v := *def.AnonymousRead
		def.AnonymousRead = &v
	}
	if def.AutoBlock != nil {
		v := *def.AutoBlock
		def.AutoBlock = &v
	}
	if def.Firewall != nil {
		f := *def.Firewall
		f.AllowList = slices.Clone(f.AllowList)
		if f.BlockMalicious != nil {
			v := *f.BlockMalicious
			f.BlockMalicious = &v
		}
		def.Firewall = &f
	}
	return def
}

// validationError flattens the joined errors of config.ValidateRepositories.
func validationError(err error) error {
	var list []string
	var walk func(error)
	walk = func(e error) {
		if u, ok := e.(interface{ Unwrap() []error }); ok {
			for _, x := range u.Unwrap() {
				walk(x)
			}
			return
		}
		list = append(list, e.Error())
	}
	walk(err)
	return &ValidationError{Errors: list}
}
