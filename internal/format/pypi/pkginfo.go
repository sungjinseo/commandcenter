package pypi

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"path"
	"strings"
)

const (
	maxMetadata = 1 << 20 // cap for the header block of METADATA / PKG-INFO
	maxEntries  = 10000   // cap for archive entries inspected
)

// maxDecompressed caps the decompressed bytes read from an sdist (gzip bombs).
var maxDecompressed int64 = 64 << 20

// readRequiresPython returns the Requires-Python header of a distribution's
// metadata: a wheel/zip's *.dist-info/METADATA or an sdist's <name>-<ver>/PKG-INFO.
// It returns "" when the file is not a readable archive or has no such header.
func readRequiresPython(r io.ReaderAt, size int64, filename string) (string, error) {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".whl"):
		return fromZip(r, size, func(n string) bool {
			dir, base := path.Split(n)
			return base == "METADATA" && strings.Count(dir, "/") == 1 && strings.HasSuffix(dir, ".dist-info/")
		})
	case strings.HasSuffix(lower, ".zip"):
		return fromZip(r, size, func(n string) bool {
			return strings.Count(n, "/") == 1 && path.Base(n) == "PKG-INFO"
		})
	case strings.HasSuffix(lower, ".tar.gz"):
		return fromTarGz(io.NewSectionReader(r, 0, size))
	}
	return "", nil
}

func fromZip(r io.ReaderAt, size int64, match func(string) bool) (string, error) {
	if !zipDirectoryOK(r, size) {
		return "", nil
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return "", nil
	}
	for i, f := range zr.File {
		if i >= maxEntries {
			break
		}
		if !match(f.Name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", nil
		}
		defer rc.Close()
		return parseRequiresPython(rc)
	}
	return "", nil
}

func fromTarGz(r io.Reader) (string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", nil
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, maxDecompressed))
	for i := 0; i < maxEntries; i++ {
		h, err := tr.Next()
		if err != nil {
			return "", nil
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag == tar.TypeReg && strings.Count(name, "/") == 1 && path.Base(name) == "PKG-INFO" {
			return parseRequiresPython(tr)
		}
	}
	return "", nil
}

// parseRequiresPython scans the RFC 822 header block of a metadata document,
// which ends at the first blank line. Only that block is read, so a large
// long_description body never matters; a header block over maxMetadata is
// ignored ("" — the caller keeps the form's value), never an error.
func parseRequiresPython(r io.Reader) (string, error) {
	br := bufio.NewReader(io.LimitReader(r, maxMetadata+1))
	var read int
	for {
		line, err := br.ReadString('\n')
		read += len(line)
		if read > maxMetadata {
			return "", nil // header block too large: ignore the file's metadata
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return "", nil // end of headers (or of the document)
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Requires-Python") {
			return strings.TrimSpace(v), nil
		}
		if err != nil {
			return "", nil
		}
	}
}

// maxDirectory caps the bytes of a zip central directory that is parsed.
const maxDirectory = 2 << 20

// zipDirectoryOK reads the end-of-central-directory record (and the zip64 one
// whenever archive/zip would consult it) and reports whether the archive
// claims few enough entries and a small enough directory to be parsed.
// archive/zip allocates a File per directory record up front, so this must run
// before zip.NewReader. The zip64 trigger mirrors readDirectoryEnd.
func zipDirectoryOK(r io.ReaderAt, size int64) bool {
	n := int64(65557)
	if size < n {
		n = size
	}
	tail := make([]byte, n)
	if _, err := r.ReadAt(tail, size-n); err != nil && err != io.EOF {
		return false
	}
	i := -1
	for j := len(tail) - 22; j >= 0; j-- {
		if bytes.Equal(tail[j:j+4], []byte("PK\x05\x06")) {
			i = j
			break
		}
	}
	if i < 0 {
		return false
	}
	eocd := tail[i:]
	if int(binary.LittleEndian.Uint16(eocd[20:]))+22 > len(eocd) {
		return false
	}
	count := uint64(binary.LittleEndian.Uint16(eocd[10:]))
	dirSize := uint64(binary.LittleEndian.Uint32(eocd[12:]))
	dirOff := uint64(binary.LittleEndian.Uint32(eocd[16:]))
	end := uint64(size-n) + uint64(i) // start of the record that ends the directory
	if count == 0xffff || dirSize == 0xffff || dirOff == 0xffffffff {
		// archive/zip would read the zip64 record; so must we, and never trust
		// the 16-bit values when it is missing or invalid.
		if i < 20 || !bytes.Equal(tail[i-20:i-16], []byte("PK\x06\x07")) ||
			binary.LittleEndian.Uint32(tail[i-16:]) != 0 || binary.LittleEndian.Uint32(tail[i-4:]) != 1 {
			return false
		}
		z64 := binary.LittleEndian.Uint64(tail[i-12:])
		if size < 56 || z64 > uint64(size)-56 {
			return false
		}
		rec := make([]byte, 56)
		if _, err := r.ReadAt(rec, int64(z64)); err != nil || !bytes.Equal(rec[:4], []byte("PK\x06\x06")) {
			return false
		}
		count = binary.LittleEndian.Uint64(rec[32:])
		dirSize = binary.LittleEndian.Uint64(rec[40:])
		dirOff = binary.LittleEndian.Uint64(rec[48:])
		end = z64
	}
	return count <= maxEntries && dirSize <= maxDirectory && dirOff <= end && end-dirOff <= maxDirectory
}
