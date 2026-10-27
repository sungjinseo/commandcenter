package pypi

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/zipdir"
)

// coreMeta is the part of a distribution's METADATA / PKG-INFO that Ingest uses.
type coreMeta struct {
	Name, Version, RequiresPython string
}

// errMetadataTooLarge marks a header block over maxMetadata.
var errMetadataTooLarge = repo.Errorf(http.StatusRequestEntityTooLarge, "distribution metadata is larger than %d bytes", maxMetadata)

// parseCoreMeta scans the RFC 822 header block of a metadata document (it ends
// at the first blank line); only that block is read. A block over maxMetadata
// is refused.
func parseCoreMeta(r io.Reader) (coreMeta, error) {
	br := bufio.NewReader(io.LimitReader(r, maxMetadata+1))
	var m coreMeta
	var read int
	for {
		line, err := br.ReadString('\n')
		read += len(line)
		if read > maxMetadata {
			return m, errMetadataTooLarge
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return m, nil
		}
		if k, v, ok := strings.Cut(line, ":"); ok && k != "" && k[0] != ' ' && k[0] != '\t' {
			v = strings.TrimSpace(v)
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "name":
				if m.Name == "" {
					m.Name = v
				}
			case "version":
				if m.Version == "" {
					m.Version = v
				}
			case "requires-python":
				if m.RequiresPython == "" {
					m.RequiresPython = v
				}
			}
		}
		if err != nil {
			return m, nil
		}
	}
}

// readCoreMeta reads the metadata of a stored distribution (a wheel's or zip's
// *.dist-info/METADATA, an sdist's <name>-<ver>/PKG-INFO). found is false when
// the file is not a readable archive or holds no such document; an oversized
// metadata document is an error.
func readCoreMeta(r io.ReaderAt, size int64, filename string) (m coreMeta, found bool, err error) {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".whl"):
		return coreFromZip(r, size, func(n string) bool {
			dir, base := path.Split(n)
			return base == "METADATA" && strings.Count(dir, "/") == 1 && strings.HasSuffix(dir, ".dist-info/")
		})
	case strings.HasSuffix(lower, ".zip"):
		return coreFromZip(r, size, func(n string) bool {
			return strings.Count(n, "/") == 1 && path.Base(n) == "PKG-INFO"
		})
	case strings.HasSuffix(lower, ".tar.gz"):
		return coreFromTarGz(io.NewSectionReader(r, 0, size))
	}
	return coreMeta{}, false, nil
}

func coreFromZip(r io.ReaderAt, size int64, match func(string) bool) (coreMeta, bool, error) {
	if !zipdir.OK(r, size, maxEntries, maxDirectory) {
		return coreMeta{}, false, nil
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return coreMeta{}, false, nil
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
			return coreMeta{}, false, nil
		}
		defer rc.Close()
		m, err := parseCoreMeta(rc)
		return m, err == nil, err
	}
	return coreMeta{}, false, nil
}

func coreFromTarGz(r io.Reader) (coreMeta, bool, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return coreMeta{}, false, nil
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, maxDecompressed))
	for i := 0; i < maxEntries; i++ {
		h, err := tr.Next()
		if err != nil {
			return coreMeta{}, false, nil
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag == tar.TypeReg && strings.Count(name, "/") == 1 && path.Base(name) == "PKG-INFO" {
			m, err := parseCoreMeta(tr)
			return m, err == nil, err
		}
	}
	return coreMeta{}, false, nil
}

// projectOfFile derives the project name from a distribution file name when
// the file carries no readable metadata.
func projectOfFile(filename string) string {
	base, ext, ok := splitUploadExt(filename)
	if !ok {
		return ""
	}
	if ext == ".whl" {
		return strings.Split(base, "-")[0]
	}
	if i := sdistSplit("", base); i >= 0 {
		return base[:i]
	}
	return ""
}

// Ingest stores one distribution file in the hosted repository rp, as an
// upload does, and reports whether it was stored or skipped. The file is
// streamed to a blob (its sha256 must equal expectSHA256 when that is not
// empty). The project name comes from its METADATA / PKG-INFO (or its file
// name), and the file is then checked as an upload checks it (readMetadata:
// the metadata must be there and name this project and version); a file that
// fails is refused before anything is recorded. A file already stored with
// the same content is Skipped, a different file of the same name is a
// conflict (repo.IsIngestConflict); nothing is overwritten. It makes no
// privilege checks and ignores the repository's write policy: the caller is
// the admin import.
func (h *Handler) Ingest(ctx context.Context, rp *repo.Repository, filename string, r io.Reader, expectSHA256, actor string) (repo.IngestResult, error) {
	if !validFile(filename) {
		return "", repo.Errorf(http.StatusBadRequest, "invalid file name %q", filename)
	}
	_, _, ok := splitUploadExt(filename)
	if !ok {
		return "", repo.Errorf(http.StatusBadRequest, "unsupported file type %q: use .whl, .tar.gz or .zip", filename)
	}
	sums, err := h.Store.WriteBlob(rp.Name, r, func(s blob.Sums) error {
		if expectSHA256 != "" && !strings.EqualFold(expectSHA256, s.SHA256) {
			return repo.Errorf(http.StatusBadRequest, "sha256 of %s does not match the expected %s", filename, expectSHA256)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	bs, err := h.Store.BlobStore(rp.Name)
	if err != nil {
		return "", err
	}
	f, err := bs.Open(sums.SHA256)
	if err != nil {
		return "", err
	}
	cm, found, err := readCoreMeta(f, sums.Size, filename)
	f.Close()
	if err != nil {
		return "", err
	}
	name := cm.Name
	if !found || name == "" {
		name = projectOfFile(filename)
		cm = coreMeta{}
	}
	if !ValidName(name) {
		return "", repo.Errorf(http.StatusBadRequest, "invalid project name %q", name)
	}
	project := Normalize(name)
	if !fileMatches(project, filename) {
		return "", repo.Errorf(http.StatusBadRequest, "file %q does not belong to project %q", filename, project)
	}
	version, ok := fileVersion(project, filename)
	if !ok {
		return "", repo.Errorf(http.StatusBadRequest, "unsupported file type %q: use .whl, .tar.gz or .zip", filename)
	}
	if !validVersion(version) {
		return "", repo.Errorf(http.StatusBadRequest, "invalid version %q: must be a PEP 440 version", version)
	}
	if cm.Version != "" && canonicalVersion(cm.Version) != canonicalVersion(version) {
		return "", repo.Errorf(http.StatusBadRequest, "metadata version %q does not match file name version %q", cm.Version, version)
	}
	if base, ext, _ := splitUploadExt(filename); ext == ".whl" {
		if _, ok := wheelTags(strings.Split(base, "-")[2:]); !ok {
			return "", repo.Errorf(http.StatusBadRequest, "invalid wheel file name %q: want {name}-{version}(-{build})?-{python}-{abi}-{platform}.whl", filename)
		}
	}
	// The same check of the file's own metadata as an upload makes.
	md, err := h.fileMetadata(rp, sums, filename, project, version)
	if err != nil {
		return "", err
	}
	if Normalize(md.Name) != project || canonicalVersion(md.Version) != canonicalVersion(version) {
		return "", repo.Errorf(http.StatusBadRequest, "the file's metadata is for %.100q %.100q, not for %s %s", md.Name, md.Version, project, version)
	}
	requiresPython := ""
	if md.RequiresPython != "" {
		if !validSpecifier(md.RequiresPython) {
			return "", repo.Errorf(http.StatusBadRequest, "invalid Requires-Python %.100q in the file's metadata", md.RequiresPython)
		}
		requiresPython = md.RequiresPython
	}
	doc, err := json.Marshal(fileAttrs{Filename: filename, Version: version, SHA256: sums.SHA256, RequiresPython: requiresPython})
	if err != nil {
		return "", err
	}

	unlock := h.locks.Lock(rp.Name + "\x00" + project)
	defer unlock()
	res := repo.Ingested
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		res = repo.Ingested
		pkgs, err := tx.ListPackages(ctx, rp.Name, project)
		if err != nil {
			return err
		}
		want := canonicalKey(project, filename)
		for _, p := range pkgs {
			if p.Version != filename && canonicalKey(project, p.Version) != want {
				continue
			}
			a, err := tx.GetAsset(ctx, rp.Name, fileKey(project, p.Version))
			if err == nil && a.BlobSHA256 == sums.SHA256 {
				res = repo.Skipped
				return nil
			}
			if err != nil && !errors.Is(err, meta.ErrNotFound) {
				return err
			}
			return repo.IngestConflict("%s already exists with different content", filename)
		}
		err = tx.InsertPackage(ctx, meta.Package{
			Repo: rp.Name, Format: "pypi", Name: project, Version: filename, Attrs: string(doc), CreatedAt: h.Now(),
		})
		if errors.Is(err, meta.ErrExists) {
			return repo.IngestConflict("%s already exists", filename)
		}
		if err != nil {
			return err
		}
		return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, fileKey(project, filename), "application/octet-stream", sums))
	})
	if err != nil {
		return "", err
	}
	if res == repo.Ingested {
		h.Store.EmitAs("upload", actor, rp.Name, filename, "")
	}
	return res, nil
}
