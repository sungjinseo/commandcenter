package nuget

import (
	"regexp"
	"strconv"
	"strings"
)

// version is a NuGet package version: one to four numeric parts, release
// labels and build metadata (SemVer 2 plus the legacy fourth "revision").
type version struct {
	num  [4]uint64
	pre  string // release labels as written
	meta string // build metadata, not part of the identity
}

var labelRE = regexp.MustCompile(`^[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*$`)

// parseVersion parses s as NuGet.Versioning does, leniently about leading
// zeros ("1.01.0" is 1.1.0) and the number of parts ("1.0" is 1.0.0).
func parseVersion(s string) (version, bool) {
	var v version
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s, v.meta = s[:i], s[i+1:]
		if !labelRE.MatchString(v.meta) {
			return version{}, false
		}
	}
	core := s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		core, v.pre = s[:i], s[i+1:]
		if !labelRE.MatchString(v.pre) {
			return version{}, false
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) > 4 {
		return version{}, false
	}
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil || p == "" || strings.ContainsAny(p, "+-_ ") {
			return version{}, false
		}
		v.num[i] = n
	}
	return v, true
}

// String is the normalized version NuGet names files and URLs with:
// three numeric parts, a fourth when it is not 0, the release labels,
// no build metadata.
func (v version) String() string {
	s := strconv.FormatUint(v.num[0], 10) + "." + strconv.FormatUint(v.num[1], 10) + "." + strconv.FormatUint(v.num[2], 10)
	if v.num[3] != 0 {
		s += "." + strconv.FormatUint(v.num[3], 10)
	}
	if v.pre != "" {
		s += "-" + v.pre
	}
	return s
}

// key is the lower-cased normalized version: the row version and the path
// part. NuGet compares versions without regard to case.
func (v version) key() string { return strings.ToLower(v.String()) }

func (v version) prerelease() bool { return v.pre != "" }

// compareVersions orders versions as NuGet does: numeric parts, then a
// release above its prereleases, then the labels as SemVer 2 orders them
// (numeric labels numerically, others case-insensitively).
func compareVersions(a, b version) int {
	for i := range a.num {
		if a.num[i] != b.num[i] {
			if a.num[i] < b.num[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.pre == "" && b.pre == "":
		return 0
	case a.pre == "":
		return 1
	case b.pre == "":
		return -1
	}
	as, bs := strings.Split(strings.ToLower(a.pre), "."), strings.Split(strings.ToLower(b.pre), ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aerr := strconv.ParseUint(as[i], 10, 64)
		bn, berr := strconv.ParseUint(bs[i], 10, 64)
		var c int
		switch {
		case aerr == nil && berr == nil:
			c = cmp(an, bn)
		case aerr == nil:
			c = -1
		case berr == nil:
			c = 1
		default:
			c = strings.Compare(as[i], bs[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmp(uint64(len(as)), uint64(len(bs)))
}

func cmp(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// versionRange is the registration "range" of a nuspec dependency version:
// a plain version means "at least", written [v, ); "" means any, (, ); a
// range written in the nuspec is kept.
func versionRange(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "(, )"
	case strings.ContainsAny(s, "[]()"):
		return s
	}
	if v, ok := parseVersion(s); ok {
		return "[" + v.String() + ", )"
	}
	return s
}
