package rubygems

import (
	"strconv"
	"strings"
)

// segment is one piece of a Gem::Version: a number, or a word (which
// makes the version a prerelease and sorts before any number).
type segment struct {
	n    int
	word string
}

// segments splits a version as Gem::Version does: runs of digits and runs
// of letters, separators ignored ("1.0.0.pre.rc1" is 1 0 0 pre rc 1).
func segments(v string) []segment {
	var out []segment
	for i := 0; i < len(v); {
		c := v[i]
		j := i + 1
		switch {
		case c >= '0' && c <= '9':
			for j < len(v) && v[j] >= '0' && v[j] <= '9' {
				j++
			}
			n, err := strconv.Atoi(v[i:j])
			if err != nil { // longer than an int: compare as a word of digits, which keeps the order among themselves
				out = append(out, segment{word: v[i:j]})
			} else {
				out = append(out, segment{n: n})
			}
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			for j < len(v) && (v[j] >= 'a' && v[j] <= 'z' || v[j] >= 'A' && v[j] <= 'Z') {
				j++
			}
			out = append(out, segment{word: v[i:j]})
		}
		i = j
	}
	return out
}

// canonical is Gem::Version#canonical_segments: trailing zeros dropped from
// the numeric part and from the part after the first word.
func canonical(v string) []segment {
	segs := segments(v)
	split := len(segs)
	for i, s := range segs {
		if s.word != "" {
			split = i
			break
		}
	}
	trim := func(s []segment) []segment {
		for len(s) > 0 && s[len(s)-1].word == "" && s[len(s)-1].n == 0 {
			s = s[:len(s)-1]
		}
		return s
	}
	return append(trim(segs[:split:split]), trim(segs[split:])...)
}

// compareVersions orders two versions as Gem::Version#<=> does: segment by
// segment, a missing segment being 0, a word before a number.
func compareVersions(a, b string) int {
	as, bs := canonical(a), canonical(b)
	for i := 0; i < max(len(as), len(bs)); i++ {
		var x, y segment
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		switch {
		case x == y:
		case x.word != "" && y.word == "":
			return -1
		case x.word == "" && y.word != "":
			return 1
		case x.word != "":
			return strings.Compare(x.word, y.word)
		case x.n < y.n:
			return -1
		default:
			return 1
		}
	}
	return 0
}

// prerelease reports whether a version holds a letter, as
// Gem::Version#prerelease? does.
func prerelease(v string) bool {
	return strings.ContainsFunc(v, func(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' })
}

// splitRowVersion splits a stored version, <version>[-<platform>], into the
// two; the platform is "ruby" when there is none.
func splitRowVersion(row string) (version, platform string) {
	version, platform, _ = strings.Cut(row, "-")
	if platform == "" {
		platform = "ruby"
	}
	return version, platform
}
