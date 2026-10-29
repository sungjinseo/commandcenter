// Package apt implements Debian/Ubuntu package repositories (Nexus "apt"):
// .deb files in pool/ and the dists/<distribution>/ metadata apt reads. A
// hosted repository takes .deb uploads, reads their control file and keeps
// Release, Packages and, with a signing key, Release.gpg and InRelease
// itself (regenerated in the transaction of every upload and delete); a
// proxy caches whatever its remote serves, dists/ as metadata and the rest
// as content; a group answers from the first member that has the path.
package apt

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"

	"commandcenter/internal/blob"
	"commandcenter/internal/repo"
	"commandcenter/internal/xz"
	"commandcenter/internal/zstd"
)

const (
	maxControl    = 1 << 20  // bound of a control file
	maxControlTar = 64 << 20 // bound of the unpacked control archive read looking for it
	ctDeb         = "application/vnd.debian.binary-package"
)

func badDeb(format string, args ...any) error {
	return repo.Errorf(http.StatusBadRequest, "deb: "+format, args...)
}

var (
	packageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
	versionRE = regexp.MustCompile(`^[0-9][A-Za-z0-9.+~:-]*$`)
	archRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	fieldRE   = regexp.MustCompile(`^[!-9;-~]+$`) // RFC 822 field name: printable ASCII but ":"
)

// fields is a control stanza in file order: [name, value] pairs, a
// multi-line value holding its continuation lines ("\n" + " text").
type fields [][2]string

func (f fields) get(name string) string {
	for _, kv := range f {
		if strings.EqualFold(kv[0], name) {
			return kv[1]
		}
	}
	return ""
}

// row is what a hosted package row stores (Attrs): the control fields and
// what the Packages stanza adds. Version is the Debian version; the row's
// key version is "<version>_<architecture>".
type row struct {
	Package      string `json:"package"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
	Component    string `json:"component"`
	Filename     string `json:"filename"`
	Size         int64  `json:"size"`
	MD5          string `json:"md5"`
	SHA1         string `json:"sha1"`
	SHA256       string `json:"sha256"`
	Fields       fields `json:"fields"`
}

func (r row) key() string { return r.Version + "_" + r.Architecture }

func newRow(f fields, component string, sums blob.Sums) row {
	r := row{Package: f.get("Package"), Version: f.get("Version"), Architecture: f.get("Architecture"), Component: component,
		Size: sums.Size, MD5: sums.MD5, SHA1: sums.SHA1, SHA256: sums.SHA256, Fields: f}
	r.Filename = poolPath(component, r.Package, r.Version, r.Architecture)
	return r
}

func parseRow(attrs string) (row, error) {
	var r row
	err := json.Unmarshal([]byte(attrs), &r)
	return r, err
}

// poolPath is where a package's file lives, as Debian lays the pool out:
// pool/<component>/<prefix>/<package>/<package>_<version>_<arch>.deb, the
// prefix the first letter, or "lib" plus one for library packages, and
// the version's ":" written "%3a" as dpkg names the file.
func poolPath(component, pkg, version, arch string) string {
	prefix := pkg[:1]
	if strings.HasPrefix(pkg, "lib") && len(pkg) > 3 {
		prefix = pkg[:4]
	}
	return "pool/" + component + "/" + prefix + "/" + pkg + "/" + pkg + "_" + strings.ReplaceAll(version, ":", "%3a") + "_" + arch + ".deb"
}

// readDeb reads the control file out of a .deb: an ar archive whose first
// member is debian-binary ("2.0") and whose second is the control archive,
// a tar holding "control", uncompressed or compressed with gzip, xz (what
// Debian's dpkg-deb writes), zstd (Ubuntu's, since 21.10), bzip2 or lzma.
// What it decompresses to is bounded by maxControlTar.
func readDeb(r io.Reader) (fields, error) {
	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil || string(magic[:]) != "!<arch>\n" {
		return nil, badDeb("not a Debian package (ar archive)")
	}
	for i := 0; ; i++ {
		name, size, err := arHeader(r)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			if name != "debian-binary" || size > 64 {
				return nil, badDeb("first member is %q, not debian-binary", name)
			}
			v, err := io.ReadAll(io.LimitReader(r, size+size%2))
			if err != nil || !strings.HasPrefix(string(v), "2.") {
				return nil, badDeb("debian-binary is not 2.0")
			}
			continue
		}
		base, ext, _ := strings.Cut(name, ".tar")
		if base != "control" {
			return nil, badDeb("second member is %q, not control.tar", name)
		}
		var body io.Reader = io.LimitReader(r, size)
		switch ext {
		case "":
		case ".gz":
			body, err = gzip.NewReader(body)
		case ".xz":
			body, err = xz.NewReader(body, xz.Options{})
		case ".zst":
			body, err = zstd.NewReader(body, zstd.Options{})
		case ".bz2":
			body = bzip2.NewReader(body)
		case ".lzma":
			body, err = xz.NewLZMAReader(body, xz.Options{})
		default:
			return nil, badDeb("control archive compression %q is not supported (gzip, xz, zstd, bzip2 and lzma are)", ext)
		}
		if err != nil {
			return nil, badDeb("control.tar%s: %v", ext, err)
		}
		return readControlTar(body)
	}
}

// arHeader reads one 60-byte ar member header: the name (GNU ar ends it
// with "/") and the size.
func arHeader(r io.Reader) (name string, size int64, err error) {
	var hdr [60]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", 0, badDeb("truncated archive")
	}
	if string(hdr[58:60]) != "`\n" {
		return "", 0, badDeb("malformed archive member header")
	}
	name = strings.TrimSuffix(strings.TrimRight(string(hdr[0:16]), " "), "/")
	size, err = strconv.ParseInt(strings.TrimSpace(string(hdr[48:58])), 10, 64)
	if err != nil || size < 0 {
		return "", 0, badDeb("malformed archive member size")
	}
	return name, size, nil
}

func readControlTar(r io.Reader) (fields, error) {
	lr := &io.LimitedReader{R: r, N: maxControlTar}
	tr := tar.NewReader(lr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF || lr.N == 0 {
			return nil, badDeb("no control file in the control archive")
		}
		if err != nil {
			return nil, badDeb("control archive is not a tar: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg || path.Clean(hdr.Name) != "control" {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxControl+1))
		if err != nil {
			return nil, badDeb("reading control: %v", err)
		}
		if len(b) > maxControl {
			return nil, badDeb("control is larger than %d bytes", maxControl)
		}
		return parseControl(b)
	}
}

// parseControl reads the first stanza of a control file (RFC 822 style:
// "Name: value", a line starting with a space or tab continuing the value
// before it, a blank line ending the stanza) and checks the fields a
// package needs.
func parseControl(b []byte) (fields, error) {
	var out fields
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		switch {
		case strings.TrimSpace(line) == "":
			if len(out) > 0 {
				return checkFields(out)
			}
		case line[0] == ' ' || line[0] == '\t':
			if len(out) == 0 {
				return nil, badDeb("control starts with a continuation line")
			}
			out[len(out)-1][1] += "\n" + strings.TrimRight(line, " \t")
		case line[0] == '#':
		default:
			name, value, ok := strings.Cut(line, ":")
			if !ok || !fieldRE.MatchString(name) {
				return nil, badDeb("control line %q is not a field", line)
			}
			if out.get(name) != "" {
				return nil, badDeb("control field %s is repeated", name)
			}
			out = append(out, [2]string{name, strings.TrimSpace(value)})
		}
	}
	return checkFields(out)
}

func checkFields(f fields) (fields, error) {
	if len(f) == 0 {
		return nil, badDeb("control is empty")
	}
	for _, c := range []struct {
		name string
		re   *regexp.Regexp
	}{{"Package", packageRE}, {"Version", versionRE}, {"Architecture", archRE}} {
		if v := f.get(c.name); !c.re.MatchString(v) {
			return nil, badDeb("control field %s %q is missing or invalid", c.name, v)
		}
	}
	return f, nil
}

// uploadErr maps a body read error to its status.
func uploadErr(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return repo.Errorf(http.StatusRequestEntityTooLarge, "package is larger than the upload limit")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return badDeb("upload ended early: %v", err)
	}
	return fmt.Errorf("reading package: %w", err)
}

// stanza is the package's Packages entry: its control fields but
// Description, then Filename, Size and the checksums, then Description,
// as dpkg-scanpackages orders them; it ends with a newline.
func (r row) stanza() string {
	var sb strings.Builder
	for _, kv := range r.Fields {
		if !strings.EqualFold(kv[0], "Description") {
			sb.WriteString(kv[0] + ": " + kv[1] + "\n")
		}
	}
	sb.WriteString("Filename: " + r.Filename + "\nSize: " + strconv.FormatInt(r.Size, 10) + "\nMD5sum: " + r.MD5 + "\nSHA1: " + r.SHA1 + "\nSHA256: " + r.SHA256 + "\n")
	if d := r.Fields.get("Description"); d != "" {
		sb.WriteString("Description: " + d + "\n")
	}
	return sb.String()
}

// gzipBytes is b gzip-compressed without a timestamp, so the same
// Packages gives the same Packages.gz.
func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(b)
	gz.Close()
	return buf.Bytes()
}
