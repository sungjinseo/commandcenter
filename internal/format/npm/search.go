package npm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
	"commandcenter/internal/semver"
)

// The bounds of a search: the page a client may ask for (Nexus's), how many
// packages of a hosted repository it looks through, and the answer of a
// remote that is read into memory for a group.
const (
	searchMaxSize  = 250
	searchScanRows = 2000
	searchBodyMax  = 8 << 20
)

// search answers `npm search` (GET /-/v1/search?text=&size=&from=). A proxy
// passes the request to its remote and the answer back, uncached. A hosted
// repository looks through its own packages (hostedHits). A group gives its
// members' hits in member order, each package name once, and pages over
// that; a proxy member that fails or has no search is left out. What a hit
// names is listed only to a caller who may read it (a selector-only caller,
// whose selector has to match the search path to get here).
func (h *Handler) search(w http.ResponseWriter, r *http.Request, rp *repo.Repository) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return repo.MethodNotAllowed("GET, HEAD", "method %s not allowed", r.Method)
	}
	ctx, q := r.Context(), r.URL.Query()
	if rp.Type == "proxy" {
		resp, err := h.askRemote(ctx, rp, r.URL.RawQuery)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 500 {
			return proxy.ErrRemote
		}
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, err = io.Copy(w, resp.Body)
		return err
	}
	size, from := 20, 0
	if n, err := strconv.Atoi(q.Get("size")); err == nil {
		size = min(max(n, 1), searchMaxSize)
	}
	if n, err := strconv.Atoi(q.Get("from")); err == nil {
		from = min(max(n, 0), searchScanRows)
	}
	want := from + size // of every member: any of them may give the whole page
	q.Set("from", "0")
	q.Set("size", strconv.Itoa(min(want, searchMaxSize)))
	objects := []json.RawMessage{}
	seen := map[string]bool{}
	for _, m := range resolve.Leaves(rp) {
		var hits []json.RawMessage
		var err error
		if m.Type == "proxy" {
			hits, err = h.remoteHits(ctx, m, q.Encode())
			if err != nil && ctx.Err() == nil {
				slog.Warn("npm search: group member left out", "group", rp.Name, "member", m.Name, "err", logsafe.Err(err))
				err = nil
			}
		} else {
			hits, err = h.hostedHits(ctx, rp, m, q.Get("text"), want)
		}
		if err != nil {
			return err
		}
		for _, hit := range hits {
			var o struct {
				Package struct {
					Name string `json:"name"`
				} `json:"package"`
			}
			if json.Unmarshal(hit, &o) != nil || o.Package.Name == "" || seen[o.Package.Name] ||
				!h.Auth.Permits(auth.WithPath(ctx, o.Package.Name), rp.Name, auth.Read) {
				continue
			}
			seen[o.Package.Name] = true
			objects = append(objects, hit)
		}
	}
	page := objects[min(from, len(objects)):min(want, len(objects))]
	writeJSON(w, http.StatusOK, map[string]any{"objects": page, "total": len(page), "time": h.Now().UTC().Format(time.RFC1123)})
	return nil
}

// askRemote sends a search with the query string query to the remote of
// proxy m: like the proxy's other requests, only where its routing rule lets
// it through (not found) and while the remote is not blocked (Request).
func (h *Handler) askRemote(ctx context.Context, m *repo.Repository, query string) (*http.Response, error) {
	const path = "-/v1/search"
	f := h.Resolver.Fetcher
	if !h.routed(m, path) {
		return nil, repo.ErrNotFound
	}
	u := *m.RemoteURL
	u.Path += path
	u.RawQuery = query
	return f.Request(ctx, m, u.String())
}

// remoteHits returns the objects the remote of proxy m finds for query, none
// from a remote without search (it answers 404 or 400).
func (h *Handler) remoteHits(ctx context.Context, m *repo.Repository, query string) ([]json.RawMessage, error) {
	resp, err := h.askRemote(ctx, m, query)
	if repo.StatusOf(err) == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 500:
		return nil, proxy.ErrRemote
	case resp.StatusCode != http.StatusOK:
		return nil, nil
	}
	var body struct {
		Objects []json.RawMessage `json:"objects"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, searchBodyMax)).Decode(&body)
	return body.Objects, err
}

// person is a name and an address of a search result.
type person struct {
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
}

// searchManifest is what a search reads of a version's manifest.
type searchManifest struct {
	Description string            `json:"description"`
	Keywords    json.RawMessage   `json:"keywords"`
	Author      json.RawMessage   `json:"author"`
	Maintainers []json.RawMessage `json:"maintainers"`
	NpmUser     json.RawMessage   `json:"_npmUser"`
	License     json.RawMessage   `json:"license"`
	Homepage    string            `json:"homepage"`
	Repository  json.RawMessage   `json:"repository"`
	Bugs        json.RawMessage   `json:"bugs"`
}

// hostedHits returns the packages of hosted m that match text, by name, at
// most n, for a search of rp: one object per package, of the version its
// latest tag points at, in the registry API's shape (scores are zero:
// nothing is ranked here). Every word of text must match (matches). An empty
// text finds nothing.
// ponytail: the first searchScanRows packages (by name) that hold the first
// plain word anywhere in their manifest are looked through, by a scan of the
// repository's rows; a search index if hosted repositories outgrow that.
func (h *Handler) hostedHits(ctx context.Context, rp, m *repo.Repository, text string, n int) ([]json.RawMessage, error) {
	words := strings.Fields(strings.ToLower(text))
	if len(words) == 0 {
		return nil, nil
	}
	first := "" // as typed: the database folds ASCII only
	for _, w := range strings.Fields(text) {
		if !strings.Contains(w, ":") {
			first = w
			break
		}
	}
	var out []json.RawMessage
	var jsonErr error
	err := h.Store.DB.WalkLatestPackages(ctx, m.Name, first, searchScanRows, func(p meta.Package) bool {
		var man searchManifest
		json.Unmarshal([]byte(p.Attrs), &man) // fields of another type stay empty
		keywords := stringList(man.Keywords)
		people := []person{}
		for _, raw := range man.Maintainers {
			if pe, ok := personOf(raw); ok {
				people = append(people, pe)
			}
		}
		author, hasAuthor := personOf(man.Author)
		if len(people) == 0 && hasAuthor {
			people = append(people, author)
		}
		if !matches(words, p, man.Description, keywords, append(people[:len(people):len(people)], author)) {
			return true
		}
		pkg := map[string]any{
			"name": p.Name, "version": p.Version, "description": man.Description, "keywords": keywords,
			"date": npmTime(p.CreatedAt), "maintainers": people,
			"links": links(h.baseURL()+"/repository/"+rp.Name+"/"+p.Name, man),
		}
		if publisher, ok := personOf(man.NpmUser); ok {
			pkg["publisher"] = publisher
		} else if hasAuthor {
			pkg["publisher"] = author
		}
		var hit []byte
		hit, jsonErr = json.Marshal(map[string]any{
			"package":     pkg,
			"score":       map[string]any{"final": 0, "detail": map[string]any{"quality": 0, "popularity": 0, "maintenance": 0}},
			"searchScore": 0,
		})
		out = append(out, hit)
		return jsonErr == nil && len(out) < n
	})
	if err == nil {
		err = jsonErr
	}
	return out, err
}

// ManifestField returns what the REST search's assets.attributes.npm.<field>
// looks at in a stored version manifest: author, description, keywords or
// license; nothing for another field or one the manifest does not have.
func ManifestField(attrs, field string) []string {
	var man searchManifest
	json.Unmarshal([]byte(attrs), &man) // fields of another type stay empty
	var out []string
	switch field {
	case "author":
		if pe, ok := personOf(man.Author); ok {
			out = []string{strings.TrimSpace(pe.Username + " " + pe.Email)}
		}
	case "description":
		out = []string{man.Description}
	case "keywords":
		out = stringList(man.Keywords)
	case "license":
		var o struct{ Type string }
		var s string
		if json.Unmarshal(man.License, &s) != nil {
			json.Unmarshal(man.License, &o)
			s = o.Type
		}
		out = []string{s}
	}
	if len(out) == 1 && out[0] == "" {
		return nil
	}
	return out
}

// matches reports whether every word of a search finds the package p: a
// plain word in the name, the description or a keyword; keywords:, author:
// (maintainer:) and scope: name the field, and is:unstable or not:unstable
// a version below 1.0.0, as in Nexus. Another qualifier asks nothing.
func matches(words []string, p meta.Package, description string, keywords []string, people []person) bool {
	name := strings.ToLower(p.Name)
	has := func(list []string, s string) bool {
		for _, v := range list {
			if strings.Contains(strings.ToLower(v), s) {
				return true
			}
		}
		return false
	}
	for _, w := range words {
		field, v, qualified := strings.Cut(w, ":")
		ok := true
		switch {
		case !qualified:
			ok = strings.Contains(name, w) || strings.Contains(strings.ToLower(description), w) || has(keywords, w)
		case field == "keywords":
			ok = has(keywords, v)
		case field == "author" || field == "maintainer":
			ok = false
			for _, pe := range people {
				ok = ok || strings.Contains(strings.ToLower(pe.Username), v)
			}
		case field == "scope":
			scope, _, scoped := strings.Cut(name, "/")
			ok = scoped && strings.TrimPrefix(scope, "@") == strings.TrimPrefix(v, "@")
		case (field == "is" || field == "not") && v == "unstable":
			ver, parsed := semver.Parse(p.Version)
			ok = (parsed && ver.Major == 0) == (field == "is")
		}
		if !ok {
			return false
		}
	}
	return true
}

// stringList reads a manifest's keywords: a list, or the one string old
// packages have (split at commas and spaces).
func stringList(raw json.RawMessage) []string {
	out := []string{}
	var s string
	if json.Unmarshal(raw, &out) != nil && json.Unmarshal(raw, &s) == nil {
		out = strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// personOf reads a manifest's person: {"name", "email"} or "Name <email> (url)".
func personOf(raw json.RawMessage) (person, bool) {
	var o struct{ Name, Email string }
	var s string
	if json.Unmarshal(raw, &o) != nil && json.Unmarshal(raw, &s) == nil {
		o.Name, s, _ = strings.Cut(s, "<")
		o.Email, _, _ = strings.Cut(s, ">")
		o.Name, _, _ = strings.Cut(o.Name, "(")
	}
	pe := person{Username: strings.TrimSpace(o.Name), Email: strings.TrimSpace(o.Email)}
	return pe, pe.Username != ""
}

// links are a search result's links: the package here, and what its manifest names.
func links(npm string, man searchManifest) map[string]string {
	out := map[string]string{"npm": npm}
	if man.Homepage != "" {
		out["homepage"] = man.Homepage
	}
	for key, raw := range map[string]json.RawMessage{"repository": man.Repository, "bugs": man.Bugs} {
		var o struct{ URL string }
		var s string
		if json.Unmarshal(raw, &o) != nil {
			json.Unmarshal(raw, &s)
			o.URL = s
		}
		if o.URL != "" {
			out[key] = o.URL
		}
	}
	return out
}
