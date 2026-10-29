package saml

import (
	"bytes"
	"errors"
	"maps"
	"slices"
	"strings"
)

// Exclusive XML Canonicalization 1.0 (https://www.w3.org/TR/xml-exc-c14n/)
// of an element subtree, the form XML Signature digests and signs. The
// subset is the apex and everything below it, less an optional excluded
// element (the enveloped-signature transform) and, unless withComments,
// the comments.

const (
	algExcC14N         = "http://www.w3.org/2001/10/xml-exc-c14n#"
	algExcC14NComments = "http://www.w3.org/2001/10/xml-exc-c14n#WithComments"
)

type c14n struct {
	buf      bytes.Buffer
	skip     *node
	comments bool
	incl     []string // the InclusiveNamespaces PrefixList; "" is #default
}

// canonical returns the exclusive canonical form of apex's subtree without
// skip. inclusive is the InclusiveNamespaces PrefixList ("#default" names
// the default namespace).
func canonical(apex, skip *node, withComments bool, inclusive []string) []byte {
	c := &c14n{skip: skip, comments: withComments}
	for _, p := range inclusive {
		if p == "#default" {
			p = ""
		}
		if p != "xml" && p != "xmlns" && !slices.Contains(c.incl, p) {
			c.incl = append(c.incl, p)
		}
	}
	c.element(apex, map[string]string{})
	return c.buf.Bytes()
}

// element writes n. rendered maps each prefix to the namespace an output
// ancestor declared for it ("" for the default namespace).
func (c *c14n) element(n *node, rendered map[string]string) {
	b := &c.buf
	b.WriteByte('<')
	b.WriteString(n.qname())

	// The namespace axis: the prefixes n visibly utilizes (its own, its
	// attributes') and the inclusive ones, each declared unless the nearest
	// output ancestor declared the same.
	want := []string{n.prefix}
	for _, a := range n.attrs {
		if a.prefix != "" {
			want = append(want, a.prefix)
		}
	}
	want = append(want, c.incl...)
	var decls []nsDecl
	for _, p := range want {
		if p == "xml" || slices.ContainsFunc(decls, func(d nsDecl) bool { return d.prefix == p }) {
			continue
		}
		uri, ok := n.lookup(p)
		if !ok {
			continue // an inclusive prefix that is not in scope
		}
		prev, had := rendered[p]
		if p == "" && uri == "" {
			// No default namespace: xmlns="" only to undo an ancestor's.
			if !had || prev == "" {
				continue
			}
		} else if had && prev == uri {
			continue
		}
		decls = append(decls, nsDecl{p, uri})
	}
	slices.SortFunc(decls, func(x, y nsDecl) int { return strings.Compare(x.prefix, y.prefix) })
	if len(decls) > 0 {
		rendered = maps.Clone(rendered)
	}
	for _, d := range decls {
		rendered[d.prefix] = d.uri
		if d.prefix == "" {
			b.WriteString(` xmlns="`)
		} else {
			b.WriteString(` xmlns:` + d.prefix + `="`)
		}
		escapeAttr(b, d.uri)
		b.WriteByte('"')
	}

	// Attributes by namespace URI, then local name.
	attrs := slices.Clone(n.attrs)
	slices.SortFunc(attrs, func(x, y attr) int {
		if r := strings.Compare(x.space, y.space); r != 0 {
			return r
		}
		return strings.Compare(x.local, y.local)
	})
	for _, a := range attrs {
		b.WriteByte(' ')
		if a.prefix != "" {
			b.WriteString(a.prefix + ":")
		}
		b.WriteString(a.local + `="`)
		escapeAttr(b, a.value)
		b.WriteByte('"')
	}
	b.WriteByte('>')

	for _, k := range n.kids {
		switch k.kind {
		case elementNode:
			if k != c.skip {
				c.element(k, rendered)
			}
		case textNode:
			escapeText(b, k.text)
		case commentNode:
			if c.comments {
				b.WriteString("<!--" + k.text + "-->")
			}
		case piNode:
			b.WriteString("<?" + k.local)
			if k.text != "" {
				b.WriteString(" " + k.text)
			}
			b.WriteString("?>")
		}
	}
	b.WriteString("</" + n.qname() + ">")
}

func escapeText(b *bytes.Buffer, s string) {
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '\r':
			b.WriteString("&#xD;")
		default:
			b.WriteRune(r)
		}
	}
}

func escapeAttr(b *bytes.Buffer, s string) {
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '"':
			b.WriteString("&quot;")
		case '\t':
			b.WriteString("&#x9;")
		case '\n':
			b.WriteString("&#xA;")
		case '\r':
			b.WriteString("&#xD;")
		default:
			b.WriteRune(r)
		}
	}
}

// Canonicalize returns the exclusive canonical form, without comments, of
// the element of doc whose ID is id (the root for ""): what a Reference to
// it digests before its signature is inserted. CanonicalizeSignedInfo
// returns the canonical form of the SignedInfo of that element's signature,
// per the method it names: what SignatureValue signs. Both exist to build
// signatures in tests; verification never takes canonical forms from
// outside.
func Canonicalize(doc []byte, id string, inclusive []string) ([]byte, error) {
	el, err := byID(doc, id)
	if err != nil {
		return nil, err
	}
	return canonical(el, nil, false, inclusive), nil
}

// CanonicalizeSignedInfo: see Canonicalize.
func CanonicalizeSignedInfo(doc []byte, id string) ([]byte, error) {
	el, err := byID(doc, id)
	if err != nil {
		return nil, err
	}
	sig, err := el.child(nsDS, "Signature")
	if err != nil {
		return nil, err
	}
	si, err := sig.child(nsDS, "SignedInfo")
	if err != nil {
		return nil, err
	}
	cm, err := si.child(nsDS, "CanonicalizationMethod")
	if err != nil {
		return nil, err
	}
	m, err := c14nOf(cm)
	if err != nil {
		return nil, err
	}
	return canonical(si, nil, m.comments, m.inclusive()), nil
}

func byID(doc []byte, id string) (*node, error) {
	root, err := parse(doc, nil)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return root, nil
	}
	var el *node
	root.walk(func(n *node) {
		if v, ok := n.attr("ID"); ok && v == id && el == nil {
			el = n
		}
	})
	if el == nil {
		return nil, errors.New("no element has that ID")
	}
	return el, nil
}
