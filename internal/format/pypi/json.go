package pypi

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

const (
	metadataType = "text/plain; charset=utf-8" // the PEP 658 sidecar
	// maxJSONDoc caps an upstream /pypi/<project>/json document, which bounds
	// what rewriteJSON holds of it (one value); pypi.org's largest documents
	// are single-digit MiB.
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

// SearchAttributes are what Nexus's search parameters pypi.classifiers,
// pypi.description, pypi.keywords and pypi.summary look at in the core
// metadata document raw, by the parameter's last part.
func SearchAttributes(raw []byte) map[string][]string {
	h := parseCore(raw)
	description := h.fields["description"]
	if h.body != "" {
		description = []string{h.body}
	}
	return map[string][]string{"classifiers": h.fields["classifier"], "description": description, "keywords": h.fields["keywords"], "summary": h.fields["summary"]}
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
		"has_sig": attrs.HasSig, "comment_text": "", "yanked": false, "yanked_reason": nil,
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

// jsonDoc returns what writes the /pypi/<project>/json (version "") or
// /pypi/<project>/<version>/json document of project, answered by owner
// (see owner) with file URLs of serving. res.Asset.LastModified is the
// document's time when known (a proxy's cached copy).
func (h *Handler) jsonDoc(ctx context.Context, serving, owner *repo.Repository, project, version string) (write func(io.Writer) error, res proxy.Result, err error) {
	switch owner.Type {
	case "hosted":
		write, err := h.hostedJSON(ctx, serving, owner, project, version)
		return write, proxy.Result{}, err
	case "proxy":
		return h.proxyJSON(ctx, serving, owner, project, version)
	}
	return nil, proxy.Result{}, repo.Errorf(http.StatusInternalServerError, "unknown repository type %q", owner.Type)
}

// hostedJSON returns what writes the document from the package rows, the
// assets and the core metadata of one file (a wheel when there is one) of
// the release: the one asked for, else the newest (PEP 440) release, final
// releases before pre-releases as warehouse does. The rows are read a batch
// at a time, once for the releases' files and again as they are written:
// 16 clients reading the document of 1500 releases of three files each took
// a 128 MiB server down when each request held it whole.
func (h *Handler) hostedJSON(ctx context.Context, serving, owner *repo.Repository, project, version string) (func(io.Writer) error, error) {
	pkgs, err := h.Store.DB.ListPackageVersions(ctx, owner.Name, project)
	if err != nil {
		return nil, err
	}
	if len(pkgs) == 0 {
		return nil, repo.ErrNotFound
	}
	names := make([]string, len(pkgs))
	for i, p := range pkgs {
		names[i] = p.Version // a file name
	}
	files := map[string][]string{} // a release's file names, in row order
	err = h.Store.DB.EachPackage(ctx, owner.Name, project, names, func(p meta.Package) error {
		a, err := rowAttrs(project, p)
		files[a.Version] = append(files[a.Version], p.Version)
		return err
	})
	if err != nil {
		return nil, err
	}
	versions := make([]string, 0, len(files))
	for v := range files {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	asked := version
	if asked == "" {
		version = latestVersion(versions)
	} else if _, ok := files[asked]; !ok { // the client's spelling may differ from the stored one
		version = ""
		for _, v := range versions {
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
		if strings.HasSuffix(strings.ToLower(f), ".whl") {
			chosen = f
			break
		}
	}
	var attrs fileAttrs
	err = h.Store.DB.EachPackage(ctx, owner.Name, project, []string{chosen}, func(p meta.Package) (err error) {
		attrs, err = rowAttrs(project, p)
		return err
	})
	if err != nil {
		return nil, err
	}
	raw, err := h.coreMetadata(ctx, owner, project, attrs.Filename)
	if err != nil && repo.StatusOf(err) != http.StatusNotFound { // no readable metadata: the files, without info fields
		return nil, err
	}
	inf := info(parseCore(raw))
	if raw == nil {
		inf["name"] = project
	}
	inf["version"] = version
	inf["requires_python"] = nullable(attrs.RequiresPython)
	infJSON, err := json.Marshal(inf)
	if err != nil {
		return nil, err
	}
	return func(w io.Writer) error {
		// The keys in the order json.Marshal gives a map's.
		fmt.Fprintf(w, `{"info":%s,"last_serial":0`, infJSON)
		if asked == "" {
			// Every release's files in one walk, so a batch holds the files
			// of many releases.
			var all []string
			for _, v := range versions {
				all = append(all, files[v]...)
			}
			io.WriteString(w, `,"releases":{`)
			cur := ""
			err := h.eachFile(ctx, serving, owner, project, all, func(v string, entry []byte) error {
				if v != cur {
					if cur != "" {
						io.WriteString(w, "],")
					}
					k, _ := json.Marshal(v)
					w.Write(append(k, ':', '['))
					cur = v
				} else {
					io.WriteString(w, ",")
				}
				_, err := w.Write(entry)
				return err
			})
			if err != nil {
				return err
			}
			if cur != "" {
				io.WriteString(w, "]")
			}
			io.WriteString(w, "}")
		}
		io.WriteString(w, `,"urls":[`)
		sep := ""
		err := h.eachFile(ctx, serving, owner, project, files[version], func(_ string, entry []byte) error {
			io.WriteString(w, sep)
			sep = ","
			_, err := w.Write(entry)
			return err
		})
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, `],"vulnerabilities":[]}`)
		return err
	}, nil
}

// rowAttrs reads a hosted row's attrs; a row without a version has its file
// name's.
func rowAttrs(project string, p meta.Package) (fileAttrs, error) {
	var a fileAttrs
	if err := json.Unmarshal([]byte(p.Attrs), &a); err != nil {
		return a, err
	}
	if a.Version == "" {
		a.Version, _ = fileVersion(project, a.Filename)
	}
	return a, nil
}

// eachFile calls fn with the version and the JSON API entry of each of the
// named files of hosted rp, in that order, reading their rows and assets a
// batch at a time.
func (h *Handler) eachFile(ctx context.Context, serving, rp *repo.Repository, project string, names []string, fn func(version string, entry []byte) error) error {
	for len(names) > 0 {
		batch := names[:min(len(names), 64)]
		names = names[len(batch):]
		paths := make([]string, len(batch))
		for i, name := range batch {
			paths[i] = fileKey(project, name)
		}
		assets, err := h.Store.DB.AssetsIn(ctx, rp.Name, paths)
		if err != nil {
			return err
		}
		err = h.Store.DB.EachPackage(ctx, rp.Name, project, batch, func(p meta.Package) error {
			a, err := rowAttrs(project, p)
			if err != nil {
				return err
			}
			asset := assets[fileKey(project, a.Filename)]
			if asset.CreatedAt.IsZero() {
				asset.CreatedAt = p.CreatedAt
			}
			b, err := json.Marshal(fileDoc(h.jsonFileURL(serving, project, a.Filename), a, asset))
			if err != nil {
				return err
			}
			return fn(a.Version, b)
		})
		if err != nil {
			return err
		}
	}
	return nil
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
func (h *Handler) proxyJSON(ctx context.Context, serving, owner *repo.Repository, project, version string) (func(io.Writer) error, proxy.Result, error) {
	remote := "pypi/" + project + "/json"
	if version != "" {
		remote = "pypi/" + project + "/" + version + "/json"
	}
	t := proxy.Target{Key: remote, Remote: remote, Header: http.Header{"Accept": {"application/json"}}, NotHTML: true, Check: proxy.JSONDocument("info")}
	res, err := h.Fetcher.FetchTarget(ctx, owner, t, proxy.Metadata)
	if err != nil {
		return nil, res, err
	}
	if res.Asset.Size > maxJSONDoc {
		return nil, res, repo.Errorf(http.StatusBadGateway, "upstream document for %q is over %d MiB", project, maxJSONDoc>>20)
	}
	write := func(w io.Writer) error {
		f, err := h.Store.Open(res.Asset)
		if err != nil {
			return err
		}
		defer f.Close()
		err = rewriteJSON(f, w, func(file string) string { return h.jsonFileURL(serving, project, file) })
		if _, ok := err.(writeError); err != nil && !ok {
			err = repo.Errorf(http.StatusBadGateway, "upstream document for %q is not a PyPI JSON document: %v", project, err)
		}
		return err
	}
	return write, res, nil
}

// writeError is a failed write to the client, not a fault of the document.
type writeError struct{ error }

// rewriteJSON copies the document r to w with the "url" of every file in
// "urls" and "releases" replaced by fileURL(filename); everything else is
// kept as is. It holds one file at a time (another top-level value whole):
// 16 clients reading a document of botocore's size (3.9 MB) at once took a
// 128 MiB server down when each request held it decoded.
func rewriteJSON(r io.Reader, w io.Writer, fileURL func(string) string) error {
	dec := json.NewDecoder(r)
	put := func(b []byte) error {
		if _, err := w.Write(b); err != nil {
			return writeError{err}
		}
		return nil
	}
	putKey := func(first bool, k string) error {
		b, _ := json.Marshal(k)
		if !first {
			b = append([]byte{','}, b...)
		}
		return put(append(b, ':'))
	}
	// open reads the opening delimiter of an object or array, or a null,
	// which it copies and reports with false.
	open := func(d json.Delim) (bool, error) {
		tok, err := dec.Token()
		switch {
		case err != nil:
			return false, err
		case tok == nil:
			return false, put([]byte("null"))
		case tok != d:
			return false, fmt.Errorf("%v where %v was expected", tok, d)
		}
		return true, put([]byte{byte(d)})
	}
	closeWith := func(d json.Delim) error {
		if _, err := dec.Token(); err != nil {
			return err
		}
		return put([]byte{byte(d)})
	}
	files := func() error {
		if ok, err := open('['); !ok || err != nil {
			return err
		}
		for first := true; dec.More(); first = false {
			var f map[string]json.RawMessage
			if err := dec.Decode(&f); err != nil {
				return err
			}
			var name string
			if err := json.Unmarshal(f["filename"], &name); err != nil {
				return err
			}
			f["url"], _ = json.Marshal(fileURL(name))
			b, err := json.Marshal(f)
			if err != nil {
				return err
			}
			if !first {
				b = append([]byte{','}, b...)
			}
			if err := put(b); err != nil {
				return err
			}
		}
		return closeWith(']')
	}
	if ok, err := open('{'); !ok || err != nil {
		return cmp.Or(err, errors.New("null"))
	}
	for first := true; dec.More(); first = false {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		if err := putKey(first, key); err != nil {
			return err
		}
		switch key {
		case "urls":
			err = files()
		case "releases":
			var ok bool
			if ok, err = open('{'); !ok || err != nil {
				break
			}
			for first := true; dec.More() && err == nil; first = false {
				if tok, err = dec.Token(); err == nil {
					v, _ := tok.(string)
					if err = putKey(first, v); err == nil {
						err = files()
					}
				}
			}
			if err == nil {
				err = closeWith('}')
			}
		default:
			var raw json.RawMessage
			if err = dec.Decode(&raw); err == nil {
				err = put(raw)
			}
		}
		if err != nil {
			return err
		}
	}
	if err := closeWith('}'); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("data after the document")
	}
	return nil
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
	unlock, err := h.Store.Lock(ctx, "pypi\x00"+rp.Name+"\x00"+project)
	if err != nil {
		return meta.Asset{}, err
	}
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
	return editRow(ctx, tx, repoName, project, filename, func(a *fileAttrs) { a.MetadataSHA256 = sha })
}

// editRow changes the attrs of filename's row in tx; a file without a row
// changes nothing.
func editRow(ctx context.Context, tx *meta.Tx, repoName, project, filename string, edit func(*fileAttrs)) error {
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
		edit(&attrs)
		doc, err := json.Marshal(attrs)
		if err != nil {
			return err
		}
		return tx.UpdatePackage(ctx, repoName, project, filename, string(doc))
	}
	return nil
}
