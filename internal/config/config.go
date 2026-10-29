// Package config loads and validates the Command Center YAML configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"commandcenter/internal/pgp"
)

// Duration is a time.Duration that unmarshals from strings like "24h".
// The value "-1" means "never expire". Zero means "use the default".
type Duration time.Duration

// Never marks a cache entry that never expires.
const Never Duration = -1

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	if s == "-1" {
		*d = Never
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Text renders d like the config file does: "" for zero (the default), "-1"
// for never, otherwise "24h", "10m", "30s" or Go duration syntax.
func (d Duration) Text() string {
	switch {
	case d == 0:
		return ""
	case d == Never:
		return "-1"
	}
	v := time.Duration(d)
	switch {
	case v%time.Hour == 0:
		return fmt.Sprintf("%dh", v/time.Hour)
	case v%time.Minute == 0:
		return fmt.Sprintf("%dm", v/time.Minute)
	case v%time.Second == 0:
		return fmt.Sprintf("%ds", v/time.Second)
	}
	return v.String()
}

// ParseDuration parses the config format: "" (zero), "-1" (never) or a Go
// duration. It accepts negative durations, which older builds stored: the
// definition being created or updated rejects them (ValidateDefinition), and
// a stored row treats them as 0 (NormalizeStored; final review C1).
func ParseDuration(s string) (Duration, error) {
	switch s {
	case "":
		return 0, nil
	case "-1":
		return Never, nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return Duration(v), nil
}

// MarshalJSON writes d as a string in the config format (see Text).
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.Text()) }

// UnmarshalJSON reads a string in the config format (see ParseDuration).
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"24h\" or \"-1\": %w", err)
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// Database is the metadata database. Without a URL it is SQLite at
// <dataDir>/meta.db; COMMANDCENTER_DATABASE_URL may give the URL instead
// (the app reads it, see app.OpenDB).
type Database struct {
	URL      string `yaml:"url"`      // postgres:// or postgresql://; never logged (it may hold a password)
	MaxConns int    `yaml:"maxConns"` // connection pool size; 0: 4 for SQLite, 10 for PostgreSQL
}

type Config struct {
	Listen              string          `yaml:"listen"`
	BaseURL             string          `yaml:"baseURL"`
	DataDir             string          `yaml:"dataDir"`
	MemoryLimitMiB      int             `yaml:"memoryLimitMiB"`
	MaxUploadMiB        int             `yaml:"maxUploadMiB"`
	MetricsFlushSeconds int             `yaml:"metricsFlushSeconds"` // hidden: how often download/request counters are written (default 30)
	SecretKeyFile       string          `yaml:"secretKeyFile"`       // file holding the key that encrypts stored secrets (or COMMANDCENTER_SECRET_KEY)
	Database            Database        `yaml:"database"`
	AnonymousRead       bool            `yaml:"anonymousRead"`
	Users               []User          `yaml:"users"`
	Roles               map[string]Role `yaml:"roles"`
	Repositories        []Repository    `yaml:"repositories"`

	// Present names the top-level keys the file sets, so the app can warn
	// about keys that the database has superseded.
	Present map[string]bool `yaml:"-"`

	// Path is the file Load read the configuration from; empty for Parse.
	Path string `yaml:"-"`

	// RepositoriesErr reports a problem with the repositories section. Parse
	// does not fail on it: the section only matters until the first import.
	RepositoriesErr error `yaml:"-"`

	// AccessErr reports a problem with the users or roles sections. Like
	// RepositoriesErr it does not fail Parse: the sections only matter until
	// the first import (plan 12).
	AccessErr error `yaml:"-"`

	// SettingsErr reports a problem with the import-only baseURL or
	// maxUploadMiB keys. It does not fail Parse either: after the first
	// import the database holds the values and the keys are ignored.
	SettingsErr error `yaml:"-"`
}

type User struct {
	Name         string   `yaml:"name"`
	PasswordHash string   `yaml:"passwordHash"`
	Roles        []string `yaml:"roles"`
}

// Role grants write access to the named repositories ("*" means all).
// The role name "admin" is built in and grants everything.
type Role struct {
	Write []string `yaml:"write"`
}

type RemoteAuth struct {
	Username    string `yaml:"username" json:"username,omitempty"`
	Password    string `yaml:"password" json:"password,omitempty"`
	BearerToken string `yaml:"bearerToken" json:"bearerToken,omitempty"`
}

type Repository struct {
	Name   string `yaml:"name" json:"name,omitempty"`
	Format string `yaml:"format" json:"format,omitempty"`
	Type   string `yaml:"type" json:"type,omitempty"`

	// proxy
	Remote           string      `yaml:"remote" json:"remote,omitempty"`
	RemoteAuth       *RemoteAuth `yaml:"remoteAuth" json:"remoteAuth,omitempty"`
	MetadataMaxAge   Duration    `yaml:"metadataMaxAge" json:"metadataMaxAge,omitempty"`
	ContentMaxAge    Duration    `yaml:"contentMaxAge" json:"contentMaxAge,omitempty"`
	NegativeCacheTTL Duration    `yaml:"negativeCacheTTL" json:"negativeCacheTTL,omitempty"`

	// group
	Members     []string `yaml:"members" json:"members,omitempty"`
	WriteMember string   `yaml:"writeMember" json:"writeMember,omitempty"`

	// hosted
	VersionPolicy string `yaml:"versionPolicy" json:"versionPolicy,omitempty"` // maven only: release | snapshot | mixed
	WritePolicy   string `yaml:"writePolicy" json:"writePolicy,omitempty"`     // allow | allow_once | deny
	// Depth is where a yum hosted repository keeps its repodata/: 0 at the
	// root, listing every RPM; 1 in each top-level directory, listing its
	// RPMs (Nexus "repodata depth").
	Depth int `yaml:"depth,omitempty" json:"depth,omitempty"` // yum hosted only: 0 | 1

	// apt hosted: the dists/<distribution> and component the generated
	// Release and Packages files are published under (defaults "stable"
	// and "main"), and the ASCII-armored, unencrypted RSA OpenPGP private
	// key that signs them (Release.gpg, InRelease); without one the
	// repository is unsigned ([trusted=yes]). The key is sealed at rest
	// like the remote credentials.
	Distribution string `yaml:"distribution,omitempty" json:"distribution,omitempty"`
	Component    string `yaml:"component,omitempty" json:"component,omitempty"`
	SigningKey   string `yaml:"signingKey,omitempty" json:"signingKey,omitempty"`

	AnonymousRead *bool `yaml:"anonymousRead" json:"anonymousRead,omitempty"` // overrides Config.AnonymousRead

	// CleanupPolicy names the cleanup policy applied to this repository
	// (hosted and proxy only; never Cargo hosted). Whether the policy exists
	// and matches the format is checked by the registry, which can see the
	// stored policies (plan 11 Decision 5).
	CleanupPolicy string `yaml:"cleanupPolicy" json:"cleanupPolicy,omitempty"`

	// Online is a pointer so that a definition stored before plan 12 (no
	// "online" key) reads as online; ApplyRepositoryDefaults sets nil to
	// true. An offline repository answers 503 and groups skip it; npm and
	// PyPI groups answer 503 instead unless a member before it has the
	// requested package.
	Online *bool `yaml:"online,omitempty" json:"online,omitempty"`

	// BlobStore names the file store this repository's blobs live in
	// (plan 12 Decision 7), chosen at creation and immutable afterwards.
	// ApplyRepositoryDefaults sets an empty value to "default" for non-group
	// repositories; group repositories have no blob store. A definition
	// stored before plan 12 (no "blobStore" key) resolves to "default".
	BlobStore string `yaml:"blobStore,omitempty" json:"blobStore,omitempty"`
}

// IsOnline reports whether the repository serves requests; unset means online.
func (r Repository) IsOnline() bool { return r.Online == nil || *r.Online }

var (
	knownFormats = map[string]bool{"maven": true, "npm": true, "go": true, "pypi": true, "cargo": true, "docker": true, "raw": true, "helm": true, "yum": true, "nuget": true, "apt": true, "composer": true, "conda": true}
	// aptNameRE is a distribution or component name: path segments, as
	// apt's sources.list takes them ("stable", "bookworm-updates", "main").
	aptNameRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*(/[A-Za-z0-9][A-Za-z0-9._+-]*)*$`)
	knownTypes     = map[string]bool{"hosted": true, "proxy": true, "group": true}
	versionPolicys = map[string]bool{"release": true, "snapshot": true, "mixed": true}
	writePolicies  = map[string]bool{"allow": true, "allow_once": true, "deny": true}
)

// Load reads and validates the configuration file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	c, err := Parse(b)
	if err != nil {
		return nil, err
	}
	c.Path = path
	return c, nil
}

// Parse decodes YAML, applies defaults and validates the result.
//
// The repositories section is decoded and validated on its own: once the
// repositories have been imported into the database it is ignored, so a
// problem there must not stop startup. Such a problem is reported in
// RepositoriesErr, and the app decides whether it matters.
func Parse(b []byte) (*Config, error) {
	rest, reposNode, err := splitRepositories(b)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	var c Config
	if err := decodeStrict(rest, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if reposNode != nil {
		c.RepositoriesErr = decodeRepositories(reposNode, &c.Repositories)
	}
	c.Present = topLevelKeys(b)
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func decodeStrict(b []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	return dec.Decode(v)
}

// splitRepositories removes the top-level repositories entry from the
// document. It returns the remaining YAML and the entry's value node, or b
// unchanged and nil when there is no such entry.
func splitRepositories(b []byte) ([]byte, *yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return b, nil, nil
	}
	m := doc.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != "repositories" {
			continue
		}
		value := m.Content[i+1]
		m.Content = append(m.Content[:i:i], m.Content[i+2:]...)
		rest, err := yaml.Marshal(&doc)
		if err != nil {
			return nil, nil, err
		}
		return rest, value, nil
	}
	return b, nil, nil
}

// decodeRepositories decodes the repositories section strictly into out.
func decodeRepositories(n *yaml.Node, out *[]Repository) error {
	b, err := yaml.Marshal(n)
	if err != nil {
		return fmt.Errorf("repositories: %w", err)
	}
	var list []Repository
	if err := decodeStrict(b, &list); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("repositories: %w", err)
	}
	*out = list
	return nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8081"
	}
	if c.BaseURL == "" {
		c.BaseURL = "http://localhost:8081"
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.DataDir == "" {
		c.DataDir = "/data"
	}
	if c.MaxUploadMiB == 0 {
		c.MaxUploadMiB = 1024
	}
	if c.MetricsFlushSeconds == 0 {
		c.MetricsFlushSeconds = 30
	}
	if c.MemoryLimitMiB == 0 {
		c.MemoryLimitMiB = 80
	}
	for i := range c.Repositories {
		ApplyRepositoryDefaults(&c.Repositories[i])
	}
}

// ApplyRepositoryDefaults fills the zero-valued fields of r with their defaults.
func ApplyRepositoryDefaults(r *Repository) {
	if r.MetadataMaxAge == 0 {
		r.MetadataMaxAge = Duration(24 * time.Hour)
	}
	if r.ContentMaxAge == 0 {
		r.ContentMaxAge = Never
	}
	if r.NegativeCacheTTL == 0 {
		r.NegativeCacheTTL = Duration(10 * time.Minute)
	}
	if r.Online == nil {
		online := true
		r.Online = &online
	}
	// Non-group repositories default to the "default" blob store; groups have
	// none (they serve from their members).
	if r.Type != "group" && r.BlobStore == "" {
		r.BlobStore = "default"
	}
	if r.Type == "hosted" {
		if r.WritePolicy == "" {
			r.WritePolicy = "allow_once"
		}
		if r.Format == "maven" && r.VersionPolicy == "" {
			r.VersionPolicy = "release"
		}
		if r.Format == "apt" {
			if r.Distribution == "" {
				r.Distribution = "stable"
			}
			if r.Component == "" {
				r.Component = "main"
			}
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if c.MetricsFlushSeconds < 1 || c.MetricsFlushSeconds > 3600 {
		add("metricsFlushSeconds must be between 1 and 3600")
	}
	if c.Database.MaxConns < 0 || c.Database.MaxConns > 1000 {
		add("database.maxConns must be between 1 and 1000 (0: the default)")
	}
	// baseURL and maxUploadMiB are import-only (plan 12): a problem is
	// reported in SettingsErr and only matters before the first import.
	var serrs []error
	if c.MaxUploadMiB < 0 || c.MaxUploadMiB > 1<<20 {
		serrs = append(serrs, errors.New("maxUploadMiB must be between 1 and 1048576"))
	}
	if err := ValidateBaseURL(c.BaseURL); err != nil {
		serrs = append(serrs, err)
	}
	c.SettingsErr = errors.Join(serrs...)

	// The repositories section is reported separately (see Parse).
	if c.RepositoriesErr == nil {
		// The section is only used for the one-time import, which creates
		// new rows: each definition gets the create-time checks too.
		var rerrs []error
		for _, r := range c.Repositories {
			rerrs = append(rerrs, ValidateDefinition(r))
		}
		c.RepositoriesErr = errors.Join(append(rerrs, ValidateRepositories(c.Repositories))...)
	}

	// Users and roles only matter until they are imported into the database
	// (plan 12), so their problems are reported in AccessErr, like
	// repositories, and the app decides whether they matter.
	c.AccessErr = ValidateAccess(c.Users, c.Roles)
	return errors.Join(errs...)
}

// ValidateBaseURL checks a baseURL: an absolute http(s) URL without user
// info, query or fragment. Clients get it as the prefix of every npm, PyPI
// and Cargo URL, so user info would hand its credentials to all of them, and
// a '?' or '#' (even with nothing after it) would cut every URL built on it
// (QA N-24). The error does not repeat the value: it may hold a password.
func ValidateBaseURL(v string) error {
	if u, err := url.Parse(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || strings.ContainsAny(v, "?#") {
		return errors.New("baseURL must be an absolute http(s) URL without user info, query or fragment")
	}
	return nil
}

// ValidateAccess checks the users and roles sections. A role's write entries
// may name repositories that exist only in the metadata database, so they
// are not checked here (plan 9 Decision 2).
func ValidateAccess(list []User, roles map[string]Role) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	users := map[string]bool{}
	for _, u := range list {
		if u.Name == "" || users[u.Name] {
			add("invalid or duplicate user name %q", u.Name)
			continue
		}
		users[u.Name] = true
		if u.PasswordHash == "" {
			add("user %q: passwordHash is required", u.Name)
		}
		for _, role := range u.Roles {
			if _, ok := roles[role]; !ok && role != "admin" {
				add("user %q: unknown role %q", u.Name, role)
			}
		}
	}
	return errors.Join(errs...)
}

// durationFields lists the cache durations of r with their config names.
func durationFields(r *Repository) []namedDuration {
	return []namedDuration{
		{"metadataMaxAge", &r.MetadataMaxAge},
		{"contentMaxAge", &r.ContentMaxAge},
		{"negativeCacheTTL", &r.NegativeCacheTTL},
	}
}

type namedDuration struct {
	name string
	d    *Duration
}

// ValidateDefinition checks the rules that apply only to a definition being
// created or updated (API/UI, or the one-time import of the config file): no
// negative duration other than "-1" (never), no group member listed twice,
// no remote (or remoteAuth) except on a proxy and no versionPolicy on a
// group (QA L-9), and no remote host that is not ASCII, which remote fetches
// refuse. Rows that older builds stored are not held to them; the app loads
// those through NormalizeStored instead (final review C1).
func ValidateDefinition(r Repository) error {
	var errs []error
	if r.Type != "proxy" && (r.Remote != "" || r.RemoteAuth != nil) {
		errs = append(errs, fmt.Errorf("repository %q: remote and remoteAuth are only valid for proxy repositories", r.Name))
	}
	if u, err := url.Parse(r.Remote); r.Type == "proxy" && err == nil && strings.ContainsFunc(u.Hostname(), func(c rune) bool { return c >= 0x80 }) {
		errs = append(errs, fmt.Errorf("repository %q: remote host %q is not ASCII; use its punycode form (xn--...)", r.Name, u.Hostname()))
	}
	if r.Type == "group" && r.VersionPolicy != "" {
		errs = append(errs, fmt.Errorf("repository %q: versionPolicy is not valid for group repositories", r.Name))
	}
	if (r.Distribution != "" || r.Component != "" || r.SigningKey != "") && (r.Format != "apt" || r.Type != "hosted") {
		errs = append(errs, fmt.Errorf("repository %q: distribution, component and signingKey are only valid for apt hosted repositories", r.Name))
	}
	for _, f := range []struct{ name, v string }{{"distribution", r.Distribution}, {"component", r.Component}} {
		if f.v != "" && !aptNameRE.MatchString(f.v) {
			errs = append(errs, fmt.Errorf("repository %q: %s %q: use letters, digits, '.', '_', '+' and '-'", r.Name, f.name, f.v))
		}
	}
	if r.SigningKey != "" {
		if _, err := pgp.ParsePrivateKey(r.SigningKey); err != nil {
			errs = append(errs, fmt.Errorf("repository %q: signingKey: %w", r.Name, err))
		}
	}
	for _, f := range durationFields(&r) {
		if *f.d < 0 && *f.d != Never {
			errs = append(errs, fmt.Errorf("repository %q: %s %q must not be negative (use \"-1\" for never)", r.Name, f.name, f.d.Text()))
		}
	}
	seen := map[string]bool{}
	for _, m := range r.Members {
		if seen[m] {
			errs = append(errs, fmt.Errorf("repository %q: duplicate member %q", r.Name, m))
			continue
		}
		seen[m] = true
	}
	return errors.Join(errs...)
}

// NormalizeStored makes a stored definition that an older build accepted
// usable: a negative duration other than "-1" becomes 0, which the proxy
// treats as never fresh / expiring at once (as those builds behaved), a
// group member listed twice is kept once, and a remote outside a proxy or a
// group's versionPolicy (both unused) is dropped. It returns one note per
// change for the caller's warning; the stored row is not rewritten.
func NormalizeStored(r *Repository) []string {
	var notes []string
	if r.Type != "proxy" && (r.Remote != "" || r.RemoteAuth != nil) {
		notes = append(notes, "remote ignored: only proxy repositories have one")
		r.Remote, r.RemoteAuth = "", nil
	}
	if r.Type == "group" && r.VersionPolicy != "" {
		notes = append(notes, fmt.Sprintf("versionPolicy %q ignored: groups have none", r.VersionPolicy))
		r.VersionPolicy = ""
	}
	for _, f := range durationFields(r) {
		if *f.d < 0 && *f.d != Never {
			notes = append(notes, fmt.Sprintf("%s %q is negative, treated as 0 (no caching)", f.name, f.d.Text()))
			*f.d = 0
		}
	}
	if len(r.Members) > 1 {
		seen := map[string]bool{}
		out := make([]string, 0, len(r.Members))
		for _, m := range r.Members {
			if seen[m] {
				notes = append(notes, fmt.Sprintf("duplicate member %q ignored", m))
				continue
			}
			seen[m] = true
			out = append(out, m)
		}
		r.Members = out
	}
	return notes
}

// ValidateRepositories checks a complete list of repository definitions,
// defaults already applied. The result joins one error per problem (it
// implements Unwrap() []error), in list order; nil means the list is valid.
func ValidateRepositories(list []Repository) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	repos := map[string]*Repository{}
	var order []*Repository
	for i := range list {
		r := &list[i]
		if r.Name == "" || strings.ContainsAny(r.Name, "/ \\") {
			add("invalid repository name %q", r.Name)
			continue
		}
		if repos[r.Name] != nil {
			add("duplicate repository name %q", r.Name)
			continue
		}
		repos[r.Name] = r
		order = append(order, r)
		if !knownFormats[r.Format] {
			add("repository %q: unknown format %q", r.Name, r.Format)
		}
		if !knownTypes[r.Type] {
			add("repository %q: unknown type %q", r.Name, r.Type)
		}
		if r.Type == "proxy" {
			if u, err := url.Parse(r.Remote); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				add("repository %q: proxy needs an http(s) remote", r.Name)
			}
		}
		if r.Type == "group" && len(r.Members) == 0 {
			add("repository %q: group needs members", r.Name)
		}
		if r.Type != "group" && (len(r.Members) > 0 || r.WriteMember != "") {
			add("repository %q: members/writeMember are only valid for groups", r.Name)
		}
		if r.Type == "group" && r.BlobStore != "" {
			add("repository %q: groups have no blob store", r.Name)
		}
		if r.VersionPolicy != "" {
			if r.Format != "maven" {
				add("repository %q: versionPolicy is only valid for maven", r.Name)
			} else if !versionPolicys[r.VersionPolicy] {
				add("repository %q: unknown versionPolicy %q", r.Name, r.VersionPolicy)
			}
		}
		if r.WritePolicy != "" && !writePolicies[r.WritePolicy] {
			add("repository %q: unknown writePolicy %q", r.Name, r.WritePolicy)
		}
		if r.Depth != 0 {
			if r.Format != "yum" || r.Type != "hosted" {
				add("repository %q: depth is only valid for yum hosted repositories", r.Name)
			} else if r.Depth != 1 {
				add("repository %q: depth must be 0 or 1", r.Name)
			}
		}
		if r.CleanupPolicy != "" {
			switch {
			case r.Type != "hosted" && r.Type != "proxy":
				add("repository %q: cleanupPolicy is only valid for hosted and proxy repositories", r.Name)
			case r.Format == "cargo" && r.Type == "hosted":
				add("repository %q: cleanup is not supported for cargo hosted repositories (yank crates, or delete versions in the web UI)", r.Name)
			case r.Format == "docker":
				add("repository %q: cleanup is not supported for docker repositories (delete manifests in the web UI or with the registry API)", r.Name)
			}
		}
	}

	for _, r := range order {
		if r.Type != "group" {
			continue
		}
		for _, m := range r.Members {
			mr := repos[m]
			switch {
			case mr == nil:
				add("repository %q: unknown member %q", r.Name, m)
			case mr.Format != r.Format:
				add("repository %q: member %q has format %s", r.Name, m, mr.Format)
			}
		}
		if r.WriteMember != "" {
			mr := repos[r.WriteMember]
			if mr == nil || mr.Type != "hosted" || !contains(r.Members, r.WriteMember) {
				add("repository %q: writeMember %q must be a hosted member", r.Name, r.WriteMember)
			}
		}
	}
	if name := findCycle(repos); name != "" {
		add("group cycle involving repository %q", name)
	}
	return errors.Join(errs...)
}

// findCycle returns the name of a group that is part of a membership cycle, or "".
func findCycle(repos map[string]*Repository) string {
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var visit func(name string) string
	visit = func(name string) string {
		switch state[name] {
		case visiting:
			return name
		case done:
			return ""
		}
		state[name] = visiting
		if r := repos[name]; r != nil {
			for _, m := range r.Members {
				if c := visit(m); c != "" {
					return c
				}
			}
		}
		state[name] = done
		return ""
	}
	for name := range repos {
		if c := visit(name); c != "" {
			return c
		}
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// topLevelKeys returns the keys of the document's top-level mapping.
func topLevelKeys(b []byte) map[string]bool {
	keys := map[string]bool{}
	var doc yaml.Node
	if yaml.Unmarshal(b, &doc) != nil || doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return keys
	}
	m := doc.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		keys[m.Content[i].Value] = true
	}
	return keys
}

// AccessKeys are the top-level keys imported into the database once
// (plan 12) and ignored afterwards.
var AccessKeys = []string{"users", "roles", "anonymousRead", "baseURL", "maxUploadMiB"}
