package saml

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The XML documents of SAML (responses, metadata) are read into a small tree
// of their own: encoding/xml's RawToken does the tokenizing (entities,
// character references, line ends, well-formedness), and this file adds what
// the canonicalization needs and RawToken leaves out: the prefixes as
// written, namespace resolution, matching tags, attribute-value
// normalization and duplicate attributes. A DTD is refused, so no entity but
// the predefined ones ever expands.

// Namespaces.
const (
	nsXML    = "http://www.w3.org/XML/1998/namespace"
	nsXMLNS  = "http://www.w3.org/2000/xmlns/"
	nsP      = "urn:oasis:names:tc:SAML:2.0:protocol"
	nsA      = "urn:oasis:names:tc:SAML:2.0:assertion"
	nsMD     = "urn:oasis:names:tc:SAML:2.0:metadata"
	nsDS     = "http://www.w3.org/2000/09/xmldsig#"
	nsXENC   = "http://www.w3.org/2001/04/xmlenc#"
	nsXENC11 = "http://www.w3.org/2009/xmlenc11#"
	nsEC     = "http://www.w3.org/2001/10/xml-exc-c14n#"
)

// maxDepth bounds element nesting; SAML documents are a dozen deep.
const maxDepth = 64

var errDTD = errors.New("the XML has a DTD, which is not allowed")

type kind uint8

const (
	elementNode kind = iota
	textNode
	commentNode
	piNode
)

// node is an element, a text, a comment or a processing instruction.
type node struct {
	kind   kind
	prefix string // element: the prefix as written ("" for none)
	local  string // element: the local name; PI: the target
	space  string // element: the namespace URI ("" for none)
	attrs  []attr
	ns     []nsDecl // the namespace declarations on the element, in document order
	kids   []*node
	parent *node
	text   string // text, comment or PI data
}

type attr struct{ prefix, local, space, value string }

// nsDecl is a namespace declaration; prefix "" is the default namespace.
type nsDecl struct{ prefix, uri string }

// parse reads b, one UTF-8 XML document, into a tree and returns its root
// element. scope holds the namespace declarations in effect around the
// document (those around a decrypted assertion); nil for none.
func parse(b []byte, scope []nsDecl) (*node, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	top := &node{kind: elementNode, ns: scope} // virtual parent: the outer scope, never output
	cur, depth := top, 0
	var root *node
	for first := true; ; first = false {
		start := d.InputOffset()
		tok, err := d.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the XML does not parse: %w", err)
		}
		switch t := tok.(type) {
		case xml.ProcInst:
			if strings.EqualFold(t.Target, "xml") {
				if !first {
					return nil, errors.New("the XML does not parse: misplaced XML declaration")
				}
				continue
			}
			if cur != top {
				cur.kids = append(cur.kids, &node{kind: piNode, local: t.Target, text: string(t.Inst), parent: cur})
			}
		case xml.Directive:
			return nil, errDTD
		case xml.Comment:
			if cur != top {
				cur.kids = append(cur.kids, &node{kind: commentNode, text: string(t), parent: cur})
			}
		case xml.CharData:
			if cur == top {
				if strings.Trim(string(t), " \t\r\n") != "" {
					return nil, errors.New("the XML does not parse: text outside the root element")
				}
				continue
			}
			if n := len(cur.kids); n > 0 && cur.kids[n-1].kind == textNode {
				cur.kids[n-1].text += string(t) // text next to a CDATA section is one text node
			} else {
				cur.kids = append(cur.kids, &node{kind: textNode, text: string(t), parent: cur})
			}
		case xml.StartElement:
			if cur == top && root != nil {
				return nil, errors.New("the XML does not parse: more than one root element")
			}
			if depth++; depth > maxDepth {
				return nil, errors.New("the XML nests too deeply")
			}
			n, err := element(cur, t, b[start:d.InputOffset()])
			if err != nil {
				return nil, err
			}
			cur.kids = append(cur.kids, n)
			if cur == top {
				root = n
			}
			cur = n
		case xml.EndElement:
			if cur == top || t.Name.Space != cur.prefix || t.Name.Local != cur.local {
				return nil, errors.New("the XML does not parse: mismatched end tag")
			}
			cur, depth = cur.parent, depth-1
		}
	}
	if root == nil || cur != top {
		return nil, errors.New("the XML does not parse: unexpected end")
	}
	return root, nil
}

// element builds the element of start tag t (raw as written) under parent.
func element(parent *node, t xml.StartElement, raw []byte) (*node, error) {
	n := &node{kind: elementNode, prefix: t.Name.Space, local: t.Name.Local, parent: parent}
	if !ncName(n.local) || (n.prefix != "" && !ncName(n.prefix)) {
		return nil, fmt.Errorf("the XML does not parse: bad element name %q", n.local)
	}
	vals, err := attrValues(raw, len(t.Attr))
	if err != nil {
		return nil, err
	}
	for i, a := range t.Attr {
		v := vals[i]
		switch {
		case a.Name.Space == "" && a.Name.Local == "xmlns":
			if v == nsXML || v == nsXMLNS {
				return nil, errors.New("the XML does not parse: bad namespace declaration")
			}
			n.ns = append(n.ns, nsDecl{"", v})
		case a.Name.Space == "xmlns":
			p := a.Name.Local
			if !ncName(p) || p == "xmlns" || (p == "xml") != (v == nsXML) || v == "" || v == nsXMLNS {
				return nil, fmt.Errorf("the XML does not parse: bad namespace declaration of %q", p)
			}
			n.ns = append(n.ns, nsDecl{p, v})
		default:
			if !ncName(a.Name.Local) || (a.Name.Space != "" && !ncName(a.Name.Space)) {
				return nil, fmt.Errorf("the XML does not parse: bad attribute name %q", a.Name.Local)
			}
			n.attrs = append(n.attrs, attr{prefix: a.Name.Space, local: a.Name.Local, value: v})
		}
	}
	for i, x := range n.ns {
		for _, y := range n.ns[:i] {
			if x.prefix == y.prefix {
				return nil, errors.New("the XML does not parse: a namespace is declared twice")
			}
		}
	}
	var ok bool
	if n.space, ok = n.lookup(n.prefix); !ok {
		return nil, fmt.Errorf("the XML does not parse: undeclared prefix %q", n.prefix)
	}
	for i := range n.attrs {
		a := &n.attrs[i]
		if a.prefix != "" {
			if a.space, ok = n.lookup(a.prefix); !ok {
				return nil, fmt.Errorf("the XML does not parse: undeclared prefix %q", a.prefix)
			}
		}
		for _, b := range n.attrs[:i] {
			if a.space == b.space && a.local == b.local {
				return nil, fmt.Errorf("the XML does not parse: duplicate attribute %q", a.local)
			}
		}
	}
	return n, nil
}

// ncName accepts a name without a colon; encoding/xml checked the characters.
func ncName(s string) bool { return s != "" && !strings.Contains(s, ":") }

// attrValues returns the n attribute values of the start tag raw, in order,
// normalized as XML 1.0 section 3.3.3 says for CDATA attributes: a literal
// tab, newline or carriage return becomes a space (a CR LF pair counts
// once), references expand. encoding/xml keeps literal white space as it
// is, and a canonical form built from that would differ from the signer's.
// The syntax was checked by encoding/xml, so a quote always delimits a value.
func attrValues(raw []byte, n int) ([]string, error) {
	out := make([]string, 0, n)
	for i := 0; len(out) < n; {
		q := bytes.IndexAny(raw[i:], `"'`)
		if q < 0 {
			return nil, errors.New("the XML does not parse: malformed start tag")
		}
		i += q
		end := bytes.IndexByte(raw[i+1:], raw[i])
		if end < 0 {
			return nil, errors.New("the XML does not parse: malformed start tag")
		}
		v, err := normalizeAttr(raw[i+1 : i+1+end])
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		i += end + 2
	}
	return out, nil
}

var predefined = map[string]string{"lt": "<", "gt": ">", "amp": "&", "apos": "'", "quot": `"`}

func normalizeAttr(v []byte) (string, error) {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case '\r':
			if i+1 < len(v) && v[i+1] == '\n' {
				i++
			}
			b.WriteByte(' ')
		case '\n', '\t':
			b.WriteByte(' ')
		case '&':
			end := bytes.IndexByte(v[i:], ';')
			if end < 0 {
				return "", errors.New("the XML does not parse: bad reference")
			}
			ref := string(v[i+1 : i+end])
			i += end
			if s, ok := predefined[ref]; ok {
				b.WriteString(s)
				continue
			}
			var r uint64
			err := errors.New("entity")
			switch {
			case strings.HasPrefix(ref, "#x"):
				r, err = strconv.ParseUint(ref[2:], 16, 32)
			case strings.HasPrefix(ref, "#"):
				r, err = strconv.ParseUint(ref[1:], 10, 32)
			}
			if err != nil || !utf8.ValidRune(rune(r)) {
				return "", errors.New("the XML does not parse: bad reference")
			}
			b.WriteRune(rune(r))
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// lookup returns the namespace URI bound to prefix at n ("" for the default
// namespace when none is declared).
func (n *node) lookup(prefix string) (string, bool) {
	if prefix == "xml" {
		return nsXML, true
	}
	for e := n; e != nil; e = e.parent {
		for _, d := range e.ns {
			if d.prefix == prefix {
				return d.uri, true
			}
		}
	}
	return "", prefix == ""
}

// scope returns the namespace declarations in effect at n, nearest first.
func (n *node) scope() []nsDecl {
	var out []nsDecl
	seen := map[string]bool{}
	for e := n; e != nil; e = e.parent {
		for _, d := range e.ns {
			if !seen[d.prefix] {
				seen[d.prefix] = true
				out = append(out, d)
			}
		}
	}
	return out
}

func (n *node) qname() string {
	if n.prefix == "" {
		return n.local
	}
	return n.prefix + ":" + n.local
}

func (n *node) is(space, local string) bool {
	return n != nil && n.kind == elementNode && n.space == space && n.local == local
}

// attr returns the value of n's attribute local without a namespace.
func (n *node) attr(local string) (string, bool) {
	for _, a := range n.attrs {
		if a.space == "" && a.local == local {
			return a.value, true
		}
	}
	return "", false
}

// elements returns n's child elements; text other than white space between
// them is an error (SAML and XML Signature elements have element content).
func (n *node) elements() ([]*node, error) {
	var out []*node
	for _, k := range n.kids {
		switch k.kind {
		case elementNode:
			out = append(out, k)
		case textNode:
			if strings.Trim(k.text, " \t\r\n") != "" {
				return nil, fmt.Errorf("unexpected text in <%s>", n.local)
			}
		}
	}
	return out, nil
}

// children returns n's child elements named {space}local.
func (n *node) children(space, local string) []*node {
	var out []*node
	for _, k := range n.kids {
		if k.is(space, local) {
			out = append(out, k)
		}
	}
	return out
}

// child returns the only child element {space}local of n; none or several
// is an error.
func (n *node) child(space, local string) (*node, error) {
	c := n.children(space, local)
	if len(c) != 1 {
		return nil, fmt.Errorf("<%s> must have exactly one <%s>", n.local, local)
	}
	return c[0], nil
}

// content returns n's text content: its descendant texts, comments left out.
func (n *node) content() string {
	var b strings.Builder
	var walk func(*node)
	walk = func(e *node) {
		for _, k := range e.kids {
			switch k.kind {
			case textNode:
				b.WriteString(k.text)
			case elementNode:
				walk(k)
			}
		}
	}
	walk(n)
	return b.String()
}

// walk calls fn for n and every element below it, in document order.
func (n *node) walk(fn func(*node)) {
	fn(n)
	for _, k := range n.kids {
		if k.kind == elementNode {
			k.walk(fn)
		}
	}
}
