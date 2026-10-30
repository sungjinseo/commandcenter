package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Replication is a proxy's pull replication (Nexus "pre-emptive pull"):
// what is uploaded to the hosted repository its remote names, on another
// Nexus or Command Center instance, is fetched ahead of any request by the
// repository.pull-replication task. The source and its credentials are the
// proxy's remote and remoteAuth (help.sonatype.com, content replication),
// so nothing here is secret.
type Replication struct {
	PreemptivePullEnabled bool `yaml:"preemptivePullEnabled" json:"preemptivePullEnabled"`
	// AssetPathRegex (RE2, "" for every asset) limits replication to the
	// asset paths it matches, with or without their leading "/".
	AssetPathRegex string `yaml:"assetPathRegex,omitempty" json:"assetPathRegex,omitempty"`
	// VerifyChecksums drops a fetched asset whose checksum differs from the
	// one the source lists, and counts it as failed.
	VerifyChecksums bool `yaml:"verifyChecksums,omitempty" json:"verifyChecksums,omitempty"`
}

// ReplicationEnabled reports whether r is a proxy that replicates.
func (r Repository) ReplicationEnabled() bool {
	return r.Type == "proxy" && r.Replication != nil && r.Replication.PreemptivePullEnabled
}

// ReplicationFormats are the formats whose proxy fetches a hosted asset by
// the path the source's asset list gives (TestPullReplicationFormats).
var ReplicationFormats = map[string]bool{"maven": true, "npm": true, "go": true, "pypi": true, "cargo": true, "raw": true,
	"helm": true, "yum": true, "nuget": true, "apt": true, "composer": true, "conda": true, "rubygems": true, "r": true,
	"conan": true, "swift": true, "terraform": true}

// noReplication says why a format with proxies has no pull replication.
var noReplication = map[string]string{
	"docker":      "images are fetched by name and reference through the registry API, not by asset path",
	"p2":          "there are no p2 hosted repositories to replicate from",
	"cocoapods":   "there are no cocoapods hosted repositories to replicate from",
	"huggingface": "there are no huggingface hosted repositories to replicate from",
}

// MaxReplicationRegexLen bounds assetPathRegex.
const MaxReplicationRegexLen = 512

// ReplicationSource splits a remote that names a hosted repository,
// <base>/repository/<name>/, into the instance's base URL (no trailing
// "/", query or fragment) and the repository name.
func ReplicationSource(remote string) (base, name string, ok bool) {
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	i := strings.LastIndex(u.Path, "/repository/")
	if i < 0 {
		return "", "", false
	}
	name = strings.TrimSuffix(u.Path[i+len("/repository/"):], "/")
	if name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	u.Path, u.RawPath, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = u.Path[:i], "", "", false, "", ""
	return u.String(), name, true
}

// validateReplication checks r's replication: an enabled one needs a proxy
// of a format that has it, whose remote names a hosted repository; the
// regex must compile. A disabled, empty one is accepted anywhere (a form
// sends it).
func validateReplication(r Repository) error {
	p := r.Replication
	if p == nil || (!p.PreemptivePullEnabled && p.AssetPathRegex == "" && !p.VerifyChecksums) {
		return nil
	}
	if r.Type != "proxy" {
		return fmt.Errorf("repository %q: replication is only valid for proxy repositories", r.Name)
	}
	if p.AssetPathRegex != "" {
		if _, err := regexp.Compile(p.AssetPathRegex); err != nil || len(p.AssetPathRegex) > MaxReplicationRegexLen {
			return fmt.Errorf("repository %q: replication.assetPathRegex: %q is not a regular expression of at most %d characters", r.Name, p.AssetPathRegex, MaxReplicationRegexLen)
		}
	}
	if !p.PreemptivePullEnabled {
		return nil
	}
	if !ReplicationFormats[r.Format] {
		why := noReplication[r.Format]
		if why == "" {
			why = "its proxy cannot fetch an asset by its path alone"
		}
		return fmt.Errorf("repository %q: pull replication is not supported for %s repositories: %s", r.Name, r.Format, why)
	}
	if _, _, ok := ReplicationSource(r.Remote); !ok {
		return fmt.Errorf("repository %q: pull replication needs the remote to be a hosted repository's URL, <base URL>/repository/<name>/", r.Name)
	}
	return nil
}
