package golang

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Upload size limits per file kind; variables so tests can lower them.
var (
	maxZipUpload  int64 = 500 << 20
	maxModUpload  int64 = 16 << 20
	maxInfoUpload int64 = 1 << 20
)

func uploadLimit(k Kind) int64 {
	switch k {
	case KindZip:
		return maxZipUpload
	case KindMod:
		return maxModUpload
	}
	return maxInfoUpload
}

// upload stores a .mod, .zip or .info file of a hosted module version. The body
// is streamed to disk and validated from there before it is recorded.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	if rt.Kind != KindMod && rt.Kind != KindZip && rt.Kind != KindInfo {
		return repo.MethodNotAllowed("GET, HEAD", "only .mod, .zip and .info files can be uploaded")
	}
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	module, err := Unescape(rt.Module)
	if err != nil {
		return repo.Errorf(http.StatusBadRequest, "invalid module path: %v", err)
	}
	version, err := Unescape(rt.Version)
	if err != nil {
		return repo.Errorf(http.StatusBadRequest, "invalid version: %v", err)
	}
	if err := checkPathMajor(module, version); err != nil {
		return err
	}
	ctx := r.Context()
	key := assetKey(rt.Module, rt.Version+ext(rt.Kind))
	infoKey := assetKey(rt.Module, rt.Version+".info")

	unlock := h.locks.Lock(rp.Name + "\x00" + rt.Module + "@" + rt.Version)
	defer unlock()
	// Only a .mod may be re-uploaded, and only with byte-identical content (for
	// example one derived from a zip): that is a no-op. Anything else is 409.
	if rp.WritePolicy != "allow" {
		a, err := h.Store.Get(ctx, rp.Name, key)
		switch {
		case err == nil:
			if rt.Kind != KindMod {
				return repo.Errorf(http.StatusConflict, "%s already exists in %s", key, rp.Name)
			}
			// Hash the body without storing it: no orphan blob either way.
			h256 := sha256.New()
			if _, err := io.Copy(h256, http.MaxBytesReader(w, r.Body, uploadLimit(rt.Kind))); err != nil {
				return uploadReadErr(err)
			}
			if hex.EncodeToString(h256.Sum(nil)) != a.BlobSHA256 {
				return repo.Errorf(http.StatusConflict, "%s already exists in %s", key, rp.Name)
			}
			w.WriteHeader(http.StatusCreated) // identical content: no-op
			return nil
		case !errors.Is(err, repo.ErrNotFound):
			return err
		}
	} else if !h.Auth.Permits(ctx, rp.Name, auth.Edit) {
		// writePolicy allow: replacing an existing file is an edit (plan 12
		// Decision 2); checked under the lock, like the write itself.
		_, err := h.Store.Get(ctx, rp.Name, key)
		if err == nil {
			return repo.Errorf(http.StatusForbidden, "user %q may not overwrite %s", auth.FromContext(ctx).User, key)
		}
		if !errors.Is(err, repo.ErrNotFound) {
			return err
		}
	}

	sums, err := h.Store.WriteBlob(rp.Name, http.MaxBytesReader(w, r.Body, uploadLimit(rt.Kind)), nil)
	if err != nil {
		return uploadReadErr(err)
	}
	if err := h.validate(rp, rt.Kind, sums, module, version); err != nil {
		return err
	}

	var info *blob.Sums
	if rt.Kind != KindInfo {
		if _, err := h.Store.Get(ctx, rp.Name, infoKey); errors.Is(err, repo.ErrNotFound) {
			doc, _ := json.Marshal(map[string]string{"Version": version, "Time": h.Now().UTC().Format(time.RFC3339)})
			s, err := h.Store.WriteBlob(rp.Name, bytes.NewReader(doc), nil)
			if err != nil {
				return err
			}
			info = &s
		} else if err != nil {
			return err
		}
	}

	var mod *blob.Sums
	if rt.Kind == KindZip {
		modKey := assetKey(rt.Module, rt.Version+".mod")
		if _, err := h.Store.Get(ctx, rp.Name, modKey); errors.Is(err, repo.ErrNotFound) {
			s, err := h.deriveMod(rp, sums, module, version)
			if err != nil {
				return err
			}
			mod = &s
		} else if err != nil {
			return err
		}
	}

	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, key, contentType(rt.Kind), sums)); err != nil {
			return err
		}
		if mod != nil {
			if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, assetKey(rt.Module, rt.Version+".mod"), contentType(KindMod), *mod)); err != nil {
				return err
			}
		}
		if info != nil {
			return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, infoKey, contentType(KindInfo), *info))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if rt.Kind == KindZip {
		h.Store.Emit(r, "upload", rp.Name, module+"@"+version, "")
	}
	w.WriteHeader(http.StatusCreated)
	return nil
}

// validate checks a stored upload against the module path and version it was
// uploaded as.
func (h *Handler) validate(rp *repo.Repository, k Kind, sums blob.Sums, module, version string) error {
	bs, err := h.Store.BlobStore(rp.Name)
	if err != nil {
		return err
	}
	f, err := bs.Open(sums.SHA256)
	if err != nil {
		return err
	}
	defer f.Close()
	switch k {
	case KindMod:
		p, err := modulePath(f)
		if err != nil {
			return repo.Errorf(http.StatusBadRequest, "invalid go.mod: %v", err)
		}
		if p != module {
			return repo.Errorf(http.StatusBadRequest, "go.mod declares %q, want %q", p, module)
		}
	case KindZip:
		return checkZip(f, sums.Size, module, version)
	case KindInfo:
		var doc struct {
			Version string
			Time    *time.Time
		}
		if err := json.NewDecoder(io.LimitReader(f, maxDoc)).Decode(&doc); err != nil {
			return repo.Errorf(http.StatusBadRequest, "invalid .info: %v", err)
		}
		if doc.Version != version {
			return repo.Errorf(http.StatusBadRequest, ".info names %q, want %q", doc.Version, version)
		}
	}
	return nil
}

// deriveMod stores a .mod for a zip uploaded without one: the zip's root go.mod
// (bounded by maxModUpload and streamed to the blob store) or a module stub when
// the zip has none. The zip was already validated by checkZip.
func (h *Handler) deriveMod(rp *repo.Repository, zipSums blob.Sums, module, version string) (blob.Sums, error) {
	bs, err := h.Store.BlobStore(rp.Name)
	if err != nil {
		return blob.Sums{}, err
	}
	f, err := bs.Open(zipSums.SHA256)
	if err != nil {
		return blob.Sums{}, err
	}
	defer f.Close()
	zr, err := zip.NewReader(f, zipSums.Size)
	if err != nil {
		return blob.Sums{}, repo.Errorf(http.StatusBadRequest, "not a valid zip file: %v", err)
	}
	want := module + "@" + version + "/go.mod"
	for _, zf := range zr.File {
		if zf.Name != want {
			continue
		}
		if zf.UncompressedSize64 > uint64(maxModUpload) {
			return blob.Sums{}, repo.Errorf(http.StatusBadRequest, "zip go.mod exceeds %d bytes", maxModUpload)
		}
		rc, err := zf.Open()
		if err != nil {
			return blob.Sums{}, repo.Errorf(http.StatusBadRequest, "zip go.mod: %v", err)
		}
		defer rc.Close()
		src := &readErrRecorder{r: io.LimitReader(rc, maxModUpload)}
		sums, err := h.Store.WriteBlob(rp.Name, src, nil)
		if err != nil && src.err != nil {
			// a CRC or deflate failure inside the zip entry is a bad upload, not a server fault
			return blob.Sums{}, repo.Errorf(http.StatusBadRequest, "zip go.mod is corrupt: %v", src.err)
		}
		return sums, err
	}
	return h.Store.WriteBlob(rp.Name, strings.NewReader("module "+module+"\n"), nil)
}

// uploadReadErr maps a failed upload-body read to its HTTP error.
func uploadReadErr(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return repo.Errorf(http.StatusRequestEntityTooLarge, "upload exceeds %d bytes", tooBig.Limit)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return repo.Errorf(http.StatusBadRequest, "upload body ended early: %v", err)
	}
	return err
}

// readErrRecorder remembers a non-EOF error from the wrapped reader.
type readErrRecorder struct {
	r   io.Reader
	err error
}

func (e *readErrRecorder) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF {
		e.err = err
	}
	return n, err
}
