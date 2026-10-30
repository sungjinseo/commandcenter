// Package nexusapi serves the part of the Sonatype Nexus Repository 3 REST
// API (v1) that scripts, CI jobs and tools use, at /service/rest/v1, on
// Command Center's repositories. Reads go to the registry and the metadata
// database; every write (repository create, update, delete; component
// delete) is an in-process call of the web UI's JSON API, so validation,
// events and cache invalidation stay in one place.
package nexusapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/component"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

// Prefix is where the API is mounted.
const Prefix = "/service/rest/v1"

// pageSize is Nexus's page size for components, assets and search.
const pageSize = 50

// Handler serves the API. Mount it under Prefix behind the server's
// authentication (it reads auth.FromContext).
type Handler struct {
	Repos   *repo.Registry
	Auth    *auth.Authenticator
	DB      *meta.DB
	BaseURL func() string                       // the live baseURL, without trailing slash
	UI      interface{ Routes(*http.ServeMux) } // the web UI, whose /-/api/ handlers do the writes

	// Repository serves an in-process request for /repository/{name}/{path...}
	// (server.Server.ServeRepository): the component upload sends each file
	// there, as the format's own client would. nil: no component upload.
	Repository func(http.ResponseWriter, *http.Request)
	// TempDir holds the files of a component upload while it is processed;
	// "" is the system's.
	TempDir string

	once sync.Once
	mux  *http.ServeMux // this API
	api  *http.ServeMux // the UI's routes, for in-process calls
}

func (h *Handler) init() {
	h.api = http.NewServeMux()
	if h.UI != nil {
		h.UI.Routes(h.api)
	}
	m := http.NewServeMux()
	p := Prefix
	m.HandleFunc("GET "+p+"/status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m.HandleFunc("GET "+p+"/status/writable", h.statusWritable)
	m.HandleFunc("GET "+p+"/status/check", h.statusCheck)
	m.HandleFunc("GET "+p+"/repositories", h.listRepositories)
	m.HandleFunc("GET "+p+"/repositories/{name}", h.getRepository)
	m.HandleFunc("GET "+p+"/repositorySettings", h.listSettings)
	m.HandleFunc("GET "+p+"/repositories/{format}/{type}/{name}", h.getSettings)
	m.HandleFunc("POST "+p+"/repositories/{format}/{type}", h.createRepository)
	m.HandleFunc("PUT "+p+"/repositories/{format}/{type}/{name}", h.updateRepository)
	m.HandleFunc("DELETE "+p+"/repositories/{name}", h.deleteRepository)
	m.HandleFunc("GET "+p+"/components", h.listComponents)
	m.HandleFunc("GET "+p+"/components/{id}", h.getComponent)
	m.HandleFunc("DELETE "+p+"/components/{id}", h.deleteComponent)
	m.HandleFunc("POST "+p+"/components", h.uploadComponent)
	m.HandleFunc("GET "+p+"/formats/upload-specs", h.uploadSpecList)
	m.HandleFunc("GET "+p+"/formats/{format}/upload-specs", h.uploadSpecOne)
	m.HandleFunc("GET "+p+"/assets", h.listAssets)
	m.HandleFunc("GET "+p+"/assets/{id}", h.getAsset)
	m.HandleFunc("DELETE "+p+"/assets/{id}", h.deleteAsset)
	m.HandleFunc("GET "+p+"/search", h.search)
	m.HandleFunc("GET "+p+"/search/assets", h.searchAssets)
	m.HandleFunc("GET "+p+"/search/assets/download", h.searchDownload)
	m.HandleFunc("GET "+p+"/security/users", h.users)
	m.HandleFunc("GET "+p+"/security/roles", h.roles)
	m.HandleFunc("GET "+p+"/blobstores", h.blobStores)
	// {name}/{sub}, not {name}/quota-status: the literal form would conflict
	// with GET /blobstores/group/{name} in the mux (neither is more specific).
	m.HandleFunc("GET "+p+"/blobstores/{name}/{sub}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("sub") != "quota-status" {
			writeMessage(w, http.StatusNotFound, "not found")
			return
		}
		h.quotaStatus(w, r)
	})
	m.HandleFunc("POST "+p+"/blobstores/file", h.createFileBlobStore)
	m.HandleFunc("PUT "+p+"/blobstores/file/{name}", h.updateFileBlobStore)
	m.HandleFunc("GET /service/rest/swagger.json", h.swagger) // outside the prefix; the server routes it here
	m.HandleFunc("GET "+p+"/tasks", h.tasks)
	h.securityRoutes(m, p)
	m.HandleFunc("GET "+p+"/security/ssl", h.sslList)
	m.HandleFunc("POST "+p+"/security/ssl/truststore", h.sslAdd)
	m.HandleFunc("DELETE "+p+"/security/ssl/truststore/{id}", h.sslRemove)
	h.userTokenRoutes(m, p)
	h.blobGroupRoutes(m, p)
	h.stagingRoutes(m, p)
	m.HandleFunc(p+"/", func(w http.ResponseWriter, r *http.Request) { writeMessage(w, http.StatusNotFound, "not found") })
	h.proRoutes(m, p)
	h.azureBlobRoutes(m, p)
	h.configRoutes(m, p)
	h.mux = m
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.once.Do(h.init)
	w.Header().Set("Cache-Control", "no-store")
	h.mux.ServeHTTP(w, r)
}

// ---- JSON and errors ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeMessage answers like Nexus's JSON errors: {"message": ...}.
func writeMessage(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

// validation is one entry of Nexus's 400 body for repository bodies.
type validation struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

func writeValidation(w http.ResponseWriter, errs []string) {
	out := make([]validation, 0, len(errs))
	for _, e := range errs {
		id := "*"
		if f, _, ok := strings.Cut(e, ": "); ok && !strings.ContainsAny(f, " \"") {
			id = f
		}
		out = append(out, validation{ID: id, Message: e})
	}
	writeJSON(w, http.StatusBadRequest, out)
}

// deny answers 401 (anonymous, with a Basic challenge) or 403.
func deny(w http.ResponseWriter, p auth.Principal) {
	if p.Anonymous() {
		w.Header().Set("WWW-Authenticate", `Basic realm="commandcenter"`)
		writeMessage(w, http.StatusUnauthorized, "authentication required")
		return
	}
	writeMessage(w, http.StatusForbidden, "user "+strconv.Quote(p.User)+" is not allowed to do this")
}

func principal(r *http.Request) auth.Principal { return auth.FromContext(r.Context()) }

func (h *Handler) admin(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p := principal(r)
	if !p.Admin {
		deny(w, p)
		return p, false
	}
	return p, true
}

// readable returns the named repository when p may read it; a hidden
// repository is as unknown as a missing one.
func (h *Handler) readable(p auth.Principal, name string) *repo.Repository {
	rp := h.Repos.Get(name)
	if rp == nil || !h.Auth.Can(p, name, auth.Read) {
		return nil
	}
	return rp
}

// readableAll returns every repository p may read, by name.
func (h *Handler) readableAll(p auth.Principal) []*repo.Repository {
	var out []*repo.Repository
	for _, rp := range h.Repos.Snapshot().Sorted {
		if h.Auth.Can(p, rp.Name, auth.Read) {
			out = append(out, rp)
		}
	}
	return out
}

// lookupRepo answers the repository named by the query parameter or 400,
// 401 (anonymous) or 404.
func (h *Handler) lookupRepo(w http.ResponseWriter, r *http.Request, name string) *repo.Repository {
	p := principal(r)
	if name == "" {
		writeMessage(w, http.StatusBadRequest, "repository is required")
		return nil
	}
	rp := h.readable(p, name)
	if rp == nil {
		if p.Anonymous() {
			deny(w, p)
		} else {
			writeMessage(w, http.StatusNotFound, "repository not found")
		}
	}
	return rp
}

// ---- in-process calls of the UI's JSON API ----

type recorder struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (c *recorder) Header() http.Header { return c.h }
func (c *recorder) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}
func (c *recorder) Write(b []byte) (int, error) {
	c.WriteHeader(http.StatusOK)
	return c.body.Write(b)
}

// call runs method path (a /-/api/ route) as the caller of r, with the
// body JSON-encoded, and returns the status and body.
func (h *Handler) call(r *http.Request, method, p string, body any) (int, []byte) {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), method, p, &buf)
	if err != nil {
		return http.StatusInternalServerError, nil
	}
	req.Header.Set(auth.UIHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", r.Header.Get("X-Request-Id"))
	req.RemoteAddr = r.RemoteAddr
	rec := &recorder{h: http.Header{}}
	h.api.ServeHTTP(rec, req)
	return rec.status, rec.body.Bytes()
}

// writeUIError translates a UI error body into Nexus's shapes: a 400 with
// an errors list becomes the validation array, anything else {"message"}.
func writeUIError(w http.ResponseWriter, status int, body []byte) {
	var e struct {
		Error  string   `json:"error"`
		Errors []string `json:"errors"`
	}
	json.Unmarshal(body, &e)
	if status == http.StatusBadRequest && len(e.Errors) > 0 {
		writeValidation(w, e.Errors)
		return
	}
	if e.Error == "" {
		e.Error = http.StatusText(status)
	}
	writeMessage(w, status, e.Error)
}

// ---- formats ----

// nexusFormat is the Nexus spelling of a Command Center format.
func nexusFormat(f string) string {
	if f == "maven" {
		return "maven2"
	}
	return f
}

// ccFormat is the Command Center spelling of a Nexus format.
func ccFormat(f string) string {
	if f == "maven2" {
		return "maven"
	}
	return f
}

// ---- repositories ----

func (h *Handler) repoURL(name string) string { return h.BaseURL() + "/repository/" + name }

// publicRemote is the remote without userinfo, query and fragment, which may
// hold credentials (the UI shows the same).
func publicRemote(rp *repo.Repository) string {
	if rp.RemoteURL == nil {
		return ""
	}
	ru := *rp.RemoteURL
	ru.User = nil
	ru.RawQuery, ru.ForceQuery, ru.Fragment, ru.RawFragment = "", false, "", ""
	return ru.String()
}

// repoJSON is a repository as GET /repositories lists it; size is the
// sum of its assets' sizes (Nexus Pro's repository size; usage counters).
func (h *Handler) repoJSON(rp *repo.Repository, usage map[string]meta.RepoUsage) map[string]any {
	attrs := map[string]any{}
	if rp.Type == "proxy" {
		attrs["proxy"] = map[string]any{"remoteUrl": publicRemote(rp)}
	}
	return map[string]any{"name": rp.Name, "format": nexusFormat(rp.Format), "type": rp.Type,
		"url": h.repoURL(rp.Name), "attributes": attrs, "size": usage[rp.Name].Bytes}
}

// usage reads the repositories' counters; nil (size 0) when they cannot be read.
func (h *Handler) usage(ctx context.Context) map[string]meta.RepoUsage {
	u, _ := h.DB.RepoUsages(ctx)
	return u
}

// minutes renders a config duration as Nexus does: minutes, -1 for never.
func minutes(d config.Duration) int {
	if d == config.Never {
		return -1
	}
	return int(time.Duration(d) / time.Minute)
}

func (h *Handler) settingsJSON(rp *repo.Repository) map[string]any {
	d := rp.Repository
	storage := map[string]any{"blobStoreName": repo.StoreOf(d), "strictContentTypeValidation": d.StrictContentTypeValidation}
	if d.Type == "hosted" {
		storage["writePolicy"] = strings.ToUpper(d.WritePolicy)
	}
	policies := []string{}
	if d.CleanupPolicy != "" {
		policies = []string{d.CleanupPolicy}
	}
	out := map[string]any{"name": d.Name, "format": nexusFormat(d.Format), "type": d.Type, "url": h.repoURL(d.Name),
		"online": d.IsOnline(), "storage": storage, "cleanup": map[string]any{"policyNames": policies}}
	switch d.Type {
	case "proxy":
		out["proxy"] = map[string]any{"remoteUrl": publicRemote(rp), "contentMaxAge": minutes(d.ContentMaxAge), "metadataMaxAge": minutes(d.MetadataMaxAge)}
		out["negativeCache"] = map[string]any{"enabled": true, "timeToLive": minutes(d.NegativeCacheTTL)}
		hc := map[string]any{"blocked": d.Blocked, "autoBlock": d.AutoBlocks(), "authentication": nil}
		if a := d.RemoteAuth; a != nil && a.Username != "" {
			hc["authentication"] = map[string]any{"type": "username", "username": a.Username}
		} else if a != nil && a.BearerToken != "" {
			hc["authentication"] = map[string]any{"type": "bearerToken"}
		}
		out["httpClient"] = hc
		out["routingRule"], out["routingRuleName"] = d.RoutingRule, d.RoutingRule
		repl := map[string]any{"preemptivePullEnabled": false, "assetPathRegex": nil, "verifyChecksums": false}
		if p := d.Replication; p != nil {
			repl["preemptivePullEnabled"], repl["verifyChecksums"] = p.PreemptivePullEnabled, p.VerifyChecksums
			if p.AssetPathRegex != "" {
				repl["assetPathRegex"] = p.AssetPathRegex
			}
		}
		out["replication"] = repl
	case "group":
		g := map[string]any{"memberNames": append([]string{}, d.Members...)}
		if d.WriteMember != "" {
			g["writableMember"] = d.WriteMember
		}
		out["group"] = g
	}
	if d.Format == "maven" {
		vp := strings.ToUpper(d.VersionPolicy)
		if vp == "" {
			vp = "MIXED"
		}
		out["maven"] = map[string]any{"versionPolicy": vp, "layoutPolicy": "PERMISSIVE", "contentDisposition": "INLINE"}
	}
	if d.Format == "raw" {
		cd := strings.ToUpper(d.ContentDisposition)
		if cd == "" {
			cd = "INLINE"
		}
		out["raw"] = map[string]any{"contentDisposition": cd}
	}
	return out
}

func (h *Handler) listRepositories(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	repos := h.readableAll(p)
	if p.Anonymous() && len(repos) == 0 {
		deny(w, p)
		return
	}
	out := make([]map[string]any, 0, len(repos))
	usage := h.usage(r.Context())
	for _, rp := range repos {
		out = append(out, h.repoJSON(rp, usage))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getRepository(w http.ResponseWriter, r *http.Request) {
	if rp := h.lookupRepo(w, r, r.PathValue("name")); rp != nil {
		writeJSON(w, http.StatusOK, h.repoJSON(rp, h.usage(r.Context())))
	}
}

func (h *Handler) listSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	out := []map[string]any{}
	for _, rp := range h.Repos.Snapshot().Sorted {
		out = append(out, h.settingsJSON(rp))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	rp := h.Repos.Get(r.PathValue("name"))
	if rp == nil || rp.Format != ccFormat(r.PathValue("format")) || rp.Type != r.PathValue("type") {
		writeMessage(w, http.StatusNotFound, "repository not found")
		return
	}
	writeJSON(w, http.StatusOK, h.settingsJSON(rp))
}

// repoBody is a Nexus repository body (any format and type); fields Command
// Center has no equivalent for are ignored.
type repoBody struct {
	Name    string `json:"name"`
	Online  *bool  `json:"online"`
	Storage struct {
		BlobStoreName string `json:"blobStoreName"`
		WritePolicy   string `json:"writePolicy"`

		StrictContentTypeValidation bool `json:"strictContentTypeValidation"`
	} `json:"storage"`
	Cleanup *struct {
		PolicyNames []string `json:"policyNames"`
	} `json:"cleanup"`
	Proxy struct {
		RemoteURL      string `json:"remoteUrl"`
		ContentMaxAge  *int   `json:"contentMaxAge"`
		MetadataMaxAge *int   `json:"metadataMaxAge"`
	} `json:"proxy"`
	NegativeCache struct {
		TimeToLive *int `json:"timeToLive"`
	} `json:"negativeCache"`
	Group struct {
		MemberNames    []string `json:"memberNames"`
		WritableMember string   `json:"writableMember"`
	} `json:"group"`
	HTTPClient struct {
		Blocked        bool  `json:"blocked"`
		AutoBlock      *bool `json:"autoBlock"`
		Authentication *struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"authentication"`
	} `json:"httpClient"`
	Maven struct {
		VersionPolicy string `json:"versionPolicy"`
	} `json:"maven"`
	Raw struct {
		ContentDisposition string `json:"contentDisposition"` // INLINE | ATTACHMENT
	} `json:"raw"`
	// RoutingRule is the request field, RoutingRuleName the one Nexus
	// returns; either is accepted.
	RoutingRule     *string `json:"routingRule"`
	RoutingRuleName *string `json:"routingRuleName"`
	// Replication is a proxy's pull replication (Nexus's replication
	// object; verifyChecksums is Command Center's); absent keeps it.
	Replication *config.Replication `json:"replication"`
}

// minutesText renders Nexus minutes in the config format.
func minutesText(m *int) string {
	switch {
	case m == nil:
		return ""
	case *m < 0:
		return "-1"
	}
	return fmt.Sprintf("%dm", *m)
}

// uiRepo maps a Nexus body to the UI's repository request (its exact
// fields: the UI refuses unknown ones).
func uiRepo(format, typ string, b repoBody) map[string]any {
	out := map[string]any{
		"name": b.Name, "format": ccFormat(format), "type": typ,
		"remote":           b.Proxy.RemoteURL,
		"metadataMaxAge":   minutesText(b.Proxy.MetadataMaxAge),
		"contentMaxAge":    minutesText(b.Proxy.ContentMaxAge),
		"negativeCacheTTL": minutesText(b.NegativeCache.TimeToLive),
		"members":          b.Group.MemberNames,
		"writeMember":      b.Group.WritableMember,
		"versionPolicy":    strings.ToLower(b.Maven.VersionPolicy),
		"writePolicy":      strings.ToLower(b.Storage.WritePolicy),
		"online":           b.Online,
		"blobStore":        b.Storage.BlobStoreName,
	}
	if typ == "hosted" {
		out["strictContentTypeValidation"] = b.Storage.StrictContentTypeValidation
	}
	if typ == "proxy" {
		out["blocked"], out["autoBlock"] = b.HTTPClient.Blocked, b.HTTPClient.AutoBlock
	}
	if cd := strings.ToLower(b.Raw.ContentDisposition); cd != "" {
		out["contentDisposition"] = cd
	}
	if typ == "group" { // Nexus group bodies name a blob store; Command Center groups have none
		out["blobStore"] = ""
	}
	if b.RoutingRule != nil {
		out["routingRule"] = *b.RoutingRule
	} else if b.RoutingRuleName != nil {
		out["routingRule"] = *b.RoutingRuleName
	}
	if b.Cleanup != nil { // ponytail: one policy per repository here; Nexus allows several
		name := ""
		if len(b.Cleanup.PolicyNames) > 0 {
			name = b.Cleanup.PolicyNames[0]
		}
		out["cleanupPolicy"] = name
	}
	if a := b.HTTPClient.Authentication; a != nil {
		out["remoteAuth"] = map[string]any{"username": a.Username, "password": a.Password}
	}
	if b.Replication != nil {
		out["replication"] = b.Replication
	}
	return out
}

func decodeRepoBody(w http.ResponseWriter, r *http.Request) (repoBody, bool) {
	var b repoBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&b); err != nil {
		writeMessage(w, http.StatusBadRequest, "invalid JSON body")
		return b, false
	}
	return b, true
}

func (h *Handler) createRepository(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	b, ok := decodeRepoBody(w, r)
	if !ok {
		return
	}
	status, body := h.call(r, http.MethodPost, "/-/api/repositories", uiRepo(r.PathValue("format"), r.PathValue("type"), b))
	if status != http.StatusCreated {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) updateRepository(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	b, ok := decodeRepoBody(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	b.Name = name
	status, body := h.call(r, http.MethodPut, "/-/api/repositories/"+url.PathEscape(name), uiRepo(r.PathValue("format"), r.PathValue("type"), b))
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteRepository(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodDelete, "/-/api/repositories/"+url.PathEscape(r.PathValue("name")), nil)
	if status != http.StatusNoContent {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- components and assets ----

// comp is a derived component with the paths of its files.
type comp struct {
	repo, format, group, name, version string
	paths                              []string
}

func (c *comp) key() string { return c.repo + ":" + c.group + ":" + c.name + ":" + c.version }

// compID and assetID are the opaque ids: base64url of "<repo>:<group>:<name>:<version>"
// and "<repo>:<path>". A repository name holds no ":", and a name holds
// one only for raw, whose version is empty, so the last ":" splits it.
func compID(c *comp) string { return base64.RawURLEncoding.EncodeToString([]byte(c.key())) }

func parseCompID(id string) (c comp, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return c, false
	}
	parts := strings.SplitN(string(b), ":", 3)
	if len(parts) != 3 {
		return c, false
	}
	c.repo, c.group = parts[0], parts[1]
	i := strings.LastIndexByte(parts[2], ':')
	if i < 0 {
		return c, false
	}
	c.name, c.version = parts[2][:i], parts[2][i+1:]
	return c, c.name != ""
}

func assetID(repoName, p string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(repoName + ":" + p))
}

func parseAssetID(id string) (repoName, p string, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", "", false
	}
	repoName, p, ok = strings.Cut(string(b), ":")
	return repoName, p, ok && p != ""
}

// components derives every component of rp, ordered by group, name and
// version. ponytail: a whole-repository scan of paths per request (the ids
// alone, not the rows); index the derivation if listing huge repositories
// gets slow.
func (h *Handler) components(ctx context.Context, rp *repo.Repository) ([]*comp, error) {
	byKey := map[string]*comp{}
	add := func(group, name, version, p string) {
		c := &comp{repo: rp.Name, format: rp.Format, group: group, name: name, version: version}
		k := c.key()
		if x := byKey[k]; x != nil {
			c = x
		} else {
			byKey[k] = c
		}
		c.paths = append(c.paths, p)
	}
	if rp.Format == "docker" {
		// An image's tags: Nexus names a docker component image:tag, while
		// the package rows hold digests.
		names, err := h.DB.ListPackageNames(ctx, rp.Name)
		if err != nil {
			return nil, err
		}
		for _, image := range names {
			tags, err := h.DB.ListDistTags(ctx, rp.Name, image)
			if err != nil {
				return nil, err
			}
			for _, t := range tags {
				add("", image, t.Tag, image+"/manifests/"+t.Version)
			}
		}
	} else if err := h.DB.WalkAssetPaths(ctx, rp.Name, "", func(p string) {
		if group, name, version, ok := component.DeriveAsset(rp.Format, p); ok {
			add(group, name, version, p)
		}
	}); err != nil {
		return nil, err
	}
	out := make([]*comp, 0, len(byKey))
	for _, c := range byKey {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *comp) int {
		return strings.Compare(a.key(), b.key())
	})
	return out, nil
}

// page cuts the page after the continuation token (the id of the previous
// page's last item) and returns the next token ("" on the last page).
func page[T any](items []T, id func(T) string, token string) ([]T, string, bool) {
	if token != "" {
		i := slices.IndexFunc(items, func(x T) bool { return id(x) == token })
		if i < 0 {
			return nil, "", false
		}
		items = items[i+1:]
	}
	if len(items) > pageSize {
		items = items[:pageSize]
		return items, id(items[len(items)-1]), true
	}
	return items, "", true
}

func rfc3339(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func (h *Handler) assetJSON(rp *repo.Repository, a meta.Asset) map[string]any {
	return map[string]any{
		"id":           assetID(rp.Name, a.Path),
		"path":         a.Path,
		"downloadUrl":  h.repoURL(rp.Name) + "/" + escapePath(a.Path),
		"repository":   rp.Name,
		"format":       nexusFormat(rp.Format),
		"checksum":     map[string]string{"sha1": a.SHA1, "sha256": a.BlobSHA256, "md5": a.MD5, "sha512": a.SHA512},
		"contentType":  a.ContentType,
		"lastModified": rfc3339(a.LastModified), "blobCreated": rfc3339(a.CreatedAt), "lastDownloaded": rfc3339(a.LastDownloaded),
		"fileSize": a.Size, "blobStoreName": repo.StoreOf(rp.Repository),
		"uploader": nil, "uploaderIp": nil, // not recorded
	}
}

func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// componentJSON renders c with its assets (read now, for the page only).
func (h *Handler) componentJSON(ctx context.Context, rp *repo.Repository, c *comp) (map[string]any, error) {
	assets := make([]map[string]any, 0, len(c.paths))
	for _, p := range c.paths {
		a, err := h.DB.GetAsset(ctx, rp.Name, p)
		if errors.Is(err, meta.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		assets = append(assets, h.assetJSON(rp, a))
	}
	var group, version any
	switch {
	case c.format == "raw": // Nexus: group is the directory, a raw file has no version
		group = "/" + strings.TrimSuffix(path.Dir(c.name), ".")
	case c.format == "npm":
		if c.group != "" {
			group = strings.TrimPrefix(c.group, "@")
		}
	case c.group != "":
		group = c.group
	}
	if c.version != "" {
		version = c.version
	}
	tags, err := h.DB.ComponentTags(ctx, c.metaKey())
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": compID(c), "repository": c.repo, "format": nexusFormat(c.format),
		"group": group, "name": c.name, "version": version, "assets": assets, "tags": tags}, nil
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		writeMessage(w, http.StatusServiceUnavailable, "server busy")
		return
	}
	writeMessage(w, http.StatusInternalServerError, "internal error")
}

func (h *Handler) listComponents(w http.ResponseWriter, r *http.Request) {
	rp := h.lookupRepo(w, r, r.URL.Query().Get("repository"))
	if rp == nil {
		return
	}
	comps, err := h.components(r.Context(), rp)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.writeComponentPage(w, r, comps, r.URL.Query().Get("continuationToken"))
}

func (h *Handler) writeComponentPage(w http.ResponseWriter, r *http.Request, comps []*comp, token string) {
	comps, next, ok := page(comps, compID, token)
	if !ok {
		writeMessage(w, http.StatusBadRequest, "invalid continuationToken")
		return
	}
	items := make([]map[string]any, 0, len(comps))
	for _, c := range comps {
		j, err := h.componentJSON(r.Context(), h.Repos.Get(c.repo), c)
		if err != nil {
			h.fail(w, err)
			return
		}
		items = append(items, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "continuationToken": nullable(next)})
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// findComponent resolves a component id to the repository and the derived
// component, or answers the error.
func (h *Handler) findComponent(w http.ResponseWriter, r *http.Request) (*repo.Repository, *comp) {
	want, ok := parseCompID(r.PathValue("id"))
	if !ok {
		writeMessage(w, http.StatusNotFound, "component not found")
		return nil, nil
	}
	rp := h.lookupRepo(w, r, want.repo)
	if rp == nil {
		return nil, nil
	}
	comps, err := h.components(r.Context(), rp)
	if err != nil {
		h.fail(w, err)
		return nil, nil
	}
	k := want.key()
	if i := slices.IndexFunc(comps, func(c *comp) bool { return c.key() == k }); i >= 0 {
		return rp, comps[i]
	}
	writeMessage(w, http.StatusNotFound, "component not found")
	return nil, nil
}

func (h *Handler) getComponent(w http.ResponseWriter, r *http.Request) {
	rp, c := h.findComponent(w, r)
	if c == nil {
		return
	}
	j, err := h.componentJSON(r.Context(), rp, c)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// deleteComponent deletes through the UI's component delete (the format's
// deleter: all files of the component, the event recorded). A docker tag is
// deleted by its digest.
func (h *Handler) deleteComponent(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p.Anonymous() {
		deny(w, p)
		return
	}
	rp, c := h.findComponent(w, r)
	if c == nil {
		return
	}
	h.deleteDerived(w, r, rp, c)
}

func (h *Handler) deleteDerived(w http.ResponseWriter, r *http.Request, rp *repo.Repository, c *comp) {
	if !h.Auth.Can(principal(r), rp.Name, auth.Delete) {
		deny(w, principal(r))
		return
	}
	version := c.version
	if rp.Format == "docker" && len(c.paths) > 0 {
		version = path.Base(c.paths[0]) // the manifest digest the tag points at
	}
	q := url.Values{"group": {c.group}, "name": {c.name}, "version": {version}}
	status, body := h.call(r, http.MethodDelete, "/-/api/repositories/"+url.PathEscape(rp.Name)+"/components?"+q.Encode(), nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listAssets(w http.ResponseWriter, r *http.Request) {
	rp := h.lookupRepo(w, r, r.URL.Query().Get("repository"))
	if rp == nil {
		return
	}
	// Keyset paging on the previous page's last asset, which need not exist
	// any more: a replicating proxy pages through whole repositories while
	// they change (pull replication).
	after := ""
	if tok := r.URL.Query().Get("continuationToken"); tok != "" {
		name, p, ok := parseAssetID(tok)
		if !ok || name != rp.Name {
			writeMessage(w, http.StatusBadRequest, "invalid continuationToken")
			return
		}
		after = p
	}
	assets, err := h.DB.AssetPage(r.Context(), rp.Name, after, time.Time{}, pageSize+1)
	if err != nil {
		h.fail(w, err)
		return
	}
	next := ""
	if len(assets) > pageSize {
		assets = assets[:pageSize]
		next = assetID(rp.Name, assets[pageSize-1].Path)
	}
	items := make([]map[string]any, 0, len(assets))
	for _, a := range assets {
		items = append(items, h.assetJSON(rp, a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "continuationToken": nullable(next)})
}

func (h *Handler) findAsset(w http.ResponseWriter, r *http.Request) (*repo.Repository, meta.Asset, bool) {
	repoName, p, ok := parseAssetID(r.PathValue("id"))
	if !ok {
		writeMessage(w, http.StatusNotFound, "asset not found")
		return nil, meta.Asset{}, false
	}
	rp := h.lookupRepo(w, r, repoName)
	if rp == nil {
		return nil, meta.Asset{}, false
	}
	a, err := h.DB.GetAsset(r.Context(), rp.Name, p)
	if errors.Is(err, meta.ErrNotFound) {
		writeMessage(w, http.StatusNotFound, "asset not found")
		return nil, meta.Asset{}, false
	}
	if err != nil {
		h.fail(w, err)
		return nil, meta.Asset{}, false
	}
	return rp, a, true
}

func (h *Handler) getAsset(w http.ResponseWriter, r *http.Request) {
	if rp, a, ok := h.findAsset(w, r); ok {
		writeJSON(w, http.StatusOK, h.assetJSON(rp, a))
	}
}

// deleteAsset: Command Center deletes components, not single files, so a
// raw file (a component of its own) can be deleted here; other formats
// answer 400 and point at DELETE /components/{id}.
func (h *Handler) deleteAsset(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p.Anonymous() {
		deny(w, p)
		return
	}
	rp, a, ok := h.findAsset(w, r)
	if !ok {
		return
	}
	if rp.Format != "raw" {
		writeMessage(w, http.StatusBadRequest, "Command Center deletes whole components: use DELETE /service/rest/v1/components/{id}")
		return
	}
	h.deleteDerived(w, r, rp, &comp{repo: rp.Name, format: "raw", name: a.Path, paths: []string{a.Path}})
}

// ---- search ----

// criteria are the Nexus search parameters Command Center can answer.
type criteria struct {
	q, group, name, version, extension, classifier string
	sort, direction                                string
}

func parseCriteria(q url.Values) criteria {
	first := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(q.Get(k)); v != "" {
				return v
			}
		}
		return ""
	}
	c := criteria{
		q:          first("q"),
		group:      first("group", "maven.groupId", "npm.scope"),
		name:       first("name", "maven.artifactId", "docker.imageName"),
		version:    first("version", "maven.baseVersion", "docker.imageTag"),
		extension:  first("maven.extension"),
		classifier: first("maven.classifier"),
		sort:       first("sort"),
		direction:  strings.ToLower(first("direction")),
	}
	if c.direction == "" {
		c.direction = "asc"
		if c.sort == "version" {
			c.direction = "desc"
		}
	}
	return c
}

// match is Nexus's exact match, case-insensitive, with "*" wildcards.
func match(want, got string) bool {
	if want == "" {
		return true
	}
	if !strings.Contains(want, "*") {
		return strings.EqualFold(want, got)
	}
	parts := strings.Split(want, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	re := regexp.MustCompile("(?is)^" + strings.Join(parts, ".*") + "$")
	return re.MatchString(got)
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// assetParts is the classifier and extension of a maven file
// name-version[-classifier].ext; other formats have only an extension.
func assetParts(c *comp, p string) (classifier, ext string) {
	file := path.Base(p)
	if c.format != "maven" {
		return "", strings.TrimPrefix(path.Ext(file), ".")
	}
	rest, ok := strings.CutPrefix(file, c.name+"-"+c.version)
	if !ok {
		return "", strings.TrimPrefix(path.Ext(file), ".")
	}
	if cl, ok := strings.CutPrefix(rest, "-"); ok {
		classifier, ext, _ = strings.Cut(cl, ".")
		return classifier, ext
	}
	return "", strings.TrimPrefix(rest, ".")
}

// matches reports whether c matches cr, with its paths cut to the matching
// files when extension or classifier are given (nil when none match).
func (cr criteria) matches(c *comp) *comp {
	group := c.group
	if c.format == "npm" {
		group = strings.TrimPrefix(group, "@")
	}
	if !match(strings.TrimPrefix(cr.group, "@"), group) || !match(cr.name, c.name) || !match(cr.version, c.version) {
		return nil
	}
	if cr.q != "" && !(containsFold(c.group, cr.q) || containsFold(c.name, cr.q) || containsFold(c.version, cr.q) ||
		slices.ContainsFunc(c.paths, func(p string) bool { return containsFold(p, cr.q) })) {
		return nil
	}
	if cr.extension == "" && cr.classifier == "" {
		return c
	}
	out := *c
	out.paths = nil
	for _, p := range c.paths {
		cl, ext := assetParts(c, p)
		if match(cr.classifier, cl) && match(strings.TrimPrefix(cr.extension, "."), ext) {
			out.paths = append(out.paths, p)
		}
	}
	if len(out.paths) == 0 {
		return nil
	}
	return &out
}

// compareVersions orders versions: semver when both are, else segment by
// segment with numeric segments compared as numbers (1.10 after 1.9).
func compareVersions(a, b string) int {
	if va, ok := semver.Parse(a); ok {
		if vb, ok := semver.Parse(b); ok {
			return semver.Compare(va, vb)
		}
	}
	sep := func(r rune) bool { return r == '.' || r == '-' || r == '_' || r == '+' }
	as, bs := strings.FieldsFunc(a, sep), strings.FieldsFunc(b, sep)
	for i := 0; i < len(as) && i < len(bs); i++ {
		na, ea := strconv.Atoi(as[i])
		nb, eb := strconv.Atoi(bs[i])
		var c int
		if ea == nil && eb == nil {
			c = na - nb
		} else {
			c = strings.Compare(as[i], bs[i])
		}
		if c != 0 {
			return c
		}
	}
	return len(as) - len(bs)
}

func (cr criteria) order(comps []*comp) {
	if cr.sort == "" {
		return
	}
	key := func(c *comp) string {
		switch cr.sort {
		case "group":
			return c.group
		case "name":
			return c.name
		case "repository":
			return c.repo
		case "format":
			return c.format
		}
		return c.version
	}
	slices.SortStableFunc(comps, func(a, b *comp) int {
		var c int
		if cr.sort == "version" {
			c = compareVersions(a.version, b.version)
		} else {
			c = strings.Compare(key(a), key(b))
		}
		if cr.direction == "desc" {
			c = -c
		}
		return c
	})
}

// searched runs a search: the matching components of the readable
// repositories the parameters select, in the requested order.
func (h *Handler) searched(w http.ResponseWriter, r *http.Request) ([]*comp, criteria, bool) {
	q := r.URL.Query()
	p := principal(r)
	cr := parseCriteria(q)
	var repos []*repo.Repository
	if name := q.Get("repository"); name != "" {
		rp := h.lookupRepo(w, r, name)
		if rp == nil {
			return nil, cr, false
		}
		repos = []*repo.Repository{rp}
	} else if repos = h.readableAll(p); p.Anonymous() && len(repos) == 0 {
		deny(w, p)
		return nil, cr, false
	}
	out, err := h.find(r.Context(), repos, q)
	if err != nil {
		h.fail(w, err)
		return nil, cr, false
	}
	return out, cr, true
}

// find runs a search in repos (groups skipped): the components that match
// the parameters q, the format and tag included, in the requested order.
func (h *Handler) find(ctx context.Context, repos []*repo.Repository, q url.Values) ([]*comp, error) {
	cr := parseCriteria(q)
	format := ccFormat(q.Get("format"))
	tagged, err := h.tagged(ctx, q.Get("tag"))
	if err != nil {
		return nil, err
	}
	var out []*comp
	for _, rp := range repos {
		if rp.Type == "group" || (format != "" && rp.Format != format) || (tagged != nil && !tagged[meta.ComponentKey{Repo: rp.Name}]) {
			continue
		}
		comps, err := h.components(ctx, rp)
		if err != nil {
			return nil, err
		}
		for _, c := range comps {
			if tagged != nil && !tagged[c.metaKey()] {
				continue
			}
			if m := cr.matches(c); m != nil {
				out = append(out, m)
			}
		}
	}
	cr.order(out)
	return out, nil
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	comps, _, ok := h.searched(w, r)
	if !ok {
		return
	}
	h.writeComponentPage(w, r, comps, r.URL.Query().Get("continuationToken"))
}

type hit struct {
	rp *repo.Repository
	a  meta.Asset
}

// hits flattens the components' files, in component order.
func (h *Handler) hits(ctx context.Context, comps []*comp) ([]hit, error) {
	var out []hit
	for _, c := range comps {
		rp := h.Repos.Get(c.repo)
		for _, p := range c.paths {
			a, err := h.DB.GetAsset(ctx, c.repo, p)
			if errors.Is(err, meta.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out = append(out, hit{rp, a})
		}
	}
	return out, nil
}

func (h *Handler) searchAssets(w http.ResponseWriter, r *http.Request) {
	comps, _, ok := h.searched(w, r)
	if !ok {
		return
	}
	hits, err := h.hits(r.Context(), comps)
	if err != nil {
		h.fail(w, err)
		return
	}
	hits, next, ok := page(hits, func(x hit) string { return assetID(x.rp.Name, x.a.Path) }, r.URL.Query().Get("continuationToken"))
	if !ok {
		writeMessage(w, http.StatusBadRequest, "invalid continuationToken")
		return
	}
	items := make([]map[string]any, 0, len(hits))
	for _, x := range hits {
		items = append(items, h.assetJSON(x.rp, x.a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "continuationToken": nullable(next)})
}

// searchDownload redirects to the one matching asset; several match only
// with a sort, which picks the first (sort=version&direction=desc: the
// newest), as Nexus does.
func (h *Handler) searchDownload(w http.ResponseWriter, r *http.Request) {
	comps, cr, ok := h.searched(w, r)
	if !ok {
		return
	}
	hits, err := h.hits(r.Context(), comps)
	if err != nil {
		h.fail(w, err)
		return
	}
	switch {
	case len(hits) == 0:
		writeMessage(w, http.StatusNotFound, "Search returned no assets")
	case len(hits) > 1 && cr.sort == "":
		writeMessage(w, http.StatusBadRequest, "Search returned multiple assets, please refine search criteria to find a single asset")
	default:
		http.Redirect(w, r, h.repoURL(hits[0].rp.Name)+"/"+escapePath(hits[0].a.Path), http.StatusFound)
	}
}

// ---- status, security, blob stores, tasks ----

// statusWritable is the readiness check (Kubernetes, load balancers): 200
// while the node should take requests (meta.DB.Ready: the database answers,
// the node is not draining, its schema is compatible and
// its lock connection and listener are up), else 503.
func (h *Handler) statusWritable(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.DB.Ready(ctx); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// statusCheck is the UI's health checks in Nexus's shape.
func (h *Handler) statusCheck(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/status/checks", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var res struct {
		Checks []struct {
			Name        string `json:"name"`
			Healthy     bool   `json:"healthy"`
			Message     string `json:"message"`
			DurationMs  int64  `json:"durationMs"`
			LastChecked string `json:"lastChecked"`
		} `json:"checks"`
	}
	json.Unmarshal(body, &res)
	out := map[string]any{}
	for _, c := range res.Checks {
		t, _ := time.Parse(time.RFC3339, c.LastChecked)
		out[c.Name] = map[string]any{"healthy": c.Healthy, "message": c.Message, "error": "", "details": map[string]any{},
			"time": t.UnixMilli(), "duration": c.DurationMs, "timestamp": c.LastChecked}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) users(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	want := r.URL.Query().Get("userId")
	out := []map[string]any{}
	for _, u := range h.Auth.Snapshot().Users() {
		if want != "" && u.ID != want {
			continue
		}
		source := u.Source
		if source == "" || source == meta.SourceLocal {
			source = "default"
		}
		roles := u.Roles
		if roles == nil {
			roles = []string{}
		}
		out = append(out, map[string]any{"userId": u.ID, "firstName": u.Name, "lastName": "", "emailAddress": u.Email,
			"source": source, "status": u.Status, "readOnly": false, "roles": roles, "externalRoles": []string{}})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) roles(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	out := []map[string]any{}
	for _, role := range h.Auth.Snapshot().Roles() {
		privs := []string{}
		for _, p := range role.Privileges {
			for _, a := range p.Actions {
				privs = append(privs, p.Target+":"+a)
			}
		}
		out = append(out, map[string]any{"id": role.ID, "source": "default", "name": role.ID, "description": role.Description,
			"readOnly": role.ID == meta.AdminRole, "privileges": privs, "roles": []string{}})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) blobStores(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/blobstores", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var res struct {
		Stores []blobStoreRow `json:"blobstores"`
	}
	json.Unmarshal(body, &res)
	out := []map[string]any{}
	for _, s := range res.Stores {
		typ := "File"
		if s.Type == meta.BlobStoreS3 {
			typ = "S3"
		}
		if s.Type == meta.BlobStoreGroup {
			typ = "Group"
		}
		if s.Type == meta.BlobStoreAzure {
			typ = "Azure Cloud Storage"
		}
		if s.Type == meta.BlobStoreGoogle {
			typ = "Google Cloud Storage"
		}
		var free int64
		if s.FsFree != nil {
			free = *s.FsFree
		}
		out = append(out, map[string]any{"name": s.Name, "type": typ, "unavailable": false, "blobCount": s.BlobCount,
			"totalSizeInBytes": s.UsedBytes, "availableSpaceInBytes": free, "softQuota": softQuota(s.Quota)})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- blob store soft quota (Nexus: type spaceUsedQuota | spaceRemainingQuota, limit in MB) ----

type uiQuota struct {
	Type       string `json:"type"`
	LimitBytes int64  `json:"limitBytes"`
}

type nexusQuota struct {
	Type  string `json:"type"`
	Limit int64  `json:"limit"` // MB, as Nexus's API
}

var quotaTypes = map[string]string{"spaceUsedQuota": "spaceUsedBytes", "spaceRemainingQuota": "spaceRemainingBytes"}

func softQuota(q *uiQuota) *nexusQuota {
	if q == nil {
		return nil
	}
	for nx, cc := range quotaTypes {
		if cc == q.Type {
			return &nexusQuota{Type: nx, Limit: q.LimitBytes >> 20}
		}
	}
	return nil
}

func (q *nexusQuota) ui() any {
	if q == nil {
		return nil
	}
	typ, ok := quotaTypes[q.Type]
	if !ok {
		typ = q.Type // the UI's own names are accepted too, and refused there otherwise
	}
	return uiQuota{Type: typ, LimitBytes: q.Limit << 20}
}

type blobStoreRow struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	BlobCount int64    `json:"blobCount"`
	UsedBytes int64    `json:"usedBytes"`
	FsFree    *int64   `json:"fsFree"`
	Quota     *uiQuota `json:"quota"`
	Violated  bool     `json:"quotaViolated"`
}

// quotaStatus: GET /blobstores/{name}/quota-status.
func (h *Handler) quotaStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	status, body := h.call(r, http.MethodGet, "/-/api/blobstores/"+url.PathEscape(name), nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var s blobStoreRow
	json.Unmarshal(body, &s)
	msg := ""
	if s.Violated {
		msg = "Blob store " + name + " is over its soft quota"
	}
	writeJSON(w, http.StatusOK, map[string]any{"isViolation": s.Violated, "message": msg, "blobStoreName": name})
}

type fileBlobStoreBody struct {
	Name      string      `json:"name"`
	Path      string      `json:"path"`
	SoftQuota *nexusQuota `json:"softQuota"`
}

func (h *Handler) createFileBlobStore(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var b fileBlobStoreBody
	if !decodeSmall(w, r, &b) {
		return
	}
	if b.Path == b.Name { // Nexus's default, relative to its work directory: the default location here
		b.Path = ""
	}
	status, body := h.call(r, http.MethodPost, "/-/api/blobstores", map[string]any{"name": b.Name, "path": b.Path, "type": "file", "quota": b.SoftQuota.ui()})
	if status != http.StatusCreated {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) updateFileBlobStore(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var b fileBlobStoreBody
	if !decodeSmall(w, r, &b) {
		return
	}
	status, body := h.call(r, http.MethodPut, "/-/api/blobstores/"+url.PathEscape(r.PathValue("name")), map[string]any{"quota": b.SoftQuota.ui()})
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) tasks(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/tasks", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var res struct {
		Tasks []uiTask `json:"tasks"`
	}
	json.Unmarshal(body, &res)
	items := []map[string]any{}
	for _, t := range res.Tasks {
		items = append(items, nexusTaskItem(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "continuationToken": nil})
}

// nexusTaskTypes maps Command Center task types to the Nexus type ids
// scripts know; a type without one keeps its own id.
var nexusTaskTypes = map[string]string{
	"repository.rebuild-metadata": "repository.maven.rebuild-metadata",
	"maven.purge-snapshots":       "repository.maven.remove-snapshots",
	"blobstore.compact":           "blobstore.compact",
	"blobstore.delete-temp-files": "blobstore.delete-temp-files",
	"repository.cleanup":          "repository.cleanup",
}

func nexusTaskType(typ string) string {
	if n, ok := nexusTaskTypes[typ]; ok {
		return n
	}
	return typ
}
