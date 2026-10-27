package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"commandcenter/internal/auth"
	"commandcenter/internal/format/golang"
	"commandcenter/internal/format/maven"
	"commandcenter/internal/format/pypi"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// componentMetadata is GET /-/api/repositories/{name}/components/metadata:
// the description, license and links of one component and its declared
// dependencies, read from what the format stored (the npm version manifest,
// the Cargo index line, the Maven POM, the Go .mod). "collected" is false
// when the format keeps no such metadata (PyPI files, Go without a .mod),
// which the UI shows apart from "no dependencies". Nothing is resolved:
// Maven parents and BOMs are not read, so requirements are as declared.
type componentMetadata struct {
	Format        string         `json:"format"`
	Description   *string        `json:"description"`
	License       *string        `json:"license"`
	Homepage      *string        `json:"homepage"`
	RepositoryURL *string        `json:"repositoryUrl"`
	Collected     bool           `json:"collected"`
	Dependencies  []dependencyJS `json:"dependencies"`
	Resolved      bool           `json:"resolved"`
}

type dependencyJS struct {
	Scope       string `json:"scope"`
	Group       string `json:"group"`
	Name        string `json:"name"`
	Requirement string `json:"requirement"`
	Optional    bool   `json:"optional"`
}

// metadataReadLimit bounds one stored file read for metadata (a POM or
// go.mod); real ones are kilobytes.
const metadataReadLimit = 1 << 20

func (u *UI) componentMetadata(w http.ResponseWriter, r *http.Request) {
	rp, ok := u.lookup(w, r)
	if !ok {
		return
	}
	if !u.Auth.Can(principal(r), rp.Name, auth.Read) {
		writeError(w, http.StatusForbidden, "read access to "+rp.Name+" is required")
		return
	}
	q := r.URL.Query()
	group, name, version := strings.TrimSpace(q.Get("group")), strings.TrimSpace(q.Get("name")), strings.TrimSpace(q.Get("version"))
	if name == "" || version == "" || (rp.Format == "maven" && group == "") {
		writeError(w, http.StatusBadRequest, "give name and version (and group for maven)")
		return
	}
	m := componentMetadata{Format: rp.Format, Dependencies: []dependencyJS{}}
	var (
		found bool
		err   error
	)
	switch rp.Format {
	case "maven":
		found, err = u.mavenMetadata(r.Context(), rp, group, name, version, &m)
	case "npm":
		found, err = u.npmMetadata(r.Context(), rp, name, version, &m)
	case "cargo":
		found, err = u.cargoMetadata(r.Context(), rp, name, version, &m)
	case "pypi":
		found, err = u.pypiMetadata(r.Context(), rp, name, version, &m)
	case "go":
		found, err = u.goMetadata(r.Context(), rp, name, version, &m)
	case "helm":
		found, err = u.helmMetadata(r.Context(), rp, name, version, &m)
	default:
		writeError(w, http.StatusBadRequest, "no metadata for format "+rp.Format)
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "component not found")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// storedPackage finds the row of one version of a package. Hosted and proxy
// rows look the same; a proxy has rows for what it cached (npm, Cargo), or
// none (Maven, Go, whose files are found as assets).
func (u *UI) storedPackage(ctx context.Context, rp *repo.Repository, name, version string) (meta.Package, bool, error) {
	pkgs, err := u.DB.ListPackages(ctx, rp.Name, name)
	if err != nil {
		return meta.Package{}, false, err
	}
	for _, p := range pkgs {
		if p.Version == version {
			return p, true, nil
		}
	}
	return meta.Package{}, false, nil
}

// readStored opens the asset at path and reads at most metadataReadLimit of it.
func (u *UI) readStored(ctx context.Context, rp *repo.Repository, path string) ([]byte, bool, error) {
	a, err := u.DB.GetAsset(ctx, rp.Name, path)
	if errors.Is(err, meta.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if u.Store == nil {
		return nil, false, nil
	}
	f, err := u.Store.Open(a)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, metadataReadLimit))
	return b, true, err
}

func (u *UI) mavenMetadata(ctx context.Context, rp *repo.Repository, group, name, version string, m *componentMetadata) (bool, error) {
	// The model POM lies in the version directory under its own name; a
	// SNAPSHOT's timestamped POM is found through the directory listing.
	dir := strings.ReplaceAll(group, ".", "/") + "/" + name + "/" + version + "/"
	pomPath := dir + name + "-" + version + ".pom"
	b, ok, err := u.readStored(ctx, rp, pomPath)
	if err != nil {
		return false, err
	}
	if !ok {
		entries, _, err := u.DB.ListDir(ctx, rp.Name, dir, browseLimit)
		if err != nil {
			return false, err
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name, ".pom") && !strings.Contains(strings.TrimSuffix(e.Name, ".pom"), "-sources") {
				b, ok, err = u.readStored(ctx, rp, dir+e.Name)
				if err != nil {
					return false, err
				}
				if ok {
					break
				}
			}
		}
		if !ok {
			if len(entries) == 0 {
				return false, nil
			}
			return true, nil // files without a POM: nothing collected
		}
	}
	info := maven.ReadPOMInfo(bufio.NewReader(strings.NewReader(string(b))))
	m.Collected = true
	m.Description, m.License, m.Homepage, m.RepositoryURL = optStr(info.Description), optStr(info.License), optStr(info.Homepage), optStr(info.SCM)
	for _, d := range info.Dependencies {
		scope := d.Scope
		if scope == "" {
			scope = "compile"
		}
		m.Dependencies = append(m.Dependencies, dependencyJS{Scope: scope, Group: d.GroupID, Name: d.ArtifactID, Requirement: d.Version, Optional: d.Optional})
	}
	return true, nil
}

func (u *UI) npmMetadata(ctx context.Context, rp *repo.Repository, name, version string, m *componentMetadata) (bool, error) {
	p, ok, err := u.storedPackage(ctx, rp, name, version)
	if err != nil || !ok {
		return ok, err
	}
	var v struct {
		Description string            `json:"description"`
		License     json.RawMessage   `json:"license"`
		Homepage    string            `json:"homepage"`
		Repository  json.RawMessage   `json:"repository"`
		Deps        map[string]string `json:"dependencies"`
		Dev         map[string]string `json:"devDependencies"`
		Peer        map[string]string `json:"peerDependencies"`
		Optional    map[string]string `json:"optionalDependencies"`
	}
	if err := json.Unmarshal([]byte(p.Attrs), &v); err != nil {
		return true, nil // an unreadable manifest: nothing collected
	}
	m.Collected = true
	m.Description, m.Homepage = optStr(v.Description), optStr(v.Homepage)
	m.License = optStr(jsonStringOrField(v.License, "type"))
	m.RepositoryURL = optStr(jsonStringOrField(v.Repository, "url"))
	add := func(scope string, deps map[string]string, optional bool) {
		for _, n := range sortedKeys(deps) {
			m.Dependencies = append(m.Dependencies, dependencyJS{Scope: scope, Name: n, Requirement: deps[n], Optional: optional})
		}
	}
	add("runtime", v.Deps, false)
	add("optional", v.Optional, true)
	add("peer", v.Peer, false)
	add("dev", v.Dev, false)
	return true, nil
}

// jsonStringOrField reads a value npm writes either as a string or as an
// object ("license": "MIT" or {"type": "MIT"}; "repository" likewise).
func jsonStringOrField(raw json.RawMessage, field string) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o map[string]string
	if json.Unmarshal(raw, &o) == nil {
		return o[field]
	}
	return ""
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ { // insertion sort: dependency lists are short
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func (u *UI) cargoMetadata(ctx context.Context, rp *repo.Repository, name, version string, m *componentMetadata) (bool, error) {
	p, ok, err := u.storedPackage(ctx, rp, strings.ToLower(name), version)
	if err != nil || !ok {
		return ok, err
	}
	var line struct {
		Deps []struct {
			Name     string  `json:"name"`
			Req      string  `json:"req"`
			Optional bool    `json:"optional"`
			Kind     string  `json:"kind"`
			Package  *string `json:"package"`
		} `json:"deps"`
	}
	if err := json.Unmarshal([]byte(p.Attrs), &line); err != nil {
		return true, nil
	}
	m.Collected = true // the index line holds dependencies but no description or license
	for _, d := range line.Deps {
		n := d.Name
		if d.Package != nil && *d.Package != "" {
			n = *d.Package
		}
		kind := d.Kind
		if kind == "" {
			kind = "normal"
		}
		m.Dependencies = append(m.Dependencies, dependencyJS{Scope: kind, Name: n, Requirement: d.Req, Optional: d.Optional})
	}
	return true, nil
}

// helmMetadata reads the index entry a helm hosted row holds: its Chart.yaml
// (description, home, dependencies) plus created, digest and urls.
func (u *UI) helmMetadata(ctx context.Context, rp *repo.Repository, name, version string, m *componentMetadata) (bool, error) {
	p, ok, err := u.storedPackage(ctx, rp, name, version)
	if err != nil || !ok {
		return ok, err
	}
	var e struct {
		Description  string `json:"description"`
		Home         string `json:"home"`
		Dependencies []struct {
			Name, Version, Repository, Condition string
		} `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(p.Attrs), &e); err != nil {
		return true, nil
	}
	m.Collected = true
	m.Description, m.Homepage = optStr(e.Description), optStr(e.Home)
	for _, d := range e.Dependencies {
		m.Dependencies = append(m.Dependencies, dependencyJS{Scope: "chart", Group: d.Repository, Name: d.Name, Requirement: d.Version, Optional: d.Condition != ""})
	}
	return true, nil
}

func (u *UI) pypiMetadata(ctx context.Context, rp *repo.Repository, name, version string, m *componentMetadata) (bool, error) {
	// PyPI rows are per file (Version holds the file name). The core
	// metadata of a file is its PEP 658 sidecar, <file>.metadata, which
	// uploads store and older files get on their first request.
	project := pypi.Normalize(name)
	pkgs, err := u.DB.ListPackages(ctx, rp.Name, project)
	if err != nil {
		return false, err
	}
	found := false
	for _, p := range pkgs {
		var a struct {
			Version string `json:"version"`
		}
		if p.Version != version && !(json.Unmarshal([]byte(p.Attrs), &a) == nil && a.Version == version) {
			continue
		}
		found = true
		b, ok, err := u.readStored(ctx, rp, "packages/"+project+"/"+p.Version+".metadata")
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		fillCoreMetadata(string(b), m)
		m.Collected = true
		break // one file's metadata describes the release
	}
	return found, nil
}

// fillCoreMetadata reads Python core metadata (RFC 822 headers up to the
// first blank line): Summary, License, Home-page, Project-URL and
// Requires-Dist. Extras markers are kept in the requirement as declared.
func fillCoreMetadata(doc string, m *componentMetadata) {
	head, _, _ := strings.Cut(doc, "\n\n")
	var repoURL string
	for _, line := range strings.Split(strings.ReplaceAll(head, "\r\n", "\n"), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "summary":
			m.Description = optStr(v)
		case "license", "license-expression":
			if m.License == nil || k == "license-expression" {
				m.License = optStr(v)
			}
		case "home-page":
			m.Homepage = optStr(v)
		case "project-url":
			label, url, ok := strings.Cut(v, ",")
			if ok && repoURL == "" && strings.Contains(strings.ToLower(label), "source") || ok && strings.Contains(strings.ToLower(label), "repository") {
				repoURL = strings.TrimSpace(url)
			}
			if m.Homepage == nil && ok && strings.Contains(strings.ToLower(label), "homepage") {
				m.Homepage = optStr(strings.TrimSpace(url))
			}
		case "requires-dist":
			name, rest := v, ""
			if i := strings.IndexAny(v, " (;<>=!~["); i >= 0 {
				name, rest = v[:i], strings.TrimSpace(v[i:])
			}
			scope := "runtime"
			if strings.Contains(rest, "extra ==") {
				scope = "optional"
			}
			if len(m.Dependencies) < 2000 {
				m.Dependencies = append(m.Dependencies, dependencyJS{Scope: scope, Name: name, Requirement: rest, Optional: scope == "optional"})
			}
		}
	}
	if repoURL != "" {
		m.RepositoryURL = optStr(repoURL)
	}
}

func (u *UI) goMetadata(ctx context.Context, rp *repo.Repository, module, version string, m *componentMetadata) (bool, error) {
	b, ok, err := u.readStored(ctx, rp, golang.Escape(module)+"/@v/"+golang.Escape(version)+".mod")
	if err != nil || !ok {
		return ok, err
	}
	m.Collected = true
	for _, d := range goModRequires(string(b)) {
		m.Dependencies = append(m.Dependencies, dependencyJS{Scope: "runtime", Name: d[0], Requirement: d[1]})
	}
	return true, nil
}

// goModRequires lists the (path, version) pairs of a go.mod's require
// directives, single-line and block forms. Indirect requirements keep
// their comment out; they are listed like the others.
func goModRequires(mod string) [][2]string {
	var out [][2]string
	block := false
	for _, line := range strings.Split(mod, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case line == "":
		case block && line == ")":
			block = false
		case block:
			if f := strings.Fields(line); len(f) == 2 {
				out = append(out, [2]string{f[0], f[1]})
			}
		case line == "require (":
			block = true
		case strings.HasPrefix(line, "require "):
			if f := strings.Fields(line); len(f) == 3 {
				out = append(out, [2]string{f[1], f[2]})
			}
		}
		if len(out) >= 2000 {
			break
		}
	}
	return out
}
