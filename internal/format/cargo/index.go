package cargo

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"commandcenter/internal/repo"
)

// maxMeta bounds publish metadata and maxLine an index line; both are 1 MiB
// so a published line always fits what readers accept. Tests may lower maxLine.
const maxMeta = 1 << 20

var maxLine = maxMeta

// indexDep is a dependency in an index line (crates.io index schema).
type indexDep struct {
	Name            string   `json:"name"`
	Req             string   `json:"req"`
	Features        []string `json:"features"`
	Optional        bool     `json:"optional"`
	DefaultFeatures bool     `json:"default_features"`
	Target          *string  `json:"target"`
	Kind            string   `json:"kind"`
	Registry        *string  `json:"registry,omitempty"`
	Package         *string  `json:"package,omitempty"`
}

// indexLine is one version of a crate in the index.
type indexLine struct {
	Name        string              `json:"name"`
	Vers        string              `json:"vers"`
	Deps        []indexDep          `json:"deps"`
	Cksum       string              `json:"cksum"`
	Features    map[string][]string `json:"features"`
	Yanked      bool                `json:"yanked"`
	Links       *string             `json:"links,omitempty"`
	V           int                 `json:"v,omitempty"`
	Features2   map[string][]string `json:"features2,omitempty"`
	RustVersion *string             `json:"rust_version,omitempty"`
}

func (l indexLine) marshal() ([]byte, error) { return json.Marshal(l) }

// publishMeta is the JSON part of a `cargo publish` request.
type publishMeta struct {
	Name string `json:"name"`
	Vers string `json:"vers"`
	Deps []struct {
		Name               string   `json:"name"`
		VersionReq         string   `json:"version_req"`
		Features           []string `json:"features"`
		Optional           bool     `json:"optional"`
		DefaultFeatures    bool     `json:"default_features"`
		Target             *string  `json:"target"`
		Kind               string   `json:"kind"`
		Registry           *string  `json:"registry"`
		ExplicitNameInToml *string  `json:"explicit_name_in_toml"`
	} `json:"deps"`
	Features    map[string][]string `json:"features"`
	Links       *string             `json:"links"`
	RustVersion *string             `json:"rust_version"`
}

func badPublish(format string, args ...any) error {
	return repo.Errorf(http.StatusBadRequest, format, args...)
}

// readPublish parses a publish body: a little-endian uint32 length, the JSON
// metadata, another uint32 length and the .crate bytes. The returned reader
// yields at most the declared crate length.
func readPublish(r io.Reader) (publishMeta, io.Reader, int64, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		if isTooLarge(err) {
			return publishMeta{}, nil, 0, err
		}
		return publishMeta{}, nil, 0, badPublish("publish body is truncated: %v", err)
	}
	if n > maxMeta {
		return publishMeta{}, nil, 0, repo.Errorf(http.StatusRequestEntityTooLarge, "publish metadata is too large")
	}
	raw, err := io.ReadAll(io.LimitReader(r, int64(n)))
	if isTooLarge(err) {
		return publishMeta{}, nil, 0, err
	}
	if err != nil || len(raw) != int(n) {
		return publishMeta{}, nil, 0, badPublish("publish metadata is truncated")
	}
	var m publishMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return publishMeta{}, nil, 0, badPublish("invalid publish metadata: %v", err)
	}
	if !ValidName(m.Name) {
		return publishMeta{}, nil, 0, badPublish("invalid crate name %q", m.Name)
	}
	if !validVersion(m.Vers) {
		return publishMeta{}, nil, 0, badPublish("invalid version %q", m.Vers)
	}
	var size uint32
	if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
		if isTooLarge(err) {
			return publishMeta{}, nil, 0, err
		}
		return publishMeta{}, nil, 0, badPublish("publish body has no crate: %v", err)
	}
	return m, io.LimitReader(r, int64(size)), int64(size), nil
}

// indexLine converts publish metadata to the line stored in the index.
func (m publishMeta) indexLine(cksum string) indexLine {
	l := indexLine{Name: m.Name, Vers: m.Vers, Cksum: cksum, Links: m.Links, RustVersion: m.RustVersion,
		Deps: []indexDep{}, Features: map[string][]string{}}
	for _, d := range m.Deps {
		dep := indexDep{Name: d.Name, Req: d.VersionReq, Features: d.Features, Optional: d.Optional,
			DefaultFeatures: d.DefaultFeatures, Target: d.Target, Kind: d.Kind, Registry: d.Registry}
		if dep.Features == nil {
			dep.Features = []string{}
		}
		if dep.Kind == "" {
			dep.Kind = "normal"
		}
		if d.ExplicitNameInToml != nil && *d.ExplicitNameInToml != "" {
			pkg := d.Name
			dep.Name, dep.Package = *d.ExplicitNameInToml, &pkg
		}
		l.Deps = append(l.Deps, dep)
	}
	for f, vals := range m.Features {
		if usesNewSyntax(vals) {
			if l.Features2 == nil {
				l.Features2 = map[string][]string{}
			}
			l.Features2[f] = vals
			l.V = 2
			continue
		}
		l.Features[f] = vals
	}
	return l
}

func usesNewSyntax(vals []string) bool {
	for _, v := range vals {
		if strings.HasPrefix(v, "dep:") || strings.Contains(v, "?/") {
			return true
		}
	}
	return false
}

// lineVersion returns the "vers" of an index line.
func lineVersion(line []byte) (string, error) {
	var v struct {
		Vers string `json:"vers"`
	}
	if err := json.Unmarshal(line, &v); err != nil {
		return "", err
	}
	return v.Vers, nil
}

// downloadURL expands a registry "dl" template for one crate version.
func downloadURL(dl, name, version, cksum string) (string, error) {
	prefix := IndexPath(name)
	prefix = prefix[:strings.LastIndexByte(prefix, '/')]
	var out string
	if hasMarker(dl) {
		out = strings.NewReplacer(
			"{crate}", name, "{version}", version, "{sha256-checksum}", cksum,
			"{lowerprefix}", strings.ToLower(prefix), "{prefix}", originalPrefix(name),
		).Replace(dl)
	} else {
		out = strings.TrimRight(dl, "/") + "/" + name + "/" + version + "/download"
	}
	u, err := url.Parse(out)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid download URL %q", out)
	}
	return out, nil
}

// originalPrefix is the index directory of name without lower-casing.
func originalPrefix(name string) string {
	switch len(name) {
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return "3/" + name[:1]
	}
	return name[:2] + "/" + name[2:4]
}

// hasMarker reports whether dl contains one of the cargo "dl" template markers.
func hasMarker(dl string) bool {
	for _, m := range []string{"{crate}", "{version}", "{prefix}", "{lowerprefix}", "{sha256-checksum}"} {
		if strings.Contains(dl, m) {
			return true
		}
	}
	return false
}

// isTooLarge reports whether err is an exceeded request-body cap, which must
// reach repo.StatusOf unchanged so it maps to 413.
func isTooLarge(err error) bool {
	return errors.As(err, new(*http.MaxBytesError))
}
