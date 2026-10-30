package blob

import (
	"bytes"
	"cmp"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AzureConfig describes an Azure Blob Storage container that holds a store's
// blobs at <Prefix>/<sha256>, and its meta objects at <Prefix>/meta/<key>.
// The fields are the ones the blob-store API takes; AccountKey, SASToken and
// ClientSecret are sealed in the database like the other stored secrets.
type AzureConfig struct {
	AccountName   string `json:"accountName"`
	ContainerName string `json:"containerName"`      // an existing container
	Prefix        string `json:"prefix,omitempty"`   // blob name prefix, no leading or trailing slash; empty: the container root
	Endpoint      string `json:"endpoint,omitempty"` // https://<accountName>.blob.core.windows.net when empty; Azurite, sovereign clouds, private endpoints: theirs
	// Auth is how requests are authorized, one of the Azure* methods
	// (AzureAccountKey when empty). Validate drops the secrets the method
	// does not use.
	Auth          string `json:"authenticationMethod"`
	AccountKey    string `json:"accountKey,omitempty"`    // AzureAccountKey: the account's base64 access key (Shared Key)
	SASToken      string `json:"sasToken,omitempty"`      // AzureSASToken: the query string of a shared access signature
	TenantID      string `json:"tenantId,omitempty"`      // AzureClientSecret; AzureWorkloadIdentity: AZURE_TENANT_ID when empty
	ClientID      string `json:"clientId,omitempty"`      // AzureClientSecret; AzureWorkloadIdentity: AZURE_CLIENT_ID when empty; AzureManagedIdentity: a user-assigned identity
	ClientSecret  string `json:"clientSecret,omitempty"`  // AzureClientSecret
	AuthorityHost string `json:"authorityHost,omitempty"` // Microsoft Entra ID; AZURE_AUTHORITY_HOST or https://login.microsoftonline.com when empty
	IMDSEndpoint  string `json:"imdsEndpoint,omitempty"`  // AzureManagedIdentity's token endpoint; the instance metadata service when empty
}

// Azure authentication methods.
const (
	AzureAccountKey       = "accountKey"       // Shared Key with the account's access key
	AzureSASToken         = "sasToken"         // a shared access signature on every request
	AzureClientSecret     = "clientSecret"     // Microsoft Entra ID client credentials
	AzureManagedIdentity  = "managedIdentity"  // the VM's or node's managed identity (IMDS)
	AzureWorkloadIdentity = "workloadIdentity" // Kubernetes workload identity: AZURE_FEDERATED_TOKEN_FILE
)

// AzureMethods lists the authentication methods.
var AzureMethods = []string{AzureAccountKey, AzureSASToken, AzureClientSecret, AzureManagedIdentity, AzureWorkloadIdentity}

var (
	azureAccountRE   = regexp.MustCompile(`^[a-z0-9]{3,24}$`)
	azureContainerRE = regexp.MustCompile(`^[a-z0-9](-?[a-z0-9])+$`)
	azureIDRE        = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

// Validate reports the first thing wrong with c, after trimming the slashes
// of its prefix, defaulting the method and dropping the secrets the method
// does not use. It does not touch the container.
func (c *AzureConfig) Validate() error {
	c.Prefix = strings.Trim(c.Prefix, "/")
	c.Auth = cmp.Or(c.Auth, AzureAccountKey)
	switch {
	case !azureAccountRE.MatchString(c.AccountName):
		return errors.New("accountName must be 3 to 24 lower-case letters and digits")
	case len(c.ContainerName) < 3 || len(c.ContainerName) > 63 || !azureContainerRE.MatchString(c.ContainerName):
		return errors.New("containerName must be 3 to 63 lower-case letters, digits and single hyphens, starting and ending with a letter or digit")
	case c.Prefix != "" && !s3PrefixRE.MatchString(c.Prefix):
		return errors.New("prefix may hold letters, digits, slashes, dots, underscores and hyphens")
	case c.TenantID != "" && !azureIDRE.MatchString(c.TenantID):
		return errors.New("tenantId must be a directory (tenant) id or domain")
	case c.ClientID != "" && !azureIDRE.MatchString(c.ClientID):
		return errors.New("clientId must be an application (client) id")
	}
	for _, ep := range [][2]string{{"endpoint", c.Endpoint}, {"authorityHost", c.AuthorityHost}, {"imdsEndpoint", c.IMDSEndpoint}} {
		if err := validEndpoint(ep[1]); err != nil {
			return errors.New(ep[0] + " " + err.Error())
		}
	}
	switch c.Auth {
	case AzureAccountKey:
		if k, err := base64.StdEncoding.DecodeString(c.AccountKey); err != nil || len(k) == 0 {
			return errors.New("accountKey must be the storage account's base64 access key")
		}
	case AzureSASToken:
		c.SASToken = strings.TrimPrefix(c.SASToken, "?")
		if q, err := url.ParseQuery(c.SASToken); err != nil || q.Get("sig") == "" {
			return errors.New("sasToken must be a shared access signature query string (with sig=)")
		}
	case AzureClientSecret:
		if c.TenantID == "" || c.ClientID == "" || c.ClientSecret == "" {
			return errors.New("clientSecret authentication needs tenantId, clientId and clientSecret")
		}
	case AzureManagedIdentity, AzureWorkloadIdentity:
	default:
		return errors.New("authenticationMethod must be one of " + strings.Join(AzureMethods, ", "))
	}
	if c.Auth != AzureAccountKey {
		c.AccountKey = ""
	}
	if c.Auth != AzureSASToken {
		c.SASToken = ""
	}
	if c.Auth != AzureClientSecret {
		c.ClientSecret = ""
	}
	return nil
}

// endpointURL is the account's blob endpoint in one spelling (S3Config's
// normalization: scheme and host lower-cased, a default port and a trailing
// slash dropped). c must have passed Validate.
func (c AzureConfig) endpointURL() *url.URL {
	return S3Config{Endpoint: cmp.Or(c.Endpoint, "https://"+c.AccountName+".blob.core.windows.net")}.endpointURL()
}

// Location names the blob namespace c targets, endpoint/container/prefix
// with the endpoint in one spelling, so two configurations of one container
// and prefix compare equal however the endpoint is written.
func (c AzureConfig) Location() string {
	return c.endpointURL().String() + "/" + c.ContainerName + "/" + strings.Trim(c.Prefix, "/")
}

// azureVersion is the Blob service REST version every request asks for.
const azureVersion = "2025-05-05"

// azureBlockSize is the largest blob sent as one Put Blob, and the size of
// the blocks a larger one goes up in (Put Block, then Put Block List); a
// test lowers it.
var azureBlockSize int64 = 64 << 20

// azureMaxBlocks is the most blocks one block blob commits; a blob over
// azureMaxBlocks*azureBlockSize goes up in larger blocks (at most 4000 MiB
// each, so up to the service's 190.7 TiB).
const azureMaxBlocks = 50000

// azureListMax is the most names one List Blobs page asks for (a test lowers it).
var azureListMax = 5000

// azureRetries is how often a request that got no answer, 429 or 5xx is
// sent again, after azureBackoff, then twice that, and so on (a test
// lowers azureBackoff). Every request the backend sends is idempotent.
const azureRetries = 3

var azureBackoff = 500 * time.Millisecond

// azure is the backend for one container and prefix.
type azure struct {
	cfg    AzureConfig // the location; SetAzure changes only the credentials
	base   *url.URL    // endpoint/container
	auth   atomic.Pointer[azureAuth]
	client *http.Client
	now    func() time.Time
}

// azureAuth is one set of credentials: a Shared Key, a SAS or a token source.
type azureAuth struct {
	cfg AzureConfig
	key []byte
	sas url.Values
	tok *azureToken
}

// OpenAzure opens a store whose blobs live in the container cfg describes;
// root/tmp holds its uploads in progress and Docker sessions, as for Open.
func OpenAzure(root string, cfg AzureConfig) (*Store, error) {
	be, err := newAzure(cfg)
	if err != nil {
		return nil, err
	}
	return open(root, be)
}

// CheckAzure confirms that the container of cfg lists with its credentials,
// without opening a store.
func CheckAzure(cfg AzureConfig) error {
	be, err := newAzure(cfg)
	if err != nil {
		return err
	}
	defer be.client.CloseIdleConnections()
	return be.check()
}

// Azure returns an Azure store's configuration; ok is false for any other.
func (s *Store) Azure() (cfg AzureConfig, ok bool) {
	be, ok := s.be.(*azure)
	if !ok {
		return AzureConfig{}, false
	}
	return be.auth.Load().cfg, true
}

// SetAzure replaces an Azure store's credentials (a rotated key, a new SAS,
// another method); its account, container, prefix and endpoint stay.
// Requests already sent finish with the old ones.
func (s *Store) SetAzure(cfg AzureConfig) error {
	be, ok := s.be.(*azure)
	if !ok {
		return errors.New("not an azure blob store")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.Location() != be.cfg.Location() {
		return errors.New("an azure store's account, container, prefix and endpoint cannot change")
	}
	auth, err := be.newAuth(cfg)
	if err != nil {
		return err
	}
	be.auth.Store(auth)
	return nil
}

func newAzure(cfg AzureConfig) (*azure, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	a := &azure{cfg: cfg, base: cfg.endpointURL(), now: time.Now}
	a.base.Path += "/" + cfg.ContainerName
	auth, err := a.newAuth(cfg)
	if err != nil {
		return nil, err
	}
	a.auth.Store(auth)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.DisableCompression = true // a gateway that gzips would hide the blob's size
	a.client = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return a, nil
}

// newAuth builds the credentials of cfg, which passed Validate.
func (a *azure) newAuth(cfg AzureConfig) (*azureAuth, error) {
	auth := &azureAuth{cfg: cfg}
	switch cfg.Auth {
	case AzureAccountKey:
		auth.key, _ = base64.StdEncoding.DecodeString(cfg.AccountKey)
	case AzureSASToken:
		auth.sas, _ = url.ParseQuery(cfg.SASToken)
	default:
		t, err := newAzureToken(cfg, func() time.Time { return a.now() })
		if err != nil {
			return nil, err
		}
		auth.tok = t
	}
	return auth, nil
}

// name is the blob name of sha, or with "" the prefix every name starts with.
func (a *azure) name(sha string) string {
	if a.cfg.Prefix == "" {
		return sha
	}
	return a.cfg.Prefix + "/" + sha
}

// request builds an authorized request for blob name ("" is the container)
// with query q, headers hdr and a body of the given size.
func (a *azure) request(method, name string, q url.Values, hdr map[string]string, body io.Reader, size int64) (*http.Request, error) {
	auth := a.auth.Load()
	u := *a.base
	if name != "" {
		u.Path += "/" + name
	}
	all := url.Values{}
	maps.Copy(all, q)
	maps.Copy(all, auth.sas)
	u.RawQuery = all.Encode()
	if size == 0 {
		body = http.NoBody
	}
	r, err := http.NewRequest(method, u.String(), body)
	if err != nil {
		return nil, err
	}
	r.ContentLength = size
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r.Header.Set("x-ms-version", azureVersion)
	r.Header.Set("x-ms-date", a.now().UTC().Format(http.TimeFormat))
	switch {
	case auth.key != nil:
		SignSharedKey(r, a.cfg.AccountName, auth.key)
	case auth.tok != nil:
		tok, err := auth.tok.get()
		if err != nil {
			return nil, err
		}
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return r, nil
}

// do sends the request build makes, again while it fails unavailable (no
// answer, 429, 5xx), and returns the response when its status is one of ok.
// A 404 is ErrNotFound.
func (a *azure) do(build func() (*http.Request, error), ok ...int) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		r, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := a.send(r, ok...)
		if attempt == azureRetries || !isUnavailable(err) {
			return resp, err
		}
		time.Sleep(azureBackoff << attempt)
	}
}

func (a *azure) send(r *http.Request, ok ...int) (*http.Response, error) {
	resp, err := a.client.Do(r)
	if err != nil {
		return nil, unavailable{fmt.Errorf("azure %s %s: %w", r.Method, r.URL.Path, err)}
	}
	if slices.Contains(ok, resp.StatusCode) {
		return resp, nil
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	err = fmt.Errorf("azure %s %s: %s: %s %s", r.Method, r.URL.Path, resp.Status, resp.Header.Get("x-ms-error-code"), strings.TrimSpace(string(msg)))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, unavailable{err}
	}
	return nil, err
}

// put uploads the spooled file: one Put Blob up to azureBlockSize, blocks
// above. An existing blob is replaced, which refreshes its Last-Modified for
// GC's age guard the way the directory backend touches a file.
func (a *azure) put(tmp string, sums Sums) error {
	defer os.Remove(tmp)
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	sum, _ := hex.DecodeString(sums.MD5)
	if sums.Size <= azureBlockSize {
		return a.putBlob(a.name(sums.SHA256), func() io.Reader { return io.NewSectionReader(f, 0, sums.Size) }, sums.Size, sum)
	}
	return a.putBlocks(f, a.name(sums.SHA256), sums.Size, sum)
}

// putBlob is one Put Blob of the body body returns (again for a retry); the
// service checks it against its MD5.
func (a *azure) putBlob(name string, body func() io.Reader, size int64, sum []byte) error {
	hdr := map[string]string{"x-ms-blob-type": "BlockBlob", "Content-MD5": base64.StdEncoding.EncodeToString(sum)}
	resp, err := a.do(func() (*http.Request, error) { return a.request(http.MethodPut, name, nil, hdr, body(), size) }, http.StatusCreated)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// putBlocks uploads f in blocks, each checked against its MD5, and commits
// them; the blocks of an upload that fails are never committed and expire
// on their own (after a week).
func (a *azure) putBlocks(f *os.File, name string, size int64, sum []byte) error {
	bs := max(azureBlockSize, (size+azureMaxBlocks-1)/azureMaxBlocks)
	var list strings.Builder
	list.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
	for i, off := 0, int64(0); off < size; i, off = i+1, off+bs {
		n := min(bs, size-off)
		h := md5.New()
		if _, err := io.Copy(h, io.NewSectionReader(f, off, n)); err != nil {
			return err
		}
		id := base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "%06d", i)) // every id of a blob has the same length
		q := url.Values{"comp": {"block"}, "blockid": {id}}
		hdr := map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(h.Sum(nil))}
		resp, err := a.do(func() (*http.Request, error) {
			return a.request(http.MethodPut, name, q, hdr, io.NewSectionReader(f, off, n), n)
		}, http.StatusCreated)
		if err != nil {
			return err
		}
		resp.Body.Close()
		list.WriteString("<Latest>" + id + "</Latest>")
	}
	list.WriteString("</BlockList>")
	body := list.String()
	hdr := map[string]string{"x-ms-blob-content-md5": base64.StdEncoding.EncodeToString(sum)}
	resp, err := a.do(func() (*http.Request, error) {
		return a.request(http.MethodPut, name, url.Values{"comp": {"blocklist"}}, hdr, strings.NewReader(body), int64(len(body)))
	}, http.StatusCreated)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// get starts a Get Blob of name from offset from, to offset to inclusive
// (-1: the end), and returns the body and the blob's total size.
func (a *azure) get(name string, from, to int64) (io.ReadCloser, int64, error) {
	var hdr map[string]string
	if from > 0 || to >= 0 {
		rng := "bytes=" + strconv.FormatInt(from, 10) + "-"
		if to >= 0 {
			rng += strconv.FormatInt(to, 10)
		}
		hdr = map[string]string{"x-ms-range": rng}
	}
	resp, err := a.do(func() (*http.Request, error) { return a.request(http.MethodGet, name, nil, hdr, nil, 0) }, http.StatusOK, http.StatusPartialContent)
	if err != nil {
		return nil, 0, err
	}
	size := resp.ContentLength
	switch {
	case resp.StatusCode == http.StatusPartialContent: // Content-Range: bytes from-to/size
		_, total, ok := strings.Cut(resp.Header.Get("Content-Range"), "/")
		if size, err = strconv.ParseInt(total, 10, 64); !ok || err != nil {
			resp.Body.Close()
			return nil, 0, fmt.Errorf("azure GET %s: bad Content-Range %q", name, resp.Header.Get("Content-Range"))
		}
	case size < 0:
		resp.Body.Close()
		return nil, 0, fmt.Errorf("azure GET %s: no Content-Length", name)
	case from > 0:
		resp.Body.Close()
		return nil, 0, fmt.Errorf("azure GET %s: the server ignored the range", name)
	}
	return resp.Body, size, nil
}

// ranger reads a range of a stored object (see s3.get): what s3File needs.
type ranger interface {
	get(key string, from, to int64) (io.ReadCloser, int64, error)
}

// open reads with the S3 backend's file: one stream for sequential reads, a
// ranged Get Blob after a Seek and for each ReadAt.
func (a *azure) open(sha string) (File, error) {
	body, size, err := a.get(a.name(sha), 0, -1)
	if err != nil {
		return nil, err
	}
	return &s3File{s: a, key: a.name(sha), size: size, body: body}, nil
}

func (a *azure) stat(sha string) (Info, error) {
	resp, err := a.do(func() (*http.Request, error) { return a.request(http.MethodHead, a.name(sha), nil, nil, nil, 0) }, http.StatusOK)
	if err != nil {
		return Info{}, err
	}
	resp.Body.Close()
	if resp.ContentLength < 0 {
		return Info{}, fmt.Errorf("azure HEAD %s: no Content-Length", sha)
	}
	mod, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	return Info{Size: resp.ContentLength, ModTime: mod}, nil
}

func (a *azure) remove(sha string) error { return a.del(a.name(sha)) }

// del deletes blob name; a missing one is not an error.
func (a *azure) del(name string) error {
	resp, err := a.do(func() (*http.Request, error) { return a.request(http.MethodDelete, name, nil, nil, nil, 0) }, http.StatusAccepted)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// azureListing is one List Blobs page.
type azureListing struct {
	Blobs []struct {
		Name       string `xml:"Name"`
		Properties struct {
			LastModified  string `xml:"Last-Modified"`
			ContentLength int64  `xml:"Content-Length"`
		} `xml:"Properties"`
	} `xml:"Blobs>Blob"`
	NextMarker string `xml:"NextMarker"`
}

// listPage is one List Blobs page of the names under prefix; with delim,
// a name holding it after the prefix is rolled up and not listed.
func (a *azure) listPage(prefix, delim, marker string, max int) (*azureListing, error) {
	q := url.Values{"restype": {"container"}, "comp": {"list"}, "maxresults": {strconv.Itoa(max)}}
	for k, v := range map[string]string{"prefix": prefix, "delimiter": delim, "marker": marker} {
		if v != "" {
			q.Set(k, v)
		}
	}
	resp, err := a.do(func() (*http.Request, error) { return a.request(http.MethodGet, "", q, nil, nil, 0) }, http.StatusOK)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var page azureListing
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&page); err != nil {
		return nil, fmt.Errorf("azure list %s: decode response: %w", a.cfg.ContainerName, err)
	}
	return &page, nil
}

// list calls fn for every name under prefix, page by page, in lexical order.
func (a *azure) list(prefix, delim string, fn func(name string, size int64, mod time.Time) error) error {
	marker := ""
	for {
		page, err := a.listPage(prefix, delim, marker, azureListMax)
		if err != nil {
			return err
		}
		for _, b := range page.Blobs {
			mod, _ := http.ParseTime(b.Properties.LastModified)
			if err := fn(b.Name, b.Properties.ContentLength, mod); err != nil {
				return err
			}
		}
		if page.NextMarker == "" {
			return nil
		}
		marker = page.NextMarker
	}
}

// walk lists the names directly under the prefix: the meta objects, under
// <prefix>/meta/, roll up into a prefix and are never reported.
func (a *azure) walk(fn func(sha string, size int64, modTime time.Time) error) error {
	p := a.name("")
	return a.list(p, "/", func(name string, size int64, mod time.Time) error {
		if sha := strings.TrimPrefix(name, p); validSHA(sha) {
			return fn(sha, size, mod)
		}
		return nil
	})
}

// check lists one name: Get Container Properties is refused to a
// container-scoped SAS, which may still list.
func (a *azure) check() error {
	_, err := a.listPage(a.name(""), "/", "", 1)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("container %q not found in account %q: %v", a.cfg.ContainerName, a.cfg.AccountName, err)
	}
	return err
}

// Meta objects: <prefix>/meta/<key>.

func (a *azure) putMeta(key string, data []byte) error {
	sum := md5.Sum(data)
	return a.putBlob(a.name("meta/"+key), func() io.Reader { return bytes.NewReader(data) }, int64(len(data)), sum[:])
}

func (a *azure) getMeta(key string) ([]byte, error) {
	body, _, err := a.get(a.name("meta/"+key), 0, -1)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

func (a *azure) listMeta(prefix string, fn func(key string) error) error {
	p := a.name("meta/")
	return a.list(p+prefix, "", func(name string, _ int64, _ time.Time) error { return fn(strings.TrimPrefix(name, p)) })
}

func (a *azure) removeMeta(key string) error { return a.del(a.name("meta/" + key)) }

// SignSharedKey signs r for the Blob service with the storage account's
// key ("Authorize with Shared Key"): the Authorization header over the
// standard headers, every x-ms-* header and the canonicalized resource. r
// must already carry x-ms-date and x-ms-version.
func SignSharedKey(r *http.Request, account string, key []byte) {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(sharedKeyString(r, account)))
	r.Header.Set("Authorization", "SharedKey "+account+":"+base64.StdEncoding.EncodeToString(m.Sum(nil)))
}

// sharedKeyString is the string Shared Key signs (version 2009-09-19 and
// later, with Content-Length empty when zero as from 2015-02-21).
func sharedKeyString(r *http.Request, account string) string {
	h := r.Header
	length := ""
	if r.ContentLength > 0 {
		length = strconv.FormatInt(r.ContentLength, 10)
	}
	var b strings.Builder
	for _, v := range []string{r.Method, h.Get("Content-Encoding"), h.Get("Content-Language"), length, h.Get("Content-MD5"),
		h.Get("Content-Type"), h.Get("Date"), h.Get("If-Modified-Since"), h.Get("If-Match"), h.Get("If-None-Match"),
		h.Get("If-Unmodified-Since"), h.Get("Range")} {
		b.WriteString(v + "\n")
	}
	ms := map[string]string{}
	for k, v := range h {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-ms-") {
			ms[lk] = strings.Join(strings.Fields(strings.Join(v, ",")), " ") // trimmed, inner whitespace one space
		}
	}
	for _, k := range slices.Sorted(maps.Keys(ms)) {
		b.WriteString(k + ":" + ms[k] + "\n")
	}
	b.WriteString("/" + account + r.URL.EscapedPath())
	params := map[string][]string{}
	for k, vs := range r.URL.Query() {
		lk := strings.ToLower(k)
		params[lk] = append(params[lk], vs...)
	}
	for _, k := range slices.Sorted(maps.Keys(params)) {
		b.WriteString("\n" + k + ":" + strings.Join(slices.Sorted(slices.Values(params[k])), ","))
	}
	return b.String()
}

// azureScope is what a token is asked for: Azure Storage, in every cloud.
const azureScope = "https://storage.azure.com/.default"

// azureToken fetches Microsoft Entra ID access tokens for Azure Storage and
// keeps one until shortly before it expires.
type azureToken struct {
	build  func() (*http.Request, error) // the token request
	client *http.Client
	now    func() time.Time

	mu               sync.Mutex
	token            string
	refresh, expires time.Time
}

func newAzureToken(cfg AzureConfig, now func() time.Time) (*azureToken, error) {
	t := &azureToken{now: now, client: &http.Client{Timeout: 30 * time.Second}}
	authority := strings.TrimSuffix(cmp.Or(cfg.AuthorityHost, os.Getenv("AZURE_AUTHORITY_HOST"), "https://login.microsoftonline.com"), "/")
	form := func(tenant string, f func() (url.Values, error)) func() (*http.Request, error) {
		return func() (*http.Request, error) {
			v, err := f()
			if err != nil {
				return nil, err
			}
			r, err := http.NewRequest(http.MethodPost, authority+"/"+url.PathEscape(tenant)+"/oauth2/v2.0/token", strings.NewReader(v.Encode()))
			if err == nil {
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			return r, err
		}
	}
	switch cfg.Auth {
	case AzureClientSecret:
		v := url.Values{"grant_type": {"client_credentials"}, "client_id": {cfg.ClientID}, "client_secret": {cfg.ClientSecret}, "scope": {azureScope}}
		t.build = form(cfg.TenantID, func() (url.Values, error) { return v, nil })
	case AzureWorkloadIdentity:
		file := os.Getenv("AZURE_FEDERATED_TOKEN_FILE")
		tenant, client := cmp.Or(cfg.TenantID, os.Getenv("AZURE_TENANT_ID")), cmp.Or(cfg.ClientID, os.Getenv("AZURE_CLIENT_ID"))
		if file == "" || tenant == "" || client == "" {
			return nil, errors.New("workload identity needs AZURE_FEDERATED_TOKEN_FILE, and AZURE_TENANT_ID and AZURE_CLIENT_ID unless tenantId and clientId are set " +
				"(the AKS workload identity webhook sets all three)")
		}
		t.build = form(tenant, func() (url.Values, error) {
			jwt, err := os.ReadFile(file) // the kubelet rotates it: read it for every token
			if err != nil {
				return nil, fmt.Errorf("workload identity token file: %w", err)
			}
			return url.Values{"grant_type": {"client_credentials"}, "client_id": {client}, "scope": {azureScope},
				"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
				"client_assertion":      {strings.TrimSpace(string(jwt))}}, nil
		})
	case AzureManagedIdentity:
		q := url.Values{"api-version": {"2018-02-01"}, "resource": {"https://storage.azure.com/"}}
		if cfg.ClientID != "" {
			q.Set("client_id", cfg.ClientID)
		}
		ep := cmp.Or(cfg.IMDSEndpoint, "http://169.254.169.254/metadata/identity/oauth2/token") + "?" + q.Encode()
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil // the metadata service is link-local, never behind a proxy
		t.client.Transport = tr
		t.build = func() (*http.Request, error) {
			r, err := http.NewRequest(http.MethodGet, ep, nil)
			if err == nil {
				r.Header.Set("Metadata", "true")
			}
			return r, err
		}
	default:
		return nil, fmt.Errorf("authentication method %q takes no token", cfg.Auth)
	}
	return t, nil
}

// get returns the current token, fetching a new one from five minutes (or
// half its lifetime) before it expires. While a refresh fails the old token
// is used until it expires, and the refresh is tried again 30 seconds later
// (so a token endpoint that hangs does not hold up every request).
func (t *azureToken) get() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if t.token != "" && now.Before(t.refresh) {
		return t.token, nil
	}
	tok, life, err := t.fetch()
	if err != nil {
		if t.token != "" && now.Before(t.expires) {
			t.refresh = now.Add(30 * time.Second)
			if t.refresh.After(t.expires) {
				t.refresh = t.expires
			}
			return t.token, nil
		}
		return "", err
	}
	t.token, t.expires = tok, now.Add(life)
	t.refresh = t.expires.Add(-min(5*time.Minute, life/2))
	return tok, nil
}

func (t *azureToken) fetch() (string, time.Duration, error) {
	r, err := t.build()
	if err != nil {
		return "", 0, err
	}
	resp, err := t.client.Do(r)
	if err != nil {
		return "", 0, fmt.Errorf("azure token: %w", err)
	}
	defer resp.Body.Close()
	var v struct {
		AccessToken string      `json:"access_token"`
		ExpiresIn   json.Number `json:"expires_in"` // a number from Entra ID, a string from the metadata service
		Error       string      `json:"error"`
		Description string      `json:"error_description"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v)
	if resp.StatusCode != http.StatusOK || v.AccessToken == "" {
		return "", 0, fmt.Errorf("azure token from %s: %s: %s %s", r.URL.Host, resp.Status, v.Error, v.Description)
	}
	secs, _ := v.ExpiresIn.Int64()
	return v.AccessToken, max(time.Duration(secs)*time.Second, time.Minute), nil
}
