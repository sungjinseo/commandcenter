// Package semver orders semantic versions (2.0.0). A leading "v" and build
// metadata ("+...") are ignored, so Go module versions parse too.
package semver

import (
	"regexp"
	"strconv"
	"strings"
)

type Version struct {
	Major, Minor, Patch uint64
	Pre                 []string
}

// Parse parses "1.2.3", "v1.2.3", "1.2.3-beta.1+build".
func Parse(s string) (Version, bool) {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	core, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var nums [3]uint64
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return Version{}, false
		}
		nums[i] = n
	}
	v := Version{Major: nums[0], Minor: nums[1], Patch: nums[2]}
	if pre != "" {
		v.Pre = strings.Split(pre, ".")
	}
	return v, true
}

// Release reports whether v has no prerelease part.
func (v Version) Release() bool { return len(v.Pre) == 0 }

// Compare orders versions by semver precedence.
func Compare(a, b Version) int {
	for _, d := range [][2]uint64{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.Release() && b.Release():
		return 0
	case a.Release():
		return 1
	case b.Release():
		return -1
	}
	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		if c := comparePre(a.Pre[i], b.Pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.Pre) < len(b.Pre):
		return -1
	case len(a.Pre) > len(b.Pre):
		return 1
	}
	return 0
}

func comparePre(a, b string) int {
	na, errA := strconv.ParseUint(a, 10, 64)
	nb, errB := strconv.ParseUint(b, 10, 64)
	switch {
	case errA == nil && errB == nil:
		switch {
		case na < nb:
			return -1
		case na > nb:
			return 1
		}
		return 0
	case errA == nil:
		return -1 // numeric identifiers sort before alphanumeric ones
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// NewerRelease returns whichever of a and b should be treated as "latest": a
// release beats a prerelease, otherwise the higher version wins. Unparsable
// versions lose; an empty a means "none yet".
func NewerRelease(a, b string) string {
	if a == "" {
		return b
	}
	va, okA := Parse(a)
	vb, okB := Parse(b)
	switch {
	case !okB:
		return a
	case !okA:
		return b
	}
	if va.Release() != vb.Release() {
		if va.Release() {
			return a
		}
		return b
	}
	if Compare(vb, va) > 0 {
		return b
	}
	return a
}

// versionRE is strict SemVer 2.0.0 (the grammar at semver.org): no leading
// "v" or "=", no whitespace, and no leading zeros in the numeric core or in a
// numeric prerelease identifier ("1.0.0-01" is invalid; build metadata may
// have them).
var versionRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
	`(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?` +
	`(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER: npm (node-semver)
// rejects a major, minor or patch above it.
const maxSafeInteger = 1<<53 - 1

// Valid reports whether s is a strict SemVer 2.0.0 version of at most 256
// bytes whose major, minor and patch are at most 2^53-1. Parse stays lenient
// for ordering stored versions; Valid is for checking new ones.
func Valid(s string) bool {
	m := versionRE.FindStringSubmatch(s)
	if len(s) > 256 || m == nil {
		return false
	}
	for _, n := range m[1:4] {
		if v, err := strconv.ParseUint(n, 10, 64); err != nil || v > maxSafeInteger {
			return false
		}
	}
	return true
}
