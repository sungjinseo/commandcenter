// Package maven implements the Maven 2 repository format.
package maven

import (
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"commandcenter/internal/repo"
)

// Coordinates identify an artifact file.
type Coordinates struct {
	GroupID     string
	ArtifactID  string
	BaseVersion string // directory version, e.g. 1.0-SNAPSHOT
	Version     string // file version, e.g. 1.0-20261014.120000-3
	Classifier  string
	Extension   string
	Snapshot    bool
}

// Path is a parsed request path.
type Path struct {
	Path     string       // as requested
	Main     string       // without a checksum suffix
	Hash     string       // "", "sha1", "md5", "sha256" or "sha512"
	Metadata bool         // Main is a maven-metadata.xml file
	Coords   *Coordinates // nil for metadata or non-artifact paths
}

var (
	hashTypes   = []string{"sha1", "md5", "sha256", "sha512"}
	timestampRE = regexp.MustCompile(`^\d{8}\.\d{6}-\d+`)
	// timestampedVersionRE matches a resolved snapshot version such as
	// 1.4-20260101.000000-1, which is not a release.
	timestampedVersionRE = regexp.MustCompile(`-\d{8}\.\d{6}-\d+$`)
	// sbtPluginRE matches an artifact directory with sbt's _<scala>_<sbt>
	// plugin cross suffix, such as sbt-assembly_2.12_1.0.
	sbtPluginRE = regexp.MustCompile(`^(.+)_\d+(?:\.\d+)*_\d+(?:\.\d+)*$`)
)

func ParsePath(p string) (Path, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || strings.Contains(p, "//") {
		return Path{}, repo.Errorf(http.StatusBadRequest, "invalid path %q", p)
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if s == "." || s == ".." {
			return Path{}, repo.Errorf(http.StatusBadRequest, "invalid path %q", p)
		}
	}
	out := Path{Path: p, Main: p}
	for _, h := range hashTypes {
		if strings.HasSuffix(p, "."+h) {
			out.Hash = h
			out.Main = strings.TrimSuffix(p, "."+h)
			break
		}
	}
	segs = strings.Split(out.Main, "/")
	if segs[len(segs)-1] == "maven-metadata.xml" {
		out.Metadata = true
		return out, nil
	}
	out.Coords = parseCoords(segs)
	return out, nil
}

// fetchable reports whether a proxy may fetch and cache p: a Maven file (an
// artifact, signatures included, a maven-metadata.xml, the prefix list or the
// archetype catalog), an sbt plugin file in sbt's legacy layout, or a
// checksum of one.
func (p Path) fetchable() bool {
	return p.Coords != nil || p.Metadata || p.Main == prefixesPath || p.Main == "archetype-catalog.xml" || sbtLegacy(p.Main)
}

// sbtLegacy reports whether path is an sbt plugin file in the legacy layout,
// which sbt before 1.9 publishes and asks for (later versions fall back to
// it): the artifact directory has the cross suffix and the file name does
// not, as in com/eed3si9n/sbt-assembly_2.12_1.0/2.1.1/sbt-assembly-2.1.1.jar.
// Such a path has no Coords, so hosted repositories still refuse it.
func sbtLegacy(path string) bool {
	segs := strings.Split(path, "/")
	n := len(segs)
	if n < 4 {
		return false
	}
	m := sbtPluginRE.FindStringSubmatch(segs[n-3])
	if m == nil {
		return false
	}
	segs[n-3] = m[1]
	return parseCoords(segs) != nil
}

// validateWritePath refuses names a new upload may not use: bytes that are
// not UTF-8, control characters and URL delimiters (query, fragment, matrix
// parameters) never belong to a Maven file name (QA L-13). It applies to
// writes only, so assets stored by older builds under such names are still
// served and deleted.
func validateWritePath(p string) error {
	if !utf8.ValidString(p) || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '?' || r == '#' || r == ';' }) {
		return repo.Errorf(http.StatusBadRequest, "invalid path %q", p)
	}
	return nil
}

func parseCoords(segs []string) *Coordinates {
	n := len(segs)
	if n < 4 {
		return nil
	}
	file, version, artifact := segs[n-1], segs[n-2], segs[n-3]
	rest, ok := strings.CutPrefix(file, artifact+"-")
	if !ok {
		return nil
	}
	c := &Coordinates{
		GroupID:     strings.Join(segs[:n-3], "."),
		ArtifactID:  artifact,
		BaseVersion: version,
		Snapshot:    strings.HasSuffix(version, "-SNAPSHOT"),
	}
	switch {
	case strings.HasPrefix(rest, version):
		c.Version = version
	case c.Snapshot:
		base := strings.TrimSuffix(version, "SNAPSHOT")
		tail, ok := strings.CutPrefix(rest, base)
		if !ok {
			return nil
		}
		ts := timestampRE.FindString(tail)
		if ts == "" {
			return nil
		}
		c.Version = base + ts
	default:
		return nil
	}
	if len(rest) == len(c.Version) || (rest[len(c.Version)] != '-' && rest[len(c.Version)] != '.') {
		return nil
	}
	tail := rest[len(c.Version):]
	if strings.HasPrefix(tail, "-") {
		i := strings.IndexByte(tail, '.')
		if i < 0 {
			return nil
		}
		c.Classifier = tail[1:i]
		tail = tail[i:]
	}
	ext, ok := strings.CutPrefix(tail, ".")
	if !ok || ext == "" || isDigit(ext[0]) {
		return nil
	}
	c.Extension = ext
	return c
}

// ContentType returns the media type served for a Maven path.
func ContentType(path string) string {
	switch {
	case strings.HasSuffix(path, ".pom"), strings.HasSuffix(path, ".xml"):
		return "application/xml"
	case strings.HasSuffix(path, ".jar"), strings.HasSuffix(path, ".war"), strings.HasSuffix(path, ".ear"):
		return "application/java-archive"
	case strings.HasSuffix(path, ".sha1"), strings.HasSuffix(path, ".md5"), strings.HasSuffix(path, ".sha256"),
		strings.HasSuffix(path, ".sha512"), strings.HasSuffix(path, ".asc"):
		return "text/plain"
	case strings.HasSuffix(path, ".zip"):
		return "application/zip"
	case strings.HasSuffix(path, ".json"):
		return "application/json"
	case strings.HasSuffix(path, ".gz"), strings.HasSuffix(path, ".tgz"):
		return "application/x-gzip"
	}
	return "application/octet-stream"
}
