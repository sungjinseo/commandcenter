package saml

import (
	"strings"
	"testing"
)

// The whole-document vectors were written by hand from the specification
// and cross-checked with libxml2 (xmllint --exc-c14n, which keeps comments).
var wholeDocVectors = []struct{ name, in, want string }{
	{"attribute and namespace order",
		`<r xmlns="urn:d" xmlns:b="urn:b" xmlns:a="urn:a" b:x="1" a:y="2" z="3" a="4"/>`,
		`<r xmlns="urn:d" xmlns:a="urn:a" xmlns:b="urn:b" a="4" z="3" a:y="2" b:x="1"></r>`},
	{"unused namespace dropped, redeclaration kept only when different",
		`<a:r xmlns:a="urn:a" xmlns:u="urn:unused"><a:c xmlns:a="urn:a"/><a:d xmlns:a="urn:other"/></a:r>`,
		`<a:r xmlns:a="urn:a"><a:c></a:c><a:d xmlns:a="urn:other"></a:d></a:r>`},
	{"same local name sorted by namespace URI",
		`<r xmlns:z="urn:a" xmlns:a="urn:z" a:k="2" z:k="1"/>`,
		`<r xmlns:a="urn:z" xmlns:z="urn:a" z:k="1" a:k="2"></r>`},
	{"default namespace undeclared",
		`<r xmlns="urn:d"><c xmlns=""><e/></c></r>`,
		`<r xmlns="urn:d"><c xmlns=""><e></e></c></r>`},
	{"no xmlns=\"\" without a rendered default",
		`<a:r xmlns:a="urn:a" xmlns="urn:d"><c xmlns=""/><d/></a:r>`,
		`<a:r xmlns:a="urn:a"><c></c><d xmlns="urn:d"></d></a:r>`},
	{"text escaping",
		`<r>a&amp;b&lt;c&gt;d"e'f&#13;g&#xD;h&#xE9;é</r>`,
		`<r>a&amp;b&lt;c&gt;d"e'f&#xD;g&#xD;héé</r>`},
	{"attribute escaping",
		`<r a="&lt;&amp;&quot;&#9;&#10;&#13;'&gt;"/>`,
		`<r a="&lt;&amp;&quot;&#x9;&#xA;&#xD;'>"></r>`},
	{"literal white space in an attribute is normalized",
		"<r a=\"x\ty\nz\r\nw\"/>",
		`<r a="x y z w"></r>`},
	{"comment, processing instruction, CDATA",
		`<r><!--c--><?p d ?><![CDATA[<x>&]]></r>`,
		`<r><!--c--><?p d ?>&lt;x&gt;&amp;</r>`},
	{"white space in tags, empty elements expanded",
		`<r   a = "1"  ><e  /></r  >`,
		`<r a="1"><e></e></r>`},
	{"xml prefix never declared",
		`<r xml:lang="en" xmlns:xml="http://www.w3.org/XML/1998/namespace"><c/></r>`,
		`<r xml:lang="en"><c></c></r>`},
	{"line ends",
		"<r>a\r\nb\rc</r>",
		"<r>a\nb\nc</r>"},
	{"white space text kept",
		"<r>\n  <c> x </c>\n</r>",
		"<r>\n  <c> x </c>\n</r>"},
	{"attribute prefix declared where used",
		`<r xmlns:p="urn:p"><c p:x="1"/></r>`,
		`<r><c xmlns:p="urn:p" p:x="1"></c></r>`},
}

func TestC14NWholeDocument(t *testing.T) {
	for _, v := range wholeDocVectors {
		root, err := parse([]byte(v.in), nil)
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		if got := string(canonical(root, nil, true, nil)); got != v.want {
			t.Errorf("%s:\n got %q\nwant %q", v.name, got, v.want)
		}
	}
}

func TestC14NWithoutComments(t *testing.T) {
	root, _ := parse([]byte(`<r><!--c--><a><!-- x --></a>t<!--y--></r>`), nil)
	if got := string(canonical(root, nil, false, nil)); got != `<r><a></a>t</r>` {
		t.Fatalf("got %q", got)
	}
	if got := string(canonical(root, nil, true, nil)); got != `<r><!--c--><a><!-- x --></a>t<!--y--></r>` {
		t.Fatalf("with comments: %q", got)
	}
}

// find returns the first element named local.
func find(root *node, local string) *node {
	var out *node
	root.walk(func(n *node) {
		if out == nil && n.local == local {
			out = n
		}
	})
	return out
}

func TestC14NSubtree(t *testing.T) {
	for _, v := range []struct {
		name, doc, apex, skip string
		incl                  []string
		want                  string
	}{
		{"exclusive c14n spec example (2.2)",
			`<n0:local xmlns:n0="foo:bar" xmlns:n3="ftp://example.org"><n1:elem2 xmlns:n1="http://example.net" xml:lang="en"><n3:stuff xmlns:n3="ftp://example.org"/></n1:elem2></n0:local>`,
			"elem2", "", nil,
			`<n1:elem2 xmlns:n1="http://example.net" xml:lang="en"><n3:stuff xmlns:n3="ftp://example.org"></n3:stuff></n1:elem2>`},
		{"inclusive prefix from an ancestor",
			`<n0:local xmlns:n0="foo:bar" xmlns:n3="ftp://example.org"><n1:elem2 xmlns:n1="http://example.net" xml:lang="en"><n3:stuff xmlns:n3="ftp://example.org"/></n1:elem2></n0:local>`,
			"elem2", "", []string{"n0", "zz"},
			`<n1:elem2 xmlns:n0="foo:bar" xmlns:n1="http://example.net" xml:lang="en"><n3:stuff xmlns:n3="ftp://example.org"></n3:stuff></n1:elem2>`},
		{"inherited namespaces rendered where used",
			`<root xmlns:a="urn:a" xmlns="urn:d"><a:c x="1"><d/></a:c></root>`,
			"c", "", nil,
			`<a:c xmlns:a="urn:a" x="1"><d xmlns="urn:d"></d></a:c>`},
		{"apex without a default namespace",
			`<r xmlns="urn:d"><c xmlns=""/></r>`,
			"c", "", nil,
			`<c></c>`},
		{"#default in the PrefixList",
			`<a:r xmlns:a="urn:a" xmlns="urn:d"><a:c><a:e/></a:c></a:r>`,
			"c", "", []string{"#default"},
			`<a:c xmlns="urn:d" xmlns:a="urn:a"><a:e></a:e></a:c>`},
		{"inclusive prefix not repeated below",
			`<r xmlns:xs="urn:xs"><s:a xmlns:s="urn:s"><s:b xmlns:xs="urn:xs" t="xs:string"/></s:a></r>`,
			"a", "", []string{"xs"},
			`<s:a xmlns:s="urn:s" xmlns:xs="urn:xs"><s:b t="xs:string"></s:b></s:a>`},
		{"enveloped signature left out",
			`<r ID="x"><a/><ds:Signature xmlns:ds="urn:ds"><v/></ds:Signature><b/></r>`,
			"r", "Signature", nil,
			`<r ID="x"><a></a><b></b></r>`},
	} {
		root, err := parse([]byte(v.doc), nil)
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		var skip *node
		if v.skip != "" {
			skip = find(root, v.skip)
		}
		if got := string(canonical(find(root, v.apex), skip, false, v.incl)); got != v.want {
			t.Errorf("%s:\n got %q\nwant %q", v.name, got, v.want)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	for name, doc := range map[string]string{
		"DTD":                 `<!DOCTYPE r [<!ENTITY e "x">]><r>&e;</r>`,
		"external entity":     `<!DOCTYPE r SYSTEM "http://evil.example/x.dtd"><r/>`,
		"undefined entity":    `<r>&e;</r>`,
		"undeclared prefix":   `<p:r/>`,
		"undeclared attr ns":  `<r p:a="1"/>`,
		"duplicate attribute": `<r a="1" a="2"/>`,
		"duplicate expanded":  `<r xmlns:p="urn:x" xmlns:q="urn:x" p:a="1" q:a="2"/>`,
		"mismatched tags":     `<r><a></b></r>`,
		"two roots":           `<r/><r/>`,
		"text outside":        `<r/>x`,
		"unclosed":            `<r><a>`,
		"other encoding":      `<?xml version="1.0" encoding="ISO-8859-1"?><r/>`,
		"bad xml prefix":      `<r xmlns:xml="urn:x"/>`,
		"undeclare a prefix":  `<r xmlns:p=""/>`,
		"deep":                strings.Repeat("<a>", maxDepth+1) + strings.Repeat("</a>", maxDepth+1),
	} {
		if _, err := parse([]byte(doc), nil); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
