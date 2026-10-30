package nexusapi

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/sysinfo"
)

// The rest of the Nexus REST surface the Terraform provider and scripts
// use: the format sections of the closed formats' repository bodies, file
// and S3 blob stores, e-mail, the metrics endpoints, and the deprecated
// /service/rest/beta twins. Writes are in-process calls of the UI's API.

func (h *Handler) repoAPIRoutes(m *http.ServeMux, p string) {
	m.HandleFunc("GET "+p+"/blobstores/file/{name}", h.getFileBlobStore)
	m.HandleFunc("DELETE "+p+"/blobstores/{name}", h.deleteBlobStore)
	m.HandleFunc("POST "+p+"/blobstores/s3", h.createS3BlobStore)
	m.HandleFunc("GET "+p+"/blobstores/s3/{name}", h.getS3BlobStore)
	m.HandleFunc("PUT "+p+"/blobstores/s3/{name}", h.updateS3BlobStore)
	m.HandleFunc("GET "+p+"/email", h.getEmail)
	m.HandleFunc("PUT "+p+"/email", h.putEmail)
	m.HandleFunc("DELETE "+p+"/email", h.deleteEmail)
	m.HandleFunc("POST "+p+"/email/verify", h.verifyEmail)
	m.HandleFunc("GET /service/metrics/", h.metricsRedirect)
	m.HandleFunc("GET /service/rest/metrics/ping", h.metricsPing)
	m.HandleFunc("GET /service/rest/metrics/prometheus", h.metricsPrometheus)
	m.HandleFunc("GET /service/rest/metrics/data", h.metricsData)
	m.HandleFunc("GET /service/rest/metrics/threads", h.metricsThreads)
}

// ---- format sections of repository bodies ----

// formatBodies are the sections of the formats whose Nexus code is closed.
// Command Center answers each with what it does, and refuses a value it
// cannot honour instead of dropping it, so a client's plan converges.
type formatBodies struct {
	Docker *struct {
		V1Enabled bool    `json:"v1Enabled"`
		HTTPPort  *int    `json:"httpPort"`
		HTTPSPort *int    `json:"httpsPort"`
		Subdomain *string `json:"subdomain"`
	} `json:"docker"`
	DockerProxy *struct {
		IndexType                string   `json:"indexType"`
		IndexURL                 *string  `json:"indexUrl"`
		CacheForeignLayers       bool     `json:"cacheForeignLayers"`
		ForeignLayerURLWhitelist []string `json:"foreignLayerUrlWhitelist"`
	} `json:"dockerProxy"`
	Yum *struct {
		RepodataDepth *int   `json:"repodataDepth"`
		DeployPolicy  string `json:"deployPolicy"`
	} `json:"yum"`
	YumSigning *struct {
		Keypair *string `json:"keypair"`
	} `json:"yumSigning"`
	NugetProxy *struct {
		NugetVersion string `json:"nugetVersion"`
	} `json:"nugetProxy"`
	Npm       *quarantineBody  `json:"npm"`
	Pypi      *quarantineBody  `json:"pypi"`
	Cargo     *requireAuthBody `json:"cargo"`
	Terraform *requireAuthBody `json:"terraform"`
	Swift     *requireAuthBody `json:"swift"`
}

type quarantineBody struct {
	RemoveNonCataloged bool `json:"removeNonCataloged"`
	RemoveQuarantined  bool `json:"removeQuarantined"`
}

type requireAuthBody struct {
	RequireAuthentication *bool `json:"requireAuthentication"`
}

// formatRequest maps the format sections onto out (the UI's request) and
// returns the values Command Center cannot honour, as "field: why".
func formatRequest(format, typ string, b repoBody, out map[string]any, cur *config.Repository) []string {
	var errs []string
	refuse := func(bad bool, msg string) {
		if bad {
			errs = append(errs, msg)
		}
	}
	// component.proprietaryComponents is accepted and answered false: there
	// is no proprietary-namespace protection here (earlier builds took it too).
	const connector = ": every docker repository is served at /v2/<repository>/ on the server's own port; there are no per-repository connectors"
	switch format {
	case "docker":
		if d := b.Docker; d != nil { // forceBasicAuth: Basic credentials and registry tokens are both accepted
			refuse(d.V1Enabled, "docker.v1Enabled: the Docker registry API v1 is not served")
			refuse(d.HTTPPort != nil, "docker.httpPort"+connector)
			refuse(d.HTTPSPort != nil, "docker.httpsPort"+connector)
			refuse(d.Subdomain != nil && *d.Subdomain != "", "docker.subdomain"+connector)
		}
		refuse(typ == "hosted" && b.Storage.LatestPolicy != nil && *b.Storage.LatestPolicy,
			"storage.latestPolicy: every tag can be redeployed as the write policy allows")
		if p := b.DockerProxy; p != nil && typ == "proxy" {
			refuse(strings.EqualFold(p.IndexType, "CUSTOM") || p.IndexURL != nil && *p.IndexURL != "",
				"dockerProxy.indexType: there is no docker search index; use HUB or REGISTRY")
			refuse(p.CacheForeignLayers, "dockerProxy.cacheForeignLayers: foreign layers are not cached; clients fetch them from their own URLs")
			refuse(len(p.ForeignLayerURLWhitelist) > 0, "dockerProxy.foreignLayerUrlWhitelist: foreign layers are not cached")
		}
	case "yum":
		if y := b.Yum; y != nil && typ == "hosted" {
			if y.RepodataDepth != nil {
				out["depth"] = *y.RepodataDepth // 0 or 1; the UI refuses others
			}
			depth, _ := out["depth"].(int)
			refuse(depth > 0 && strings.EqualFold(y.DeployPolicy, "PERMISSIVE"),
				"yum.deployPolicy: at repodataDepth 1 every RPM must be in a directory (STRICT)")
		}
		if s := b.YumSigning; s != nil {
			refuse(s.Keypair != nil && *s.Keypair != "", "yumSigning.keypair: yum metadata is not signed")
		}
	case "nuget":
		if n := b.NugetProxy; n != nil && typ == "proxy" { // queryCacheItemMaxAge: queries follow metadataMaxAge
			refuse(n.NugetVersion != "" && !strings.EqualFold(n.NugetVersion, "V3"), "nugetProxy.nugetVersion: only NuGet V3 remotes are proxied")
		}
	case "npm", "pypi":
		q := b.Npm
		if format == "pypi" {
			q = b.Pypi
		}
		if q != nil && typ == "proxy" {
			refuse(q.RemoveQuarantined, format+".removeQuarantined: the firewall refuses quarantined files but leaves them in the metadata")
			refuse(q.RemoveNonCataloged, format+".removeNonCataloged: there is no component catalog")
		}
	case "cargo", "terraform", "swift":
		a := map[string]*requireAuthBody{"cargo": b.Cargo, "terraform": b.Terraform, "swift": b.Swift}[format]
		if a == nil || a.RequireAuthentication == nil {
			break
		}
		switch {
		case *a.RequireAuthentication: // the repository's anonymousRead override
			out["anonymousRead"] = false
		case cur == nil || cur.AnonymousRead != nil && !*cur.AnonymousRead:
			out["anonymousRead"] = nil // follow the global setting; an explicit true stays
		}
	}
	return errs
}

// formatBlocks adds the format sections of rp's settings.
func formatBlocks(out map[string]any, rp *repo.Repository) {
	d := rp.Repository
	switch {
	case d.Format == "docker":
		out["docker"] = map[string]any{"v1Enabled": false, "forceBasicAuth": true, "httpPort": nil, "httpsPort": nil, "subdomain": nil}
		if d.Type == "hosted" {
			out["storage"].(map[string]any)["latestPolicy"] = false
		}
		if d.Type == "proxy" {
			index := "REGISTRY"
			if rp.RemoteURL != nil && (rp.RemoteURL.Hostname() == "docker.io" || strings.HasSuffix(rp.RemoteURL.Hostname(), ".docker.io")) {
				index = "HUB"
			}
			out["dockerProxy"] = map[string]any{"indexType": index, "indexUrl": nil, "cacheForeignLayers": false, "foreignLayerUrlWhitelist": []string{}}
		}
	case d.Format == "yum" && d.Type == "hosted":
		out["yum"] = map[string]any{"repodataDepth": d.Depth, "deployPolicy": "STRICT"}
	case d.Format == "nuget" && d.Type == "proxy":
		out["nugetProxy"] = map[string]any{"queryCacheItemMaxAge": 3600, "nugetVersion": "V3"} // ponytail: Nexus's default; not a setting here
	case d.Format == "npm" && d.Type == "proxy":
		out["npm"] = map[string]any{"removeNonCataloged": false, "removeQuarantined": false}
	case d.Format == "pypi" && d.Type == "proxy":
		out["pypi"] = map[string]any{"removeQuarantined": false}
	case d.Format == "cargo" || d.Format == "terraform" || d.Format == "swift":
		out[d.Format] = map[string]any{"requireAuthentication": d.AnonymousRead != nil && !*d.AnonymousRead}
	}
}

// ---- file blob stores: GET one, and DELETE of any store ----

// storeOfType reads the named store, answering 404 when it is missing and
// 400 when it is of another type (as Nexus does).
func (h *Handler) storeOfType(w http.ResponseWriter, r *http.Request, name, typ string) (meta.BlobStore, bool) {
	bs, err := h.DB.GetBlobStore(r.Context(), name)
	switch {
	case errors.Is(err, meta.ErrNotFound):
		writeMessage(w, http.StatusNotFound, "blob store not found")
	case err != nil:
		writeMessage(w, http.StatusInternalServerError, "cannot read the blob store")
	case cmp.Or(bs.Type, meta.BlobStoreFile) != typ:
		writeMessage(w, http.StatusBadRequest, "blob store "+strconv.Quote(name)+" is not a "+typ+" blob store")
	default:
		return bs, true
	}
	return bs, false
}

// filePath is a file store's path as the Nexus API takes it on create: its
// name for the default location under the data directory.
func filePath(bs meta.BlobStore) string {
	if bs.Path == filepath.Join("blobstores", bs.Name) {
		return bs.Name
	}
	return bs.Path
}

func (h *Handler) getFileBlobStore(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	bs, ok := h.storeOfType(w, r, r.PathValue("name"), meta.BlobStoreFile)
	if ok {
		writeJSON(w, http.StatusOK, map[string]any{"path": filePath(bs), "softQuota": softQuota(bs.Quota)})
	}
}

func (h *Handler) deleteBlobStore(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodDelete, "/-/api/blobstores/"+url.PathEscape(r.PathValue("name")), nil)
	if status == http.StatusConflict {
		status = http.StatusBadRequest // in use (or the default store): Nexus's 400
	}
	if status != http.StatusNoContent {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- S3 blob stores ----

// secretPlaceholder is what Nexus answers for a stored secret; a PUT that
// sends it back keeps the secret.
const secretPlaceholder = "#~NXRM~PLACEHOLDER~PASSWORD~#"

// nexusS3 is Nexus's S3 blob store body. Requests name the security and
// connection sections security and advancedConnection, answers
// bucketSecurity and advancedBucketConnection; both are read.
type nexusS3 struct {
	Name      string      `json:"name"`
	Type      string      `json:"type,omitempty"`
	SoftQuota *nexusQuota `json:"softQuota"`
	Bucket    struct {
		Bucket struct {
			Region string `json:"region"`
			Name   string `json:"name"`
			Prefix string `json:"prefix"`
		} `json:"bucket"`
		Security     *s3Security   `json:"bucketSecurity,omitempty"`
		SecurityIn   *s3Security   `json:"security,omitempty"`
		Encryption   *s3Encryption `json:"encryption,omitempty"`
		Advanced     *s3Advanced   `json:"advancedBucketConnection,omitempty"`
		AdvancedIn   *s3Advanced   `json:"advancedConnection,omitempty"`
		Failover     []s3Failover  `json:"failoverBuckets,omitempty"`
		ActiveRegion string        `json:"activeRegion,omitempty"`
		PreSigned    *bool         `json:"preSignedUrlEnabled,omitempty"`
	} `json:"bucketConfiguration"`
}

type s3Security struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey,omitempty"`
	Role            string `json:"role,omitempty"`
	SessionToken    string `json:"sessionToken,omitempty"`
}

type s3Encryption struct {
	Type string `json:"encryptionType"` // s3ManagedEncryption | kmsManagedEncryption
	Key  string `json:"encryptionKey,omitempty"`
}

type s3Advanced struct {
	Endpoint       string `json:"endpoint,omitempty"`
	SignerType     string `json:"signerType,omitempty"`
	ForcePathStyle bool   `json:"forcePathStyle"`
	MaxConnections *int   `json:"maxConnectionPoolSize,omitempty"`
}

type s3Failover struct {
	Region     string `json:"region"`
	BucketName string `json:"bucketName"`
	Endpoint   string `json:"endpoint,omitempty"` // Command Center's
}

var sseNames = map[string]string{"AES256": "s3ManagedEncryption", "aws:kms": "kmsManagedEncryption"}

// config maps the body onto Command Center's S3 settings; a secret sent as
// the placeholder (or empty) is stored's. It returns what cannot be honoured.
func (in nexusS3) config(stored *blob.S3Config) (blob.S3Config, []string) {
	b := in.Bucket
	c := blob.S3Config{Bucket: b.Bucket.Name, Prefix: b.Bucket.Prefix, Region: b.Bucket.Region}
	if strings.EqualFold(c.Region, "DEFAULT") {
		c.Region = ""
	}
	var errs []string
	refuse := func(bad bool, msg string) {
		if bad {
			errs = append(errs, msg)
		}
	}
	if s := cmp.Or(b.Security, b.SecurityIn); s != nil {
		c.AccessKey, c.SecretKey = s.AccessKeyID, s.SecretAccessKey
		if (c.SecretKey == "" || c.SecretKey == secretPlaceholder) && stored != nil {
			c.SecretKey = stored.SecretKey
		}
		refuse(s.Role != "", "bucketConfiguration.bucketSecurity.role: assuming a role is not supported; use keys or the AWS_* environment")
		refuse(s.SessionToken != "" && s.SessionToken != secretPlaceholder, "bucketConfiguration.bucketSecurity.sessionToken: a stored session token is not supported; use the AWS_* environment")
	}
	if e := b.Encryption; e != nil && e.Type != "" && !strings.EqualFold(e.Type, "none") {
		c.Encryption = &blob.S3Encryption{Type: e.Type, KMSKeyID: e.Key}
	}
	if a := cmp.Or(b.Advanced, b.AdvancedIn); a != nil {
		c.Endpoint, c.PathStyle = a.Endpoint, a.ForcePathStyle
		refuse(a.SignerType != "" && a.SignerType != "DEFAULT" && a.SignerType != "AWSS3V4SignerType",
			"bucketConfiguration.advancedBucketConnection.signerType: requests are signed with AWS Signature Version 4 only")
		refuse(a.MaxConnections != nil, "bucketConfiguration.advancedBucketConnection.maxConnectionPoolSize: the connection pool is not set per store")
	}
	for _, f := range b.Failover {
		c.Failover = append(c.Failover, blob.S3Failover{Region: f.Region, Bucket: f.BucketName, Endpoint: f.Endpoint})
	}
	refuse(b.PreSigned != nil && *b.PreSigned, "bucketConfiguration.preSignedUrlEnabled: downloads are served by this server, not redirected to the bucket")
	return c, errs
}

// s3Norm is c in one spelling, for comparing a request with the stored row.
func s3Norm(c blob.S3Config) blob.S3Config {
	c.LegacyAccessKey, c.LegacySecretKey = "", ""
	c.Prefix = strings.Trim(c.Prefix, "/")
	if c.Encryption == nil && c.SSE != "" {
		c.Encryption = &blob.S3Encryption{Type: sseNames[c.SSE]}
	}
	c.SSE = ""
	if len(c.Failover) == 0 {
		c.Failover = nil
	}
	return c
}

func (h *Handler) createS3BlobStore(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var in nexusS3
	if !decodeSmall(w, r, &in) {
		return
	}
	c, errs := in.config(nil)
	if len(errs) > 0 {
		writeValidation(w, errs)
		return
	}
	status, body := h.call(r, http.MethodPost, "/-/api/blobstores", map[string]any{"name": in.Name, "type": meta.BlobStoreS3, "quota": in.SoftQuota.ui(), "s3": c})
	if status != http.StatusCreated {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) getS3BlobStore(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	bs, ok := h.storeOfType(w, r, r.PathValue("name"), meta.BlobStoreS3)
	if !ok {
		return
	}
	var c blob.S3Config
	if bs.S3 != nil {
		c = *bs.S3
	}
	var out nexusS3
	out.Name, out.Type, out.SoftQuota = bs.Name, "S3", softQuota(bs.Quota)
	b := &out.Bucket
	b.Bucket.Region, b.Bucket.Name, b.Bucket.Prefix = cmp.Or(c.Region, "DEFAULT"), c.Bucket, c.Prefix
	b.ActiveRegion = cmp.Or(c.Region, "us-east-1")
	if c.AccessKey != "" { // never the secret itself
		b.Security = &s3Security{AccessKeyID: c.AccessKey, SecretAccessKey: secretPlaceholder}
	}
	if e := s3Norm(c).Encryption; e != nil {
		b.Encryption = &s3Encryption{Type: e.Type, Key: e.KMSKeyID}
	}
	if c.Endpoint != "" || c.PathStyle {
		b.Advanced = &s3Advanced{Endpoint: c.Endpoint, ForcePathStyle: c.PathStyle}
	}
	for _, f := range c.Failover {
		b.Failover = append(b.Failover, s3Failover{Region: f.Region, BucketName: f.Bucket, Endpoint: f.Endpoint})
	}
	writeJSON(w, http.StatusOK, out)
}

// updateS3BlobStore changes the soft quota. The bucket settings cannot
// change here (nor in the UI): the same ones are accepted, others refused.
func (h *Handler) updateS3BlobStore(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var in nexusS3
	if !decodeSmall(w, r, &in) {
		return
	}
	name := r.PathValue("name")
	if in.Name != "" && !strings.EqualFold(in.Name, name) {
		writeValidation(w, []string{"name: a blob store cannot be renamed"})
		return
	}
	bs, ok := h.storeOfType(w, r, name, meta.BlobStoreS3)
	if !ok || bs.S3 == nil {
		return
	}
	c, errs := in.config(bs.S3)
	if err := c.Validate(); err == nil && len(errs) == 0 && !reflect.DeepEqual(s3Norm(c), s3Norm(*bs.S3)) {
		errs = append(errs, "bucketConfiguration: the bucket, its credentials, encryption and connection cannot be changed; "+
			"create another blob store for other settings (only softQuota changes)")
	} else if err != nil {
		errs = append(errs, "bucketConfiguration: "+err.Error())
	}
	if len(errs) > 0 {
		writeValidation(w, errs)
		return
	}
	status, body := h.call(r, http.MethodPut, "/-/api/blobstores/"+url.PathEscape(name), map[string]any{"quota": in.SoftQuota.ui()})
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- e-mail: /v1/email over /-/api/settings/smtp ----

type nexusEmail struct {
	Enabled            bool    `json:"enabled"`
	Host               string  `json:"host"`
	Port               *int    `json:"port"`
	Username           string  `json:"username"`
	Password           *string `json:"password"`
	FromAddress        string  `json:"fromAddress"`
	SubjectPrefix      *string `json:"subjectPrefix"`
	StartTLSEnabled    bool    `json:"startTlsEnabled"`
	StartTLSRequired   bool    `json:"startTlsRequired"`
	SSLOnConnect       bool    `json:"sslOnConnectEnabled"`
	SSLIdentityCheck   bool    `json:"sslServerIdentityCheckEnabled"`
	NexusTrustStoreUse bool    `json:"nexusTrustStoreEnabled"`
}

func (h *Handler) getEmail(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/settings/smtp", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var c meta.SMTP // the answer has no password
	json.Unmarshal(body, &c)
	tls := c.Security == meta.SMTPStartTLS || c.Security == meta.SMTPTLS
	writeJSON(w, http.StatusOK, nexusEmail{Enabled: c.Enabled, Host: c.Host, Port: &c.Port, Username: c.Username, FromAddress: c.From,
		StartTLSEnabled: c.Security == meta.SMTPStartTLS, StartTLSRequired: c.Security == meta.SMTPStartTLS, SSLOnConnect: c.Security == meta.SMTPTLS,
		// TLS always checks the server's identity and trusts the SSL trust store beside the system roots.
		SSLIdentityCheck: tls, NexusTrustStoreUse: tls})
}

// putEmail maps the TLS switches onto none, starttls (always required) or
// tls; the combinations without an equivalent are refused.
func (h *Handler) putEmail(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var in nexusEmail
	if !decodeSmall(w, r, &in) {
		return
	}
	security := meta.SMTPNone
	switch {
	case in.SSLOnConnect:
		security = meta.SMTPTLS
	case in.StartTLSEnabled || in.StartTLSRequired:
		security = meta.SMTPStartTLS
	}
	var errs []string
	refuse := func(bad bool, msg string) {
		if bad {
			errs = append(errs, msg)
		}
	}
	refuse(in.Port == nil, "port: port is required")
	refuse(in.SSLOnConnect && (in.StartTLSEnabled || in.StartTLSRequired), "sslOnConnectEnabled: choose TLS on connect or STARTTLS, not both")
	refuse(security == meta.SMTPStartTLS && in.StartTLSEnabled != in.StartTLSRequired,
		"startTlsRequired: STARTTLS never falls back to plain text here; set startTlsEnabled and startTlsRequired together")
	refuse(security != meta.SMTPNone && !in.SSLIdentityCheck, "sslServerIdentityCheckEnabled: the server's identity is always checked over TLS")
	refuse(in.SubjectPrefix != nil && *in.SubjectPrefix != "", "subjectPrefix: a subject prefix is not supported")
	if len(errs) > 0 {
		writeValidation(w, errs)
		return
	}
	req := map[string]any{"host": in.Host, "port": *in.Port, "security": security, "username": in.Username, "from": in.FromAddress, "enabled": in.Enabled}
	if in.Password != nil { // empty or absent keeps the stored password (re-entered when the server changes)
		req["password"] = *in.Password
	}
	h.putSMTP(w, r, req)
}

// deleteEmail resets the settings to none.
func (h *Handler) deleteEmail(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	h.putSMTP(w, r, map[string]any{"host": "", "port": 0, "security": meta.SMTPNone, "username": "", "from": "", "enabled": false, "clearPassword": true})
}

func (h *Handler) putSMTP(w http.ResponseWriter, r *http.Request, req map[string]any) {
	status, body := h.call(r, http.MethodPut, "/-/api/settings/smtp", req)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// verifyEmail sends a test message to the address in the body (plain, or a
// JSON string): 200 {success: true}, or 400 {success: false, reason}.
func (h *Handler) verifyEmail(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	b, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<10))
	to := strings.Trim(strings.TrimSpace(string(b)), `"`)
	status, body := h.call(r, http.MethodPost, "/-/api/settings/smtp/test", map[string]string{"to": to})
	switch status {
	case http.StatusOK:
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "reason": nil})
	case http.StatusUnauthorized, http.StatusForbidden:
		deny(w, p)
	default:
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(body, &e)
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "reason": cmp.Or(e.Error, http.StatusText(status))})
	}
}

// ---- metrics: /service/rest/metrics/*, and /service/metrics/* redirects ----

// metricsAllowed applies the rule of /-/metrics/prometheus: admins, or anyone
// when metricsAnonymous is on.
func (h *Handler) metricsAllowed(w http.ResponseWriter, r *http.Request) bool {
	p := principal(r)
	if p.Admin || h.Auth.Snapshot().Settings().MetricsAnonymous {
		return true
	}
	deny(w, p)
	return false
}

// metricsRedirect answers Nexus's old paths with 301, healthcheck to the
// status checks.
func (h *Handler) metricsRedirect(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/service/metrics/")
	target := "/service/rest/metrics/" + rest
	if rest == "healthcheck" {
		target = Prefix + "/status/check"
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

func (h *Handler) metricsPing(w http.ResponseWriter, r *http.Request) {
	if h.metricsAllowed(w, r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "pong")
	}
}

// metricsPrometheus is /-/metrics/prometheus, the families narrowed to the
// name[] parameters when there are any.
func (h *Handler) metricsPrometheus(w http.ResponseWriter, r *http.Request) {
	if !h.metricsAllowed(w, r) {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/metrics/prometheus", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	if names := r.URL.Query()["name[]"]; len(names) > 0 {
		body = promFilter(body, names)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write(body)
}

// promFilter keeps the lines of the named metrics (HELP, TYPE and samples).
func promFilter(text []byte, names []string) []byte {
	var out bytes.Buffer
	for _, line := range bytes.SplitAfter(text, []byte("\n")) {
		f := strings.Fields(string(line))
		name := ""
		switch {
		case len(f) >= 3 && f[0] == "#":
			name = f[2]
		case len(f) > 0:
			name, _, _ = strings.Cut(f[0], "{")
		}
		if name != "" && slices.Contains(names, name) {
			out.Write(line)
		}
	}
	return out.Bytes()
}

// metricsData is Nexus's metrics JSON (the Dropwizard registry's shape):
// the Go runtime's figures under the JVM names that mean the same, and
// Command Center's own.
func (h *Handler) metricsData(w http.ResponseWriter, r *http.Request) {
	if !h.metricsAllowed(w, r) {
		return
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	gauge := func(v any) map[string]any { return map[string]any{"value": v} }
	out := map[string]any{"version": "4.0.0", "counters": map[string]any{}, "histograms": map[string]any{}, "meters": map[string]any{}, "timers": map[string]any{},
		"gauges": map[string]any{
			"jvm.memory.heap.used":        gauge(ms.HeapAlloc),
			"jvm.memory.heap.committed":   gauge(ms.HeapSys),
			"jvm.thread-states.count":     gauge(runtime.NumGoroutine()),
			"commandcenter.memory.sys":    gauge(ms.Sys),
			"commandcenter.memory.rss":    gauge(sysinfo.RSSBytes()),
			"commandcenter.gc.count":      gauge(ms.NumGC),
			"commandcenter.goroutines":    gauge(runtime.NumGoroutine()),
			"commandcenter.cpu.available": gauge(runtime.NumCPU()),
		}}
	if r.URL.Query().Get("download") == "true" {
		w.Header().Set("Content-Disposition", `attachment; filename="metrics.json"`)
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	if r.URL.Query().Get("pretty") == "true" {
		enc.SetIndent("", "  ")
	}
	enc.Encode(out)
}

// metricsThreads is the support thread dump (goroutines), admin only.
func (h *Handler) metricsThreads(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/support/threads", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	if r.URL.Query().Get("download") == "true" {
		w.Header().Set("Content-Disposition", `attachment; filename="threads.txt"`)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(body)
}

// ---- /service/rest/beta: Nexus's deprecated twins of v1 resources ----

// BetaPrefix is where Nexus serves the deprecated Beta resources.
const BetaPrefix = "/service/rest/beta"

// betaTwins are the first segments of the v1 resources with a Beta twin.
var betaTwins = map[string]bool{"repositories": true, "security": true, "routing-rules": true, "email": true, "blobstores": true}

// betaPath is the v1 path of the Beta path p, or false.
func betaPath(method, p string) (string, bool) {
	rest := strings.TrimPrefix(p, BetaPrefix+"/")
	first, _, _ := strings.Cut(rest, "/")
	switch {
	case !betaTwins[first]:
		return "", false
	case rest == "repositories" && method == http.MethodGet: // Beta's list is the settings
		return Prefix + "/repositorySettings", true
	}
	for _, typ := range []string{"file", "s3"} { // Beta's typed stores repeat the type
		twice := "blobstores/" + typ + "/" + typ
		if after, ok := strings.CutPrefix(rest, twice); ok && (after == "" || after[0] == '/') {
			rest = "blobstores/" + typ + after
		}
	}
	return Prefix + "/" + rest, true
}

// betaRequest is r on the v1 path, or nil for a path without a twin.
func betaRequest(r *http.Request) *http.Request {
	p, ok := betaPath(r.Method, r.URL.Path)
	if !ok {
		return nil
	}
	r2 := new(http.Request)
	*r2 = *r
	r2.URL = new(url.URL)
	*r2.URL = *r.URL
	r2.URL.Path = p
	if r.URL.RawPath != "" {
		r2.URL.RawPath, _ = betaPath(r.Method, r.URL.RawPath)
	}
	return r2
}

// ---- system information sections Nexus has beyond the UI's ----

// systemNetwork is the interfaces and their addresses (Nexus's
// system-network section).
func systemNetwork() map[string]any {
	out := map[string]any{}
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		var addrs []string
		if as, err := i.Addrs(); err == nil {
			for _, a := range as {
				addrs = append(addrs, a.String())
			}
		}
		out[i.Name] = map[string]any{"name": i.Name, "displayName": i.Name, "up": i.Flags&net.FlagUp != 0, "loopback": i.Flags&net.FlagLoopback != 0,
			"pointToPoint": i.Flags&net.FlagPointToPoint != 0, "supportsMulticast": i.Flags&net.FlagMulticast != 0, "mtu": i.MTU,
			"addresses": strings.Join(addrs, ", ")}
	}
	return out
}

// systemProperties is the Go build and runtime settings (the JVM's system
// properties' counterpart): no secrets, as builds carry none.
func systemProperties() map[string]any {
	out := map[string]any{"go.version": runtime.Version(), "os.name": runtime.GOOS, "os.arch": runtime.GOARCH}
	if bi, ok := debug.ReadBuildInfo(); ok {
		out["main.path"], out["main.version"] = bi.Main.Path, bi.Main.Version
		for _, s := range bi.Settings {
			if s.Key != "-ldflags" { // may carry anything a build passes
				out["build."+s.Key] = s.Value
			}
		}
	}
	return out
}
