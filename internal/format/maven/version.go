package maven

import "strings"

// Compare orders Maven versions like a simplified ComparableVersion:
// numbers compare numerically, and qualifiers order as
// alpha < beta < milestone < rc < snapshot < (release) < sp < anything else.
func Compare(a, b string) int {
	x, y := normalize(tokenize(a)), normalize(tokenize(b))
	for i := 0; i < len(x) || i < len(y); i++ {
		var p, q token
		if i < len(x) {
			p = x[i]
		} else {
			p = padFor(y[i])
		}
		if i < len(y) {
			q = y[i]
		} else {
			q = padFor(x[i])
		}
		if c := compareToken(p, q); c != 0 {
			return c
		}
	}
	return 0
}

type token struct {
	num bool
	s   string
}

var aliases = map[string]string{"a": "alpha", "b": "beta", "m": "milestone", "cr": "rc", "ga": "", "final": "", "release": ""}

var qualifierRank = map[string]int{"alpha": 0, "beta": 1, "milestone": 2, "rc": 3, "snapshot": 4, "": 5, "sp": 6}

func tokenize(v string) []token {
	v = strings.ToLower(v)
	var out []token
	start := 0
	flush := func(end int) {
		if end > start {
			s := v[start:end]
			t := token{num: isDigit(s[0]), s: s}
			if !t.num {
				if a, ok := aliases[s]; ok {
					t.s = a
				}
			}
			out = append(out, t)
		}
		start = end
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '.' || c == '-' || c == '_':
			flush(i)
			start = i + 1
		case i > start && isDigit(c) != isDigit(v[i-1]):
			flush(i)
		}
	}
	flush(len(v))
	return out
}

func isZero(t token) bool { return t.num && strings.TrimLeft(t.s, "0") == "" }

// normalize drops numeric zeros that Maven ignores: a zero directly before a
// qualifier (1.0.0-beta -> 1-beta) and trailing zeros (1.0.0 -> 1). Leading
// zeros before another number (0.0.1) are kept.
func normalize(ts []token) []token {
	for changed := true; changed; {
		changed = false
		for i := range ts {
			last := i+1 == len(ts)
			if isZero(ts[i]) && (last || !ts[i+1].num) {
				ts = append(ts[:i:i], ts[i+1:]...)
				changed = true
				break
			}
		}
	}
	return ts
}

// padFor is the value a missing position takes when compared with t.
func padFor(t token) token {
	if t.num {
		return token{num: true, s: "0"}
	}
	return token{s: ""}
}

func compareToken(p, q token) int {
	switch {
	case p.num && q.num:
		a, b := strings.TrimLeft(p.s, "0"), strings.TrimLeft(q.s, "0")
		if len(a) != len(b) {
			return cmp(len(a), len(b))
		}
		return strings.Compare(a, b)
	case p.num:
		return 1
	case q.num:
		return -1
	}
	rp, okp := qualifierRank[p.s]
	rq, okq := qualifierRank[q.s]
	if !okp {
		rp = len(qualifierRank)
	}
	if !okq {
		rq = len(qualifierRank)
	}
	if rp != rq {
		return cmp(rp, rq)
	}
	if !okp && !okq {
		return strings.Compare(p.s, q.s)
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func cmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
