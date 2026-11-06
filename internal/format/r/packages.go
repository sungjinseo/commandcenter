package r

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"commandcenter/internal/meta"
)

// stdFields are the columns tools::write_PACKAGES writes, in its order;
// Built follows for binary directories.
var stdFields = []string{"Package", "Version", "Priority", "Depends", "Imports", "LinkingTo", "Suggests", "Enhances",
	"License", "License_is_FOSS", "License_restricts_use", "OS_type", "Archs", "MD5sum", "NeedsCompilation"}

// stanza is one PACKAGES entry: fields in order, no empty values.
type stanza [][2]string

func (s stanza) get(name string) string { return get(s, name) }

// stanzaOf is the PACKAGES entry of a hosted row: the standard fields its
// DESCRIPTION has, MD5sum being the file's, and Built for a binary. File
// is never written: the file is always named <pkg>_<version><ext>.
func stanzaOf(r row) stanza {
	var s stanza
	for _, f := range stdFields {
		v := r.field(f)
		if f == "MD5sum" {
			v = r.MD5
		}
		if v != "" {
			s = append(s, [2]string{f, v})
		}
	}
	if v := r.field("Built"); v != "" && binaryDir(r.Dir) {
		s = append(s, [2]string{"Built", v})
	}
	return s
}

// binaryDir reports whether dir holds binary packages: it is below a bin
// directory (bin/windows/contrib/4.4, prod/bin/linux/jammy).
func binaryDir(dir string) bool { return strings.Contains("/"+dir+"/", "/bin/") }

// sourceDir is the src/contrib of the tree binary directory dir is in, ""
// for a directory that is not binary.
func sourceDir(dir string) string {
	if !binaryDir(dir) {
		return ""
	}
	return strings.TrimPrefix(("/" + dir)[:strings.LastIndex("/"+dir+"/", "/bin/")]+"/src/contrib", "/")
}

// write renders the stanza as DCF, continuation lines indented as write.dcf
// does.
func (s stanza) write(w io.StringWriter) error {
	for _, f := range s {
		if _, err := w.WriteString(f[0] + ": " + strings.ReplaceAll(f[1], "\n", "\n        ") + "\n"); err != nil {
			return err
		}
	}
	return nil
}

// render writes the stanzas as DCF, a blank line between them.
func render(ss []stanza) string {
	var b strings.Builder
	for i, s := range ss {
		if i > 0 {
			b.WriteByte('\n')
		}
		s.write(&b)
	}
	return b.String()
}

// eachStanza calls fn with every record of a PACKAGES file (at most maxDoc):
// DCF records separated by blank lines.
func eachStanza(r io.Reader, fn func(stanza) error) error {
	sc := bufio.NewScanner(io.LimitReader(r, maxDoc))
	sc.Buffer(nil, 1<<20)
	var cur stanza
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case strings.TrimSpace(line) == "":
			if cur != nil {
				if err := fn(cur); err != nil {
					return err
				}
				cur = nil
			}
		case line[0] == ' ' || line[0] == '\t':
			if cur == nil {
				return fmt.Errorf("PACKAGES: continuation line %q without a field", line)
			}
			cur[len(cur)-1][1] += "\n" + strings.TrimSpace(line)
		default:
			key, value, ok := strings.Cut(line, ":")
			if !ok || !dcfKeyRE.MatchString(key) {
				return fmt.Errorf("PACKAGES: line %q is not \"Field: value\"", line)
			}
			cur = append(cur, [2]string{key, strings.TrimSpace(value)})
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("PACKAGES: %w", err)
	}
	if cur != nil {
		return fn(cur)
	}
	return nil
}

func parsePackages(r io.Reader) ([]stanza, error) {
	var out []stanza
	err := eachStanza(r, func(s stanza) error {
		out = append(out, s)
		return nil
	})
	return out, err
}

// gzipBytes compresses b without a name or mtime, so the same input gives
// the same bytes.
func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(b)
	gz.Close()
	return buf.Bytes()
}

// columns are the matrix columns of PACKAGES.rds: the standard fields,
// Built for a binary directory, then any other field the stanzas have
// (a member's File or Path), in order of appearance.
func columns(dir string, ss []stanza) []string {
	cols := append([]string(nil), stdFields...)
	if binaryDir(dir) {
		cols = append(cols, "Built")
	}
	seen := map[string]bool{}
	for _, c := range cols {
		seen[c] = true
	}
	for _, s := range ss {
		for _, f := range s {
			if !seen[f[0]] {
				seen[f[0]] = true
				cols = append(cols, f[0])
			}
		}
	}
	return cols
}

// R serialization (XDR, format version 2, what saveRDS writes and any R
// since 2.3 reads) of what PACKAGES.rds holds: a character matrix, rows
// the packages and columns the fields, NA where a stanza has no field.
// Column-major, as R stores matrices; the dim and dimnames attributes
// follow the data as a tagged pairlist. See R's serialize.c.
const (
	sxpNil     = 254
	sxpSym     = 1
	sxpList    = 2
	sxpChar    = 9
	sxpInt     = 13
	sxpStr     = 16
	sxpVec     = 19
	flagAttr   = 1 << 9
	flagTag    = 1 << 10
	utf8Mask   = 1 << 3 << 12 // gp bits are stored at bit 12
	asciiMask  = 1 << 6 << 12
	rVersion   = 4<<16 | 4<<8 // R 4.4.0, the writer
	minVersion = 2<<16 | 3<<8 // R 2.3.0, the oldest reader of format 2
)

type rdsWriter struct{ bytes.Buffer }

func (w *rdsWriter) int(v int32) { binary.Write(&w.Buffer, binary.BigEndian, v) }

// char writes a CHARSXP; "" is NA_character_.
func (w *rdsWriter) char(s string) {
	switch {
	case s == "":
		w.int(sxpChar)
		w.int(-1)
		return
	case isASCII(s):
		w.int(sxpChar | asciiMask)
	case utf8.ValidString(s):
		w.int(sxpChar | utf8Mask)
	default: // ponytail: unknown bytes are left native; DESCRIPTION is UTF-8 or ASCII in practice
		w.int(sxpChar)
	}
	w.int(int32(len(s)))
	w.WriteString(s)
}

func (w *rdsWriter) symbol(name string) {
	w.int(sxpSym)
	w.char(name)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// writeRDS is PACKAGES.rds of the stanzas with the columns cols, gzipped
// as saveRDS compresses.
func writeRDS(cols []string, ss []stanza) []byte {
	var w rdsWriter
	w.WriteString("X\n")
	w.int(2)
	w.int(rVersion)
	w.int(minVersion)
	w.int(sxpStr | flagAttr)
	w.int(int32(len(ss) * len(cols)))
	for _, c := range cols {
		for _, s := range ss {
			w.char(s.get(c))
		}
	}
	w.int(sxpList | flagTag)
	w.symbol("dim")
	w.int(sxpInt)
	w.int(2)
	w.int(int32(len(ss)))
	w.int(int32(len(cols)))
	w.int(sxpList | flagTag)
	w.symbol("dimnames")
	w.int(sxpVec)
	w.int(2)
	w.int(sxpNil)
	w.int(sxpStr)
	w.int(int32(len(cols)))
	for _, c := range cols {
		w.char(c)
	}
	w.int(sxpNil)
	return gzipBytes(w.Bytes())
}

// versionLess orders R package versions: dot- or dash-separated integers
// compared one by one, a missing part being less ("1.0" < "1.0-1").
func versionLess(a, b string) bool {
	pa, pb := strings.FieldsFunc(a, sep), strings.FieldsFunc(b, sep)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		if i >= len(pa) {
			return true
		}
		if i >= len(pb) {
			return false
		}
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return false
}

func sep(r rune) bool { return r == '.' || r == '-' }

// sortStanzas orders by Package, then version.
func sortStanzas(ss []stanza) {
	sort.SliceStable(ss, func(i, j int) bool {
		if a, b := ss[i].get("Package"), ss[j].get("Package"); a != b {
			return a < b
		}
		return versionLess(ss[i].get("Version"), ss[j].get("Version"))
	})
}

// member is what a group's merge reads of one member: its index of the
// directory and, for a binary directory, its source index; nil where it has
// none.
type member struct{ index, source *meta.Asset }

// mergePrefix starts the fingerprint of a merged PACKAGES, so that a copy
// stored by a build with another merge rule is merged again once.
const mergePrefix = "r-merge:"

// mergePackages writes a group's <dir>/PACKAGES: a package name is listed
// from the first member that has it, with all the versions that member
// lists there, whatever a later member has. So a newer public version never
// beats an internal package of the same name, which R would otherwise
// install (dependency confusion). A member's source index counts as well:
// Windows and macOS clients read a binary directory too, and pick a newer
// binary over a source package. With everyVersion (mergeMembers) a member
// keeps what it adds, only a version listed earlier being the first's.
func (h *Handler) mergePackages(w io.Writer, members []member, everyVersion bool) error {
	owned := map[string]bool{}
	first := true
	bw := bufio.NewWriterSize(w, 64<<10)
	for _, m := range members {
		mine := map[string]bool{}
		err := h.eachOf(m.index, func(s stanza) error {
			key := s.get("Package")
			if everyVersion {
				key += "\x00" + s.get("Version")
			}
			if owned[key] {
				return nil
			}
			mine[key] = true
			if !first {
				bw.WriteByte('\n')
			}
			first = false
			return s.write(bw)
		})
		if err != nil {
			return err
		}
		err = h.eachOf(m.source, func(s stanza) error {
			mine[s.get("Package")] = true
			return nil
		})
		if err != nil {
			return err
		}
		maps.Copy(owned, mine)
	}
	return bw.Flush()
}

// eachOf calls fn with every record of a stored index, plain or gzipped
// (nil: none).
func (h *Handler) eachOf(a *meta.Asset, fn func(stanza) error) error {
	if a == nil {
		return nil
	}
	f, err := h.Store.Open(*a)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	var r io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		if r, err = gzip.NewReader(br); err != nil {
			return fmt.Errorf("%s of %q: %w", a.Path, a.Repo, err)
		}
	}
	return eachStanza(r, fn)
}
