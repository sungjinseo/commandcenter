// Package docker implements the Docker/OCI Distribution registry API (v2)
// for hosted, proxy and group repositories. It is served at /v2/, where a
// registry name is <repository>/<image path>: `docker push
// host/<repo>/<image>:<tag>`.
package docker

import (
	"regexp"
	"strings"
)

// Kind is the kind of registry request a path addresses.
type Kind int

const (
	KindBase     Kind = iota // /v2/ (API version check)
	KindToken                // /v2/token
	KindCatalog              // /v2/_catalog
	KindManifest             // /v2/<name>/manifests/<reference>
	KindTags                 // /v2/<name>/tags/list
	KindBlob                 // /v2/<name>/blobs/<digest>
	KindUploads              // /v2/<name>/blobs/uploads/ (start)
	KindUpload               // /v2/<name>/blobs/uploads/<uuid>
)

// Route is a parsed /v2/ path. Repo is the Command Center repository (the
// first segment of the registry name) and Image the rest of the name.
type Route struct {
	Kind      Kind
	Repo      string
	Image     string
	Reference string // manifest tag or digest
	Digest    string // blob digest (blobs/<digest>)
	UUID      string // upload session id
}

// Name is the registry name: repo/image.
func (rt Route) Name() string { return rt.Repo + "/" + rt.Image }

var (
	// imageRE is the Distribution grammar for a name component path.
	imageRE  = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	tagRE    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	uuidRE   = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

// ValidName reports whether name is a registry name of a repository here:
// a repository name (no slash) and a valid image path, at most 255 bytes.
func ValidName(name string) bool {
	repo, image, ok := strings.Cut(name, "/")
	return ok && len(name) <= 255 && repo != "" && imageRE.MatchString(image)
}

// ValidDigest reports whether d is a digest this registry stores (sha256).
func ValidDigest(d string) bool { return digestRE.MatchString(d) }

// ValidTag reports whether t is a tag.
func ValidTag(t string) bool { return tagRE.MatchString(t) }

// ParseRoute parses the path after /v2/. ok is false when the path is not a
// registry route (404) and the name is invalid when the route has one but
// it fails ValidName (NAME_INVALID).
func ParseRoute(path string) (rt Route, ok bool) {
	switch path {
	case "":
		return Route{Kind: KindBase}, true
	case "token":
		return Route{Kind: KindToken}, true
	case "_catalog":
		return Route{Kind: KindCatalog}, true
	}
	segs := strings.Split(path, "/")
	n := len(segs)
	name := func(k int) (Route, bool) { // the name is the first n-k segments
		if n-k < 2 {
			return Route{}, false
		}
		rt := Route{Repo: segs[0], Image: strings.Join(segs[1:n-k], "/")}
		for _, s := range segs[:n-k] {
			if s == "" {
				return Route{}, false
			}
		}
		return rt, true
	}
	switch {
	case n >= 4 && segs[n-2] == "manifests":
		rt, ok := name(2)
		rt.Kind, rt.Reference = KindManifest, segs[n-1]
		return rt, ok
	case n >= 4 && segs[n-2] == "tags" && segs[n-1] == "list":
		rt, ok := name(2)
		rt.Kind = KindTags
		return rt, ok
	case n >= 5 && segs[n-3] == "blobs" && segs[n-2] == "uploads":
		rt, ok := name(3)
		if segs[n-1] == "" {
			rt.Kind = KindUploads
		} else {
			rt.Kind, rt.UUID = KindUpload, segs[n-1]
		}
		return rt, ok
	case n >= 4 && segs[n-2] == "blobs":
		rt, ok := name(2)
		rt.Kind, rt.Digest = KindBlob, segs[n-1]
		return rt, ok
	}
	return Route{}, false
}

// asset paths under the repository: an image's manifests and blobs.
func manifestKey(image, digest string) string { return image + "/manifests/" + digest }
func blobKey(image, digest string) string     { return image + "/blobs/" + digest }
