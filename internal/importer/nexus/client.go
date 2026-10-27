// Package nexus is the read-only source client for importing from a Nexus
// Repository 3 server through its public REST API. It only issues GET
// requests, keeps credentials in memory and out of every error text, follows
// redirects only within the same scheme and host, and bounds every JSON
// response. Artifact bodies are streamed, never buffered.
package nexus

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"commandcenter/internal/logsafe"
)

// Defaults and caps.
const (
	DefaultTimeout         = 60 * time.Second // JSON calls and response headers
	DefaultDownloadTimeout = 30 * time.Minute // per asset
	MaxPageBytes           = 16 << 20         // one assets/repositories page
	MaxPackumentBytes      = 64 << 20
	MaxIndexBytes          = 16 << 20 // one cargo sparse index file
	maxRedirects           = 5
	maxPages               = 1_000_000
)

// Options configures a Client.
type Options struct {
	BaseURL            string // http(s)://host[:port][/context]
	Username, Password string // Basic auth; may be a user-token pair
	InsecureSkipVerify bool   // disable TLS verification (opt-in)
	Timeout            time.Duration
	DownloadTimeout    time.Duration
}

// Client talks to one Nexus server.
type Client struct {
	base            *url.URL // no trailing slash, no userinfo/query/fragment
	user, pass      string
	hc              *http.Client
	timeout         time.Duration
	downloadTimeout time.Duration
}

var errRedirectRefused = errors.New("redirect to another host refused")

// New validates opts and builds a Client.
func New(opts Options) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(opts.BaseURL))
	if err != nil {
		return nil, errors.New("nexus: invalid base URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("nexus: base URL must be http or https")
	}
	if u.Host == "" {
		return nil, errors.New("nexus: base URL has no host")
	}
	if u.User != nil {
		return nil, errors.New("nexus: base URL must not contain credentials; pass username and password separately")
	}
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""

	c := &Client{base: u, user: opts.Username, pass: opts.Password,
		timeout: opts.Timeout, downloadTimeout: opts.DownloadTimeout}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	if c.downloadTimeout <= 0 {
		c.downloadTimeout = DefaultDownloadTimeout
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: opts.InsecureSkipVerify} //nolint:gosec // explicit opt-in
	tr.ResponseHeaderTimeout = c.timeout
	c.hc = &http.Client{
		Transport: tr, // no overall Timeout: bodies are streamed
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			first := via[0].URL
			if !strings.EqualFold(req.URL.Scheme, first.Scheme) || !strings.EqualFold(req.URL.Host, first.Host) {
				return errRedirectRefused
			}
			return nil
		},
	}
	return c, nil
}

// API pages and the repositories list.

// ---- JSON shapes (Nexus Repository 3 REST: AssetXO, PageXO, RepositoryXO,
// and the repository settings XOs) ----

// Checksum is AssetXO.checksum (a map of algorithm to hex digest).
type Checksum struct {
	SHA1   string `json:"sha1"`
	SHA256 string `json:"sha256"`
	SHA512 string `json:"sha512"`
	MD5    string `json:"md5"`
}

// Asset is one entry of GET /service/rest/v1/assets.
type Asset struct {
	ID           string   `json:"id"`
	Path         string   `json:"path"`
	DownloadURL  string   `json:"downloadUrl"`
	Repository   string   `json:"repository"`
	Format       string   `json:"format"`
	Checksum     Checksum `json:"checksum"`
	ContentType  string   `json:"contentType"`
	LastModified string   `json:"lastModified"` // ISO-8601, see ModTime
	FileSize     int64    `json:"fileSize"`

	// Format attributes, present for the matching format.
	Maven2 *struct {
		GroupID    string `json:"groupId"`
		ArtifactID string `json:"artifactId"`
		Version    string `json:"version"`
		Extension  string `json:"extension"`
		Classifier string `json:"classifier"`
	} `json:"maven2"`
	Npm   *NameVersion `json:"npm"`
	Pypi  *NameVersion `json:"pypi"`
	Cargo *NameVersion `json:"cargo"`
}

// NameVersion is the npm, pypi or cargo attribute block of an asset; both
// fields are empty on metadata assets (a packument, an index file).
type NameVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ModTime parses LastModified; the zero time when absent or malformed.
func (a Asset) ModTime() time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000-07:00"} {
		if t, err := time.Parse(layout, a.LastModified); err == nil {
			return t
		}
	}
	return time.Time{}
}

type assetPage struct {
	Items             []Asset `json:"items"`
	ContinuationToken *string `json:"continuationToken"`
}

// Repository is one entry of GET /service/rest/v1/repositories.
type Repository struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Type   string `json:"type"` // hosted | proxy | group
	URL    string `json:"url"`
}

// RepoSettings is one entry of GET /service/rest/v1/repositorySettings (and
// the body of the per-repository GET). Only the fields the importer uses.
type RepoSettings struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Type   string `json:"type"`
	URL    string `json:"url"`
	Online bool   `json:"online"`

	Storage struct {
		BlobStoreName               string `json:"blobStoreName"`
		StrictContentTypeValidation bool   `json:"strictContentTypeValidation"`
		WritePolicy                 string `json:"writePolicy"` // ALLOW | ALLOW_ONCE | DENY (upper case)
	} `json:"storage"`
	Maven *struct {
		VersionPolicy string `json:"versionPolicy"` // RELEASE | SNAPSHOT | MIXED
		LayoutPolicy  string `json:"layoutPolicy"`
	} `json:"maven"`
	Proxy *struct {
		RemoteURL      string `json:"remoteUrl"`
		ContentMaxAge  int    `json:"contentMaxAge"`  // minutes; -1 never expires
		MetadataMaxAge int    `json:"metadataMaxAge"` // minutes; -1 never expires
	} `json:"proxy"`
	// NegativeCache.TimeToLive is in minutes.
	NegativeCache *struct {
		Enabled    bool `json:"enabled"`
		TimeToLive int  `json:"timeToLive"`
	} `json:"negativeCache"`
	// HTTPClient.Authentication is non-null when the proxy authenticates to
	// its remote. The API never returns the secret, so the importer only
	// reports that credentials must be entered again; never log the block.
	HTTPClient *struct {
		Authentication *json.RawMessage `json:"authentication"`
	} `json:"httpClient"`
	Group *struct {
		MemberNames []string `json:"memberNames"`
	} `json:"group"`
}

// Repositories lists every repository on the source.
func (c *Client) Repositories(ctx context.Context) ([]Repository, error) {
	var out []Repository
	if err := c.getJSON(ctx, c.apiURL("repositories", nil), MaxPageBytes, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RepositorySettings returns the configuration of every repository. It reads
// /v1/repositorySettings and falls back to
// /v1/repositories/{format}/{type}/{name} for a server without that endpoint
// and for any repository missing from it.
func (c *Client) RepositorySettings(ctx context.Context) ([]RepoSettings, error) {
	var all []RepoSettings
	err := c.getJSON(ctx, c.apiURL("repositorySettings", nil), MaxPageBytes, &all)
	if err != nil {
		if errors.Is(err, ErrAuth) || errors.Is(err, ErrNetwork) || errors.Is(err, ErrRedirect) || ctx.Err() != nil {
			return nil, err
		}
		all = nil // endpoint missing or unusable: per-repository fallback for all
	}
	repos, err := c.Repositories(ctx)
	if err != nil {
		if len(all) > 0 {
			return all, nil
		}
		return nil, err
	}
	have := make(map[string]bool, len(all))
	for _, s := range all {
		have[s.Name] = true
	}
	for _, r := range repos {
		if have[r.Name] {
			continue
		}
		s, err := c.repositorySettingsOne(ctx, r)
		if errors.Is(err, ErrNotFound) {
			// No per-repository endpoint (a format this server cannot
			// describe): the listing still names its format and type.
			s, err = RepoSettings{Name: r.Name, Format: r.Format, Type: r.Type, URL: r.URL}, nil
		}
		if err != nil {
			return nil, err
		}
		all = append(all, s)
	}
	return all, nil
}

func (c *Client) repositorySettingsOne(ctx context.Context, r Repository) (RepoSettings, error) {
	f := r.Format
	if f == "maven2" {
		f = "maven"
	}
	u := c.base.String() + "/service/rest/v1/repositories/" +
		url.PathEscape(f) + "/" + url.PathEscape(r.Type) + "/" + url.PathEscape(r.Name)
	var s RepoSettings
	if err := c.getJSON(ctx, u, MaxPageBytes, &s); err != nil {
		return s, err
	}
	// The per-repository body omits nothing we need except, on some versions,
	// the identity fields; fill them from the listing.
	if s.Name == "" {
		s.Name = r.Name
	}
	if s.Format == "" {
		s.Format = r.Format
	}
	if s.Type == "" {
		s.Type = r.Type
	}
	return s, nil
}

// Assets pages through GET /service/rest/v1/assets?repository=repo with
// continuationToken and calls fn for every asset. fn's error stops the walk.
func (c *Client) Assets(ctx context.Context, repo string, fn func(Asset) error) error {
	token := ""
	seen := map[string]bool{}
	for page := 0; page < maxPages; page++ {
		q := url.Values{"repository": {repo}}
		if token != "" {
			q.Set("continuationToken", token)
		}
		var p assetPage
		if err := c.getJSON(ctx, c.apiURL("assets", q), MaxPageBytes, &p); err != nil {
			return err
		}
		for _, a := range p.Items {
			// Nexus 3 paths start with "/" ("/com/ex/app/1.0/app-1.0.jar").
			a.Path = strings.TrimLeft(a.Path, "/")
			if err := fn(a); err != nil {
				return err
			}
		}
		if p.ContinuationToken == nil || *p.ContinuationToken == "" {
			return nil
		}
		token = *p.ContinuationToken
		if seen[token] {
			return &Error{Kind: ErrProtocol, Op: "list assets", URL: c.base.String(), Err: errors.New("continuation token repeated")}
		}
		seen[token] = true
	}
	return &Error{Kind: ErrProtocol, Op: "list assets", URL: c.base.String(), Err: errors.New("too many pages")}
}

func (c *Client) apiURL(resource string, q url.Values) string {
	u := c.base.String() + "/service/rest/v1/" + resource
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// ---- low-level GET ----

// get issues a GET with Basic auth and maps failures to *Error. The caller
// closes the body. cancel must be called by the caller when done.
func (c *Client) get(ctx context.Context, op, rawurl, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, &Error{Kind: ErrProtocol, Op: op, URL: rawurl, Err: errors.New("invalid request URL")}
	}
	if c.user != "" || c.pass != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if errors.Is(err, errRedirectRefused) {
			return nil, &Error{Kind: ErrRedirect, Op: op, URL: rawurl}
		}
		return nil, &Error{Kind: ErrNetwork, Op: op, URL: rawurl, Err: errors.New(logsafe.Err(err))}
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		resp.Body.Close()
		return nil, &Error{Kind: ErrAuth, Op: op, URL: rawurl, Status: resp.StatusCode}
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		return nil, &Error{Kind: ErrNotFound, Op: op, URL: rawurl, Status: resp.StatusCode}
	case resp.StatusCode != http.StatusOK:
		resp.Body.Close()
		return nil, &Error{Kind: ErrStatus, Op: op, URL: rawurl, Status: resp.StatusCode}
	}
	return resp, nil
}

// getBounded reads a whole response of at most limit bytes.
func (c *Client) getBounded(ctx context.Context, op, rawurl, accept string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.get(ctx, op, rawurl, accept)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > limit {
		return nil, &Error{Kind: ErrTooLarge, Op: op, URL: rawurl}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, &Error{Kind: ErrNetwork, Op: op, URL: rawurl, Err: errors.New(logsafe.Err(err))}
	}
	if int64(len(b)) > limit {
		return nil, &Error{Kind: ErrTooLarge, Op: op, URL: rawurl}
	}
	return b, nil
}

func (c *Client) getJSON(ctx context.Context, rawurl string, limit int64, v any) error {
	b, err := c.getBounded(ctx, "GET", rawurl, "application/json", limit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return &Error{Kind: ErrProtocol, Op: "GET", URL: rawurl, Err: errors.New("response is not the expected JSON")}
	}
	return nil
}

// ---- errors ----

// Error kinds, for errors.Is.
var (
	ErrAuth     = errors.New("authentication failed (HTTP 401/403): check the source username and password")
	ErrNotFound = errors.New("not found (HTTP 404)")
	ErrNetwork  = errors.New("network error")
	ErrStatus   = errors.New("unexpected HTTP status")
	ErrRedirect = errors.New("redirect to another host refused")
	ErrTooLarge = errors.New("response exceeds the size cap")
	ErrChecksum = errors.New("checksum mismatch")
	ErrProtocol = errors.New("unexpected response")
)

// Error describes one failed source request. Its text never contains
// credentials: the URL is passed through logsafe (no userinfo, redacted
// query values) and server bodies are not echoed.
type Error struct {
	Kind   error // one of the Err* values
	Op     string
	URL    string
	Status int
	Err    error // optional detail, already safe
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("nexus source: ")
	b.WriteString(e.Op)
	if e.URL != "" {
		b.WriteString(` "`)
		b.WriteString(logsafe.URL(e.URL))
		b.WriteString(`"`)
	}
	b.WriteString(": ")
	b.WriteString(e.Kind.Error())
	if e.Status != 0 && e.Kind == ErrStatus {
		fmt.Fprintf(&b, " %d", e.Status)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Kind }

func safeErr(err error) string { return logsafe.Err(err) }
