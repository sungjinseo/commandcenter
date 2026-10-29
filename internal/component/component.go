// Package component is the component model shared by the UI and cleanup: a
// package version derived from asset paths or package rows (plan 10,
// Decision 3). It is a leaf package so format packages can implement
// component deletion without import cycles.
package component

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"commandcenter/internal/semver"
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
	case c.Version == "": // a raw file
		return c.Name
	}
	return c.Name + "@" + c.Version
}

// DeriveMaven maps a Maven asset path g1/g2/artifact/version/file to its
// component. Metadata files and paths without a group are not components.
// An sbt plugin's files in sbt's legacy layout, named without the cross
// suffix of their artifact directory, belong to that directory's component.
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
	if name == "" || version == "" || (!mavenFileMatches(file, name, version) && !sbtLegacyFile(file, name, version)) {
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

// SbtPluginDir matches an artifact directory with sbt's _<scala>_<sbt>
// plugin cross suffix, such as sbt-assembly_2.12_1.0. Its submatch is the
// name without the suffix, which names the files of sbt's legacy layout there.
var SbtPluginDir = regexp.MustCompile(`^(.+)_\d+(?:\.\d+)*_\d+(?:\.\d+)*$`)

// sbtLegacyFile reports whether file, in artifact directory name, is named
// as sbt's legacy layout names a plugin file: mavenFileMatches with the name
// without the cross suffix.
func sbtLegacyFile(file, name, version string) bool {
	m := SbtPluginDir.FindStringSubmatch(name)
	return m != nil && mavenFileMatches(file, m[1], version)
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
// proxies record no package rows, so their cached files are read the same
// way. A raw file is a component without a version.
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
		file, _ := strings.CutSuffix(segs[len(segs)-1], ".metadata") // the PEP 658 sidecar goes with its file
		if _, _, known := pypiSplitExt(file); !known {
			return "", "", "", false
		}
		name = segs[1]
		return "", name, PyPIDisplayVersion(pypiNormalize(name), file), true
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
	case "raw": // every file is a component of its own, named by its path
		return "", p, "", p != ""
	case "helm":
		name, version, ok = SplitHelm(p)
		return "", name, version, ok
	case "yum":
		name, version, ok = SplitRPM(p)
		return "", name, version, ok
	case "nuget":
		name, version, ok = SplitNuGet(p)
		return "", name, version, ok
	case "apt": // pool/<component>/<prefix>/<package>/<package>_<version>_<arch>.deb: the architecture is the group
		return SplitDeb(p)
	case "composer":
		name, version, ok = SplitComposer(p)
		return "", name, version, ok
	case "conda": // <subdir>/<name>-<version>-<build>.tar.bz2 or .conda: the subdir is the group
		return SplitConda(p)
	case "rubygems": // gems/<name>-<version>[-<platform>].gem, hosted and proxy alike
		rest, cut := strings.CutPrefix(p, "gems/")
		file, isGem := strings.CutSuffix(rest, ".gem")
		if !cut || !isGem || strings.Contains(file, "/") {
			return "", "", "", false
		}
		name, version, ok = SplitGem(file)
		return "", name, version, ok
	case "gitlfs": // objects/<aa>/<bb>/<oid>: the object is a component named by its oid, without a version
		name, ok = SplitLFS(p)
		return "", name, "", ok
	}
	return "", "", "", false
}

// SplitLFS maps a Git LFS object path, objects/<aa>/<bb>/<oid> with oid a
// lower-case hex sha256, to the oid.
func SplitLFS(p string) (oid string, ok bool) {
	segs := strings.Split(p, "/")
	if len(segs) != 4 || segs[0] != "objects" || len(segs[3]) != 64 || strings.Trim(segs[3], "0123456789abcdef") != "" {
		return "", false
	}
	oid = segs[3]
	return oid, segs[1] == oid[:2] && segs[2] == oid[2:4]
}

// condaSubdirRE is a conda subdir (platform directory): noarch, linux-64,
// osx-arm64, emscripten-wasm32, ...
var condaSubdirRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// SplitConda splits the path of a conda package as the conda format
// stores it, <subdir>/<name>-<version>-<build>.tar.bz2 or .conda, into
// its component: the subdir is the group, and the version is
// "<version>-<build>", as conda itself names a build. Versions and build
// strings never hold "-", so the name ends at the second-last one; the
// repodata files are not components.
func SplitConda(p string) (subdir, name, version string, ok bool) {
	subdir, file, ok := strings.Cut(p, "/")
	if !ok || !condaSubdirRE.MatchString(subdir) || strings.Contains(file, "/") {
		return "", "", "", false
	}
	stem, cut := strings.CutSuffix(file, ".tar.bz2")
	if !cut {
		stem, cut = strings.CutSuffix(file, ".conda")
	}
	i := strings.LastIndexByte(stem, '-')
	if !cut || i <= 0 {
		return "", "", "", false
	}
	j := strings.LastIndexByte(stem[:i], '-')
	if j <= 0 || j+1 == i || stem[0] == '.' {
		return "", "", "", false
	}
	return subdir, stem[:j], stem[j+1:], true
}

// SplitRPM splits an RPM path, <dir>/<name>-<version>-<release>.<arch>.rpm,
// into the component the yum format stores: the name qualified by its
// directory ("el9/foo", "foo" at the root) and version-release.arch
// ("1.0-1.el9.x86_64"). Version and release never hold "-", so the name
// ends at the second-last one. Files under repodata/ are not components.
func SplitRPM(p string) (name, version string, ok bool) {
	stem, cut := strings.CutSuffix(p, ".rpm")
	if !cut || strings.HasPrefix(p, "repodata/") || strings.Contains(p, "/repodata/") {
		return "", "", false
	}
	i := strings.LastIndexByte(stem, '-')
	if i <= 0 || !strings.Contains(stem[i:], ".") {
		return "", "", false
	}
	j := strings.LastIndexByte(stem[:i], '-')
	if j <= 0 || j+1 == i || strings.HasSuffix(stem[:j], "/") || stem[j+1:i] == "." {
		return "", "", false
	}
	if base := path.Base(stem[:j]); base == "" || base == "." || base == ".." {
		return "", "", false
	}
	return stem[:j], stem[j+1:], true
}

// SplitDeb splits the path of a .deb in the pool as the apt format stores
// it (and as Debian names them): the file <package>_<version>_<arch>.deb
// under pool/<component>/<prefix>/<package>/, the version's ":" written
// "%3a" as dpkg does. The architecture is the component's group.
func SplitDeb(p string) (arch, name, version string, ok bool) {
	segs := strings.Split(p, "/")
	if len(segs) != 5 || segs[0] != "pool" {
		return "", "", "", false
	}
	stem, cut := strings.CutSuffix(segs[4], ".deb")
	parts := strings.Split(stem, "_")
	if !cut || len(parts) != 3 || parts[0] != segs[3] || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[2], parts[0], strings.ReplaceAll(parts[1], "%3a", ":"), true
}

// helmNameRE is a chart name as the helm format stores it: it starts a
// `<name>-<version>.tgz` file name, so no "/" and no leading ".".
var helmNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// SplitHelm splits a chart file name, <name>-<version>.tgz, at the last "-"
// whose suffix is a strict semver version: names may hold "-" ("ingress-nginx")
// and so may prerelease versions ("1.0.0-rc.1"). "+" in the version may be
// written "_", as helm package names the file.
func SplitHelm(file string) (name, version string, ok bool) {
	stem, cut := strings.CutSuffix(file, ".tgz")
	if !cut || strings.Contains(stem, "/") {
		return "", "", false
	}
	for i := strings.LastIndexByte(stem, '-'); i > 0; i = strings.LastIndexByte(stem[:i], '-') {
		v := strings.Replace(stem[i+1:], "_", "+", 1)
		if semver.Valid(v) && helmNameRE.MatchString(stem[:i]) {
			return stem[:i], v, true
		}
	}
	return "", "", false
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
	case "docker": // an image's manifest row: version is its digest
		return "", name, version, name + "/manifests/" + version
	case "helm":
		return "", name, version, name + "-" + version + ".tgz"
	case "yum": // the name is directory-qualified (SplitRPM)
		return "", name, version, name + "-" + version + ".rpm"
	case "nuget":
		return "", name, version, name + "/" + version + "/" + name + "." + version + ".nupkg"
	case "composer": // the name is vendor/package, the version normalized
		return "", name, version, "dist/" + name + "/" + version + ".zip"
	case "conda": // a row's version is the file's path, <subdir>/<file> (SplitConda)
		if subdir, cname, cversion, ok := SplitConda(version); ok && cname == name {
			return subdir, cname, cversion, version
		}
		return "", name, version, ""
	case "rubygems": // the version holds the platform (1.0.0-java), as the file name does
		return "", name, version, "gems/" + name + "-" + version + ".gem"
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

// SplitNuGet maps a NuGet file path, <id>/<version>/<id>.<version>.nupkg
// or .nuspec (under v3-flatcontainer/ in a proxy), to its component. The
// id and version are lower-case in paths, so the nuspec and nupkg of one
// version agree.
func SplitNuGet(p string) (name, version string, ok bool) {
	p = strings.TrimPrefix(p, "v3-flatcontainer/")
	segs := strings.Split(p, "/")
	if len(segs) != 3 || segs[0] == "" || segs[1] == "" {
		return "", "", false
	}
	stem, cut := strings.CutSuffix(segs[2], ".nupkg")
	if !cut {
		stem, cut = strings.CutSuffix(segs[2], ".nuspec")
	}
	if !cut || stem != segs[0]+"."+segs[1] {
		return "", "", false
	}
	return segs[0], segs[1], true
}

// SplitComposer maps a Composer archive path to its component, named
// vendor/package: dist/<vendor>/<package>/<version>.zip in a hosted
// repository, dist/<vendor>/<package>/<version>/<sha1>.zip in a proxy's
// cache (the sha1 of the upstream URL; a branch version may hold "/").
// The metadata files under p2/ are not components.
func SplitComposer(p string) (name, version string, ok bool) {
	segs := strings.Split(p, "/")
	if len(segs) < 4 || segs[0] != "dist" || segs[1] == "" || segs[2] == "" {
		return "", "", false
	}
	name = segs[1] + "/" + segs[2]
	stem, cut := strings.CutSuffix(segs[len(segs)-1], ".zip")
	if !cut || stem == "" {
		return "", "", false
	}
	if len(segs) == 4 {
		return name, stem, true
	}
	if len(stem) != 40 || strings.Trim(stem, "0123456789abcdef") != "" {
		return "", "", false
	}
	version = strings.Join(segs[3:len(segs)-1], "/")
	return name, version, version != ""
}

// Gem versions as RubyGems normalizes them (Gem::Version turns "-" into
// ".pre.", so a version never holds "-"), and platforms, which start with a
// letter (java, x86_64-linux, arm64-darwin-22).
var (
	gemVersionRE  = regexp.MustCompile(`^[0-9]+(\.[0-9A-Za-z]+)*$`)
	gemPlatformRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)
)

// SplitGem splits a gem's full name, <name>-<version>[-<platform>], at the
// first "-" followed by a digit whose remainder is a version and an optional
// platform, as RubyGems does: names may hold "-" ("rack-test", "ruby-2d").
// The version returned keeps the platform ("1.0.0-java"): it is what the
// rubygems format stores as the row version.
func SplitGem(full string) (name, version string, ok bool) {
	for i := 1; i+1 < len(full); i++ {
		if full[i] != '-' || full[i+1] < '0' || full[i+1] > '9' {
			continue
		}
		v, platform, _ := strings.Cut(full[i+1:], "-")
		if gemVersionRE.MatchString(v) && (platform == "" || gemPlatformRE.MatchString(platform)) {
			return full[:i], full[i+1:], true
		}
	}
	return "", "", false
}
