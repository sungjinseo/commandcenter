package helm

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"commandcenter/internal/component"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

const (
	maxChartYAML = 1 << 20 // bound of a Chart.yaml
	maxUnpacked  = 1 << 30 // bound of the unpacked bytes read looking for it
	maxProv      = 2 << 20 // bound of a provenance file: the Chart.yaml again, and a signature
	ctChart      = "application/gzip"
	ctYAML       = "application/x-yaml"
	ctProv       = "text/plain; charset=utf-8"
	// provHead starts a provenance file: PGP clear-signed text.
	provHead = "-----BEGIN PGP SIGNED MESSAGE-----"
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
// tar, the chart in one top-level directory) and checks its name and
// version: one helm accepts (component.HelmVersion).
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
		switch v := e["version"].(type) { // unquoted, a number to helm as well: 1.2 and 1, and 1.10 is 1.1
		case int:
			e["version"] = strconv.Itoa(v)
		case float64:
			e["version"] = strconv.FormatFloat(v, 'f', -1, 64)
		}
		name, version := e.str("name"), e.str("version")
		if _, _, ok := component.SplitHelm(ChartFile(name, version)); !ok || !component.HelmVersion(version) {
			return nil, badChart("Chart.yaml needs a name (%q) and a version helm accepts (%q): SemVer 2, or one such as v1.2.3 or 1.2", name, version)
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

// provFile reads a provenance file as `helm package --sign` writes one: PGP
// clear-signed text that holds the chart's Chart.yaml, a "..." line and
// "files:" with the chart file and its sha256. It returns that file and
// digest. The signature is the client's to check (helm pull --verify).
func provFile(b []byte) (file, digest string, err error) {
	text := string(b)
	end := strings.Index(text, "\n-----BEGIN PGP SIGNATURE-----")
	if !strings.HasPrefix(text, provHead) || end < 0 {
		return "", "", badChart("not a provenance file: no PGP clear-signed text")
	}
	_, tail, _ := strings.Cut(text[:end], "\n...\n")
	var sums struct {
		Files map[string]string `yaml:"files"`
	}
	if err := yaml.Unmarshal([]byte(tail), &sums); err != nil || len(sums.Files) != 1 {
		return "", "", badChart("the provenance file does not name one chart file under \"files:\"")
	}
	for f, d := range sums.Files {
		file, digest = f, strings.TrimPrefix(d, "sha256:")
	}
	if !fileRE.MatchString(file) || !proxy.IsSHA256Hex(digest) {
		return "", "", badChart("the provenance file names %.100q with %.100q, not a chart file with its sha256", file, sums.Files[file])
	}
	return file, digest, nil
}
