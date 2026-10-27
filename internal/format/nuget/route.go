// Package nuget implements NuGet V3 feeds (what dotnet restore, nuget.exe
// and Visual Studio use) under /repository/<repo>/: the service index
// (index.json), the flat container (v3-flatcontainer/), registration pages
// (registration/), search (query) and the push API (api/v2/package). A
// hosted repository takes packages by PUT, reads the .nuspec out of the
// .nupkg and derives every document from its rows; a proxy caches an
// upstream V3 feed (api.nuget.org or another) with the documents' URLs
// rewritten to itself; a group answers from the first member that has the
// package id and pushes to its writeMember.
package nuget

import (
	"net/http"
	"regexp"
	"strings"

	"commandcenter/internal/repo"
)

// Kind is the kind of repository request a path addresses.
type Kind int

const (
	KindIndex        Kind = iota // index.json, the service index
	KindFlatIndex                // v3-flatcontainer/<id>/index.json
	KindFlatFile                 // v3-flatcontainer/<id>/<version>/<id>.<version>.nupkg or .nuspec
	KindRegistration             // registration/<id>/index.json, <version>.json, or an upstream page (proxy)
	KindSearch                   // query?q=&skip=&take=&prerelease=
	KindPublish                  // api/v2/package (PUT) and api/v2/package/<id>/<version> (DELETE)
	KindSymbols                  // api/v2/symbolpackage: refused
)

// Route is a parsed request. ID and Version are lower-cased (the version
// normalized when it parses); File is the flat container file name and
// Rest the path under registration/<id>/.
type Route struct {
	Kind              Kind
	ID, Version, File string
	Rest              string
}

const (
	flatPrefix  = "v3-flatcontainer/"
	regPrefix   = "registration/"
	upstreamKey = "upstream/index.json" // a proxy's cached copy of the remote service index
	ctJSON      = "application/json"
	ctNupkg     = "application/octet-stream"
	ctNuspec    = "application/xml"
)

// idRE is a package id as NuGet accepts it: letters, digits, "_", "." and
// "-", at most 100 characters.
var idRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,99}$`)

// restRE is what may follow registration/<id>/: index.json, <version>.json,
// or a page path an upstream splits its registration into.
var restRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+(/[A-Za-z0-9._+-]+)*\.json$`)

func ParseRoute(path string) (Route, error) {
	notFound := repo.Errorf(http.StatusNotFound, "not found: %s", path)
	switch {
	case path == "index.json":
		return Route{Kind: KindIndex}, nil
	case path == "query":
		return Route{Kind: KindSearch}, nil
	case path == "api/v2/package":
		return Route{Kind: KindPublish}, nil
	case path == "api/v2/symbolpackage":
		return Route{Kind: KindSymbols}, nil
	}
	if rest, ok := strings.CutPrefix(path, "api/v2/package/"); ok {
		id, ver, found := strings.Cut(rest, "/")
		if !found || !idRE.MatchString(id) || strings.Contains(ver, "/") {
			return Route{}, notFound
		}
		v, ok := parseVersion(ver)
		if !ok {
			return Route{}, repo.Errorf(http.StatusBadRequest, "invalid version %q", ver)
		}
		return Route{Kind: KindPublish, ID: strings.ToLower(id), Version: v.key()}, nil
	}
	if rest, ok := strings.CutPrefix(path, flatPrefix); ok {
		segs := strings.Split(rest, "/")
		if !idRE.MatchString(segs[0]) {
			return Route{}, notFound
		}
		id := strings.ToLower(segs[0])
		switch {
		case len(segs) == 2 && segs[1] == "index.json":
			return Route{Kind: KindFlatIndex, ID: id}, nil
		case len(segs) == 3:
			v, ok := parseVersion(segs[1])
			if !ok {
				return Route{}, notFound
			}
			file := strings.ToLower(segs[2])
			if file != NupkgFile(id, v.key()) && file != NuspecFile(id, v.key()) {
				return Route{}, notFound
			}
			return Route{Kind: KindFlatFile, ID: id, Version: v.key(), File: file}, nil
		}
		return Route{}, notFound
	}
	if rest, ok := strings.CutPrefix(path, regPrefix); ok {
		id, sub, found := strings.Cut(rest, "/")
		if !found || !idRE.MatchString(id) || !restRE.MatchString(sub) || strings.Contains(sub, "..") {
			return Route{}, notFound
		}
		rt := Route{Kind: KindRegistration, ID: strings.ToLower(id), Rest: sub}
		if leaf, ok := strings.CutSuffix(sub, ".json"); ok && !strings.Contains(leaf, "/") && leaf != "index" {
			if v, ok := parseVersion(leaf); ok {
				rt.Version, rt.Rest = v.key(), v.key()+".json"
			}
		}
		return rt, nil
	}
	return Route{}, notFound
}

// NupkgFile is the file name of a package version (lower-case id and
// normalized version), and NuspecFile its manifest's.
func NupkgFile(id, version string) string  { return id + "." + version + ".nupkg" }
func NuspecFile(id, version string) string { return id + "." + version + ".nuspec" }

// assetPath is where a hosted repository stores a package version's file.
func assetPath(id, version, file string) string { return id + "/" + version + "/" + file }
