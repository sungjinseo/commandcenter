package maven

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"commandcenter/internal/repo"
)

// maxPOMSize bounds an uploaded .pom (413 beyond it), as the web UI does
// when it reads one. Real ones are a few KiB, the largest BOMs a few hundred.
const maxPOMSize = 1 << 20

// maxPOMDepth bounds how deep checkPOM lets elements nest: the decoder keeps
// a record per open element (real POMs nest some 15 deep).
const maxPOMDepth = 1000

// maxPOMValues bounds the properties checkPOM keeps for ${...} references.
const maxPOMValues = 1000

// pomSem bounds how many uploaded POMs are parsed at once: a crafted 1 MiB
// one (an element with 100000 attributes, say) takes some 16 MiB to decode.
// The parse reads the stored file, not the client, so a slot is held briefly.
var pomSem = make(chan struct{}, 2)

var pomRefRE = regexp.MustCompile(`\$\{[^}]*\}`)

// checkPOM verifies that r (the stored upload, at most maxPOMSize bytes) is
// well-formed XML with a <project> root naming the coordinates c of its path
// (QA N-6): groupId (or the parent's), artifactId and version (or the
// parent's), after ${name} references are replaced with the POM's own
// <properties>. A reference left in groupId or artifactId stands for any
// text there (sameCoord). A version that holds a reference is not compared:
// a CI-friendly ${revision} may be set on the command line (mvn -Drevision=1.2
// deploy without flatten-maven-plugin, which Nexus accepts) or by the
// parent, not by the POM's defaults. A POM that fails the check gets a 400.
func checkPOM(r io.Reader, c *Coordinates) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	vals, err := parsePOM(b)
	if err != nil {
		return err
	}
	g, _ := expand(vals, inherited(vals, "groupId"))
	a, _ := expand(vals, vals["artifactId"])
	raw := inherited(vals, "version")
	v, _ := expand(vals, raw)
	if sameCoord(g, c.GroupID) && sameCoord(a, c.ArtifactID) && (strings.Contains(raw, "${") || v == c.BaseVersion || v == c.Version) {
		return nil
	}
	got := g + ":" + a + ":" + v
	if raw != v {
		got += " (version " + raw + ")"
	}
	return repo.Errorf(http.StatusBadRequest, ".pom declares %.300q, want %q", got, c.GroupID+":"+c.ArtifactID+":"+c.BaseVersion)
}

// parsePOM is readPOM of b, or a 400. Maven reads bytes that are not
// UTF-8 in a UTF-8 or undeclared POM (text saved in MS949, say) as U+FFFD,
// so such a POM is read a second time that way. One that declares another
// encoding needs no second pass: latin1 reads any byte.
func parsePOM(b []byte) (map[string]string, error) {
	vals, err := readPOM(bytes.NewReader(b))
	if err != nil && !utf8.Valid(b) {
		vals, err = readPOM(bytes.NewReader(bytes.ToValidUTF8(b, []byte("\uFFFD"))))
	}
	if err != nil {
		return nil, repo.Errorf(http.StatusBadRequest, "invalid .pom: %.300v", err)
	}
	return vals, nil
}

// inherited is readPOM's value of the project's own k, else the parent's.
func inherited(vals map[string]string, k string) string {
	if v := vals[k]; v != "" {
		return v
	}
	return vals["parent/"+k]
}

// sameCoord reports whether s, a groupId or artifactId after expand, names
// want. Each ${...} left in s, a property the parent or the command line
// sets (artifactId name_${scala.binary.version}, say), matches one or more
// characters other than '/' and ':'; the rest must be equal. An s over 256
// bytes, which expand leaves alone too, must be equal as a whole.
func sameCoord(s, want string) bool {
	if s == want || len(s) > 256 || !strings.Contains(s, "${") {
		return s == want
	}
	lit := pomRefRE.Split(s, -1)
	for i := range lit {
		lit[i] = regexp.QuoteMeta(lit[i])
	}
	re, err := regexp.Compile("^" + strings.Join(lit, "[^/:]+") + "$")
	return err == nil && re.MatchString(want)
}

// readPOM returns the trimmed text of the project's and its parent's
// groupId, artifactId and version, keyed as "groupId" or "parent/version",
// and of the first maxPOMValues properties, keyed as "${name}".
func readPOM(r io.Reader) (map[string]string, error) {
	br := bufio.NewReader(r)
	if b, _ := br.Peek(3); string(b) == "\xef\xbb\xbf" {
		br.Discard(3) // a UTF-8 byte order mark
	}
	d := xml.NewDecoder(br)
	d.CharsetReader = latin1
	vals := map[string]string{}
	var (
		names [3]string // the open elements of the first three levels
		depth int
		text  []byte // the text of the element pomKey names, so far
		props int    // the "${name}" keys in vals
	)
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 && (names[0] != "" || t.Name.Local != "project") {
				return nil, fmt.Errorf("want one <project> root element, found <%s>", t.Name.Local)
			}
			if depth == maxPOMDepth {
				return nil, fmt.Errorf("elements nest more than %d deep", maxPOMDepth)
			}
			if depth < len(names) {
				names[depth] = t.Name.Local
			}
			depth++
			text = text[:0]
		case xml.CharData:
			if depth == 0 && len(bytes.Trim(t, " \t\r\n")) > 0 {
				return nil, errors.New("text outside the <project> element")
			}
			if pomKey(names, depth) != "" {
				text = append(text, t...)
			}
		case xml.EndElement:
			if k := pomKey(names, depth); k != "" {
				// Only properties count against the cap, so the
				// coordinates are kept wherever they come.
				_, ok := vals[k]
				newProp := !ok && strings.HasPrefix(k, "${")
				if !newProp || props < maxPOMValues {
					vals[k] = strings.TrimSpace(string(text))
					if newProp {
						props++
					}
				}
			}
			depth--
		}
	}
	if names[0] == "" {
		return nil, errors.New("no <project> element")
	}
	return vals, nil
}

// pomKey is readPOM's key for the element open at depth, or "" when its text
// is not kept.
func pomKey(names [3]string, depth int) string {
	switch {
	case depth == 2 && (names[1] == "groupId" || names[1] == "artifactId" || names[1] == "version"):
		return names[1]
	case depth == 3 && names[1] == "parent" && (names[2] == "groupId" || names[2] == "version"):
		return "parent/" + names[2]
	case depth == 3 && names[1] == "properties":
		return "${" + names[2] + "}"
	}
	return ""
}

// expand replaces each ${name} in s with that property, also within the
// values it brings in, and reports whether no reference is left. Values and
// results longer than 256 bytes are not expanded, which bounds the work.
func expand(vals map[string]string, s string) (string, bool) {
	for i := 0; i < 8 && len(s) <= 256 && strings.Contains(s, "${"); i++ {
		s = pomRefRE.ReplaceAllStringFunc(s, func(ref string) string {
			if v, ok := vals[ref]; ok && len(v) <= 256 {
				return v
			}
			return ref
		})
	}
	return s, !strings.Contains(s, "${")
}

// latin1 is readPOM's CharsetReader for a POM that declares an encoding
// other than UTF-8: it reads the bytes as ISO-8859-1. That is exact for
// ISO-8859-1 and keeps the markup and ASCII coordinates of any
// ASCII-compatible encoding (windows-1252, EUC-KR, Shift_JIS, ...), which
// is all readPOM needs.
func latin1(_ string, r io.Reader) (io.Reader, error) {
	return &latin1Reader{r: bufio.NewReader(r)}, nil
}

type latin1Reader struct {
	r    *bufio.Reader
	next byte // the second UTF-8 byte of the last character, or 0
}

func (l *latin1Reader) ReadByte() (byte, error) {
	if b := l.next; b != 0 {
		l.next = 0
		return b, nil
	}
	b, err := l.r.ReadByte()
	if err == nil && b >= 0x80 {
		b, l.next = 0xc0|b>>6, 0x80|b&0x3f
	}
	return b, err
}

func (l *latin1Reader) Read(p []byte) (int, error) {
	for i := range p {
		b, err := l.ReadByte()
		if err != nil {
			return i, err
		}
		p[i] = b
	}
	return len(p), nil
}
