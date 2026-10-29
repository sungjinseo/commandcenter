package csel

import (
	"strings"
	"testing"
)

func TestParseAndMatch(t *testing.T) {
	for _, c := range []struct {
		expr         string
		format, path string
		want         bool
	}{
		{`format == "maven2"`, "maven2", "/x", true},
		{`format == "maven2"`, "npm", "/x", false},
		{`format != "maven2"`, "npm", "/x", true},
		{`path =^ "/org/acme/"`, "maven2", "/org/acme/lib/1.0/lib-1.0.jar", true},
		{`path =^ "/org/acme/"`, "maven2", "/org/acmex/lib", false},
		{`path == "/readme.txt"`, "raw", "/readme.txt", true},
		{`path == "/readme.txt"`, "raw", "/readme.txt.bak", false},
		{`path =~ "/org/acme/.*-SNAPSHOT/.*"`, "maven2", "/org/acme/lib/1.0-SNAPSHOT/lib.jar", true},
		{`path =~ "/org/acme/.*-SNAPSHOT/.*"`, "maven2", "/org/acme/lib/1.0/lib.jar", false},
		{`path =~ "acme"`, "maven2", "/org/acme/lib", false}, // whole-path match, as Java's matches
		{`coordinate.groupId =^ "org.acme"`, "maven2", "/org/acme/lib/1.0/lib.jar", true},
		{`coordinate.groupId =^ "org.acme"`, "maven2", "/org/acme-tools/lib", true},
		{`coordinate.groupId == "org.acme"`, "maven2", "/org/acme-tools/lib", false},
		{`coordinate.groupId == "org.acme"`, "maven2", "/org/acme/lib/1.0/lib.jar", true},
		{`coordinate.groupId =^ "org.acme"`, "npm", "/org/acme/lib", false},
		// precedence: and binds tighter than or; not binds tightest
		{`format == "npm" or format == "maven2" and path =^ "/org/"`, "npm", "/x", true},
		{`format == "npm" or format == "maven2" and path =^ "/org/"`, "maven2", "/x", false},
		{`(format == "npm" or format == "maven2") and path =^ "/org/"`, "npm", "/x", false},
		{`not path =^ "/org/" and format == "maven2"`, "maven2", "/com/x", true},
		{`not (path =^ "/org/" and format == "maven2")`, "maven2", "/org/x", false},
		{`not not path =^ "/a"`, "raw", "/a", true},
		{`path =^ '/single/'`, "raw", "/single/x", true},
		{`path == "quo\"te"`, "raw", `quo"te`, true},
		{`AND`, "", "", false}, // error case below
	} {
		e, err := Parse(c.expr)
		if c.expr == "AND" {
			if err == nil {
				t.Errorf("%q parsed", c.expr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.expr, err)
			continue
		}
		if got := e.Match(c.format, c.path); got != c.want {
			t.Errorf("%q on (%s, %s) = %v, want %v", c.expr, c.format, c.path, got, c.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, c := range []struct{ expr, want string }{
		{``, "empty"},
		{`   `, "empty"},
		{`path`, "expected <field>"},
		{`path == `, "expected <field>"},
		{`path = "/x"`, "unexpected character"},
		{`path == /x`, "unexpected character"},
		{`path == "/x`, "unterminated string"},
		{`(path == "/x"`, "missing ')'"},
		{`path == "/x")`, "unexpected \")\""},
		{`path == "/x" and`, "unexpected end"},
		{`size == "3"`, "unknown field"},
		{`path =~ "("`, "invalid regular expression"},
		{`coordinate.groupId =~ "org.*"`, "does not support =~"},
		{strings.Repeat("path == \"/x\" or ", 100) + `path == "/y"`, "longer than"},
	} {
		_, err := Parse(c.expr)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want %q", c.expr, err, c.want)
		}
	}
}
