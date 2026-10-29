package yum

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"commandcenter/internal/repo"
)

// The RPM file format (rpm.org "Package File Format"): a 96-byte lead, the
// signature header, padding to 8 bytes, the main header, the payload. Both
// headers are "header structures": magic 8E AD E8 01, 4 reserved bytes, the
// index entry count and the data size, then the entries (tag, type, offset,
// count; 16 bytes) and the data.

const (
	leadSize    = 96
	maxEntries  = 1 << 16 // rpm refuses more
	maxHdrBytes = 64 << 20

	// Entry types.
	typeInt16  = 3
	typeInt32  = 4
	typeInt64  = 5
	typeString = 6
	typeArray  = 8 // STRING_ARRAY
	typeI18N   = 9 // I18NSTRING: one string per locale, the first is the default

	// Main header tags (rpmtag.h).
	tagName           = 1000
	tagVersion        = 1001
	tagRelease        = 1002
	tagEpoch          = 1003
	tagSummary        = 1004
	tagDescription    = 1005
	tagBuildTime      = 1006
	tagBuildHost      = 1007
	tagSize           = 1009
	tagVendor         = 1011
	tagLicense        = 1014
	tagPackager       = 1015
	tagGroup          = 1016
	tagURL            = 1020
	tagArch           = 1022
	tagFileModes      = 1030
	tagFileFlags      = 1037
	tagSourceRPM      = 1044
	tagArchiveSize    = 1046
	tagProvideName    = 1047
	tagRequireFlags   = 1048
	tagRequireName    = 1049
	tagRequireVer     = 1050
	tagConflictFlags  = 1053
	tagConflictName   = 1054
	tagConflictVer    = 1055
	tagChangeTime     = 1080
	tagChangeName     = 1081
	tagChangeText     = 1082
	tagObsoleteName   = 1090
	tagSourcePackage  = 1106
	tagProvideFlags   = 1112
	tagProvideVer     = 1113
	tagObsoleteFlags  = 1114
	tagObsoleteVer    = 1115
	tagDirIndexes     = 1116
	tagBasenames      = 1117
	tagDirnames       = 1118
	tagLongSize       = 5009
	sigTagPayload     = 1007 // signature header: the uncompressed payload size
	sigTagLongPayload = 271

	// Dependency flags (rpmds.h); the sense bits name the comparison.
	senseLess    = 1 << 1
	senseGreater = 1 << 2
	senseEqual   = 1 << 3
	sensePre     = 0x40 | 0x200 | 0x400 // PREREQ, SCRIPT_PRE, SCRIPT_POST: pre="1"

	fileFlagGhost = 1 << 6
	modeDir       = 0o040000
)

var headerMagic = [4]byte{0x8e, 0xad, 0xe8, 0x01}

func badRPM(format string, args ...any) error {
	return repo.Errorf(http.StatusBadRequest, "rpm: "+format, args...)
}

// Package is what the repodata says of one RPM: its header fields, as the
// primary, filelists and other documents print them. It is the row's Attrs.
type Package struct {
	Name        string `json:"name"`
	Epoch       string `json:"epoch"`
	Version     string `json:"version"`
	Release     string `json:"release"`
	Arch        string `json:"arch"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Packager    string `json:"packager,omitempty"`
	URL         string `json:"url,omitempty"`
	BuildTime   int64  `json:"buildTime"`
	BuildHost   string `json:"buildHost,omitempty"`
	License     string `json:"license,omitempty"`
	Vendor      string `json:"vendor,omitempty"`
	Group       string `json:"group,omitempty"`
	SourceRPM   string `json:"sourceRpm,omitempty"`
	// Sizes: the file, the installed files (SIZE), the uncompressed payload.
	SizePackage   int64 `json:"sizePackage"`
	SizeInstalled int64 `json:"sizeInstalled"`
	SizeArchive   int64 `json:"sizeArchive"`
	// HeaderStart/End are the byte range of the main header in the file.
	HeaderStart int64 `json:"headerStart"`
	HeaderEnd   int64 `json:"headerEnd"`
	// Checksum is the sha256 of the whole file (the pkgid); Location the
	// stored path; Uploaded the upload time (<time file>).
	Checksum  string      `json:"checksum"`
	Location  string      `json:"location"`
	Uploaded  int64       `json:"uploaded"`
	Provides  []Dep       `json:"provides,omitempty"`
	Requires  []Dep       `json:"requires,omitempty"`
	Conflicts []Dep       `json:"conflicts,omitempty"`
	Obsoletes []Dep       `json:"obsoletes,omitempty"`
	Files     []File      `json:"files,omitempty"`
	Changelog []Changelog `json:"changelog,omitempty"`
}

// Dep is one rpm:entry: name, the comparison (EQ, LT, GT, LE, GE or none)
// and the version it compares with.
type Dep struct {
	Name  string `json:"name"`
	Flags string `json:"flags,omitempty"`
	Epoch string `json:"epoch,omitempty"`
	Ver   string `json:"ver,omitempty"`
	Rel   string `json:"rel,omitempty"`
	Pre   bool   `json:"pre,omitempty"`
}

// File is one installed path; Type is "dir", "ghost" or "" (a file).
type File struct {
	Path string `json:"path"`
	Type string `json:"type,omitempty"`
}

type Changelog struct {
	Author string `json:"author"`
	Date   int64  `json:"date"`
	Text   string `json:"text"`
}

// NVRA is the file name the package has: name-version-release.arch.rpm.
func (p *Package) NVRA() string {
	return p.Name + "-" + p.Version + "-" + p.Release + "." + p.Arch + ".rpm"
}

// header is one parsed header structure: its entries and data.
type header struct {
	entries map[int32]entry
	data    []byte
}

type entry struct {
	typ, offset, count int32
}

// readHeader reads the header structure at off. It returns it and the
// offset after it (the data's end, before any padding).
func readHeader(r io.ReaderAt, off int64) (*header, int64, error) {
	var pre [16]byte
	if _, err := r.ReadAt(pre[:], off); err != nil {
		return nil, 0, badRPM("header at %d: %v", off, err)
	}
	if [4]byte(pre[:4]) != headerMagic {
		return nil, 0, badRPM("no header magic at offset %d", off)
	}
	n, size := int64(binary.BigEndian.Uint32(pre[8:])), int64(binary.BigEndian.Uint32(pre[12:]))
	if n == 0 || n > maxEntries || size > maxHdrBytes {
		return nil, 0, badRPM("header at %d has %d entries and %d data bytes", off, n, size)
	}
	buf := make([]byte, 16*n+size)
	if _, err := r.ReadAt(buf, off+16); err != nil {
		return nil, 0, badRPM("header at %d is truncated: %v", off, err)
	}
	h := &header{entries: make(map[int32]entry, n), data: buf[16*n:]}
	for i := range n {
		e := buf[16*i:]
		tag := int32(binary.BigEndian.Uint32(e))
		en := entry{typ: int32(binary.BigEndian.Uint32(e[4:])), offset: int32(binary.BigEndian.Uint32(e[8:])), count: int32(binary.BigEndian.Uint32(e[12:]))}
		if en.offset < 0 || int64(en.offset) > size || en.count < 0 {
			return nil, 0, badRPM("tag %d points outside the header", tag)
		}
		h.entries[tag] = en
	}
	return h, off + 16 + 16*n + size, nil
}

// str returns a STRING tag, or the first string of an I18NSTRING or
// STRING_ARRAY tag ("" when absent).
func (h *header) str(tag int32) string {
	s := h.strs(tag)
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// strs returns the strings of a STRING, STRING_ARRAY or I18NSTRING tag.
func (h *header) strs(tag int32) []string {
	e, ok := h.entries[tag]
	if !ok || (e.typ != typeString && e.typ != typeArray && e.typ != typeI18N) {
		return nil
	}
	n := int(e.count)
	if e.typ == typeString {
		n = 1
	}
	out := make([]string, 0, n)
	rest := h.data[e.offset:]
	for range n {
		i := strings.IndexByte(string(rest), 0)
		if i < 0 {
			break
		}
		out = append(out, string(rest[:i]))
		rest = rest[i+1:]
	}
	return out
}

// ints returns the values of an INT16, INT32 or INT64 tag.
func (h *header) ints(tag int32) []int64 {
	e, ok := h.entries[tag]
	if !ok {
		return nil
	}
	var width int
	switch e.typ {
	case typeInt16:
		width = 2
	case typeInt32:
		width = 4
	case typeInt64:
		width = 8
	default:
		return nil
	}
	if int64(e.offset)+int64(e.count)*int64(width) > int64(len(h.data)) {
		return nil
	}
	out := make([]int64, e.count)
	for i := range out {
		b := h.data[int(e.offset)+i*width:]
		switch width {
		case 2:
			out[i] = int64(binary.BigEndian.Uint16(b))
		case 4:
			out[i] = int64(int32(binary.BigEndian.Uint32(b)))
		default:
			out[i] = int64(binary.BigEndian.Uint64(b))
		}
	}
	return out
}

func (h *header) int(tag int32) int64 {
	if v := h.ints(tag); len(v) > 0 {
		return v[0]
	}
	return 0
}

// nameRE is a package name, version or release as rpmbuild accepts them
// (no whitespace, no "/", no "-" in version and release).
var nameRE = regexp.MustCompile(`^[A-Za-z0-9_.+%{}~^][A-Za-z0-9_.+%{}~^-]*$`)

// Parse reads the RPM of size bytes at r.
func Parse(r io.ReaderAt, size int64) (*Package, error) {
	var lead [4]byte
	if _, err := r.ReadAt(lead[:], 0); err != nil || lead != [4]byte{0xed, 0xab, 0xee, 0xdb} {
		return nil, badRPM("not an RPM file (no lead magic)")
	}
	sig, end, err := readHeader(r, leadSize)
	if err != nil {
		return nil, err
	}
	start := (end + 7) &^ 7 // the main header is 8-byte aligned
	h, end, err := readHeader(r, start)
	if err != nil {
		return nil, err
	}
	p := &Package{
		Name: h.str(tagName), Version: h.str(tagVersion), Release: h.str(tagRelease), Arch: h.str(tagArch),
		Epoch:   strconv.FormatInt(h.int(tagEpoch), 10),
		Summary: h.str(tagSummary), Description: h.str(tagDescription), Packager: h.str(tagPackager), URL: h.str(tagURL),
		BuildTime: h.int(tagBuildTime), BuildHost: h.str(tagBuildHost),
		License: h.str(tagLicense), Vendor: h.str(tagVendor), Group: h.str(tagGroup), SourceRPM: h.str(tagSourceRPM),
		SizePackage: size, SizeInstalled: h.int(tagSize), SizeArchive: sig.int(sigTagPayload),
		HeaderStart: start, HeaderEnd: end,
	}
	if _, ok := h.entries[tagSourcePackage]; ok {
		p.Arch = "src"
	}
	if v := h.int(tagLongSize); v != 0 {
		p.SizeInstalled = v
	}
	if v := sig.int(sigTagLongPayload); v != 0 {
		p.SizeArchive = v
	} else if v := h.int(tagArchiveSize); p.SizeArchive == 0 && v != 0 {
		p.SizeArchive = v
	}
	if !nameRE.MatchString(p.Name) || !nameRE.MatchString(p.Version) || !nameRE.MatchString(p.Release) || !nameRE.MatchString(p.Arch) ||
		strings.Contains(p.Version, "-") || strings.Contains(p.Release, "-") || strings.Contains(p.Arch, "-") {
		return nil, badRPM("header needs a name (%q), version (%q), release (%q) and arch (%q)", p.Name, p.Version, p.Release, p.Arch)
	}
	p.Provides = deps(h, tagProvideName, tagProvideFlags, tagProvideVer)
	p.Requires = deps(h, tagRequireName, tagRequireFlags, tagRequireVer)
	p.Conflicts = deps(h, tagConflictName, tagConflictFlags, tagConflictVer)
	p.Obsoletes = deps(h, tagObsoleteName, tagObsoleteFlags, tagObsoleteVer)
	dirs, bases, idx := h.strs(tagDirnames), h.strs(tagBasenames), h.ints(tagDirIndexes)
	modes, flags := h.ints(tagFileModes), h.ints(tagFileFlags)
	for i, b := range bases {
		if i >= len(idx) || idx[i] < 0 || int(idx[i]) >= len(dirs) {
			return nil, badRPM("file %q has no directory", b)
		}
		f := File{Path: dirs[idx[i]] + b}
		switch {
		case i < len(modes) && modes[i]&0o170000 == modeDir:
			f.Type = "dir"
		case i < len(flags) && flags[i]&fileFlagGhost != 0:
			f.Type = "ghost"
		}
		p.Files = append(p.Files, f)
	}
	times, names, texts := h.ints(tagChangeTime), h.strs(tagChangeName), h.strs(tagChangeText)
	for i := range min(len(times), len(names), len(texts)) {
		p.Changelog = append(p.Changelog, Changelog{Author: names[i], Date: times[i], Text: texts[i]})
	}
	return p, nil
}

// deps reads one dependency table (names, flags, versions). rpmlib()
// requirements are rpm's own and left out, as createrepo does.
func deps(h *header, nameTag, flagTag, verTag int32) []Dep {
	names, flags, vers := h.strs(nameTag), h.ints(flagTag), h.strs(verTag)
	var out []Dep
	for i, n := range names {
		if strings.HasPrefix(n, "rpmlib(") {
			continue
		}
		d := Dep{Name: n}
		if i < len(flags) {
			switch flags[i] & (senseLess | senseGreater | senseEqual) {
			case senseEqual:
				d.Flags = "EQ"
			case senseLess:
				d.Flags = "LT"
			case senseGreater:
				d.Flags = "GT"
			case senseLess | senseEqual:
				d.Flags = "LE"
			case senseGreater | senseEqual:
				d.Flags = "GE"
			}
			d.Pre = nameTag == tagRequireName && flags[i]&sensePre != 0
		}
		if i < len(vers) && vers[i] != "" && d.Flags != "" {
			d.Epoch, d.Ver, d.Rel = splitEVR(vers[i])
		}
		out = append(out, d)
	}
	return out
}

// splitEVR splits [epoch:]version[-release]; a missing epoch is "0".
func splitEVR(s string) (epoch, ver, rel string) {
	epoch = "0"
	if e, rest, ok := strings.Cut(s, ":"); ok {
		epoch, s = e, rest
	}
	ver, rel, _ = strings.Cut(s, "-")
	return epoch, ver, rel
}

// uploadErr maps a body read error to its status.
func uploadErr(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return repo.Errorf(http.StatusRequestEntityTooLarge, "package is larger than the upload limit")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return badRPM("upload ended early: %v", err)
	}
	return fmt.Errorf("reading package: %w", err)
}
