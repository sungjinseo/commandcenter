package r

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"commandcenter/internal/blob"
	"commandcenter/internal/component"
	"commandcenter/internal/repo"
)

const (
	ctText = "text/plain; charset=utf-8"
	ctGzip = "application/gzip"
	ctZip  = "application/zip"
	ctRDS  = "application/octet-stream"
	// maxDescription bounds a package's DESCRIPTION file.
	maxDescription = 1 << 20
	// maxDoc bounds a member PACKAGES a group merge reads (CRAN's is ~10 MB).
	maxDoc = 1 << 28
)

// row is a hosted package's row (Attrs): its path, the fields of its
// DESCRIPTION in order, and the file's sums.
type row struct {
	File   string      `json:"file"` // <dir>/<pkg>_<version><ext>
	Dir    string      `json:"dir"`  // the contrib directory, the component's group
	Fields [][2]string `json:"fields"`
	MD5    string      `json:"md5"`
	SHA256 string      `json:"sha256"`
	Size   int64       `json:"size"`
}

func (r row) field(name string) string { return get(r.Fields, name) }

// get is the value of the first field named name (DCF field names are
// case-sensitive, but DESCRIPTIONs are hand-written: match any case).
func get(fields [][2]string, name string) string {
	for _, f := range fields {
		if strings.EqualFold(f[0], name) {
			return f[1]
		}
	}
	return ""
}

func badPackage(format string, args ...any) error {
	return repo.Errorf(http.StatusBadRequest, "r: "+format, args...)
}

func uploadErr(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return repo.Errorf(http.StatusRequestEntityTooLarge, "package is larger than the upload limit")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return badPackage("upload ended early: %v", err)
	}
	return fmt.Errorf("reading package: %w", err)
}

var dcfKeyRE = regexp.MustCompile(`^[^\s:]+$`)

// parseDCF reads one DCF record ("Field: value", continuation lines
// indented) as R's read.dcf does: the fields in order, a continuation
// line joined to its value with "\n". The record ends at the first blank
// line after a field (DESCRIPTION has one record).
func parseDCF(b []byte) ([][2]string, error) {
	var fields [][2]string
	for _, line := range strings.Split(strings.TrimPrefix(string(b), "\xef\xbb\xbf"), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.TrimSpace(line) == "":
			if len(fields) > 0 {
				return fields, nil
			}
		case line[0] == ' ' || line[0] == '\t':
			if len(fields) == 0 {
				return nil, badPackage("DESCRIPTION starts with a continuation line")
			}
			v := strings.TrimSpace(line)
			if v == "." { // a blank line inside a value, as DCF writes it
				v = ""
			}
			fields[len(fields)-1][1] += "\n" + v
		default:
			key, value, ok := strings.Cut(line, ":")
			if !ok || !dcfKeyRE.MatchString(key) {
				return nil, badPackage("DESCRIPTION line %q is not \"Field: value\"", line)
			}
			fields = append(fields, [2]string{key, strings.TrimSpace(value)})
		}
	}
	if len(fields) == 0 {
		return nil, badPackage("DESCRIPTION is empty")
	}
	return fields, nil
}

// parse reads the stored package at p (SplitR) and checks that its
// DESCRIPTION is the file name's: <pkg>/DESCRIPTION of the tar.gz (a
// source or macOS .tgz) or the zip (Windows).
func parse(f blob.File, sums blob.Sums, p string) (row, error) {
	dir, name, version, ok := component.SplitR(p)
	if !ok {
		return row{}, badPackage("upload <contrib dir>/<pkg>_<version>.tar.gz (.zip under bin/windows, .tgz under bin/macosx), not %s", p)
	}
	var (
		b   []byte
		err error
	)
	if component.RExt(dir) == ".zip" {
		b, err = readZip(f, sums.Size, name+"/DESCRIPTION")
	} else {
		b, err = readTarGz(f, name+"/DESCRIPTION")
	}
	if err != nil {
		return row{}, err
	}
	fields, err := parseDCF(b)
	if err != nil {
		return row{}, err
	}
	if got := get(fields, "Package"); got != name {
		return row{}, badPackage("file name %q does not match the package: Package %q", p[len(dir)+1:], got)
	}
	if got := get(fields, "Version"); got != version {
		return row{}, badPackage("file name %q does not match the package: Version %q", p[len(dir)+1:], got)
	}
	return row{File: p, Dir: dir, Fields: fields, MD5: sums.MD5, SHA256: sums.SHA256, Size: sums.Size}, nil
}

// readTarGz returns the entry name of the gzipped tar r holds. R CMD
// build writes "<pkg>/DESCRIPTION" (some tools "./<pkg>/DESCRIPTION").
func readTarGz(r io.Reader, name string) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, badPackage("not a gzip file: %v", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, badPackage("no %s in the package", name)
		}
		if err != nil {
			return nil, badPackage("not a tar archive: %v", err)
		}
		if strings.TrimPrefix(hdr.Name, "./") != name {
			continue
		}
		if hdr.Size > maxDescription {
			return nil, badPackage("%s is larger than %d bytes", name, maxDescription)
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxDescription))
		if err != nil {
			return nil, badPackage("reading %s: %v", name, err)
		}
		return b, nil
	}
}

func readZip(f io.ReaderAt, size int64, name string) ([]byte, error) {
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return nil, badPackage("not a zip file: %v", err)
	}
	zf, err := zr.Open(name)
	if err != nil {
		return nil, badPackage("no %s in the package", name)
	}
	defer zf.Close()
	b, err := io.ReadAll(io.LimitReader(zf, maxDescription+1))
	if err != nil {
		return nil, badPackage("reading %s: %v", name, err)
	}
	if len(b) > maxDescription {
		return nil, badPackage("%s is larger than %d bytes", name, maxDescription)
	}
	return b, nil
}
