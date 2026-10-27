package nuget

import (
	"archive/zip"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"commandcenter/internal/repo"
)

// maxNuspec bounds a .nuspec read out of a package.
const maxNuspec = 1 << 20

func badPackage(format string, args ...any) error {
	return repo.Errorf(http.StatusBadRequest, "package: "+format, args...)
}

// pkgAttrs is the packages.attrs document of a hosted version: what the
// registration, search and the UI's metadata show, read from the .nuspec.
type pkgAttrs struct {
	ID                       string     `json:"id"`      // as spelled in the nuspec
	Version                  string     `json:"version"` // normalized, case as in the nuspec
	Title                    string     `json:"title,omitempty"`
	Authors                  string     `json:"authors"`
	Description              string     `json:"description"`
	Summary                  string     `json:"summary,omitempty"`
	License                  string     `json:"license,omitempty"` // SPDX expression, or the license file
	LicenseURL               string     `json:"licenseUrl,omitempty"`
	ProjectURL               string     `json:"projectUrl,omitempty"`
	IconURL                  string     `json:"iconUrl,omitempty"`
	RepositoryURL            string     `json:"repositoryUrl,omitempty"`
	Tags                     []string   `json:"tags"`
	RequireLicenseAcceptance bool       `json:"requireLicenseAcceptance"`
	DependencyGroups         []depGroup `json:"dependencyGroups"`
	Listed                   bool       `json:"listed"`
	Published                string     `json:"published"`   // RFC 3339
	PackageHash              string     `json:"packageHash"` // sha512, base64
	Size                     int64      `json:"size"`
}

type depGroup struct {
	TargetFramework string       `json:"targetFramework,omitempty"`
	Dependencies    []dependency `json:"dependencies"`
}

type dependency struct {
	ID    string `json:"id"`
	Range string `json:"range"`
}

type nuspecDep struct {
	ID      string `xml:"id,attr"`
	Version string `xml:"version,attr"`
}

// nuspecXML is the part of a .nuspec the feed uses. Field tags carry no
// namespace, so every nuspec schema version matches.
type nuspecXML struct {
	Metadata struct {
		ID                       string `xml:"id"`
		Version                  string `xml:"version"`
		Title                    string `xml:"title"`
		Authors                  string `xml:"authors"`
		Description              string `xml:"description"`
		Summary                  string `xml:"summary"`
		LicenseURL               string `xml:"licenseUrl"`
		ProjectURL               string `xml:"projectUrl"`
		IconURL                  string `xml:"iconUrl"`
		Tags                     string `xml:"tags"`
		RequireLicenseAcceptance string `xml:"requireLicenseAcceptance"`
		License                  struct {
			Type string `xml:"type,attr"`
			Text string `xml:",chardata"`
		} `xml:"license"`
		Repository struct {
			URL string `xml:"url,attr"`
		} `xml:"repository"`
		PackageTypes []struct {
			Name string `xml:"name,attr"`
		} `xml:"packageTypes>packageType"`
		Dependencies struct {
			Groups []struct {
				TargetFramework string      `xml:"targetFramework,attr"`
				Deps            []nuspecDep `xml:"dependency"`
			} `xml:"group"`
			Deps []nuspecDep `xml:"dependency"`
		} `xml:"dependencies"`
	} `xml:"metadata"`
}

// readNupkg reads the .nuspec at the root of a .nupkg (a zip) and returns
// its attributes and the raw manifest. A symbols package is refused.
func readNupkg(r io.ReaderAt, size int64) (pkgAttrs, []byte, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return pkgAttrs{}, nil, badPackage("not a zip (.nupkg): %v", err)
	}
	for _, f := range zr.File {
		if strings.Contains(f.Name, "/") || !strings.HasSuffix(strings.ToLower(f.Name), ".nuspec") {
			continue
		}
		if f.UncompressedSize64 > maxNuspec {
			return pkgAttrs{}, nil, badPackage("%s is larger than %d bytes", f.Name, maxNuspec)
		}
		rc, err := f.Open()
		if err != nil {
			return pkgAttrs{}, nil, badPackage("reading %s: %v", f.Name, err)
		}
		raw, err := io.ReadAll(io.LimitReader(rc, maxNuspec+1))
		rc.Close()
		if err != nil || len(raw) > maxNuspec {
			return pkgAttrs{}, nil, badPackage("reading %s: %v", f.Name, errors.Join(err, errors.New("too large")))
		}
		a, err := parseNuspec(raw)
		return a, raw, err
	}
	return pkgAttrs{}, nil, badPackage("no .nuspec at the root of the package")
}

// parseNuspec reads the manifest fields the feed uses and checks the id
// and version.
func parseNuspec(raw []byte) (pkgAttrs, error) {
	var n nuspecXML
	if err := xml.Unmarshal(raw, &n); err != nil {
		return pkgAttrs{}, badPackage(".nuspec is not XML: %v", err)
	}
	m := n.Metadata
	for _, t := range m.PackageTypes {
		if strings.EqualFold(t.Name, "SymbolsPackage") {
			return pkgAttrs{}, badPackage("symbol packages (.snupkg) are not supported; push the .nupkg only")
		}
	}
	id := strings.TrimSpace(m.ID)
	if !idRE.MatchString(id) {
		return pkgAttrs{}, badPackage(".nuspec needs an id (%q)", m.ID)
	}
	v, ok := parseVersion(m.Version)
	if !ok {
		return pkgAttrs{}, badPackage(".nuspec needs a version (%q)", m.Version)
	}
	a := pkgAttrs{
		ID: id, Version: v.String(), Title: strings.TrimSpace(m.Title), Authors: strings.TrimSpace(m.Authors),
		Description: strings.TrimSpace(m.Description), Summary: strings.TrimSpace(m.Summary),
		LicenseURL: strings.TrimSpace(m.LicenseURL), ProjectURL: strings.TrimSpace(m.ProjectURL), IconURL: strings.TrimSpace(m.IconURL),
		RepositoryURL: strings.TrimSpace(m.Repository.URL), License: strings.TrimSpace(m.License.Text),
		RequireLicenseAcceptance: strings.EqualFold(strings.TrimSpace(m.RequireLicenseAcceptance), "true"),
		Tags:                     strings.Fields(m.Tags), DependencyGroups: []depGroup{},
	}
	if a.Tags == nil {
		a.Tags = []string{}
	}
	deps := func(list []nuspecDep) []dependency {
		out := make([]dependency, 0, len(list))
		for _, d := range list {
			if id := strings.TrimSpace(d.ID); id != "" {
				out = append(out, dependency{ID: id, Range: versionRange(d.Version)})
			}
		}
		return out
	}
	if len(m.Dependencies.Deps) > 0 {
		a.DependencyGroups = append(a.DependencyGroups, depGroup{Dependencies: deps(m.Dependencies.Deps)})
	}
	for _, g := range m.Dependencies.Groups {
		a.DependencyGroups = append(a.DependencyGroups, depGroup{TargetFramework: strings.TrimSpace(g.TargetFramework), Dependencies: deps(g.Deps)})
	}
	return a, nil
}

// hashBase64 writes a hex sha512 as the base64 NuGet's packageHash uses.
func hashBase64(hexSum string) string {
	b, err := hex.DecodeString(hexSum)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// uploadErr maps a body read error to its status.
func uploadErr(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return repo.Errorf(http.StatusRequestEntityTooLarge, "package is larger than the upload limit")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return badPackage("upload ended early: %v", err)
	}
	return fmt.Errorf("reading package: %w", err)
}
