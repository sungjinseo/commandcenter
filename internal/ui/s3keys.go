package ui

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
)

// S3 access keys (auth/s3key.go). Anyone signed in lists their own keys;
// creating one needs a password or session login plus the UI header (a
// token never mints one). Deleting needs the owner or an admin. The secret
// is answered once, on creation.

func (u *UI) s3KeyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/s3keys", u.listS3Keys)
	mux.HandleFunc("POST /-/api/s3keys", u.createS3Key)
	mux.HandleFunc("DELETE /-/api/s3keys/{id}", u.deleteS3Key)
}

func s3KeyJSON(k meta.S3Key, now time.Time) map[string]any {
	return map[string]any{"accessKeyId": k.AccessKeyID, "owner": k.Owner, "name": k.Name, "created": rfc3339(k.Created),
		"expires": rfc3339(k.Expires), "expired": !k.Expires.IsZero() && !now.Before(k.Expires), "lastUsed": rfc3339(k.LastUsed)}
}

// s3Endpoint is the URL S3 clients use (--endpoint-url), as far as the
// configuration tells: the S3 host under baseURL's scheme, else baseURL's
// host on the S3 port. "" when the S3 API is off.
func (u *UI) s3Endpoint() string {
	base := ""
	if u.BaseURL != nil {
		base = u.BaseURL()
	} else {
		base = u.Auth.Snapshot().Settings().BaseURL
	}
	b, err := url.Parse(base)
	switch {
	case u.S3.Host != "" && err == nil:
		return b.Scheme + "://" + u.S3.Host
	case u.S3.Listen != "" && err == nil:
		_, port, _ := net.SplitHostPort(u.S3.Listen)
		return "http://" + net.JoinHostPort(b.Hostname(), port)
	}
	return ""
}

func (u *UI) listS3Keys(w http.ResponseWriter, r *http.Request) {
	p, ok := requireUser(w, r)
	if !ok {
		return
	}
	owner := p.Owner()
	if r.URL.Query().Get("all") == "1" {
		if !p.Admin {
			writeError(w, http.StatusForbidden, "only an admin lists every S3 key")
			return
		}
		owner = ""
	}
	list, err := u.Auth.ListS3Keys(r.Context(), owner)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	now := u.clock()
	out := make([]map[string]any, 0, len(list))
	for _, k := range list {
		out = append(out, s3KeyJSON(k, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"s3Keys": out, "max": auth.MaxS3KeysPerUser,
		"enabled": u.S3.Enabled(), "endpoint": u.s3Endpoint(), "region": u.S3.Region})
}

func (u *UI) createS3Key(w http.ResponseWriter, r *http.Request) {
	p, ok := requireInteractive(w, r)
	if !ok || !requireUIHeader(w, r) {
		return
	}
	var body struct {
		Name    string `json:"name"`
		Expires string `json:"expires"` // RFC 3339 or a date (yyyy-mm-dd); empty: never
	}
	if r.ContentLength != 0 && !decodeBody(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if len(body.Name) > 100 {
		writeError(w, http.StatusBadRequest, "the name may be at most 100 characters")
		return
	}
	var exp time.Time
	if body.Expires != "" {
		var err error
		if exp, err = time.Parse(time.RFC3339, body.Expires); err != nil {
			if exp, err = time.Parse(time.DateOnly, body.Expires); err != nil {
				writeError(w, http.StatusBadRequest, "expires must be an RFC 3339 time or a date (yyyy-mm-dd)")
				return
			}
		}
		if !exp.After(u.clock()) {
			writeError(w, http.StatusBadRequest, "expires must be in the future")
			return
		}
	}
	k, secret, err := u.Auth.CreateS3Key(r.Context(), p.Owner(), body.Name, exp)
	switch {
	case errors.Is(err, auth.ErrTooManyS3Keys):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, auth.ErrUserDisabled):
		writeError(w, http.StatusConflict, "the user is unknown or disabled")
		return
	case err != nil:
		u.fail(w, r, err)
		return
	}
	u.emit(r, "s3key.create", "", p.Owner(), "access key "+k.AccessKeyID)
	out := s3KeyJSON(k, u.clock())
	out["secretAccessKey"] = secret
	out["endpoint"], out["region"] = u.s3Endpoint(), u.S3.Region
	writeJSON(w, http.StatusCreated, out)
}

func (u *UI) deleteS3Key(w http.ResponseWriter, r *http.Request) {
	p, ok := requireUser(w, r)
	if !ok || !requireUIHeader(w, r) {
		return
	}
	id := r.PathValue("id")
	keys, err := u.Auth.ListS3Keys(r.Context(), "")
	if err != nil {
		u.fail(w, r, err)
		return
	}
	owner := ""
	for _, k := range keys {
		if k.AccessKeyID == id {
			owner = k.Owner
		}
	}
	// Another user's key looks missing to whoever may not delete it.
	if owner == "" || owner != p.Owner() && !p.Admin {
		writeError(w, http.StatusNotFound, "no such S3 key")
		return
	}
	if _, err := u.Auth.DeleteS3Key(r.Context(), id); err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such S3 key")
			return
		}
		u.fail(w, r, err)
		return
	}
	u.emit(r, "s3key.delete", "", owner, "access key "+id)
	w.WriteHeader(http.StatusNoContent)
}
