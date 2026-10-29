package composer

import (
	"regexp"
	"strings"
)

// Composer's VersionParser::normalize, for the common cases: classical
// versions (up to four numbers, a stability suffix, "-dev"), "dev-<branch>"
// and numeric branches ("1.x-dev"). Date versions are not recognised.
var (
	classicalRE = regexp.MustCompile(`(?i)^v?(\d{1,5})(\.\d+)?(\.\d+)?(\.\d+)?[._-]?(?:(stable|beta|b|RC|alpha|a|patch|pl|p)((?:[.-]?\d+)*))?([.-]?dev)?$`)
	branchRE    = regexp.MustCompile(`^v?(\d+)(\.(?:\d+|[xX*]))?(\.(?:\d+|[xX*]))?(\.(?:\d+|[xX*]))?$`)
)

// Normalize returns the normalized form of a Composer version ("v1.2" →
// "1.2.0.0", "1.0.0-beta1" → "1.0.0.0-beta1", "1.x-dev" →
// "1.9999999.9999999.9999999-dev", "dev-main" as is) and whether v is one.
func Normalize(v string) (string, bool) {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "master", "trunk", "default":
		return "9999999-dev", true
	}
	if len(v) > 4 && strings.EqualFold(v[:4], "dev-") {
		return "dev-" + v[4:], true
	}
	if m := classicalRE.FindStringSubmatch(v); m != nil {
		out := m[1]
		for _, part := range m[2:5] {
			if part == "" {
				part = ".0"
			}
			out += part
		}
		if s := strings.ToLower(m[5]); s != "" && s != "stable" {
			out += "-" + expandStability(s) + strings.TrimLeft(m[6], ".-")
		}
		if m[7] != "" {
			out += "-dev"
		}
		return out, true
	}
	if stem, ok := strings.CutSuffix(strings.ToLower(v), "dev"); ok {
		if m := branchRE.FindStringSubmatch(strings.TrimRight(stem, ".-")); m != nil {
			out := m[1]
			for _, part := range m[2:5] {
				switch part {
				case "":
					part = ".9999999"
				case ".x", ".X", ".*":
					part = ".9999999"
				}
				out += part
			}
			return out + "-dev", true
		}
	}
	return "", false
}

func expandStability(s string) string {
	switch s {
	case "a":
		return "alpha"
	case "b":
		return "beta"
	case "p", "pl":
		return "patch"
	case "rc":
		return "RC"
	}
	return s
}

// IsDev reports whether a normalized version is a development one, listed
// in the package's ~dev metadata file.
func IsDev(normalized string) bool {
	return strings.HasPrefix(normalized, "dev-") || strings.HasSuffix(normalized, "-dev")
}
