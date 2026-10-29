package r

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
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
	if v := r.field("Built"); v != "" && strings.HasPrefix(r.Dir, "bin/") {
		s = append(s, [2]string{"Built", v})
	}
	return s
}

// render writes the stanzas as DCF, a blank line between them,
// continuation lines indented as write.dcf does.
func render(ss []stanza) string {
	var b strings.Builder
	for i, s := range ss {
		if i > 0 {
			b.WriteByte('\n')
		}
		for _, f := range s {
			b.WriteString(f[0] + ": " + strings.ReplaceAll(f[1], "\n", "\n        ") + "\n")
		}
	}
	return b.String()
}

// parsePackages reads a PACKAGES file (at most maxDoc): DCF records
// separated by blank lines.
func parsePackages(r io.Reader) ([]stanza, error) {
	sc := bufio.NewScanner(io.LimitReader(r, maxDoc))
	sc.Buffer(nil, 1<<20)
	var out []stanza
	var cur stanza
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case strings.TrimSpace(line) == "":
			if cur != nil {
				out, cur = append(out, cur), nil
			}
		case line[0] == ' ' || line[0] == '\t':
			if cur == nil {
				return nil, fmt.Errorf("PACKAGES: continuation line %q without a field", line)
			}
			cur[len(cur)-1][1] += "\n" + strings.TrimSpace(line)
		default:
			key, value, ok := strings.Cut(line, ":")
			if !ok || !dcfKeyRE.MatchString(key) {
				return nil, fmt.Errorf("PACKAGES: line %q is not \"Field: value\"", line)
			}
			cur = append(cur, [2]string{key, strings.TrimSpace(value)})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("PACKAGES: %w", err)
	}
	if cur != nil {
		out = append(out, cur)
	}
	return out, nil
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
	if strings.HasPrefix(dir, "bin/") {
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

// mergePackages is the resolve.MergeFunc of a group's <dir>/PACKAGES: the
// members' stanzas in member order, a Package and Version listed by
// several members being the first member's (the one that answers for
// the file, as Lookup does).
func mergePackages(ctx context.Context, members []io.ReadSeeker, w io.Writer) error {
	seen := map[string]bool{}
	var out []stanza
	for _, m := range members {
		ss, err := parsePackages(m)
		if err != nil {
			return err
		}
		for _, s := range ss {
			key := s.get("Package") + "\x00" + s.get("Version")
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, s)
		}
	}
	_, err := io.WriteString(w, render(out))
	return err
}
