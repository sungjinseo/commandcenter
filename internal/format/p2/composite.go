package p2

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

const (
	// remotePrefix is where a proxy mirrors the children of a composite
	// that live elsewhere than under its remote: .remote/<sha1 of the child
	// URL>/<path>. remoteIndex is the proxy's table of sha1 -> child URL.
	remotePrefix = ".remote/"
	remoteIndex  = ".remote/index.json"
	// maxComposite bounds a composite document read for rewriting (a few KB
	// in practice); a larger one is served as it is.
	maxComposite = 8 << 20
)

// childRE matches the location attribute of a <child> element.
var childRE = regexp.MustCompile(`(<child\s[^>]*?\blocation\s*=\s*)(["'])([^"']*)(["'])`)

var xmlUnescaper = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'", "&amp;", "&")

// rewritten returns the composite document a (cached by the proxy rp) with
// its child locations rewritten to resolve through the proxy, and its media
// type, or nil to serve a as it is (too large, or a jar that is not one).
// A .jar is unzipped, its XML rewritten and zipped again.
func (h *Handler) rewritten(ctx context.Context, rp *repo.Repository, a meta.Asset) ([]byte, string, error) {
	if a.Size > maxComposite {
		return nil, "", nil
	}
	f, err := h.Store.Open(a)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, "", err
	}
	docURL, rootURL, err := h.docURL(ctx, rp, a.Path)
	if err != nil {
		return nil, "", err
	}
	dirKey := ""
	if d := path.Dir(a.Path); d != "." {
		dirKey = d + "/"
	}
	if !strings.HasSuffix(a.Path, ".jar") {
		out, err := h.rewrite(ctx, rp, raw, docURL, rootURL, dirKey)
		return out, "application/xml", err
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, "", nil
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, e := range zr.File {
		rc, err := e.Open()
		if err != nil {
			return nil, "", nil
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxComposite+1))
		rc.Close()
		if err != nil || len(b) > maxComposite {
			return nil, "", nil
		}
		if strings.HasSuffix(e.Name, ".xml") {
			if b, err = h.rewrite(ctx, rp, b, docURL, rootURL, dirKey); err != nil {
				return nil, "", err
			}
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: e.Name, Method: zip.Deflate, Modified: e.Modified})
		if err != nil {
			return nil, "", err
		}
		if _, err := w.Write(b); err != nil {
			return nil, "", err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	return out.Bytes(), "application/java-archive", nil
}

// docURL is where the document cached at key came from, and the root URL
// the key's top directory maps to: the remote, or for .remote/<sha1>/ the
// child that sha1 names.
func (h *Handler) docURL(ctx context.Context, rp *repo.Repository, key string) (doc, root *url.URL, err error) {
	root = rp.RemoteURL
	rest := key
	if sub, ok := strings.CutPrefix(key, remotePrefix); ok {
		sha, s, _ := strings.Cut(sub, "/")
		children, err := h.children(ctx, rp)
		if err != nil {
			return nil, nil, err
		}
		child, ok := children[sha]
		if !ok {
			return nil, nil, repo.ErrNotFound
		}
		if root, err = url.Parse(child); err != nil {
			return nil, nil, err
		}
		rest = s
	}
	ref, err := url.Parse(rest)
	if err != nil {
		return nil, nil, err
	}
	return root.ResolveReference(ref), root, nil
}

// rewrite rewrites the child locations of the composite doc, which came
// from docURL and is served at the proxy directory dirKey (the root maps
// to rootURL): a relative child that stays under the root is left alone
// (it resolves under the proxy the same way); a child under the remote
// becomes the relative path of that place in the proxy; any other child is
// mirrored under .remote/<sha1 of its URL>/, recorded in the proxy's table.
func (h *Handler) rewrite(ctx context.Context, rp *repo.Repository, doc []byte, docURL, rootURL *url.URL, dirKey string) ([]byte, error) {
	added := map[string]string{}
	up := strings.Repeat("../", strings.Count(dirKey, "/"))
	out := childRE.ReplaceAllFunc(doc, func(m []byte) []byte {
		sm := childRE.FindSubmatch(m)
		ref, err := url.Parse(xmlUnescaper.Replace(string(sm[3])))
		if err != nil {
			return m
		}
		abs := docURL.ResolveReference(ref)
		if !strings.HasSuffix(abs.Path, "/") {
			abs.Path += "/"
		}
		abs.Fragment = ""
		var target string
		switch {
		case !ref.IsAbs() && under(abs, rootURL):
			return m
		case under(abs, rp.RemoteURL):
			target = abs.Path[len(rp.RemoteURL.Path):]
		default:
			sum := sha1.Sum([]byte(abs.String()))
			sha := hex.EncodeToString(sum[:])
			added[sha] = abs.String()
			target = remotePrefix + sha + "/"
		}
		return []byte(string(sm[1]) + string(sm[2]) + xmlEscape(up+target) + string(sm[4]))
	})
	if len(added) == 0 {
		return out, nil
	}
	return out, h.addChildren(ctx, rp, added)
}

// under reports whether u is at or below root (same scheme and host, root's
// path a prefix); root's path ends with "/".
func under(u, root *url.URL) bool {
	return strings.EqualFold(u.Scheme, root.Scheme) && strings.EqualFold(u.Host, root.Host) && strings.HasPrefix(u.Path, root.Path)
}

// children reads the proxy's table of mirrored children, sha1 -> URL.
func (h *Handler) children(ctx context.Context, rp *repo.Repository) (map[string]string, error) {
	out := map[string]string{}
	a, err := h.Store.Get(ctx, rp.Name, remoteIndex)
	if errors.Is(err, repo.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	f, err := h.Store.Open(a)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := json.NewDecoder(io.LimitReader(f, maxComposite)).Decode(&out); err != nil {
		return nil, repo.Errorf(500, "reading %s of %q: %v", remoteIndex, rp.Name, err)
	}
	return out, nil
}

// addChildren records the mirrored children added by a rewrite in the
// proxy's table; one writer at a time per proxy.
func (h *Handler) addChildren(ctx context.Context, rp *repo.Repository, added map[string]string) error {
	unlock := h.locks.Lock(rp.Name)
	defer unlock()
	cur, err := h.children(ctx, rp)
	if err != nil {
		return err
	}
	changed := false
	for sha, u := range added {
		if cur[sha] != u {
			cur[sha] = u
			changed = true
		}
	}
	if !changed {
		return nil
	}
	b, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	_, err = h.Store.Put(ctx, rp.Name, remoteIndex, "application/json", bytes.NewReader(b))
	return err
}
