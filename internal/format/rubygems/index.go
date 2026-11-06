package rubygems

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
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
	pkg               meta.Package
}

func newRow(p meta.Package) row {
	r := row{pkg: p}
	r.version, r.platform = splitRowVersion(p.Version)
	return r
}

// attrs reads the row's attrs when they are needed, one row at a time: all of
// a gem with 1500 versions, per request, took a 128 MiB server down at 32
// clients at once. A row whose attrs cannot be read still lists with its name
// and version.
func (r row) attrs() gemAttrs {
	var a gemAttrs
	if json.Unmarshal([]byte(r.pkg.Attrs), &a) != nil || a.Name == "" {
		a = gemAttrs{Name: r.pkg.Name, Version: r.version, Dependencies: []gemDep{}}
		if r.platform != "ruby" {
			a.Platform = r.platform
		}
	}
	return a
}

// rows lists the versions of gem name in hosted repoName in the order the
// compact index keeps them.
func rows(ctx context.Context, db lister, repoName, name string) ([]row, error) {
	pkgs, err := db.ListPackages(ctx, repoName, name)
	if err != nil {
		return nil, err
	}
	return sortRows(pkgs), nil
}

// sortRows is the compact index's order: by creation, then version.
func sortRows(pkgs []meta.Package) []row {
	out := make([]row, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, newRow(p))
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
	return out
}

// infoDoc is info/<gem>: "---" then one line per version,
// `version[-platform] dep:req,...|checksum:sha256,ruby:req,rubygems:req`,
// with the runtime dependencies in name order.
func infoDoc(rs []row) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	for _, r := range rs {
		writeInfoLine(&b, r)
	}
	return []byte(b.String())
}

func writeInfoLine(b *strings.Builder, r row) {
	a := r.attrs()
	b.WriteString(r.pkg.Version)
	b.WriteByte(' ')
	var deps []string
	for _, d := range a.Dependencies {
		if d.Type != "development" {
			deps = append(deps, d.String())
		}
	}
	b.WriteString(strings.Join(deps, ","))
	b.WriteString("|checksum:")
	b.WriteString(a.SHA256)
	if a.RequiredRubyVersion != "" {
		b.WriteString(",ruby:" + requirementKey(a.RequiredRubyVersion))
	}
	if a.RequiredRubygemsVersion != "" {
		b.WriteString(",rubygems:" + requirementKey(a.RequiredRubygemsVersion))
	}
	b.WriteByte('\n')
}

// hostedInfo is infoDoc of gem name for a request, reading a batch of rows at a
// time: Bundler asks for it on every install, and holding the rows of a gem
// with 1500 versions came close to a 128 MiB server's limit at 64 clients. Nil
// when the gem has no version.
func hostedInfo(ctx context.Context, db *meta.DB, repoName, name string) ([]byte, error) {
	pkgs, err := db.ListPackageVersions(ctx, repoName, name)
	if err != nil || len(pkgs) == 0 {
		return nil, err
	}
	rs := sortRows(pkgs)
	versions := make([]string, 0, len(rs))
	for _, r := range rs {
		versions = append(versions, r.pkg.Version)
	}
	var b strings.Builder
	b.WriteString("---\n")
	err = db.EachPackage(ctx, repoName, name, versions, func(p meta.Package) error {
		writeInfoLine(&b, newRow(p))
		return nil
	})
	return []byte(b.String()), err
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

// maxVersionsLine bounds one line of a member's versions file (a gem with
// its every version; rubygems.org's longest is some 350 KiB).
const maxVersionsLine = 16 << 20

// mergeVersions appends a member's versions file to a group's (dst): its
// lines after the header, except those of the gems in owned, which an
// earlier member has. The first member's header becomes the group's, so the
// merge does not change while the members' files do not. Unless the member
// is the last one, the gems it has a version of (one left once its "-"
// lines are applied) join owned and hide the later members' lines for them,
// so the group's info/<gem> (the owner's) and its versions agree. The member
// is read line by line. Only the versions of a gem with a removed version
// are kept: a first pass finds those, which spares a rubygems.org-sized
// member's 1.8 million versions (some 170 MiB) when it is not the last.
func mergeVersions(dst *bytes.Buffer, member io.ReadSeeker, owned map[string]bool, first, last bool) error {
	removals := map[string]bool{}
	if !last {
		err := scanVersions(member, nil, func(_, name, versions []byte) {
			if versions[0] == '-' || bytes.Contains(versions, []byte(",-")) {
				removals[string(name)] = true
			}
		})
		if err != nil {
			return err
		}
		if _, err := member.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	header := func(h []byte) {
		if first {
			dst.Write(h)
		}
	}
	has := map[string]bool{}             // gems without a removal
	left := map[string]map[string]bool{} // the versions left of the others
	err := scanVersions(member, header, func(line, name, versions []byte) {
		if !owned[string(name)] {
			dst.Write(line)
			dst.WriteByte('\n')
		}
		switch {
		case last:
		case !removals[string(name)]:
			has[string(name)] = true
		default:
			set := left[string(name)]
			if set == nil {
				set = map[string]bool{}
				left[string(name)] = set
			}
			for _, v := range bytes.Split(versions, []byte(",")) {
				if gone, cut := bytes.CutPrefix(v, []byte("-")); cut {
					delete(set, string(gone))
				} else if len(v) > 0 {
					set[string(v)] = true
				}
			}
		}
	})
	for name := range has {
		owned[name] = true
	}
	for name, set := range left {
		if len(set) > 0 {
			owned[name] = true
		}
	}
	return err
}

// scanVersions reads a versions file line by line: header, if given, gets what
// comes before the lines (its header up to "---", or "---" when it has none),
// fn each line with a gem and versions. A line's bytes are valid during the call.
func scanVersions(r io.Reader, header func([]byte), fn func(line, name, versions []byte)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	// The header ends at a "---" line, which is at the top when there is one.
	head, _ := br.Peek(1024)
	n := 0
	if bytes.HasPrefix(head, []byte("---\n")) {
		n = 4
	} else if i := bytes.Index(head, []byte("\n---\n")); i >= 0 {
		n = i + 5
	}
	if header != nil {
		if n > 0 {
			header(head[:n])
		} else {
			header([]byte("---\n"))
		}
	}
	br.Discard(n)
	sc := bufio.NewScanner(br)
	sc.Buffer(nil, maxVersionsLine)
	for sc.Scan() {
		line := sc.Bytes()
		name, rest, ok := bytes.Cut(line, []byte(" "))
		if !ok || len(name) == 0 {
			continue
		}
		versions, _, _ := bytes.Cut(rest, []byte(" "))
		if len(versions) > 0 {
			fn(line, name, versions)
		}
	}
	return sc.Err()
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
	for _, d := range r.attrs().Dependencies {
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
