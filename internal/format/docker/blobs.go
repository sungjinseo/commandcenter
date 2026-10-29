package docker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// session is a resumable blob upload: its bytes accumulate in a file under
// the repository's blob store tmp directory (blob.Store.SessionPath), next
// to a JSON sidecar naming its owner, so it survives a restart. It expires
// blob.SessionMaxAge after it started.
type session struct {
	Repo    string    `json:"repo"`
	Image   string    `json:"image"`
	Started time.Time `json:"started"`

	id   string
	path string // data file; the sidecar is path + ".json"
	size int64
}

func (h *Handler) sessionStore(rp *repo.Repository) (*blob.Store, error) {
	return h.Store.BlobStore(rp.Name)
}

func errUploadUnknown(id string) error {
	return Err(http.StatusNotFound, CodeBlobUploadUnknown, "blob upload unknown to registry: "+id)
}

// openSession loads the session of rt in rp, or BLOB_UPLOAD_UNKNOWN.
func (h *Handler) openSession(rp *repo.Repository, rt Route) (*session, error) {
	if !uuidRE.MatchString(rt.UUID) {
		return nil, errUploadUnknown(rt.UUID)
	}
	bs, err := h.sessionStore(rp)
	if err != nil {
		return nil, err
	}
	s := &session{id: rt.UUID, path: bs.SessionPath(rt.UUID)}
	b, err := os.ReadFile(s.path + ".json")
	if err != nil {
		return nil, errUploadUnknown(rt.UUID)
	}
	if json.Unmarshal(b, s) != nil || s.Repo != rp.Name || s.Image != rt.Image {
		return nil, errUploadUnknown(rt.UUID)
	}
	if h.now().After(s.Started.Add(blob.SessionMaxAge)) {
		s.remove()
		return nil, errUploadUnknown(rt.UUID)
	}
	// The size from an open file: on NFS, open revalidates the attributes
	// (close-to-open), so a chunk another node just appended counts, where a
	// stat could answer from the attribute cache.
	f, err := os.Open(s.path)
	if err != nil {
		s.remove()
		return nil, errUploadUnknown(rt.UUID)
	}
	info, err := f.Stat()
	f.Close()
	if err != nil {
		return nil, err
	}
	s.size = info.Size()
	return s, nil
}

func (s *session) remove() {
	os.Remove(s.path)
	os.Remove(s.path + ".json")
}

// respond answers 202 with where the upload stands.
func (s *session) respond(w http.ResponseWriter, name string, status int) {
	end := s.size - 1
	if end < 0 {
		end = 0
	}
	w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+s.id)
	w.Header().Set("Range", fmt.Sprintf("0-%d", end))
	w.Header().Set("Docker-Upload-UUID", s.id)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

// startUpload serves POST /v2/<name>/blobs/uploads/: a cross-repository
// mount (?mount=&from=), a monolithic upload (?digest=), or a new session.
func (h *Handler) startUpload(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route, c caller) error {
	q := r.URL.Query()
	if mount, from := q.Get("mount"), q.Get("from"); mount != "" && from != "" {
		if done, err := h.mount(r, rp, rt, c, mount, from); err != nil || done {
			if err == nil {
				w.Header().Set("Location", "/v2/"+rt.Name()+"/blobs/"+mount)
				w.Header().Set("Docker-Content-Digest", mount)
				w.Header().Set("Content-Length", "0")
				w.WriteHeader(http.StatusCreated)
			}
			return err
		}
		// not mountable: a normal session, as Distribution does
	}
	if digest := q.Get("digest"); digest != "" {
		return h.monolithic(w, r, rp, rt, digest)
	}
	bs, err := h.sessionStore(rp)
	if err != nil {
		return err
	}
	id := make([]byte, 16)
	rand.Read(id)
	s := &session{Repo: rp.Name, Image: rt.Image, Started: h.now().UTC(), id: hex.EncodeToString(id)}
	s.path = bs.SessionPath(s.id)
	meta, _ := json.Marshal(s)
	if err := os.WriteFile(s.path+".json", meta, 0o644); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		os.Remove(s.path + ".json")
		return err
	}
	f.Close()
	s.respond(w, rt.Name(), http.StatusAccepted)
	return nil
}

// mount links a blob of another image (same or another repository) into
// rt's image when c may pull it there; done is false when it cannot be
// mounted, which falls back to a normal upload.
func (h *Handler) mount(r *http.Request, rp *repo.Repository, rt Route, c caller, digest, from string) (bool, error) {
	if !ValidDigest(digest) || !ValidName(from) || !h.allowed(c, from, "pull") {
		return false, nil
	}
	fromRepo, fromImage, _ := strings.Cut(from, "/")
	src := h.Repos.Get(fromRepo)
	if src == nil || src.Format != "docker" || !src.IsOnline() {
		return false, nil
	}
	ctx := r.Context()
	a, err := h.Store.DB.GetAsset(ctx, src.Name, blobKey(fromImage, digest))
	if err != nil {
		return false, offline(err)
	}
	// Same store: the blob is there already. Another store: copy it over.
	sums := blob.Sums{SHA256: a.BlobSHA256, SHA1: a.SHA1, MD5: a.MD5, SHA512: a.SHA512, Size: a.Size}
	bs, err := h.sessionStore(rp)
	if err != nil {
		return false, err
	}
	if _, err := bs.Stat(a.BlobSHA256); err != nil {
		f, err := h.Store.Open(a)
		if err != nil {
			return false, err
		}
		defer f.Close()
		if sums, err = h.Store.WriteBlob(rp.Name, f, nil); err != nil {
			return false, err
		}
	}
	return true, h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, blobKey(rt.Image, digest), "application/octet-stream", sums))
	})
}

// monolithic stores the request body as the blob digest in one request.
func (h *Handler) monolithic(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route, digest string) error {
	if !ValidDigest(digest) {
		return Err(http.StatusBadRequest, CodeDigestInvalid, "invalid digest: "+digest)
	}
	sums, err := h.Store.WriteBlob(rp.Name, r.Body, func(s blob.Sums) error {
		if "sha256:"+s.SHA256 != digest {
			return Err(http.StatusBadRequest, CodeDigestInvalid, "uploaded content has digest sha256:"+s.SHA256+", not "+digest)
		}
		return nil
	})
	if err != nil {
		return uploadErr(err)
	}
	return h.finishBlob(w, r, rp, rt, digest, sums)
}

// finishBlob records the stored blob as the image's and answers 201.
func (h *Handler) finishBlob(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route, digest string, sums blob.Sums) error {
	ctx := r.Context()
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, blobKey(rt.Image, digest), "application/octet-stream", sums))
	})
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v2/"+rt.Name()+"/blobs/"+digest)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
	return nil
}

// uploadErr maps body read failures to registry errors.
func uploadErr(err error) error {
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		return Err(http.StatusRequestEntityTooLarge, CodeSizeInvalid, "upload is larger than the maxUploadMiB setting allows")
	case errors.Is(err, io.ErrUnexpectedEOF):
		return Err(http.StatusBadRequest, CodeBlobUploadInvalid, "upload body ended early")
	}
	return err
}

// upload serves the session routes: PATCH appends a chunk, PUT appends the
// rest and completes, GET reports progress, DELETE cancels. The session's
// lock is a cluster lock, so the steps of one upload are serialized
// whichever nodes they reach (the session lives on the shared store).
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	unlock, err := h.Store.Lock(r.Context(), "docker\x00upload\x00"+rt.UUID)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := h.openSession(rp, rt)
	if err != nil {
		return err
	}
	switch r.Method {
	case http.MethodGet:
		s.respond(w, rt.Name(), http.StatusNoContent)
		return nil
	case http.MethodDelete:
		s.remove()
		w.WriteHeader(http.StatusNoContent)
		return nil
	case http.MethodPatch:
		if cr := r.Header.Get("Content-Range"); cr != "" {
			start, end, ok := parseContentRange(cr)
			if !ok || end < start {
				return Err(http.StatusBadRequest, CodeBlobUploadInvalid, "invalid Content-Range "+cr)
			}
			if start != s.size {
				w.Header().Set("Range", fmt.Sprintf("0-%d", max(s.size-1, 0)))
				w.Header().Set("Docker-Upload-UUID", s.id)
				return Err(http.StatusRequestedRangeNotSatisfiable, CodeBlobUploadInvalid,
					fmt.Sprintf("chunk starts at %d, upload is at %d", start, s.size))
			}
		}
		if err := h.appendChunk(w, r, s); err != nil {
			return err
		}
		s.respond(w, rt.Name(), http.StatusAccepted)
		return nil
	case http.MethodPut:
		digest := r.URL.Query().Get("digest")
		if !ValidDigest(digest) {
			return Err(http.StatusBadRequest, CodeDigestInvalid, "invalid digest: "+digest)
		}
		if err := h.appendChunk(w, r, s); err != nil {
			return err
		}
		bs, err := h.sessionStore(rp)
		if err != nil {
			return err
		}
		os.Remove(s.path + ".json")
		sums, err := bs.CommitFile(s.path) // removes the data file either way
		if err != nil {
			return err
		}
		if "sha256:"+sums.SHA256 != digest {
			return Err(http.StatusBadRequest, CodeDigestInvalid, "uploaded content has digest sha256:"+sums.SHA256+", not "+digest)
		}
		return h.finishBlob(w, r, rp, rt, digest, sums)
	}
	return repo.MethodNotAllowed("GET, PATCH, PUT, DELETE", "method %s not allowed", r.Method)
}

// appendChunk streams the body onto the session file. The whole blob is
// bounded by MaxUpload; an upload past it is dropped.
func (h *Handler) appendChunk(w http.ResponseWriter, r *http.Request, s *session) error {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return nil
	}
	body := io.Reader(r.Body)
	limit := int64(0)
	if h.MaxUpload != nil {
		limit = h.MaxUpload()
	}
	if limit > 0 {
		if s.size >= limit {
			s.remove()
			return Err(http.StatusRequestEntityTooLarge, CodeSizeInvalid, "upload is larger than the maxUploadMiB setting allows")
		}
		body = io.LimitReader(r.Body, limit-s.size+1)
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, body)
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	s.size += n
	if err != nil {
		return uploadErr(err)
	}
	if limit > 0 && s.size > limit {
		s.remove()
		return Err(http.StatusRequestEntityTooLarge, CodeSizeInvalid, "upload is larger than the maxUploadMiB setting allows")
	}
	return nil
}

// parseContentRange parses "start-end" (Distribution's form; a "bytes "
// prefix and "/size" suffix are tolerated).
func parseContentRange(v string) (start, end int64, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "bytes ")
	v, _, _ = strings.Cut(v, "/")
	a, b, found := strings.Cut(v, "-")
	if !found {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(a, 10, 64)
	end, err2 := strconv.ParseInt(b, 10, 64)
	return start, end, err1 == nil && err2 == nil && start >= 0
}
