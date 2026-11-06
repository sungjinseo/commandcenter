package npm

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
)

// forwardAudit forwards an audit request to target's remote without the names
// it may not hear of (auditKeeper): a client sends every package of its lock
// file, wherever it came from. The body is read and written one token at a time,
// gunzipped, so a request of megabytes is not held. It holds a name in two
// shapes: bulk ({name: [version]}, npm 7 and later) and the tree of the older
// endpoints (pnpm 10; "requires" and "dependencies" at every level), where the
// project's own name and the install and remove lists go as well.
func (h *Handler) forwardAudit(w http.ResponseWriter, r *http.Request, rp, target *repo.Repository, path string) error {
	ctx := r.Context()
	src := io.Reader(http.MaxBytesReader(w, r.Body, auditBodyMax))
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(src)
		if err != nil {
			return bodyDecodeErr(err, "invalid audit request")
		}
		defer gz.Close()
		src = io.LimitReader(gz, auditBodyMax)
	}
	pr, pw := io.Pipe()
	var failed, undecided error
	done := make(chan struct{})
	go func() {
		defer close(done)
		keep := h.auditKeeper(ctx, rp, target)
		failed = func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					undecided = fmt.Errorf("audit request panicked: %v", p)
					err = undecided
				}
			}()
			dec := json.NewDecoder(src)
			dec.UseNumber()
			return copyAudit(dec, pw, strings.HasSuffix(path, "/advisories/bulk"), 0, func(name string) (bool, error) {
				ok, err := keep(name)
				undecided = err
				return ok, err
			})
		}()
		if failed != nil {
			// As a too large body: Forward stops, and the remote is not to blame.
			pw.CloseWithError(&http.MaxBytesError{Limit: auditBodyMax})
			return
		}
		pw.Close()
	}()
	out := r.Clone(ctx)
	out.Body = pr
	out.Header.Del("Content-Encoding")
	err := h.Resolver.Fetcher.Forward(ctx, target, w, out, path, auditBodyMax)
	pr.Close()
	<-done
	switch {
	case undecided != nil:
		return undecided
	case failed != nil && !errors.Is(failed, io.ErrClosedPipe):
		return bodyDecodeErr(failed, "invalid audit request")
	}
	return err
}

// auditKeeper returns what decides whether target may hear of a package, the
// private-name rule (as in Composer's advisories):
//
//   - A name a hosted repository of this server holds, a group member or not, is
//     private. A hosted member ahead of target answers for it alone, and otherwise
//     target hears of it only if it holds the package document already: a read
//     through it found the same name upstream, so nothing new leaves. It is not
//     looked up to find that out. A mergeMembers group has no member ahead.
//   - target's routing rule refusing the name keeps it from target.
//   - With another proxy behind it target must have the package, as a read would
//     find out; that proxy is not asked about it, as before (first proxy only).
//
// What is no package name (pnpm's "." for the project) stays.
func (h *Handler) auditKeeper(ctx context.Context, rp, target *repo.Repository) func(string) (bool, error) {
	leaves := resolve.Leaves(rp)
	ahead := leaves
	behind := false
	if i := slices.Index(leaves, target); i >= 0 {
		ahead = leaves[:i]
		behind = slices.ContainsFunc(leaves[i+1:], func(m *repo.Repository) bool { return m.Type == "proxy" })
	}
	f := h.Resolver.Fetcher
	decide := func(name string) (bool, error) {
		if !ValidName(name) {
			return true, nil
		}
		if f.RouteAllowed != nil && !f.RouteAllowed(target, "/"+name) {
			return false, nil
		}
		held, err := h.Store.DB.HostedPackages(ctx, "npm", []string{name})
		if err != nil {
			return false, err
		}
		if private := held[name]; len(private) > 0 {
			if !rp.MergeMembers && slices.ContainsFunc(ahead, func(m *repo.Repository) bool { return slices.Contains(private, m.Name) }) {
				return false, nil
			}
			switch _, err := h.Store.Get(ctx, target.Name, PackumentKey(name)); {
			case err == nil:
				return true, nil
			case errors.Is(err, repo.ErrNotFound):
				return false, nil
			default:
				return false, err
			}
		}
		if !behind {
			return true, nil
		}
		_, err = h.Resolver.LookupTarget(ctx, target, proxy.Target{Key: PackumentKey(name)}, proxy.Metadata)
		if repo.StatusOf(err) == http.StatusNotFound {
			return false, nil
		}
		return err == nil, err
	}
	seen := map[string]bool{}
	return func(name string) (bool, error) {
		keep, ok := seen[name]
		if ok {
			return keep, nil
		}
		keep, err := decide(name)
		seen[name] = keep
		return keep, err
	}
}

// maxAuditDepth bounds the nesting of an audit request, which copyAudit follows
// by recursion. A lock file's tree is two levels per level of dependencies.
const maxAuditDepth = 256

// copyAudit copies the JSON value next in dec to w. In an object keyed by package
// name (names) keep decides which stay; the values under "requires" and
// "dependencies" are such objects, as a bulk request is at its top.
func copyAudit(dec *json.Decoder, w io.Writer, names bool, depth int, keep func(string) (bool, error)) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	open, ok := tok.(json.Delim)
	if !ok {
		return writeJSONValue(w, tok)
	}
	if depth == maxAuditDepth {
		return fmt.Errorf("nested deeper than %d", maxAuditDepth)
	}
	if _, err := io.WriteString(w, open.String()); err != nil {
		return err
	}
	written := 0
	for dec.More() {
		out, children := w, false
		if open == '{' {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name, _ := key.(string)
			switch {
			case w == io.Discard:
			case names:
				if ok, err := keep(name); err != nil {
					return err
				} else if !ok {
					out = io.Discard
				}
			case depth == 0 && (name == "name" || name == "install" || name == "remove"):
				out = io.Discard
			default:
				children = name == "requires" || name == "dependencies"
			}
			if out != io.Discard {
				if written++; written > 1 {
					io.WriteString(w, ",")
				}
				if err := writeJSONValue(w, key); err != nil {
					return err
				}
				io.WriteString(w, ":")
			}
		} else if out != io.Discard {
			if written++; written > 1 {
				io.WriteString(w, ",")
			}
		}
		if err := copyAudit(dec, out, children, depth+1, keep); err != nil {
			return err
		}
	}
	end, err := dec.Token()
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(w, end)
	return err
}

func writeJSONValue(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
