package rubygems

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"commandcenter/internal/component"
	"commandcenter/internal/repo"
)

// maxMetadata bounds a gem's metadata.gz once inflated (a specification
// with a long file list is a few hundred KiB).
const maxMetadata = 8 << 20

func badGem(format string, args ...any) error {
	return repo.Errorf(http.StatusBadRequest, "gem: "+format, args...)
}

// gemAttrs is the packages.attrs document of a hosted version: the
// Gem::Specification fields the indexes, the gemspec and the UI's metadata
// use, read from the .gem's metadata.gz.
type gemAttrs struct {
	Name                    string            `json:"name"`
	Version                 string            `json:"version"`            // without the platform
	Platform                string            `json:"platform,omitempty"` // "" for ruby
	Authors                 []string          `json:"authors"`
	Email                   []string          `json:"email,omitempty"`
	Summary                 string            `json:"summary"`
	Description             string            `json:"description,omitempty"`
	Homepage                string            `json:"homepage,omitempty"`
	Licenses                []string          `json:"licenses"`
	RequiredRubyVersion     string            `json:"required_ruby_version,omitempty"`
	RequiredRubygemsVersion string            `json:"required_rubygems_version,omitempty"`
	Dependencies            []gemDep          `json:"dependencies"`
	Metadata                map[string]string `json:"metadata"`
	Date                    string            `json:"date,omitempty"` // YYYY-MM-DD
	RubygemsVersion         string            `json:"rubygems_version,omitempty"`
	SHA256                  string            `json:"sha256"`
	Size                    int64             `json:"size"`
}

type gemDep struct {
	Name        string `json:"name"`
	Requirement string `json:"requirement"` // Gem::Requirement#to_s: ">= 1.0, < 2"
	Type        string `json:"type"`        // runtime or development
}

// rowVersion is what the packages row stores as the version: the version
// with the platform, when there is one ("1.0.0-java").
func (a gemAttrs) rowVersion() string {
	if a.Platform == "" {
		return a.Version
	}
	return a.Version + "-" + a.Platform
}

// gemspecYAML is the part of a YAML Gem::Specification the format reads.
// Psych writes Gem::Version and Gem::Requirement as tagged mappings, which
// decode as plain ones.
type gemspecYAML struct {
	Name                           string `yaml:"name"`
	Version                        struct{ Version string }
	Platform                       string
	Authors                        []string
	Email                          any // a string or a list
	Date                           string
	Summary, Description, Homepage string
	Licenses                       []string
	Metadata                       map[string]string
	RequiredRubyVersion            requirementYAML `yaml:"required_ruby_version"`
	RequiredRubygemsVersion        requirementYAML `yaml:"required_rubygems_version"`
	RubygemsVersion                string          `yaml:"rubygems_version"`
	Dependencies                   []struct {
		Name        string
		Requirement requirementYAML
		Type        string
	}
}

// requirementYAML is a Gem::Requirement: pairs of [operator, Gem::Version].
type requirementYAML struct {
	Requirements [][]yaml.Node
}

// String is Gem::Requirement#to_s ("" for none, which is ">= 0").
func (r requirementYAML) String() string {
	var parts []string
	for _, pair := range r.Requirements {
		if len(pair) != 2 {
			continue
		}
		var v struct{ Version string }
		if pair[1].Decode(&v) != nil || v.Version == "" {
			continue
		}
		parts = append(parts, pair[0].Value+" "+v.Version)
	}
	return strings.Join(parts, ", ")
}

// readGem reads the specification out of a .gem: a tar holding metadata.gz
// (the YAML Gem::Specification), data.tar.gz and checksums.yaml.gz. Only
// the metadata is read; the archive is streamed past.
func readGem(r io.Reader) (gemAttrs, error) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return gemAttrs{}, badGem("no metadata.gz in the gem")
		}
		if err != nil {
			return gemAttrs{}, badGem("not a tar (.gem): %v", err)
		}
		if hdr.Name != "metadata.gz" {
			continue
		}
		gz, err := gzip.NewReader(tr)
		if err != nil {
			return gemAttrs{}, badGem("metadata.gz: %v", err)
		}
		raw, err := io.ReadAll(io.LimitReader(gz, maxMetadata+1))
		if err != nil {
			return gemAttrs{}, badGem("metadata.gz: %v", err)
		}
		if len(raw) > maxMetadata {
			return gemAttrs{}, badGem("metadata.gz is larger than %d MiB", maxMetadata>>20)
		}
		return parseSpec(raw)
	}
}

// parseSpec reads a YAML Gem::Specification and checks its name, version
// and platform.
func parseSpec(raw []byte) (gemAttrs, error) {
	var s gemspecYAML
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return gemAttrs{}, badGem("metadata is not a Gem::Specification: %v", err)
	}
	a := gemAttrs{
		Name: strings.TrimSpace(s.Name), Version: strings.ReplaceAll(strings.TrimSpace(s.Version.Version), "-", ".pre."),
		Authors: compact(s.Authors), Summary: strings.TrimSpace(s.Summary), Description: strings.TrimSpace(s.Description),
		Homepage: strings.TrimSpace(s.Homepage), Licenses: compact(s.Licenses), Metadata: s.Metadata,
		RequiredRubyVersion: s.RequiredRubyVersion.String(), RequiredRubygemsVersion: s.RequiredRubygemsVersion.String(),
		RubygemsVersion: s.RubygemsVersion, Dependencies: []gemDep{},
	}
	if a.Metadata == nil {
		a.Metadata = map[string]string{}
	}
	if !nameRE.MatchString(a.Name) {
		return gemAttrs{}, badGem("invalid gem name %q", s.Name)
	}
	if !versionRE.MatchString(a.Version) {
		return gemAttrs{}, badGem("invalid version %q", s.Version.Version)
	}
	if p := strings.TrimSpace(s.Platform); p != "" && p != "ruby" {
		if !platformRE.MatchString(p) {
			return gemAttrs{}, badGem("invalid platform %q", s.Platform)
		}
		a.Platform = p
	}
	if name, version, ok := component.SplitGem(fullName(a.Name, a.rowVersion())); !ok || name != a.Name || version != a.rowVersion() {
		return gemAttrs{}, badGem("the file name %s.gem does not split back into %s %s", fullName(a.Name, a.rowVersion()), a.Name, a.rowVersion())
	}
	if len(s.Date) >= 10 {
		a.Date = s.Date[:10]
	}
	switch e := s.Email.(type) {
	case string:
		a.Email = compact([]string{e})
	case []any:
		for _, x := range e {
			if str, ok := x.(string); ok {
				a.Email = append(a.Email, str)
			}
		}
		a.Email = compact(a.Email)
	}
	for _, d := range s.Dependencies {
		name := strings.TrimSpace(d.Name)
		if !nameRE.MatchString(name) {
			return gemAttrs{}, badGem("invalid dependency name %q", d.Name)
		}
		typ := strings.TrimPrefix(strings.TrimSpace(d.Type), ":")
		if typ != "development" {
			typ = "runtime"
		}
		a.Dependencies = append(a.Dependencies, gemDep{Name: name, Requirement: d.Requirement.String(), Type: typ})
	}
	sort.SliceStable(a.Dependencies, func(i, j int) bool { return a.Dependencies[i].Name < a.Dependencies[j].Name })
	return a, nil
}

// compact trims the strings and drops empty ones, never returning nil.
func compact(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// requirementKey is a requirement in the compact index's form: its parts
// sorted and joined with "&" (">= 1.0&< 2"), ">= 0" when empty.
func requirementKey(s string) string {
	var parts []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return ">= 0"
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func (d gemDep) String() string { return fmt.Sprintf("%s:%s", d.Name, requirementKey(d.Requirement)) }
