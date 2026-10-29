package terraform

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

const (
	maxReadme   = 64 << 10 // of a README.md, for the description
	maxManifest = 1 << 20  // a terraform-registry-manifest.json or SHA256SUMS body
	ctZip       = "application/zip"
	ctTgz       = "application/gzip"
)

var (
	defaultProtocols = []string{"5.0"}
	protocolRE       = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
)

func bad(format string, args ...any) error {
	return repo.Errorf(http.StatusBadRequest, format, args...)
}

// moduleAttrs is a module row's attrs: the archive's extension, the
// directory the module is in ("*" when the archive has one top-level
// directory, as `git archive --prefix` makes it) and the first paragraph
// of its README.md.
type moduleAttrs struct {
	Ext         string `json:"ext"`
	Subdir      string `json:"subdir,omitempty"`
	Description string `json:"description,omitempty"`
}

// providerAttrs is a provider row's attrs: the protocol versions of its
// registry manifest (terraform-registry-manifest.json); none until one is
// uploaded, which means defaultProtocols.
type providerAttrs struct {
	Protocols []string `json:"protocols,omitempty"`
}

// rows lists the versions of name in hosted rp, newest first.
func (h *Handler) rows(ctx context.Context, rp *repo.Repository, name string) ([]meta.Package, error) {
	pkgs, err := h.Store.DB.ListPackages(ctx, rp.Name, name)
	if err != nil {
		return nil, err
	}
	versions := make([]string, 0, len(pkgs))
	byVersion := map[string]meta.Package{}
	for _, p := range pkgs {
		versions = append(versions, p.Version)
		byVersion[p.Version] = p
	}
	sortVersions(versions)
	out := make([]meta.Package, 0, len(pkgs))
	for _, v := range versions {
		out = append(out, byVersion[v])
	}
	return out, nil
}

func (h *Handler) hostedModuleVersions(ctx context.Context, rp *repo.Repository, rt Route) (doc, error) {
	rows, err := h.rows(ctx, rp, rt.Name)
	if err != nil {
		return doc{}, err
	}
	if len(rows) == 0 {
		return doc{}, repo.ErrNotFound
	}
	versions := []map[string]string{}
	for _, r := range rows {
		versions = append(versions, map[string]string{"version": r.Version})
	}
	return marshal(map[string]any{"modules": []map[string]any{{"source": rt.Name, "versions": versions}}})
}

// moduleRow is the row of one module version with its attrs.
func (h *Handler) moduleRow(ctx context.Context, rp *repo.Repository, rt Route) (moduleAttrs, error) {
	pkgs, err := h.Store.DB.ListPackages(ctx, rp.Name, rt.Name)
	if err != nil {
		return moduleAttrs{}, err
	}
	for _, p := range pkgs {
		if p.Version == rt.Version {
			a := moduleAttrs{Ext: "tgz"}
			json.Unmarshal([]byte(p.Attrs), &a)
			return a, nil
		}
	}
	return moduleAttrs{}, repo.ErrNotFound
}

// platforms lists the platforms a provider version has a zip for.
func (h *Handler) platforms(ctx context.Context, rp *repo.Repository, name, version string) ([]map[string]string, error) {
	assets, err := h.Store.DB.ListAssets(ctx, rp.Name, providerDir(name, version))
	if err != nil {
		return nil, err
	}
	out := []map[string]string{}
	for _, a := range assets {
		if os, arch, ok := platformOf(path.Base(a.Path)); ok {
			out = append(out, map[string]string{"os": os, "arch": arch})
		}
	}
	return out, nil
}

func protocolsOf(p meta.Package) []string {
	var a providerAttrs
	if json.Unmarshal([]byte(p.Attrs), &a) != nil || len(a.Protocols) == 0 {
		return defaultProtocols
	}
	return a.Protocols
}

func (h *Handler) hostedProviderVersions(ctx context.Context, rp *repo.Repository, rt Route) (doc, error) {
	rows, err := h.rows(ctx, rp, rt.Name)
	if err != nil {
		return doc{}, err
	}
	if len(rows) == 0 {
		return doc{}, repo.ErrNotFound
	}
	versions := []map[string]any{}
	for _, r := range rows {
		plats, err := h.platforms(ctx, rp, rt.Name, r.Version)
		if err != nil {
			return doc{}, err
		}
		versions = append(versions, map[string]any{"version": r.Version, "protocols": protocolsOf(r), "platforms": plats})
	}
	return marshal(map[string]any{"versions": versions})
}

func (h *Handler) hostedProviderDownload(ctx context.Context, serving, rp *repo.Repository, rt Route) (doc, error) {
	pkgs, err := h.Store.DB.ListPackages(ctx, rp.Name, rt.Name)
	if err != nil {
		return doc{}, err
	}
	var row *meta.Package
	for i := range pkgs {
		if pkgs[i].Version == rt.Version {
			row = &pkgs[i]
		}
	}
	if row == nil {
		return doc{}, repo.ErrNotFound
	}
	file := providerFile(rt.Name, rt.Version, rt.OS, rt.Arch)
	a, err := h.Store.Get(ctx, rp.Name, providerDir(rt.Name, rt.Version)+file)
	if err != nil {
		return doc{}, err
	}
	base := h.repoURL(serving) + "/" + providerDir(rt.Name, rt.Version)
	sums := providerFile(rt.Name, rt.Version, "", "")
	return marshal(map[string]any{
		"protocols": protocolsOf(*row), "os": rt.OS, "arch": rt.Arch, "filename": file,
		"download_url": base + file, "shasums_url": base + sums, "shasums_signature_url": base + sums + ".sig",
		"shasum": a.BlobSHA256, "signing_keys": signingKeys(rp),
	})
}

// upload takes a module archive (`PUT v1/modules/<ns>/<name>/<provider>/
// <version>`, or the path form `PUT modules/<ns>/<name>/<provider>/
// <version>.tgz`) or one part of a provider release (`PUT v1/providers/
// <ns>/<type>/<version>/<os>/<arch>` with the platform zip, `.../shasums`
// with the SHA256SUMS file, `.../shasums.sig` with its signature,
// `.../manifest` with terraform-registry-manifest.json) into hosted rp.
// The server's maxUploadMiB bounds the body. The same file again changes
// nothing; another one where a file exists is a 409 unless writePolicy is
// allow, when replacing it needs Edit. Answers 201.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	if rt.Kind == KindProviderUpload && rt.Part == "manifest" {
		return h.manifest(w, r, rp, rt)
	}
	sums, err := h.Store.WriteBlob(rp.Name, r.Body, nil)
	if err != nil {
		return uploadErr(err)
	}
	if sums.Size == 0 {
		return bad("empty upload")
	}
	var key, ct string
	var attrs string
	switch {
	case rt.Kind == KindModuleUpload:
		a, err := h.readModule(rp, sums)
		if err != nil {
			return err
		}
		if rt.Ext != "" && rt.Ext != a.Ext { // the path form names the format
			return bad("the archive is a %s, not a %s", a.Ext, rt.Ext)
		}
		key, ct = modulePath(rt.Name, rt.Version, a.Ext), ctTgz
		if a.Ext == "zip" {
			ct = ctZip
		}
		b, _ := json.Marshal(a)
		attrs = string(b)
	case rt.Part == "zip":
		if err := h.checkMagic(rp, sums, "PK\x03\x04"); err != nil {
			return err
		}
		key, ct = providerDir(rt.Name, rt.Version)+providerFile(rt.Name, rt.Version, rt.OS, rt.Arch), ctZip
	case rt.Part == "shasums":
		if err := h.checkSums(rp, sums); err != nil {
			return err
		}
		key, ct = providerDir(rt.Name, rt.Version)+providerFile(rt.Name, rt.Version, "", ""), "text/plain"
	case rt.Part == "sig":
		key, ct = providerDir(rt.Name, rt.Version)+providerFile(rt.Name, rt.Version, "", "")+".sig", "application/pgp-signature"
	}
	if attrs == "" {
		attrs = "{}" // a provider's file: the manifest sets the protocols
	}
	// What a re-upload replaces: a module's archive of either extension,
	// a provider's one file.
	module := rt.Kind == KindModuleUpload
	if err := h.write(r, rp, rt, attrs, module, func(tx *meta.Tx) error {
		ctx := r.Context()
		var have []meta.Asset
		if module {
			var err error
			if have, err = tx.ListAssets(ctx, rp.Name, moduleStore+rt.Name+"/"+rt.Version+"."); err != nil {
				return err
			}
		} else if a, err := tx.GetAsset(ctx, rp.Name, key); err == nil {
			have = append(have, a)
		} else if !errors.Is(err, meta.ErrNotFound) {
			return err
		}
		switch {
		case len(have) == 1 && have[0].Path == key && have[0].BlobSHA256 == sums.SHA256:
			return nil // the same file again (a retried release): nothing changes
		case len(have) > 0 && rp.WritePolicy != "allow":
			return repo.Errorf(http.StatusConflict, "%s %s already exists in repository %q", rt.Name, rt.Version, rp.Name)
		case len(have) > 0 && !h.Auth.Permits(ctx, rp.Name, auth.Edit):
			return repo.Errorf(http.StatusForbidden, "user %q may not overwrite %s %s", auth.FromContext(ctx).User, rt.Name, rt.Version)
		}
		for _, a := range have {
			if a.Path != key {
				if _, err := tx.DeleteAsset(ctx, rp.Name, a.Path); err != nil {
					return err
				}
			}
		}
		return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, key, ct, sums))
	}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusCreated)
	return nil
}

// manifest stores a provider version's protocol versions from its
// terraform-registry-manifest.json ({"version":1,"metadata":
// {"protocol_versions":["5.0"]}}), creating the row when needed. Other
// protocols than an earlier manifest's are an overwrite (writePolicy).
func (h *Handler) manifest(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxManifest+1))
	if err != nil {
		return uploadErr(err)
	}
	if len(raw) > maxManifest {
		return bad("manifest is larger than %d bytes", maxManifest)
	}
	var m struct {
		Metadata struct {
			Protocols []string `json:"protocol_versions"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return bad("manifest is not JSON: %v", err)
	}
	if len(m.Metadata.Protocols) == 0 {
		return bad("manifest has no metadata.protocol_versions")
	}
	for _, p := range m.Metadata.Protocols {
		if !protocolRE.MatchString(p) {
			return bad("protocol version %q is not <major>.<minor>", p)
		}
	}
	b, _ := json.Marshal(providerAttrs{Protocols: m.Metadata.Protocols})
	if err := h.write(r, rp, rt, string(b), false, func(tx *meta.Tx) error {
		ctx := r.Context()
		pkgs, err := tx.ListPackages(ctx, rp.Name, rt.Name)
		if err != nil {
			return err
		}
		var cur providerAttrs
		for _, p := range pkgs {
			if p.Version == rt.Version {
				json.Unmarshal([]byte(p.Attrs), &cur)
			}
		}
		switch {
		case slices.Equal(cur.Protocols, m.Metadata.Protocols):
			return nil // inserted just now, or the same manifest again
		case len(cur.Protocols) == 0: // the row of a file upload
		case rp.WritePolicy != "allow":
			return repo.Errorf(http.StatusConflict, "%s %s already has a manifest in repository %q", rt.Name, rt.Version, rp.Name)
		case !h.Auth.Permits(ctx, rp.Name, auth.Edit):
			return repo.Errorf(http.StatusForbidden, "user %q may not overwrite the manifest of %s %s", auth.FromContext(ctx).User, rt.Name, rt.Version)
		}
		return tx.UpdatePackage(ctx, rp.Name, rt.Name, rt.Version, string(b))
	}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusCreated)
	return nil
}

// write runs fn in a transaction under the module's or provider's lock,
// after inserting the version's row with attrs when it has none (or
// updating its attrs when update is set), then emits the upload event.
func (h *Handler) write(r *http.Request, rp *repo.Repository, rt Route, attrs string, update bool, fn func(*meta.Tx) error) error {
	unlock, err := h.Store.Lock(r.Context(), "terraform\x00"+rp.Name+"\x00"+rt.Name)
	if err != nil {
		return err
	}
	defer unlock()
	ctx := r.Context()
	now := h.Now().UTC()
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		row := meta.Package{Repo: rp.Name, Format: "terraform", Name: rt.Name, Version: rt.Version, Attrs: attrs, CreatedAt: now}
		switch err := tx.InsertPackage(ctx, row); {
		case errors.Is(err, meta.ErrExists) && update:
			if err := tx.UpdatePackage(ctx, rp.Name, rt.Name, rt.Version, attrs); err != nil {
				return err
			}
		case err != nil && !errors.Is(err, meta.ErrExists):
			return err
		}
		return fn(tx)
	})
	if err != nil {
		return err
	}
	h.Store.Emit(r, "upload", rp.Name, rt.Name+"@"+rt.Version, rt.Part)
	return nil
}

// checkMagic refuses a stored upload that does not start with magic.
func (h *Handler) checkMagic(rp *repo.Repository, sums blob.Sums, magic string) error {
	f, err := h.Store.Open(meta.Asset{Repo: rp.Name, BlobSHA256: sums.SHA256})
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, len(magic))
	if _, err := io.ReadFull(f, head); err != nil || string(head) != magic {
		return bad("not a zip archive")
	}
	return nil
}

// checkSums checks a SHA256SUMS upload: every line is a hex sha256, spaces
// and a file name, as sha256sum writes them.
func (h *Handler) checkSums(rp *repo.Repository, sums blob.Sums) error {
	if sums.Size > maxManifest {
		return bad("SHA256SUMS is larger than %d bytes", maxManifest)
	}
	f, err := h.Store.Open(meta.Asset{Repo: rp.Name, BlobSHA256: sums.SHA256})
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		sum, file, ok := strings.Cut(line, " ")
		if !ok || !proxy.IsSHA256Hex(sum) || strings.TrimLeft(file, " *") == "" {
			return bad("SHA256SUMS line %q is not \"<sha256>  <file>\"", line)
		}
		n++
	}
	if n == 0 {
		return bad("SHA256SUMS is empty")
	}
	return nil
}

// readModule reads the stored archive: a gzip tar or a zip that holds
// *.tf files at its root or in exactly one top-level directory. The
// description is the first paragraph of the README.md beside them.
func (h *Handler) readModule(rp *repo.Repository, sums blob.Sums) (moduleAttrs, error) {
	f, err := h.Store.Open(meta.Asset{Repo: rp.Name, BlobSHA256: sums.SHA256})
	if err != nil {
		return moduleAttrs{}, err
	}
	defer f.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return moduleAttrs{}, bad("not an archive")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return moduleAttrs{}, err
	}
	var entries []archiveEntry
	ext := "tgz"
	switch {
	case head[0] == 0x1f && head[1] == 0x8b:
		entries, err = tgzEntries(f)
	case string(head) == "PK\x03\x04":
		entries, err = zipEntries(f, sums.Size)
		ext = "zip"
	default:
		return moduleAttrs{}, bad("not a gzip tar or zip archive")
	}
	if err != nil {
		return moduleAttrs{}, err
	}
	a, err := moduleOf(entries)
	a.Ext = ext
	return a, err
}

// archiveEntry is one file of an archive with its README text when it is
// a README.md.
type archiveEntry struct {
	name   string
	readme string
}

func tgzEntries(r io.Reader) ([]archiveEntry, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, bad("not a gzip archive: %v", err)
	}
	tr := tar.NewReader(gz)
	var out []archiveEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, bad("not a tar archive: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		e := archiveEntry{name: hdr.Name}
		if isReadme(hdr.Name) {
			b, _ := io.ReadAll(io.LimitReader(tr, maxReadme))
			e.readme = string(b)
		}
		out = append(out, e)
	}
}

func zipEntries(r io.ReaderAt, size int64) ([]archiveEntry, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, bad("not a zip archive: %v", err)
	}
	var out []archiveEntry
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		e := archiveEntry{name: f.Name}
		if isReadme(f.Name) {
			if rc, err := f.Open(); err == nil {
				b, _ := io.ReadAll(io.LimitReader(rc, maxReadme))
				rc.Close()
				e.readme = string(b)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

func isReadme(name string) bool { return strings.EqualFold(path.Base(name), "README.md") }

// moduleOf finds the module in the archive's entries: *.tf at the root,
// else in the one top-level directory ("*" subdir, as go-getter spells it).
func moduleOf(entries []archiveEntry) (moduleAttrs, error) {
	dirs := map[string]bool{}
	var root, inDir bool
	var readme string
	for _, e := range entries {
		name := strings.TrimPrefix(path.Clean("/"+e.name), "/")
		dir, file := path.Split(name)
		isTF := strings.HasSuffix(file, ".tf")
		switch {
		case dir == "":
			root = root || isTF
			if e.readme != "" {
				readme = e.readme
			}
		case strings.Count(dir, "/") == 1:
			dirs[dir] = true
			inDir = inDir || isTF
			if e.readme != "" && !root {
				readme = e.readme
			}
		}
	}
	a := moduleAttrs{Description: firstParagraph(readme)}
	switch {
	case root:
	case inDir && len(dirs) == 1:
		a.Subdir = "*"
	default:
		return moduleAttrs{}, bad("no *.tf files at the root of the archive (or in its one top-level directory)")
	}
	return a, nil
}

// firstParagraph is the first paragraph of a README's text, its lines
// joined: headings, badge and image lines and HTML are skipped, and the
// first heading stands in when there is nothing else.
func firstParagraph(readme string) string {
	var heading string
	var para []string
	for _, line := range strings.Split(readme, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "![") || strings.HasPrefix(line, "[![") || strings.HasPrefix(line, "<"):
			if heading == "" && strings.HasPrefix(line, "#") {
				heading = strings.TrimSpace(strings.TrimLeft(line, "#"))
			}
			if len(para) > 0 {
				return strings.Join(para, " ")
			}
		default:
			para = append(para, line)
		}
	}
	if len(para) > 0 {
		return strings.Join(para, " ")
	}
	return heading
}

// unpublish is `DELETE v1/modules/<ns>/<name>/<provider>/<version>` or
// `DELETE v1/providers/<ns>/<type>/<version>`: the version's row and files
// are deleted. 204, or 404 when there is no such version.
func (h *Handler) unpublish(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	if rt.Part != "" {
		return repo.Errorf(http.StatusNotFound, "delete the version, not one of its files")
	}
	c := component.Component{Repo: rp.Name, Format: "terraform", Name: rt.Name, Version: rt.Version}
	res, err := h.DeleteComponent(r.Context(), rp, c, auth.FromContext(r.Context()).Owner())
	if err != nil {
		return err
	}
	if res.Assets == 0 {
		return repo.Errorf(http.StatusNotFound, "%s %s not found in repository %q", rt.Name, rt.Version, rp.Name)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// DeleteComponent removes one version of a hosted module (its row and
// archive) or provider (its row and every file of the version), in one
// incarnation-guarded transaction under the lock, for the upload API's
// DELETE, the web UI's delete and cleanup policies. The documents are
// derived from the rows and files, so nothing else changes. Proxies use
// the generic proxy deleter.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "terraform" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("terraform component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Name == "" || c.Version == "" {
		return component.Result{}, fmt.Errorf("terraform component %q is incomplete", c.Label())
	}
	unlock, err := h.Store.Lock(ctx, "terraform\x00"+rp.Name+"\x00"+c.Name)
	if err != nil {
		return component.Result{}, err
	}
	defer unlock()
	// A module's files: modules/<name>/<version>.<ext>; a provider's: the
	// version directory. The name's shape tells them apart.
	var prefix string
	if strings.Count(c.Name, "/") == 2 {
		prefix = moduleStore + c.Name + "/" + c.Version + "."
	} else {
		prefix = providerDir(c.Name, c.Version)
	}
	rowGone := false
	var res component.Result
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		rowGone, res = false, component.Result{}
		have, err := tx.ListAssets(ctx, rp.Name, prefix)
		if err != nil {
			return err
		}
		if err := cleanup.CheckUnchanged(c, have); err != nil {
			return err
		}
		switch err := tx.DeletePackage(ctx, rp.Name, c.Name, c.Version); {
		case err == nil:
			rowGone = true
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
		for _, a := range have {
			size, err := tx.DeleteAsset(ctx, rp.Name, a.Path)
			if err != nil {
				return err
			}
			res.Add(size)
		}
		return nil
	})
	if err != nil {
		return component.Result{}, err
	}
	if rowGone || res.Assets > 0 {
		h.Store.EmitAs("delete", actor, rp.Name, c.Label(), fmt.Sprintf("assets=%d", res.Assets))
	}
	return res, nil
}

// uploadErr maps a body read error to its status.
func uploadErr(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return repo.Errorf(http.StatusRequestEntityTooLarge, "upload is larger than the upload limit")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return bad("upload ended early: %v", err)
	}
	return fmt.Errorf("reading upload: %w", err)
}
