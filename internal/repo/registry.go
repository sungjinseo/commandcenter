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
// load the current Snapshot; writes are serialized, validated against the
// whole would-be list, persisted, then published as a new Snapshot.
type Registry struct {
	mu   sync.Mutex // serializes writes
	snap atomic.Pointer[Snapshot]
	db   *meta.DB

	// gens gives each repository incarnation a distinct Gen: it changes when a
	// repository is created again or its remote changes, never otherwise.
	gens    map[string]uint64
	nextGen uint64

	// OnDelete, if set, is called after a repository is deleted, with the
	// write lock held. The app uses it to warn about roles naming it.
	OnDelete func(name string)

	// Policies looks up a cleanup policy's format ("all" or a format) by
	// name; ok is false when there is no such policy (cleanup.PolicyLookup).
	// Create and Update call it, under the write lock, for a definition that
	// names a policy. With a nil Policies no policy can be named.
	Policies func(ctx context.Context, name string) (format string, ok bool, err error)

	// HasStore reports whether a blob store exists. Create and Update check,
	// under the write lock, that a non-group definition's store exists, so a
	// repository never names a store routing cannot resolve. Blob-store create
	// and delete take the same lock (WithWriteLock). A nil HasStore skips the
	// check (tests without stores).
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
	KeepCleanupPolicy bool
}

// NewRegistry publishes the first snapshot from defs, which must already be
// validated with defaults applied. db receives the writes; with a nil db the
// registry is read-only (tests).
func NewRegistry(db *meta.DB, defs []config.Repository) *Registry {
	r := &Registry{db: db, gens: map[string]uint64{}}
	for _, d := range defs {
		r.newGen(d.Name)
	}
	r.snap.Store(newSnapshot(defs, r.gens))
	return r
}

// newGen starts a new incarnation of the named repository.
func (r *Registry) newGen(name string) {
	r.nextGen++
	r.gens[name] = r.nextGen
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.store(); err != nil {
		return config.Repository{}, err
	}
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
	if err := r.db.CreateRepository(ctx, def); err != nil {
		if errors.Is(err, meta.ErrExists) {
			return config.Repository{}, &ValidationError{Errors: []string{fmt.Sprintf("duplicate repository name %q", def.Name)}}
		}
		return config.Repository{}, err
	}
	r.newGen(def.Name)
	r.snap.Store(newSnapshot(list, r.gens))
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.store(); err != nil {
		return config.Repository{}, err
	}
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
	if clear.KeepCleanupPolicy {
		def.CleanupPolicy = cur.CleanupPolicy
	}
	ra, err := mergeAuth(name, cur.Remote, def.Remote, cur.RemoteAuth, def.RemoteAuth, clear)
	if err != nil {
		return config.Repository{}, err
	}
	def.RemoteAuth = ra
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
	if err := r.db.UpdateRepository(ctx, def); err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return config.Repository{}, Errorf(http.StatusNotFound, "repository %q not found", name)
		}
		return config.Repository{}, err
	}
	if def.Remote != cur.Remote {
		r.newGen(name)
	}
	r.snap.Store(newSnapshot(list, r.gens))
	return cloneDef(def), nil
}

// Delete removes the named repository and purges its rows (plan 9 Decision
// 4). A repository that is a member or writeMember of a group is refused
// with 409, naming the groups.
func (r *Registry) Delete(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.store(); err != nil {
		return err
	}
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
	if err := r.db.DeleteRepository(ctx, name); err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return Errorf(http.StatusNotFound, "repository %q not found", name)
		}
		return err
	}
	list := r.definitions()
	list = slices.DeleteFunc(list, func(d config.Repository) bool { return d.Name == name })
	delete(r.gens, name)
	r.snap.Store(newSnapshot(list, r.gens))
	if r.OnDelete != nil {
		r.OnDelete(name)
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

// WithWriteLock runs fn with the write lock held and the current snapshot,
// so no repository is created, updated or deleted meanwhile. Cleanup policy
// changes use it: a policy delete or format change must not race with a
// repository being connected to that policy.
func (r *Registry) WithWriteLock(fn func(*Snapshot) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
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
