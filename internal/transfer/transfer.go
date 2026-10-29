// Package transfer exports repositories to a directory and imports such an
// export into a server: the repository export and import tasks of Nexus
// Repository Pro. An export is a portable, verifiable tree under the
// configured export root:
//
//	<destination>/<export id>/manifest.json
//	<destination>/<export id>/<repo>/assets.jsonl
//	<destination>/<export id>/<repo>/blobs/<sha256[0:2]>/<sha256>
//
// assets.jsonl holds one Line per file (its metadata, its package rows and
// their tags), in path order; blobs/ the content, hard-linked from a file
// blob store when it can be. manifest.json is written last: a directory
// without one is an export that did not finish.
//
// An import writes the rows and blobs through the store layer, not through
// the formats' upload protocols (which would lose timestamps and rows), one
// transaction per component under the locks an upload takes, verifying
// every blob's SHA-256; the formats' derived index documents are
// regenerated afterwards when the import was not a plain copy.
package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"commandcenter/internal/component"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// FormatVersion is the layout version of the exports this build writes and reads.
const FormatVersion = 1

// RootEnv names the export root; exportRoot in the config file gives it instead.
const RootEnv = "COMMANDCENTER_EXPORT_ROOT"

// ErrNoRoot refuses an export or import when no export root is configured.
var ErrNoRoot = errors.New("repository export and import need an export root: set exportRoot in the config file or " + RootEnv)

// Manifest is manifest.json.
type Manifest struct {
	FormatVersion        int         `json:"formatVersion"`
	CommandCenterVersion string      `json:"commandCenterVersion"`
	ID                   string      `json:"id"`
	Created              time.Time   `json:"created"`
	Since                time.Time   `json:"since,omitzero"` // a delta export: the files changed after it
	Repositories         []RepoEntry `json:"repositories"`
}

// RepoEntry is one exported repository.
type RepoEntry struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Type   string `json:"type"`
	// Config is the definition without its secrets (remote credentials, the
	// remote's userinfo and query, apt's signing key).
	Config          config.Repository `json:"config"`
	Assets          int64             `json:"assets"`
	Bytes           int64             `json:"bytes"`
	Packages        int64             `json:"packages"` // package rows
	AssetList       string            `json:"assetList"`
	AssetListSHA256 string            `json:"assetListSha256"`
}

// Line is one line of assets.jsonl: a stored file.
type Line struct {
	Path           string    `json:"path"`
	SHA256         string    `json:"sha256"`
	Size           int64     `json:"size"`
	ContentType    string    `json:"contentType,omitempty"`
	Created        time.Time `json:"created,omitzero"`
	LastUpdated    time.Time `json:"lastUpdated,omitzero"`
	LastDownloaded time.Time `json:"lastDownloaded,omitzero"`
	// Component labels the package version the file belongs to; an import
	// writes the adjacent lines of one component in one transaction.
	Component string       `json:"component,omitempty"`
	Attrs     AssetAttrs   `json:"attrs"`
	Packages  []PackageRow `json:"packages,omitempty"`
}

// AssetAttrs are the other columns of an asset.
type AssetAttrs struct {
	SHA1               string    `json:"sha1,omitempty"`
	MD5                string    `json:"md5,omitempty"`
	SHA512             string    `json:"sha512,omitempty"`
	RemoteETag         string    `json:"remoteETag,omitempty"`
	RemoteLastModified string    `json:"remoteLastModified,omitempty"`
	Checked            time.Time `json:"checked,omitzero"`
	Downloads          int64     `json:"downloads,omitempty"`
}

// PackageRow is a package row the file belongs to.
type PackageRow struct {
	Name    string    `json:"name"`
	Version string    `json:"version"`
	Attrs   string    `json:"attrs"` // the format's version document, as stored
	Yanked  bool      `json:"yanked,omitempty"`
	Created time.Time `json:"created,omitzero"`
	Tags    []string  `json:"tags,omitempty"` // npm dist-tags, docker tags naming this version
}

// Logger receives a run's progress (tasks.RunLog).
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Locker is implemented by the format handlers: LockPaths takes the locks
// an upload of paths to rp takes, on every node, and returns their unlock.
type Locker interface {
	LockPaths(ctx context.Context, rp *repo.Repository, paths []string) (unlock func(), err error)
}

// RebuildFunc regenerates the derived index documents of a hosted
// repository from its rows (repo.MetadataRebuilder's method).
type RebuildFunc func(ctx context.Context, rp *repo.Repository, logf func(string, ...any)) error

// Service exports and imports repositories.
type Service struct {
	DB      *meta.DB
	Store   *repo.Store
	Repos   *repo.Registry
	Root    string // the export root; "" disables export and import
	Version string // Command Center's, for the manifest
	Lockers map[string]Locker
	Rebuild map[string]RebuildFunc // by format; maven included
	Events  repo.EventSink         // repo.create and repo.import events; nil: none
	Now     func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) emit(kind, actor, repoName, subject, detail string) {
	if s.Events != nil {
		s.Events.Emit(kind, actor, repoName, subject, detail)
	}
}

// Result is what a run did.
type Result struct {
	Repos         int
	Assets, Bytes int64
	Failed        int64 // export: files not exported; import: components not imported
}

// rel turns dir, relative to the export root or absolute under it, into
// the relative path os.Root takes, refusing one that leaves the root.
// Symbolic links are refused by os.Root when the directory is opened.
func (s *Service) rel(dir string) (string, error) {
	if s.Root == "" {
		return "", ErrNoRoot
	}
	d := filepath.Clean(dir)
	if filepath.IsAbs(d) {
		r, err := filepath.Rel(filepath.Clean(s.Root), d)
		if err != nil {
			return "", fmt.Errorf("%q is not under the export root", dir)
		}
		d = r
	}
	if d != "." && !filepath.IsLocal(d) {
		return "", fmt.Errorf("%q is not under the export root", dir)
	}
	return d, nil
}

// CheckDir checks a destination or source directory of a task: under the
// export root and, when it exists, not leaving it through a symbolic link.
func (s *Service) CheckDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("the directory is required")
	}
	rel, err := s.rel(dir)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(s.Root)
	if err != nil {
		return fmt.Errorf("export root: %w", err)
	}
	defer root.Close()
	if _, err := root.Stat(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%q: %w", dir, err)
	}
	return nil
}

// strip removes the secrets from a definition before it goes into a manifest.
func strip(def config.Repository) config.Repository {
	if def.RemoteAuth != nil {
		a := *def.RemoteAuth
		a.Password, a.BearerToken = "", ""
		def.RemoteAuth = &a
	}
	def.SigningKey = ""
	if u, err := url.Parse(def.Remote); err == nil && (u.User != nil || u.RawQuery != "") {
		u.User, u.RawQuery = nil, ""
		def.Remote = u.String()
	}
	return def
}

// ---- what the formats store ----

// derivedFormats keep index documents derived from their rows or files,
// which a repository import regenerates with the format's rebuild.
var derivedFormats = map[string]bool{"maven": true, "npm": true, "helm": true, "yum": true, "apt": true, "conda": true, "r": true, "rubygems": true}

// derived reports whether p is such a document in a hosted repository.
func derived(format, p string) bool {
	base := path.Base(p)
	switch format {
	case "maven":
		return strings.HasPrefix(base, "maven-metadata.xml")
	case "npm":
		return base == "package.json" && !strings.Contains(p, "/-/")
	case "helm":
		return p == "index.yaml"
	case "yum":
		return strings.HasPrefix(p, "repodata/") || strings.Contains(p, "/repodata/")
	case "apt":
		return strings.HasPrefix(p, "dists/")
	case "conda":
		return p == "channeldata.json" || base == "repodata.json" || base == "current_repodata.json"
	case "r":
		return base == "PACKAGES" || base == "PACKAGES.gz" || base == "PACKAGES.rds"
	case "rubygems":
		return p == "versions"
	}
	return false
}

// componentOf labels the component a file belongs to ("" for none): an
// import keeps the adjacent files of one component in one transaction.
func componentOf(format, p string) string {
	if format == "docker" { // each manifest is a unit of its own; blobs are not components
		if strings.Contains(p, "/manifests/") {
			return p
		}
		return ""
	}
	g, n, v, ok := component.DeriveAsset(format, p)
	if !ok {
		return ""
	}
	return component.Component{Format: format, Group: g, Name: n, Version: v}.Label()
}

// rowName is the package name whose rows the file at p may belong to.
func rowName(format, p string) (string, bool) {
	switch format {
	case "docker":
		if i := strings.LastIndex(p, "/manifests/"); i > 0 {
			return p[:i], true
		}
		return "", false
	case "maven", "raw", "go", "gitlfs":
		return "", false
	}
	g, n, v, ok := component.DeriveAsset(format, p)
	switch {
	case !ok:
		return "", false
	case (format == "npm" || format == "r") && g != "":
		return g + "/" + n, true
	case format == "conan":
		ver, _, _ := strings.Cut(v, "#")
		if g != "" {
			return n + "/" + ver + "@" + g, true
		}
		return n + "/" + ver, true
	}
	return n, true
}

// rowPath is the path of the file a package row belongs to.
func rowPath(format string, p meta.Package) string {
	if format == "apt" { // where the upload put it, whatever the component setting is now
		var a struct {
			Filename string `json:"filename"`
		}
		json.Unmarshal([]byte(p.Attrs), &a)
		return a.Filename
	}
	_, _, _, file := component.PackageComponent(format, p.Name, p.Version)
	return file
}

// validPath refuses asset paths no upload could have stored.
func validPath(p string) bool {
	if p == "" || len(p) > 4096 || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func validSHA(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}

// blobPath is where a repository's blob lives in an export, relative to it.
func blobPath(repoName, sha string) string {
	return filepath.Join(repoName, "blobs", sha[:2], sha)
}
