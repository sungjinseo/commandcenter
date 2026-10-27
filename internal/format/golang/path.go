// Package golang implements the Go module proxy protocol (GOPROXY).
package golang

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"commandcenter/internal/repo"
)

// Kind is the kind of GOPROXY request a path addresses.
type Kind int

const (
	KindList   Kind = iota // {module}/@v/list
	KindInfo               // {module}/@v/{version}.info
	KindMod                // {module}/@v/{version}.mod
	KindZip                // {module}/@v/{version}.zip
	KindLatest             // {module}/@latest
)

// Route is a parsed request. Module and Version are case-encoded, as in the URL.
type Route struct {
	Kind    Kind
	Module  string
	Version string
	Query   bool // .info of a version query such as "master", not a canonical version
}

// errBadVersion marks a request whose version is not canonical (HTTP 400).
var errBadVersion error = &repo.Error{Status: http.StatusBadRequest, Msg: "invalid version"}

// versionError is a 400 that names the version and matches errBadVersion.
type versionError struct{ err *repo.Error }

func badVersion(v string) error {
	return &versionError{repo.Errorf(http.StatusBadRequest, "invalid version %q", v).(*repo.Error)}
}

func (e *versionError) Error() string        { return e.err.Error() }
func (e *versionError) Unwrap() error        { return e.err }
func (e *versionError) Is(target error) bool { return target == errBadVersion }

// validQuerySegment reports whether an escaped .info name is safe to forward
// upstream as a version query.
func validQuerySegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("._~+!-", c) >= 0) {
			return false
		}
	}
	return true
}

var versionRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+incompatible)?$`)

func ParseRoute(path string) (Route, error) {
	if module, ok := strings.CutSuffix(path, "/@latest"); ok {
		if !validModule(module) {
			return Route{}, badModule(module)
		}
		return Route{Kind: KindLatest, Module: module}, nil
	}
	i := strings.LastIndex(path, "/@v/")
	if i < 0 {
		return Route{}, repo.Errorf(http.StatusNotFound, "not found: %s", path)
	}
	module, file := path[:i], path[i+len("/@v/"):]
	if !validModule(module) {
		return Route{}, badModule(module)
	}
	if file == "list" {
		return Route{Kind: KindList, Module: module}, nil
	}
	for _, k := range []Kind{KindInfo, KindMod, KindZip} {
		if v, ok := strings.CutSuffix(file, ext(k)); ok {
			if !validVersion(v) {
				if k == KindInfo && validQuerySegment(v) {
					return Route{Kind: KindInfo, Module: module, Version: v, Query: true}, nil
				}
				return Route{}, badVersion(v)
			}
			return Route{Kind: k, Module: module, Version: v}, nil
		}
	}
	return Route{}, repo.Errorf(http.StatusNotFound, "not found: %s", path)
}

func badModule(m string) error {
	return repo.Errorf(http.StatusBadRequest, "invalid module path %q", m)
}

// validModule checks a case-encoded module path: lower-case path elements
// made of [a-z0-9._~+-] and "!x" escapes, no empty, "." or ".." elements.
func validModule(escaped string) bool {
	if escaped == "" || len(escaped) > 1024 {
		return false
	}
	for _, seg := range strings.Split(escaped, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9', strings.IndexByte("._~+-", c) >= 0:
			case c == '!':
				if i+1 >= len(seg) || seg[i+1] < 'a' || seg[i+1] > 'z' {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

// validVersion checks a case-encoded semantic version as used by Go modules.
func validVersion(escaped string) bool {
	v, err := Unescape(escaped)
	return err == nil && versionRE.MatchString(v)
}

// Escape applies Go's case-encoding: each upper-case letter becomes "!" plus
// its lower-case form.
func Escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Unescape reverses Escape.
func Unescape(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			return "", fmt.Errorf("unescaped upper-case letter in %q", s)
		}
		if c != '!' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(s) || s[i+1] < 'a' || s[i+1] > 'z' {
			return "", fmt.Errorf("invalid escape in %q", s)
		}
		b.WriteByte(s[i+1] - ('a' - 'A'))
		i++
	}
	return b.String(), nil
}

func ext(k Kind) string {
	switch k {
	case KindInfo:
		return ".info"
	case KindMod:
		return ".mod"
	case KindZip:
		return ".zip"
	}
	return ""
}

func contentType(k Kind) string {
	switch k {
	case KindInfo, KindLatest:
		return "application/json"
	case KindZip:
		return "application/zip"
	}
	return "text/plain; charset=utf-8"
}

// assetKey is the storage path of a file under {module}/@v/.
func assetKey(module, file string) string { return module + "/@v/" + file }

var (
	gopkgSuffixRE = regexp.MustCompile(`\.v(0|[1-9][0-9]*)$`)
	slashMajorRE  = regexp.MustCompile(`^v([2-9]|[1-9][0-9]+)$`)
	badMajorRE    = regexp.MustCompile(`^v[0-9.]+$`)
)

// checkPathMajor ports golang.org/x/mod module.CheckPathMajor: the major
// version of version must agree with the major-version suffix of module,
// which must be valid as SplitPathVersion sees it. Both are unescaped and
// version is already a canonical semantic version.
func checkPathMajor(module, version string) error {
	bad := func(format string, a ...any) error {
		return repo.Errorf(http.StatusBadRequest, format, a...)
	}
	major, _, _ := strings.Cut(strings.TrimPrefix(version, "v"), ".")
	incompatible := strings.HasSuffix(version, "+incompatible")
	last := module[strings.LastIndex(module, "/")+1:]

	if strings.HasPrefix(module, "gopkg.in/") {
		m := gopkgSuffixRE.FindStringSubmatch(last)
		if m == nil {
			return bad("module path %q needs a .vN suffix", module)
		}
		if incompatible || m[1] != major {
			return bad("version %s does not match the .v%s suffix of %q", version, m[1], module)
		}
		return nil
	}
	if strings.Contains(module, "/") {
		if m := slashMajorRE.FindStringSubmatch(last); m != nil {
			if incompatible || m[1] != major {
				return bad("version %s does not match the /v%s suffix of %q", version, m[1], module)
			}
			return nil
		}
		// SplitPathVersion: any other /v<digits and dots> (/v0, /v1, /v01,
		// /v1.2) makes the path invalid.
		if badMajorRE.MatchString(last) {
			return bad("module path %q has an invalid major version suffix /%s", module, last)
		}
	}
	if major == "0" || major == "1" || incompatible {
		return nil
	}
	return bad("version %s requires module path %q to end in /v%s (or use +incompatible)", version, module, major)
}

// pseudoVersionRE follows golang.org/x/mod/module.IsPseudoVersion.
var pseudoVersionRE = regexp.MustCompile(`^v[0-9]+\.(0\.0-|\d+\.\d+-([^+]*\.)?0\.)\d{14}-[A-Za-z0-9]+(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// isPseudoVersion reports whether v is a pseudo-version, which @v/list must not show.
func isPseudoVersion(v string) bool {
	return strings.Count(v, "-") >= 2 && versionRE.MatchString(v) && pseudoVersionRE.MatchString(v)
}
