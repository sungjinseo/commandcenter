// Package conan implements Conan remotes (the Conan 2 "v2" API with
// revisions, and the v1 API subset that Conan 1.x clients without revisions
// use) under /repository/<repo>/. A hosted repository stores every file at
// the path its v2 URL has, so the request path is the asset path:
// v2/conans/<name>/<version>/<user>/<channel>/revisions/<rrev>/files/<file>
// for recipe files and .../revisions/<rrev>/packages/<pkgid>/revisions/
// <prev>/files/<file> for binaries, and keeps one packages row per recipe
// revision as its search index; a proxy caches the same paths from an
// upstream Conan server (Conan Center); a group answers from the first
// member that has the recipe. The v1 API is a view over the latest
// revisions, with download and upload URLs pointing at the v2 files.
package conan

import (
	"net/http"
	"regexp"
	"strings"

	"commandcenter/internal/repo"
)

// Kind is the kind of request a path addresses. The v1 recipe and package
// routes share kinds with v2 where they name the same thing (Route.V1
// tells them apart).
type Kind int

const (
	KindPing              Kind = iota // v1/ping
	KindAuthenticate                  // v1/users/authenticate
	KindCheckCredentials              // v1/users/check_credentials
	KindSearch                        // v2/conans/search?q=, v1/conans/search?q=
	KindRecipe                        // v2/conans/<ref>: DELETE; v1: GET the snapshot, DELETE
	KindLatest                        // v2/conans/<ref>/latest
	KindRevisions                     // v2/conans/<ref>/revisions
	KindRevision                      // v2/conans/<ref>/revisions/<rrev>: DELETE
	KindFiles                         // .../revisions/<rrev>/files
	KindFile                          // .../revisions/<rrev>/files/<file>: GET, PUT
	KindPkgSearch                     // .../revisions/<rrev>/search; v1/conans/<ref>/search
	KindPackages                      // .../revisions/<rrev>/packages: DELETE
	KindPackage                       // .../packages/<pkgid>: DELETE; v1 .../packages/<pkgid>: GET the snapshot
	KindPkgLatest                     // .../packages/<pkgid>/latest
	KindPkgRevisions                  // .../packages/<pkgid>/revisions
	KindPkgRevision                   // .../packages/<pkgid>/revisions/<prev>: DELETE
	KindPkgFiles                      // .../revisions/<prev>/files
	KindPkgFile                       // .../revisions/<prev>/files/<file>: GET, PUT
	KindV1Digest                      // v1/conans/<ref>/digest
	KindV1DownloadURLs                // v1/conans/<ref>/download_urls
	KindV1UploadURLs                  // v1/conans/<ref>/upload_urls: POST
	KindV1PkgDigest                   // v1/conans/<ref>/packages/<pkgid>/digest
	KindV1PkgDownloadURLs             // .../packages/<pkgid>/download_urls
	KindV1PkgUploadURLs               // .../packages/<pkgid>/upload_urls: POST
)

// Ref is a recipe reference. User and Channel are "_" when absent, as the
// URLs spell them.
type Ref struct{ Name, Version, User, Channel string }

// String is the reference as Conan prints it: name/version, or
// name/version@user/channel.
func (r Ref) String() string {
	if r.User == "_" {
		return r.Name + "/" + r.Version
	}
	return r.Name + "/" + r.Version + "@" + r.User + "/" + r.Channel
}

// Group is the component group of the reference: user/channel, or "".
func (r Ref) Group() string {
	if r.User == "_" {
		return ""
	}
	return r.User + "/" + r.Channel
}

// dir is the reference's v2 path prefix, v2/conans/<name>/<version>/<user>/<channel>/.
func (r Ref) dir() string {
	return v2Prefix + r.Name + "/" + r.Version + "/" + r.User + "/" + r.Channel + "/"
}

// ParseRef parses a reference as String prints it (an empty user or
// channel reads as "_").
func ParseRef(s string) (Ref, bool) {
	nv, uc, hasUC := strings.Cut(s, "@")
	name, version, ok := strings.Cut(nv, "/")
	r := Ref{Name: name, Version: version, User: "_", Channel: "_"}
	if hasUC {
		r.User, r.Channel, _ = strings.Cut(uc, "/")
	}
	if !ok || !segRE.MatchString(name) || !segRE.MatchString(version) || !segRE.MatchString(r.User) || !segRE.MatchString(r.Channel) {
		return Ref{}, false
	}
	return r, true
}

// Route is a parsed request.
type Route struct {
	Kind              Kind
	V1                bool // a v1 API request
	Ref               Ref
	RRev, PkgID, PRev string
	File              string // the path under files/ (metadata files are nested)
}

const (
	v2Prefix     = "v2/conans/"
	searchPrefix = "search/" // a proxy's cached search answers
	ctJSON       = "application/json"
	ctText       = "text/plain; charset=utf-8"
	// Capabilities is what v1/ping advertises: Conan 2 needs revisions.
	Capabilities = "complex_search,checksum_deploy,revisions,matrix_params"
)

var (
	// segRE is a name, version, user or channel (also "_").
	segRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_+.-]{0,100}$`)
	// revRE is a recipe or package revision, or a package id.
	revRE = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	// fileRE is one segment of a file path under files/.
	fileRE = regexp.MustCompile(`^[A-Za-z0-9_.-][A-Za-z0-9._+-]{0,200}$`)
)

func notFound(path string) error { return repo.Errorf(http.StatusNotFound, "not found: %s", path) }

// ParseRoute parses the path after /repository/<repo>/. Matrix parameters
// (";key=value", which Conan may add after a file name) are dropped.
func ParseRoute(path string) (Route, error) {
	switch path {
	case "v1/ping":
		return Route{Kind: KindPing, V1: true}, nil
	case "v1/users/authenticate":
		return Route{Kind: KindAuthenticate, V1: true}, nil
	case "v1/users/check_credentials":
		return Route{Kind: KindCheckCredentials, V1: true}, nil
	case "v1/conans/search":
		return Route{Kind: KindSearch, V1: true}, nil
	case "v2/conans/search":
		return Route{Kind: KindSearch}, nil
	}
	segs := strings.Split(path, "/")
	if len(segs) < 6 || (segs[0] != "v1" && segs[0] != "v2") || segs[1] != "conans" {
		return Route{}, notFound(path)
	}
	rt := Route{V1: segs[0] == "v1", Ref: Ref{segs[2], segs[3], segs[4], segs[5]}}
	for _, s := range segs[2:6] {
		if !segRE.MatchString(s) {
			return Route{}, notFound(path)
		}
	}
	rest := segs[6:]
	file := func(segs []string) bool {
		if len(segs) == 0 {
			return false
		}
		segs[len(segs)-1], _, _ = strings.Cut(segs[len(segs)-1], ";")
		for _, s := range segs {
			if !fileRE.MatchString(s) || s == "." || s == ".." {
				return false
			}
		}
		rt.File = strings.Join(segs, "/")
		return true
	}
	if rt.V1 {
		switch {
		case len(rest) == 0:
			rt.Kind = KindRecipe
		case len(rest) == 1 && rest[0] == "digest":
			rt.Kind = KindV1Digest
		case len(rest) == 1 && rest[0] == "download_urls":
			rt.Kind = KindV1DownloadURLs
		case len(rest) == 1 && rest[0] == "upload_urls":
			rt.Kind = KindV1UploadURLs
		case len(rest) == 1 && rest[0] == "search":
			rt.Kind = KindPkgSearch
		case len(rest) >= 2 && rest[0] == "packages" && revRE.MatchString(rest[1]):
			rt.PkgID = rest[1]
			switch {
			case len(rest) == 2:
				rt.Kind = KindPackage
			case len(rest) == 3 && rest[2] == "digest":
				rt.Kind = KindV1PkgDigest
			case len(rest) == 3 && rest[2] == "download_urls":
				rt.Kind = KindV1PkgDownloadURLs
			case len(rest) == 3 && rest[2] == "upload_urls":
				rt.Kind = KindV1PkgUploadURLs
			default:
				return Route{}, notFound(path)
			}
		default:
			return Route{}, notFound(path)
		}
		return rt, nil
	}
	switch {
	case len(rest) == 0:
		rt.Kind = KindRecipe
	case len(rest) == 1 && rest[0] == "latest":
		rt.Kind = KindLatest
	case len(rest) == 1 && rest[0] == "revisions":
		rt.Kind = KindRevisions
	case len(rest) >= 2 && rest[0] == "revisions" && revRE.MatchString(rest[1]):
		rt.RRev = rest[1]
		rest = rest[2:]
		switch {
		case len(rest) == 0:
			rt.Kind = KindRevision
		case len(rest) == 1 && rest[0] == "files":
			rt.Kind = KindFiles
		case len(rest) >= 2 && rest[0] == "files" && file(rest[1:]):
			rt.Kind = KindFile
		case len(rest) == 1 && rest[0] == "search":
			rt.Kind = KindPkgSearch
		case len(rest) == 1 && rest[0] == "packages":
			rt.Kind = KindPackages
		case len(rest) >= 2 && rest[0] == "packages" && revRE.MatchString(rest[1]):
			rt.PkgID = rest[1]
			rest = rest[2:]
			switch {
			case len(rest) == 0:
				rt.Kind = KindPackage
			case len(rest) == 1 && rest[0] == "latest":
				rt.Kind = KindPkgLatest
			case len(rest) == 1 && rest[0] == "revisions":
				rt.Kind = KindPkgRevisions
			case len(rest) >= 2 && rest[0] == "revisions" && revRE.MatchString(rest[1]):
				rt.PRev = rest[1]
				rest = rest[2:]
				switch {
				case len(rest) == 0:
					rt.Kind = KindPkgRevision
				case len(rest) == 1 && rest[0] == "files":
					rt.Kind = KindPkgFiles
				case len(rest) >= 2 && rest[0] == "files" && file(rest[1:]):
					rt.Kind = KindPkgFile
				default:
					return Route{}, notFound(path)
				}
			default:
				return Route{}, notFound(path)
			}
		default:
			return Route{}, notFound(path)
		}
	default:
		return Route{}, notFound(path)
	}
	return rt, nil
}

// revDir is where a recipe revision's files and packages are.
func revDir(ref Ref, rrev string) string { return ref.dir() + "revisions/" + rrev + "/" }

// pkgDir is where a package id's revisions are.
func pkgDir(ref Ref, rrev, pkgid string) string { return revDir(ref, rrev) + "packages/" + pkgid + "/" }

// prevDir is where a package revision's files are.
func prevDir(ref Ref, rrev, pkgid, prev string) string {
	return pkgDir(ref, rrev, pkgid) + "revisions/" + prev + "/"
}

// v2Path is the v2 request path of a route (and a hosted asset's path, for
// the file kinds), for the v1 views and the proxy's cache keys.
func (rt Route) v2Path() string {
	switch rt.Kind {
	case KindLatest:
		return rt.Ref.dir() + "latest"
	case KindRevisions:
		return rt.Ref.dir() + "revisions"
	case KindFiles:
		return revDir(rt.Ref, rt.RRev) + "files"
	case KindFile:
		return revDir(rt.Ref, rt.RRev) + "files/" + rt.File
	case KindPkgSearch:
		return revDir(rt.Ref, rt.RRev) + "search"
	case KindPkgLatest:
		return pkgDir(rt.Ref, rt.RRev, rt.PkgID) + "latest"
	case KindPkgRevisions:
		return pkgDir(rt.Ref, rt.RRev, rt.PkgID) + "revisions"
	case KindPkgFiles:
		return prevDir(rt.Ref, rt.RRev, rt.PkgID, rt.PRev) + "files"
	case KindPkgFile:
		return prevDir(rt.Ref, rt.RRev, rt.PkgID, rt.PRev) + "files/" + rt.File
	}
	return ""
}

// allow lists the methods a route accepts (writes only when the repository
// takes them), for a 405's Allow header.
func allow(rt Route, writes bool) string {
	read, write := "GET, HEAD", ""
	switch rt.Kind {
	case KindFile, KindPkgFile:
		write = "PUT"
	case KindRecipe:
		if !rt.V1 {
			read = ""
		}
		write = "DELETE"
	case KindRevision, KindPackages, KindPkgRevision:
		read, write = "", "DELETE"
	case KindPackage:
		if !rt.V1 {
			read, write = "", "DELETE"
		}
	case KindV1UploadURLs, KindV1PkgUploadURLs:
		read, write = "", "POST"
	}
	if !writes {
		write = ""
	}
	switch {
	case read == "":
		return write
	case write == "":
		return read
	}
	return read + ", " + write
}

// RecipeFile is the path of a recipe revision's file (the web UI reads
// conanfile.py there for a component's metadata).
func RecipeFile(ref Ref, rrev, file string) string { return revDir(ref, rrev) + "files/" + file }
