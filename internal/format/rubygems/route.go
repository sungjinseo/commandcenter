// Package rubygems implements RubyGems repositories (what bundler and gem
// use) under /repository/<repo>/: the compact index (versions, names,
// info/<gem>) that Bundler and RubyGems 3 resolve from, the legacy Marshal
// indexes (specs.4.8.gz and friends, quick/Marshal.4.8/*.gemspec.rz, the
// dependency API) older gem clients read, the gem files (gems/*.gem), and
// the push API (api/v1/gems, api/v1/gems/yank). A hosted repository takes
// gems by POST, reads the Gem::Specification out of the .gem's metadata.gz
// and derives every index from its rows, keeping the versions file as an
// append-only asset; a proxy caches rubygems.org (or another server) as is;
// a group merges its members' versions and names and answers info, gemspec
// and gem requests from the first member that has the gem.
package rubygems

import (
	"net/http"
	"regexp"
	"strings"

	"commandcenter/internal/component"
	"commandcenter/internal/repo"
)

// Kind is the kind of repository request a path addresses.
type Kind int

const (
	KindVersions     Kind = iota // versions: the compact index's gem list
	KindNames                    // names
	KindInfo                     // info/<gem>
	KindSpecs                    // specs.4.8.gz, latest_specs.4.8.gz, prerelease_specs.4.8.gz
	KindGemspec                  // quick/Marshal.4.8/<full>.gemspec.rz
	KindGem                      // gems/<full>.gem
	KindDependencies             // api/v1/dependencies?gems=a,b
	KindPush                     // api/v1/gems (POST)
	KindYank                     // api/v1/gems/yank (DELETE)
)

// Route is a parsed request: Name is the gem of an info request, Full the
// <name>-<version>[-<platform>] of a gem or gemspec request, File the specs
// index file name.
type Route struct {
	Kind             Kind
	Name, Full, File string
}

const (
	versionsKey = "versions"
	namesKey    = "names"
	infoPrefix  = "info/"
	gemsPrefix  = "gems/"
	quickPrefix = "quick/Marshal.4.8/"
	depsPrefix  = "api/v1/dependencies/" // a proxy's cached dependency answers, by query hash
	ctText      = "text/plain; charset=utf-8"
	ctBinary    = "application/octet-stream"
	ctGzip      = "application/x-gzip"
)

// nameRE is a gem name as RubyGems accepts one. fullRE is a gem's full name
// in a file name; SplitGem checks its shape.
var (
	nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
	fullRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,199}$`)
)

var specsFiles = map[string]bool{"specs.4.8.gz": true, "latest_specs.4.8.gz": true, "prerelease_specs.4.8.gz": true}

func ParseRoute(path string) (Route, error) {
	notFound := repo.Errorf(http.StatusNotFound, "not found: %s", path)
	switch {
	case path == versionsKey:
		return Route{Kind: KindVersions}, nil
	case path == namesKey:
		return Route{Kind: KindNames}, nil
	case specsFiles[path]:
		return Route{Kind: KindSpecs, File: path}, nil
	case path == "api/v1/dependencies":
		return Route{Kind: KindDependencies}, nil
	case path == "api/v1/gems":
		return Route{Kind: KindPush}, nil
	case path == "api/v1/gems/yank":
		return Route{Kind: KindYank}, nil
	}
	if name, ok := strings.CutPrefix(path, infoPrefix); ok {
		if !nameRE.MatchString(name) {
			return Route{}, notFound
		}
		return Route{Kind: KindInfo, Name: name}, nil
	}
	if rest, ok := strings.CutPrefix(path, gemsPrefix); ok {
		full, isGem := strings.CutSuffix(rest, ".gem")
		if _, _, ok := component.SplitGem(full); !isGem || !ok || !fullRE.MatchString(full) {
			return Route{}, notFound
		}
		return Route{Kind: KindGem, Full: full}, nil
	}
	if rest, ok := strings.CutPrefix(path, quickPrefix); ok {
		full, isSpec := strings.CutSuffix(rest, ".gemspec.rz")
		if _, _, ok := component.SplitGem(full); !isSpec || !ok || !fullRE.MatchString(full) {
			return Route{}, notFound
		}
		return Route{Kind: KindGemspec, Full: full}, nil
	}
	return Route{}, notFound
}

// gemFile is the stored path of a gem version: gems/<full>.gem, as clients
// request it and as rubygems.org lays them out.
func gemFile(full string) string { return gemsPrefix + full + ".gem" }

// fullName is <name>-<version>[-<platform>].
func fullName(name, rowVersion string) string { return name + "-" + rowVersion }

// versionRE is a version as Gem::Version normalizes one (never a "-", which
// it turns into ".pre."); platformRE a platform, which starts with a letter.
// They mirror component.SplitGem's.
var (
	versionRE  = regexp.MustCompile(`^[0-9]+(\.[0-9A-Za-z]+)*$`)
	platformRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)
)

// candidates lists the (name, version[-platform]) splits of a full name
// that SplitGem's rule allows, longest name first, for the lookups that
// cannot know where the name ends ("ruby-2d-0.1.0" is ruby-2d 0.1.0, but
// rows decide): callers try each until a row exists.
func candidates(full string) [][2]string {
	var out [][2]string
	for i := len(full) - 2; i > 0; i-- {
		if full[i] != '-' {
			continue
		}
		v, platform, _ := strings.Cut(full[i+1:], "-")
		if versionRE.MatchString(v) && (platform == "" || platformRE.MatchString(platform)) && nameRE.MatchString(full[:i]) {
			out = append(out, [2]string{full[:i], full[i+1:]})
		}
	}
	return out
}
