package pypi

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"commandcenter/internal/repo"
	"commandcenter/internal/zipdir"
)

const (
	maxMetadata = 1 << 20 // cap for the header block of METADATA / PKG-INFO
	maxEntries  = 10000   // cap for zip entries: archive/zip allocates for each up front
)

// maxDecompressed caps the decompressed bytes read from an sdist (gzip bombs).
// Build backends such as hatchling, flit and poetry write PKG-INFO last.
var maxDecompressed int64 = 512 << 20

// distMeta holds the core metadata fields an upload is checked against.
type distMeta struct{ Name, Version, RequiresPython string }

// readMetadata reads the core metadata of an uploaded distribution of
// project at version: the METADATA of a wheel's only *.dist-info directory,
// or an sdist's (.tar.gz or .zip) first top-level PKG-INFO, which must be in
// the <name>-<version>/ directory of this release. Errors are 400s: the file
// is not a readable archive of its kind, lacks that file, or the file has no
// Name or Version. Nothing read depends on a size the archive claims.
func readMetadata(r io.ReaderAt, size int64, filename, project, version string) (distMeta, error) {
	lower := strings.ToLower(filename)
	if strings.HasSuffix(lower, ".whl") {
		return fromWheel(r, size)
	}
	var md distMeta
	var dir string
	var err error
	if strings.HasSuffix(lower, ".tar.gz") {
		md, dir, err = fromTarGz(io.NewSectionReader(r, 0, size))
	} else {
		md, dir, err = fromZip(r, size)
	}
	if err != nil {
		return distMeta{}, err
	}
	i := -1
	if len(dir) <= 255 { // a file name's limit; sdistSplit is quadratic in it
		i = sdistSplit(project, dir)
	}
	if i < 0 || canonicalVersion(dir[i+1:]) != canonicalVersion(version) {
		return distMeta{}, badDist("PKG-INFO is in %.100q, not in the %s-%s/ directory", dir, project, version)
	}
	return md, nil
}

// fromWheel reads the METADATA of a wheel's only .dist-info directory.
func fromWheel(r io.ReaderAt, size int64) (distMeta, error) {
	zr, err := openZip(r, size)
	if err != nil {
		return distMeta{}, err
	}
	var dir string
	var md *zip.File
	for _, f := range zr.File {
		top, rest, ok := strings.Cut(f.Name, "/")
		if !ok || !strings.HasSuffix(top, ".dist-info") {
			continue
		}
		if dir != "" && top != dir {
			return distMeta{}, badDist("more than one .dist-info directory (%.100q, %.100q)", dir, top)
		}
		dir = top
		if rest == "METADATA" {
			if md != nil {
				return distMeta{}, badDist("more than one %.100q", f.Name)
			}
			md = f
		}
	}
	if md == nil {
		return distMeta{}, badDist("no *.dist-info/METADATA")
	}
	return fromZipFile(md)
}

// fromZip reads a zip sdist's first top-level PKG-INFO and returns its directory.
func fromZip(r io.ReaderAt, size int64) (distMeta, string, error) {
	zr, err := openZip(r, size)
	if err != nil {
		return distMeta{}, "", err
	}
	for _, f := range zr.File {
		if dir, rest, ok := strings.Cut(f.Name, "/"); ok && rest == "PKG-INFO" {
			md, err := fromZipFile(f)
			return md, dir, err
		}
	}
	return distMeta{}, "", badDist("no PKG-INFO in a <name>-<version>/ directory")
}

// openZip opens a zip archive whose central directory is small enough to parse.
func openZip(r io.ReaderAt, size int64) (*zip.Reader, error) {
	if !zipdir.OK(r, size, maxEntries, maxDirectory) {
		return nil, badDist("not a zip archive, or one with over %d entries or a central directory over %d MiB", maxEntries, maxDirectory>>20)
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, badDist("not a zip archive: %v", err)
	}
	return zr, nil
}

func fromZipFile(f *zip.File) (distMeta, error) {
	rc, err := f.Open()
	if err != nil {
		return distMeta{}, badDist("cannot read %.100q: %v", f.Name, err)
	}
	defer rc.Close()
	return parseMetadata(rc, f.Name)
}

func fromTarGz(r io.Reader) (distMeta, string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return distMeta{}, "", badDist("not a gzip file")
	}
	defer gz.Close()
	lr := &io.LimitedReader{R: gz, N: maxDecompressed}
	tr := tar.NewReader(lr)
	for {
		h, err := tr.Next()
		switch {
		case lr.N <= 0:
			return distMeta{}, "", badDist("no PKG-INFO in the first %d MiB", maxDecompressed>>20)
		case err == io.EOF:
			return distMeta{}, "", badDist("no PKG-INFO in a <name>-<version>/ directory")
		case err != nil:
			return distMeta{}, "", badDist("not a tar archive: %v", err)
		}
		dir, rest, ok := strings.Cut(strings.TrimPrefix(h.Name, "./"), "/")
		if ok && rest == "PKG-INFO" && h.Typeflag == tar.TypeReg {
			md, err := parseMetadata(tr, h.Name)
			return md, dir, err
		}
	}
}

// parseMetadata reads Name, Version and Requires-Python from the RFC 822
// header block of a metadata document, which ends at the first blank line.
// Only that block is read, so a large long_description body never matters.
// Reading stops after maxMetadata bytes of headers; a header past that is
// not seen (the caller keeps the form's Requires-Python).
func parseMetadata(r io.Reader, name string) (distMeta, error) {
	br := bufio.NewReader(io.LimitReader(r, maxMetadata+1))
	var md distMeta
	fields := map[string]*string{"name": &md.Name, "version": &md.Version, "requires-python": &md.RequiresPython}
	for read := 0; ; {
		line, err := br.ReadString('\n')
		read += len(line)
		if err != nil && err != io.EOF {
			return distMeta{}, badDist("cannot read %.100q: %v", name, err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || read > maxMetadata {
			break // end of headers (or of the document), or too many
		}
		if k, v, ok := strings.Cut(line, ":"); ok && line[0] != ' ' && line[0] != '\t' { // not a folded line
			if p := fields[strings.ToLower(strings.TrimSpace(k))]; p != nil && *p == "" {
				*p = strings.TrimSpace(v)
			}
		}
		if err == io.EOF {
			break
		}
	}
	if md.Name == "" || md.Version == "" {
		return distMeta{}, badDist("%.100q has no Name or no Version", name)
	}
	return md, nil
}

func badDist(format string, a ...any) error {
	return repo.Errorf(http.StatusBadRequest, "invalid distribution file: "+format, a...)
}

// maxDirectory caps the bytes of a zip central directory that is parsed.
const maxDirectory = 2 << 20
