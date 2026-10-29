package cargo

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// Handler serves Cargo sparse registries.
type Handler struct {
	Store   *repo.Store
	Fetcher *proxy.Fetcher
	Auth    *auth.Authenticator
	BaseURL func() string // live: read per request (plan 12)
	Now     func() time.Time
}

func New(store *repo.Store, fetcher *proxy.Fetcher, authn *auth.Authenticator, baseURL func() string) *Handler {
	return &Handler{Store: store, Fetcher: fetcher, Auth: authn, BaseURL: baseURL, Now: time.Now}
}

// errStop ends an index walk early.
var errStop = errors.New("stop")

// WriteError implements repo.ErrorWriter with Cargo's error body.
func (h *Handler) WriteError(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := repo.ErrorMessage(r, err)
	repo.SetAllow(w, err)
	writeJSON(w, status, map[string]any{"errors": []map[string]string{{"detail": msg}}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	rt, err := ParseRoute(path)
	if err == nil {
		err = h.serve(w, r, rp, rt)
	}
	if err != nil {
		h.WriteError(w, r, err)
	}
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	switch rt.Kind {
	case KindPublish, KindYank, KindUnyank:
		want := map[Kind]string{KindPublish: http.MethodPut, KindYank: http.MethodDelete, KindUnyank: http.MethodPut}[rt.Kind]
		allow := want
		if !repo.AcceptsWrites(rp) {
			allow = "" // a proxy, or a group without writeMember, accepts no method here
		}
		if r.Method != want {
			return repo.MethodNotAllowed(allow, "method %s not allowed", r.Method)
		}
		target, err := repo.WriteTarget(rp)
		if err != nil {
			return repo.WithAllow(err, allow)
		}
		if rt.Kind == KindPublish {
			return h.publish(w, r, target)
		}
		return h.yank(w, r, target, rt, rt.Kind == KindYank)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return repo.MethodNotAllowed("GET, HEAD", "method %s not allowed", r.Method)
	}
	switch rt.Kind {
	case KindConfig:
		cfg := map[string]any{
			"dl":            h.baseURL() + "/repository/" + rp.Name + "/crates",
			"api":           h.baseURL() + "/repository/" + rp.Name,
			"auth-required": !h.Auth.Can(auth.Principal{}, rp.Name, auth.Read),
		}
		writeJSON(w, http.StatusOK, cfg)
		return nil
	case KindIndex:
		return h.index(w, r, rp, rt.Name)
	}
	res, err := h.download(r.Context(), rp, rt.Name, rt.Version)
	if err != nil {
		return err
	}
	proxy.SetStaleHeader(w, res)
	a := res.Asset
	a.ContentType = "application/octet-stream"
	repo.ServeAsset(w, r, h.Store, a)
	return nil
}

// indexFormatVersion is mixed into every ETag; bump it when the generated
// index body changes shape for the same source.
const indexFormatVersion = "1"

// indexSource is where an index body comes from: the rows of a hosted
// repository, or the cached upstream file (res) of a proxy. tag identifies
// what the body is generated from, and the ETag is its hash.
type indexSource struct {
	rp  *repo.Repository
	res proxy.Result
	tag string
}

// source finds where name's index in rp comes from; ok is false when rp does
// not have the crate. A group answers from the first online member that has
// the crate, alone: mixing versions from several members would offer ones the
// download cannot match. Only "not found" moves on to the next member; any
// other member error fails the request so a later (public) member never
// silently stands in.
//
// A hosted tag hashes row metadata (version, yanked, created_at) instead of
// the generated body, so no body has to be buffered. COUPLING: the body is
// built from each row's Attrs, and the only Attrs change that keeps the other
// columns is a yank, which also updates the yanked column (see yank). Any new
// code that rewrites cargo Attrs must also change a hashed column, or the ETag
// goes stale. A proxy's tag is its cached file's sha256, and a group's is the
// answering member's name and tag.
func (h *Handler) source(ctx context.Context, rp *repo.Repository, name string) (indexSource, bool, error) {
	switch rp.Type {
	case "hosted":
		st, err := h.Store.DB.PackageStamp(ctx, rp.Name, name)
		return indexSource{rp: rp, tag: st.Hash}, err == nil && st.Count > 0, err
	case "proxy":
		path := IndexPath(name)
		res, err := h.Fetcher.FetchTarget(ctx, rp, proxy.Target{Key: indexKey(path), Remote: path}, proxy.Metadata)
		if repo.StatusOf(err) == http.StatusNotFound {
			return indexSource{}, false, nil
		}
		// An empty file lists no versions: like a 404, a group moves on.
		return indexSource{rp: rp, res: res, tag: "proxy\x00" + res.Asset.BlobSHA256}, err == nil && res.Asset.Size > 0, err
	case "group":
		for _, m := range rp.OnlineMembers() {
			src, ok, err := h.source(ctx, m, name)
			if err != nil {
				return indexSource{}, false, memberFailure(rp, m, name, err)
			}
			if ok {
				src.tag = "group\x00" + m.Name + "\x00" + src.tag
				return src, true, nil
			}
		}
		return indexSource{}, false, nil
	}
	return indexSource{}, false, fmt.Errorf("unknown repository type %q", rp.Type)
}

// etagMatches implements the weak comparison of If-None-Match.
func etagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == etag {
			return true
		}
	}
	return false
}

// index streams the crate's index file. The ETag is computed from the source
// before the body is read from that same source (a proxy's very file, a
// group's answering member), so a cached copy is never validated against
// another source's body; error responses carry none. Only ETag/If-None-Match
// is supported: created_at does not change on yank and a group can change
// members, so Last-Modified could produce a stale 304.
func (h *Handler) index(w http.ResponseWriter, r *http.Request, rp *repo.Repository, name string) error {
	src, ok, err := h.source(r.Context(), rp, name)
	if err != nil {
		return err
	}
	if !ok {
		return repo.Errorf(http.StatusNotFound, "crate %q not found", name)
	}
	sum := sha256.Sum256([]byte(indexFormatVersion + "\x00" + src.tag))
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, etag) {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	seen := map[string]bool{}
	var bw *bufio.Writer
	err = h.walk(r.Context(), src, name, func(line []byte) error {
		v, err := lineVersion(line)
		if err != nil || seen[v] {
			return nil // unreadable lines and shadowed versions are skipped
		}
		seen[v] = true
		if bw == nil {
			w.Header().Set("ETag", etag)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			bw = bufio.NewWriterSize(w, 32*1024)
		}
		bw.Write(line)
		bw.WriteByte('\n')
		return nil
	})
	if err != nil {
		if bw != nil {
			slog.Error("cargo index failed after output started", "path", logsafe.Path(r.URL.Path), "err", logsafe.Err(err))
			panic(http.ErrAbortHandler)
		}
		return err
	}
	if bw == nil {
		return repo.Errorf(http.StatusNotFound, "crate %q not found", name)
	}
	if err := bw.Flush(); err != nil {
		slog.Warn("cargo index write failed", "path", logsafe.Path(r.URL.Path), "err", logsafe.Err(err))
	}
	return nil
}

// walk calls fn for each line of name's index in src.
func (h *Handler) walk(ctx context.Context, src indexSource, name string, fn func(line []byte) error) error {
	if src.rp.Type == "hosted" {
		pkgs, err := h.Store.DB.ListPackages(ctx, src.rp.Name, name)
		if err != nil {
			return err
		}
		for _, p := range pkgs {
			if err := fn([]byte(p.Attrs)); err != nil {
				return err
			}
		}
		return nil
	}
	f, err := h.Store.Open(src.res.Asset)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			if err := fn(line); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// readLine reads one index line (without the newline), capped at maxLine.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxLine {
			return nil, fmt.Errorf("cargo: index line longer than %d bytes", maxLine)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return []byte(strings.TrimSpace(string(line))), err
	}
}

// CachedName returns the spelling name was published with, from the first
// line of the proxy's cached index file, or "" when that file is not cached
// or its first line does not start with a valid name equal to name ignoring
// case. It reads at most 4 KiB of the file and never asks the upstream, so
// the UI can call it for each crate it shows (QA L-20).
func CachedName(ctx context.Context, s *repo.Store, repoName, name string) string {
	a, err := s.Get(ctx, repoName, indexKey(IndexPath(name)))
	if err != nil {
		return ""
	}
	f, err := s.Open(a)
	if err != nil {
		return ""
	}
	defer f.Close()
	// ponytail: "name" must be the first key, as crates.io and cargo write
	// it; decode further into the line if an upstream writes it later.
	dec := json.NewDecoder(io.LimitReader(f, 4<<10))
	open, _ := dec.Token()
	key, _ := dec.Token()
	val, _ := dec.Token()
	if n, ok := val.(string); ok && open == json.Delim('{') && key == "name" && ValidName(n) && strings.EqualFold(n, name) {
		return n
	}
	return ""
}

// download returns a crate file. Proxies find its checksum in the cached index
// and the upstream download URL in the upstream config.json.
func (h *Handler) download(ctx context.Context, rp *repo.Repository, name, version string) (proxy.Result, error) {
	key := crateKey(name, version)
	switch rp.Type {
	case "hosted":
		a, err := h.Store.Get(ctx, rp.Name, key)
		return proxy.Result{Asset: a}, err
	case "proxy":
		a, err := h.Store.Get(ctx, rp.Name, key)
		if err == nil {
			return proxy.Result{Asset: a}, nil
		}
		if !errors.Is(err, repo.ErrNotFound) {
			return proxy.Result{}, err
		}
		var found indexLine
		hit := false
		src, ok, err := h.source(ctx, rp, name)
		if ok {
			err = h.walk(ctx, src, name, func(line []byte) error {
				var l indexLine
				if json.Unmarshal(line, &l) == nil && l.Vers == version && ValidName(l.Name) && strings.EqualFold(l.Name, name) {
					found, hit = l, true
					return errStop
				}
				return nil
			})
		}
		if err != nil && !errors.Is(err, errStop) {
			return proxy.Result{}, err
		}
		if !hit {
			return proxy.Result{}, repo.ErrNotFound
		}
		if !validCksum(found.Cksum) {
			return proxy.Result{}, repo.Errorf(http.StatusBadGateway, "upstream index has no valid cksum for %s@%s", name, version)
		}
		dl, err := h.upstreamDL(ctx, rp)
		if err != nil {
			return proxy.Result{}, err
		}
		u, err := downloadURL(dl, found.Name, version, found.Cksum)
		if err != nil {
			return proxy.Result{}, repo.Errorf(http.StatusBadGateway, "upstream config.json: %v", err)
		}
		return h.Fetcher.FetchTarget(ctx, rp, proxy.Target{Key: key, URL: u, SHA256: found.Cksum}, proxy.Content)
	case "group":
		for _, m := range rp.OnlineMembers() {
			res, err := h.download(ctx, m, name, version)
			if err == nil {
				return res, nil
			}
			if repo.StatusOf(err) != http.StatusNotFound {
				return proxy.Result{}, memberFailure(rp, m, name, err)
			}
		}
		return proxy.Result{}, repo.ErrNotFound
	}
	return proxy.Result{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// validCksum reports whether s is a 64-character lowercase hex sha256.
func validCksum(s string) bool {
	return len(s) == 64 && proxy.IsSHA256Hex(s) && s == strings.ToLower(s)
}

// upstreamDL returns the "dl" template from the proxy's cached upstream config.json.
func (h *Handler) upstreamDL(ctx context.Context, rp *repo.Repository) (string, error) {
	res, err := h.Fetcher.FetchTarget(ctx, rp, proxy.Target{Key: indexKey("config.json"), Remote: "config.json"}, proxy.Metadata)
	if err != nil {
		if repo.StatusOf(err) == http.StatusNotFound {
			return "", repo.Errorf(http.StatusBadGateway, "upstream registry has no config.json")
		}
		return "", err
	}
	f, err := h.Store.Open(res.Asset)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var cfg struct {
		DL string `json:"dl"`
	}
	if err := json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&cfg); err != nil || cfg.DL == "" {
		return "", repo.Errorf(http.StatusBadGateway, "upstream config.json has no dl")
	}
	return cfg.DL, nil
}

// baseURL is the current external URL without a trailing slash.
func (h *Handler) baseURL() string {
	if h.BaseURL == nil {
		return ""
	}
	return strings.TrimRight(h.BaseURL(), "/")
}

// memberFailure logs a group member's error and returns it as a 5xx.
func memberFailure(group, m *repo.Repository, name string, err error) error {
	slog.Error("cargo group member failed", "group", group.Name, "member", m.Name, "crate", name, "err", logsafe.Err(err))
	if errors.Is(err, proxy.ErrRateLimited) {
		return err // the remote's 429, so a client's retry policy sees it
	}
	if repo.StatusOf(err) < http.StatusInternalServerError {
		return repo.Errorf(http.StatusBadGateway, "repository %q failed", m.Name)
	}
	return err
}
