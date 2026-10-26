// Package component is the component model shared by the UI and cleanup: a
// package version derived from asset paths or package rows (plan 10,
// Decision 3). It is a leaf package so format packages can implement
// component deletion without import cycles.
package component

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Component is a derived package version.
type Component struct {
	Repo, Format, Group, Name, Version string
	LastUpdated                        time.Time
	Assets                             []Asset
	// Evaluated is set by cleanup evaluation: the deleters then delete the
	// component only if it is still what was evaluated (cleanup.CheckUnchanged).
	Evaluated *Evaluated
}

// Evaluated is what cleanup evaluation saw of a component: the paths of its
// assets, their newest creation and modification times and their newest
// download (zero if never downloaded).
type Evaluated struct {
	Paths                   map[string]bool
	MaxCreated, MaxModified time.Time
	LastDownloaded          time.Time
}

// ErrChanged is returned by a deleter that skipped a component because it
// changed since cleanup evaluated it: an asset was added, replaced or
// downloaded. Nothing was deleted.
var ErrChanged = errors.New("the component changed since it was evaluated")

// Asset is one file of a component.
type Asset struct {
	Path string
	Size int64
	// Downloads and LastDownloaded are set by the UI's component search only.
	Downloads      int64
	LastDownloaded time.Time
}

// ErrNotSupported is returned by a deleter for repositories whose components
// cannot be deleted (groups, or a repository of another format or type).
var ErrNotSupported = errors.New("component deletion is not supported for this repository")

// Result is what one component delete removed: only rows actually deleted
// count. Bytes is the sum of the deleted assets' sizes; blobs are reclaimed
// later by the compact task.
type Result struct {
	Assets int
	Bytes  int64
}

// Add counts one deleted asset of size bytes.
func (r *Result) Add(size int64) {
	r.Assets++
	r.Bytes += size
}

// CheckTarget is the deleters' defensive guard: c must belong to the
// repository it is deleted from (repoName, format). An empty c.Repo is
// accepted; the format must always match.
func (c Component) CheckTarget(repoName, format string) error {
	if c.Repo != "" && c.Repo != repoName {
		return fmt.Errorf("component %s of repository %q cannot be deleted from %q", c.Label(), c.Repo, repoName)
	}
	if c.Format != format {
		return fmt.Errorf("%q component %s cannot be deleted from %s repository %q", c.Format, c.Label(), format, repoName)
	}
	return nil
}

// Label names the component in events and logs: group:name:version for
// Maven, name@version otherwise (npm scopes included).
func (c Component) Label() string {
	switch {
	case c.Format == "maven":
		return c.Group + ":" + c.Name + ":" + c.Version
	case c.Group != "":
		return c.Group + "/" + c.Name + "@" + c.Version
	}
	return c.Name + "@" + c.Version
}

// DeriveMaven maps a Maven asset path g1/g2/artifact/version/file to its
// component. Metadata files and paths without a group are not components.
func DeriveMaven(p string) (group, name, version string, ok bool) {
	segs := strings.Split(p, "/")
	if len(segs) < 4 {
		return "", "", "", false
	}
	file := segs[len(segs)-1]
	if strings.HasPrefix(file, "maven-metadata.xml") {
		return "", "", "", false
	}
	name, version = segs[len(segs)-3], segs[len(segs)-2]
	if name == "" || version == "" || !mavenFileMatches(file, name, version) {
		return "", "", "", false
	}
	for _, s := range segs[:len(segs)-3] {
		if s == "" {
			return "", "", "", false
		}
	}
	return strings.Join(segs[:len(segs)-3], "."), name, version, true
}

// SnapshotStamp is the timestamp-build part of a snapshot file name
// (yyyyMMdd.HHmmss-N; shorter time parts are tolerated).
var SnapshotStamp = regexp.MustCompile("^[0-9]{8}[.][0-9]+-[0-9]+")

// mavenFileMatches reports whether file starts with artifact-version, or for
// a snapshot with artifact-base-stamp, ending at a name boundary.
func mavenFileMatches(file, name, version string) bool {
	boundary := func(rest string) bool { return rest == "" || rest[0] == '-' || rest[0] == '.' }
	rest, ok := strings.CutPrefix(file, name+"-"+version)
	if ok && boundary(rest) {
		return true
	}
	base, snap := strings.CutSuffix(version, "-SNAPSHOT")
	if !snap {
		return false
	}
	rest, ok = strings.CutPrefix(file, name+"-"+base+"-")
	if !ok {
		return false
	}
	stamp := SnapshotStamp.FindString(rest)
	return stamp != "" && boundary(rest[len(stamp):])
}

// DeriveNpm maps a tarball path name/-/name-version.tgz (or the scoped
// form) to its component.
func DeriveNpm(p string) (group, name, version string, ok bool) {
	pkg, file, found := strings.Cut(p, "/-/")
	if !found || pkg == "" || strings.Contains(file, "/") {
		return "", "", "", false
	}
	name = pkg
	if strings.HasPrefix(pkg, "@") {
		g, n, hasName := strings.Cut(pkg, "/")
		if !hasName || n == "" || strings.Contains(n, "/") {
			return "", "", "", false
		}
		group, name = g, n
	} else if strings.Contains(pkg, "/") {
		return "", "", "", false
	}
	v, okp := strings.CutPrefix(file, name+"-")
	v, oks := strings.CutSuffix(v, ".tgz")
	if !okp || !oks || v == "" {
		return "", "", "", false
	}
	return group, name, v, true
}

// DeriveGo maps module/@v/version.(zip|mod|info) to its component. The
// module and version are case-decoded; text that is not validly encoded is
// kept raw.
func DeriveGo(p string) (name, version string, ok bool) {
	mod, file, found := strings.Cut(p, "/@v/")
	if !found || mod == "" || strings.Contains(file, "/") {
		return "", "", false
	}
	for _, ext := range []string{".zip", ".mod", ".info"} {
		if v, cut := strings.CutSuffix(file, ext); cut && v != "" {
			if m, err := goUnescape(mod); err == nil {
				mod = m
			}
			if u, err := goUnescape(v); err == nil {
				v = u
			}
			return mod, v, true
		}
	}
	return "", "", false
}

var pypiExts = []string{".whl", ".tar.gz", ".tar.bz2", ".zip", ".egg"}

// pypiSplitExt strips a known distribution extension from file.
func pypiSplitExt(file string) (base, ext string, ok bool) {
	for _, e := range pypiExts {
		if b, cut := strings.CutSuffix(file, e); cut && b != "" {
			return b, e, true
		}
	}
	return file, "", false
}

// PyPIDisplayVersion extracts the version from a wheel or sdist file name of
// project (a normalized name): the name part is compared in normalized form,
// so spellings like Demo.Pkg or demo_pkg match, and the version is what
// follows (for wheels, up to the next dash). It falls back to a wheel's
// second dash field, then to the whole file name.
func PyPIDisplayVersion(project, file string) string {
	base, ext, _ := pypiSplitExt(file)
	for i := 0; i < len(base)-1; i++ {
		if base[i] == '-' && base[i+1] >= '0' && base[i+1] <= '9' && pypiNormalize(base[:i]) == project {
			v := base[i+1:]
			if ext == ".whl" {
				v, _, _ = strings.Cut(v, "-")
			}
			return v
		}
	}
	if fields := strings.Split(base, "-"); ext == ".whl" && len(fields) >= 2 && fields[1] != "" {
		return fields[1]
	}
	return file
}

// DeriveAsset derives a component from the path of a stored file for
// formats whose components come from the assets table. pypi and cargo
// proxies record no package rows, so their cached files are read the same way.
func DeriveAsset(format, p string) (group, name, version string, ok bool) {
	switch format {
	case "maven":
		return DeriveMaven(p)
	case "npm":
		return DeriveNpm(p)
	case "go":
		name, version, ok = DeriveGo(p)
		return "", name, version, ok
	case "pypi":
		segs := strings.Split(p, "/")
		if segs[0] != "packages" || (len(segs) != 3 && len(segs) != 4) {
			return "", "", "", false
		}
		if _, _, known := pypiSplitExt(segs[len(segs)-1]); !known {
			return "", "", "", false
		}
		name = segs[1]
		return "", name, PyPIDisplayVersion(pypiNormalize(name), segs[len(segs)-1]), true
	case "cargo":
		segs := strings.Split(p, "/")
		if len(segs) != 3 || segs[0] != "crates" {
			return "", "", "", false
		}
		v, cut := strings.CutSuffix(segs[2], ".crate")
		if !cut || v == "" {
			return "", "", "", false
		}
		return "", segs[1], v, true
	}
	return "", "", "", false
}

// PackageComponent maps a packages-table row to a component and the path of
// its file ("" when it cannot be built). PyPI rows hold the file name in
// version and the normalized project in name.
func PackageComponent(format, name, version string) (group, cname, cversion, assetPath string) {
	switch format {
	case "npm":
		group, cname = "", name
		if g, n, ok := strings.Cut(name, "/"); ok && strings.HasPrefix(g, "@") {
			group, cname = g, n
		}
		return group, cname, version, name + "/-/" + cname + "-" + version + ".tgz"
	case "pypi":
		return "", name, PyPIDisplayVersion(name, version), "packages/" + name + "/" + version
	case "cargo":
		return "", name, version, "crates/" + name + "/" + version + ".crate"
	}
	return "", name, version, ""
}

// The two helpers below mirror golang.Unescape and pypi.Normalize, which this
// leaf package cannot import (the format packages import it). parity_test.go
// keeps them in step.

// goUnescape reverses Go's module path case-encoding ("!b" for "B").
func goUnescape(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			return "", errors.New("unescaped upper-case letter")
		}
		if c != '!' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(s) || s[i+1] < 'a' || s[i+1] > 'z' {
			return "", errors.New("invalid escape")
		}
		b.WriteByte(s[i+1] - ('a' - 'A'))
		i++
	}
	return b.String(), nil
}

var pypiSepRE = regexp.MustCompile(`[-_.]+`)

// pypiNormalize is the PEP 503 normalized form of a project name.
func pypiNormalize(name string) string { return strings.ToLower(pypiSepRE.ReplaceAllString(name, "-")) }
