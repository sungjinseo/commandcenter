package conda

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"commandcenter/internal/blob"
	"commandcenter/internal/component"
	"commandcenter/internal/repo"
)

const (
	ctJSON  = "application/json"
	ctBzip2 = "application/x-bzip2"
	ctZip   = "application/zip" // a .conda is a zip
	// maxInfo bounds info/index.json, info/about.json and metadata.json.
	maxInfo = 1 << 20
)

// row is a hosted package's row (Attrs): the repodata entry of the file
// and, for a .tar.bz2, its about.json, for channeldata and the metadata
// API. Numbers are json.Number, so index.json's timestamp and build_number
// come out as they went in.
type row struct {
	File   string         `json:"file"`   // <name>-<version>-<build>.tar.bz2 or .conda
	Subdir string         `json:"subdir"` // the directory the file is in
	Index  map[string]any `json:"index"`  // the repodata entry: index.json fields, md5, sha256, size
	About  map[string]any `json:"about,omitempty"`
}

func (r row) path() string { return r.Subdir + "/" + r.File }

func (r row) str(key string) string {
	s, _ := r.Index[key].(string)
	return s
}

// version is the component's version, <version>-<build>.
func (r row) version() string { return r.str("version") + "-" + r.str("build") }

// decodeJSON is json.Unmarshal with numbers kept as json.Number.
func decodeJSON(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v)
}

func badPackage(format string, args ...any) error {
	return repo.Errorf(http.StatusBadRequest, "conda: "+format, args...)
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

// parse reads the stored package at subdir/file: a .tar.bz2's info/index.json
// and info/about.json, or a .conda's metadata.json (its index is in a zstd
// tarball the standard library cannot read: the entry then has what the
// file name gives, and no depends).
func parse(f blob.File, sums blob.Sums, subdir, file string) (row, error) {
	r := row{File: file, Subdir: subdir}
	var err error
	if strings.HasSuffix(file, ".conda") {
		err = readConda(f, sums.Size, strings.TrimSuffix(file, ".conda"))
		r.Index = map[string]any{}
	} else {
		r.Index, r.About, err = readInfo(bzip2.NewReader(f))
	}
	if err != nil {
		return row{}, err
	}
	return r.finish(sums)
}

// finish completes the repodata entry of r from its file name and sums:
// the name, version and build the file name gives must be the index's
// (as yum requires of an RPM's name) and fill a .conda's; so must the
// subdir; md5, sha256 and size are the file's.
func (r row) finish(sums blob.Sums) (row, error) {
	_, name, version, ok := component.SplitConda(r.path())
	if !ok {
		return row{}, badPackage("upload <subdir>/<name>-<version>-<build>.tar.bz2 or .conda, not %s", r.File)
	}
	ver, build, _ := strings.Cut(version, "-")
	for k, v := range map[string]string{"name": name, "version": ver, "build": build} {
		got, ok := r.Index[k].(string)
		if !ok && strings.HasSuffix(r.File, ".conda") {
			r.Index[k] = v
			continue
		}
		if got != v {
			return row{}, badPackage("file name %q does not match the package: %s %q", r.File, k, got)
		}
	}
	if got, ok := r.Index["subdir"].(string); ok && got != r.Subdir {
		return row{}, badPackage("package is for subdir %q, not %q", got, r.Subdir)
	}
	r.Index["subdir"] = r.Subdir
	if _, ok := r.Index["build_number"]; !ok {
		r.Index["build_number"] = json.Number("0")
	}
	if _, ok := r.Index["depends"].([]any); !ok {
		r.Index["depends"] = []any{}
	}
	r.Index["md5"], r.Index["sha256"], r.Index["size"] = sums.MD5, sums.SHA256, json.Number(fmt.Sprint(sums.Size))
	return r, nil
}

// readInfo reads info/index.json and info/about.json from a package's tar
// stream (a .tar.bz2 decompressed). conda-build writes info/ first, so
// reading stops at the first entry outside info/ after the index was
// seen, before the payload.
func readInfo(r io.Reader) (index, about map[string]any, err error) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, badPackage("not a bzip2 tar archive: %v", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if index != nil && !strings.HasPrefix(name, "info/") {
			break
		}
		if name != "info/index.json" && name != "info/about.json" {
			continue
		}
		if hdr.Size > maxInfo {
			return nil, nil, badPackage("%s is larger than %d bytes", name, maxInfo)
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxInfo))
		if err != nil {
			return nil, nil, badPackage("reading %s: %v", name, err)
		}
		var m map[string]any
		if err := decodeJSON(b, &m); err != nil {
			return nil, nil, badPackage("%s: %v", name, err)
		}
		if name == "info/index.json" {
			index = m
		} else {
			about = m
		}
	}
	if index == nil {
		return nil, nil, badPackage("no info/index.json in the package")
	}
	return index, about, nil
}

// readConda checks that a .conda is one: a zip with metadata.json
// (conda_pkg_format_version) and the info-<stem>.tar.zst and
// pkg-<stem>.tar.zst tarballs.
func readConda(f io.ReaderAt, size int64, stem string) error {
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return badPackage("not a .conda (zip) file: %v", err)
	}
	has := map[string]bool{}
	for _, zf := range zr.File {
		has[zf.Name] = true
	}
	for _, name := range []string{"metadata.json", "info-" + stem + ".tar.zst", "pkg-" + stem + ".tar.zst"} {
		if !has[name] {
			return badPackage("no %s in the .conda file", name)
		}
	}
	mf, err := zr.Open("metadata.json")
	if err != nil {
		return badPackage("metadata.json: %v", err)
	}
	defer mf.Close()
	b, err := io.ReadAll(io.LimitReader(mf, maxInfo))
	if err != nil {
		return badPackage("metadata.json: %v", err)
	}
	var m struct {
		Version json.Number `json:"conda_pkg_format_version"`
	}
	if err := json.Unmarshal(b, &m); err != nil || m.Version == "" {
		return badPackage("metadata.json has no conda_pkg_format_version")
	}
	return nil
}
