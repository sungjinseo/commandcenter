// Package importer copies repository definitions and hosted content from a
// running Nexus Repository 3 server into Command Center (plan 15). It runs
// inside the server as the import.nexus task: the source client
// (internal/importer/nexus) streams each asset into the format ingesters,
// and per-asset state in the import_state table makes a stopped import
// resumable and a re-run idempotent.
package importer

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Limits of the parameters.
const (
	DefaultConcurrency = 4
	MaxConcurrency     = 16
	maxRepoFilter      = 1000
	maxPrefixLen       = 32
)

var (
	jobRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	prefixRE = regexp.MustCompile(`^[A-Za-z0-9._-]*$`)
	hostRE   = regexp.MustCompile(`[^A-Za-z0-9.-]+`)
)

// Params are the stored parameters of an import job. They never hold the
// source password: that is passed to Run as Credentials and lives in memory
// only for the duration of the run.
type Params struct {
	URL                string   `json:"url"`
	Username           string   `json:"username,omitempty"`
	InsecureSkipVerify bool     `json:"insecureSkipVerify,omitempty"`
	Repositories       []string `json:"repositories"` // source names; empty means all
	ImportDefinitions  bool     `json:"importDefinitions"`
	ImportContent      bool     `json:"importContent"`
	BlobStore          string   `json:"blobStore"`
	RenamePrefix       string   `json:"renamePrefix,omitempty"`
	Job                string   `json:"job"`
	Concurrency        int      `json:"concurrency"`
	DryRun             bool     `json:"dryRun,omitempty"`
	RetryFailed        bool     `json:"retryFailed,omitempty"`
}

// Credentials hold the source password for one run. They are never
// stored; their text and JSON forms are redacted so that a stray %v or
// marshal cannot leak them.
type Credentials struct {
	Password string
}

func (Credentials) String() string                  { return "[redacted]" }
func (Credentials) GoString() string                { return "[redacted]" }
func (Credentials) MarshalJSON() ([]byte, error)    { return []byte(`"[redacted]"`), nil }
func (c Credentials) Format(f fmt.State, verb rune) { fmt.Fprint(f, "[redacted]") }

// Normalize validates p and returns its canonical form: the URL without a
// trailing slash, the repository filter deduplicated, the blob store
// defaulted to "default", the concurrency to DefaultConcurrency and the job
// to <source-host>-<yyyymmdd> (UTC). hasStore, when set, checks that the blob
// store exists.
func (p Params) Normalize(now time.Time, hasStore func(string) bool) (Params, []string) {
	var errs []string
	p.URL = strings.TrimRight(strings.TrimSpace(p.URL), "/")
	u, err := url.Parse(p.URL)
	switch {
	case p.URL == "":
		errs = append(errs, "url is required")
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		errs = append(errs, "url must be an http or https URL of the source server")
	case u.User != nil:
		errs = append(errs, "url must not contain credentials: pass username and password separately")
	case u.RawQuery != "" || u.Fragment != "":
		errs = append(errs, "url must not have a query or fragment")
	}
	p.Username = strings.TrimSpace(p.Username)
	if len(p.Username) > 256 || strings.ContainsAny(p.Username, "\r\n\x00:") {
		errs = append(errs, "username is invalid")
	}
	if len(p.Repositories) > maxRepoFilter {
		errs = append(errs, fmt.Sprintf("repositories lists at most %d names", maxRepoFilter))
	}
	seen := map[string]bool{}
	repos := []string{}
	for _, r := range p.Repositories {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		if len(r) > 200 || strings.ContainsAny(r, "/\\\x00\r\n") {
			errs = append(errs, fmt.Sprintf("invalid repository name %q", clip(r)))
			continue
		}
		seen[r] = true
		repos = append(repos, r)
	}
	p.Repositories = repos
	if !p.ImportDefinitions && !p.ImportContent {
		errs = append(errs, "nothing to do: set importDefinitions, importContent or both")
	}
	if p.BlobStore = strings.TrimSpace(p.BlobStore); p.BlobStore == "" {
		p.BlobStore = "default"
	}
	if hasStore != nil && !hasStore(p.BlobStore) {
		errs = append(errs, fmt.Sprintf("blob store %q does not exist", clip(p.BlobStore)))
	}
	if len(p.RenamePrefix) > maxPrefixLen || !prefixRE.MatchString(p.RenamePrefix) {
		errs = append(errs, fmt.Sprintf("renamePrefix must be at most %d letters, digits, '.', '_' or '-'", maxPrefixLen))
	}
	switch {
	case p.Concurrency == 0:
		p.Concurrency = DefaultConcurrency
	case p.Concurrency < 1 || p.Concurrency > MaxConcurrency:
		errs = append(errs, fmt.Sprintf("concurrency must be between 1 and %d", MaxConcurrency))
	}
	p.Job = strings.TrimSpace(p.Job)
	if p.Job == "" && u != nil && u.Host != "" {
		host := strings.Trim(hostRE.ReplaceAllString(strings.ToLower(u.Host), "-"), "-.")
		if host == "" {
			host = "source"
		}
		if len(host) > 80 {
			host = host[:80]
		}
		p.Job = host + "-" + now.UTC().Format("20060102")
	}
	if p.Job != "" && !jobRE.MatchString(p.Job) {
		errs = append(errs, "job must be 1-100 letters, digits, '.', '_' or '-', starting with a letter or digit")
	}
	return p, errs
}

// ValidJob reports whether s can name a job.
func ValidJob(s string) bool { return jobRE.MatchString(s) }

func clip(s string) string {
	if r := []rune(s); len(r) > 60 {
		return string(r[:60])
	}
	return s
}

// Host returns the host[:port] of the source URL, for messages.
func (p Params) Host() string {
	if u, err := url.Parse(p.URL); err == nil {
		return u.Host
	}
	return ""
}
