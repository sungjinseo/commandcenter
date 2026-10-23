package pypi

import (
	"regexp"
	"strings"
)

var (
	versionRE = regexp.MustCompile(`(?i)^([1-9][0-9]*!)?(0|[1-9][0-9]*)(\.(0|[1-9][0-9]*))*((a|b|rc)(0|[1-9][0-9]*))?(\.post(0|[1-9][0-9]*))?(\.dev(0|[1-9][0-9]*))?(\+[a-z0-9]+(\.[a-z0-9]+)*)?$`)
	clauseRE  = regexp.MustCompile(`(?i)^(~=|===|==|!=|<=|>=|<|>)\s*[a-z0-9][a-z0-9.*+!_-]*$`)
)

// validVersion reports whether v is a PEP 440 version in the restricted form
// accepted for new uploads: no "v" prefix and no dashes.
func validVersion(v string) bool { return len(v) <= 100 && versionRE.MatchString(v) }

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
