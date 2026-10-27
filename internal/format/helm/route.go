// Package helm implements Helm chart repositories (Nexus "helm"): index.yaml
// and <name>-<version>.tgz files under /repository/<repo>/. A hosted
// repository takes charts by PUT of the file or by helm cm-push's POST to
// api/charts, and keeps index.yaml itself; a proxy caches an upstream
// repository's index.yaml and charts; a group merges its members' indexes.
// Charts pushed with `helm push oci://` go to a docker repository, which the
// registry API already serves (see internal/format/docker).
package helm

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
	KindIndex Kind = iota // index.yaml
	KindChart             // <name>-<version>.tgz
	KindAPI               // api/charts (helm cm-push)
)

// Route is a parsed request. File is the chart file name of a KindChart.
type Route struct {
	Kind Kind
	File string
}

// fileRE is a chart file name: one path segment ending in .tgz, as helm
// package writes it (no "/", no leading ".").
var fileRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]{0,255}\.tgz$`)

const (
	indexKey    = "index.yaml"
	upstreamKey = "upstream/index.yaml" // a proxy's cached copy of the remote index.yaml
)

// ChartFile is the file name of a chart version, and its asset path.
func ChartFile(name, version string) string { return name + "-" + version + ".tgz" }

func ParseRoute(path string) (Route, error) {
	switch {
	case path == indexKey:
		return Route{Kind: KindIndex}, nil
	case path == "api/charts":
		return Route{Kind: KindAPI}, nil
	case fileRE.MatchString(path):
		return Route{Kind: KindChart, File: path}, nil
	}
	return Route{}, repo.Errorf(http.StatusNotFound, "not found: %s", path)
}

// chartName is the chart a file name belongs to, for the owner rule of
// groups; "" when the name cannot be told (component.SplitHelm).
func chartName(file string) string {
	name, _, _ := component.SplitHelm(file)
	return name
}

// sameFile reports whether got names the chart file want, "+" in the
// version written "_" as helm package names the file or not.
func sameFile(want, got string) bool {
	return want == got || strings.ReplaceAll(want, "+", "_") == got
}
