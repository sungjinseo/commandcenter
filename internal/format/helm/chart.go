package helm

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"gopkg.in/yaml.v3"

	"commandcenter/internal/component"
	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

const (
	maxChartYAML = 1 << 20 // bound of a Chart.yaml
	maxUnpacked  = 1 << 30 // bound of the unpacked bytes read looking for it
	ctChart      = "application/gzip"
	ctYAML       = "application/x-yaml"
)

func badChart(format string, args ...any) error {
	return repo.Errorf(http.StatusBadRequest, "chart: "+format, args...)
}

// entry is one chart version of index.yaml: its Chart.yaml as a map, plus
// created, digest and urls. A map keeps every Chart.yaml field (helm search
// shows description and appVersion; helm dependency reads dependencies).
type entry map[string]any

func (e entry) str(key string) string { s, _ := e[key].(string); return s }

// readChart reads <dir>/Chart.yaml out of a chart tarball (gzip-compressed
// tar, the chart in one top-level directory) and checks its name and version.
func readChart(r io.Reader) (entry, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, badChart("not a gzip-compressed tar: %v", err)
	}
	lr := &io.LimitedReader{R: gz, N: maxUnpacked}
	tr := tar.NewReader(lr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF || lr.N == 0 {
			return nil, badChart("no Chart.yaml in the chart's top-level directory")
		}
		if err != nil {
			return nil, badChart("not a gzip-compressed tar: %v", err)
		}
		p := path.Clean(hdr.Name)
		if hdr.Typeflag != tar.TypeReg || path.Base(p) != "Chart.yaml" || strings.Count(p, "/") != 1 || p[0] == '/' {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxChartYAML+1))
		if err != nil {
			return nil, badChart("reading Chart.yaml: %v", err)
		}
		if len(b) > maxChartYAML {
			return nil, badChart("Chart.yaml is larger than %d bytes", maxChartYAML)
		}
		var e entry
		if err := yaml.Unmarshal(b, &e); err != nil || e == nil {
			return nil, badChart("Chart.yaml is not a YAML mapping: %v", err)
		}
		name, version := e.str("name"), e.str("version")
		if _, _, ok := component.SplitHelm(ChartFile(name, version)); !ok || !semver.Valid(version) {
			return nil, badChart("Chart.yaml needs a name (%q) and a SemVer 2 version (%q)", name, version)
		}
		return e, nil
	}
}

// uploadErr maps a body read error to its status.
func uploadErr(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return repo.Errorf(http.StatusRequestEntityTooLarge, "chart is larger than the upload limit")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return badChart("upload ended early: %v", err)
	}
	return fmt.Errorf("reading chart: %w", err)
}
