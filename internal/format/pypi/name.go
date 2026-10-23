// Package pypi implements the Python package index format (PEP 503 / PEP 691).
package pypi

import (
	"net/http"
	"regexp"
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
	KindUpload  Kind = iota // POST /
	KindRoot                // /simple/
	KindProject             // /simple/{project}/
	KindFile                // /packages/{project}/{file} or /packages/{project}/{version}/{file}
)

// Route is a parsed request path. Name is normalized.
type Route struct {
	Kind Kind
	Name string
	File string
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
		return Route{Kind: KindFile, Name: Normalize(name), File: file}, nil
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
// sdists keep "-", so both are compared in normalized form. The version that
// follows the name must start with a digit.
func fileMatches(project, filename string) bool {
	rest, ok := strings.CutPrefix(Normalize(filename), project+"-")
	return ok && rest != "" && rest[0] >= '0' && rest[0] <= '9'
}

// fileKey is the asset path of a distribution file.
func fileKey(project, file string) string { return "packages/" + project + "/" + file }

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
	for i := 0; i < len(base)-1; i++ {
		if base[i] == '-' && base[i+1] >= '0' && base[i+1] <= '9' && Normalize(base[:i]) == project {
			return base[i+1:], true
		}
	}
	return "", false
}

// canonicalFile is the identity of a distribution file independent of its
// spelling; see canonicalKey. The project is taken from the file name.
func canonicalFile(filename string) string { return canonicalKey("", filename) }

// canonicalKey builds the identity from parsed fields: the normalized project,
// the canonical version (trailing ".0" release segments dropped), for wheels
// the remaining tag fields kept apart (build, python, abi, platform), and the
// lowercased extension. project may be "" when unknown. A name that does not
// parse falls back to its normalized form.
func canonicalKey(project, filename string) string {
	base, ext, ok := splitUploadExt(filename)
	if !ok {
		return "\x01" + Normalize(filename)
	}
	var name, version string
	var tags []string
	if ext == ".whl" {
		f := strings.Split(base, "-")
		if len(f) < 2 {
			return "\x01" + Normalize(filename)
		}
		name, version, tags = f[0], f[1], f[2:]
	} else {
		i := sdistSplit(project, base)
		if i < 0 {
			return "\x01" + Normalize(filename)
		}
		name, version = base[:i], base[i+1:]
	}
	parts := append([]string{Normalize(name), canonicalVersion(version)}, tags...)
	return strings.ToLower(strings.Join(parts, "\x00")) + "\x00" + ext
}

// sdistSplit returns the index of the dash between project name and version in
// the base of an sdist file name, or -1. A known project must match exactly.
func sdistSplit(project, base string) int {
	for i := 0; i < len(base)-1; i++ {
		if base[i] == '-' && base[i+1] >= '0' && base[i+1] <= '9' && (project == "" || Normalize(base[:i]) == project) {
			return i
		}
	}
	return -1
}

// canonicalVersion lowercases v and drops trailing ".0" release segments
// (1.0.0 and 1.0 are the same version), keeping at least one segment.
func canonicalVersion(v string) string {
	v = strings.ToLower(v)
	i := 0
	if j := strings.IndexByte(v, '!'); j >= 0 {
		i = j + 1
	}
	j := i
	for j < len(v) && (v[j] == '.' || v[j] >= '0' && v[j] <= '9') {
		j++
	}
	release, rest := v[i:j], v[j:]
	release = strings.TrimRight(release, ".")
	for strings.HasSuffix(release, ".0") {
		release = strings.TrimSuffix(release, ".0")
	}
	return v[:i] + release + rest
}
