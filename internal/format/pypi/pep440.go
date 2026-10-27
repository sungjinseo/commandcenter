package pypi

import (
	"regexp"
	"strings"
)

var (
	// versionRE is the PEP 440 grammar (packaging's VERSION_PATTERN) over a
	// lower-cased version, without the leading "v" and surrounding spaces
	// PEP 440 tolerates: no build tool writes them into a file name.
	versionRE = regexp.MustCompile(`^(?:([0-9]+)!)?([0-9]+(?:\.[0-9]+)*)` +
		`(?:[-_.]?(alpha|a|beta|b|preview|pre|c|rc)[-_.]?([0-9]+)?)?` +
		`(?:-([0-9]+)|[-_.]?(post|rev|r)[-_.]?([0-9]+)?)?` +
		`(?:[-_.]?(dev)[-_.]?([0-9]+)?)?` +
		`(?:\+([a-z0-9]+(?:[-_.][a-z0-9]+)*))?$`)
	clauseRE = regexp.MustCompile(`(?i)^(~=|===|==|!=|<=|>=|<|>)\s*[a-z0-9][a-z0-9.*+!_-]*$`)
	preLabel = map[string]string{"alpha": "a", "beta": "b", "c": "rc", "pre": "rc", "preview": "rc"}
)

// validVersion reports whether v is a PEP 440 version (see versionRE).
func validVersion(v string) bool { _, ok := normalizeVersion(v); return ok }

// normalizeVersion returns the PEP 440 normal form of v ("1.0-1" is
// "1.0.post1", "01.2" is "1.2", "2.0A" is "2.0a0", "1.0.dev" is "1.0.dev0")
// and false when v is not a version.
func normalizeVersion(v string) (string, bool) {
	if len(v) > 100 {
		return "", false
	}
	for i := 0; i < len(v); i++ {
		if v[i] >= 0x80 { // strings.ToLower would fold e.g. the Kelvin sign into "k"
			return "", false
		}
	}
	m := versionRE.FindStringSubmatch(strings.ToLower(v))
	if m == nil {
		return "", false
	}
	var b strings.Builder
	if e := trimZeros(m[1]); e != "" && e != "0" {
		b.WriteString(e + "!")
	}
	for i, seg := range strings.Split(m[2], ".") {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(trimZeros(seg))
	}
	if m[3] != "" {
		if l, ok := preLabel[m[3]]; ok {
			m[3] = l
		}
		b.WriteString(m[3] + number(m[4]))
	}
	if m[5] != "" || m[6] != "" {
		b.WriteString(".post" + number(m[5]+m[7])) // m[5] (implicit "-N") and m[7] never both match
	}
	if m[8] != "" {
		b.WriteString(".dev" + number(m[9]))
	}
	if m[10] != "" {
		for i, seg := range strings.FieldsFunc(m[10], func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
			if i == 0 {
				b.WriteByte('+')
			} else {
				b.WriteByte('.')
			}
			if strings.Trim(seg, "0123456789") == "" { // a numeric part compares as a number
				seg = trimZeros(seg)
			}
			b.WriteString(seg)
		}
	}
	return b.String(), true
}

// trimZeros drops the leading zeros of a decimal number, keeping one digit.
func trimZeros(s string) string {
	if t := strings.TrimLeft(s, "0"); t != "" || s == "" {
		return t
	}
	return "0"
}

// number is a pre, post or dev number, implicitly 0.
func number(s string) string {
	if s == "" {
		return "0"
	}
	return trimZeros(s)
}

// validSpecifier reports whether s is a PEP 440 version specifier set such as
// ">=3.8, <4".
func validSpecifier(s string) bool {
	if len(s) > 200 {
		return false
	}
	for _, c := range strings.Split(s, ",") {
		if !clauseRE.MatchString(strings.TrimSpace(c)) {
			return false
		}
	}
	return true
}
