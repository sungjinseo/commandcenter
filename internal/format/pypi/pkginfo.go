package pypi

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"commandcenter/internal/repo"
	"commandcenter/internal/zipdir"
)

const (
	maxMetadata     = 1 << 20  // cap for the header block of METADATA / PKG-INFO
	maxCoreMetadata = 16 << 20 // cap for the whole METADATA / PKG-INFO document (kept as the PEP 658 sidecar)
	maxEntries      = 10000    // cap for zip entries: archive/zip allocates for each up front
)

// maxDecompressed caps the decompressed bytes read from an sdist (gzip bombs).
// Build backends such as hatchling, flit and poetry write PKG-INFO last.
var maxDecompressed int64 = 512 << 20

// distMeta holds the core metadata fields an upload is checked against.
type distMeta struct{ Name, Version, RequiresPython string }

// readMetadata reads the core metadata of an uploaded distribution of
// project at version: the METADATA of a wheel's only *.dist-info directory,
// or an sdist's (.tar.gz or .zip) first top-level PKG-INFO, which must be in
// the <name>-<version>/ directory of this release. raw is that whole
// document (at most maxCoreMetadata bytes), served as the file's PEP 658
// sidecar. Errors are 400s: the file is not a readable archive of its kind,
// lacks that file, or the file has no Name or Version. Nothing read depends
// on a size the archive claims.
func readMetadata(r io.ReaderAt, size int64, filename, project, version string) (md distMeta, raw []byte, err error) {
	lower := strings.ToLower(filename)
	var dir, name string
	switch {
	case strings.HasSuffix(lower, ".whl"):
		raw, name, err = fromWheel(r, size)
	case strings.HasSuffix(lower, ".tar.gz"):
		raw, name, dir, err = fromTarGz(io.NewSectionReader(r, 0, size))
	default:
		raw, name, dir, err = fromZip(r, size)
	}
	if err != nil {
		return distMeta{}, nil, err
	}
	if md, err = parseMetadata(bytes.NewReader(raw), name); err != nil {
		return distMeta{}, nil, err
	}
	if dir == "" { // a wheel
		return md, raw, nil
	}
	i := -1
	if len(dir) <= 255 { // a file name's limit; sdistSplit is quadratic in it
		i = sdistSplit(project, dir)
	}
	if i < 0 || canonicalVersion(dir[i+1:]) != canonicalVersion(version) {
		return distMeta{}, nil, badDist("PKG-INFO is in %.100q, not in the %s-%s/ directory", dir, project, version)
	}
	return md, raw, nil
}

// fromWheel reads the METADATA of a wheel's only .dist-info directory.
func fromWheel(r io.ReaderAt, size int64) (raw []byte, name string, err error) {
	zr, err := openZip(r, size)
	if err != nil {
		return nil, "", err
	}
	var dir string
	var md *zip.File
	for _, f := range zr.File {
		top, rest, ok := strings.Cut(f.Name, "/")
		if !ok || !strings.HasSuffix(top, ".dist-info") {
			continue
		}
		if dir != "" && top != dir {
			return nil, "", badDist("more than one .dist-info directory (%.100q, %.100q)", dir, top)
		}
		dir = top
		if rest == "METADATA" {
			if md != nil {
				return nil, "", badDist("more than one %.100q", f.Name)
			}
			md = f
		}
	}
	if md == nil {
		return nil, "", badDist("no *.dist-info/METADATA")
	}
	raw, err = fromZipFile(md)
	return raw, md.Name, err
}

// fromZip reads a zip sdist's first top-level PKG-INFO and returns its directory.
func fromZip(r io.ReaderAt, size int64) (raw []byte, name, dir string, err error) {
	zr, err := openZip(r, size)
	if err != nil {
		return nil, "", "", err
	}
	for _, f := range zr.File {
		if dir, rest, ok := strings.Cut(f.Name, "/"); ok && rest == "PKG-INFO" {
			raw, err := fromZipFile(f)
			return raw, f.Name, dir, err
		}
	}
	return nil, "", "", badDist("no PKG-INFO in a <name>-<version>/ directory")
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

func fromZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, badDist("cannot read %.100q: %v", f.Name, err)
	}
	defer rc.Close()
	return readDocument(rc, f.Name)
}

// readDocument reads a metadata document of at most maxCoreMetadata bytes.
func readDocument(r io.Reader, name string) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxCoreMetadata+1))
	if err != nil {
		return nil, badDist("cannot read %.100q: %v", name, err)
	}
	if len(raw) > maxCoreMetadata {
		return nil, badDist("%.100q is over %d MiB", name, maxCoreMetadata>>20)
	}
	return raw, nil
}

func fromTarGz(r io.Reader) (raw []byte, name, dir string, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, "", "", badDist("not a gzip file")
	}
	defer gz.Close()
	lr := &io.LimitedReader{R: gz, N: maxDecompressed}
	tr := tar.NewReader(lr)
	for {
		h, err := tr.Next()
		switch {
		case lr.N <= 0:
			return nil, "", "", badDist("no PKG-INFO in the first %d MiB", maxDecompressed>>20)
		case err == io.EOF:
			return nil, "", "", badDist("no PKG-INFO in a <name>-<version>/ directory")
		case err != nil:
			return nil, "", "", badDist("not a tar archive: %v", err)
		}
		dir, rest, ok := strings.Cut(strings.TrimPrefix(h.Name, "./"), "/")
		if ok && rest == "PKG-INFO" && h.Typeflag == tar.TypeReg {
			raw, err := readDocument(tr, h.Name)
			return raw, h.Name, dir, err
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
