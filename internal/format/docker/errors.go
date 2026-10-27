package docker

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"commandcenter/internal/repo"
)

// Error is a registry error: the Distribution error codes, rendered as
// {"errors":[{"code","message","detail"}]}. Any other error is mapped by
// its HTTP status (repo.StatusOf).
type Error struct {
	Status  int
	Code    string
	Message string
	Detail  any
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Unwrap gives repo.StatusOf the status.
func (e *Error) Unwrap() error { return &repo.Error{Status: e.Status, Msg: e.Message} }

// Standard codes (Distribution and OCI distribution spec).
const (
	CodeBlobUnknown         = "BLOB_UNKNOWN"
	CodeBlobUploadInvalid   = "BLOB_UPLOAD_INVALID"
	CodeBlobUploadUnknown   = "BLOB_UPLOAD_UNKNOWN"
	CodeDigestInvalid       = "DIGEST_INVALID"
	CodeManifestBlobUnknown = "MANIFEST_BLOB_UNKNOWN"
	CodeManifestInvalid     = "MANIFEST_INVALID"
	CodeManifestUnknown     = "MANIFEST_UNKNOWN"
	CodeNameInvalid         = "NAME_INVALID"
	CodeNameUnknown         = "NAME_UNKNOWN"
	CodeSizeInvalid         = "SIZE_INVALID"
	CodeUnauthorized        = "UNAUTHORIZED"
	CodeDenied              = "DENIED"
	CodeUnsupported         = "UNSUPPORTED"
	CodeTooManyRequests     = "TOOMANYREQUESTS"
)

// Err makes a registry error.
func Err(status int, code, msg string) error { return &Error{Status: status, Code: code, Message: msg} }

func errUnknownName(name string) error {
	return Err(http.StatusNotFound, CodeNameUnknown, "repository name not known to registry: "+name)
}

// codeFor maps a status to the code of an error that has none.
func codeFor(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusForbidden:
		return CodeDenied
	case http.StatusTooManyRequests:
		return CodeTooManyRequests
	case http.StatusMethodNotAllowed, http.StatusNotImplemented, http.StatusUnsupportedMediaType:
		return CodeUnsupported
	}
	return "UNKNOWN"
}

// WriteError implements repo.ErrorWriter: every error, the server's 401 and
// 403 included, in the registry's JSON. A 401 carries the Bearer challenge
// for r (the server's Basic one is replaced), except on the token endpoint
// itself, whose credentials are Basic.
func (h *Handler) WriteError(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := repo.ErrorMessage(r, err)
	code, detail := codeFor(status), any(nil)
	var e *Error
	if errors.As(err, &e) {
		code, detail, msg = e.Code, e.Detail, e.Message
	}
	if status == http.StatusUnauthorized && strings.TrimPrefix(r.URL.Path, "/v2/") != "token" {
		w.Header().Set("WWW-Authenticate", h.challenge(r))
	}
	repo.SetAllow(w, err)
	writeJSON(w, status, map[string]any{"errors": []map[string]any{{"code": code, "message": msg, "detail": detail}}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
