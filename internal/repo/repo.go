// Package repo holds the repository model shared by all formats.
package repo

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
)

// Repository is a configured repository with its references resolved.
type Repository struct {
	config.Repository
	MemberRepos []*Repository // group members in lookup order
	WriteTarget *Repository   // group writeMember
	RemoteURL   *url.URL      // proxy remote; Path always ends with "/"
	// Gen identifies the incarnation: the registry changes it when the
	// repository is created again or its remote changes. Zero outside a registry.
	Gen uint64
}

// Build resolves repository references. cfg must already be validated.
func Build(cfg *config.Config) map[string]*Repository { return buildMap(cfg.Repositories) }

// buildMap resolves the references of a validated definition list.
func buildMap(defs []config.Repository) map[string]*Repository {
	m := make(map[string]*Repository, len(defs))
	for _, rc := range defs {
		r := &Repository{Repository: rc}
		if rc.Type == "proxy" {
			u, _ := url.Parse(rc.Remote)
			if !strings.HasSuffix(u.Path, "/") {
				u.Path += "/"
			}
			r.RemoteURL = u
		}
		m[rc.Name] = r
	}
	for _, r := range m {
		for _, name := range r.Members {
			r.MemberRepos = append(r.MemberRepos, m[name])
		}
		if r.WriteMember != "" {
			r.WriteTarget = m[r.WriteMember]
		}
	}
	return m
}

// OnlineMembers returns the group members that serve requests, in lookup
// order. A group answers as if its offline members did not exist; MemberRepos
// still lists them for the UI and the health check. npm and PyPI groups walk
// MemberRepos instead: an offline member ahead of the one that has the
// package is a 503 (resolve.Owner).
func (r *Repository) OnlineMembers() []*Repository {
	out := make([]*Repository, 0, len(r.MemberRepos))
	for _, m := range r.MemberRepos {
		if m.IsOnline() {
			out = append(out, m)
		}
	}
	return out
}

// MembersOffline reports whether r is a group with members none of which can
// serve: each is offline or, a group itself, MembersOffline.
func (r *Repository) MembersOffline() bool {
	if len(r.MemberRepos) == 0 {
		return false
	}
	for _, m := range r.MemberRepos {
		if m.IsOnline() && !m.MembersOffline() {
			return false
		}
	}
	return true
}

// Handler serves requests for every repository of one format.
// path is the part of the URL after /repository/{name}/, unescaped.
type Handler interface {
	Serve(w http.ResponseWriter, r *http.Request, rp *Repository, path string)
}

// AccessDecider lets a Handler override the default rule
// (GET/HEAD need Read, DELETE needs Delete, everything else needs Add). Write
// kinds (Add, Edit, Delete) on a group are checked against its writeMember.
type AccessDecider interface {
	Access(r *http.Request, rp *Repository, path string) auth.Access
}

// Error is an error that maps to an HTTP status code.
type Error struct {
	Status int
	Msg    string
	Allow  string // the Allow header of a 405 (RFC 9110 §15.5.6); see MethodNotAllowed

	allowKnown bool // Allow lists the resource's methods, even when empty
}

func (e *Error) Error() string { return e.Msg }

func Errorf(status int, format string, args ...any) error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// MethodNotAllowed is a 405 that lists the methods the resource does accept.
// An empty allow means it accepts none (an empty Allow header).
func MethodNotAllowed(allow, format string, args ...any) error {
	return &Error{Status: http.StatusMethodNotAllowed, Msg: fmt.Sprintf(format, args...), Allow: allow, allowKnown: true}
}

// WithAllow returns err with allow as the methods its resource accepts when
// err is a 405 that does not list them (e.g. WriteTarget's); otherwise err.
func WithAllow(err error, allow string) error {
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusMethodNotAllowed || e.allowKnown {
		return err
	}
	return &Error{Status: e.Status, Msg: e.Msg, Allow: allow, allowKnown: true}
}

// SetAllow adds the Allow header of a 405 whose methods are known: made with
// MethodNotAllowed or WithAllow. A 405 whose methods are unknown gets no
// Allow header rather than a wrong one (final review: a GET answered with
// 405 must not be told GET is allowed). A header the handler set already is
// kept.
func SetAllow(w http.ResponseWriter, err error) {
	if StatusOf(err) != http.StatusMethodNotAllowed {
		return
	}
	if _, set := w.Header()["Allow"]; set {
		return
	}
	var e *Error
	if errors.As(err, &e) && e.allowKnown {
		w.Header()["Allow"] = []string{e.Allow}
	}
}

var ErrNotFound error = &Error{Status: http.StatusNotFound, Msg: "not found"}

// StatusOf maps an error to an HTTP status code.
func StatusOf(err error) int {
	var e *Error
	var ve *ValidationError
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &e):
		return e.Status
	case errors.As(err, &ve):
		return http.StatusBadRequest
	case errors.Is(err, meta.ErrNotFound), errors.Is(err, blob.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, meta.ErrInvalidText): // a name PostgreSQL cannot store
		return http.StatusBadRequest
	case errors.As(err, &tooBig):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, os.ErrDeadlineExceeded): // the request body stalled (IdleBody)
		return http.StatusRequestTimeout
	case errors.Is(err, blob.ErrDiskFull):
		return http.StatusInsufficientStorage
	}
	return http.StatusInternalServerError
}

// ErrorWriter lets a Handler render errors, including the server's 401 and 403
// responses, in its clients' format.
type ErrorWriter interface {
	WriteError(w http.ResponseWriter, r *http.Request, err error)
}

// ErrorMessage returns the status and the client-safe message for err.
// Server errors are logged and internal details are not exposed.
func ErrorMessage(r *http.Request, err error) (int, string) {
	status := StatusOf(err)
	msg := err.Error()
	if status >= 500 {
		slog.Error("request failed", "id", r.Header.Get("X-Request-Id"), "method", r.Method, "path", logsafe.Path(r.URL.Path), "err", logsafe.Err(err))
		if status == http.StatusInternalServerError {
			msg = "internal error (request id " + r.Header.Get("X-Request-Id") + ")"
		}
	}
	return status, msg
}

// WriteError writes err as a plain-text response.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := ErrorMessage(r, err)
	SetAllow(w, err)
	http.Error(w, msg, status)
}

// WriteTarget returns the hosted repository that receives writes for rp:
// a group's writeMember, or rp itself when it is hosted.
func WriteTarget(rp *Repository) (*Repository, error) {
	if rp.Type == "group" {
		if rp.WriteTarget == nil {
			return nil, Errorf(http.StatusMethodNotAllowed, "group repository %q has no writeMember", rp.Name)
		}
		if !rp.WriteTarget.IsOnline() { // final review M4: offline, not missing
			return nil, Errorf(http.StatusServiceUnavailable, "repository is offline")
		}
		rp = rp.WriteTarget
	}
	if rp.Type != "hosted" {
		return nil, Errorf(http.StatusMethodNotAllowed, "repository %q does not accept uploads", rp.Name)
	}
	return rp, nil
}

// AcceptsWrites reports whether writes to rp reach a hosted repository, for
// a 405's Allow: hosted, or a group with a writeMember (an offline one
// counts; the method is allowed, the repository is just down).
func AcceptsWrites(rp *Repository) bool {
	_, err := WriteTarget(rp)
	return err == nil || StatusOf(err) != http.StatusMethodNotAllowed
}
