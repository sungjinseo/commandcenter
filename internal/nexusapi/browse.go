package nexusapi

import (
	"html"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// BrowsePrefix is where Nexus's HTML directory listing of a repository's
// tree is, outside the v1 prefix (the server routes it here).
const BrowsePrefix = "/service/rest/repository/browse/"

// browseLimit bounds a listing, as the web UI's browse does; the page says
// when it stopped there.
const browseLimit = 1000

// browseHTML is GET /service/rest/repository/browse/<repo>/<dir>/: the
// directory's children as an HTML table, a directory linking to its own
// listing and a file to its download. The root lists the repositories the
// caller may browse; a path without its trailing "/" is redirected (303)
// to it, as Nexus does. A group lists its members' trees merged. Browse
// privilege; a repository the caller may not browse is not found.
func (h *Handler) browseHTML(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	rest := strings.TrimPrefix(r.URL.Path, BrowsePrefix)
	name, dir, hasSlash := strings.Cut(rest, "/")
	if name != "" && (!hasSlash || dir != "" && !strings.HasSuffix(dir, "/")) {
		http.Redirect(w, r, r.URL.EscapedPath()+"/", http.StatusSeeOther)
		return
	}
	var rows []browseRow
	truncated := false
	if name == "" {
		for _, rp := range h.Repos.Snapshot().Sorted {
			if h.Auth.Can(p, rp.Name, auth.Browse) {
				rows = append(rows, browseRow{name: rp.Name, href: url.PathEscape(rp.Name) + "/", dir: true})
			}
		}
		if len(rows) == 0 && p.Anonymous() {
			deny(w, p)
			return
		}
	} else {
		rp := h.Repos.Get(name)
		if rp == nil || !h.Auth.Can(p, name, auth.Browse) {
			if p.Anonymous() && !h.Auth.Can(p, name, auth.Browse) { // as /repository/: a hidden name is not told from a missing one
				deny(w, p)
				return
			}
			browsePage(w, http.StatusNotFound, "Repository not found", nil, false)
			return
		}
		entries, more, err := h.listDir(r, rp, dir)
		if err != nil {
			writeMessage(w, http.StatusInternalServerError, "could not list the directory")
			return
		}
		if len(entries) == 0 && dir != "" {
			browsePage(w, http.StatusNotFound, "Path not found", nil, false)
			return
		}
		truncated = more
		rows = append(rows, browseRow{name: "Parent Directory", href: "../", dir: true})
		for _, e := range entries {
			if e.Dir {
				rows = append(rows, browseRow{name: e.Name, href: url.PathEscape(e.Name) + "/", dir: true})
				continue
			}
			href := h.repoURL(rp.Name) + "/" + escapeSegments(dir+e.Name)
			rows = append(rows, browseRow{name: e.Name, href: href, size: e.Size, modified: e.LastModified})
		}
	}
	title := "Index of /" + name
	if dir != "" {
		title += "/" + strings.TrimSuffix(dir, "/")
	}
	browsePage(w, http.StatusOK, title, rows, truncated)
}

// listDir lists dir in rp, or for a group in each of its members (nested
// groups included), merged by name.
func (h *Handler) listDir(r *http.Request, rp *repo.Repository, dir string) ([]meta.DirEntry, bool, error) {
	if rp.Type != "group" {
		return h.DB.ListDir(r.Context(), rp.Name, dir, browseLimit)
	}
	seen := map[string]bool{}
	var out []meta.DirEntry
	truncated := false
	for _, m := range rp.MemberRepos {
		entries, more, err := h.listDir(r, m, dir)
		if err != nil {
			return nil, false, err
		}
		truncated = truncated || more
		for _, e := range entries {
			if key := strconv.FormatBool(e.Dir) + e.Name; !seen[key] {
				seen[key] = true
				out = append(out, e)
			}
		}
	}
	slices.SortStableFunc(out, func(a, b meta.DirEntry) int { return strings.Compare(a.Name, b.Name) })
	if len(out) > browseLimit {
		out, truncated = out[:browseLimit], true
	}
	return out, truncated, nil
}

func escapeSegments(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

type browseRow struct {
	name, href string
	dir        bool
	size       int64
	modified   time.Time
}

func browsePage(w http.ResponseWriter, status int, title string, rows []browseRow, truncated bool) {
	var sb strings.Builder
	t := html.EscapeString(title)
	sb.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head><meta charset=\"utf-8\"><title>" + t + "</title></head>\n<body>\n<h1>" + t + "</h1>\n")
	if status == http.StatusOK {
		sb.WriteString("<table>\n<tr><th>Name</th><th>Last Modified</th><th>Size</th><th>Description</th></tr>\n")
		for _, r := range rows {
			name, modified, size := html.EscapeString(r.name), "", ""
			if r.dir && r.name != "Parent Directory" {
				name += "/"
			}
			if !r.dir {
				modified, size = r.modified.UTC().Format(time.RFC1123), strconv.FormatInt(r.size, 10)
			}
			sb.WriteString("<tr><td><a href=\"" + html.EscapeString(r.href) + "\">" + name + "</a></td><td>" + modified + "</td><td>" + size + "</td><td></td></tr>\n")
		}
		sb.WriteString("</table>\n")
		if truncated {
			sb.WriteString("<p>The listing stops at " + strconv.Itoa(browseLimit) + " entries.</p>\n")
		}
	}
	sb.WriteString("</body>\n</html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.WriteHeader(status)
	w.Write([]byte(sb.String()))
}
