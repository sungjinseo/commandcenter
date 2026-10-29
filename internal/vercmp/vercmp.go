// Package vercmp orders package versions the way each ecosystem's own
// client does, for evaluating OSV affected ranges: npm, crates.io and Go
// (semver), PyPI (PEP 440), Maven (ComparableVersion), NuGet, RubyGems
// (Gem::Version), Packagist (Composer's normalization and PHP's
// version_compare) and CRAN or ConanCenter (dotted numbers). Ecosystem names
// are OSV's. A version an ecosystem's rules cannot parse is ordered by
// Generic, so every pair compares.
package vercmp

import (
	"regexp"
	"strings"

	"commandcenter/internal/format/maven"
	"commandcenter/internal/semver"
)

// Compare returns -1, 0 or 1 as a orders before, equal to or after b in
// ecosystem.
func Compare(ecosystem, a, b string) int {
	switch ecosystem {
	case "npm", "crates.io", "Go":
		return Semver(a, b)
	case "PyPI":
		return PEP440(a, b)
	case "Maven":
		return sign(maven.Compare(a, b))
	case "NuGet":
		return NuGet(a, b)
	case "RubyGems":
		return Gem(a, b)
	case "Packagist":
		return Composer(a, b)
	}
	return Generic(a, b)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// cmpNum compares two digit strings of any length numerically.
func cmpNum(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return cmpInt(int64(len(a)), int64(len(b)))
	}
	return sign(strings.Compare(a, b))
}

func isDigits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// Semver orders semantic versions (a leading "v" and build metadata are
// ignored), falling back to Generic when either does not parse.
func Semver(a, b string) int {
	x, okA := semver.Parse(a)
	y, okB := semver.Parse(b)
	if !okA || !okB {
		return Generic(a, b)
	}
	return semver.Compare(x, y)
}

// tokens splits s into runs of digits and runs of letters, dropping every
// other character as a separator.
func tokens(s string) []string {
	var out []string
	start := -1
	kind := 0 // 1 digit, 2 letter
	for i := 0; i <= len(s); i++ {
		k := 0
		if i < len(s) {
			switch c := s[i]; {
			case c >= '0' && c <= '9':
				k = 1
			case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
				k = 2
			}
		}
		if k != kind {
			if start >= 0 {
				out = append(out, s[start:i])
			}
			start, kind = -1, k
			if k != 0 {
				start = i
			}
		}
	}
	return out
}

// Generic orders dotted versions such as CRAN's 1.2-3 or Conan's 1.2.3:
// runs of digits compare numerically, runs of letters case-insensitively,
// a number after a letter run (1.0 > 1.0rc1), and a missing part as 0.
func Generic(a, b string) int {
	x, y := tokens(strings.ToLower(a)), tokens(strings.ToLower(b))
	for i := 0; i < len(x) || i < len(y); i++ {
		p, q := "0", "0"
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		pn, qn := isDigits(p), isDigits(q)
		var c int
		switch {
		case pn && qn:
			c = cmpNum(p, q)
		case pn:
			c = 1
		case qn:
			c = -1
		default:
			c = sign(strings.Compare(p, q))
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

// ---- PEP 440 ----

// pep440RE is the version pattern of PEP 440's appendix (case-insensitive,
// surrounding space allowed).
var pep440RE = regexp.MustCompile(`(?i)^\s*v?(?:(?:([0-9]+)!)?([0-9]+(?:\.[0-9]+)*)` +
	`(?:[-_.]?(a|b|c|rc|alpha|beta|pre|preview)[-_.]?([0-9]+)?)?` +
	`(?:(?:-([0-9]+))|(?:[-_.]?(post|rev|r)[-_.]?([0-9]+)?))?` +
	`(?:[-_.]?(dev)[-_.]?([0-9]+)?)?)(?:\+([a-z0-9]+(?:[-_.][a-z0-9]+)*))?\s*$`)

type pep440 struct {
	epoch    string
	release  []string
	preKind  int // -1 none; 0 a, 1 b, 2 rc
	pre      string
	post     string // "" none
	hasPost  bool
	dev      string
	hasDev   bool
	local    []string
	hasLocal bool
}

func parsePEP440(s string) (pep440, bool) {
	m := pep440RE.FindStringSubmatch(s)
	if m == nil {
		return pep440{}, false
	}
	v := pep440{epoch: m[1], release: strings.Split(m[2], "."), preKind: -1}
	for len(v.release) > 1 && strings.TrimLeft(v.release[len(v.release)-1], "0") == "" {
		v.release = v.release[:len(v.release)-1]
	}
	if m[3] != "" {
		switch strings.ToLower(m[3]) {
		case "a", "alpha":
			v.preKind = 0
		case "b", "beta":
			v.preKind = 1
		default:
			v.preKind = 2
		}
		v.pre = m[4]
	}
	switch {
	case m[5] != "":
		v.hasPost, v.post = true, m[5]
	case m[6] != "":
		v.hasPost, v.post = true, m[7]
	}
	if m[8] != "" {
		v.hasDev, v.dev = true, m[9]
	}
	if m[10] != "" {
		v.hasLocal, v.local = true, strings.FieldsFunc(strings.ToLower(m[10]), func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	}
	return v, true
}

// PEP440 orders Python versions as packaging.version does: epoch, release
// (trailing zeros ignored), then a dev release before its pre-releases,
// pre-releases before the release, post-releases after it, a local version
// after the same public one.
func PEP440(a, b string) int {
	x, okA := parsePEP440(a)
	y, okB := parsePEP440(b)
	if !okA || !okB {
		return Generic(a, b)
	}
	if c := cmpNum(orZero(x.epoch), orZero(y.epoch)); c != 0 {
		return c
	}
	for i := 0; i < len(x.release) || i < len(y.release); i++ {
		p, q := "0", "0"
		if i < len(x.release) {
			p = x.release[i]
		}
		if i < len(y.release) {
			q = y.release[i]
		}
		if c := cmpNum(p, q); c != 0 {
			return c
		}
	}
	// pre: a bare dev release sorts before every pre-release; no pre after all.
	preKey := func(v pep440) (int, int, string) {
		switch {
		case v.preKind < 0 && !v.hasPost && v.hasDev:
			return 0, 0, "0"
		case v.preKind < 0:
			return 2, 0, "0"
		}
		return 1, v.preKind, orZero(v.pre)
	}
	pc, pk, pn := preKey(x)
	qc, qk, qn := preKey(y)
	if c := cmpInt(int64(pc), int64(qc)); c != 0 {
		return c
	}
	if c := cmpInt(int64(pk), int64(qk)); c != 0 {
		return c
	}
	if c := cmpNum(pn, qn); c != 0 {
		return c
	}
	if x.hasPost != y.hasPost {
		return boolOrder(x.hasPost)
	}
	if c := cmpNum(orZero(x.post), orZero(y.post)); c != 0 {
		return c
	}
	if x.hasDev != y.hasDev { // no dev sorts after a dev release
		return -boolOrder(x.hasDev)
	}
	if c := cmpNum(orZero(x.dev), orZero(y.dev)); c != 0 {
		return c
	}
	if x.hasLocal != y.hasLocal {
		return boolOrder(x.hasLocal)
	}
	for i := 0; i < len(x.local) || i < len(y.local); i++ {
		if i >= len(x.local) {
			return -1
		}
		if i >= len(y.local) {
			return 1
		}
		p, q := x.local[i], y.local[i]
		pn, qn := isDigits(p), isDigits(q)
		var c int
		switch {
		case pn && qn:
			c = cmpNum(p, q)
		case pn: // numeric segments sort after alphanumeric ones
			c = 1
		case qn:
			c = -1
		default:
			c = sign(strings.Compare(p, q))
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// boolOrder is 1 when has (and the other has not), else -1.
func boolOrder(has bool) int {
	if has {
		return 1
	}
	return -1
}

// ---- NuGet ----

// NuGet orders NuGet versions: up to four numeric parts (missing ones are
// 0), a release after its pre-releases, pre-release labels compared
// part by part (numbers numerically and before text, text without case),
// build metadata ignored.
func NuGet(a, b string) int {
	split := func(v string) ([]string, []string, bool) {
		v, _, _ = strings.Cut(strings.TrimSpace(v), "+")
		core, pre, hasPre := strings.Cut(v, "-")
		nums := strings.Split(core, ".")
		if len(nums) > 4 {
			return nil, nil, false
		}
		for _, n := range nums {
			if !isDigits(n) {
				return nil, nil, false
			}
		}
		var labels []string
		if hasPre {
			labels = strings.Split(strings.ToLower(pre), ".")
		}
		return nums, labels, true
	}
	xn, xp, okA := split(a)
	yn, yp, okB := split(b)
	if !okA || !okB {
		return Generic(a, b)
	}
	for i := 0; i < 4; i++ {
		p, q := "0", "0"
		if i < len(xn) {
			p = xn[i]
		}
		if i < len(yn) {
			q = yn[i]
		}
		if c := cmpNum(p, q); c != 0 {
			return c
		}
	}
	if (len(xp) == 0) != (len(yp) == 0) {
		return boolOrder(len(xp) == 0)
	}
	for i := 0; i < len(xp) && i < len(yp); i++ {
		p, q := xp[i], yp[i]
		pn, qn := isDigits(p), isDigits(q)
		var c int
		switch {
		case pn && qn:
			c = cmpNum(p, q)
		case pn:
			c = -1
		case qn:
			c = 1
		default:
			c = sign(strings.Compare(p, q))
		}
		if c != 0 {
			return c
		}
	}
	return cmpInt(int64(len(xp)), int64(len(yp)))
}

// ---- RubyGems ----

// gemSegments are Gem::Version's canonical segments: runs of digits and of
// letters ("-" reads as ".pre."), with trailing zeros dropped from the
// numeric release part and from the pre-release part.
func gemSegments(v string) []string {
	segs := tokens(strings.ReplaceAll(strings.TrimSpace(v), "-", ".pre."))
	start := len(segs)
	for i, s := range segs {
		if !isDigits(s) {
			start = i
			break
		}
	}
	trim := func(p []string) []string {
		for len(p) > 0 && isDigits(p[len(p)-1]) && strings.TrimLeft(p[len(p)-1], "0") == "" {
			p = p[:len(p)-1]
		}
		return p
	}
	num, pre := trim(segs[:start:start]), trim(segs[start:])
	return append(num, pre...)
}

// Gem orders RubyGems versions as Gem::Version#<=> does: segment by
// segment, a missing one is 0, a letter segment (a pre-release) sorts
// before a number, letters compare as strings.
func Gem(a, b string) int {
	x, y := gemSegments(a), gemSegments(b)
	for i := 0; i < len(x) || i < len(y); i++ {
		p, q := "0", "0"
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		pn, qn := isDigits(p), isDigits(q)
		var c int
		switch {
		case pn && qn:
			c = cmpNum(p, q)
		case pn:
			c = 1
		case qn:
			c = -1
		default:
			c = sign(strings.Compare(p, q))
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

// ---- Packagist (Composer) ----

// composerStability is the order of PHP version_compare's special forms;
// an unknown word sorts before all of them.
var composerStability = map[string]int{"dev": 0, "alpha": 1, "a": 1, "beta": 2, "b": 2, "rc": 3, "#": 4, "pl": 5, "p": 5, "patch": 5}

// composerNormalize pads the numeric part of a Composer version to four
// parts (1.2 -> 1.2.0.0) and drops a leading "v", as Composer's
// VersionParser does before comparing.
func composerNormalize(v string) string {
	v = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(strings.ToLower(v)), "v"), "=")
	i := 0
	for i < len(v) && (v[i] >= '0' && v[i] <= '9' || v[i] == '.') {
		i++
	}
	core, rest := strings.Trim(v[:i], "."), v[i:]
	if core == "" {
		return v
	}
	parts := strings.Split(core, ".")
	for len(parts) < 4 {
		parts = append(parts, "0")
	}
	return strings.Join(parts, ".") + rest
}

func phpOrder(s string) int {
	if isDigits(s) {
		return composerStability["#"]
	}
	if n, ok := composerStability[s]; ok {
		return n
	}
	return -1
}

// Composer orders Packagist versions: normalized as Composer does, then
// compared as PHP's version_compare: numbers numerically, special forms in
// the order dev < alpha < beta < RC < (number) < pl, so 1.0.0-beta1 <
// 1.0.0 < 1.0.0-p1.
func Composer(a, b string) int {
	// tokens splits where version_compare canonicalizes: at "-", "_", "+"
	// and "." and between a digit and a non-digit.
	x, y := tokens(composerNormalize(a)), tokens(composerNormalize(b))
	for i := 0; i < len(x) || i < len(y); i++ {
		switch {
		case i >= len(x): // what remains of y decides: a number makes y greater
			return -sign(phpOrder(y[i]) - phpOrder("#") + boolInt(isDigits(y[i])))
		case i >= len(y):
			return sign(phpOrder(x[i]) - phpOrder("#") + boolInt(isDigits(x[i])))
		}
		p, q := x[i], y[i]
		var c int
		if isDigits(p) && isDigits(q) {
			c = cmpNum(p, q)
		} else {
			c = cmpInt(int64(phpOrder(p)), int64(phpOrder(q)))
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
