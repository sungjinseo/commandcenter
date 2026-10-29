package ui

import (
	"context"
	"sort"
	"strings"

	"commandcenter/internal/format/cocoapods"
	"commandcenter/internal/format/huggingface"
	"commandcenter/internal/repo"
)

// cocoapodsMetadata reads the cached podspec of a pod version: summary (or
// description), license, homepage, the upstream source URL and the
// dependencies map (scope "dependencies").
func (u *UI) cocoapodsMetadata(ctx context.Context, rp *repo.Repository, name, version string, m *componentMetadata) (bool, error) {
	raw, ok, err := u.readStored(ctx, rp, cocoapods.SpecPath(name, version))
	if err != nil || !ok {
		return ok, err
	}
	pm, err := cocoapods.ParseMetadata(raw)
	if err != nil {
		return true, nil
	}
	desc := pm.Summary
	if desc == "" {
		desc = pm.Description
	}
	m.Collected = true
	m.Description, m.License, m.Homepage, m.RepositoryURL = optStr(desc), optStr(pm.License), optStr(pm.Homepage), optStr(pm.Source)
	names := make([]string, 0, len(pm.Dependencies))
	for n := range pm.Dependencies {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		m.Dependencies = append(m.Dependencies, dependencyJS{Scope: "dependencies", Name: n, Requirement: strings.Join(pm.Dependencies[n], ", ")})
	}
	return true, nil
}

// huggingfaceMetadata describes a cached commit of a hub repository (the
// group is datasets or spaces, "" for models): the pipeline tag and
// library as the description, the license and the repository's page at
// the hub, from whichever API document of the repository is cached (the
// commit's revision document, the repository document, or main's). The
// component exists when a file of the commit is cached.
func (u *UI) huggingfaceMetadata(ctx context.Context, rp *repo.Repository, kind, name, version string, m *componentMetadata) (bool, error) {
	if kind == "" {
		kind = "models"
	}
	prefix := name + "/resolve/" + version + "/"
	if kind != "models" {
		prefix = kind + "/" + prefix
	}
	files, err := u.DB.ListAssets(ctx, rp.Name, prefix)
	if err != nil || len(files) == 0 {
		return false, err
	}
	page := strings.TrimSuffix(rp.RemoteURL.String(), "/") + "/" + strings.TrimSuffix(prefix, "resolve/"+version+"/")
	m.Homepage = optStr(strings.TrimSuffix(page, "/"))
	doc := "api/" + kind + "/" + name
	for _, p := range []string{doc + "/revision/" + version, doc, doc + "/revision/main"} {
		raw, ok, err := u.readStored(ctx, rp, p)
		if err != nil {
			return true, err
		}
		if !ok {
			continue
		}
		hm, err := huggingface.ParseMetadata(raw)
		if err != nil {
			continue
		}
		m.Collected = true
		desc := hm.PipelineTag
		if hm.Library != "" {
			desc = strings.TrimSpace(desc + " (" + hm.Library + ")")
		}
		m.Description, m.License = optStr(desc), optStr(hm.License)
		break
	}
	return true, nil
}
