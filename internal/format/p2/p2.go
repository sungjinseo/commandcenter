// Package p2 implements the p2 (Eclipse update site) format as Nexus has it:
// proxy and group only, no hosted. A proxy passes the site through and
// caches it (p2.index, the content and artifacts documents, features,
// plugins and binaries), rewriting the children of a composite repository
// so that every child resolves through the proxy: a child elsewhere than
// under the remote is mirrored under .remote/<sha1 of its URL>/. A group is
// a composite repository whose children are its members.
package p2

import (
	"context"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// Handler serves p2 repositories.
type Handler struct {
	Store   *repo.Store
	Fetcher *proxy.Fetcher
	BaseURL func() string // the prefix of a group's child locations
	// locks serializes the read-modify-write of a proxy's .remote/index.json.
	locks repo.KeyedMutex
}

func New(store *repo.Store, fetcher *proxy.Fetcher, baseURL func() string) *Handler {
	return &Handler{Store: store, Fetcher: fetcher, BaseURL: baseURL}
}

// isMetadata reports whether file (a base name) is a repository document a
// proxy re-checks every metadataMaxAge; everything else (features, plugins,
// binaries, the legacy pack200 files) is content.
func isMetadata(file string) bool {
	switch file {
	case "p2.index", "site.xml", "content.xml", "content.jar", "content.xml.xz", "artifacts.xml", "artifacts.jar", "artifacts.xml.xz",
		"compositeContent.xml", "compositeContent.jar", "compositeArtifacts.xml", "compositeArtifacts.jar":
		return true
	}
	return false
}

// isComposite reports whether file is a composite repository document,
// whose child locations a proxy rewrites. A composite .xml.xz is passed
// through as it is (xz is not in the standard library); sites do not
// publish them.
func isComposite(file string) bool {
	switch file {
	case "compositeContent.xml", "compositeContent.jar", "compositeArtifacts.xml", "compositeArtifacts.jar":
		return true
	}
	return false
}

// checkPath refuses paths that escape or alias the tree, and directories.
func checkPath(p string) error {
	if p == "" || strings.HasSuffix(p, "/") || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || strings.Contains(p, "//") {
		return repo.Errorf(http.StatusBadRequest, "invalid path %q", p)
	}
	for _, s := range strings.Split(p, "/") {
		if s == "." || s == ".." {
			return repo.Errorf(http.StatusBadRequest, "invalid path %q", p)
		}
	}
	return nil
}

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		repo.WriteError(w, r, repo.MethodNotAllowed("GET, HEAD", "method %s not allowed", r.Method))
		return
	}
	if err := checkPath(p); err != nil {
		repo.WriteError(w, r, err)
		return
	}
	if rp.Type == "group" && !strings.Contains(p, "/") && isMetadata(p) {
		// A group is a composite of its members: its own p2.index and
		// composite documents; the members' root documents are not its.
		body, ct, ok := h.groupDoc(rp, p)
		if !ok {
			repo.WriteError(w, r, repo.ErrNotFound)
			return
		}
		repo.ServeText(w, r, body, ct, time.Time{}) // generated: no Last-Modified
		return
	}
	res, owner, err := h.lookup(r.Context(), rp, p)
	if err != nil {
		repo.WriteError(w, r, err)
		return
	}
	proxy.SetStaleHeader(w, res)
	if isComposite(path.Base(p)) {
		// The composite the client gets is rewritten on the way out; the
		// cached asset stays the remote's, so its validators keep working.
		if body, ct, err := h.rewritten(r.Context(), owner, res.Asset); err != nil {
			repo.WriteError(w, r, err)
			return
		} else if body != nil {
			repo.ServeText(w, r, string(body), ct, res.Asset.LastModified)
			return
		}
	}
	if res.Asset.ContentType == "" {
		res.Asset.ContentType = contentType(p)
	}
	repo.ServeAsset(w, r, h.Store, res.Asset)
}

// contentType is the media type of a p2 file whose remote named none.
func contentType(p string) string {
	switch path.Ext(p) {
	case ".xml":
		return "application/xml"
	case ".jar":
		return "application/java-archive"
	case ".gz", ".xz":
		return "application/octet-stream"
	}
	return "text/plain"
}

// lookup finds p in rp and returns it with the proxy that has it: a proxy's
// cache or its remote (a .remote/<sha1>/ path from the child that sha1
// names); a group asks its online members in order, as resolve.Resolver
// does, each proxy with its own targets.
func (h *Handler) lookup(ctx context.Context, rp *repo.Repository, p string) (proxy.Result, *repo.Repository, error) {
	switch rp.Type {
	case "proxy":
		t, err := h.target(ctx, rp, p)
		if err != nil {
			return proxy.Result{}, nil, err
		}
		kind := proxy.Content
		if isMetadata(path.Base(p)) {
			kind = proxy.Metadata
		}
		res, err := h.Fetcher.FetchTarget(ctx, rp, t, kind)
		return res, rp, err
	case "group":
		var firstErr error
		for _, m := range rp.OnlineMembers() {
			res, owner, err := h.lookup(ctx, m, p)
			if err == nil {
				return res, owner, nil
			}
			if repo.StatusOf(err) != http.StatusNotFound && firstErr == nil {
				firstErr = err
			}
		}
		if firstErr != nil {
			return proxy.Result{}, nil, firstErr
		}
		return proxy.Result{}, nil, repo.ErrNotFound
	}
	return proxy.Result{}, nil, repo.Errorf(http.StatusNotFound, "p2 repository %q is not a proxy or group", rp.Name) // config validation refuses hosted; defensive
}

// target is the fetch target of p in the proxy rp: the path under the
// remote, or for .remote/<sha1>/<rest> the URL <child>/<rest> of the child
// the proxy's table maps sha1 to. The table itself is not served.
func (h *Handler) target(ctx context.Context, rp *repo.Repository, p string) (proxy.Target, error) {
	rest, ok := strings.CutPrefix(p, remotePrefix)
	if !ok {
		return proxy.Target{Key: p, Remote: p, NotHTML: true}, nil
	}
	sha, sub, ok := strings.Cut(rest, "/")
	if !ok || sub == "" || !isSHA1Hex(sha) {
		return proxy.Target{}, repo.ErrNotFound
	}
	children, err := h.children(ctx, rp)
	if err != nil {
		return proxy.Target{}, err
	}
	child, ok := children[sha]
	if !ok {
		return proxy.Target{}, repo.ErrNotFound
	}
	return proxy.Target{Key: p, URL: child + sub, NotHTML: true}, nil
}

func isSHA1Hex(s string) bool {
	return len(s) == 40 && strings.Trim(s, "0123456789abcdef") == ""
}

// groupDoc is a group's own p2.index and composite documents: a composite
// repository (metadata and artifacts alike) whose children are the online
// members, under the base URL. Everything under a directory comes from
// the members.
func (h *Handler) groupDoc(rp *repo.Repository, p string) (body, contentType string, ok bool) {
	switch p {
	case "p2.index":
		return "version=1\nmetadata.repository.factory.order=compositeContent.xml,\\!\nartifact.repository.factory.order=compositeArtifacts.xml,\\!\n", "text/plain", true
	case "compositeContent.xml", "compositeArtifacts.xml":
		var sb strings.Builder
		sb.WriteString("<?xml version='1.0' encoding='UTF-8'?>\n")
		if p == "compositeContent.xml" {
			sb.WriteString("<?compositeMetadataRepository version='1.0.0'?>\n<repository name='" + xmlEscape(rp.Name) + "' type='org.eclipse.equinox.internal.p2.metadata.repository.CompositeMetadataRepository' version='1.0.0'>\n")
		} else {
			sb.WriteString("<?compositeArtifactRepository version='1.0.0'?>\n<repository name='" + xmlEscape(rp.Name) + "' type='org.eclipse.equinox.internal.p2.artifact.repository.CompositeArtifactRepository' version='1.0.0'>\n")
		}
		// Not atomic: a member that fails does not fail the whole group.
		sb.WriteString("  <properties size='1'>\n    <property name='p2.atomic.composite.loading' value='false'/>\n  </properties>\n")
		members := rp.OnlineMembers()
		sb.WriteString("  <children size='" + strconv.Itoa(len(members)) + "'>\n")
		base := ""
		if h.BaseURL != nil {
			base = strings.TrimRight(h.BaseURL(), "/")
		}
		for _, m := range members {
			sb.WriteString("    <child location='" + xmlEscape(base+"/repository/"+m.Name+"/") + "'/>\n")
		}
		sb.WriteString("  </children>\n</repository>\n")
		return sb.String(), "application/xml", true
	}
	return "", "", false
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "'", "&apos;", `"`, "&quot;")

func xmlEscape(s string) string { return xmlEscaper.Replace(s) }
