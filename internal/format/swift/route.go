// Package swift implements Swift package registries (SE-0292, what `swift
// package-registry` and SwiftPM 5.7+ use) under /repository/<repo>/: the
// release list GET {scope}/{name}, the release metadata GET
// {scope}/{name}/{version}, the manifests GET
// {scope}/{name}/{version}/Package.swift[?swift-version=], the source
// archive GET {scope}/{name}/{version}.zip and the lookup GET
// identifiers?url=. A hosted repository takes `swift package-registry
// publish` (PUT {scope}/{name}/{version}, multipart) and derives every
// document from its rows; a proxy caches another registry with the release
// URLs rewritten to itself; a group answers from the first member that has
// the package. POST login answers 200 to a signed-in caller, which is how
// `swift package-registry login` tests credentials. Every response carries
// Content-Version: 1 and errors are application/problem+json.
package swift

import (
	"net/http"
	"regexp"
	"strings"
)

// Kind is the kind of registry request a path addresses.
type Kind int

const (
	KindList        Kind = iota // {scope}/{name}[.json]: the releases
	KindRelease                 // {scope}/{name}/{version}[.json]: the release metadata; PUT publishes
	KindManifest                // {scope}/{name}/{version}/Package.swift[?swift-version=]
	KindArchive                 // {scope}/{name}/{version}.zip
	KindIdentifiers             // identifiers?url=
	KindLogin                   // POST login: `swift package-registry login` checks its credentials
)

// Route is a parsed request. Scope and Name are as the client spelled them
// (the registry matches them case-insensitively).
type Route struct {
	Kind    Kind
	Scope   string
	Name    string
	Version string
}

const (
	ctJSON    = "application/json"
	ctProblem = "application/problem+json"
	ctZip     = "application/zip"
	ctSwift   = "text/x-swift"
	// ctRegistry is the vendor media type prefix of the registry API.
	ctRegistry   = "application/vnd.swift.registry.v1"
	archiveFile  = "source-archive.zip"
	releaseFile  = "release.json"  // a proxy's cached release metadata
	releasesFile = "releases.json" // a proxy's cached release list
	manifestFile = "Package.swift"
)

var (
	// scopeRE and nameRE are the identifier parts the specification allows
	// (3.6): alphanumerics with single inner hyphens, and underscores in
	// names too; validScope and validName add the length bounds.
	scopeRE = regexp.MustCompile(`^[A-Za-z0-9](-?[A-Za-z0-9])*$`)
	nameRE  = regexp.MustCompile(`^[A-Za-z0-9]([-_]?[A-Za-z0-9])*$`)
	// versionRE is semver 2.0.0 (no "v": the specification forbids it).
	versionRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	// variantRE is a version-specific manifest, Package@swift-5.9.swift.
	variantRE = regexp.MustCompile(`^Package@swift-([0-9]+(\.[0-9]+){0,2})\.swift$`)
)

// ValidVersion reports whether v is a semver 2 version as the registry
// takes them.
func ValidVersion(v string) bool { return versionRE.MatchString(v) }

func validScope(s string) bool { return len(s) <= 39 && scopeRE.MatchString(s) }
func validName(s string) bool  { return len(s) <= 100 && nameRE.MatchString(s) }

func ParseRoute(path string) (Route, error) {
	notFound := problem(http.StatusNotFound, "not found: %s", path)
	switch path {
	case "identifiers":
		return Route{Kind: KindIdentifiers}, nil
	case "login":
		return Route{Kind: KindLogin}, nil
	}
	segs := strings.Split(path, "/")
	if len(segs) < 2 || len(segs) > 4 || !validScope(segs[0]) {
		return Route{}, notFound
	}
	rt := Route{Scope: segs[0]}
	switch len(segs) {
	case 2:
		rt.Kind, rt.Name = KindList, strings.TrimSuffix(segs[1], ".json")
	case 3:
		rt.Name = segs[1]
		switch v := segs[2]; {
		case strings.HasSuffix(v, ".zip"):
			rt.Kind, rt.Version = KindArchive, strings.TrimSuffix(v, ".zip")
		default:
			rt.Kind, rt.Version = KindRelease, strings.TrimSuffix(v, ".json")
		}
	case 4:
		if segs[3] != manifestFile {
			return Route{}, notFound
		}
		rt.Kind, rt.Name, rt.Version = KindManifest, segs[1], segs[2]
	}
	if !validName(rt.Name) || (rt.Kind != KindList && !ValidVersion(rt.Version)) {
		return Route{}, notFound
	}
	return rt, nil
}

// ID is the package identifier the rows and components use: scope.name,
// lower-cased (the registry matches identifiers case-insensitively).
func ID(scope, name string) string { return strings.ToLower(scope + "." + name) }

// SplitID splits a stored identifier into its scope and name.
func SplitID(id string) (scope, name string, ok bool) {
	scope, name, ok = strings.Cut(id, ".")
	return scope, name, ok && validScope(scope) && validName(name)
}

// versionDir is where a version's files live, hosted and proxy alike:
// <scope>/<name>/<version>/ with the identifier lower-cased.
func versionDir(scope, name, version string) string {
	return strings.ToLower(scope+"/"+name) + "/" + version + "/"
}

// archivePath is where a version's source archive is stored.
func archivePath(scope, name, version string) string {
	return versionDir(scope, name, version) + archiveFile
}
