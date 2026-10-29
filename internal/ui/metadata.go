package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"commandcenter/internal/auth"
	"commandcenter/internal/format/conan"
	"commandcenter/internal/format/gitlfs"
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
	Maintainer    *string        `json:"maintainer,omitempty"` // apt only: the control file's Maintainer
	BugReports    *string        `json:"bugReports,omitempty"` // r only: DESCRIPTION's BugReports
	Collected     bool           `json:"collected"`
	Dependencies  []dependencyJS `json:"dependencies"`
	Resolved      bool           `json:"resolved"`
	Size          *int64         `json:"size,omitempty"`   // gitlfs only: the object's size
	SHA256        *string        `json:"sha256,omitempty"` // gitlfs only: the object's oid
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
	if name == "" || (version == "" && rp.Format != "gitlfs") || (rp.Format == "maven" && group == "") {
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
	case "yum":
		found, err = u.yumMetadata(r.Context(), rp, name, version, &m)
	case "nuget":
		found, err = u.nugetMetadata(r.Context(), rp, name, version, &m)
	case "apt":
		found, err = u.aptMetadata(r.Context(), rp, group, name, version, &m)
	case "composer":
		found, err = u.composerMetadata(r.Context(), rp, name, version, &m)
	case "conda":
		found, err = u.condaMetadata(r.Context(), rp, group, name, version, &m)
	case "rubygems":
		found, err = u.rubygemsMetadata(r.Context(), rp, name, version, &m)
	case "gitlfs": // an object has no manifest: its size and sha256 (the oid) are all there is
		if gitlfs.ValidOID(name) {
			var a meta.Asset
			a, err = u.DB.GetAsset(r.Context(), rp.Name, gitlfs.ObjectPath(name))
			if found = err == nil; found {
				m.Collected, m.Size, m.SHA256 = true, &a.Size, &a.BlobSHA256
			} else if errors.Is(err, meta.ErrNotFound) {
				err = nil
			}
		}
	case "r":
		found, err = u.rMetadata(r.Context(), rp, group, name, version, &m)
	case "conan":
		found, err = u.conanMetadata(r.Context(), rp, group, name, version, &m)
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

// yumMetadata reads the parsed RPM header a yum hosted row holds: summary
// and description, license, url, and the requires and provides as
// dependencies (scopes "requires" and "provides").
func (u *UI) yumMetadata(ctx context.Context, rp *repo.Repository, name, version string, m *componentMetadata) (bool, error) {
	p, ok, err := u.storedPackage(ctx, rp, name, version)
	if err != nil || !ok {
		return ok, err
	}
	var e struct {
		Summary, Description, License, URL string
		Requires, Provides                 []struct {
			Name, Flags, Epoch, Ver, Rel string
			Pre                          bool
		}
	}
	if err := json.Unmarshal([]byte(p.Attrs), &e); err != nil {
		return true, nil
	}
	m.Collected = true
	desc := e.Summary
	if e.Description != "" && e.Description != e.Summary {
		desc = e.Summary + "\n\n" + e.Description
	}
	m.Description, m.License, m.Homepage = optStr(desc), optStr(e.License), optStr(e.URL)
	for _, scope := range []string{"requires", "provides"} {
		deps := e.Requires
		if scope == "provides" {
			deps = e.Provides
		}
		for _, d := range deps {
			req := ""
			if d.Flags != "" {
				req = map[string]string{"EQ": "= ", "LT": "< ", "GT": "> ", "LE": "<= ", "GE": ">= "}[d.Flags] + d.Ver
				if d.Epoch != "" && d.Epoch != "0" {
					req = req[:strings.Index(req, " ")+1] + d.Epoch + ":" + d.Ver
				}
				if d.Rel != "" {
					req += "-" + d.Rel
				}
			}
			m.Dependencies = append(m.Dependencies, dependencyJS{Scope: scope, Name: d.Name, Requirement: req, Optional: false})
		}
	}
	return true, nil
}

// aptMetadata reads the control fields an apt hosted row holds (the
// group is the architecture): Description's first line, Maintainer,
// Homepage, and Depends as "depends" dependencies; Pre-Depends, Recommends
// and Suggests follow with their own scopes. Debian control has no license
// field.
func (u *UI) aptMetadata(ctx context.Context, rp *repo.Repository, arch, name, version string, m *componentMetadata) (bool, error) {
	if arch == "" {
		return false, nil
	}
	p, ok, err := u.storedPackage(ctx, rp, name, version+"_"+arch)
	if err != nil || !ok {
		return ok, err
	}
	var row struct {
		Fields [][2]string `json:"fields"`
	}
	if err := json.Unmarshal([]byte(p.Attrs), &row); err != nil {
		return true, nil
	}
	m.Collected = true
	field := func(name string) string {
		for _, f := range row.Fields {
			if strings.EqualFold(f[0], name) {
				return f[1]
			}
		}
		return ""
	}
	desc, _, _ := strings.Cut(field("Description"), "\n")
	m.Description, m.Homepage, m.Maintainer = optStr(desc), optStr(field("Homepage")), optStr(field("Maintainer"))
	for _, scope := range []string{"depends", "pre-depends", "recommends", "suggests"} {
		for _, alt := range strings.Split(field(scope), ",") {
			alt = strings.TrimSpace(alt)
			if alt == "" {
				continue
			}
			// "a (>= 1.0) | b": the alternatives stay in one entry, the
			// first package named, the rest as the requirement
			pkg, req, _ := strings.Cut(alt, " ")
			m.Dependencies = append(m.Dependencies, dependencyJS{Scope: scope, Name: pkg, Requirement: strings.TrimSpace(req), Optional: scope != "depends" && scope != "pre-depends"})
		}
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
// first blank line, folded lines continuing a field) with the pypi
// format's own parser: Summary, License, Home-page, Project-URL and
// Requires-Dist. Extras markers are kept in the requirement as declared.
func fillCoreMetadata(doc string, m *componentMetadata) {
	f := pypi.CoreFields([]byte(doc))
	last := func(k string) string {
		if v := f[k]; len(v) > 0 {
			return v[len(v)-1]
		}
		return ""
	}
	m.Description = optStr(last("summary"))
	if v := last("license-expression"); v != "" {
		m.License = optStr(v)
	} else {
		m.License = optStr(last("license"))
	}
	m.Homepage = optStr(last("home-page"))
	var repoURL string
	for _, v := range f["project-url"] {
		label, url, ok := strings.Cut(v, ",")
		if ok && repoURL == "" && strings.Contains(strings.ToLower(label), "source") || ok && strings.Contains(strings.ToLower(label), "repository") {
			repoURL = strings.TrimSpace(url)
		}
		if m.Homepage == nil && ok && strings.Contains(strings.ToLower(label), "homepage") {
			m.Homepage = optStr(strings.TrimSpace(url))
		}
	}
	for _, v := range f["requires-dist"] {
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

// nugetMetadata reads what a nuget hosted row holds of the .nuspec:
// description, license (expression, else URL), projectUrl, repository and
// the dependency groups, whose scope is the target framework.
func (u *UI) nugetMetadata(ctx context.Context, rp *repo.Repository, name, version string, m *componentMetadata) (bool, error) {
	p, ok, err := u.storedPackage(ctx, rp, strings.ToLower(name), strings.ToLower(version))
	if err != nil || !ok {
		return ok, err
	}
	var a struct {
		Description   string `json:"description"`
		License       string `json:"license"`
		LicenseURL    string `json:"licenseUrl"`
		ProjectURL    string `json:"projectUrl"`
		RepositoryURL string `json:"repositoryUrl"`
		Groups        []struct {
			TargetFramework string `json:"targetFramework"`
			Dependencies    []struct {
				ID    string `json:"id"`
				Range string `json:"range"`
			} `json:"dependencies"`
		} `json:"dependencyGroups"`
	}
	if err := json.Unmarshal([]byte(p.Attrs), &a); err != nil {
		return true, nil
	}
	m.Collected = true
	m.Description, m.Homepage, m.RepositoryURL = optStr(a.Description), optStr(a.ProjectURL), optStr(a.RepositoryURL)
	if a.License != "" {
		m.License = optStr(a.License)
	} else {
		m.License = optStr(a.LicenseURL)
	}
	for _, g := range a.Groups {
		scope := g.TargetFramework
		if scope == "" {
			scope = "any"
		}
		for _, d := range g.Dependencies {
			m.Dependencies = append(m.Dependencies, dependencyJS{Scope: scope, Name: d.ID, Requirement: d.Range})
		}
	}
	return true, nil
}

// composerMetadata reads what a composer hosted row holds of composer.json:
// description, license (one or several), homepage, support.source and the
// require (scope "require") and require-dev (scope "require-dev",
// optional) links.
func (u *UI) composerMetadata(ctx context.Context, rp *repo.Repository, name, version string, m *componentMetadata) (bool, error) {
	p, ok, err := u.storedPackage(ctx, rp, strings.ToLower(name), version)
	if err != nil || !ok {
		return ok, err
	}
	var a struct {
		Description string            `json:"description"`
		License     any               `json:"license"`
		Homepage    string            `json:"homepage"`
		Support     map[string]string `json:"support"`
		Require     map[string]string `json:"require"`
		RequireDev  map[string]string `json:"require-dev"`
	}
	if err := json.Unmarshal([]byte(p.Attrs), &a); err != nil {
		return true, nil
	}
	m.Collected = true
	m.Description, m.Homepage, m.RepositoryURL = optStr(a.Description), optStr(a.Homepage), optStr(a.Support["source"])
	switch l := a.License.(type) {
	case string:
		m.License = optStr(l)
	case []any:
		var names []string
		for _, x := range l {
			if s, ok := x.(string); ok {
				names = append(names, s)
			}
		}
		m.License = optStr(strings.Join(names, ", "))
	}
	for _, s := range []struct {
		scope string
		links map[string]string
		opt   bool
	}{{"require", a.Require, false}, {"require-dev", a.RequireDev, true}} {
		names := make([]string, 0, len(s.links))
		for n := range s.links {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			m.Dependencies = append(m.Dependencies, dependencyJS{Scope: s.scope, Name: n, Requirement: s.links[n], Optional: s.opt})
		}
	}
	return true, nil
}

// rubygemsMetadata reads what a rubygems hosted row holds of the gem's
// specification: summary and description, licenses, homepage (and the
// source_code_uri of its metadata) and the dependencies, whose scope is
// their type (runtime or development).
func (u *UI) rubygemsMetadata(ctx context.Context, rp *repo.Repository, name, version string, m *componentMetadata) (bool, error) {
	p, ok, err := u.storedPackage(ctx, rp, name, version)
	if err != nil || !ok {
		return ok, err
	}
	var a struct {
		Summary      string            `json:"summary"`
		Description  string            `json:"description"`
		Homepage     string            `json:"homepage"`
		Licenses     []string          `json:"licenses"`
		Metadata     map[string]string `json:"metadata"`
		Dependencies []struct {
			Name, Requirement, Type string
		} `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(p.Attrs), &a); err != nil {
		return true, nil
	}
	m.Collected = true
	desc := a.Description
	if desc == "" {
		desc = a.Summary
	}
	m.Description, m.Homepage = optStr(desc), optStr(a.Homepage)
	m.License, m.RepositoryURL = optStr(strings.Join(a.Licenses, ", ")), optStr(a.Metadata["source_code_uri"])
	for _, d := range a.Dependencies {
		scope := d.Type
		if scope == "" {
			scope = "runtime"
		}
		m.Dependencies = append(m.Dependencies, dependencyJS{Scope: scope, Name: d.Name, Requirement: d.Requirement})
	}
	return true, nil
}

// condaMetadata reads the index.json and about.json a conda hosted row
// holds (the group is the subdir, the version <version>-<build>): summary
// and description, license, home, dev_url, and depends ("depends") and
// constrains ("constrains") as dependencies. A build uploaded as both
// .tar.bz2 and .conda is read from the .tar.bz2, which has the index.
func (u *UI) condaMetadata(ctx context.Context, rp *repo.Repository, subdir, name, version string, m *componentMetadata) (bool, error) {
	if subdir == "" {
		return false, nil
	}
	pkgs, err := u.DB.ListPackages(ctx, rp.Name, name)
	if err != nil {
		return false, err
	}
	type row struct {
		Index map[string]any `json:"index"`
		About map[string]any `json:"about"`
	}
	str := func(m map[string]any, key string) string {
		s, _ := m[key].(string)
		return s
	}
	found := false
	var best row
	for _, p := range pkgs {
		var r row
		if !strings.HasPrefix(p.Version, subdir+"/") || json.Unmarshal([]byte(p.Attrs), &r) != nil || str(r.Index, "version")+"-"+str(r.Index, "build") != version {
			continue
		}
		if !found || strings.HasSuffix(p.Version, ".tar.bz2") {
			best = r
		}
		found = true
	}
	if !found {
		return false, nil
	}
	m.Collected = true
	desc := str(best.About, "summary")
	if d := str(best.About, "description"); d != "" && d != desc {
		desc = strings.TrimSpace(desc + "\n\n" + d)
	}
	m.Description, m.License, m.Homepage, m.RepositoryURL = optStr(desc), optStr(str(best.About, "license")), optStr(str(best.About, "home")), optStr(str(best.About, "dev_url"))
	for _, scope := range []string{"depends", "constrains"} {
		list, _ := best.Index[scope].([]any)
		for _, d := range list {
			spec, _ := d.(string)
			dn, req, _ := strings.Cut(spec, " ")
			m.Dependencies = append(m.Dependencies, dependencyJS{Scope: scope, Name: dn, Requirement: req})
		}
	}
	return true, nil
}

// rMetadata reads the DESCRIPTION fields an r hosted row holds (the group
// is the contrib directory, the row's name <dir>/<pkg>): Title and
// Description, License, URL (the first of a comma-separated list) as the
// homepage, BugReports, and Depends, Imports, LinkingTo and Suggests as
// dependencies with those scopes (Suggests optional). Version
// requirements keep R's spelling without the parentheses: ">= 1.0".
func (u *UI) rMetadata(ctx context.Context, rp *repo.Repository, dir, name, version string, m *componentMetadata) (bool, error) {
	if dir == "" {
		return false, nil
	}
	p, ok, err := u.storedPackage(ctx, rp, dir+"/"+name, version)
	if err != nil || !ok {
		return ok, err
	}
	var row struct {
		Fields [][2]string `json:"fields"`
	}
	if err := json.Unmarshal([]byte(p.Attrs), &row); err != nil {
		return true, nil
	}
	m.Collected = true
	field := func(name string) string {
		for _, f := range row.Fields {
			if strings.EqualFold(f[0], name) {
				return f[1]
			}
		}
		return ""
	}
	desc := field("Title")
	if d := strings.ReplaceAll(field("Description"), "\n", " "); d != "" && d != desc {
		desc = strings.TrimSpace(desc + "\n\n" + d)
	}
	home, _, _ := strings.Cut(field("URL"), ",")
	m.Description, m.License, m.Homepage, m.BugReports = optStr(desc), optStr(field("License")), optStr(strings.TrimSpace(home)), optStr(field("BugReports"))
	for _, scope := range []string{"depends", "imports", "linkingto", "suggests"} {
		for _, dep := range strings.Split(strings.ReplaceAll(field(scope), "\n", " "), ",") {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			// "pkg (>= 1.0)": the requirement is what the parentheses hold
			pkg, req, _ := strings.Cut(dep, "(")
			req = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(req), ")"))
			m.Dependencies = append(m.Dependencies, dependencyJS{Scope: scope, Name: strings.TrimSpace(pkg), Requirement: req, Optional: scope == "suggests"})
		}
	}
	return true, nil
}

// conanMetadata scans the conanfile.py of a recipe revision (the group is
// user/channel, the version <version>#<rrev>) as conan.ParseConanfile
// does: description, license, homepage, url, and the requires,
// tool_requires, build_requires and test_requires references as
// dependencies of those scopes. The stored file counts for hosted and
// proxy repositories alike; a revision without a conanfile.py is not found.
func (u *UI) conanMetadata(ctx context.Context, rp *repo.Repository, group, name, version string, m *componentMetadata) (bool, error) {
	ref, rrev, ok := conan.ComponentRef(group, name, version)
	if !ok {
		return false, nil
	}
	src, ok, err := u.readStored(ctx, rp, conan.RecipeFile(ref, rrev, "conanfile.py"))
	if err != nil || !ok {
		return ok, err
	}
	cf := conan.ParseConanfile(src)
	m.Collected = true
	m.Description, m.License, m.Homepage, m.RepositoryURL = optStr(cf.Description), optStr(cf.License), optStr(cf.Homepage), optStr(cf.URL)
	for _, kind := range conan.RequireKinds {
		for _, dep := range cf.Requires[kind] {
			n, req, _ := strings.Cut(dep, "/")
			m.Dependencies = append(m.Dependencies, dependencyJS{Scope: kind, Name: n, Requirement: req})
		}
	}
	return true, nil
}
