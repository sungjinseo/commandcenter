package yum

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

const (
	ctXML  = "application/xml"
	ctGzip = "application/gzip"
	ctRPM  = "application/x-rpm"
)

// primaryFileRE selects the files primary.xml lists (createrepo's rule):
// everything under /etc, sendmail, and anything in a bin directory. The
// rest is only in filelists.xml.
var primaryFileRE = regexp.MustCompile(`^/etc/|^/usr/lib/sendmail$|bin/`)

// xw writes XML text with escaping.
type xw struct{ strings.Builder }

func (w *xw) text(s string) { xml.EscapeText(&w.Builder, []byte(s)) }

// attr writes ` name="value"`.
func (w *xw) attr(name, value string) {
	w.WriteString(" " + name + `="`)
	w.text(value)
	w.WriteByte('"')
}

// elem writes <name>text</name> on its own line, indented; nothing when
// text is "" and omitEmpty is set.
func (w *xw) elem(indent, name, text string, omitEmpty bool) {
	if text == "" && omitEmpty {
		return
	}
	w.WriteString(indent + "<" + name + ">")
	w.text(text)
	w.WriteString("</" + name + ">\n")
}

func (w *xw) version(indent string, p *Package) {
	w.WriteString(indent + "<version")
	w.attr("epoch", p.Epoch)
	w.attr("ver", p.Version)
	w.attr("rel", p.Release)
	w.WriteString("/>\n")
}

// depList writes one <rpm:provides> (or requires, ...) block.
func (w *xw) depList(name string, deps []Dep) {
	if len(deps) == 0 {
		return
	}
	w.WriteString("    <rpm:" + name + ">\n")
	for _, d := range deps {
		w.WriteString("      <rpm:entry")
		w.attr("name", d.Name)
		if d.Flags != "" {
			w.attr("flags", d.Flags)
			w.attr("epoch", d.Epoch)
			w.attr("ver", d.Ver)
			if d.Rel != "" {
				w.attr("rel", d.Rel)
			}
		}
		if d.Pre {
			w.attr("pre", "1")
		}
		w.WriteString("/>\n")
	}
	w.WriteString("    </rpm:" + name + ">\n")
}

// pkgOpen writes the <package> start tag of filelists and other.
func (w *xw) pkgOpen(p *Package) {
	w.WriteString("<package")
	w.attr("pkgid", p.Checksum)
	w.attr("name", p.Name)
	w.attr("arch", p.Arch)
	w.WriteString(">\n")
	w.version("  ", p)
}

// Primary renders primary.xml of pkgs; hrefs are relative to the directory
// that holds repodata/.
func Primary(pkgs []*Package) string {
	var w xw
	w.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="` + strconv.Itoa(len(pkgs)) + `">` + "\n")
	for _, p := range pkgs {
		w.WriteString(`<package type="rpm">` + "\n")
		w.elem("  ", "name", p.Name, false)
		w.elem("  ", "arch", p.Arch, false)
		w.version("  ", p)
		w.WriteString(`  <checksum type="sha256" pkgid="YES">` + p.Checksum + "</checksum>\n")
		w.elem("  ", "summary", p.Summary, false)
		w.elem("  ", "description", p.Description, false)
		w.elem("  ", "packager", p.Packager, false)
		w.elem("  ", "url", p.URL, false)
		w.WriteString(`  <time file="` + strconv.FormatInt(p.Uploaded, 10) + `" build="` + strconv.FormatInt(p.BuildTime, 10) + `"/>` + "\n")
		w.WriteString(`  <size package="` + strconv.FormatInt(p.SizePackage, 10) + `" installed="` + strconv.FormatInt(p.SizeInstalled, 10) + `" archive="` + strconv.FormatInt(p.SizeArchive, 10) + `"/>` + "\n")
		w.WriteString("  <location")
		w.attr("href", p.Location)
		w.WriteString("/>\n  <format>\n")
		w.elem("    ", "rpm:license", p.License, false)
		w.elem("    ", "rpm:vendor", p.Vendor, false)
		w.elem("    ", "rpm:group", p.Group, false)
		w.elem("    ", "rpm:buildhost", p.BuildHost, false)
		w.elem("    ", "rpm:sourcerpm", p.SourceRPM, false)
		w.WriteString(`    <rpm:header-range start="` + strconv.FormatInt(p.HeaderStart, 10) + `" end="` + strconv.FormatInt(p.HeaderEnd, 10) + `"/>` + "\n")
		w.depList("provides", p.Provides)
		w.depList("requires", p.Requires)
		w.depList("conflicts", p.Conflicts)
		w.depList("obsoletes", p.Obsoletes)
		for _, f := range p.Files {
			if f.Type == "" && primaryFileRE.MatchString(f.Path) {
				w.elem("    ", "file", f.Path, false)
			}
		}
		w.WriteString("  </format>\n</package>\n")
	}
	w.WriteString("</metadata>\n")
	return w.String()
}

// Filelists renders filelists.xml: every file of every package.
func Filelists(pkgs []*Package) string {
	var w xw
	w.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<filelists xmlns="http://linux.duke.edu/metadata/filelists" packages="` + strconv.Itoa(len(pkgs)) + `">` + "\n")
	for _, p := range pkgs {
		w.pkgOpen(p)
		for _, f := range p.Files {
			w.WriteString("  <file")
			if f.Type != "" {
				w.attr("type", f.Type)
			}
			w.WriteString(">")
			w.text(f.Path)
			w.WriteString("</file>\n")
		}
		w.WriteString("</package>\n")
	}
	w.WriteString("</filelists>\n")
	return w.String()
}

// Other renders other.xml: the changelogs.
func Other(pkgs []*Package) string {
	var w xw
	w.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<otherdata xmlns="http://linux.duke.edu/metadata/other" packages="` + strconv.Itoa(len(pkgs)) + `">` + "\n")
	for _, p := range pkgs {
		w.pkgOpen(p)
		for _, c := range p.Changelog {
			w.WriteString("  <changelog")
			w.attr("author", c.Author)
			w.attr("date", strconv.FormatInt(c.Date, 10))
			w.WriteString(">")
			w.text(c.Text)
			w.WriteString("</changelog>\n")
		}
		w.WriteString("</package>\n")
	}
	w.WriteString("</otherdata>\n")
	return w.String()
}

// repoData is one <data> of repomd.xml.
type repoData struct {
	typ            string
	gz, open       []byte
	sum, openSum   string
	size, openSize int64
}

func newRepoData(typ, open string) (repoData, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf) // no name or mtime: the same XML gives the same bytes
	gz.Write([]byte(open))
	if err := gz.Close(); err != nil {
		return repoData{}, err
	}
	return repoData{typ: typ, gz: buf.Bytes(), open: []byte(open), sum: sha256Hex(buf.Bytes()), openSum: sha256Hex([]byte(open)), size: int64(buf.Len()), openSize: int64(len(open))}, nil
}

func (d repoData) file() string { return "repodata/" + d.sum + "-" + d.typ + ".xml.gz" }

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Repomd renders repomd.xml for the data files.
func Repomd(data []repoData, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	var w xw
	w.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<repomd xmlns="http://linux.duke.edu/metadata/repo" xmlns:rpm="http://linux.duke.edu/metadata/rpm">` + "\n  <revision>" + ts + "</revision>\n")
	for _, d := range data {
		w.WriteString(`  <data type="` + d.typ + `">` + "\n")
		w.WriteString(`    <checksum type="sha256">` + d.sum + "</checksum>\n")
		w.WriteString(`    <open-checksum type="sha256">` + d.openSum + "</open-checksum>\n")
		w.WriteString(`    <location href="` + d.file() + `"/>` + "\n")
		w.WriteString("    <timestamp>" + ts + "</timestamp>\n")
		w.WriteString("    <size>" + strconv.FormatInt(d.size, 10) + "</size>\n")
		w.WriteString("    <open-size>" + strconv.FormatInt(d.openSize, 10) + "</open-size>\n")
		w.WriteString("  </data>\n")
	}
	w.WriteString("</repomd>\n")
	return w.String()
}

// repoDir is the directory of rp that holds repodata/ for the file at
// path: "" (the root) at depth 0, the first segment at depth 1. ok is
// false when path is not under a repodata directory (depth 1, a file at
// the root).
func repoDir(rp *repo.Repository, path string) (dir string, ok bool) {
	if rp.Depth == 0 {
		return "", true
	}
	dir, _, ok = strings.Cut(path, "/")
	return dir + "/", ok
}

// packagesOf lists the rows of rp whose files are under dir (repoDir),
// in name then version order.
func packagesOf(ctx context.Context, tx *meta.Tx, rp *repo.Repository, dir string) ([]*Package, error) {
	names, err := tx.ListPackageNames(ctx, rp.Name)
	if err != nil {
		return nil, err
	}
	var out []*Package
	for _, name := range names {
		if rp.Depth == 1 && !strings.HasPrefix(name, dir) {
			continue
		}
		rows, err := tx.ListPackages(ctx, rp.Name, name)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			var p Package
			if err := json.Unmarshal([]byte(row.Attrs), &p); err != nil {
				return nil, fmt.Errorf("package %s %s of %q: %w", row.Name, row.Version, rp.Name, err)
			}
			out = append(out, &p)
		}
	}
	return out, nil
}

// rebuild writes dir's repodata from rp's rows, in the transaction that
// changed them: the three sha256-named data files, repomd.xml, and the
// previous data files removed. The hrefs are relative to dir.
func (h *Handler) rebuild(ctx context.Context, tx *meta.Tx, rp *repo.Repository, dir string) error {
	pkgs, err := packagesOf(ctx, tx, rp, dir)
	if err != nil {
		return err
	}
	for _, p := range pkgs {
		p.Location = strings.TrimPrefix(p.Location, dir)
	}
	var data []repoData
	for _, d := range []struct{ typ, xml string }{{"primary", Primary(pkgs)}, {"filelists", Filelists(pkgs)}, {"other", Other(pkgs)}} {
		rd, err := newRepoData(d.typ, d.xml)
		if err != nil {
			return err
		}
		data = append(data, rd)
	}
	keep := map[string]bool{}
	for _, d := range data {
		keep[dir+d.file()] = true
		sums, err := h.Store.WriteBlob(rp.Name, bytes.NewReader(d.gz), nil)
		if err != nil {
			return err
		}
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, dir+d.file(), ctGzip, sums)); err != nil {
			return err
		}
	}
	sums, err := h.Store.WriteBlob(rp.Name, strings.NewReader(Repomd(data, h.Now())), nil)
	if err != nil {
		return err
	}
	if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, dir+"repodata/repomd.xml", ctXML, sums)); err != nil {
		return err
	}
	old, err := tx.ListAssets(ctx, rp.Name, dir+"repodata/")
	if err != nil {
		return err
	}
	for _, a := range old {
		if keep[a.Path] || a.Path == dir+"repodata/repomd.xml" {
			continue
		}
		if _, err := tx.DeleteAsset(ctx, rp.Name, a.Path); err != nil && !errors.Is(err, meta.ErrNotFound) {
			return err
		}
	}
	return nil
}
