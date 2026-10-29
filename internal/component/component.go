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
	case "r": // <contrib dir>/<pkg>_<version>.tar.gz (.zip, .tgz): the contrib dir is the group
		return SplitR(p)
	case "conan": // v2/conans/<name>/<version>/<user>/<channel>/revisions/<rrev>/...: user/channel is the group
		return SplitConan(p)
	case "p2": // [.remote/<sha1>/]{features,plugins}/<id>_<version>.jar, binary/<id>_<version>: the directory is the group
		return SplitP2(p)
	case "swift": // <scope>/<name>/<version>/<file>: every file of the version directory (archive, manifests, a proxy's release.json)
		name, version, ok = SplitSwift(p)
		return "", name, version, ok
	case "cocoapods": // Specs/<a>/<b>/<c>/<Pod>/<version>/<file>: the podspec and the archives cached beside it
		name, version, ok = SplitPod(p)
		return "", name, version, ok
	case "huggingface": // [datasets/|spaces/]<repo id>/resolve/<commit sha>/<file>: the kind is the group
		return SplitHF(p)
	}
	return "", "", "", false
}

// SplitP2 maps a p2 artifact path to its component: features/<id>_<version>.jar,
// plugins/<id>_<version>.jar or binary/<id>_<version>, each possibly under
// a proxy's .remote/<sha1>/ mirror of a composite child. The version
// starts at the first "_" followed by a digit, as an id may hold "_"
// (a.b_c_1.0.0.v20240901). The directory is the group.
func SplitP2(p string) (group, name, version string, ok bool) {
	if rest, cut := strings.CutPrefix(p, ".remote/"); cut {
		if _, rest, cut = strings.Cut(rest, "/"); !cut {
			return "", "", "", false
		}
		p = rest
	}
	dir, file, cut := strings.Cut(p, "/")
	if !cut || strings.Contains(file, "/") {
		return "", "", "", false
	}
	switch dir {
	case "features", "plugins":
		if file, cut = strings.CutSuffix(file, ".jar"); !cut {
			return "", "", "", false
		}
	case "binary":
	default:
		return "", "", "", false
	}
	for i := 1; i+1 < len(file); i++ {
		if file[i] == '_' && file[i+1] >= '0' && file[i+1] <= '9' {
			return dir, file[:i], file[i+1:], true
		}
	}
	return "", "", "", false
}

// swiftScopeRE, swiftNameRE and swiftVersionRE mirror the swift format's
// identifier and semver rules (parity_test.go keeps them in step).
var (
	swiftScopeRE   = regexp.MustCompile(`^[a-z0-9](-?[a-z0-9])*$`)    // at most 39 bytes
	swiftNameRE    = regexp.MustCompile(`^[a-z0-9]([-_]?[a-z0-9])*$`) // at most 100 bytes
	swiftVersionRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
)

// SplitSwift maps a Swift registry asset path, <scope>/<name>/<version>/
// <file> with the identifier lower-cased, to its package scope.name and
// version.
func SplitSwift(p string) (name, version string, ok bool) {
	segs := strings.Split(p, "/")
	if len(segs) != 4 || segs[3] == "" || len(segs[0]) > 39 || len(segs[1]) > 100 ||
		!swiftScopeRE.MatchString(segs[0]) || !swiftNameRE.MatchString(segs[1]) || !swiftVersionRE.MatchString(segs[2]) {
		return "", "", false
	}
	return segs[0] + "." + segs[1], segs[2], true
}

// SplitPod maps a path of a CocoaPods proxy's cache,
// Specs/<a>/<b>/<c>/<Pod>/<version>/<file> (the trunk CDN's layout, the
// shard letters being the first three of the md5 of the pod name), to its
// component. The file is <Pod>.podspec.json or an archive fetched for the
// podspec's source.http (<sha1 of its URL>.<ext>). The index files at the
// root are not components.
func SplitPod(p string) (name, version string, ok bool) {
	segs := strings.Split(p, "/")
	if len(segs) != 7 || segs[0] != "Specs" || segs[4] == "" || segs[5] == "" || segs[6] == "" {
		return "", "", false
	}
	for _, s := range segs[1:4] {
		if len(s) != 1 || strings.Trim(s, "0123456789abcdef") != "" {
			return "", "", false
		}
	}
	return segs[4], segs[5], true
}

// SplitHF maps a path of a Hugging Face proxy's cache,
// [datasets/|spaces/]<repo id>/resolve/<commit sha>/<file>, to its
// component: the repository id (org/name, or a bare name) at the commit,
// with the kind (datasets, spaces; "" for models) as the group. The api/
// documents and branch names are not components.
func SplitHF(p string) (kind, repoID, sha string, ok bool) {
	if strings.HasPrefix(p, "api/") {
		return "", "", "", false
	}
	head, rest, found := strings.Cut(p, "/resolve/")
	sha, file, hasFile := strings.Cut(rest, "/")
	if !found || !hasFile || file == "" || len(sha) != 40 || strings.Trim(sha, "0123456789abcdef") != "" {
		return "", "", "", false
	}
	for _, k := range []string{"datasets/", "spaces/"} {
		if id, cut := strings.CutPrefix(head, k); cut {
			kind, head = strings.TrimSuffix(k, "/"), id
			break
		}
	}
	if n := strings.Count(head, "/"); head == "" || n > 1 || strings.HasPrefix(head, "/") || strings.HasSuffix(head, "/") {
		return "", "", "", false
	}
	return kind, head, sha, true
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

// R repositories (CRAN layout). rContribRE is a contrib directory, the
// unit an R client reads PACKAGES from: src/contrib for sources,
// bin/windows/contrib/<R.x> and bin/macosx[/<build>]/contrib/<R.x> for
// binaries. rArchiveRE is CRAN's src/contrib/Archive/<pkg>, old sources a
// proxy passes through.
var (
	rContribRE = regexp.MustCompile(`^(src/contrib|bin/windows/contrib/[0-9]+\.[0-9]+|bin/macosx(/[a-z0-9_.-]+)?/contrib/[0-9]+\.[0-9]+)$`)
	rArchiveRE = regexp.MustCompile(`^src/contrib/Archive/[A-Za-z][A-Za-z0-9.]*$`)
	rNameRE    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.]*[A-Za-z0-9]$`) // "Writing R Extensions" 1.1.1
	rVersionRE = regexp.MustCompile(`^[0-9]+([.-][0-9]+)+$`)
)

// RContribDir reports whether dir is a contrib directory of an R
// repository (one with its own PACKAGES files).
func RContribDir(dir string) bool { return rContribRE.MatchString(dir) }

// RExt is the archive extension of the packages in dir: .zip under
// bin/windows, .tgz under bin/macosx, .tar.gz for sources.
func RExt(dir string) string {
	switch {
	case strings.HasPrefix(dir, "bin/windows/"):
		return ".zip"
	case strings.HasPrefix(dir, "bin/"):
		return ".tgz"
	}
	return ".tar.gz"
}

// SplitR splits the path of an R package, <dir>/<pkg>_<version><ext>
// where dir is a contrib directory (RContribDir) or an Archive directory
// and ext the directory's (RExt), into its component: the directory is
// the group, so a package built for several R versions is one component
// per directory. Package names never hold "_". PACKAGES files are not
// components.
func SplitR(p string) (dir, name, version string, ok bool) {
	i := strings.LastIndexByte(p, '/')
	if i <= 0 {
		return "", "", "", false
	}
	dir, file := p[:i], p[i+1:]
	if !rContribRE.MatchString(dir) && !rArchiveRE.MatchString(dir) {
		return "", "", "", false
	}
	stem, cut := strings.CutSuffix(file, RExt(dir))
	if !cut {
		return "", "", "", false
	}
	name, version, ok = strings.Cut(stem, "_")
	if !ok || !rNameRE.MatchString(name) || !rVersionRE.MatchString(version) {
		return "", "", "", false
	}
	return dir, name, version, true
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
	case "r": // the name is directory-qualified, <contrib dir>/<pkg>; the directory is the group
		if i := strings.LastIndexByte(name, '/'); i > 0 {
			return name[:i], name[i+1:], version, name + "_" + version + RExt(name[:i])
		}
		return "", name, version, ""
	case "conan": // a row per recipe revision: name/version[@user/channel], version = rrev; its file is the manifest
		if group, cname, cversion, ok := SplitConan(ConanRowPath(name, version) + "files/conanmanifest.txt"); ok {
			return group, cname, cversion, ConanRowPath(name, version) + "files/conanmanifest.txt"
		}
		return "", name, version, ""
	case "swift": // the name is scope.name (lower-cased); the archive is <scope>/<name>/<version>/source-archive.zip
		if scope, pkg, ok := strings.Cut(name, "."); ok {
			return "", name, version, scope + "/" + pkg + "/" + version + "/source-archive.zip"
		}
		return "", name, version, ""
	}
	return "", name, version, ""
}

// SplitConan maps a Conan asset path to its component: the recipe
// reference's user/channel as the group ("" without one), the name, and
// <version>#<rrev>. The paths are the v2 API's: v2/conans/<name>/<version>/
// <user>/<channel>/revisions/<rrev>/files/<file> for a recipe file and
// .../revisions/<rrev>/packages/<pkgid>/revisions/<prev>/files/<file> for
// a binary's; the cached metadata documents (latest, revisions, files,
// search) and a proxy's search/ answers are not components.
func SplitConan(p string) (group, name, version string, ok bool) {
	segs := strings.Split(p, "/")
	if len(segs) < 9 || segs[0] != "v2" || segs[1] != "conans" || segs[6] != "revisions" {
		return "", "", "", false
	}
	for _, s := range segs[2:8] {
		if s == "" {
			return "", "", "", false
		}
	}
	switch {
	case segs[8] == "files" && len(segs) >= 10:
	case segs[8] == "packages" && len(segs) >= 14 && segs[10] == "revisions" && segs[12] == "files" && segs[9] != "" && segs[11] != "":
	default:
		return "", "", "", false
	}
	if segs[4] != "_" {
		group = segs[4] + "/" + segs[5]
	}
	return group, segs[2], segs[3] + "#" + segs[7], true
}

// ConanRevisionDir is the directory of the recipe revision a Conan asset
// path belongs to (cleanup's unit prefix).
func ConanRevisionDir(p string) (string, bool) {
	if _, _, _, ok := SplitConan(p); !ok {
		return "", false
	}
	segs := strings.Split(p, "/")
	return strings.Join(segs[:8], "/") + "/", true
}

// ConanRowPath is the recipe revision directory of a conan packages row
// (name = name/version[@user/channel], version = the revision).
func ConanRowPath(name, rrev string) string {
	nv, uc, _ := strings.Cut(name, "@")
	user, channel, ok := strings.Cut(uc, "/")
	if !ok {
		user, channel = "_", "_"
	}
	return "v2/conans/" + nv + "/" + user + "/" + channel + "/revisions/" + rrev + "/"
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
