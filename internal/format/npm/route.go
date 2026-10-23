// Package npm implements the npm registry format.
package npm

import (
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

// Kind is the kind of npm request a path addresses.
type Kind int

const (
	KindPackument Kind = iota // /{pkg}
	KindVersion               // /{pkg}/{version or tag}
	KindTarball               // /{pkg}/-/{file}.tgz
	KindDistTags              // /-/package/{pkg}/dist-tags
	KindDistTag               // /-/package/{pkg}/dist-tags/{tag}
	KindLogin                 // /-/user/org.couchdb.user:{user}
	KindWebLogin              // /-/v1/login
	KindWhoami                // /-/whoami
	KindPing                  // /-/ping
	KindAudit                 // /-/npm/v1/security/...
	KindRev                   // /{pkg}/-rev/{rev}, /{pkg}/-/{file}.tgz/-rev/{rev} (unpublish)
	KindToken                 // /-/user/token/{token} (logout)
)

// Route is a parsed npm request path.
type Route struct {
	Kind    Kind
	Package string // full name, e.g. "@scope/name"
	Version string // KindVersion: a version or a dist-tag
	File    string // KindTarball
	Tag     string // KindDistTag
	User    string // KindLogin
	Token   string // KindToken
}

var nameRE = regexp.MustCompile(`^(?:@[a-zA-Z0-9~-][a-zA-Z0-9._~-]*/)?[a-zA-Z0-9~-][a-zA-Z0-9._~-]*$`)

// ValidName reports whether name is an acceptable npm package name
// (legacy upper-case names are accepted).
func ValidName(name string) bool {
	return len(name) <= 214 && nameRE.MatchString(name) && !strings.Contains(name, "..")
}

// ParseRoute classifies a path relative to /repository/{name}/.
func ParseRoute(path string) (Route, error) {
	notFound := repo.Errorf(http.StatusNotFound, "not found: %s", path)
	switch path {
	case "-/whoami":
		return Route{Kind: KindWhoami}, nil
	case "-/ping":
		return Route{Kind: KindPing}, nil
	case "-/v1/login":
		return Route{Kind: KindWebLogin}, nil
	case "-/npm/v1/security/advisories/bulk", "-/npm/v1/security/audits", "-/npm/v1/security/audits/quick":
		return Route{Kind: KindAudit}, nil
	}
	if user, ok := strings.CutPrefix(path, "-/user/org.couchdb.user:"); ok {
		if user == "" || strings.Contains(user, "/") {
			return Route{}, notFound
		}
		if !validSegment(user) {
			return Route{}, segmentError(user)
		}
		return Route{Kind: KindLogin, User: user}, nil
	}
	if tok, ok := strings.CutPrefix(path, "-/user/token/"); ok {
		// The message never echoes the path: it holds a token.
		if tok == "" || strings.Contains(tok, "/") || !validSegment(tok) {
			return Route{}, repo.Errorf(http.StatusNotFound, "not found")
		}
		return Route{Kind: KindToken, Token: tok}, nil
	}
	if rest, ok := strings.CutPrefix(path, "-/package/"); ok {
		segs := strings.Split(rest, "/")
		n := 1
		if strings.HasPrefix(segs[0], "@") {
			n = 2
		}
		if len(segs) < n+1 || segs[n] != "dist-tags" {
			return Route{}, notFound
		}
		name := strings.Join(segs[:n], "/")
		if !ValidName(name) {
			return Route{}, invalidName(name)
		}
		after := segs[n+1:]
		switch len(after) {
		case 0:
			return Route{Kind: KindDistTags, Package: name}, nil
		case 1:
			if after[0] == "" {
				return Route{}, notFound
			}
			if !validSegment(after[0]) {
				return Route{}, segmentError(after[0])
			}
			return Route{Kind: KindDistTag, Package: name, Tag: after[0]}, nil
		}
		return Route{}, notFound
	}
	if strings.HasPrefix(path, "-/") {
		return Route{}, notFound
	}

	segs := strings.Split(path, "/")
	n := 1
	if strings.HasPrefix(segs[0], "@") {
		n = 2
	}
	if len(segs) < n {
		return Route{}, notFound
	}
	name := strings.Join(segs[:n], "/")
	if !ValidName(name) {
		return Route{}, invalidName(name)
	}
	rest := segs[n:]
	switch {
	case len(rest) == 0:
		return Route{Kind: KindPackument, Package: name}, nil
	case len(rest) == 1 && rest[0] != "" && rest[0] != "-":
		if !validSegment(rest[0]) {
			return Route{}, segmentError(rest[0])
		}
		return Route{Kind: KindVersion, Package: name, Version: rest[0]}, nil
	case len(rest) == 2 && rest[0] == "-" && strings.HasSuffix(rest[1], ".tgz"):
		if !validSegment(rest[1]) {
			return Route{}, segmentError(rest[1])
		}
		return Route{Kind: KindTarball, Package: name, File: rest[1]}, nil
	case len(rest) == 2 && rest[0] == "-rev":
		// npm unpublish: PUT/DELETE {pkg}/-rev/{rev}.
		return Route{Kind: KindRev, Package: name}, nil
	case len(rest) == 4 && rest[0] == "-" && strings.HasSuffix(rest[1], ".tgz") && rest[2] == "-rev":
		// npm unpublish of one version: DELETE {pkg}/-/{file}.tgz/-rev/{rev}.
		if !validSegment(rest[1]) {
			return Route{}, segmentError(rest[1])
		}
		return Route{Kind: KindRev, Package: name, File: rest[1]}, nil
	}
	return Route{}, notFound
}

// rangeLikeRE matches what npm parses as a version or an X-range ("1", "1.x",
// "v1.2", "*", "1.2.3-beta"): a tag spelled like that would be read as a
// version range by `npm install pkg@<tag>`.
var rangeLikeRE = regexp.MustCompile(`^[vV]?([0-9]+|[xX*])(\.([0-9]+|[xX*])(\.([0-9]+|[xX*])(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?)?)?$`)

// ValidTagName reports whether tag may be used as a new dist-tag name: not a
// version, not a range, and free of whitespace and range operators (QA L-16).
// Tags stored by older builds are not re-checked.
func ValidTagName(tag string) bool {
	if tag == "" || semver.Valid(tag) || rangeLikeRE.MatchString(tag) {
		return false
	}
	for _, r := range tag {
		if unicode.IsSpace(r) || strings.ContainsRune("^~<>=|*", r) {
			return false
		}
	}
	return true
}

func invalidTag(tag string) error {
	return repo.Errorf(http.StatusBadRequest, "invalid dist-tag name %q: a tag must not look like a version or a version range", tag)
}

func invalidName(name string) error {
	return repo.Errorf(http.StatusBadRequest, "invalid package name %q", name)
}

// validSegment reports whether s is safe to use as a free-form path segment
// (version, tarball file, dist-tag or user).
func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." || strings.ContainsRune(s, '\\') {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return false
		}
	}
	return true
}

func segmentError(s string) error {
	return repo.Errorf(http.StatusBadRequest, "invalid path segment %q", s)
}

// baseName strips the scope: "@scope/pkg" → "pkg".
func baseName(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}

// TarballFile is the tarball file name for a version, e.g. "pkg-1.0.0.tgz".
func TarballFile(name, version string) string { return baseName(name) + "-" + version + ".tgz" }

// PackumentKey is the asset path of a package document.
func PackumentKey(name string) string { return name + "/package.json" }

// TarballKey is the asset path of a tarball.
func TarballKey(name, file string) string { return name + "/-/" + file }
