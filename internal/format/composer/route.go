// Package composer implements Composer v2 repositories (what Composer 2.x
// reads) under /repository/<repo>/: the root packages.json, the per-package
// metadata p2/<vendor>/<package>.json (and ~dev.json), dist archives, and
// search.json. A hosted repository takes zips by PUT, reads composer.json
// out of them and derives every document from its rows; a proxy caches an
// upstream Composer repository (Packagist, Satis, Private Packagist) with
// the documents' URLs rewritten to itself and the dist archives fetched on
// demand; a group answers from the first member that has the package.
package composer

import (
	"net/http"
	"regexp"
	"strings"

	"commandcenter/internal/repo"
)

// Kind is the kind of repository request a path addresses.
type Kind int

const (
	KindRoot     Kind = iota // packages.json
	KindMetadata             // p2/<vendor>/<package>.json or ~dev.json
	KindDist                 // dist/<vendor>/<package>/<version>.zip (hosted) or .../<version>/<sha1>.zip (proxy)
	KindSearch               // search.json?q=&type=
	KindUpload               // packages/upload/<vendor>/<package>/<version>: PUT a zip, DELETE the version
)

// Route is a parsed request. Name is vendor/package, lower-cased. Version is
// as given for an upload, the normalized version in a dist path. Ref is the
// sha1 of the upstream dist URL a proxy's dist path names.
type Route struct {
	Kind    Kind
	Name    string
	Version string
	Dev     bool
	Ref     string
}

const (
	p2Prefix    = "p2/"
	distPrefix  = "dist/"
	uploadPath  = "packages/upload/"
	upstreamKey = "upstream/packages.json" // a proxy's cached copy of the remote root
	ctJSON      = "application/json"
	ctZip       = "application/zip"
)

var (
	// segRE is a vendor or package name, as Composer spells them (lower-cased here).
	segRE = regexp.MustCompile(`^[A-Za-z0-9]([_.-]?[A-Za-z0-9]+)*$`)
	// verSegRE is one path segment of a version (branch names may hold "/").
	verSegRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+~-]*$`)
	sha1RE   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func ParseRoute(path string) (Route, error) {
	notFound := repo.Errorf(http.StatusNotFound, "not found: %s", path)
	switch path {
	case "packages.json":
		return Route{Kind: KindRoot}, nil
	case "search.json":
		return Route{Kind: KindSearch}, nil
	}
	name := func(vendor, pkg string) (string, bool) {
		if !segRE.MatchString(vendor) || !segRE.MatchString(pkg) {
			return "", false
		}
		return strings.ToLower(vendor + "/" + pkg), true
	}
	if rest, ok := strings.CutPrefix(path, p2Prefix); ok {
		stem, ok := strings.CutSuffix(rest, ".json")
		vendor, pkg, found := strings.Cut(stem, "/")
		if !ok || !found {
			return Route{}, notFound
		}
		pkg, dev := strings.CutSuffix(pkg, "~dev")
		n, ok := name(vendor, pkg)
		if !ok {
			return Route{}, notFound
		}
		return Route{Kind: KindMetadata, Name: n, Dev: dev}, nil
	}
	if rest, ok := strings.CutPrefix(path, distPrefix); ok {
		segs := strings.Split(rest, "/")
		if len(segs) < 3 {
			return Route{}, notFound
		}
		n, ok := name(segs[0], segs[1])
		if !ok {
			return Route{}, notFound
		}
		rt := Route{Kind: KindDist, Name: n}
		file, cut := strings.CutSuffix(segs[len(segs)-1], ".zip")
		verSegs := segs[2 : len(segs)-1]
		if cut && len(verSegs) > 0 && sha1RE.MatchString(file) {
			rt.Ref = file
		} else if cut && len(segs) == 3 {
			verSegs = []string{file}
		} else {
			return Route{}, notFound
		}
		for _, s := range verSegs {
			if !verSegRE.MatchString(s) || s == ".." {
				return Route{}, notFound
			}
		}
		rt.Version = strings.Join(verSegs, "/")
		return rt, nil
	}
	if rest, ok := strings.CutPrefix(path, uploadPath); ok {
		segs := strings.Split(rest, "/")
		if len(segs) != 3 {
			return Route{}, notFound
		}
		n, ok := name(segs[0], segs[1])
		if !ok || !verSegRE.MatchString(segs[2]) {
			return Route{}, notFound
		}
		return Route{Kind: KindUpload, Name: n, Version: segs[2]}, nil
	}
	return Route{}, notFound
}

// metadataPath is where a package's metadata file is: p2/<name>.json or
// p2/<name>~dev.json, the request path and a proxy's cache key alike.
func metadataPath(name string, dev bool) string {
	if dev {
		return p2Prefix + name + "~dev.json"
	}
	return p2Prefix + name + ".json"
}

// distPath is where a hosted repository stores a version's archive.
func distPath(name, version string) string { return distPrefix + name + "/" + version + ".zip" }
