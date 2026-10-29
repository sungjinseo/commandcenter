package rubygems

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/meta"
)

// lister reads package rows: *meta.DB outside a transaction, *meta.Tx inside.
type lister interface {
	ListPackages(ctx context.Context, repo, name string) ([]meta.Package, error)
	ListPackageNames(ctx context.Context, repo string) ([]string, error)
}

// row is one stored version of a hosted gem.
type row struct {
	version, platform string // split from the row's version
	attrs             gemAttrs
	pkg               meta.Package
}

// rows lists the versions of gem name in hosted repoName in the order the
// compact index keeps them: by creation, then version. A row whose attrs
// cannot be read still lists with its name and version.
func rows(ctx context.Context, db lister, repoName, name string) ([]row, error) {
	pkgs, err := db.ListPackages(ctx, repoName, name)
	if err != nil {
		return nil, err
	}
	out := make([]row, 0, len(pkgs))
	for _, p := range pkgs {
		r := row{pkg: p}
		r.version, r.platform = splitRowVersion(p.Version)
		if json.Unmarshal([]byte(p.Attrs), &r.attrs) != nil || r.attrs.Name == "" {
			r.attrs = gemAttrs{Name: p.Name, Version: r.version, Dependencies: []gemDep{}}
			if r.platform != "ruby" {
				r.attrs.Platform = r.platform
			}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.pkg.CreatedAt.Equal(b.pkg.CreatedAt) {
			return a.pkg.CreatedAt.Before(b.pkg.CreatedAt)
		}
		if c := compareVersions(a.version, b.version); c != 0 {
			return c < 0
		}
		return a.pkg.Version < b.pkg.Version
	})
	return out, nil
}

// infoDoc is info/<gem>: "---" then one line per version,
// `version[-platform] dep:req,...|checksum:sha256,ruby:req,rubygems:req`,
// with the runtime dependencies in name order.
func infoDoc(rs []row) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	for _, r := range rs {
		b.WriteString(r.pkg.Version)
		b.WriteByte(' ')
		var deps []string
		for _, d := range r.attrs.Dependencies {
			if d.Type != "development" {
				deps = append(deps, d.String())
			}
		}
		b.WriteString(strings.Join(deps, ","))
		b.WriteString("|checksum:")
		b.WriteString(r.attrs.SHA256)
		if r.attrs.RequiredRubyVersion != "" {
			b.WriteString(",ruby:" + requirementKey(r.attrs.RequiredRubyVersion))
		}
		if r.attrs.RequiredRubygemsVersion != "" {
			b.WriteString(",rubygems:" + requirementKey(r.attrs.RequiredRubygemsVersion))
		}
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func md5hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// versionsLine is one line of the versions file: the gem, its versions
// (a leading "-" marks a removed one) and the md5 of its info file.
func versionsLine(name string, versions []string, info []byte) string {
	return name + " " + strings.Join(versions, ",") + " " + md5hex(info) + "\n"
}

func versionsHeader(now time.Time) string {
	return "created_at: " + now.UTC().Format(time.RFC3339) + "\n---\n"
}

// buildVersions is a whole versions file from the rows of hosted repoName:
// one line per gem, in name order.
func buildVersions(ctx context.Context, db lister, repoName string, now time.Time) ([]byte, error) {
	names, err := db.ListPackageNames(ctx, repoName)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var b bytes.Buffer
	b.WriteString(versionsHeader(now))
	for _, name := range names {
		rs, err := rows(ctx, db, repoName, name)
		if err != nil {
			return nil, err
		}
		if len(rs) == 0 {
			continue
		}
		versions := make([]string, 0, len(rs))
		for _, r := range rs {
			versions = append(versions, r.pkg.Version)
		}
		b.WriteString(versionsLine(name, versions, infoDoc(rs)))
	}
	return b.Bytes(), nil
}

// namesDoc is the names file: "---" then the gems in name order.
func namesDoc(names []string) []byte {
	sort.Strings(names)
	return []byte("---\n" + strings.Join(append(names, ""), "\n"))
}

// parseVersions reads a versions file into its lines after the header,
// and the gems that have a version left once the "-" lines are applied.
func parseVersions(body []byte) (lines []string, live map[string]bool) {
	live = map[string]bool{}
	have := map[string]map[string]bool{}
	text := string(body)
	if i := strings.Index(text, "---\n"); i >= 0 {
		text = text[i+4:]
	}
	for _, line := range strings.Split(text, "\n") {
		name, rest, ok := strings.Cut(line, " ")
		if !ok || name == "" {
			continue
		}
		lines = append(lines, line)
		versions, _, _ := strings.Cut(rest, " ")
		set := have[name]
		if set == nil {
			set = map[string]bool{}
			have[name] = set
		}
		for _, v := range strings.Split(versions, ",") {
			if removed, cut := strings.CutPrefix(v, "-"); cut {
				delete(set, removed)
			} else if v != "" {
				set[v] = true
			}
		}
		live[name] = len(set) > 0
	}
	return lines, live
}

// mergeVersions is a group's versions file: each member's lines in member
// order, a gem that an earlier member has (a version left after its "-"
// lines) hiding the later members' lines for it, so the group's info/<gem>
// (the owner's) and its versions agree.
func mergeVersions(members [][]byte, now time.Time) []byte {
	var b bytes.Buffer
	b.WriteString(versionsHeader(now))
	owned := map[string]bool{}
	for _, body := range members {
		lines, live := parseVersions(body)
		for _, line := range lines {
			if name, _, _ := strings.Cut(line, " "); !owned[name] {
				b.WriteString(line)
				b.WriteByte('\n')
			}
		}
		for name, ok := range live {
			if ok {
				owned[name] = true
			}
		}
	}
	return b.Bytes()
}

// parseNames reads a names file into its gems.
func parseNames(body []byte) []string {
	text := string(body)
	if i := strings.Index(text, "---\n"); i >= 0 {
		text = text[i+4:]
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// specEntry is one [name, version, platform] of a specs index.
type specEntry struct{ name, version, platform string }

// specsDoc is specs.4.8.gz (released versions), latest_specs.4.8.gz (the
// newest released version of each gem and platform) or
// prerelease_specs.4.8.gz: a Marshal array of [name, Gem::Version,
// platform], in name, version, platform order, gzipped.
func specsDoc(file string, entries []specEntry) ([]byte, error) {
	sortSpecs(entries)
	var picked []specEntry
	switch file {
	case "prerelease_specs.4.8.gz":
		for _, e := range entries {
			if prerelease(e.version) {
				picked = append(picked, e)
			}
		}
	case "latest_specs.4.8.gz":
		latest := map[[2]string]int{}
		for _, e := range entries {
			if prerelease(e.version) {
				continue
			}
			k := [2]string{e.name, e.platform}
			if i, ok := latest[k]; ok {
				picked[i] = e // entries are in version order: the last one is the newest
			} else {
				latest[k] = len(picked)
				picked = append(picked, e)
			}
		}
	default:
		for _, e := range entries {
			if !prerelease(e.version) {
				picked = append(picked, e)
			}
		}
	}
	sortSpecs(picked) // the latest ones were picked in first-seen order
	arr := make([]any, 0, len(picked))
	for _, e := range picked {
		arr = append(arr, []any{e.name, gemVersion(e.version), e.platform})
	}
	raw, err := marshalDump(arr)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(raw)
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sortSpecs(entries []specEntry) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.name != b.name {
			return a.name < b.name
		}
		if c := compareVersions(a.version, b.version); c != 0 {
			return c < 0
		}
		return platformLess(a.platform, b.platform)
	})
}

// platformLess orders platforms as RubyGems sorts specs: ruby first.
func platformLess(a, b string) bool {
	if (a == "ruby") != (b == "ruby") {
		return a == "ruby"
	}
	return a < b
}

// dependenciesDoc is api/v1/dependencies: an array of {name, number,
// platform, dependencies: [[gem, requirement], ...]} hashes (symbol keys,
// runtime dependencies), one per version of the gems asked for.
func dependenciesDoc(entries []any) ([]byte, error) { return marshalDump(entries) }

func dependencyEntry(r row) hash {
	var deps []any
	for _, d := range r.attrs.Dependencies {
		if d.Type != "development" {
			req := d.Requirement
			if req == "" {
				req = ">= 0"
			}
			deps = append(deps, []any{d.Name, req})
		}
	}
	if deps == nil {
		deps = []any{}
	}
	return hash{{sym("name"), r.pkg.Name}, {sym("number"), r.version}, {sym("platform"), r.platform}, {sym("dependencies"), deps}}
}
