package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
	"time"
	"unicode/utf8"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

// Policy is a cleanup policy (plan 11 Decision 5): which components of the
// repositories that use it are deleted by the cleanup task.
type Policy struct {
	Name   string
	Format string // "all" or one of the five formats
	Criteria
	Created, Updated time.Time // set when read from the database
}

// Criteria select components; every set criterion must hold (AND), and at
// least one of the two ages must be set.
type Criteria struct {
	// ComponentAgeDays (0 = off): the component's newest asset was created
	// more than N days ago.
	ComponentAgeDays int `json:"componentAgeDays"`
	// LastDownloadedDays (0 = off): no asset of the component was used in
	// the last N days, where an asset never downloaded counts from its
	// creation.
	LastDownloadedDays int `json:"lastDownloadedDays"`
	// ReleaseType is any (or ""), release or prerelease (see Prerelease).
	ReleaseType string `json:"releaseType"`
	// NamePattern is an optional RE2 regular expression that must match the
	// whole of "group:name" ("name" for components without a group).
	NamePattern string `json:"namePattern"`
}

const (
	// MaxPatternLen bounds NamePattern, in characters.
	MaxPatternLen = 200
	// maxDays bounds the ages (100 years).
	maxDays = 36500
)

var (
	policyFormats = map[string]bool{"all": true, "maven": true, "npm": true, "go": true, "pypi": true, "cargo": true, "raw": true, "helm": true, "yum": true, "nuget": true, "apt": true}
	releaseTypes  = map[string]bool{"any": true, "release": true, "prerelease": true}
)

// Validate checks p and fills the default release type ("any"). Problems are
// reported as one *repo.ValidationError listing all of them.
func (p *Policy) Validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if err := repo.CheckName("policy", p.Name); err != nil {
		var ve *repo.ValidationError
		if errors.As(err, &ve) {
			errs = append(errs, ve.Errors...)
		}
	}
	if !policyFormats[p.Format] {
		add("unknown format %q: use all, maven, npm, go, pypi, cargo, raw, helm, yum, nuget or apt", p.Format)
	}
	for _, d := range []struct {
		field string
		n     int
	}{{"componentAgeDays", p.ComponentAgeDays}, {"lastDownloadedDays", p.LastDownloadedDays}} {
		if d.n < 0 || d.n > maxDays {
			add("%s must be between 0 and %d", d.field, maxDays)
		}
	}
	if p.ComponentAgeDays <= 0 && p.LastDownloadedDays <= 0 {
		add("set componentAgeDays or lastDownloadedDays (or both)")
	}
	if p.ReleaseType == "" {
		p.ReleaseType = "any"
	}
	if !releaseTypes[p.ReleaseType] {
		add("unknown releaseType %q: use any, release or prerelease", p.ReleaseType)
	}
	if _, err := compilePattern(p.NamePattern); err != nil {
		add("namePattern: %v", err)
	}
	if len(errs) > 0 {
		return &repo.ValidationError{Errors: errs}
	}
	return nil
}

// compilePattern compiles a name pattern anchored to the whole string; ""
// gives nil (no pattern).
func compilePattern(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(pattern) > MaxPatternLen {
		return nil, fmt.Errorf("at most %d characters", MaxPatternLen)
	}
	// Check alone first, so the error names the user's text, not the anchors.
	if _, err := syntax.Parse(pattern, syntax.Perl); err != nil {
		return nil, err
	}
	return regexp.Compile(`^(?:` + pattern + `)$`)
}

// FormatMatches reports whether a policy of policyFormat may be used by a
// repository of repoFormat.
func FormatMatches(policyFormat, repoFormat string) bool {
	return policyFormat == "all" || policyFormat == repoFormat
}

// Prerelease reports whether a component version is a prerelease: a Maven
// SNAPSHOT, a PEP 440 pre- or development release for PyPI, a semver
// prerelease otherwise (Go pseudo-versions included). Versions that do not
// parse count as releases.
func Prerelease(format, version string) bool {
	switch format {
	case "maven":
		return strings.HasSuffix(version, "-SNAPSHOT")
	case "pypi":
		return pypiPrerelease(version)
	}
	v, ok := semver.Parse(version)
	return ok && !v.Release()
}

var pypiReleaseRE = regexp.MustCompile(`^[vV]?(?:[0-9]+!)?[0-9]+(?:\.[0-9]+)*`)

// pypiPreLabels are the PEP 440 labels of pre- and development releases
// (post-release labels such as post, rev and r are releases).
var pypiPreLabels = map[string]bool{"a": true, "alpha": true, "b": true, "beta": true, "c": true,
	"rc": true, "pre": true, "preview": true, "dev": true}

func pypiPrerelease(version string) bool {
	public, _, _ := strings.Cut(version, "+") // the local label is not part of the release
	rel := pypiReleaseRE.FindString(public)
	if rel == "" {
		return false
	}
	for _, label := range strings.FieldsFunc(strings.ToLower(public[len(rel):]), func(r rune) bool { return r < 'a' || r > 'z' }) {
		if pypiPreLabels[label] {
			return true
		}
	}
	return false
}

// policyFromMeta decodes a stored policy.
func policyFromMeta(m meta.CleanupPolicy) (Policy, error) {
	p := Policy{Name: m.Name, Format: m.Format, Created: m.Created, Updated: m.Updated}
	if err := json.Unmarshal([]byte(m.Config), &p.Criteria); err != nil {
		return Policy{}, fmt.Errorf("cleanup policy %q: decode stored config: %w", m.Name, err)
	}
	if p.ReleaseType == "" {
		p.ReleaseType = "any"
	}
	return p, nil
}

func policyToMeta(p Policy) (meta.CleanupPolicy, error) {
	js, err := json.Marshal(p.Criteria)
	if err != nil {
		return meta.CleanupPolicy{}, err
	}
	return meta.CleanupPolicy{Name: p.Name, Format: p.Format, Config: string(js)}, nil
}

// GetPolicy reads the named policy; meta.ErrNotFound if there is none.
func GetPolicy(ctx context.Context, db *meta.DB, name string) (Policy, error) {
	m, err := db.GetCleanupPolicy(ctx, name)
	if err != nil {
		return Policy{}, err
	}
	return policyFromMeta(m)
}

// ListPolicies reads every policy, ordered by name.
func ListPolicies(ctx context.Context, db *meta.DB) ([]Policy, error) {
	list, err := db.ListCleanupPolicies(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Policy, 0, len(list))
	for _, m := range list {
		p, err := policyFromMeta(m)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// CreatePolicy validates and stores a new policy; meta.ErrExists if the name
// is taken.
func CreatePolicy(ctx context.Context, db *meta.DB, p Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	m, err := policyToMeta(p)
	if err != nil {
		return err
	}
	return db.CreateCleanupPolicy(ctx, m)
}

// UpdatePolicy validates p and replaces the stored policy of that name;
// meta.ErrNotFound if there is none.
func UpdatePolicy(ctx context.Context, db *meta.DB, p Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	m, err := policyToMeta(p)
	if err != nil {
		return err
	}
	return db.UpdateCleanupPolicy(ctx, m)
}

// PolicyLookup returns the registry's policy hook (repo.Registry.Policies):
// the format of the named policy, ok false if there is none.
func PolicyLookup(db *meta.DB) func(ctx context.Context, name string) (string, bool, error) {
	return func(ctx context.Context, name string) (string, bool, error) {
		m, err := db.GetCleanupPolicy(ctx, name)
		switch {
		case errors.Is(err, meta.ErrNotFound):
			return "", false, nil
		case err != nil:
			return "", false, err
		}
		return m.Format, true, nil
	}
}
