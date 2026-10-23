package pypi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

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
	"sha256_digest": true, "md5_digest": true,
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
				if hasCanonical(project, pkgs, filename) {
					return repo.Errorf(http.StatusBadRequest, "File already exists.")
				}
			}
			if sums, err = h.Store.WriteBlob(rp.Name, part, nil); err != nil {
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
	version := fields["version"]
	requiresPython := fields["requires_python"]
	if fromFile, err := h.fileRequiresPython(rp, sums, filename); err != nil {
		return err // blob store failures are server-side (500)
	} else if fromFile != "" && validSpecifier(fromFile) {
		requiresPython = fromFile // the file's own metadata wins over the form
	}
	if d := fields["sha256_digest"]; d != "" && !strings.EqualFold(d, sums.SHA256) {
		return repo.Errorf(http.StatusBadRequest, "sha256_digest does not match the uploaded file")
	}
	if d := fields["md5_digest"]; d != "" && !strings.EqualFold(d, sums.MD5) {
		return repo.Errorf(http.StatusBadRequest, "md5_digest does not match the uploaded file")
	}
	if err := h.record(r.Context(), rp, project, fileAttrs{
		Filename: filename, Version: version, SHA256: sums.SHA256, RequiresPython: requiresPython,
	}, sums); err != nil {
		return err
	}
	h.Store.Emit(r, "upload", rp.Name, filename, "")
	w.WriteHeader(http.StatusOK)
	return nil
}

// record stores the file row and asset in one transaction.
func (h *Handler) record(ctx context.Context, rp *repo.Repository, project string, attrs fileAttrs, sums blob.Sums) error {
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
		if hasCanonical(project, pkgs, attrs.Filename) {
			return repo.Errorf(http.StatusBadRequest, "File already exists.")
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
		return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, fileKey(project, attrs.Filename), "application/octet-stream", sums))
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
	if !validVersion(fv) || !validVersion(fields["version"]) {
		return "", repo.Errorf(http.StatusBadRequest, "invalid version: must be a PEP 440 version")
	}
	if !strings.EqualFold(fv, fields["version"]) {
		return "", repo.Errorf(http.StatusBadRequest, "version %q does not match file name version %q", fields["version"], fv)
	}
	if rpy := fields["requires_python"]; rpy != "" && !validSpecifier(rpy) {
		return "", repo.Errorf(http.StatusBadRequest, "invalid requires_python %q", rpy)
	}
	return project, nil
}

// multipartErr maps a failed read of the multipart body: an exceeded upload cap
// passes through unchanged (413), anything else is a bad request.
func multipartErr(err error) error {
	if errors.As(err, new(*http.MaxBytesError)) {
		return err
	}
	return repo.Errorf(http.StatusBadRequest, "invalid multipart body: %v", err)
}

// hasCanonical reports whether pkgs already hold a file that is the same as
// filename after folding its spelling.
func hasCanonical(project string, pkgs []meta.Package, filename string) bool {
	want := canonicalKey(project, filename)
	for _, p := range pkgs {
		if p.Version == filename || canonicalKey(project, p.Version) == want {
			return true
		}
	}
	return false
}

// fileRequiresPython reads Requires-Python from the stored upload's metadata.
func (h *Handler) fileRequiresPython(rp *repo.Repository, sums blob.Sums, filename string) (string, error) {
	bs, err := h.Store.BlobStore(rp.Name)
	if err != nil {
		return "", err
	}
	f, err := bs.Open(sums.SHA256)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return readRequiresPython(f, sums.Size, filename)
}
