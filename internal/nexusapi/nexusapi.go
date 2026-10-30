// Package nexusapi serves the part of the Sonatype Nexus Repository 3 REST
// API (v1) that scripts, CI jobs and tools use, at /service/rest/v1, on
// Command Center's repositories. Reads go to the registry and the metadata
// database; every write (repository create, update, delete; component
// delete) is an in-process call of the web UI's JSON API, so validation,
// events and cache invalidation stay in one place.
package nexusapi

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
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
	"commandcenter/internal/resolve"
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
	// Store drops a proxy's cached file, and ServeRepository serves an
	// in-process request for /repository/{name}/{path...} (server.Server's):
	// the component upload sends each file there as the format's own client
	// would, and single-file deletes go there; nil disables both.
	Store           *repo.Store
	ServeRepository func(http.ResponseWriter, *http.Request)
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
	h.repoAPIRoutes(m, p)
	h.cacheRoutes(m, p)
	h.mux = m
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.once.Do(h.init)
	w.Header().Set("Cache-Control", "no-store")
	if strings.HasPrefix(r.URL.Path, BetaPrefix+"/") {
		if r = betaRequest(r); r == nil {
			writeMessage(w, http.StatusNotFound, "not found")
			return
		}
	}
	h.mux.ServeHTTP(w, r)
}

// ---- JSON and errors ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeMessage answers like Nexus's single-message errors: {"id": "*",
// "message": ...}.
func writeMessage(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"id": "*", "message": msg})
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

// readable returns the named repository when p may browse or read it; a
// hidden repository is as unknown as a missing one.
func (h *Handler) readable(p auth.Principal, name string) *repo.Repository {
	rp := h.Repos.Get(name)
	if rp == nil || !h.Auth.Can(p, name, auth.Browse) {
		return nil
	}
	return rp
}

// readableAll returns every repository p may browse or read, by name.
func (h *Handler) readableAll(p auth.Principal) []*repo.Repository {
	var out []*repo.Repository
	for _, rp := range h.Repos.Snapshot().Sorted {
		if h.Auth.Can(p, rp.Name, auth.Browse) {
			out = append(out, rp)
		}
	}
	return out
}

// lookupRepo answers the repository whose content the query parameter (or
// an id) names, which the caller may list itself or through a group that
// contains it, or 400, 401 (anonymous) or 404.
func (h *Handler) lookupRepo(w http.ResponseWriter, r *http.Request, name string) *repo.Repository {
	if name == "" {
		writeMessage(w, http.StatusBadRequest, "repository is required")
		return nil
	}
	rp := h.Repos.Get(name)
	if rp == nil || !h.viewOf(r).can(name) {
		h.hidden(w, principal(r))
		return nil
	}
	return rp
}

// hidden answers for a repository p may not see: 401 for anonymous callers
// (a challenge), else 404 as for a missing one.
func (h *Handler) hidden(w http.ResponseWriter, p auth.Principal) {
	if p.Anonymous() {
		deny(w, p)
	} else {
		writeMessage(w, http.StatusNotFound, "repository not found")
	}
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
	switch {
	case d == config.Never:
		return -1
	case d.Off():
		return 0
	}
	return int(time.Duration(d) / time.Minute)
}

func (h *Handler) settingsJSON(rp *repo.Repository) map[string]any {
	d := rp.Repository
	storage := map[string]any{"blobStoreName": repo.StoreOf(d), "strictContentTypeValidation": d.StrictContentTypeValidation}
	if d.Type == "hosted" {
		storage["writePolicy"] = strings.ToUpper(d.WritePolicy)
	}
	policies := append([]string{}, d.CleanupPolicyNames()...)
	out := map[string]any{"name": d.Name, "format": nexusFormat(d.Format), "type": d.Type, "url": h.repoURL(d.Name),
		"online": d.IsOnline(), "storage": storage}
	if d.Type != "group" { // a Nexus group carries storage and group only
		out["cleanup"] = map[string]any{"policyNames": policies}
	}
	if d.Type == "hosted" {
		out["component"] = map[string]any{"proprietaryComponents": false}
	}
	formatBlocks(out, rp)
	switch d.Type {
	case "proxy":
		out["proxy"] = map[string]any{"remoteUrl": publicRemote(rp), "contentMaxAge": minutes(d.ContentMaxAge), "metadataMaxAge": minutes(d.MetadataMaxAge)}
		out["negativeCache"] = map[string]any{"enabled": d.NegativeCacheEnabled == nil || *d.NegativeCacheEnabled, "timeToLive": minutes(d.NegativeCacheTTL)}
		hc := map[string]any{"blocked": d.Blocked, "autoBlock": d.AutoBlocks(), "authentication": nil, "connection": config.CleanConnection(d.Connection)}
		if a := d.RemoteAuth; a != nil && a.BearerToken != "" { // the proxy sends the token when there is one
			hc["authentication"] = map[string]any{"type": "bearerToken"}
		} else if a != nil && a.Username != "" {
			hc["authentication"] = map[string]any{"type": "username", "username": a.Username}
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
		g := map[string]any{"memberNames": append([]string{}, d.Members...), "writableMember": nil}
		if d.WriteMember != "" {
			g["writableMember"] = d.WriteMember
		}
		out["group"] = g
	}
	if d.Format == "maven" && d.Type != "group" { // a Nexus group has no maven object
		vp := strings.ToUpper(d.VersionPolicy)
		if vp == "" {
			vp = "MIXED"
		}
		lp, cd := "STRICT", "INLINE"
		if d.LayoutPolicy == "permissive" {
			lp = "PERMISSIVE"
		}
		if d.ContentDisposition == "attachment" {
			cd = "ATTACHMENT"
		}
		out["maven"] = map[string]any{"versionPolicy": vp, "layoutPolicy": lp, "contentDisposition": cd}
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
	if rp := h.readable(principal(r), r.PathValue("name")); rp != nil {
		writeJSON(w, http.StatusOK, h.repoJSON(rp, h.usage(r.Context())))
	} else {
		h.hidden(w, principal(r))
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
		LatestPolicy  *bool  `json:"latestPolicy"` // docker hosted

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
		Enabled    *bool `json:"enabled"`
		TimeToLive *int  `json:"timeToLive"`
	} `json:"negativeCache"`
	Group struct {
		MemberNames    []string `json:"memberNames"`
		WritableMember string   `json:"writableMember"`
	} `json:"group"`
	HTTPClient struct {
		Blocked        bool  `json:"blocked"`
		AutoBlock      *bool `json:"autoBlock"`
		Authentication *struct {
			Type        string `json:"type"` // username (or empty), bearerToken; ntlm is refused
			Username    string `json:"username"`
			Password    string `json:"password"`
			BearerToken string `json:"bearerToken"`
		} `json:"authentication"`
		// Connection is the proxy's own connection settings; absent keeps them.
		Connection *config.Connection `json:"connection"`
	} `json:"httpClient"`
	Maven struct {
		VersionPolicy      string `json:"versionPolicy"`
		LayoutPolicy       string `json:"layoutPolicy"`       // STRICT | PERMISSIVE
		ContentDisposition string `json:"contentDisposition"` // INLINE | ATTACHMENT
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
	formatBodies
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
// fields: the UI refuses unknown ones), or returns the values Command Center
// cannot honour. cur is the stored definition on update (nil on create):
// the settings a Nexus body has no field for are kept from it.
func uiRepo(format, typ string, b repoBody, cur *config.Repository) (map[string]any, []string) {
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
		out["negativeCacheEnabled"] = b.NegativeCache.Enabled
		if c := b.HTTPClient.Connection; c != nil {
			out["connection"] = c
		}
	}
	if cur != nil {
		out["anonymousRead"], out["depth"], out["gpgPublicKey"] = cur.AnonymousRead, cur.Depth, cur.GPGPublicKey
		out["contentDisposition"] = cur.ContentDisposition
	}
	if cd := strings.ToLower(b.Raw.ContentDisposition); cd != "" {
		out["contentDisposition"] = cd
	}
	if ccFormat(format) == "maven" && typ != "group" {
		if cd := strings.ToLower(b.Maven.ContentDisposition); cd != "" {
			out["contentDisposition"] = cd
		}
		if lp := strings.ToLower(b.Maven.LayoutPolicy); lp != "" { // absent keeps the stored one
			out["layoutPolicy"] = lp
		}
	}
	if typ == "group" { // Nexus group bodies name a blob store and a maven object; Command Center groups have neither
		out["blobStore"], out["versionPolicy"] = "", ""
	}
	if b.RoutingRule != nil {
		out["routingRule"] = *b.RoutingRule
	} else if b.RoutingRuleName != nil {
		out["routingRule"] = *b.RoutingRuleName
	}
	if b.Cleanup != nil {
		out["cleanupPolicies"] = append([]string{}, b.Cleanup.PolicyNames...)
	}
	if a := b.HTTPClient.Authentication; a != nil {
		if strings.EqualFold(a.Type, "bearerToken") { // an empty token on update keeps the stored one
			out["remoteAuth"] = map[string]any{"bearerToken": a.BearerToken, "clearPassword": true}
		} else {
			out["remoteAuth"] = map[string]any{"username": a.Username, "password": a.Password, "clearBearerToken": a.Type != ""}
		}
	}
	if b.Replication != nil {
		out["replication"] = b.Replication
	}
	return out, formatRequest(ccFormat(format), typ, b, out, cur)
}

// supportedAuth refuses a remote authentication type Command Center does
// not speak with 400, rather than sending the credentials as Basic.
func supportedAuth(w http.ResponseWriter, b repoBody) bool {
	a := b.HTTPClient.Authentication
	if a == nil || a.Type == "" || a.Type == "username" || a.Type == "bearerToken" {
		return true
	}
	msg := fmt.Sprintf("httpClient.authentication.type %q is not supported: use username or bearerToken", a.Type)
	if a.Type == "ntlm" {
		msg = "httpClient.authentication.type \"ntlm\" is not supported: NTLM remotes need a proxy that speaks NTLM to them, or username or bearerToken authentication"
	}
	writeValidation(w, []string{msg})
	return false
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
	if !ok || !supportedAuth(w, b) {
		return
	}
	req, errs := uiRepo(r.PathValue("format"), r.PathValue("type"), b, nil)
	if len(errs) > 0 {
		writeValidation(w, errs)
		return
	}
	status, body := h.call(r, http.MethodPost, "/-/api/repositories", req)
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
	if !ok || !supportedAuth(w, b) {
		return
	}
	name := r.PathValue("name")
	var cur *config.Repository // nil: the UI answers 404
	if rp := h.Repos.Get(name); rp != nil {
		cur = &rp.Repository
	}
	if cur != nil && b.Name != "" && b.Name != name {
		writeValidation(w, []string{"name: the body names " + strconv.Quote(b.Name) + ", the path " + strconv.Quote(name) + "; a repository cannot be renamed"})
		return
	}
	b.Name = name
	req, errs := uiRepo(r.PathValue("format"), r.PathValue("type"), b, cur)
	if len(errs) > 0 {
		writeValidation(w, errs)
		return
	}
	status, body := h.call(r, http.MethodPut, "/-/api/repositories/"+url.PathEscape(name), req)
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

// comp is a derived component with the paths of its files and the
// repository its download URLs use (a group it was found through; "" for
// its own).
type comp struct {
	repo, format, group, name, version string
	paths                              []string
	via                                string
}

func (c *comp) key() string { return c.repo + ":" + c.group + ":" + c.name + ":" + c.version }

// compID and assetID are the opaque ids: base64url of "<repo>:<group>:<name>:<version>"
// and "<repo>:<path>", with the stored path and raw name (no leading "/";
// one is accepted). A repository name holds no ":", and a name holds one
// only for raw, whose version is empty, so the last ":" splits it.
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
	c.name, c.version = strings.TrimPrefix(parts[2][:i], "/"), parts[2][i+1:]
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
	p = strings.TrimPrefix(p, "/")
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
		if c.format == "maven" { // the main file first, so search/assets/download finds it before a classified one
			slices.SortStableFunc(c.paths, func(a, b string) int {
				ca, _, _ := component.MavenFileParts(path.Base(a), c.name, c.version)
				cb, _, _ := component.MavenFileParts(path.Base(b), c.name, c.version)
				return cmpBool(ca != "", cb != "")
			})
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *comp) int {
		return strings.Compare(a.key(), b.key())
	})
	return out, nil
}

// cmpBool orders false before true.
func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

func rfc3339(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format("2006-01-02T15:04:05.000-07:00") // Nexus's ISO form: milliseconds, +00:00
}

// assetJSON renders a file of rp as Nexus 3.91 does: the path with a
// leading "/", the download URL on via's URL ("": rp's; a group a search
// went through), only the checksums stored, and the format's attributes
// ({"maven2": {groupId, artifactId, version, extension, classifier}},
// {"<format>": {}} for the others).
func (h *Handler) assetJSON(rp *repo.Repository, via string, a meta.Asset) map[string]any {
	sums := map[string]string{}
	for k, v := range map[string]string{"sha1": a.SHA1, "sha256": a.BlobSHA256, "md5": a.MD5, "sha512": a.SHA512} {
		if v != "" {
			sums[k] = v
		}
	}
	var verified any
	if rp.Type == "proxy" {
		verified = rfc3339(a.CheckedAt) // the last time the remote was consulted
	}
	attrs := map[string]string{}
	if rp.Format == "maven" {
		attrs = maven2Attributes(a.Path) // a SNAPSHOT build's own version
	}
	return map[string]any{
		"id":           assetID(rp.Name, a.Path),
		"path":         "/" + a.Path,
		"downloadUrl":  h.repoURL(cmp.Or(via, rp.Name)) + "/" + escapePath(a.Path),
		"repository":   rp.Name,
		"format":       nexusFormat(rp.Format),
		"checksum":     sums,
		"contentType":  a.ContentType,
		"lastModified": rfc3339(a.LastModified), "blobCreated": rfc3339(a.CreatedAt), "lastDownloaded": rfc3339(a.LastDownloaded),
		"blobUpdated": rfc3339(a.LastModified), "lastVerified": verified,
		"fileSize": a.Size, "blobStoreName": repo.StoreOf(rp.Repository),
		"uploader": nil, "uploaderIp": nil, // not recorded
		nexusFormat(rp.Format): attrs,
	}
}

// maven2Attributes is a Maven asset's "maven2" object, as Nexus lists it:
// groupId, artifactId, version (a SNAPSHOT build's), classifier (when the
// file has one) and extension; empty for metadata and other files.
func maven2Attributes(p string) map[string]string {
	g, a, v, ok := component.DeriveMaven(p)
	if !ok {
		return map[string]string{}
	}
	cl, ext, ok := component.MavenFileParts(path.Base(p), a, v)
	if !ok {
		return map[string]string{}
	}
	out := map[string]string{"groupId": g, "artifactId": a, "version": v, "extension": ext}
	if cl != "" {
		out["classifier"] = cl
	}
	return out
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
		assets = append(assets, h.assetJSON(rp, c.via, a))
	}
	// Nexus: a raw file's group is its directory, its name the path with a
	// leading "/", and it has no version.
	name := c.name
	if c.format == "raw" {
		name = "/" + name
	}
	tags, err := h.DB.ComponentTags(ctx, c.metaKey())
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": compID(c), "repository": c.repo, "format": nexusFormat(c.format),
		"group": nullable(compGroup(c)), "name": name, "version": nullable(c.version), "assets": assets, "tags": tags}, nil
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		writeMessage(w, http.StatusServiceUnavailable, "server busy")
		return
	}
	writeMessage(w, http.StatusInternalServerError, "internal error")
}

// listComponents lists a repository's components; a group's are its
// members' (Nexus: a helper for users), each with its own repository and
// downloads through the group.
func (h *Handler) listComponents(w http.ResponseWriter, r *http.Request) {
	rp := h.lookupRepo(w, r, r.URL.Query().Get("repository"))
	if rp == nil {
		return
	}
	v := h.viewOf(r)
	members := resolve.Leaves(rp)
	var comps []*comp
	for _, m := range members {
		cs, err := h.components(r.Context(), m)
		if err != nil {
			h.fail(w, err)
			return
		}
		for _, c := range cs {
			if c = v.visible(c); c != nil {
				if rp.Type == "group" {
					c.via = rp.Name
				}
				comps = append(comps, c)
			}
		}
	}
	token := r.URL.Query().Get("continuationToken")
	if tok, ok := parseCompID(token); token != "" && (!ok || !slices.ContainsFunc(members, func(m *repo.Repository) bool { return m.Name == tok.repo })) {
		writeMessage(w, http.StatusBadRequest, "invalid continuationToken")
		return
	}
	var cr criteria // repository and key order
	slices.SortFunc(comps, cr.compare)
	h.writeComponentPage(w, r, comps, cr, token)
}

// writeComponentPage answers the page of comps (sorted by cr) after token.
func (h *Handler) writeComponentPage(w http.ResponseWriter, r *http.Request, comps []*comp, cr criteria, token string) {
	comps, ok := h.after(comps, cr, token)
	if !ok {
		writeMessage(w, http.StatusBadRequest, "invalid continuationToken")
		return
	}
	next := ""
	if len(comps) > pageSize {
		comps = comps[:pageSize]
		next = compID(comps[pageSize-1])
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
		if c := h.viewOf(r).visible(comps[i]); c != nil {
			return rp, c
		}
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

// listAssets lists a repository's files, a group's members' files (by
// member name, then path), each with its own repository and downloads
// through the group.
func (h *Handler) listAssets(w http.ResponseWriter, r *http.Request) {
	rp := h.lookupRepo(w, r, r.URL.Query().Get("repository"))
	if rp == nil {
		return
	}
	members := resolve.Leaves(rp)
	slices.SortFunc(members, func(a, b *repo.Repository) int { return strings.Compare(a.Name, b.Name) })
	via := ""
	if rp.Type == "group" {
		via = rp.Name
	}
	// Keyset paging on the previous page's last asset, which need not exist
	// any more: a replicating proxy pages through whole repositories while
	// they change (pull replication), a cleanup script deletes as it pages.
	afterRepo, after := "", ""
	if tok := r.URL.Query().Get("continuationToken"); tok != "" {
		name, p, ok := parseAssetID(tok)
		if !ok || !slices.ContainsFunc(members, func(m *repo.Repository) bool { return m.Name == name }) {
			writeMessage(w, http.StatusBadRequest, "invalid continuationToken")
			return
		}
		afterRepo, after = name, p
	}
	v := h.viewOf(r)
	type item struct {
		rp *repo.Repository
		a  meta.Asset
	}
	var page []item
	for _, m := range members {
		if m.Name < afterRepo {
			continue
		}
		from := ""
		if m.Name == afterRepo {
			from = after
		}
		// Files the caller does not see are skipped; read on until the page
		// is full or the member ends.
		for len(page) <= pageSize {
			assets, err := h.DB.AssetPage(r.Context(), m.Name, from, time.Time{}, pageSize+1)
			if err != nil {
				h.fail(w, err)
				return
			}
			for _, a := range assets {
				if v.sees(m, a.Path) {
					page = append(page, item{m, a})
				}
			}
			if len(assets) <= pageSize {
				break
			}
			from = assets[len(assets)-1].Path
		}
		if len(page) > pageSize {
			break
		}
	}
	next := ""
	if len(page) > pageSize {
		page = page[:pageSize]
		next = assetID(page[pageSize-1].rp.Name, page[pageSize-1].a.Path)
	}
	items := make([]map[string]any, 0, len(page))
	for _, x := range page {
		items = append(items, h.assetJSON(x.rp, via, x.a))
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
	if errors.Is(err, meta.ErrNotFound) || (err == nil && !h.viewOf(r).sees(rp, p)) {
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
		writeJSON(w, http.StatusOK, h.assetJSON(rp, "", a))
	}
}

// deleteAsset deletes one file, any format, by the format's rules:
//   - a proxy's (any format) cached copy is dropped; the next request
//     fetches it again;
//   - a hosted Maven file goes through the Maven handler's delete, which
//     rebuilds maven-metadata.xml;
//   - a hosted file that is a component of its own (a raw file, an npm
//     tarball, a .deb or .rpm, a .nupkg, ...) goes through the component
//     delete, which updates the format's index files;
//   - anything else is part of a larger component (one layer of a docker
//     image, one file of a Go module or a PyPI release) or generated
//     metadata: 400, delete the component instead.
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
	switch {
	case rp.Type == "proxy":
		h.dropCached(w, r, rp, a.Path)
	case rp.Type == "hosted" && rp.Format == "maven":
		h.deleteMavenFile(w, r, rp, a.Path)
	case rp.Type == "hosted":
		c, err := h.ownComponent(r.Context(), rp, a.Path)
		switch {
		case err != nil:
			h.fail(w, err)
		case c == nil:
			writeMessage(w, http.StatusBadRequest, "this file is part of a larger component or generated metadata: delete the component (DELETE /service/rest/v1/components/{id})")
		default:
			h.deleteDerived(w, r, rp, c)
		}
	default:
		writeMessage(w, http.StatusBadRequest, "a group has no files of its own: delete the file in its member")
	}
}

// dropCached deletes a proxy's cached file, after the delete check on its
// path.
func (h *Handler) dropCached(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) {
	pr := principal(r)
	switch {
	case !h.Auth.CanPath(pr, rp.Name, auth.Delete, selectorPath(rp, p)):
		deny(w, pr)
		return
	case h.Store == nil:
		writeMessage(w, http.StatusNotImplemented, "file deletion is not available")
		return
	}
	err := h.Store.Tx(r.Context(), rp, func(tx *meta.Tx) error {
		_, err := tx.DeleteAsset(r.Context(), rp.Name, p)
		return err
	})
	switch {
	case errors.Is(err, meta.ErrNotFound):
		writeMessage(w, http.StatusNotFound, "asset not found")
	case err != nil:
		h.fail(w, err)
	default:
		h.Store.Emit(r, "delete", rp.Name, p, "")
		w.WriteHeader(http.StatusNoContent)
	}
}

// deleteMavenFile deletes a hosted Maven file as a client's DELETE does
// (the server's checks, then the Maven handler).
func (h *Handler) deleteMavenFile(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p string) {
	if h.ServeRepository == nil {
		writeMessage(w, http.StatusNotImplemented, "file deletion is not available")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodDelete, (&url.URL{Path: "/repository/" + rp.Name + "/" + p}).String(), nil)
	if err != nil {
		writeMessage(w, http.StatusBadRequest, "invalid path")
		return
	}
	req.SetPathValue("name", rp.Name)
	req.SetPathValue("path", p)
	req.Header.Set("X-Request-Id", r.Header.Get("X-Request-Id"))
	req.RemoteAddr = r.RemoteAddr
	rec := &recorder{h: http.Header{}}
	h.ServeRepository(rec, req)
	switch {
	case rec.status == 0 || rec.status < 300:
		w.WriteHeader(http.StatusNoContent)
	case rec.status == http.StatusUnauthorized || rec.status == http.StatusForbidden:
		deny(w, principal(r))
	default:
		writeMessage(w, rec.status, strings.TrimSpace(rec.body.String()))
	}
}

// ownComponent returns the component file p of hosted rp is all of, nil
// when p is no component's or one of several files. ponytail: a
// whole-repository scan of paths per delete, as findComponent does; index
// the derivation if deletes in huge repositories get slow.
func (h *Handler) ownComponent(ctx context.Context, rp *repo.Repository, p string) (*comp, error) {
	group, name, version, ok := component.DeriveAsset(rp.Format, p)
	if !ok {
		return nil, nil
	}
	n := 0
	if err := h.DB.WalkAssetPaths(ctx, rp.Name, "", func(q string) {
		if g, nm, v, ok := component.DeriveAsset(rp.Format, q); ok && g == group && nm == name && v == version {
			n++
		}
	}); err != nil || n != 1 {
		return nil, err
	}
	return &comp{repo: rp.Name, format: rp.Format, group: group, name: name, version: version, paths: []string{p}}, nil
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
		if !strings.HasPrefix(strings.ToLower(u.ID), strings.ToLower(want)) { // Nexus: a case-insensitive prefix
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
		out = append(out, map[string]any{"name": s.Name, "type": typ, "unavailable": s.Unavailable != "", "blobCount": s.BlobCount,
			"totalSizeInBytes": s.UsedBytes, "availableSpaceInBytes": free, "softQuota": softQuota(s.Quota)})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- blob store soft quota (Nexus: type spaceUsedQuota | spaceRemainingQuota, limit in bytes) ----

type uiQuota = meta.Quota

// nexusQuota is Nexus's softQuota. Its REST API stores and returns the limit
// as given, in bytes (its own UI converts from MB before calling it).
type nexusQuota struct {
	Type  string `json:"type"`
	Limit int64  `json:"limit"`
}

var quotaTypes = map[string]string{"spaceUsedQuota": "spaceUsedBytes", "spaceRemainingQuota": "spaceRemainingBytes"}

func softQuota(q *uiQuota) *nexusQuota {
	if q == nil {
		return nil
	}
	for nx, cc := range quotaTypes {
		if cc == q.Type {
			return &nexusQuota{Type: nx, Limit: q.LimitBytes}
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
	return uiQuota{Type: typ, LimitBytes: q.Limit}
}

type blobStoreRow struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	BlobCount int64    `json:"blobCount"`
	UsedBytes int64    `json:"usedBytes"`
	FsFree    *int64   `json:"fsFree"`
	Quota     *uiQuota `json:"quota"`
	Violated  bool     `json:"quotaViolated"`

	Unavailable string `json:"unavailable"` // why it did not open on this node
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
	bs, ok := h.storeOfType(w, r, r.PathValue("name"), meta.BlobStoreFile)
	if !ok {
		return
	}
	if b.Path != "" && b.Path != filePath(bs) { // all nodes share the path: it cannot move
		writeValidation(w, []string{"path: the path of blob store " + strconv.Quote(bs.Name) + " cannot be changed"})
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
	typ := r.URL.Query().Get("type") // Nexus's type id, or Command Center's
	for _, t := range res.Tasks {
		if typ == "" || typ == t.Type || typ == nexusTaskType(t.Type) {
			items = append(items, nexusTaskItem(t))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "continuationToken": nil})
}

// nexusTaskTypes maps Command Center task types to the Nexus type ids
// scripts know; a type without one keeps its own id.
var nexusTaskTypes = map[string]string{
	"repository.rebuild-metadata":  "repository.maven.rebuild-metadata",
	"maven.purge-snapshots":        "repository.maven.remove-snapshots",
	"blobstore.compact":            "blobstore.compact",
	"blobstore.delete-temp-files":  "blobstore.delete-temp-files",
	"repository.cleanup":           "repository.cleanup",
	"blobstore.recalculate-size":   "blobstore.metrics.reconcile",
	"maven.purge-unused-snapshots": "repository.maven.purge-unused-snapshots",
}

func nexusTaskType(typ string) string {
	if n, ok := nexusTaskTypes[typ]; ok {
		return n
	}
	return typ
}
