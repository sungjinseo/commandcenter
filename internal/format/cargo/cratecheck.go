package cargo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"commandcenter/internal/repo"
)

// Bounds on the gzip+tar stream checkCrate reads. The total keeps a gzip bomb
// from pinning the CPU; none of them depends on a size an archive claims.
// maxManifest caps the Cargo.toml read into memory (the publish metadata,
// which repeats its features, has the same cap). Tests may lower them.
var (
	maxCrateEntries       = 10000
	maxCrateEntry   int64 = 50 << 20
	maxCrateTotal   int64 = 512 << 20
	maxManifest     int64 = 1 << 20
)

// checkCrate verifies that r is a gzip+tar .crate for name@vers: every entry
// sits under `<name>-<vers>/`, no path is absolute or climbs with `..`, and
// `<name>-<vers>/Cargo.toml` is present and declares name and vers in its
// [package] table. Errors are 400s.
func checkCrate(r io.Reader, name, vers string) error {
	bad := func(format string, a ...any) error {
		return repo.Errorf(http.StatusBadRequest, "invalid .crate: "+format, a...)
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return bad("not a gzip archive")
	}
	defer gz.Close()
	lr := &io.LimitedReader{R: gz, N: maxCrateTotal + 1}
	tr := tar.NewReader(lr)
	root := name + "-" + vers
	manifest := false
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if lr.N <= 0 {
				return bad("more than %d bytes uncompressed", maxCrateTotal)
			}
			return bad("not a tar archive: %v", err)
		}
		if n >= maxCrateEntries {
			return bad("more than %d entries", maxCrateEntries)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeRegA, tar.TypeDir:
		case tar.TypeSymlink, tar.TypeLink:
			return bad("entry %q is a link", h.Name)
		default:
			// Sparse files make tar.Reader synthesize zeros that cost no
			// stream bytes, so only plain files and directories are allowed.
			return bad("entry %q has unsupported type %q", h.Name, h.Typeflag)
		}
		for k := range h.PAXRecords {
			if strings.HasPrefix(k, "GNU.sparse.") {
				return bad("entry %q is a sparse file", h.Name)
			}
		}
		p := h.Name
		if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
			return bad("entry %q has an absolute or unsafe path", h.Name)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." {
				return bad("entry %q contains ..", p)
			}
		}
		if p != root && p != root+"/" && !strings.HasPrefix(p, root+"/") {
			return bad("entry %q is outside %s/", p, root)
		}
		if h.Size > maxCrateEntry {
			return bad("entry %q is larger than %d bytes", p, maxCrateEntry)
		}
		if p == root+"/Cargo.toml" && h.Typeflag == tar.TypeReg {
			manifest = true
		}
		// Every spelling of the manifest path is checked: on a case-insensitive
		// file system any of them may be the file cargo reads.
		var toml []byte
		var written int64
		if strings.EqualFold(p, root+"/Cargo.toml") && h.Typeflag != tar.TypeDir {
			if h.Size > maxManifest {
				return bad("%s is larger than %d bytes", p, maxManifest)
			}
			toml = make([]byte, h.Size)
			var n int
			n, err = io.ReadFull(tr, toml)
			written = int64(n)
		} else {
			written, err = io.Copy(io.Discard, tr)
		}
		total += written
		if err != nil {
			if lr.N <= 0 || total > maxCrateTotal {
				return bad("more than %d bytes uncompressed", maxCrateTotal)
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return bad("truncated archive")
			}
			return bad("corrupt archive: %v", err)
		}
		if lr.N <= 0 || total > maxCrateTotal {
			return bad("more than %d bytes uncompressed", maxCrateTotal)
		}
		if toml != nil {
			n, v, err := manifestPackage(toml)
			if err != nil {
				return bad("%s: %v", p, err)
			}
			if n != name || v != vers {
				return bad("%s declares %.64q version %.64q, but the publish metadata says %q version %q", p, n, v, name, vers)
			}
		}
	}
	if !manifest {
		return bad("%s/Cargo.toml is missing", root)
	}
	return nil
}

// manifestPackage returns the name and version in the [package] table of a
// Cargo.toml. It follows TOML only as far as it must to tell which keys
// belong to that table (comments, the four string forms, arrays, inline
// tables, dotted and quoted keys) and takes the two values only as one-line
// strings, undecoded. cargo publish writes the manifest that way, with
// workspace inheritance resolved, so any other form fails closed.
func manifestPackage(b []byte) (name, version string, err error) {
	s := &tomlScanner{b: b}
	got := map[string]string{}
	top, inPackage, seen := true, false, false
	for {
		s.skip(true)
		if s.i >= len(s.b) {
			break
		}
		if s.eat('[') {
			arr := s.eat('[')
			k, n, ok := s.key()
			if !ok || !s.eat(']') || arr && !s.eat(']') {
				return "", "", errTOML
			}
			top, inPackage = false, !arr && n == 1 && string(k) == "package"
			if inPackage && seen {
				return "", "", errors.New("[package] is defined twice")
			}
			seen = seen || inPackage
		} else {
			k, n, ok := s.key()
			s.skip(false)
			if !ok || !s.eat('=') {
				return "", "", errTOML
			}
			s.skip(false)
			switch {
			case inPackage && (string(k) == "name" || string(k) == "version"):
				v, ok := s.quoted()
				if !ok || n > 1 {
					return "", "", fmt.Errorf("[package] %s is not a plain string (cargo publish resolves workspace inheritance)", k)
				}
				if _, dup := got[string(k)]; dup {
					return "", "", fmt.Errorf("[package] %s is set twice", k)
				}
				got[string(k)] = string(v)
			case top && string(k) == "package":
				return "", "", errors.New("package must be a [package] table")
			default:
				if !s.value(0) {
					return "", "", errTOML
				}
			}
		}
		if !s.eol() {
			return "", "", errTOML
		}
	}
	if !seen {
		return "", "", errors.New("there is no [package] table")
	}
	for _, k := range []string{"name", "version"} {
		if _, ok := got[k]; !ok {
			return "", "", fmt.Errorf("[package] has no %s", k)
		}
	}
	return got["name"], got["version"], nil
}

var errTOML = errors.New("not valid TOML")

// tomlScanner reads b from i on. It allocates nothing per value, so the
// cost of a manifest is its bytes.
type tomlScanner struct {
	b []byte
	i int
}

func (s *tomlScanner) eat(c byte) bool {
	if s.i < len(s.b) && s.b[s.i] == c {
		s.i++
		return true
	}
	return false
}

// skip passes blanks and, with lines, also newlines and comments.
func (s *tomlScanner) skip(lines bool) {
	for s.i < len(s.b) {
		switch c := s.b[s.i]; {
		case c == ' ' || c == '\t' || lines && (c == '\n' || c == '\r'):
			s.i++
		case lines && c == '#':
			s.comment()
		default:
			return
		}
	}
}

func (s *tomlScanner) comment() {
	for s.i < len(s.b) && s.b[s.i] != '\n' {
		s.i++
	}
}

// eol passes the rest of a line: blanks, a comment and the newline.
func (s *tomlScanner) eol() bool {
	s.skip(false)
	if s.i < len(s.b) && s.b[s.i] == '#' {
		s.comment()
	}
	s.eat('\r')
	return s.i == len(s.b) || s.eat('\n')
}

// key reads a dotted key of bare and quoted parts and returns the first part
// and the number of parts.
func (s *tomlScanner) key() (first []byte, n int, ok bool) {
	for {
		s.skip(false)
		start := s.i
		for s.i < len(s.b) && bareKey(s.b[s.i]) {
			s.i++
		}
		part := s.b[start:s.i]
		if s.i == start {
			if part, ok = s.quoted(); !ok {
				return nil, 0, false
			}
		}
		if n++; n == 1 {
			first = part
		}
		s.skip(false)
		if !s.eat('.') {
			return first, n, true
		}
	}
}

func bareKey(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// triple reports whether a multi-line string starts at i.
func (s *tomlScanner) triple() bool {
	return s.i+2 < len(s.b) && s.b[s.i+1] == s.b[s.i] && s.b[s.i+2] == s.b[s.i]
}

// quoted reads a one-line basic or literal string and returns its raw text.
func (s *tomlScanner) quoted() ([]byte, bool) {
	if s.i >= len(s.b) || s.b[s.i] != '"' && s.b[s.i] != '\'' || s.triple() {
		return nil, false
	}
	q := s.b[s.i]
	s.i++
	for start := s.i; s.i < len(s.b) && s.b[s.i] != '\n'; s.i++ {
		switch s.b[s.i] {
		case q:
			s.i++
			return s.b[start : s.i-1], true
		case '\\': // an escape in a basic string; it cannot hide the newline
			if q == '"' && s.i+1 < len(s.b) && s.b[s.i+1] != '\n' {
				s.i++
			}
		}
	}
	return nil, false
}

// value skips one value: a string, an array, an inline table, or a bare
// number, boolean or date, which runs to the next delimiter.
func (s *tomlScanner) value(depth int) bool {
	if depth > 64 || s.i >= len(s.b) {
		return false
	}
	switch c := s.b[s.i]; c {
	case '"', '\'':
		if !s.triple() {
			_, ok := s.quoted()
			return ok
		}
		// A multi-line string ends at the last three quotes of a run of 3 to 5.
		for s.i += 3; s.i < len(s.b); {
			switch {
			case s.b[s.i] == '\\' && c == '"':
				s.i += 2
			case s.b[s.i] == c:
				n := 0
				for ; s.i < len(s.b) && s.b[s.i] == c; s.i++ {
					n++
				}
				if n >= 3 {
					return n <= 5
				}
			default:
				s.i++
			}
		}
		return false
	case '[', '{':
		end := byte(']')
		if c == '{' {
			end = '}'
		}
		for s.i++; ; {
			s.skip(true)
			if s.eat(end) {
				return true
			}
			if c == '{' {
				_, _, ok := s.key()
				if s.skip(false); !ok || !s.eat('=') {
					return false
				}
				s.skip(false)
			}
			if !s.value(depth + 1) {
				return false
			}
			if s.skip(true); !s.eat(',') && (s.i >= len(s.b) || s.b[s.i] != end) {
				return false
			}
		}
	}
	start := s.i
	for s.i < len(s.b) && !strings.ContainsRune(",]}#\r\n", rune(s.b[s.i])) {
		s.i++
	}
	return len(bytes.TrimSpace(s.b[start:s.i])) > 0
}
