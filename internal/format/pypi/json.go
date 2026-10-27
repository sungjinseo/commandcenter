package pypi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

const (
	metadataType = "text/plain; charset=utf-8" // the PEP 658 sidecar
	// maxJSONDoc caps an upstream /pypi/<project>/json document: rewriteJSON
	// needs ~9x its size in memory (the server's budget is 128 MiB), and
	// pypi.org's largest documents are single-digit MiB.
	maxJSONDoc = 8 << 20
)

// header is a parsed core metadata document: lower-cased field names to
// their values in order, and the body (the description, metadata 2.1+).
type header struct {
	fields map[string][]string
	body   string
}

func (h header) get(k string) string {
	if v := h.fields[k]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// parseCore parses the RFC 822 style core metadata document raw: a folded
// line (leading whitespace) continues the previous field with a newline,
// as the old-style Description is folded; the first blank line ends the
// fields and the rest is the body.
func parseCore(raw []byte) header {
	h := header{fields: map[string][]string{}}
	rest := string(raw)
	var last []string
	for rest != "" {
		line, more, _ := strings.Cut(rest, "\n")
		rest = more
		line = strings.TrimRight(line, "\r")
		if line == "" {
			h.body = rest
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			if last != nil {
				last[len(last)-1] += "\n" + strings.TrimLeft(line, " \t")
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		h.fields[k] = append(h.fields[k], strings.TrimSpace(v))
		last = h.fields[k]
	}
	return h
}

// CoreFields parses the core metadata document raw as parseCore does and
// returns its fields: lower-cased names to their values in order.
func CoreFields(raw []byte) map[string][]string {
	return parseCore(raw).fields
}

// nullable is a string field of the warehouse JSON: null when absent.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// listOrNull is a repeatable field of the warehouse JSON: null when absent.
func listOrNull(v []string) any {
	if len(v) == 0 {
		return nil
	}
	return v
}

// info builds the warehouse "info" object from a core metadata document.
func info(h header) map[string]any {
	urls := map[string]string{}
	for _, pu := range h.fields["project-url"] {
		if label, u, ok := strings.Cut(pu, ","); ok {
			urls[strings.TrimSpace(label)] = strings.TrimSpace(u)
		}
	}
	var projectURLs any
	if len(urls) > 0 {
		projectURLs = urls
	}
	description := h.body
	if description == "" {
		description = h.get("description")
	}
	classifiers := h.fields["classifier"]
	if classifiers == nil {
		classifiers = []string{}
	}
	return map[string]any{
		"name":                     h.get("name"),
		"version":                  h.get("version"),
		"summary":                  nullable(h.get("summary")),
		"description":              nullable(description),
		"description_content_type": nullable(h.get("description-content-type")),
		"author":                   nullable(h.get("author")),
		"author_email":             nullable(h.get("author-email")),
		"maintainer":               nullable(h.get("maintainer")),
		"maintainer_email":         nullable(h.get("maintainer-email")),
		"license":                  nullable(h.get("license")),
		"license_expression":       nullable(h.get("license-expression")),
		"license_files":            listOrNull(h.fields["license-file"]),
		"home_page":                nullable(h.get("home-page")),
		"download_url":             nullable(h.get("download-url")),
		"keywords":                 nullable(h.get("keywords")),
		"platform":                 nullable(h.get("platform")),
		"classifiers":              classifiers,
		"requires_dist":            listOrNull(h.fields["requires-dist"]),
		"requires_python":          nullable(h.get("requires-python")),
		"provides_extra":           listOrNull(h.fields["provides-extra"]),
		"project_urls":             projectURLs,
		"dynamic":                  listOrNull(h.fields["dynamic"]),
		"docs_url":                 nil,
		"bugtrack_url":             nil,
		"yanked":                   false,
		"yanked_reason":            nil,
	}
}

// fileDoc is one entry of the warehouse "urls" and "releases" lists.
func fileDoc(url string, attrs fileAttrs, a meta.Asset) map[string]any {
	digests := map[string]string{"sha256": attrs.SHA256}
	if a.MD5 != "" {
		digests["md5"] = a.MD5
	}
	if attrs.Blake2b256 != "" {
		digests["blake2b_256"] = attrs.Blake2b256
	}
	packagetype, pyver := "sdist", "source"
	if base, ext, _ := splitUploadExt(attrs.Filename); ext == ".whl" {
		packagetype = "bdist_wheel"
		if f := strings.Split(base, "-"); len(f) >= 3 {
			pyver = f[len(f)-3]
		}
	}
	doc := map[string]any{
		"filename": attrs.Filename, "url": url, "digests": digests, "md5_digest": a.MD5, "size": a.Size,
		"packagetype": packagetype, "python_version": pyver, "requires_python": nullable(attrs.RequiresPython),
		"has_sig": false, "comment_text": "", "yanked": false, "yanked_reason": nil,
		"upload_time": nil, "upload_time_iso_8601": nil,
	}
	if !a.CreatedAt.IsZero() {
		t := a.CreatedAt.UTC()
		doc["upload_time"] = t.Format("2006-01-02T15:04:05")
		doc["upload_time_iso_8601"] = t.Format("2006-01-02T15:04:05.000000Z")
	}
	return doc
}

// jsonFileURL is a file's absolute download URL for the JSON API, where
// clients expect a URL they can fetch as is (warehouse gives absolute ones);
// without a baseURL it is the host-relative link of the simple index.
func (h *Handler) jsonFileURL(rp *repo.Repository, project, file string) string {
	if base := h.baseURL(); base != "" {
		return base + "/repository/" + rp.Name + "/packages/" + project + "/" + url.PathEscape(file)
	}
	return h.fileURL(rp, project, file)
}

// jsonDoc builds the /pypi/<project>/json (version "") or
// /pypi/<project>/<version>/json document of project, answered by owner
// (see owner) with file URLs of serving. modTime is the document's time when
// known (a proxy's cached copy).
func (h *Handler) jsonDoc(ctx context.Context, serving, owner *repo.Repository, project, version string) (doc []byte, res proxy.Result, err error) {
	switch owner.Type {
	case "hosted":
		doc, err = h.hostedJSON(ctx, serving, owner, project, version)
		return doc, proxy.Result{}, err
	case "proxy":
		return h.proxyJSON(ctx, serving, owner, project, version)
	}
	return nil, proxy.Result{}, repo.Errorf(http.StatusInternalServerError, "unknown repository type %q", owner.Type)
}

// hostedJSON builds the document from the package rows, the assets and the
// core metadata of one file (a wheel when there is one) of the release:
// the one asked for, else the newest (PEP 440) release, final releases
// before pre-releases as warehouse does.
func (h *Handler) hostedJSON(ctx context.Context, serving, owner *repo.Repository, project, version string) ([]byte, error) {
	pkgs, err := h.Store.DB.ListPackages(ctx, owner.Name, project)
	if err != nil {
		return nil, err
	}
	if len(pkgs) == 0 {
		return nil, repo.ErrNotFound
	}
	assets, err := h.Store.DB.ListAssets(ctx, owner.Name, "packages/"+project+"/")
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]meta.Asset, len(assets))
	for _, a := range assets {
		byPath[a.Path] = a
	}
	releases := map[string][]map[string]any{}
	files := map[string][]fileAttrs{}
	for _, p := range pkgs {
		var a fileAttrs
		if err := json.Unmarshal([]byte(p.Attrs), &a); err != nil {
			return nil, err
		}
		if a.Version == "" { // a row without a version: the file name's
			a.Version, _ = fileVersion(project, a.Filename)
		}
		asset := byPath[fileKey(project, a.Filename)]
		if asset.CreatedAt.IsZero() {
			asset.CreatedAt = p.CreatedAt
		}
		releases[a.Version] = append(releases[a.Version], fileDoc(h.jsonFileURL(serving, project, a.Filename), a, asset))
		files[a.Version] = append(files[a.Version], a)
	}
	asked := version
	if asked == "" {
		versions := make([]string, 0, len(releases))
		for v := range releases {
			versions = append(versions, v)
		}
		version = latestVersion(versions)
	} else if _, ok := releases[asked]; !ok { // the client's spelling may differ from the stored one
		version = ""
		for v := range releases {
			if canonicalVersion(v) == canonicalVersion(asked) {
				version = v
			}
		}
		if version == "" {
			return nil, repo.ErrNotFound
		}
	}
	chosen := files[version][0]
	for _, f := range files[version] {
		if strings.HasSuffix(strings.ToLower(f.Filename), ".whl") {
			chosen = f
			break
		}
	}
	raw, err := h.coreMetadata(ctx, owner, project, chosen.Filename)
	if err != nil && repo.StatusOf(err) != http.StatusNotFound { // no readable metadata: the files, without info fields
		return nil, err
	}
	inf := info(parseCore(raw))
	if raw == nil {
		inf["name"] = project
	}
	inf["version"] = version
	inf["requires_python"] = nullable(chosen.RequiresPython)
	doc := map[string]any{"info": inf, "urls": releases[version], "vulnerabilities": []any{}, "last_serial": 0}
	if asked == "" {
		doc["releases"] = releases
	}
	return json.Marshal(doc)
}

// compareVersion orders two normalized PEP 440 versions (normalizeVersion)
// as packaging does: epoch, release (trailing zeros ignored), then dev-only
// < pre-release < final < post-release, local versions after their public one.
func compareVersion(a, b string) int {
	ka, kb := versionKey(a), versionKey(b)
	for i := range ka {
		if c := compareSeg(ka[i], kb[i]); c != 0 {
			return c
		}
	}
	return 0
}

// seg is one part of a version key: a number or a string (local segments),
// where strings order before numbers as packaging does.
type seg struct {
	n int
	s string
}

func compareSeg(a, b []seg) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		switch {
		case a[i].s != "" || b[i].s != "":
			if a[i].s != b[i].s {
				return strings.Compare(a[i].s, b[i].s)
			}
			if a[i].s == "" {
				return 1
			}
			if b[i].s == "" {
				return -1
			}
		case a[i].n != b[i].n:
			if a[i].n < b[i].n {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

const inf = 1 << 62

// versionKey splits a normalized version into comparable parts: epoch,
// release, pre, post, dev, local.
func versionKey(v string) [6][]seg {
	m := versionRE.FindStringSubmatch(v)
	if m == nil {
		return [6][]seg{{{s: v}}}
	}
	num := func(s string) int { n, _ := strconv.Atoi(s); return n }
	var k [6][]seg
	k[0] = []seg{{n: num(m[1])}}
	rel := strings.Split(m[2], ".")
	for len(rel) > 1 && rel[len(rel)-1] == "0" {
		rel = rel[:len(rel)-1]
	}
	for _, r := range rel {
		k[1] = append(k[1], seg{n: num(r)})
	}
	pre, post, dev := []seg{{n: inf}}, []seg{{n: -inf}}, []seg{{n: inf}}
	if m[3] != "" {
		pre = []seg{{n: map[string]int{"a": 0, "b": 1, "rc": 2}[m[3]]}, {n: num(m[4])}}
	} else if m[6] == "" && m[8] != "" {
		pre = []seg{{n: -inf}} // a dev release of a final version sorts before its pre-releases
	}
	if m[6] != "" {
		post = []seg{{n: num(m[7])}}
	}
	if m[8] != "" {
		dev = []seg{{n: num(m[9])}}
	}
	k[2], k[3], k[4] = pre, post, dev
	k[5] = []seg{{n: -inf}}
	if m[10] != "" {
		k[5] = nil
		for _, p := range strings.Split(m[10], ".") {
			if strings.Trim(p, "0123456789") == "" {
				k[5] = append(k[5], seg{n: num(p)})
			} else {
				k[5] = append(k[5], seg{s: p})
			}
		}
	}
	return k
}

// isPrerelease reports whether a normalized version is a pre- or dev-release.
func isPrerelease(v string) bool {
	m := versionRE.FindStringSubmatch(v)
	return m != nil && (m[3] != "" || m[8] != "")
}

// latestVersion is warehouse's choice for the project document: the newest
// final release, or the newest release when every one is a pre-release.
func latestVersion(versions []string) string {
	best := ""
	for _, v := range versions {
		switch {
		case best == "":
			best = v
		case isPrerelease(best) && !isPrerelease(v):
			best = v
		case isPrerelease(best) == isPrerelease(v) && compareVersion(v, best) > 0:
			best = v
		}
	}
	return best
}

// proxyJSON serves the upstream document from the metadata cache with its
// file URLs rewritten to serving's.
func (h *Handler) proxyJSON(ctx context.Context, serving, owner *repo.Repository, project, version string) ([]byte, proxy.Result, error) {
	remote := "pypi/" + project + "/json"
	if version != "" {
		remote = "pypi/" + project + "/" + version + "/json"
	}
	t := proxy.Target{Key: remote, Remote: remote, Header: http.Header{"Accept": {"application/json"}}, NotHTML: true}
	res, err := h.Fetcher.FetchTarget(ctx, owner, t, proxy.Metadata)
	if err != nil {
		return nil, res, err
	}
	f, err := h.Store.Open(res.Asset)
	if err != nil {
		return nil, res, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxJSONDoc+1))
	if err != nil {
		return nil, res, err
	}
	if len(raw) > maxJSONDoc {
		return nil, res, repo.Errorf(http.StatusBadGateway, "upstream document for %q is over %d MiB", project, maxJSONDoc>>20)
	}
	doc, err := rewriteJSON(raw, func(file string) string { return h.jsonFileURL(serving, project, file) })
	if err != nil {
		return nil, res, repo.Errorf(http.StatusBadGateway, "upstream document for %q is not a PyPI JSON document: %v", project, err)
	}
	return doc, res, nil
}

// rewriteJSON replaces the "url" of every file in "urls" and "releases" with
// fileURL(filename); everything else is kept as is.
func rewriteJSON(raw []byte, fileURL func(string) string) ([]byte, error) {
	var top map[string]json.RawMessage
	err := json.Unmarshal(raw, &top)
	if err != nil {
		return nil, err
	}
	rewrite := func(files []map[string]json.RawMessage) error {
		for _, f := range files {
			var name string
			if err := json.Unmarshal(f["filename"], &name); err != nil {
				return err
			}
			u, err := json.Marshal(fileURL(name))
			if err != nil {
				return err
			}
			f["url"] = u
		}
		return nil
	}
	if r, ok := top["urls"]; ok {
		var files []map[string]json.RawMessage
		if err := json.Unmarshal(r, &files); err != nil {
			return nil, err
		}
		if err := rewrite(files); err != nil {
			return nil, err
		}
		if top["urls"], err = json.Marshal(files); err != nil {
			return nil, err
		}
	}
	if r, ok := top["releases"]; ok {
		var releases map[string][]map[string]json.RawMessage
		if err := json.Unmarshal(r, &releases); err != nil {
			return nil, err
		}
		for _, files := range releases {
			if err := rewrite(files); err != nil {
				return nil, err
			}
		}
		if top["releases"], err = json.Marshal(releases); err != nil {
			return nil, err
		}
	}
	return json.Marshal(top)
}

// coreMetadata returns the core metadata document of a stored file of rp
// (hosted or proxy): its sidecar, extracted first when an older build stored
// the file without one.
func (h *Handler) coreMetadata(ctx context.Context, rp *repo.Repository, project, filename string) ([]byte, error) {
	a, err := h.sidecar(ctx, rp, project, filename)
	if err != nil {
		return nil, err
	}
	f, err := h.Store.Open(a)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxCoreMetadata))
}

// sidecar returns the PEP 658 sidecar asset of a stored file, extracting it
// from the file (see extract) when it is not stored yet.
func (h *Handler) sidecar(ctx context.Context, rp *repo.Repository, project, filename string) (meta.Asset, error) {
	a, err := h.Store.Get(ctx, rp.Name, metadataKey(project, filename))
	if err == nil || !errors.Is(err, repo.ErrNotFound) {
		return a, err
	}
	return h.extract(ctx, rp, project, filename)
}

// extract reads the core metadata out of the stored file and stores it as
// the file's sidecar; a hosted row learns the sidecar's sha256 so the simple
// index can advertise it. A stored file that is no distribution this server
// can read (a proxy's cached .egg, say) has no sidecar (404); a hosted row
// remembers that (noMetadata) so the index stops advertising one.
func (h *Handler) extract(ctx context.Context, rp *repo.Repository, project, filename string) (meta.Asset, error) {
	unlock := h.locks.Lock(rp.Name + "\x00" + project)
	defer unlock()
	if a, err := h.Store.Get(ctx, rp.Name, metadataKey(project, filename)); err == nil || !errors.Is(err, repo.ErrNotFound) {
		return a, err // stored while waiting for the lock
	}
	dist, err := h.Store.Get(ctx, rp.Name, fileKey(project, filename))
	if err != nil {
		return meta.Asset{}, err
	}
	none := func(err error) (meta.Asset, error) {
		if rp.Type == "hosted" {
			if err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error { return markMetadata(ctx, tx, rp.Name, project, filename, noMetadata) }); err != nil {
				return meta.Asset{}, err
			}
		}
		return meta.Asset{}, repo.Errorf(http.StatusNotFound, "no core metadata for %q: %v", filename, err)
	}
	version, ok := fileVersion(project, filename)
	if !ok {
		return none(errors.New("not a release file name"))
	}
	f, err := h.Store.Open(dist)
	if err != nil {
		return meta.Asset{}, err
	}
	defer f.Close()
	_, raw, err := readMetadata(f, dist.Size, filename, project, version)
	if err != nil {
		if repo.StatusOf(err) == http.StatusBadRequest {
			return none(err)
		}
		return meta.Asset{}, err
	}
	sums, err := h.Store.WriteBlob(rp.Name, bytes.NewReader(raw), nil)
	if err != nil {
		return meta.Asset{}, err
	}
	a := h.Store.NewAsset(rp.Name, metadataKey(project, filename), metadataType, sums)
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		if err := tx.PutAsset(ctx, a); err != nil {
			return err
		}
		if rp.Type != "hosted" {
			return nil
		}
		return markMetadata(ctx, tx, rp.Name, project, filename, sums.SHA256)
	})
	return a, err
}

// markMetadata records sha as a hosted row's MetadataSHA256.
func markMetadata(ctx context.Context, tx *meta.Tx, repoName, project, filename, sha string) error {
	pkgs, err := tx.ListPackages(ctx, repoName, project)
	if err != nil {
		return err
	}
	for _, p := range pkgs {
		if p.Version != filename {
			continue
		}
		var attrs fileAttrs
		if err := json.Unmarshal([]byte(p.Attrs), &attrs); err != nil {
			return err
		}
		attrs.MetadataSHA256 = sha
		doc, err := json.Marshal(attrs)
		if err != nil {
			return err
		}
		return tx.UpdatePackage(ctx, repoName, project, filename, string(doc))
	}
	return nil
}
