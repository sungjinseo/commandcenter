package terraform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/pgp"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

// Handler serves Terraform registries. Reads need Read, an upload Add and
// a delete Delete (the server's default rule); overwriting under
// writePolicy allow needs Edit, checked here.
type Handler struct {
	Store   *repo.Store
	Fetcher *proxy.Fetcher
	Auth    *auth.Authenticator
	BaseURL func() string // live: read per request
	Now     func() time.Time
	locks   repo.KeyedMutex // serializes the writes of one module or provider per repository
}

func New(store *repo.Store, fetcher *proxy.Fetcher, authn *auth.Authenticator, baseURL func() string) *Handler {
	return &Handler{Store: store, Fetcher: fetcher, Auth: authn, BaseURL: baseURL, Now: time.Now}
}

// doc is a derived JSON document and, for a proxy, the cached upstream
// asset it came from.
type doc struct {
	body []byte
	res  proxy.Result
}

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	rt, err := ParseRoute(path)
	if err == nil {
		err = h.serve(w, r, rp, rt)
	}
	if err != nil {
		repo.WriteError(w, r, err)
	}
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	write := r.Method == http.MethodPut || r.Method == http.MethodDelete
	allow := "PUT, DELETE"
	if !repo.AcceptsWrites(rp) {
		allow = "" // a proxy, or a group without writeMember, accepts no method here
	}
	switch rt.Kind {
	case KindModuleUpload, KindProviderUpload:
		if !write {
			return repo.MethodNotAllowed(allow, "method %s not allowed", r.Method)
		}
	case KindModuleArchive:
		if r.Method == http.MethodPut { // the path form of a module upload
			rt.Kind = KindModuleUpload
		}
	}
	if rt.Kind == KindModuleUpload || rt.Kind == KindProviderUpload {
		target, err := repo.WriteTarget(rp)
		if err != nil {
			return repo.WithAllow(err, allow)
		}
		if r.Method == http.MethodDelete {
			return h.unpublish(w, r, target, rt)
		}
		return h.upload(w, r, target, rt)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return repo.MethodNotAllowed("GET, HEAD", "method %s not allowed", r.Method)
	}
	ctx := r.Context()
	var d doc
	var err error
	switch rt.Kind {
	case KindDiscovery:
		d.body = DiscoveryDoc(h.repoURL(rp))
	case KindModuleVersions, KindProviderVersions:
		d, err = h.versions(ctx, rp, rt)
	case KindModuleDownload:
		var src string
		src, err = h.moduleSource(ctx, rp, rp, rt)
		if err != nil {
			return err
		}
		w.Header().Set("X-Terraform-Get", src)
		w.WriteHeader(http.StatusNoContent)
		return nil
	case KindProviderDownload:
		d, err = h.providerDownload(ctx, rp, rp, rt)
	case KindModuleArchive, KindProviderFile:
		var res proxy.Result
		res, err = h.file(ctx, rp, rt)
		if err != nil {
			return err
		}
		proxy.SetStaleHeader(w, res)
		repo.ServeAsset(w, r, h.Store, res.Asset)
		return nil
	}
	if err != nil {
		return err
	}
	proxy.SetStaleHeader(w, d.res)
	repo.ServeText(w, r, string(d.body), ctJSON, d.res.Asset.LastModified)
	return nil
}

// repoURL is the absolute URL of rp without a trailing slash.
func (h *Handler) repoURL(rp *repo.Repository) string {
	base := ""
	if h.BaseURL != nil {
		base = strings.TrimRight(h.BaseURL(), "/")
	}
	return base + "/repository/" + rp.Name
}

// versions answers a module's or provider's version list: from the rows
// (hosted), the upstream document (proxy) or the owning member (group).
func (h *Handler) versions(ctx context.Context, rp *repo.Repository, rt Route) (doc, error) {
	switch rp.Type {
	case "hosted":
		if rt.Kind == KindModuleVersions {
			return h.hostedModuleVersions(ctx, rp, rt)
		}
		return h.hostedProviderVersions(ctx, rp, rt)
	case "proxy":
		return h.proxyVersions(ctx, rp, rt)
	case "group":
		owner, err := h.owner(ctx, rp, rt)
		if err != nil {
			return doc{}, err
		}
		return h.versions(ctx, owner, rt)
	}
	return doc{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// moduleSource is the X-Terraform-Get of a module version served by
// serving: the archive URL under serving for a hosted or proxied archive,
// or the upstream's own source when it cannot be proxied (git::).
func (h *Handler) moduleSource(ctx context.Context, serving, rp *repo.Repository, rt Route) (string, error) {
	switch rp.Type {
	case "hosted":
		row, err := h.moduleRow(ctx, rp, rt)
		if err != nil {
			return "", err
		}
		src := h.repoURL(serving) + "/" + modulePath(rt.Name, rt.Version, row.Ext)
		if row.Subdir != "" {
			src += "//" + row.Subdir
		}
		return src + "?archive=" + row.Ext, nil
	case "proxy":
		s, err := h.proxyModuleSource(ctx, rp, rt)
		if err != nil {
			return "", err
		}
		if s.key == "" {
			return s.raw, nil
		}
		return h.repoURL(serving) + "/" + s.key + s.subdir + "?archive=" + s.ext, nil
	case "group":
		owner, err := h.owner(ctx, rp, rt)
		if err != nil {
			return "", err
		}
		return h.moduleSource(ctx, serving, owner, rt)
	}
	return "", fmt.Errorf("unknown repository type %q", rp.Type)
}

// providerDownload answers a provider version's download document for one
// platform with the file URLs under serving.
func (h *Handler) providerDownload(ctx context.Context, serving, rp *repo.Repository, rt Route) (doc, error) {
	switch rp.Type {
	case "hosted":
		return h.hostedProviderDownload(ctx, serving, rp, rt)
	case "proxy":
		return h.proxyProviderDownload(ctx, serving, rp, rt)
	case "group":
		owner, err := h.owner(ctx, rp, rt)
		if err != nil {
			return doc{}, err
		}
		return h.providerDownload(ctx, serving, owner, rt)
	}
	return doc{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// file finds a stored module archive or provider file: stored (hosted),
// cached or fetched on demand (proxy), or the owning member's (group).
func (h *Handler) file(ctx context.Context, rp *repo.Repository, rt Route) (proxy.Result, error) {
	key := modulePath(rt.Name, rt.Version, rt.Ext)
	if rt.Kind == KindProviderFile {
		key = providerDir(rt.Name, rt.Version) + rt.File
	}
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
		if rt.Kind == KindModuleArchive {
			return h.proxyModuleArchive(ctx, rp, rt, key)
		}
		return h.proxyProviderFile(ctx, rp, rt, key)
	case "group":
		owner, err := h.owner(ctx, rp, rt)
		if err != nil {
			return proxy.Result{}, err
		}
		return h.file(ctx, owner, rt)
	}
	return proxy.Result{}, fmt.Errorf("unknown repository type %q", rp.Type)
}

// owner returns the member of group rp that answers for the module or
// provider rt names: the first online member, in order, that has it
// (hosted: a row; proxy: an upstream version list; group: its own owner).
// That member answers alone, so a later (public) member never stands in
// for an internal module.
func (h *Handler) owner(ctx context.Context, rp *repo.Repository, rt Route) (*repo.Repository, error) {
	for _, m := range rp.OnlineMembers() {
		has, err := h.has(ctx, m, rt)
		if err != nil {
			return nil, memberFailure(rp, m, err)
		}
		if has {
			return m, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (h *Handler) has(ctx context.Context, rp *repo.Repository, rt Route) (bool, error) {
	var err error
	switch rp.Type {
	case "hosted":
		var pkgs []meta.Package
		pkgs, err = h.Store.DB.ListPackages(ctx, rp.Name, rt.Name)
		return len(pkgs) > 0, err
	case "proxy":
		_, err = h.proxyVersions(ctx, rp, rt)
	case "group":
		_, err = h.owner(ctx, rp, rt)
	default:
		return false, fmt.Errorf("unknown repository type %q", rp.Type)
	}
	if errors.Is(err, repo.ErrNotFound) || repo.StatusOf(err) == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// memberFailure logs a group member's error and returns it as a 5xx: a
// later (public) member never stands in for a failing one.
func memberFailure(group, m *repo.Repository, err error) error {
	slog.Error("terraform group member failed", "group", group.Name, "member", m.Name, "err", logsafe.Err(err))
	if repo.StatusOf(err) < http.StatusInternalServerError {
		return repo.Errorf(http.StatusBadGateway, "repository %q failed", m.Name)
	}
	return err
}

// sortVersions orders versions newest first (strings that are not semver
// sort after the rest, by string).
func sortVersions(versions []string) {
	sort.SliceStable(versions, func(i, j int) bool {
		a, aok := semver.Parse(versions[i])
		b, bok := semver.Parse(versions[j])
		switch {
		case aok && bok:
			return semver.Compare(a, b) > 0
		case aok != bok:
			return aok
		}
		return versions[i] > versions[j]
	})
}

// signingKeys is the signing_keys of a download document: the repository's
// GPG public key, when it has one.
func signingKeys(rp *repo.Repository) map[string]any {
	keys := []map[string]string{}
	if rp.GPGPublicKey != "" {
		id, err := pgp.PublicKeyID(rp.GPGPublicKey)
		if err == nil {
			keys = append(keys, map[string]string{"key_id": id, "ascii_armor": rp.GPGPublicKey})
		}
	}
	return map[string]any{"gpg_public_keys": keys}
}

func marshal(v any) (doc, error) {
	b, err := json.Marshal(v)
	return doc{body: b}, err
}
