package pypi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"golang.org/x/crypto/blake2b"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

const (
	maxField   = 4 << 10  // cap for each form field the handler uses
	maxIgnored = 16 << 20 // cap for all other form fields together (twine sends the README as description)
	maxParts   = 256
)

// usedFields are the only form fields buffered; everything else is drained.
var usedFields = map[string]bool{
	":action": true, "name": true, "version": true, "requires_python": true,
	"sha256_digest": true, "md5_digest": true, "blake2_256_digest": true,
}

// upload handles twine's legacy upload API. The file part is streamed to the
// blob store; other form fields are small and read into memory.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, rp *repo.Repository) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return repo.Errorf(http.StatusBadRequest, "upload must be multipart/form-data: %v", err)
	}
	fields := map[string]string{}
	b2, _ := blake2b.New256(nil) // errors only for a key over 64 bytes
	var (
		sums     blob.Sums
		filename string
		haveFile bool
		drained  int64
		parts    int
	)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return multipartErr(err)
		}
		name := part.FormName()
		if name == "content" {
			if haveFile {
				return repo.Errorf(http.StatusBadRequest, "only one file per upload")
			}
			filename = part.FileName()
			if !validFile(filename) {
				return repo.Errorf(http.StatusBadRequest, "invalid file name %q", filename)
			}
			// twine sends the metadata fields first; reject before streaming anything.
			_, hasAction := fields[":action"]
			_, hasName := fields["name"]
			_, hasVersion := fields["version"]
			if hasAction && hasName && hasVersion {
				project, err := checkFields(fields, filename)
				if err != nil {
					return err
				}
				// Cheap duplicate check; the transaction's check stays the final guard.
				pkgs, err := h.Store.DB.ListPackages(r.Context(), rp.Name, project)
				if err != nil {
					return err
				}
				if err := duplicate(project, pkgs, filename); err != nil {
					return err
				}
			}
			if sums, err = h.Store.WriteBlob(rp.Name, io.TeeReader(part, b2), nil); err != nil {
				if errors.Is(err, io.ErrUnexpectedEOF) {
					return repo.Errorf(http.StatusBadRequest, "upload body ended early: %v", err)
				}
				return err // blob.ErrDiskFull and other server-side failures stay 500
			}
			haveFile = true
			continue
		}
		parts++
		if parts > maxParts {
			return repo.Errorf(http.StatusRequestEntityTooLarge, "upload has too many form fields")
		}
		if !usedFields[name] {
			n, err := io.Copy(io.Discard, io.LimitReader(part, maxIgnored-drained+1))
			if err != nil {
				return multipartErr(err)
			}
			drained += n
			if drained > maxIgnored {
				return repo.Errorf(http.StatusRequestEntityTooLarge, "upload form fields are too large")
			}
			continue
		}
		b, err := io.ReadAll(io.LimitReader(part, maxField+1))
		if err != nil {
			return multipartErr(err)
		}
		if len(b) > maxField {
			return repo.Errorf(http.StatusRequestEntityTooLarge, "form field %q is too large", name)
		}
		if _, ok := fields[name]; !ok { // repeated fields (e.g. classifiers) keep the first value
			fields[name] = string(b)
		}
	}

	if !haveFile {
		return repo.Errorf(http.StatusBadRequest, "upload has no content part")
	}
	project, err := checkFields(fields, filename)
	if err != nil {
		return err
	}
	if d := fields["sha256_digest"]; d != "" && !strings.EqualFold(d, sums.SHA256) {
		return repo.Errorf(http.StatusBadRequest, "sha256_digest does not match the uploaded file")
	}
	if d := fields["md5_digest"]; d != "" && !strings.EqualFold(d, sums.MD5) {
		return repo.Errorf(http.StatusBadRequest, "md5_digest does not match the uploaded file")
	}
	if d := fields["blake2_256_digest"]; d != "" && !strings.EqualFold(d, hex.EncodeToString(b2.Sum(nil))) {
		return repo.Errorf(http.StatusBadRequest, "blake2_256_digest does not match the uploaded file")
	}
	version, _ := normalizeVersion(fields["version"])
	md, raw, err := h.fileMetadata(rp, sums, filename, project, version)
	if err != nil {
		return err
	}
	if Normalize(md.Name) != project || canonicalVersion(md.Version) != canonicalVersion(version) {
		return repo.Errorf(http.StatusBadRequest, "the file's metadata is for %.100q %.100q, not for %s %s", md.Name, md.Version, project, version)
	}
	requiresPython := fields["requires_python"]
	if md.RequiresPython != "" {
		if !validSpecifier(md.RequiresPython) {
			return repo.Errorf(http.StatusBadRequest, "invalid Requires-Python %.100q in the file's metadata", md.RequiresPython)
		}
		requiresPython = md.RequiresPython // the file's own metadata wins over the form
	}
	mdSums, err := h.Store.WriteBlob(rp.Name, bytes.NewReader(raw), nil)
	if err != nil {
		return err
	}
	if err := h.record(r.Context(), rp, project, fileAttrs{
		Filename: filename, Version: version, SHA256: sums.SHA256, RequiresPython: requiresPython,
		Blake2b256: hex.EncodeToString(b2.Sum(nil)), MetadataSHA256: mdSums.SHA256,
	}, sums, mdSums); err != nil {
		return err
	}
	h.Store.Emit(r, "upload", rp.Name, filename, "")
	w.WriteHeader(http.StatusOK)
	return nil
}

// record stores the file row, the file and its core metadata sidecar (PEP
// 658, blob mdSums) in one transaction.
func (h *Handler) record(ctx context.Context, rp *repo.Repository, project string, attrs fileAttrs, sums, mdSums blob.Sums) error {
	unlock := h.locks.Lock(rp.Name + "\x00" + project)
	defer unlock()
	doc, err := json.Marshal(attrs)
	if err != nil {
		return err
	}
	return h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		pkgs, err := tx.ListPackages(ctx, rp.Name, project)
		if err != nil {
			return err
		}
		if err := duplicate(project, pkgs, attrs.Filename); err != nil {
			return err
		}
		err = tx.InsertPackage(ctx, meta.Package{
			Repo: rp.Name, Format: "pypi", Name: project, Version: attrs.Filename, Attrs: string(doc), CreatedAt: h.Now(),
		})
		if errors.Is(err, meta.ErrExists) {
			return repo.Errorf(http.StatusBadRequest, "File already exists.")
		}
		if err != nil {
			return err
		}
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, fileKey(project, attrs.Filename), "application/octet-stream", sums)); err != nil {
			return err
		}
		return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, metadataKey(project, attrs.Filename), metadataType, mdSums))
	})
}

// checkFields validates the form fields against the uploaded file name and
// returns the normalized project name.
func checkFields(fields map[string]string, filename string) (string, error) {
	if fields[":action"] != "file_upload" {
		return "", repo.Errorf(http.StatusBadRequest, "unsupported :action %q", fields[":action"])
	}
	if !ValidName(fields["name"]) {
		return "", repo.Errorf(http.StatusBadRequest, "invalid project name %q", fields["name"])
	}
	project := Normalize(fields["name"])
	if !validFile(fields["version"]) {
		return "", repo.Errorf(http.StatusBadRequest, "invalid or missing version")
	}
	if !fileMatches(project, filename) {
		return "", repo.Errorf(http.StatusBadRequest, "file %q does not belong to project %q", filename, project)
	}
	fv, ok := fileVersion(project, filename)
	if !ok {
		return "", repo.Errorf(http.StatusBadRequest, "unsupported file type %q: use .whl, .tar.gz or .zip", filename)
	}
	for _, v := range []string{fields["version"], fv} {
		if !validVersion(v) {
			return "", repo.Errorf(http.StatusBadRequest, "invalid version %q: must be a PEP 440 version, without a leading \"v\"", v)
		}
	}
	if canonicalVersion(fv) != canonicalVersion(fields["version"]) {
		return "", repo.Errorf(http.StatusBadRequest, "version %q does not match file name version %q", fields["version"], fv)
	}
	if base, ext, _ := splitUploadExt(filename); ext == ".whl" {
		if _, ok := wheelTags(strings.Split(base, "-")[2:]); !ok {
			return "", repo.Errorf(http.StatusBadRequest, "invalid wheel file name %q: want {name}-{version}(-{build})?-{python}-{abi}-{platform}.whl", filename)
		}
	}
	if rpy := fields["requires_python"]; rpy != "" && !validSpecifier(rpy) {
		return "", repo.Errorf(http.StatusBadRequest, "invalid requires_python %q", rpy)
	}
	return project, nil
}

// multipartErr maps a failed read of the multipart body: an exceeded upload cap
// (413) or a stalled body (408) passes through unchanged, anything else is a
// bad request.
func multipartErr(err error) error {
	if errors.As(err, new(*http.MaxBytesError)) || errors.Is(err, os.ErrDeadlineExceeded) {
		return err
	}
	return repo.Errorf(http.StatusBadRequest, "invalid multipart body: %v", err)
}

// duplicate returns the 400 for filename when pkgs already hold the same file
// after folding its spelling (canonicalKey), and nil otherwise.
func duplicate(project string, pkgs []meta.Package, filename string) error {
	want := canonicalKey(project, filename)
	for _, p := range pkgs {
		switch {
		case p.Version == filename:
			return repo.Errorf(http.StatusBadRequest, "File already exists.")
		case canonicalKey(project, p.Version) == want: // twine --skip-existing looks for "already exist"
			return repo.Errorf(http.StatusBadRequest, "File already exists: this release already has %q (one sdist per release; wheel tags compare as sets)", p.Version)
		}
	}
	return nil
}

// fileMetadata reads the stored upload's metadata (see readMetadata). Failing
// to open the blob is server-side (500).
func (h *Handler) fileMetadata(rp *repo.Repository, sums blob.Sums, filename, project, version string) (distMeta, []byte, error) {
	bs, err := h.Store.BlobStore(rp.Name)
	if err != nil {
		return distMeta{}, nil, err
	}
	f, err := bs.Open(sums.SHA256)
	if err != nil {
		return distMeta{}, nil, err
	}
	defer f.Close()
	return readMetadata(f, sums.Size, filename, project, version)
}
