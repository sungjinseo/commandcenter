package blob

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Blob store groups: a Store whose blobs live in its member stores (file or
// S3, never another group). A read looks in the member that last served or
// stored the blob, then in every member in order; a write goes to one
// writable member, picked by the fill policy; Walk and Remove cover every
// member, so a blob copied to a second member is seen and removed in both.
// The group's own root holds only tmp/ (uploads in progress, Docker
// sessions); a commit moves the temp file into the picked member.

// Fill policies.
const (
	FillRoundRobin   = "roundRobin"   // each write goes to the next writable member
	FillWriteToFirst = "writeToFirst" // every write goes to the first writable member
)

// GroupConfig is a group's stored configuration.
type GroupConfig struct {
	Members    []GroupMember `json:"members"`    // in read order
	FillPolicy string        `json:"fillPolicy"` // FillRoundRobin (the default) or FillWriteToFirst
}

// GroupMember is one member of a group. A read-only member is still read but
// never written: the state of a member being drained.
type GroupMember struct {
	Name     string `json:"name"`
	ReadOnly bool   `json:"readOnly"`
}

// UnmarshalJSON takes a member as {"name", "readOnly"} or as its bare name.
func (m *GroupMember) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		*m = GroupMember{}
		return json.Unmarshal(b, &m.Name)
	}
	type plain GroupMember
	return json.Unmarshal(b, (*plain)(m))
}

// Validate defaults the fill policy and reports the first thing wrong with c
// on its own: whether the members exist is the caller's to check.
func (c *GroupConfig) Validate() error {
	if c.FillPolicy == "" {
		c.FillPolicy = FillRoundRobin
	}
	if c.FillPolicy != FillRoundRobin && c.FillPolicy != FillWriteToFirst {
		return fmt.Errorf("fillPolicy must be %q or %q", FillRoundRobin, FillWriteToFirst)
	}
	if len(c.Members) == 0 {
		return errors.New("a group needs at least one member")
	}
	seen := map[string]bool{}
	writable := false
	for _, m := range c.Members {
		if m.Name == "" {
			return errors.New("a member needs a name")
		}
		if seen[m.Name] {
			return fmt.Errorf("member %q is listed twice", m.Name)
		}
		seen[m.Name] = true
		writable = writable || !m.ReadOnly
	}
	if !writable {
		return errors.New("at least one member must be writable")
	}
	return nil
}

type member struct {
	name     string
	readOnly bool
	s        *Store
}

// groupState is one configuration of a group. A write read-locks mu from
// before it picks a member until its commit returns, so SetGroup can wait
// for the writes that picked under the configuration it replaces.
type groupState struct {
	mu       sync.RWMutex
	cfg      GroupConfig
	members  []member
	writable []*Store
}

type group struct {
	state  atomic.Pointer[groupState]
	next   atomic.Uint64 // round-robin counter
	hintMu sync.Mutex
	hint   map[string]*Store // blob id -> the member that last served or stored it
}

// hintMax bounds the hint map; when full it is dropped and refills from the
// reads (a miss costs one probe per member).
// ponytail: dropped wholesale, an LRU if probing members shows up.
const hintMax = 1 << 16

func newGroupState(cfg GroupConfig, lookup func(string) *Store) (*groupState, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	st := &groupState{cfg: cfg}
	st.cfg.Members = slices.Clone(cfg.Members)
	for _, m := range cfg.Members {
		s := lookup(m.Name)
		if s == nil {
			return nil, fmt.Errorf("member %q does not exist", m.Name)
		}
		if _, nested := s.be.(*group); nested {
			return nil, fmt.Errorf("member %q is a group: groups do not nest", m.Name)
		}
		st.members = append(st.members, member{name: m.Name, readOnly: m.ReadOnly, s: s})
		if !m.ReadOnly {
			st.writable = append(st.writable, s)
		}
	}
	return st, nil
}

// OpenGroup opens a group at root (its temp files) over the member stores
// lookup returns by name.
func OpenGroup(root string, cfg GroupConfig, lookup func(name string) *Store) (*Store, error) {
	st, err := newGroupState(cfg, lookup)
	if err != nil {
		return nil, err
	}
	g := &group{hint: map[string]*Store{}}
	g.state.Store(st)
	return open(root, g)
}

// SetGroup replaces a group's members and fill policy. When it returns, no
// write is still committing to a member the old configuration let it pick, so
// a member just made read-only or removed receives nothing more.
func (s *Store) SetGroup(cfg GroupConfig, lookup func(name string) *Store) error {
	g, ok := s.be.(*group)
	if !ok {
		return errors.New("not a blob store group")
	}
	st, err := newGroupState(cfg, lookup)
	if err != nil {
		return err
	}
	old := g.state.Swap(st)
	old.mu.Lock() // waits for the writes that picked under old
	old.mu.Unlock()
	g.hintMu.Lock()
	clear(g.hint)
	g.hintMu.Unlock()
	return nil
}

// Group returns a group's configuration; ok is false for a file or S3 store.
func (s *Store) Group() (cfg GroupConfig, ok bool) {
	g, ok := s.be.(*group)
	if !ok {
		return GroupConfig{}, false
	}
	cfg = g.state.Load().cfg
	cfg.Members = slices.Clone(cfg.Members)
	return cfg, true
}

func (g *group) hinted(sha string) *Store {
	g.hintMu.Lock()
	defer g.hintMu.Unlock()
	return g.hint[sha]
}

func (g *group) remember(sha string, s *Store) {
	g.hintMu.Lock()
	if len(g.hint) >= hintMax {
		clear(g.hint)
	}
	g.hint[sha] = s
	g.hintMu.Unlock()
}

// find calls fn on the hinted member, then on every other member in order,
// until one succeeds. With none, it returns the first error that is not
// ErrNotFound (a member that is down must not read as a missing blob), else
// ErrNotFound.
func (g *group) find(sha string, fn func(backend) error) error {
	st := g.state.Load()
	h := g.hinted(sha)
	order := make([]*Store, 0, len(st.members)+1)
	for _, m := range st.members {
		if m.s == h {
			order = append([]*Store{h}, order...)
		} else {
			order = append(order, m.s)
		}
	}
	var first error
	for _, s := range order {
		err := fn(s.be)
		if err == nil {
			if s != h {
				g.remember(sha, s)
			}
			return nil
		}
		if !errors.Is(err, ErrNotFound) && first == nil {
			first = err
		}
	}
	if first != nil {
		return first
	}
	return ErrNotFound
}

func (g *group) open(sha string) (File, error) {
	var f File
	err := g.find(sha, func(b backend) (err error) { f, err = b.open(sha); return err })
	return f, err
}

func (g *group) stat(sha string) (Info, error) {
	var info Info
	err := g.find(sha, func(b backend) (err error) { info, err = b.stat(sha); return err })
	return info, err
}

// put commits to the member the fill policy picks: the hinted one when it
// is writable (it holds the blob already), else the first writable member or
// the next one round robin. A failed commit is not retried on another member.
func (g *group) put(tmp string, sums Sums) error {
	for {
		st := g.state.Load()
		st.mu.RLock()
		if g.state.Load() != st { // SetGroup ran in between: pick under the new configuration
			st.mu.RUnlock()
			continue
		}
		s := g.pick(st, sums.SHA256)
		err := s.be.put(tmp, sums)
		st.mu.RUnlock()
		if err == nil {
			g.remember(sums.SHA256, s)
		}
		return err
	}
}

func (g *group) pick(st *groupState, sha string) *Store {
	if h := g.hinted(sha); h != nil && slices.Contains(st.writable, h) {
		return h
	}
	if st.cfg.FillPolicy == FillWriteToFirst {
		return st.writable[0]
	}
	return st.writable[(g.next.Add(1)-1)%uint64(len(st.writable))]
}

// remove deletes the blob from every member.
func (g *group) remove(sha string) error {
	var errs []error
	for _, m := range g.state.Load().members {
		if err := m.s.be.remove(sha); err != nil {
			errs = append(errs, fmt.Errorf("member %q: %w", m.name, err))
		}
	}
	return errors.Join(errs...)
}

// walk walks every member in order: a blob held by two members is seen twice.
func (g *group) walk(fn func(sha string, size int64, modTime time.Time) error) error {
	for _, m := range g.state.Load().members {
		if err := m.s.be.walk(fn); err != nil {
			return fmt.Errorf("member %q: %w", m.name, err)
		}
	}
	return nil
}

func (g *group) check() error {
	var errs []error
	for _, m := range g.state.Load().members {
		if err := m.s.be.check(); err != nil {
			errs = append(errs, fmt.Errorf("member %q: %w", m.name, err))
		}
	}
	return errors.Join(errs...)
}

// Probe is one reachability check of a store's backend.
type Probe struct {
	Target string `json:"target"` // what was checked: a directory, a bucket (primary or failover), a group member's
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

func probe(target string, err error) Probe {
	if err != nil {
		return Probe{Target: target, Error: err.Error()}
	}
	return Probe{Target: target, OK: true}
}

// Health checks the store's backend: a directory store that its tmp accepts
// a file, an S3 store that its bucket and each failover bucket lists, a
// group each member's, a GCS store that its bucket answers.
func (s *Store) Health() []Probe {
	switch be := s.be.(type) {
	case *s3:
		out := []Probe{probe("primary "+be.cfg.Location(), be.check())}
		for i, f := range be.failover {
			out = append(out, probe(fmt.Sprintf("failover %d %s", i+1, f.cfg.Location()), f.check()))
		}
		return out
	case *group:
		var out []Probe
		for _, m := range be.state.Load().members {
			for _, p := range m.s.Health() {
				p.Target = "member " + m.name + ": " + p.Target
				out = append(out, p)
			}
		}
		return out
	case *azure:
		return []Probe{probe("container "+be.cfg.Location(), be.check())}
	case *gcs:
		return []Probe{probe("bucket "+be.cfg.Location(), be.check())}
	}
	f, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "probe-*")
	if err == nil {
		f.Close()
		err = os.Remove(f.Name())
	}
	return []Probe{probe("directory "+s.root, err)}
}
