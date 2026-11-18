package ui

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"io"
	"net/http"

	"commandcenter/internal/meta"
)

// S3 access keys (internal/s3api): an access key id and a secret, which
// S3 clients sign requests with. As with API tokens, anyone signed in lists
// their own keys (an admin every key), and creating or revoking one needs a
// password or session login plus the UI header (a key never mints one). An
// admin creates keys for others. The secret is answered once, on creation,
// and stored sealed with the secret key.

func (u *UI) s3KeyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/s3keys", u.listS3Keys)
	mux.HandleFunc("POST /-/api/s3keys", u.createS3Key)
	mux.HandleFunc("DELETE /-/api/s3keys/{id}", u.deleteS3Key)
}

func s3KeyJSON(k meta.S3Key) map[string]any {
	id, source := splitOwner(k.Owner)
	return map[string]any{"accessKeyId": k.ID, "user": k.Owner, "userId": id, "source": source,
		"created": rfc3339(k.Created), "lastUsed": rfc3339(k.LastUsed)}
}

func (u *UI) listS3Keys(w http.ResponseWriter, r *http.Request) {
	p, ok := requireUser(w, r)
	if !ok {
		return
	}
	owner := p.Owner()
	if p.Admin {
		owner = "" // all users
	}
	keys, err := u.DB.ListS3Keys(r.Context(), owner)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, s3KeyJSON(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"s3Keys": out})
}

// createS3Key creates a key for the caller, or with {"user": "<owner>"}
// (an admin only) for that principal: a stored user's id, or an owner key
// such as "ldap:alice" of a user the realm still knows.
func (u *UI) createS3Key(w http.ResponseWriter, r *http.Request) {
	p, ok := requireInteractive(w, r)
	if !ok || !requireUIHeader(w, r) {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var body struct {
		User string `json:"user"`
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if !decodeBody(w, r, &body) {
			return
		}
	}
	owner := p.Owner()
	if body.User != "" && body.User != owner {
		if !p.Admin {
			writeError(w, http.StatusForbidden, "only an admin can create S3 access keys for other users")
			return
		}
		if _, ok := u.Auth.Lookup(body.User); !ok {
			writeError(w, http.StatusBadRequest, "unknown or disabled user")
			return
		}
		owner = body.User
	}
	id, secret := make([]byte, 15), make([]byte, 30)
	rand.Read(id)
	rand.Read(secret)
	// AWS's shapes: a 20-character id of capitals and digits, a
	// 40-character secret.
	k := meta.S3Key{ID: "CC" + base32.StdEncoding.EncodeToString(id)[:18], Owner: owner,
		Secret: base64.StdEncoding.EncodeToString(secret), Created: u.clock()}
	err = u.DB.CreateS3Key(r.Context(), k)
	switch {
	case errors.Is(err, meta.ErrTooManyS3Keys):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		u.fail(w, r, err)
		return
	}
	u.emit(r, "s3key.create", "", owner, "id "+k.ID)
	out := s3KeyJSON(k)
	out["secretAccessKey"] = k.Secret
	writeJSON(w, http.StatusCreated, out)
}

func (u *UI) deleteS3Key(w http.ResponseWriter, r *http.Request) {
	p, ok := requireInteractive(w, r)
	if !ok || !requireUIHeader(w, r) {
		return
	}
	owner := p.Owner()
	if p.Admin {
		owner = ""
	}
	// Found among the caller's keys (an admin's: all), so another user's
	// key id is not leaked; a key whose secret does not open is revoked too.
	keys, err := u.DB.ListS3Keys(r.Context(), owner)
	var k meta.S3Key
	if err == nil {
		err = meta.ErrNotFound
		for _, c := range keys {
			if c.ID == r.PathValue("id") {
				k, err = c, nil
			}
		}
	}
	if err == nil {
		err = u.DB.DeleteS3Key(r.Context(), k.ID)
	}
	switch {
	case errors.Is(err, meta.ErrNotFound):
		writeError(w, http.StatusNotFound, "S3 access key not found")
	case err != nil:
		u.fail(w, r, err)
	default:
		u.emit(r, "s3key.revoke", "", k.Owner, "id "+k.ID)
		w.WriteHeader(http.StatusNoContent)
	}
}
