// Package gitlfs implements a Git LFS server (Batch API with the basic
// transfer) as a hosted repository: every object is a content-addressed
// blob named by its sha256, stored at objects/<aa>/<bb>/<oid>. Nexus has
// hosted Git LFS only, and so does this. The lock API answers 501, which
// git-lfs tolerates (it then skips lock verification for the remote).
package gitlfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// MediaType is the Batch API's media type (Accept and Content-Type).
const MediaType = "application/vnd.git-lfs+json"

// batchLimit bounds a batch request body; git-lfs sends 100 objects per
// batch, a few hundred bytes each.
const batchLimit = 8 << 20

// expiresIn is what the actions say about the hrefs' lifetime; they carry
// no credentials, so the client only reuses its own.
const expiresIn = 3600

// Handler serves gitlfs repositories.
type Handler struct {
	Store   *repo.Store
	Auth    *auth.Authenticator // upload batches need Add; nil permits (unit tests)
	BaseURL func() string       // the prefix of the hrefs the batch answers with
}

func New(store *repo.Store, authn *auth.Authenticator, baseURL func() string) *Handler {
	return &Handler{Store: store, Auth: authn, BaseURL: baseURL}
}

// ObjectPath is where the object oid is stored: objects/<aa>/<bb>/<oid>.
func ObjectPath(oid string) string { return "objects/" + oid[:2] + "/" + oid[2:4] + "/" + oid }

// ValidOID reports whether s is a lower-case hex sha256.
func ValidOID(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}

// route strips the optional info/lfs/ prefix: lfs.url may be
// <base>/repository/<repo>/info/lfs (git-lfs's convention, Nexus's too) or
// <base>/repository/<repo>/.
func route(p string) string { return strings.TrimPrefix(p, "info/lfs/") }

// Access (repo.AccessDecider): a batch needs Read for download and Add for
// upload, which only the handler can tell (the operation is in the body),
// so it checks itself and answers the 401 with the LFS-Authenticate
// challenge git-lfs looks for; verify and locks are reads.
func (h *Handler) Access(r *http.Request, rp *repo.Repository, p string) auth.Access {
	switch r.Method {
	case http.MethodPost:
		if route(p) == "objects/batch" {
			return auth.Public
		}
		return auth.Read
	case http.MethodGet, http.MethodHead:
		return auth.Read
	case http.MethodDelete:
		return auth.Delete
	}
	return auth.Add
}

// denied is the handler's own 401 (anonymous) or 403.
func denied(ctx context.Context, rp *repo.Repository) error {
	if p := auth.FromContext(ctx); !p.Anonymous() {
		return repo.Errorf(http.StatusForbidden, "user %q is not allowed to do this in %q", p.User, rp.Name)
	}
	return repo.Errorf(http.StatusUnauthorized, "authentication required")
}

// WriteError (repo.ErrorWriter) renders err as {"message": ...}; a 401
// carries the LFS-Authenticate challenge git-lfs looks for.
func (h *Handler) WriteError(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := repo.ErrorMessage(r, err)
	repo.SetAllow(w, err)
	if status == http.StatusUnauthorized {
		const realm = `Basic realm="commandcenter"`
		w.Header().Set("WWW-Authenticate", realm)
		w.Header().Set("LFS-Authenticate", realm)
	}
	writeJSON(w, status, map[string]any{"message": msg, "request_id": r.Header.Get("X-Request-Id")})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", MediaType)
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) {
	err := h.serve(w, r, rp, route(p))
	if err != nil {
		h.WriteError(w, r, err)
	}
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) error {
	if rp.Type != "hosted" { // config validation refuses the others; defensive
		return repo.Errorf(http.StatusNotFound, "gitlfs repository %q is not hosted", rp.Name)
	}
	oid, isObject := strings.CutPrefix(p, "objects/")
	switch {
	case p == "objects/batch":
		if r.Method != http.MethodPost {
			return repo.MethodNotAllowed("POST", "method %s not allowed", r.Method)
		}
		return h.batch(w, r, rp)
	case p == "objects/verify":
		if r.Method != http.MethodPost {
			return repo.MethodNotAllowed("POST", "method %s not allowed", r.Method)
		}
		return h.verify(w, r, rp)
	case p == "locks" || strings.HasPrefix(p, "locks/"):
		// Not an error of ours to log: git-lfs asks once, then skips locks.
		writeJSON(w, http.StatusNotImplemented, map[string]string{"message": "locks are not supported"})
		return nil
	case isObject && ValidOID(oid):
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			a, err := h.Store.Get(r.Context(), rp.Name, ObjectPath(oid))
			if err != nil {
				return err
			}
			repo.ServeAsset(w, r, h.Store, a)
			return nil
		case http.MethodPut:
			return h.put(w, r, rp, oid)
		case http.MethodDelete:
			return h.delete(w, r, rp, oid)
		}
		return repo.MethodNotAllowed("GET, HEAD, PUT, DELETE", "method %s not allowed", r.Method)
	}
	return repo.ErrNotFound
}

// Batch API request and response (https://github.com/git-lfs/git-lfs/blob/main/docs/api/batch.md).
type batchRequest struct {
	Operation string   `json:"operation"`
	Transfers []string `json:"transfers"`
	HashAlgo  string   `json:"hash_algo"`
	Objects   []object `json:"objects"`
}

type object struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type batchObject struct {
	OID           string             `json:"oid"`
	Size          int64              `json:"size"`
	Authenticated bool               `json:"authenticated,omitempty"`
	Actions       map[string]*action `json:"actions,omitempty"`
	Error         *objectError       `json:"error,omitempty"`
}

type action struct {
	Href      string `json:"href"`
	ExpiresIn int    `json:"expires_in,omitempty"`
}

type objectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, batchLimit)).Decode(v); err != nil {
		return repo.Errorf(http.StatusBadRequest, "invalid request body: %v", err)
	}
	return nil
}

// batch answers which objects exist (download) or are wanted (upload).
// Download needs Read, upload Add (Access leaves both to it).
func (h *Handler) batch(w http.ResponseWriter, r *http.Request, rp *repo.Repository) error {
	var req batchRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	ctx := r.Context()
	switch req.Operation {
	case "download":
		if !h.Auth.Permits(ctx, rp.Name, auth.Read) {
			return denied(ctx, rp)
		}
	case "upload":
		if !h.Auth.Permits(ctx, rp.Name, auth.Add) {
			return denied(ctx, rp)
		}
		if rp.WritePolicy == "deny" {
			return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
		}
	default:
		return repo.Errorf(http.StatusUnprocessableEntity, "unknown operation %q", req.Operation)
	}
	if len(req.Transfers) > 0 && !contains(req.Transfers, "basic") {
		return repo.Errorf(http.StatusUnprocessableEntity, "only the basic transfer is supported")
	}
	if req.HashAlgo != "" && req.HashAlgo != "sha256" {
		return repo.Errorf(http.StatusUnprocessableEntity, "only sha256 objects are supported")
	}
	base := h.BaseURL() + "/repository/" + rp.Name + "/objects/"
	out := make([]batchObject, 0, len(req.Objects))
	for _, o := range req.Objects {
		bo := batchObject{OID: o.OID, Size: o.Size, Authenticated: true}
		if !ValidOID(o.OID) || o.Size < 0 {
			bo.Error = &objectError{Code: http.StatusUnprocessableEntity, Message: "invalid oid or size"}
			out = append(out, bo)
			continue
		}
		switch a, err := h.Store.Get(ctx, rp.Name, ObjectPath(o.OID)); {
		case err == nil && req.Operation == "download":
			bo.Size = a.Size
			bo.Actions = map[string]*action{"download": {Href: base + o.OID, ExpiresIn: expiresIn}}
		case err == nil: // upload of an object the repository has: nothing to do
		case !errors.Is(err, repo.ErrNotFound):
			return err
		case req.Operation == "download":
			bo.Error = &objectError{Code: http.StatusNotFound, Message: "object does not exist"}
		default:
			bo.Actions = map[string]*action{
				"upload": {Href: base + o.OID, ExpiresIn: expiresIn},
				"verify": {Href: base + "verify", ExpiresIn: expiresIn},
			}
		}
		out = append(out, bo)
	}
	writeJSON(w, http.StatusOK, map[string]any{"transfer": "basic", "objects": out, "hash_algo": "sha256"})
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// put stores the body as oid, refusing content whose sha256 is not oid.
// A re-upload of an object is idempotent under every writePolicy but deny:
// the same oid is the same bytes.
func (h *Handler) put(w http.ResponseWriter, r *http.Request, rp *repo.Repository, oid string) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	ctx := r.Context()
	sums, err := h.Store.WriteBlob(rp.Name, r.Body, func(s blob.Sums) error {
		if s.SHA256 != oid {
			return repo.Errorf(http.StatusUnprocessableEntity, "uploaded content has sha256 %s, not %s", s.SHA256, oid)
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, ObjectPath(oid), "application/octet-stream", sums))
	})
	if err != nil {
		return err
	}
	h.Store.Emit(r, "upload", rp.Name, oid, fmt.Sprintf("size=%d", sums.Size))
	w.WriteHeader(http.StatusOK)
	return nil
}

// verify answers 200 when the object is stored with the given size.
func (h *Handler) verify(w http.ResponseWriter, r *http.Request, rp *repo.Repository) error {
	var o object
	if err := decode(r, &o); err != nil {
		return err
	}
	if !ValidOID(o.OID) {
		return repo.Errorf(http.StatusUnprocessableEntity, "invalid oid %q", o.OID)
	}
	a, err := h.Store.Get(r.Context(), rp.Name, ObjectPath(o.OID))
	switch {
	case errors.Is(err, repo.ErrNotFound):
		return repo.Errorf(http.StatusUnprocessableEntity, "object %s is not stored", o.OID)
	case err != nil:
		return err
	case a.Size != o.Size:
		return repo.Errorf(http.StatusUnprocessableEntity, "object %s has size %d, not %d", o.OID, a.Size, o.Size)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request, rp *repo.Repository, oid string) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	if err := h.Store.Delete(r.Context(), rp.Name, ObjectPath(oid)); err != nil {
		return err
	}
	h.Store.Emit(r, "delete", rp.Name, oid, "")
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// DeleteComponent (cleanup.ComponentDeleter) removes one object: a gitlfs
// component is the object its Name (the oid) names, without a version. An
// object already gone is not an error; one that changed since cleanup
// evaluated it is skipped (cleanup.CheckUnchanged). Cleanup ignores
// writePolicy.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "gitlfs" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("gitlfs component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if !ValidOID(c.Name) || c.Version != "" {
		return component.Result{}, fmt.Errorf("gitlfs component %q must be an object id (sha256) without a version", c.Label())
	}
	var res component.Result
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		res = component.Result{}
		a, err := tx.GetAsset(ctx, rp.Name, ObjectPath(c.Name))
		if errors.Is(err, meta.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := cleanup.CheckUnchanged(c, []meta.Asset{a}); err != nil {
			return err
		}
		size, err := tx.DeleteAsset(ctx, rp.Name, a.Path)
		if err != nil {
			return err
		}
		res.Add(size)
		return nil
	})
	if err != nil {
		return component.Result{}, err
	}
	if res.Assets > 0 {
		h.Store.EmitAs("delete", actor, rp.Name, c.Label(), "assets=1")
	}
	return res, nil
}
