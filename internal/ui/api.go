package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"commandcenter/internal/auth"
	"commandcenter/internal/component"
	"commandcenter/internal/format/cargo"
	"commandcenter/internal/format/pypi"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Caps (Decision 9).
const (
	browseLimit   = 500
	searchLimit   = 100
	versionsLimit = 1000
	packagesLimit = 1000 // package names per listing

	maxBody       = 64 << 10
	sessionMaxAge = 7 * 24 * 60 * 60 // seconds; the sessions' absolute limit
	minQuery      = 2
	maxQuery      = 200
)

func (u *UI) apiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/me", u.me)
	mux.HandleFunc("POST /-/api/login", u.login)
	mux.HandleFunc("POST /-/api/logout", u.logout)
	mux.HandleFunc("GET /-/api/auth/sso", u.ssoProviders)
	mux.HandleFunc("GET /-/api/repositories", u.repositories)
	mux.HandleFunc("GET /-/api/repositories/{name}", u.repository)
	mux.HandleFunc("GET /-/api/repositories/{name}/browse", u.browse)
	mux.HandleFunc("GET /-/api/repositories/{name}/asset", u.asset)
	mux.HandleFunc("GET /-/api/repositories/{name}/packages", u.packages)
	mux.HandleFunc("GET /-/api/repositories/{name}/packages/{pkg...}", u.packageVersions)
	mux.HandleFunc("DELETE /-/api/repositories/{name}/components", u.deleteComponent)
	mux.HandleFunc("GET /-/api/repositories/{name}/components/metadata", u.componentMetadata)
	mux.HandleFunc("GET /-/api/search", u.search)
	mux.HandleFunc("GET /-/api/search/components", u.searchComponents)
	u.infoRoutes(mux)
	u.adminRoutes(mux)
	u.repoAdminRoutes(mux)
	u.blobStoreRoutes(mux)
	u.activityRoutes(mux)
	u.cleanupRoutes(mux)
	u.accessRoutes(mux)
	u.settingsRoutes(mux)
	u.smtpRoutes(mux)
	u.realmRoutes(mux)
	u.oidcRealmRoutes(mux)
	u.taskRoutes(mux)
}

func principal(r *http.Request) auth.Principal { return auth.FromContext(r.Context()) }

func (u *UI) me(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	out := map[string]any{"user": p.User, "admin": p.Admin, "anonymous": p.Anonymous(),
		// The UI language when the browser has no stored choice (plan 12
		// Decision 5); not sensitive, so anonymous callers get it too.
		"defaultLanguage": u.Auth.Snapshot().Settings().DefaultLanguage}
	if !p.Anonymous() {
		// The principal's own roles: a stored user's, or an LDAP user's
		// mapped and override roles (plan 12 Task 6).
		out["roles"] = p.Roles()
		// The key of its sessions and tokens ("ldap:<id>" for an LDAP
		// user), which the token page matches token owners against.
		out["owner"] = p.Owner()
		out["realm"] = "Local"
		switch {
		case p.IsLDAP():
			out["realm"] = "LDAP"
		case p.IsOIDC():
			out["realm"] = "OIDC"
		}
		// Signed-in users see the version in the sidebar footer (plan 10);
		// anonymous callers do not learn it.
		out["version"] = u.Version
	}
	writeJSON(w, http.StatusOK, out)
}

// decodeBody reads at most maxBody bytes of strict JSON into v.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
		}
		return false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// requireUIHeader rejects state-changing calls without the CSRF header.
func requireUIHeader(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get(auth.UIHeader) != "1" {
		writeError(w, http.StatusForbidden, "missing "+auth.UIHeader+" header")
		return false
	}
	return true
}

// cookie is the session cookie under the name the authenticator reads
// (auth.SessionCookieName), Secure exactly when that is the __Host- name.
func (u *UI) cookie(value string, maxAge int) *http.Cookie {
	name := u.Auth.SessionCookieName()
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   name == auth.HostSessionCookie,
	}
}

// legacySessionCookie is the session cookie of the repolite releases.
const legacySessionCookie = "repolite_session"

// endCookieSessions ends the sessions named by the request's session
// cookies, under both names, so none of them outlives a login or logout.
func (u *UI) endCookieSessions(r *http.Request) {
	for _, name := range []string{auth.SessionCookie, auth.HostSessionCookie} {
		for _, c := range r.CookiesNamed(name) {
			u.Auth.Sessions.Delete(c.Value)
		}
	}
}

// expireCookies expires the session cookie names other than keep: the
// other scheme's (left from before a baseURL change between http and https)
// and the repolite releases', so no stale copy lingers in the browser.
func (u *UI) expireCookies(w http.ResponseWriter, keep string) {
	for _, name := range []string{auth.SessionCookie, auth.HostSessionCookie, legacySessionCookie} {
		if name == keep {
			continue
		}
		c := u.cookie("", -1)
		c.Name = name
		c.Secure = c.Secure || name == auth.HostSessionCookie // a __Host- cookie must be Secure
		http.SetCookie(w, c)
	}
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	if !requireUIHeader(w, r) {
		return
	}
	if u.Auth.Sessions == nil {
		writeError(w, http.StatusNotFound, "sessions are disabled")
		return
	}
	var body struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	p, err := u.Auth.CheckPassword(r.Context(), body.User, body.Password)
	if wait := auth.RetryAfter(w.Header(), err); wait > 0 {
		writeThrottled(w, wait)
		return
	}
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid user name or password")
		return
	}
	u.endCookieSessions(r)
	c := u.cookie(u.Auth.Sessions.Create(p.Owner()), sessionMaxAge)
	http.SetCookie(w, c)
	u.expireCookies(w, c.Name)
	writeJSON(w, http.StatusOK, map[string]any{"user": p.User, "admin": p.Admin})
}

// writeThrottled answers a sign-in the throttle refused; the UI shows
// retryAfter (seconds, also in the Retry-After header).
func writeThrottled(w http.ResponseWriter, wait int) {
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error": fmt.Sprintf("too many failed sign-ins; try again in %d seconds", wait), "retryAfter": wait})
}

// logout ends the sessions named by the cookies, if any, and clears the
// cookies. It succeeds for an already expired session, so the UI can always
// return to the anonymous state.
func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	if !requireUIHeader(w, r) {
		return
	}
	if u.Auth.Sessions != nil {
		u.endCookieSessions(r)
	}
	u.expireCookies(w, "")
	w.WriteHeader(http.StatusNoContent)
}

// visible returns the named repository if p may browse it (a browse or read
// privilege, plan 12 Decision 2). Hidden and missing repositories are
// indistinguishable (Decision 5). Content downloads go through /repository/,
// which needs read.
func (u *UI) visible(p auth.Principal, name string) *repo.Repository {
	rp := u.Repos.Get(name)
	if rp == nil || !u.Auth.Can(p, name, auth.Browse) {
		return nil
	}
	return rp
}

func (u *UI) lookup(w http.ResponseWriter, r *http.Request) (*repo.Repository, bool) {
	name := r.PathValue("name")
	rp := u.visible(principal(r), name)
	if rp == nil {
		writeError(w, http.StatusNotFound, "repository \""+name+"\" not found")
		return nil, false
	}
	return rp, true
}

type repoJSON struct {
	Name        string   `json:"name"`
	Format      string   `json:"format"`
	Type        string   `json:"type"`
	URL         string   `json:"url"`
	Remote      string   `json:"remote"`
	Members     []string `json:"members"`
	WriteMember string   `json:"writeMember"`
	CanWrite    bool     `json:"canWrite"`
	CanDelete   bool     `json:"canDelete"`
	Online      bool     `json:"online"`
	Assets      int64    `json:"assets"`
	Bytes       int64    `json:"bytes"`

	// BlobStore is the store StoreOf assigns (groups report the default); admin-only.
	BlobStore string `json:"blobStore,omitempty"`

	// Policy fields come from the config and are admin-only (Decision 8).
	VersionPolicy    string `json:"versionPolicy,omitempty"`
	WritePolicy      string `json:"writePolicy,omitempty"`
	Depth            int    `json:"depth,omitempty"`         // yum hosted
	Distribution     string `json:"distribution,omitempty"`  // apt hosted
	Component        string `json:"component,omitempty"`     // apt hosted
	SigningKeySet    bool   `json:"signingKeySet,omitempty"` // apt hosted: Release.gpg/InRelease are signed
	MetadataMaxAge   string `json:"metadataMaxAge,omitempty"`
	ContentMaxAge    string `json:"contentMaxAge,omitempty"`
	NegativeCacheTTL string `json:"negativeCacheTTL,omitempty"`
}

func (u *UI) repoURL(name string) string { return u.baseURL() + "/repository/" + name + "/" }

func (u *UI) describe(p auth.Principal, rp *repo.Repository, st meta.RepoStat) repoJSON {
	out := repoJSON{
		Name:    rp.Name,
		Format:  rp.Format,
		Type:    rp.Type,
		URL:     u.repoURL(rp.Name),
		Online:  rp.IsOnline(),
		Members: []string{},
		Assets:  st.Assets,
		Bytes:   st.Bytes,
	}
	// The remote may embed credentials: admins only, and never the userinfo,
	// query or fragment (tokens are sometimes passed as query parameters).
	if p.Admin && rp.RemoteURL != nil {
		ru := *rp.RemoteURL
		ru.User = nil
		ru.RawQuery, ru.ForceQuery, ru.Fragment, ru.RawFragment = "", false, "", ""
		out.Remote = ru.String()
	}
	for _, m := range rp.MemberRepos {
		if u.visible(p, m.Name) != nil {
			out.Members = append(out.Members, m.Name)
		}
	}
	target, targetOnline := rp.Name, rp.IsOnline()
	if rp.WriteTarget != nil {
		target, targetOnline = rp.WriteTarget.Name, rp.WriteTarget.IsOnline()
		if u.visible(p, target) != nil {
			out.WriteMember = target
		}
	}
	if p.Admin {
		out.BlobStore = repo.StoreOf(rp.Repository)
		addPolicy(&out, rp)
	}
	// The UI upload adds; an offline target refuses writes (503).
	out.CanWrite = targetOnline && u.Auth.Can(p, target, auth.Add)
	// The UI deletes components of hosted repositories (deleteComponent).
	out.CanDelete = rp.Type == "hosted" && rp.IsOnline() && rp.WritePolicy != "deny" && u.Auth.Can(p, rp.Name, auth.Delete)
	return out
}

func (u *UI) repositories(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	repos := u.readable(p)
	list := []repoJSON{}
	if len(repos) == 0 { // nothing to show: skip the stats query
		writeJSON(w, http.StatusOK, map[string]any{"repositories": list})
		return
	}
	stats, err := u.repoStats(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	for _, rp := range repos {
		list = append(list, u.describe(p, rp, stats[rp.Name]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": list})
}

// readable returns the repositories p may browse, sorted by name, all from one
// registry snapshot.
func (u *UI) readable(p auth.Principal) []*repo.Repository {
	var out []*repo.Repository
	for _, rp := range u.Repos.Snapshot().Sorted {
		if u.Auth.Can(p, rp.Name, auth.Browse) {
			out = append(out, rp)
		}
	}
	return out
}

func (u *UI) repository(w http.ResponseWriter, r *http.Request) {
	rp, ok := u.lookup(w, r)
	if !ok {
		return
	}
	stats, err := u.repoStats(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u.describe(principal(r), rp, stats[rp.Name]))
}

// validSegments reports whether every "/"-separated segment of p is a
// plain name: not empty, ".", or "..".
func validSegments(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// validDir accepts "" or a relative directory path ending in "/".
func validDir(p string) bool {
	if p == "" {
		return true
	}
	return strings.HasSuffix(p, "/") && validSegments(strings.TrimSuffix(p, "/"))
}

// validFile accepts a relative file path.
func validFile(p string) bool { return p != "" && validSegments(p) }

type dirJSON struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
}

type fileJSON struct {
	Name           string `json:"name"`
	Dir            bool   `json:"dir"`
	Size           int64  `json:"size"`
	LastModified   string `json:"lastModified"`
	Downloads      int64  `json:"downloads"`
	LastDownloaded string `json:"lastDownloaded"`
}

func (u *UI) browse(w http.ResponseWriter, r *http.Request) {
	rp, ok := u.lookup(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if !validDir(path) {
		writeError(w, http.StatusBadRequest, "path must be empty or a relative directory ending in \"/\"")
		return
	}
	entries, truncated, err := u.DB.ListDir(r.Context(), rp.Name, path, browseLimit)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		if e.Dir {
			out = append(out, dirJSON{Name: e.Name, Dir: true})
		} else {
			out = append(out, fileJSON{Name: e.Name, Size: e.Size, LastModified: rfc3339(e.LastModified),
				Downloads: e.Downloads, LastDownloaded: rfc3339(e.LastDownloaded)})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "entries": out, "truncated": truncated})
}

// escapePath escapes each segment of a repository path for use in a URL.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func (u *UI) asset(w http.ResponseWriter, r *http.Request) {
	rp, ok := u.lookup(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if !validFile(path) {
		writeError(w, http.StatusBadRequest, "path must be a relative file path")
		return
	}
	a, err := u.DB.GetAsset(r.Context(), rp.Name, path)
	if errors.Is(err, meta.ErrNotFound) {
		writeError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	res := map[string]any{
		"path":           a.Path,
		"size":           a.Size,
		"contentType":    a.ContentType,
		"sha1":           a.SHA1,
		"md5":            a.MD5,
		"sha256":         a.BlobSHA256,
		"sha512":         a.SHA512,
		"created":        rfc3339(a.CreatedAt),
		"lastModified":   rfc3339(a.LastModified),
		"checked":        rfc3339(a.CheckedAt),
		"downloads":      a.Downloads,
		"lastDownloaded": rfc3339(a.LastDownloaded),
		"downloadUrl":    u.repoURL(rp.Name) + escapePath(a.Path),
		// browse alone shows the asset but cannot download it (Decision 2)
		"canDownload": u.Auth.Can(principal(r), rp.Name, auth.Read),
	}
	// A crate file's path holds the name lowercased; the asset view's
	// `cargo add` needs the published spelling (QA L-20).
	if rp.Format == "cargo" {
		if _, name, _, ok := component.DeriveAsset("cargo", a.Path); ok {
			res["crate"] = u.cargoName(r.Context(), rp.Name, name)
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// packageFormats are the formats that record packages in the metadata db.
var packageFormats = map[string]bool{"npm": true, "pypi": true, "cargo": true, "docker": true, "helm": true, "yum": true, "nuget": true, "composer": true}

func (u *UI) packages(w http.ResponseWriter, r *http.Request) {
	rp, ok := u.lookup(w, r)
	if !ok {
		return
	}
	q := r.URL.Query().Get("q")
	if utf8.RuneCountInString(q) > maxQuery {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("query must be at most %d characters", maxQuery))
		return
	}
	names, truncated := []string{}, false
	if packageFormats[rp.Format] {
		release, err := u.acquireHeavy(r.Context())
		if err != nil {
			u.fail(w, r, err)
			return
		}
		found, tr, err := func() ([]string, bool, error) {
			defer release()
			return u.DB.SearchPackageNames(r.Context(), rp.Name, q, packagesLimit)
		}()
		if err != nil {
			u.fail(w, r, err)
			return
		}
		names = append(names, found...)
		truncated = tr
	}
	if rp.Format == "cargo" {
		for i, n := range names {
			names[i] = u.cargoName(r.Context(), rp.Name, n)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"packages": names, "truncated": truncated})
}

// cargoName is the spelling a crate was published with. Cargo rows and paths
// hold it lowercased, but cargo matches dependency names exactly, so what
// clients see (and copy into `cargo add`) must keep it (QA L-20). A proxy
// has no rows: its cached index file has the spelling. Any failure keeps name.
func (u *UI) cargoName(ctx context.Context, repoName, name string) string {
	n, err := u.DB.PublishedName(ctx, repoName, strings.ToLower(name))
	if err == nil && n == "" && u.Store != nil {
		n = cargo.CachedName(ctx, u.Store, repoName, name)
	}
	if err == nil && strings.EqualFold(n, name) {
		return n
	}
	return name
}

type versionJSON struct {
	Version string `json:"version"`
	Created string `json:"created"`
	Yanked  bool   `json:"yanked"`
	// docker only: the manifest's media type, the bytes its layers add up
	// to, and the platforms an index lists.
	MediaType string   `json:"mediaType,omitempty"`
	Size      int64    `json:"size,omitempty"`
	Platforms []string `json:"platforms,omitempty"`
	// helm: from the chart's Chart.yaml; yum: the RPM's summary.
	Description string `json:"description,omitempty"`
	AppVersion  string `json:"appVersion,omitempty"`
}

// packageVersions lists a package's versions, newest first, at most
// versionsLimit (Decision 9). PyPI rows hold file names in Version, so its
// responses say "kind":"files"; PyPI names are PEP 503 normalized first.
// Cargo names are looked up in any case and answered as published.
func (u *UI) packageVersions(w http.ResponseWriter, r *http.Request) {
	rp, ok := u.lookup(w, r)
	if !ok {
		return
	}
	name := r.PathValue("pkg")
	kind := "versions"
	switch rp.Format {
	case "pypi":
		name, kind = pypi.Normalize(name), "files"
	case "cargo":
		name = strings.ToLower(name)
	}
	var list []meta.VersionInfo
	truncated := false
	if packageFormats[rp.Format] && name != "" {
		var err error
		if list, truncated, err = u.DB.ListVersions(r.Context(), rp.Name, name, versionsLimit); err != nil {
			u.fail(w, r, err)
			return
		}
	}
	if len(list) == 0 {
		writeError(w, http.StatusNotFound, "package not found")
		return
	}
	versions := make([]versionJSON, 0, len(list))
	for _, v := range list {
		versions = append(versions, versionJSON{Version: v.Version, Created: rfc3339(v.Created), Yanked: v.Yanked})
	}
	switch rp.Format {
	case "docker":
		u.dockerVersionDetails(r.Context(), rp.Name, name, versions)
	case "helm":
		u.helmVersionDetails(r.Context(), rp.Name, name, versions)
	case "yum":
		u.yumVersionDetails(r.Context(), rp.Name, name, versions)
	}
	tags := map[string]string{}
	if rp.Format == "npm" || rp.Format == "docker" {
		dt, err := u.DB.ListDistTags(r.Context(), rp.Name, name)
		if err != nil {
			u.fail(w, r, err)
			return
		}
		for _, t := range dt {
			tags[t.Tag] = t.Version
		}
	}
	if rp.Format == "cargo" {
		name = u.cargoName(r.Context(), rp.Name, name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "kind": kind, "versions": versions, "tags": tags, "truncated": truncated})
}

type hitJSON struct {
	Repo   string `json:"repo"`
	Format string `json:"format"`
	Path   string `json:"path"`
}

func (u *UI) search(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	q := r.URL.Query().Get("q")
	if n := utf8.RuneCountInString(q); n < minQuery || n > maxQuery {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("query must be %d to %d characters", minQuery, maxQuery))
		return
	}
	var searched []*repo.Repository
	if name := r.URL.Query().Get("repo"); name != "" {
		rp := u.visible(p, name)
		if rp == nil {
			writeError(w, http.StatusNotFound, "repository \""+name+"\" not found")
			return
		}
		searched = []*repo.Repository{rp}
	} else {
		searched = u.readable(p)
	}
	if len(searched) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"results": []hitJSON{}, "truncated": false})
		return
	}
	repos := make([]string, len(searched))
	formats := make(map[string]string, len(searched))
	for i, rp := range searched {
		repos[i] = rp.Name
		formats[rp.Name] = rp.Format
	}
	release, err := u.acquireHeavy(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	hits, truncated, err := func() ([]meta.AssetHit, bool, error) {
		defer release()
		return u.DB.SearchAssets(r.Context(), repos, q, searchLimit)
	}()
	if err != nil {
		u.fail(w, r, err)
		return
	}
	out := make([]hitJSON, 0, len(hits))
	for _, h := range hits {
		out = append(out, hitJSON{Repo: h.Repo, Format: formats[h.Repo], Path: h.Path})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out, "truncated": truncated})
}

// dockerVersionDetails fills the media type, layer bytes and platforms of
// docker manifests from their stored attrs. Failures leave the fields
// empty: the listing is more useful than an error here.
func (u *UI) dockerVersionDetails(ctx context.Context, repoName, image string, versions []versionJSON) {
	pkgs, err := u.DB.ListPackages(ctx, repoName, image)
	if err != nil {
		return
	}
	byDigest := make(map[string]string, len(pkgs))
	for _, p := range pkgs {
		byDigest[p.Version] = p.Attrs
	}
	for i := range versions {
		var a struct {
			MediaType  string   `json:"mediaType"`
			LayerBytes int64    `json:"layerBytes"`
			Platforms  []string `json:"platforms"`
		}
		if json.Unmarshal([]byte(byDigest[versions[i].Version]), &a) == nil {
			versions[i].MediaType, versions[i].Size, versions[i].Platforms = a.MediaType, a.LayerBytes, a.Platforms
		}
	}
}

// yumVersionDetails fills a package version's description with the RPM's
// summary, from its stored row. Failures leave it empty.
func (u *UI) yumVersionDetails(ctx context.Context, repoName, name string, versions []versionJSON) {
	pkgs, err := u.DB.ListPackages(ctx, repoName, name)
	if err != nil {
		return
	}
	attrs := make(map[string]string, len(pkgs))
	for _, p := range pkgs {
		attrs[p.Version] = p.Attrs
	}
	for i := range versions {
		var e struct {
			Summary string `json:"summary"`
		}
		if json.Unmarshal([]byte(attrs[versions[i].Version]), &e) == nil {
			versions[i].Description = e.Summary
		}
	}
}

// helmVersionDetails fills a chart version's description and appVersion
// from its stored index entry (Chart.yaml). Failures leave them empty.
func (u *UI) helmVersionDetails(ctx context.Context, repoName, chart string, versions []versionJSON) {
	pkgs, err := u.DB.ListPackages(ctx, repoName, chart)
	if err != nil {
		return
	}
	attrs := make(map[string]string, len(pkgs))
	for _, p := range pkgs {
		attrs[p.Version] = p.Attrs
	}
	for i := range versions {
		var e struct {
			Description string `json:"description"`
			AppVersion  string `json:"appVersion"`
		}
		if json.Unmarshal([]byte(attrs[versions[i].Version]), &e) == nil {
			versions[i].Description, versions[i].AppVersion = e.Description, e.AppVersion
		}
	}
}
