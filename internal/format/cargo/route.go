// Package cargo implements Cargo's sparse registry protocol.
package cargo

import (
	"net/http"
	"regexp"
	"strings"

	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

// Kind is the kind of registry request a path addresses.
type Kind int

const (
	KindConfig   Kind = iota // config.json
	KindIndex                // 1/a, 2/ab, 3/a/abc, ab/cd/abcd...
	KindDownload             // crates/{crate}/{version}/download
	KindPublish              // api/v1/crates/new
	KindYank                 // api/v1/crates/{crate}/{version}/yank
	KindUnyank               // api/v1/crates/{crate}/{version}/unyank
)

// Route is a parsed request; Name is lower-cased.
type Route struct {
	Kind    Kind
	Name    string
	Version string
}

var (
	nameRE        = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	pathVersionRE = regexp.MustCompile(`^[0-9A-Za-z.+-]+$`)
)

// ValidName reports whether name is a valid crate name.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// validVersion checks a newly published version: strict SemVer 2.0.
func validVersion(v string) bool { return semver.Valid(v) }

// validPathVersion accepts a version in a request path. Strict versions pass;
// so do older stored ones (e.g. a numeric prerelease with a leading zero) that
// semver.Valid rejects, so they stay downloadable and yankable. The store
// decides whether the version exists.
func validPathVersion(v string) bool {
	if semver.Valid(v) {
		return true
	}
	if len(v) > 256 || !pathVersionRE.MatchString(v) {
		return false
	}
	_, ok := semver.Parse(v)
	return ok
}

// IndexPath is the sparse index path of a crate.
func IndexPath(name string) string {
	n := strings.ToLower(name)
	switch len(n) {
	case 1:
		return "1/" + n
	case 2:
		return "2/" + n
	case 3:
		return "3/" + n[:1] + "/" + n
	}
	return n[:2] + "/" + n[2:4] + "/" + n
}

func ParseRoute(path string) (Route, error) {
	notFound := repo.Errorf(http.StatusNotFound, "not found: %s", path)
	if path == "config.json" {
		return Route{Kind: KindConfig}, nil
	}
	segs := strings.Split(path, "/")
	if len(segs) >= 3 && segs[0] == "api" && segs[1] == "v1" && segs[2] == "crates" {
		switch {
		case len(segs) == 4 && segs[3] == "new":
			return Route{Kind: KindPublish}, nil
		case len(segs) == 6 && (segs[5] == "yank" || segs[5] == "unyank"):
			name, version, err := nameVersion(segs[3], segs[4])
			if err != nil {
				return Route{}, err
			}
			k := KindYank
			if segs[5] == "unyank" {
				k = KindUnyank
			}
			return Route{Kind: k, Name: name, Version: version}, nil
		}
		return Route{}, notFound
	}
	if len(segs) == 4 && segs[0] == "crates" && segs[3] == "download" {
		name, version, err := nameVersion(segs[1], segs[2])
		if err != nil {
			return Route{}, err
		}
		return Route{Kind: KindDownload, Name: name, Version: version}, nil
	}
	// crates/{crate}/{version}.crate is the stored asset path that the UI and API
	// hand out as the download URL; it must not fall into the index branch.
	if len(segs) == 3 && segs[0] == "crates" && strings.HasSuffix(segs[2], ".crate") {
		name, version, err := nameVersion(segs[1], strings.TrimSuffix(segs[2], ".crate"))
		if err != nil {
			return Route{}, err
		}
		return Route{Kind: KindDownload, Name: name, Version: version}, nil
	}
	if len(segs) == 2 || len(segs) == 3 {
		name := segs[len(segs)-1]
		if !ValidName(name) {
			return Route{}, repo.Errorf(http.StatusBadRequest, "invalid crate name %q", name)
		}
		if IndexPath(name) == strings.ToLower(path) {
			return Route{Kind: KindIndex, Name: strings.ToLower(name)}, nil
		}
	}
	return Route{}, notFound
}

func nameVersion(name, version string) (string, string, error) {
	if !ValidName(name) {
		return "", "", repo.Errorf(http.StatusBadRequest, "invalid crate name %q", name)
	}
	if !validPathVersion(version) {
		return "", "", repo.Errorf(http.StatusBadRequest, "invalid version %q", version)
	}
	return strings.ToLower(name), version, nil
}

// crateKey is the asset path of a crate file.
func crateKey(name, version string) string { return "crates/" + name + "/" + version + ".crate" }

// indexKey is the asset path of a cached upstream index file.
func indexKey(path string) string { return "index/" + path }
