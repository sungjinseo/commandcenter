// Package raw implements the raw (generic) format: any file at any path, as
// a web server would hold it. A hosted repository stores what is PUT, a
// proxy caches whatever its remote serves (web pages included: raw remotes
// are plain web sites) and a group answers from the first member that has
// the path. Nothing is derived from the content: a component is one file.
package raw

import (
	"encoding/json"
	"errors"
	"html"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
)

// Handler serves raw repositories.
type Handler struct {
	Store    *repo.Store
	Resolver *resolve.Resolver
	locks    repo.KeyedMutex // serializes the writePolicy check and the write per path

	// Auth checks overwrites for Edit; nil leaves every check to the server.
	Auth *auth.Authenticator
}

// listLimit bounds a directory listing's entries; the listing says when
// there are more.
const listLimit = 1000

func New(store *repo.Store, resolver *resolve.Resolver) *Handler {
	return &Handler{Store: store, Resolver: resolver}
}

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) {
	if err := checkPath(p); err != nil {
		repo.WriteError(w, r, err)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.get(w, r, rp, p)
	case http.MethodPut, http.MethodDelete:
		target, err := repo.WriteTarget(rp)
		if err != nil {
			repo.WriteError(w, r, repo.WithAllow(err, "GET, HEAD"))
			return
		}
		if r.Method == http.MethodPut {
			err = validateWritePath(p)
			if err == nil {
				err = h.put(r, target, p)
			}
			if err == nil {
				w.WriteHeader(http.StatusCreated)
			}
		} else {
			err = h.delete(r, target, p)
			if err == nil {
				w.WriteHeader(http.StatusNoContent)
			}
		}
		if err != nil {
			repo.WriteError(w, r, err)
		}
	default:
		allow := "GET, HEAD"
		if repo.AcceptsWrites(rp) {
			allow += ", PUT, DELETE"
		}
		repo.WriteError(w, r, repo.MethodNotAllowed(allow, "method %s not allowed", r.Method))
	}
}

// isDir reports whether p names a directory: the repository root or a path
// ending in "/".
func isDir(p string) bool { return p == "" || strings.HasSuffix(p, "/") }

// checkPath refuses paths that escape or alias the tree (as Maven does).
// The root ("") and directories ("x/") are left to the method: GET lists them.
func checkPath(p string) error {
	if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || strings.Contains(p, "//") {
		return repo.Errorf(http.StatusBadRequest, "invalid path %q", p)
	}
	for _, s := range strings.Split(p, "/") {
		if s == "." || s == ".." {
			return repo.Errorf(http.StatusBadRequest, "invalid path %q", p)
		}
	}
	return nil
}

// validateWritePath refuses names a new upload may not use, Maven's rule:
// a directory, bytes that are not UTF-8, control characters and URL
// delimiters (query, fragment, matrix parameters).
func validateWritePath(p string) error {
	if isDir(p) || !utf8.ValidString(p) || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '?' || r == '#' || r == ';' }) {
		return repo.Errorf(http.StatusBadRequest, "invalid path %q", p)
	}
	return nil
}

// ContentType returns the media type for a path whose upload named none:
// by extension, else application/octet-stream.
func ContentType(p string) string {
	if ct := mime.TypeByExtension(path.Ext(p)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) {
	if isDir(p) {
		// Only a hosted repository has a tree of its own to list: a proxy's
		// cache is partial, and a remote's listing is a page that would
		// be cached as a file at a directory path.
		if rp.Type != "hosted" {
			repo.WriteError(w, r, repo.ErrNotFound)
			return
		}
		h.list(w, r, rp, p)
		return
	}
	res, err := h.Resolver.LookupTarget(r.Context(), rp, proxy.Target{Key: p, Remote: p}, proxy.Content)
	if err != nil {
		repo.WriteError(w, r, err)
		return
	}
	proxy.SetStaleHeader(w, res)
	a := res.Asset
	if a.ContentType == "" {
		a.ContentType = ContentType(p)
	}
	repo.ServeAsset(w, r, h.Store, a)
}

// list answers a directory: its children as JSON for Accept:
// application/json, else as a web server's HTML index. A directory that has
// no file under it does not exist, except the root.
func (h *Handler) list(w http.ResponseWriter, r *http.Request, rp *repo.Repository, dir string) {
	entries, truncated, err := h.Store.DB.ListDir(r.Context(), rp.Name, dir, listLimit)
	if err != nil {
		repo.WriteError(w, r, err)
		return
	}
	if len(entries) == 0 && dir != "" {
		repo.WriteError(w, r, repo.ErrNotFound)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		type entry struct {
			Name         string `json:"name"`
			Dir          bool   `json:"dir"`
			Size         int64  `json:"size,omitempty"`
			LastModified string `json:"lastModified,omitempty"`
		}
		out := make([]entry, 0, len(entries))
		for _, e := range entries {
			out = append(out, entry{Name: e.Name, Dir: e.Dir, Size: e.Size, LastModified: lastModified(e)})
		}
		b, _ := json.Marshal(map[string]any{"path": dir, "entries": out, "truncated": truncated})
		repo.ServeText(w, r, string(b)+"\n", "application/json", time.Time{})
		return
	}
	title := html.EscapeString("/repository/" + rp.Name + "/" + dir)
	var sb strings.Builder
	sb.WriteString("<!DOCTYPE html>\n<html><head><meta charset=\"utf-8\"><title>Index of " + title + "</title></head>\n<body>\n<h1>Index of " + title + "</h1>\n<ul>\n")
	if dir != "" {
		sb.WriteString("<li><a href=\"../\">../</a></li>\n")
	}
	for _, e := range entries {
		name, href := e.Name, url.PathEscape(e.Name)
		if e.Dir {
			name, href = name+"/", href+"/"
		}
		sb.WriteString("<li><a href=\"" + html.EscapeString(href) + "\">" + html.EscapeString(name) + "</a></li>\n")
	}
	sb.WriteString("</ul>\n")
	if truncated {
		sb.WriteString("<p>(listing truncated)</p>\n")
	}
	sb.WriteString("</body></html>\n")
	repo.ServeText(w, r, sb.String(), "text/html; charset=utf-8", time.Time{})
}

func lastModified(e meta.DirEntry) string {
	if e.Dir || e.LastModified.IsZero() {
		return ""
	}
	return e.LastModified.UTC().Format(time.RFC3339)
}

// put stores the body at p with the upload's Content-Type, or one from the
// extension. The per-path lock makes the writePolicy and privilege checks
// and the write atomic: overwriting an existing file (writePolicy allow)
// needs Edit, a new one only the Add the server checked.
func (h *Handler) put(r *http.Request, rp *repo.Repository, p string) error {
	ctx := r.Context()
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	unlock := h.locks.Lock(rp.Name + "\x00" + p)
	defer unlock()
	_, err := h.Store.Get(ctx, rp.Name, p)
	switch {
	case err == nil && rp.WritePolicy == "allow_once":
		return repo.Errorf(http.StatusBadRequest, "repository %q does not allow updating assets: %s", rp.Name, p)
	case err == nil && !h.Auth.Permits(ctx, rp.Name, auth.Edit):
		return repo.Errorf(http.StatusForbidden, "user %q may not overwrite %s", auth.FromContext(ctx).User, p)
	case err != nil && !errors.Is(err, repo.ErrNotFound):
		return err
	}
	ct := r.Header.Get("Content-Type")
	if _, _, err := mime.ParseMediaType(ct); err != nil {
		ct = ContentType(p)
	}
	if _, err := h.Store.Put(ctx, rp.Name, p, ct, r.Body); err != nil {
		return err
	}
	h.Store.Emit(r, "upload", rp.Name, p, "")
	return nil
}

func (h *Handler) delete(r *http.Request, rp *repo.Repository, p string) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	if isDir(p) {
		return repo.Errorf(http.StatusBadRequest, "invalid path %q", p)
	}
	if err := h.Store.Delete(r.Context(), rp.Name, p); err != nil {
		return err
	}
	h.Store.Emit(r, "delete", rp.Name, p, "")
	return nil
}
