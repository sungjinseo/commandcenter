package cleanup

import (
	"cmp"
	"regexp"
	"strings"
)

// CompareVersions orders two versions of one component as its format does,
// returning -1, 0 or +1. It never fails: a version the format's scheme does
// not parse still gets a consistent place. Formats without a version order
// (docker tags, raw files, Git LFS objects) compare every pair equal; retain
// sorts them by date instead (HasVersionOrder).
//
//   - npm, cargo, helm, go: SemVer 2.0.0 precedence (build metadata ignored).
//   - nuget: SemVer 2.0.0 with NuGet's four-part legacy versions, case-insensitive.
//   - conan: the version before "#<recipe revision>", SemVer-like.
//   - maven: the POM reference's version order specification (ComparableVersion).
//   - pypi: PEP 440.
//   - yum: rpmvercmp on epoch, version and release; the ".arch" suffix is ignored.
//   - apt: Debian Policy 5.6.12 (epoch, upstream version, revision, the "~" rule).
//   - rubygems: Gem::Version; the "-platform" suffix is ignored.
//   - r: R package versions, numbers separated by "." or "-".
//   - composer: normalized Composer versions, dev < alpha < beta < RC < stable < patch.
//   - conda: conda's VersionOrder, then the build number.
func CompareVersions(format, a, b string) int {
	if a == b {
		return 0
	}
	switch format {
	case "maven":
		return compareMaven(a, b)
	case "pypi":
		return comparePEP440(a, b)
	case "yum":
		return compareRPMEVR(stripRPMArch(a), stripRPMArch(b))
	case "apt":
		return compareDebian(a, b)
	case "rubygems":
		a, _, _ = strings.Cut(a, "-")
		b, _, _ = strings.Cut(b, "-")
		return compareGem(a, b)
	case "r":
		return compareR(a, b)
	case "composer":
		return compareComposer(a, b)
	case "conda":
		return compareConda(a, b)
	case "conan":
		a, _, _ = strings.Cut(a, "#")
		b, _, _ = strings.Cut(b, "#")
		return compareSemverish(a, b)
	case "nuget":
		return compareSemverish(strings.ToLower(a), strings.ToLower(b))
	case "npm", "cargo", "helm", "go":
		return compareSemverish(a, b)
	}
	return 0
}

// HasVersionOrder reports whether CompareVersions orders format's versions.
func HasVersionOrder(format string) bool {
	switch format {
	case "docker", "raw", "gitlfs":
		return false
	}
	return true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func allDigits(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// compareNum compares two decimal digit strings of any length; "" is 0.
func compareNum(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if c := cmp.Compare(len(a), len(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// ---- SemVer-like (npm, cargo, helm, go, nuget, conan) ----

// compareSemverish orders by SemVer 2.0.0 precedence and extends it to what
// the stricter parser refuses: any number of core parts (missing ones are 0,
// so NuGet's 1.0 = 1.0.0 = 1.0.0.0), a leading "v", non-numeric parts
// (after numeric ones).
func compareSemverish(a, b string) int {
	ca, pa := splitSemverish(a)
	cb, pb := splitSemverish(b)
	for i := range max(len(ca), len(cb)) {
		if c := compareIdent(partAt(ca, i), partAt(cb, i)); c != 0 {
			return c
		}
	}
	switch {
	case pa == nil && pb == nil:
		return 0
	case pa == nil: // a release follows its prereleases
		return 1
	case pb == nil:
		return -1
	}
	for i := range min(len(pa), len(pb)) {
		if c := compareIdent(pa[i], pb[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(pa), len(pb))
}

func splitSemverish(v string) (core, pre []string) {
	v = strings.TrimLeft(v, "vV=")
	v, _, _ = strings.Cut(v, "+")
	c, p, hasPre := strings.Cut(v, "-")
	core = strings.Split(c, ".")
	if hasPre {
		pre = strings.Split(p, ".")
	}
	return core, pre
}

func partAt(parts []string, i int) string {
	if i < len(parts) {
		return parts[i]
	}
	return "0"
}

// compareIdent compares SemVer identifiers: numeric ones numerically and
// before alphanumeric ones, which compare in ASCII order.
func compareIdent(a, b string) int {
	na, nb := allDigits(a), allDigits(b)
	switch {
	case na && nb:
		return compareNum(a, b)
	case na:
		return -1
	case nb:
		return 1
	}
	return strings.Compare(a, b)
}

// ---- Maven ----

// mvnToken is one token of a Maven version: a number (digits, leading zeros
// dropped) or a qualifier (lower case, aliases resolved), with the separator
// before it ('.' or '-'; a digit/letter transition counts as '-').
type mvnToken struct {
	sep   byte
	num   bool
	trans bool // the separator was a transition
	s     string
}

// null: 0 or the release qualifier.
func (t mvnToken) null() bool { return t.s == "" }

// mavenQualifiers are the qualifiers that sort before all others, in order;
// "" is the release (ga, final and release are aliases of it).
var mavenQualifiers = map[string]int{"alpha": 0, "beta": 1, "milestone": 2, "rc": 3, "snapshot": 4, "": 5, "sp": 6}

func mavenTokens(v string) []mvnToken {
	v = strings.ToLower(v)
	var toks []mvnToken
	sep, trans := byte('.'), false
	for i := 0; ; {
		j := i
		if j < len(v) && isDigit(v[j]) {
			for j < len(v) && isDigit(v[j]) {
				j++
			}
		} else {
			for j < len(v) && !isDigit(v[j]) && v[j] != '.' && v[j] != '-' {
				j++
			}
		}
		s := v[i:j]
		t := mvnToken{sep: sep, trans: trans, num: s == "" || isDigit(s[0])}
		if t.num {
			t.s = strings.TrimLeft(s, "0") // "" is 0: an empty token counts as 0
		} else {
			t.s = s
		}
		toks = append(toks, t)
		if j == len(v) {
			break
		}
		if v[j] == '.' || v[j] == '-' {
			sep, trans, i = v[j], false, j+1
		} else {
			sep, trans, i = '-', true, j
		}
	}
	for i := range toks {
		t := &toks[i]
		if t.num {
			continue
		}
		if i+1 < len(toks) && toks[i+1].num && toks[i+1].trans { // directly followed by a number
			switch t.s {
			case "a":
				t.s = "alpha"
			case "b":
				t.s = "beta"
			case "m":
				t.s = "milestone"
			}
		}
		switch t.s {
		case "cr":
			t.s = "rc"
		case "ga", "final", "release":
			t.s = ""
		}
	}
	// Trim the trailing null tokens (0 and the release qualifier), at the end
	// and before each remaining hyphen.
	out := toks[:0:0]
	trimming := true
	for i := len(toks) - 1; i >= 0; i-- {
		t := toks[i]
		if trimming && t.null() {
			continue
		}
		trimming = t.sep == '-'
		out = append(out, t)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func compareMaven(a, b string) int {
	ta, tb := mavenTokens(a), mavenTokens(b)
	for i := range max(len(ta), len(tb)) {
		var x, y mvnToken
		switch {
		case i >= len(ta):
			y = tb[i]
			x = mvnToken{sep: y.sep, num: y.num} // padding: the null of the other's kind
		case i >= len(tb):
			x = ta[i]
			y = mvnToken{sep: x.sep, num: x.num}
		default:
			x, y = ta[i], tb[i]
		}
		if c := compareMavenToken(x, y); c != 0 {
			return c
		}
	}
	return 0
}

// compareMavenToken: ".qualifier" = "-qualifier" < "-number" < ".number";
// numbers compare numerically, qualifiers by mavenQualifiers, then the
// others alphabetically.
func compareMavenToken(x, y mvnToken) int {
	class := func(t mvnToken) int {
		switch {
		case !t.num:
			return 0
		case t.sep == '-':
			return 1
		}
		return 2
	}
	if c := cmp.Compare(class(x), class(y)); c != 0 {
		return c
	}
	if x.num {
		return compareNum(x.s, y.s)
	}
	rank := func(s string) int {
		if r, ok := mavenQualifiers[s]; ok {
			return r
		}
		return len(mavenQualifiers)
	}
	if c := cmp.Compare(rank(x.s), rank(y.s)); c != 0 {
		return c
	}
	return strings.Compare(x.s, y.s)
}

// ---- PEP 440 ----

// pep440RE is the version grammar of PEP 440's appendix.
var pep440RE = regexp.MustCompile(`(?i)^\s*v?(?:([0-9]+)!)?([0-9]+(?:\.[0-9]+)*)` +
	`([-_.]?(alpha|a|beta|b|preview|pre|c|rc)[-_.]?([0-9]+)?)?` +
	`((?:-([0-9]+))|(?:[-_.]?(post|rev|r)[-_.]?([0-9]+)?))?` +
	`([-_.]?(dev)[-_.]?([0-9]+)?)?` +
	`(?:\+([a-z0-9]+(?:[-_.][a-z0-9]+)*))?\s*$`)

// pepBound is one ordering key of PEP 440: -1 or +1 for the infinities, or
// (phase, n).
type pepBound struct {
	inf   int
	phase int
	n     string
}

func comparePEPBound(a, b pepBound) int {
	if c := cmp.Compare(a.inf, b.inf); c != 0 || a.inf != 0 {
		return c
	}
	if c := cmp.Compare(a.phase, b.phase); c != 0 {
		return c
	}
	return compareNum(a.n, b.n)
}

type pepKey struct {
	epoch          string
	release        []string
	pre, post, dev pepBound
	local          []string // nil: no local version
}

func parsePEP440(v string) (pepKey, bool) {
	m := pep440RE.FindStringSubmatch(v)
	if m == nil {
		return pepKey{}, false
	}
	k := pepKey{epoch: m[1], release: strings.Split(m[2], ".")}
	k.pre, k.post, k.dev = pepBound{inf: 1}, pepBound{inf: -1}, pepBound{inf: 1}
	if m[3] != "" {
		phase := map[string]int{"a": 0, "alpha": 0, "b": 1, "beta": 1}[strings.ToLower(m[4])]
		if l := strings.ToLower(m[4]); l == "c" || l == "rc" || l == "pre" || l == "preview" {
			phase = 2
		}
		k.pre = pepBound{phase: phase, n: m[5]}
	}
	if m[6] != "" {
		k.post = pepBound{n: m[7] + m[9]}
	}
	if m[10] != "" {
		k.dev = pepBound{n: m[12]}
		if m[3] == "" && m[6] == "" { // 1.0.dev1 sorts before 1.0a1
			k.pre = pepBound{inf: -1}
		}
	}
	if m[13] != "" {
		k.local = strings.FieldsFunc(strings.ToLower(m[13]), func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	}
	return k, true
}

// comparePEP440 orders PEP 440 versions; versions that do not follow PEP
// 440 sort before every valid one, among themselves as strings.
func comparePEP440(a, b string) int {
	ka, okA := parsePEP440(a)
	kb, okB := parsePEP440(b)
	switch {
	case !okA && !okB:
		return strings.Compare(a, b)
	case !okA:
		return -1
	case !okB:
		return 1
	}
	if c := compareNum(ka.epoch, kb.epoch); c != 0 {
		return c
	}
	for i := range max(len(ka.release), len(kb.release)) {
		if c := compareNum(partAt(ka.release, i), partAt(kb.release, i)); c != 0 {
			return c
		}
	}
	for _, p := range [][2]pepBound{{ka.pre, kb.pre}, {ka.post, kb.post}, {ka.dev, kb.dev}} {
		if c := comparePEPBound(p[0], p[1]); c != 0 {
			return c
		}
	}
	switch {
	case ka.local == nil && kb.local == nil:
		return 0
	case ka.local == nil:
		return -1
	case kb.local == nil:
		return 1
	}
	// Local segments: alphanumeric ones before numeric ones.
	for i := range min(len(ka.local), len(kb.local)) {
		x, y := ka.local[i], kb.local[i]
		nx, ny := allDigits(x), allDigits(y)
		switch {
		case nx && ny:
			if c := compareNum(x, y); c != 0 {
				return c
			}
		case nx:
			return 1
		case ny:
			return -1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return cmp.Compare(len(ka.local), len(kb.local))
}

// ---- RPM ----

// rpmArches are the architectures that end an RPM file name.
var rpmArches = map[string]bool{"noarch": true, "src": true, "nosrc": true, "x86_64": true, "i386": true, "i486": true,
	"i586": true, "i686": true, "aarch64": true, "armv7hl": true, "armv7l": true, "armhfp": true, "ppc64": true,
	"ppc64le": true, "s390": true, "s390x": true, "riscv64": true, "loongarch64": true, "ia64": true, "x86_64_v2": true}

// stripRPMArch drops a trailing ".<arch>" (the yum format's versions are
// version-release.arch).
func stripRPMArch(v string) string {
	if i := strings.LastIndexByte(v, '.'); i >= 0 && rpmArches[v[i+1:]] {
		return v[:i]
	}
	return v
}

// compareRPMEVR orders [epoch:]version[-release] as rpm does: the epoch
// numerically (absent is 0), then version and release with rpmvercmp.
func compareRPMEVR(a, b string) int {
	ea, va, ra := splitEVR(a)
	eb, vb, rb := splitEVR(b)
	if c := compareNum(ea, eb); c != 0 {
		return c
	}
	if c := rpmvercmp(va, vb); c != 0 {
		return c
	}
	return rpmvercmp(ra, rb)
}

func splitEVR(s string) (epoch, version, release string) {
	if e, rest, ok := strings.Cut(s, ":"); ok && allDigits(e) {
		epoch, s = e, rest
	}
	if i := strings.LastIndexByte(s, '-'); i >= 0 {
		return epoch, s[:i], s[i+1:]
	}
	return epoch, s, ""
}

func isAlnum(c byte) bool { return isDigit(c) || isAlpha(c) }

// rpmvercmp is rpm's segment comparison: runs of digits and of letters
// compare in turn (numbers numerically, letters in ASCII order, a number
// newer than letters), other characters only separate, "~" sorts before
// anything (even the end) and "^" after the end but before anything else.
func rpmvercmp(a, b string) int {
	if a == b {
		return 0
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		for i < len(a) && !isAlnum(a[i]) && a[i] != '~' && a[i] != '^' {
			i++
		}
		for j < len(b) && !isAlnum(b[j]) && b[j] != '~' && b[j] != '^' {
			j++
		}
		ca, cb := byte(0), byte(0)
		if i < len(a) {
			ca = a[i]
		}
		if j < len(b) {
			cb = b[j]
		}
		if ca == '~' || cb == '~' {
			if ca != '~' {
				return 1
			}
			if cb != '~' {
				return -1
			}
			i, j = i+1, j+1
			continue
		}
		if ca == '^' || cb == '^' {
			switch {
			case i == len(a):
				return -1
			case j == len(b):
				return 1
			case ca != '^':
				return 1
			case cb != '^':
				return -1
			}
			i, j = i+1, j+1
			continue
		}
		if i == len(a) || j == len(b) {
			break
		}
		si, sj := i, j
		num := isDigit(a[i])
		if num {
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			for j < len(b) && isDigit(b[j]) {
				j++
			}
		} else {
			for i < len(a) && isAlpha(a[i]) {
				i++
			}
			for j < len(b) && isAlpha(b[j]) {
				j++
			}
		}
		if sj == j { // segments of different types: a number is newer
			if num {
				return 1
			}
			return -1
		}
		var c int
		if num {
			c = compareNum(a[si:i], b[sj:j])
		} else {
			c = strings.Compare(a[si:i], b[sj:j])
		}
		if c != 0 {
			return c
		}
	}
	switch {
	case i >= len(a) && j >= len(b):
		return 0
	case i >= len(a):
		return -1
	}
	return 1
}

// ---- Debian ----

// compareDebian orders Debian versions, [epoch:]upstream[-revision].
func compareDebian(a, b string) int {
	ea, ua, ra := splitDebian(a)
	eb, ub, rb := splitDebian(b)
	if c := compareNum(ea, eb); c != 0 {
		return c
	}
	if c := debVerrevcmp(ua, ub); c != 0 {
		return c
	}
	return debVerrevcmp(ra, rb)
}

func splitDebian(s string) (epoch, upstream, revision string) {
	if e, rest, ok := strings.Cut(s, ":"); ok && allDigits(e) {
		epoch, s = e, rest
	}
	if i := strings.LastIndexByte(s, '-'); i >= 0 {
		return epoch, s[:i], s[i+1:]
	}
	return epoch, s, ""
}

// debOrder is the weight of a character in a non-digit part: "~" first,
// then the end of the part, letters, and the other characters.
func debOrder(s string, i int) int {
	switch {
	case i >= len(s) || isDigit(s[i]):
		return 0
	case isAlpha(s[i]):
		return int(s[i])
	case s[i] == '~':
		return -1
	}
	return int(s[i]) + 256
}

// debVerrevcmp compares alternating non-digit parts (by debOrder) and
// digit parts (numerically), as dpkg does.
func debVerrevcmp(a, b string) int {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		for i < len(a) && !isDigit(a[i]) || j < len(b) && !isDigit(b[j]) {
			if c := cmp.Compare(debOrder(a, i), debOrder(b, j)); c != 0 {
				return c
			}
			i, j = i+1, j+1
		}
		si, sj := i, j
		for i < len(a) && isDigit(a[i]) {
			i++
		}
		for j < len(b) && isDigit(b[j]) {
			j++
		}
		if c := compareNum(a[si:i], b[sj:j]); c != 0 {
			return c
		}
	}
	return 0
}

// ---- RubyGems ----

var gemSegRE = regexp.MustCompile(`[0-9]+|[a-zA-Z]+`)

// gemCanonical is Gem::Version's canonical segments: the numeric release
// part and the prerelease part (from the first letter on), each without
// trailing zeros.
func gemCanonical(v string) []string {
	segs := gemSegRE.FindAllString(v, -1)
	split := len(segs)
	for i, s := range segs {
		if !isDigit(s[0]) {
			split = i
			break
		}
	}
	trim := func(p []string) []string {
		for len(p) > 0 && isDigit(p[len(p)-1][0]) && strings.TrimLeft(p[len(p)-1], "0") == "" {
			p = p[:len(p)-1]
		}
		return p
	}
	return append(trim(segs[:split:split]), trim(segs[split:])...)
}

func compareGem(a, b string) int {
	sa, sb := gemCanonical(a), gemCanonical(b)
	for i := range max(len(sa), len(sb)) {
		x, y := partAt(sa, i), partAt(sb, i)
		nx, ny := isDigit(x[0]), isDigit(y[0])
		switch {
		case nx && ny:
			if c := compareNum(x, y); c != 0 {
				return c
			}
		case nx: // a number is newer than a prerelease string
			return 1
		case ny:
			return -1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return 0
}

// ---- R ----

// compareR orders R package versions, numbers separated by "." or "-"
// (1.0-1 = 1.0.1); with equal leading numbers the longer version is newer.
func compareR(a, b string) int {
	split := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == '-' })
	}
	pa, pb := split(a), split(b)
	for i := range min(len(pa), len(pb)) {
		if c := compareIdent(pa[i], pb[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(pa), len(pb))
}

// ---- Composer ----

// composerStability ranks the suffix of a normalized Composer version.
func composerStability(s string) int {
	switch {
	case s == "":
		return 4
	case strings.HasPrefix(s, "dev"):
		return 0
	case strings.HasPrefix(s, "alpha"), s == "a":
		return 1
	case strings.HasPrefix(s, "beta"), s == "b":
		return 2
	case strings.HasPrefix(s, "rc"):
		return 3
	case strings.HasPrefix(s, "patch"), strings.HasPrefix(s, "pl"), s == "p":
		return 5
	}
	return -1
}

// compareComposer orders normalized Composer versions (1.0.0.0-beta1);
// dev-<branch> versions sort before every numbered one, by name.
func compareComposer(a, b string) int {
	ba, bb := strings.HasPrefix(a, "dev-"), strings.HasPrefix(b, "dev-")
	switch {
	case ba && bb:
		return strings.Compare(a, b)
	case ba:
		return -1
	case bb:
		return 1
	}
	split := func(v string) ([]string, int, string) {
		core, suffix, _ := strings.Cut(strings.ToLower(strings.TrimLeft(v, "vV")), "-")
		suffix = strings.ReplaceAll(suffix, ".", "")
		i := strings.IndexFunc(suffix, func(r rune) bool { return r >= '0' && r <= '9' })
		n := ""
		if i >= 0 {
			suffix, n = suffix[:i], suffix[i:]
		}
		return strings.Split(core, "."), composerStability(suffix), n
	}
	ca, sa, na := split(a)
	cb, sb, nb := split(b)
	for i := range max(len(ca), len(cb)) {
		if c := compareIdent(partAt(ca, i), partAt(cb, i)); c != 0 {
			return c
		}
	}
	if c := cmp.Compare(sa, sb); c != 0 {
		return c
	}
	return compareNum(na, nb)
}

// ---- conda ----

// condaPart is one subcomponent of conda's VersionOrder: rank 0 "dev",
// 1 another string, 2 a number, 3 "post".
type condaPart struct {
	rank int
	s    string
}

func condaParts(v string) [][]condaPart {
	var out [][]condaPart
	for _, comp := range strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '_' || r == '-' }) {
		var parts []condaPart
		for i := 0; i < len(comp); {
			j := i
			num := isDigit(comp[i])
			for j < len(comp) && isDigit(comp[j]) == num {
				j++
			}
			p := condaPart{rank: 2, s: comp[i:j]}
			if !num {
				switch p.s {
				case "dev":
					p.rank = 0
				case "post":
					p.rank = 3
				default:
					p.rank = 1
				}
			}
			parts = append(parts, p)
			i = j
		}
		if parts[0].rank != 2 { // 1.1.a1 = 1.1.0a1
			parts = append([]condaPart{{rank: 2}}, parts...)
		}
		out = append(out, parts)
	}
	return out
}

func compareCondaVersion(a, b string) int {
	ca, cb := condaParts(a), condaParts(b)
	zero := []condaPart{{rank: 2}}
	for i := range max(len(ca), len(cb)) {
		x, y := zero, zero
		if i < len(ca) {
			x = ca[i]
		}
		if i < len(cb) {
			y = cb[i]
		}
		for k := range max(len(x), len(y)) {
			px, py := condaPart{rank: 2}, condaPart{rank: 2}
			if k < len(x) {
				px = x[k]
			}
			if k < len(y) {
				py = y[k]
			}
			if c := cmp.Compare(px.rank, py.rank); c != 0 {
				return c
			}
			var c int
			if px.rank == 2 {
				c = compareNum(px.s, py.s)
			} else {
				c = strings.Compare(px.s, py.s)
			}
			if c != 0 {
				return c
			}
		}
	}
	return 0
}

// compareConda orders conda component versions, "<version>-<build>": the
// version by VersionOrder (epoch "N!" first, the "+local" part last), then
// the build number (the build string's trailing "_N").
func compareConda(a, b string) int {
	va, builda, _ := strings.Cut(strings.ToLower(a), "-")
	vb, buildb, _ := strings.Cut(strings.ToLower(b), "-")
	epoch := func(v string) (string, string) {
		if e, rest, ok := strings.Cut(v, "!"); ok && allDigits(e) {
			return e, rest
		}
		return "0", v
	}
	ea, va := epoch(va)
	eb, vb := epoch(vb)
	if c := compareNum(ea, eb); c != 0 {
		return c
	}
	pa, la, _ := strings.Cut(va, "+")
	pb, lb, _ := strings.Cut(vb, "+")
	if c := compareCondaVersion(pa, pb); c != 0 {
		return c
	}
	if c := compareCondaVersion(la, lb); c != 0 {
		return c
	}
	buildNum := func(s string) string {
		if i := strings.LastIndexByte(s, '_'); i >= 0 && allDigits(s[i+1:]) {
			return s[i+1:]
		}
		if allDigits(s) {
			return s
		}
		return ""
	}
	return compareNum(buildNum(builda), buildNum(buildb))
}
