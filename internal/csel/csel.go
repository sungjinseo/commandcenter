// Package csel parses and evaluates content selector expressions: the subset
// of Nexus Repository's CSEL that scopes a privilege to paths.
//
//	format == "maven2"
//	path =^ "/org/acme/"                   (starts with)
//	path == "/readme.txt"
//	path =~ "/org/acme/.*-SNAPSHOT/.*"      (RE2, whole path)
//	coordinate.groupId =^ "org.acme"        (maven: path prefix /org/acme)
//	... and ... / ... or ... / not ... / ( ... )
//	... && ... / ... || ... / eq / ne       (JEXL spellings, as Nexus accepts)
//	"maven2" == format                      (the value first)
//
// An expression is evaluated against a Nexus-spelled format ("maven2") and
// a path that starts with "/".
package csel

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// MaxLen bounds an expression's length.
const MaxLen = 1024

// Expr is a parsed expression.
type Expr struct{ root node }

// Parse parses s. The error names the position of the first problem.
func Parse(s string) (*Expr, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("expression is empty")
	}
	if len(s) > MaxLen {
		return nil, fmt.Errorf("expression is longer than %d characters", MaxLen)
	}
	p := &parser{src: s}
	if err := p.lex(); err != nil {
		return nil, err
	}
	n, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.toks) {
		return nil, fmt.Errorf("unexpected %q at %d", p.toks[p.pos].text, p.toks[p.pos].at+1)
	}
	return &Expr{root: n}, nil
}

// Match reports whether the expression holds for format and path.
func (e *Expr) Match(format, path string) bool { return e.root.eval(format, path) }

type node interface {
	eval(format, path string) bool
}

type andNode struct{ l, r node }
type orNode struct{ l, r node }
type notNode struct{ n node }

func (n andNode) eval(f, p string) bool { return n.l.eval(f, p) && n.r.eval(f, p) }
func (n orNode) eval(f, p string) bool  { return n.l.eval(f, p) || n.r.eval(f, p) }
func (n notNode) eval(f, p string) bool { return !n.n.eval(f, p) }

// term compares one field with op: "==", "!=", "=^" or "=~".
type term struct {
	field, op, value string
	re               *regexp.Regexp
}

func (t term) eval(format, path string) bool {
	var v string
	switch t.field {
	case "format":
		v = format
	case "path":
		v = path
	case "coordinate.groupId":
		// A maven groupId is the leading directories: org.acme -> /org/acme.
		// The value was mapped to a path prefix at parse time.
		if format != "maven2" {
			return false
		}
		v = path
	}
	switch t.op {
	case "==":
		return v == t.value
	case "!=":
		return v != t.value
	case "=^":
		return strings.HasPrefix(v, t.value)
	case "^=": // "value" =^ field: the value starts with the field's
		return strings.HasPrefix(t.value, v)
	case "=~":
		return t.re.MatchString(v)
	}
	return false
}

type token struct {
	kind string // ident, str, op, and, or, not, (, )
	text string
	at   int
}

type parser struct {
	src  string
	toks []token
	pos  int
}

var ops = map[string]bool{"==": true, "!=": true, "=^": true, "=~": true}

func (p *parser) lex() error {
	s := p.src
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case i+1 < len(s) && (s[i:i+2] == "&&" || s[i:i+2] == "||"):
			kind := map[string]string{"&&": "and", "||": "or"}[s[i:i+2]]
			p.toks = append(p.toks, token{kind, s[i : i+2], i})
			i += 2
		case c == '(' || c == ')':
			p.toks = append(p.toks, token{string(c), string(c), i})
			i++
		case c == '"' || c == '\'':
			j := i + 1
			var sb strings.Builder
			for ; j < len(s) && s[j] != c; j++ {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				sb.WriteByte(s[j])
			}
			if j >= len(s) {
				return fmt.Errorf("unterminated string at %d", i+1)
			}
			p.toks = append(p.toks, token{"str", sb.String(), i})
			i = j + 1
		case i+1 < len(s) && ops[s[i:i+2]]:
			p.toks = append(p.toks, token{"op", s[i : i+2], i})
			i += 2
		case c == '_' || c == '.' || unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)):
			j := i
			for j < len(s) && (s[j] == '_' || s[j] == '.' || unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j]))) {
				j++
			}
			w := s[i:j]
			kind := "ident"
			switch strings.ToLower(w) {
			case "and", "or", "not":
				kind = strings.ToLower(w)
			case "eq", "ne": // JEXL's words for == and !=
				kind, w = "op", map[string]string{"eq": "==", "ne": "!="}[strings.ToLower(w)]
			}
			p.toks = append(p.toks, token{kind, w, i})
			i = j
		default:
			return fmt.Errorf("unexpected character %q at %d", c, i+1)
		}
	}
	return nil
}

func (p *parser) peek(kind string) bool { return p.pos < len(p.toks) && p.toks[p.pos].kind == kind }

func (p *parser) or() (node, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.peek("or") {
		p.pos++
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = orNode{l, r}
	}
	return l, nil
}

func (p *parser) and() (node, error) {
	l, err := p.not()
	if err != nil {
		return nil, err
	}
	for p.peek("and") {
		p.pos++
		r, err := p.not()
		if err != nil {
			return nil, err
		}
		l = andNode{l, r}
	}
	return l, nil
}

func (p *parser) not() (node, error) {
	if p.peek("not") {
		p.pos++
		n, err := p.not()
		if err != nil {
			return nil, err
		}
		return notNode{n}, nil
	}
	return p.primary()
}

func (p *parser) primary() (node, error) {
	if p.pos >= len(p.toks) {
		return nil, errors.New("unexpected end of expression")
	}
	t := p.toks[p.pos]
	if t.kind == "(" {
		p.pos++
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		if !p.peek(")") {
			return nil, fmt.Errorf("missing ')' for '(' at %d", t.at+1)
		}
		p.pos++
		return n, nil
	}
	if t.kind == "str" && p.pos+2 < len(p.toks) && p.toks[p.pos+1].kind == "op" && p.toks[p.pos+2].kind == "ident" {
		return p.valueFirst()
	}
	if t.kind != "ident" {
		return nil, fmt.Errorf("expected a field name at %d, got %q", t.at+1, t.text)
	}
	if t.text != "format" && t.text != "path" && t.text != "coordinate.groupId" {
		return nil, fmt.Errorf("unknown field %q at %d (use format, path or coordinate.groupId)", t.text, t.at+1)
	}
	if p.pos+2 >= len(p.toks) || p.toks[p.pos+1].kind != "op" || p.toks[p.pos+2].kind != "str" {
		return nil, fmt.Errorf("expected <field> <==|!=|=^|=~> \"value\" at %d", t.at+1)
	}
	op, val := p.toks[p.pos+1], p.toks[p.pos+2]
	p.pos += 3
	tm := term{field: t.text, op: op.text, value: val.text}
	if t.text == "coordinate.groupId" {
		if op.text == "=~" {
			return nil, fmt.Errorf("coordinate.groupId does not support =~ at %d", op.at+1)
		}
		tm.field, tm.value = "coordinate.groupId", "/"+strings.ReplaceAll(strings.Trim(val.text, "."), ".", "/")
		if op.text == "==" || op.text == "!=" { // the whole groupId: its directory
			tm.value += "/"
			tm.op = map[string]string{"==": "=^", "!=": "!^"}[op.text]
			if tm.op == "!^" {
				return notNode{term{field: tm.field, op: "=^", value: tm.value}}, nil
			}
		}
	}
	if op.text == "=~" {
		re, err := regexp.Compile("^(?:" + val.text + ")$")
		if err != nil {
			return nil, fmt.Errorf("invalid regular expression at %d: %v", val.at+1, err)
		}
		tm.re = re
	}
	return tm, nil
}

// valueFirst parses `"value" op field` (Nexus allows either order): == and
// != mean the same as field first, =^ that the value starts with the
// field's. A regular expression and coordinate.groupId need the field
// first.
func (p *parser) valueFirst() (node, error) {
	val, op, f := p.toks[p.pos], p.toks[p.pos+1], p.toks[p.pos+2]
	if f.text != "format" && f.text != "path" {
		return nil, fmt.Errorf("unknown field %q at %d (a value first takes format or path)", f.text, f.at+1)
	}
	if op.text == "=~" {
		return nil, fmt.Errorf("put the field first for =~ at %d", op.at+1)
	}
	p.pos += 3
	tm := term{field: f.text, op: op.text, value: val.text}
	if op.text == "=^" {
		tm.op = "^="
	}
	return tm, nil
}
