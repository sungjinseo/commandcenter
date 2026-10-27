// Package pypi implements the Python package index format (PEP 503 / PEP 691).
package pypi

import (
	"net/http"
	"regexp"
	"slices"
	"strings"

	"commandcenter/internal/repo"
)

var (
	nameRE = regexp.MustCompile(`(?i)^([a-z0-9]|[a-z0-9][a-z0-9._-]*[a-z0-9])$`)
	sepRE  = regexp.MustCompile(`[-_.]+`)
)

// Normalize returns the PEP 503 normalized form of a project name.
func Normalize(name string) string { return strings.ToLower(sepRE.ReplaceAllString(name, "-")) }

// ValidName reports whether name is a valid project name (PEP 508).
func ValidName(name string) bool { return len(name) <= 200 && nameRE.MatchString(name) }

// Kind is the kind of request a path addresses.
type Kind int

const (
	KindUpload   Kind = iota // POST /
	KindRoot                 // /simple/
	KindProject              // /simple/{project}/
	KindFile                 // /packages/{project}/{file} or /packages/{project}/{version}/{file}
	KindMetadata             // /packages/{project}/{file}.metadata (PEP 658)
	KindJSON                 // /pypi/{project}/json or /pypi/{project}/{version}/json
)

// Route is a parsed request path. Name is normalized.
type Route struct {
	Kind    Kind
	Name    string
	File    string
	Version string // KindJSON: the release asked for, "" for the project
}

func ParseRoute(path string) (Route, error) {
	if path == "" {
		return Route{Kind: KindUpload}, nil
	}
	trimmed := strings.TrimSuffix(path, "/")
	segs := strings.Split(trimmed, "/")
	switch {
	case trimmed == "simple":
		return Route{Kind: KindRoot}, nil
	case segs[0] == "simple" && len(segs) == 2:
		if !ValidName(segs[1]) {
			return Route{}, invalidName(segs[1])
		}
		return Route{Kind: KindProject, Name: Normalize(segs[1])}, nil
	case segs[0] == "pypi" && (len(segs) == 3 || len(segs) == 4) && segs[len(segs)-1] == "json":
		if !ValidName(segs[1]) {
			return Route{}, invalidName(segs[1])
		}
		rt := Route{Kind: KindJSON, Name: Normalize(segs[1])}
		if len(segs) == 4 {
			if !validFile(segs[2]) {
				return Route{}, repo.Errorf(http.StatusBadRequest, "invalid version %q", segs[2])
			}
			rt.Version = segs[2]
		}
		return rt, nil
	case segs[0] == "packages" && (len(segs) == 3 || len(segs) == 4):
		name, file := segs[1], segs[len(segs)-1]
		if !ValidName(name) {
			return Route{}, invalidName(name)
		}
		for _, s := range segs[2:] {
			if !validFile(s) {
				return Route{}, repo.Errorf(http.StatusBadRequest, "invalid path segment %q", s)
			}
		}
		rt := Route{Kind: KindFile, Name: Normalize(name), File: file}
		if f, ok := strings.CutSuffix(file, ".metadata"); ok && f != "" {
			rt.Kind, rt.File = KindMetadata, f
		}
		return rt, nil
	}
	return Route{}, repo.Errorf(http.StatusNotFound, "not found: %s", path)
}

func invalidName(name string) error {
	return repo.Errorf(http.StatusBadRequest, "invalid project name %q", name)
}

// validFile accepts a single, non-hidden path segment without control characters.
func validFile(s string) bool {
	if s == "" || s[0] == '.' || len(s) > 255 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '/' || c == '\\' || c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// fileMatches reports whether a distribution file name belongs to project
// (a normalized name): wheels and modern sdists use "_" for separators, older
// sdists keep "-", so both are compared in normalized form. A version must
// follow the name (see versionAt).
func fileMatches(project, filename string) bool {
	rest, ok := strings.CutPrefix(Normalize(filename), project+"-")
	return ok && versionAt(rest, 0)
}

// versionAt reports whether a version starts at s[i]: a digit, or a digit
// after the "v" PEP 440 tolerates, so that "x-v1.3.tar.gz" is reported as an
// invalid version rather than as another project's file.
func versionAt(s string, i int) bool {
	if i < len(s) && (s[i] == 'v' || s[i] == 'V') {
		i++
	}
	return i < len(s) && s[i] >= '0' && s[i] <= '9'
}

// fileKey is the asset path of a distribution file.
func fileKey(project, file string) string { return "packages/" + project + "/" + file }

// metadataKey is the asset path of a file's PEP 658 core metadata sidecar.
func metadataKey(project, file string) string { return fileKey(project, file) + ".metadata" }

// uploadExts are the distribution extensions accepted for new uploads.
var uploadExts = []string{".whl", ".tar.gz", ".zip"}

// splitUploadExt strips an accepted extension (case-insensitive) from file.
func splitUploadExt(file string) (base, ext string, ok bool) {
	lower := strings.ToLower(file)
	for _, e := range uploadExts {
		if strings.HasSuffix(lower, e) && len(file) > len(e) {
			return file[:len(file)-len(e)], e, true
		}
	}
	return file, "", false
}

// fileVersion returns the version part of a distribution file name of project
// (a normalized name) and whether the extension is one accepted for uploads.
func fileVersion(project, filename string) (string, bool) {
	base, ext, ok := splitUploadExt(filename)
	if !ok {
		return "", false
	}
	if ext == ".whl" {
		fields := strings.Split(base, "-")
		if len(fields) < 2 || Normalize(fields[0]) != project || fields[1] == "" {
			return "", false
		}
		return fields[1], true
	}
	if i := sdistSplit(project, base); i >= 0 {
		return base[i+1:], true
	}
	return "", false
}

// canonicalFile is the identity of a distribution file independent of its
// spelling; see canonicalKey. The project is taken from the file name.
func canonicalFile(filename string) string { return canonicalKey("", filename) }

// canonicalKey builds the identity from parsed fields: the normalized project,
// the canonical version, for wheels the normalized tag fields (see wheelTags),
// and for sdists one marker whatever the extension, as a release has one
// sdist (PEP 527). project may be "" when unknown. A name that does not
// parse falls back to its normalized form.
func canonicalKey(project, filename string) string {
	base, ext, ok := splitUploadExt(filename)
	if !ok {
		return "\x01" + Normalize(filename)
	}
	var name, version, rest string
	if ext == ".whl" {
		f := strings.Split(base, "-")
		if len(f) < 2 {
			return "\x01" + Normalize(filename)
		}
		name, version = f[0], f[1]
		tags, ok := wheelTags(f[2:])
		if !ok { // a legacy name that is no valid wheel name: compared as spelled
			tags = strings.Join(f[2:], "\x00")
		}
		rest = tags + "\x00" + ext
	} else {
		i := sdistSplit(project, base)
		if i < 0 {
			return "\x01" + Normalize(filename)
		}
		name, version, rest = base[:i], base[i+1:], "sdist"
	}
	return strings.ToLower(Normalize(name) + "\x00" + canonicalVersion(version) + "\x00" + rest)
}

// wheelTags normalizes the fields after a wheel's name and version (PEP 427):
// an optional build tag, which must start with a digit and compares by that
// number ("01" is "1"), then the python, abi and platform tags, each a set
// ("py3.py2" is "py2.py3", "py3.py3" is "py3"). ok is false when the fields
// are not that.
func wheelTags(f []string) (key string, ok bool) {
	if len(f) != 3 && len(f) != 4 {
		return "", false
	}
	var out []string
	if len(f) == 4 {
		rest := strings.TrimLeft(f[0], "0123456789")
		if len(rest) == len(f[0]) {
			return "", false
		}
		out = append(out, trimZeros(f[0][:len(f[0])-len(rest)])+rest)
		f = f[1:]
	}
	for _, t := range f {
		set := strings.Split(strings.ToLower(t), ".")
		slices.Sort(set)
		if set = slices.Compact(set); set[0] == "" {
			return "", false
		}
		out = append(out, strings.Join(set, "."))
	}
	return strings.Join(out, "\x00"), true
}

// sdistSplit returns the index of the dash between project name and version in
// the base of an sdist file name, or -1. A known project must match exactly.
func sdistSplit(project, base string) int {
	for i := 0; i < len(base)-1; i++ {
		if base[i] == '-' && versionAt(base, i+1) && (project == "" || Normalize(base[:i]) == project) {
			return i
		}
	}
	return -1
}

// canonicalVersion is the normal form of v without trailing ".0" release
// segments, so versions PEP 440 calls equal are equal strings (1.0, 1.0.0 and
// 1.0-0 are 1, 1.0-1 is 1.post1). An invalid v is returned unchanged; it
// never equals the canonical form of a valid one.
func canonicalVersion(v string) string {
	n, ok := normalizeVersion(v)
	if !ok {
		return v
	}
	i := strings.IndexByte(n, '!') + 1
	j := i
	for j < len(n) && (n[j] >= '0' && n[j] <= '9' || n[j] == '.' && j+1 < len(n) && n[j+1] >= '0' && n[j+1] <= '9') {
		j++
	}
	release := n[i:j]
	for strings.HasSuffix(release, ".0") {
		release = strings.TrimSuffix(release, ".0")
	}
	return n[:i] + release + n[j:]
}
