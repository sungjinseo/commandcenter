package swift

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

const (
	maxManifest  = 1 << 20 // a Package.swift read out of an archive
	maxMetadata  = 1 << 20 // the metadata part of a publish
	maxSignature = 64 << 10
)

func badPackage(format string, args ...any) error {
	return problem(http.StatusBadRequest, "package: "+format, args...)
}

// attrs is what a hosted row keeps of a release: the release metadata
// (with the archive's checksum and signature) and what the manifest
// declares, for the UI.
type attrs struct {
	ID           string          `json:"id"` // scope.name as published
	Checksum     string          `json:"checksum"`
	Size         int64           `json:"size"`
	Signing      *signing        `json:"signing,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	PublishedAt  string          `json:"publishedAt"`
	Dependencies []Dependency    `json:"dependencies"`
}

type signing struct {
	SignatureBase64Encoded string `json:"signatureBase64Encoded"`
	SignatureFormat        string `json:"signatureFormat"`
}

// Dependency is one .package(...) of a manifest: the package name (the last
// path element of its URL, or its identifier) and the requirement text.
type Dependency struct {
	Name        string `json:"name"`
	URL         string `json:"url,omitempty"`
	Requirement string `json:"requirement"`
}

// row is one stored version of a hosted package.
type row struct {
	pkg   meta.Package
	attrs attrs
}

// rows lists the versions of id in hosted rp, in the database's order. A
// row whose attrs cannot be read still lists with its id and version.
func (h *Handler) rows(ctx context.Context, rp *repo.Repository, id string) ([]row, error) {
	pkgs, err := h.Store.DB.ListPackages(ctx, rp.Name, id)
	if err != nil {
		return nil, err
	}
	out := make([]row, 0, len(pkgs))
	for _, p := range pkgs {
		r := row{pkg: p}
		if json.Unmarshal([]byte(p.Attrs), &r.attrs) != nil || r.attrs.ID == "" {
			r.attrs = attrs{ID: p.Name}
		}
		out = append(out, r)
	}
	return out, nil
}

// row finds rt's version in hosted rp.
func (h *Handler) row(ctx context.Context, rp *repo.Repository, rt Route) (row, error) {
	rows, err := h.rows(ctx, rp, ID(rt.Scope, rt.Name))
	if err != nil {
		return row{}, err
	}
	for _, r := range rows {
		if r.pkg.Version == rt.Version {
			return r, nil
		}
	}
	if len(rows) == 0 {
		return row{}, notFoundPackage(rt)
	}
	return row{}, notFoundRelease(rt)
}

// publish takes a release into hosted rp: `PUT {scope}/{name}/{version}`
// as `swift package-registry publish` sends it, a multipart form with the
// parts source-archive (the zip), metadata (JSON) and
// source-archive-signature (the raw signature, its format in the
// X-Swift-Package-Signature-Format header), or the zip as the body itself
// (curl -T). The archive must hold the package in one top-level directory,
// <dir>/Package.swift, as `swift package archive-source` lays it out:
// SwiftPM strips that level when it unpacks a download. The server's
// maxUploadMiB bounds the body. An existing version is a 409
// unless writePolicy is allow, when replacing it needs Edit. Answers 201
// with the release's Location (publishing is synchronous).
func (h *Handler) publish(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route) error {
	if rp.WritePolicy == "deny" {
		return problem(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	var (
		sums     blob.Sums
		gotZip   bool
		metadata json.RawMessage
		sig      []byte
		err      error
	)
	if !isMultipart(r) {
		sums, err = h.Store.WriteBlob(rp.Name, r.Body, nil)
		if err != nil {
			return uploadErr(err)
		}
		gotZip = true
	} else {
		mr, err := r.MultipartReader()
		if err != nil {
			return badPackage("%v", err)
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return uploadErr(err)
			}
			switch part.FormName() {
			case "source-archive":
				if gotZip {
					return badPackage("more than one source-archive part")
				}
				sums, err = h.Store.WriteBlob(rp.Name, part, nil)
				if err != nil {
					return uploadErr(err)
				}
				gotZip = true
			case "metadata":
				raw, err := readPart(part, maxMetadata)
				if err != nil {
					return err
				}
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil || m == nil {
					return badPackage("metadata is not a JSON object: %v", err)
				}
				metadata = raw
			case "source-archive-signature":
				if sig, err = readPart(part, maxSignature); err != nil {
					return err
				}
			}
		}
	}
	if !gotZip {
		return badPackage("multipart body has no source-archive part")
	}
	if sums.Size == 0 {
		return badPackage("empty upload")
	}
	m, err := h.readManifests(meta.Asset{Repo: rp.Name, BlobSHA256: sums.SHA256, Size: sums.Size})
	if err != nil {
		return err
	}
	if m.dir == "" || m.others {
		return badPackage("the archive must hold the package in one top-level directory (<name>/Package.swift), as swift package archive-source makes it")
	}
	manifest, err := m.read(manifestFile)
	if err != nil {
		return err
	}
	now := h.Now().UTC()
	a := attrs{ID: rt.Scope + "." + rt.Name, Checksum: sums.SHA256, Size: sums.Size, Metadata: metadata,
		PublishedAt: now.Format(time.RFC3339), Dependencies: ParseDependencies(string(manifest))}
	if len(sig) > 0 {
		format := r.Header.Get("X-Swift-Package-Signature-Format")
		if format == "" {
			format = "cms-1.0.0"
		}
		a.Signing = &signing{SignatureBase64Encoded: base64.StdEncoding.EncodeToString(sig), SignatureFormat: format}
	}
	attrsJSON, err := json.Marshal(a)
	if err != nil {
		return err
	}
	id := ID(rt.Scope, rt.Name)
	unlock := h.locks.Lock(rp.Name + "\x00" + id)
	defer unlock()
	ctx := r.Context()
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		row := meta.Package{Repo: rp.Name, Format: "swift", Name: id, Version: rt.Version, Attrs: string(attrsJSON), CreatedAt: now}
		switch err := tx.InsertPackage(ctx, row); {
		case errors.Is(err, meta.ErrExists) && rp.WritePolicy != "allow":
			return problem(http.StatusConflict, "a release with the same version exists: %s %s in repository %q", a.ID, rt.Version, rp.Name)
		case errors.Is(err, meta.ErrExists) && !h.Auth.Permits(ctx, rp.Name, auth.Edit):
			return problem(http.StatusForbidden, "user %q may not overwrite %s %s", auth.FromContext(ctx).User, a.ID, rt.Version)
		case errors.Is(err, meta.ErrExists):
			if err := tx.UpdatePackage(ctx, rp.Name, id, rt.Version, row.Attrs); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, archivePath(rt.Scope, rt.Name, rt.Version), ctZip, sums))
	})
	if err != nil {
		return err
	}
	h.Store.Emit(r, "upload", rp.Name, id+"@"+rt.Version, "")
	w.Header().Set("Location", h.releaseURL(rp, rt, rt.Version))
	w.WriteHeader(http.StatusCreated)
	return nil
}

func readPart(p io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(p, limit+1))
	if err != nil {
		return nil, uploadErr(err)
	}
	if int64(len(raw)) > limit {
		return nil, badPackage("part is larger than %d bytes", limit)
	}
	return raw, nil
}

// uploadErr maps a body read error to its status.
func uploadErr(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return problem(http.StatusRequestEntityTooLarge, "package is larger than the upload limit")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return badPackage("upload ended early: %v", err)
	}
	return fmt.Errorf("reading package: %w", err)
}

// manifests is what an archive holds of manifests: Package.swift and its
// Package@swift-X.Y.swift variants, all in one directory (the root, or the
// first directory down), read whole (each within maxManifest).
type manifests struct {
	dir    string   // "" (the root) or the top-level directory, with its "/"
	others bool     // another top-level directory has entries too
	names  []string // sorted, Package.swift first
	bodies map[string][]byte
}

func (m *manifests) read(name string) ([]byte, error) {
	if b, ok := m.bodies[name]; ok {
		return b, nil
	}
	return nil, problem(http.StatusNotFound, "manifest %s not found", name)
}

// readManifests opens the archive a (a stored or cached blob) and reads
// its manifests.
func (h *Handler) readManifests(a meta.Asset) (*manifests, error) {
	f, err := h.Store.Open(a)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := zip.NewReader(f, a.Size)
	if err != nil {
		return nil, badPackage("not a zip: %v", err)
	}
	return findManifests(zr)
}

func findManifests(zr *zip.Reader) (*manifests, error) {
	dir := ""
	found := false
	for _, f := range zr.File {
		if f.Name == manifestFile {
			dir, found = "", true
			break
		}
		if !found && strings.HasSuffix(f.Name, "/"+manifestFile) && strings.Count(f.Name, "/") == 1 {
			dir, found = f.Name[:strings.Index(f.Name, "/")+1], true
		}
	}
	if !found {
		return nil, badPackage("no Package.swift at the root of the archive")
	}
	m := &manifests{dir: dir, bodies: map[string][]byte{}}
	for _, f := range zr.File {
		if i := strings.IndexByte(f.Name, '/'); i >= 0 && f.Name[:i+1] != dir {
			m.others = true
		}
		name, ok := strings.CutPrefix(f.Name, dir)
		if !ok || strings.Contains(name, "/") || (name != manifestFile && !variantRE.MatchString(name)) {
			continue
		}
		if f.UncompressedSize64 > maxManifest {
			return nil, badPackage("%s is larger than %d bytes", f.Name, maxManifest)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, badPackage("reading %s: %v", f.Name, err)
		}
		raw, err := io.ReadAll(io.LimitReader(rc, maxManifest+1))
		rc.Close()
		if err != nil || len(raw) > maxManifest {
			return nil, badPackage("reading %s: %v", f.Name, errors.Join(err, errors.New("too large")))
		}
		m.bodies[name] = raw
		m.names = append(m.names, name)
	}
	sort.Strings(m.names) // "Package.swift" < "Package@swift-..." ('.' < '@')
	return m, nil
}

// depRE is one .package(...) of a manifest, with one level of nested
// parentheses for the requirement (.upToNextMajor(from: "1.0.0")).
var (
	depRE    = regexp.MustCompile(`\.package\(((?:[^()]|\([^()]*\))*)\)`)
	depKeyRE = regexp.MustCompile(`^(?:name:\s*"[^"]*"\s*,\s*)?(url|id|path):\s*"([^"]*)"\s*,?\s*`)
)

// ParseDependencies finds the package dependencies of a manifest with a
// tolerant regular expression: .package(url: "...", from: "1.0.0"),
// .package(url:, .upToNextMajor(from:)), .package(id: "scope.name",
// exact: "..."), .package(path: "../local"). The requirement is the rest
// of the argument list as written.
func ParseDependencies(manifest string) []Dependency {
	out := []Dependency{}
	for _, m := range depRE.FindAllStringSubmatch(manifest, -1) {
		args := strings.TrimSpace(m[1])
		k := depKeyRE.FindStringSubmatch(args)
		if k == nil {
			continue
		}
		d := Dependency{Requirement: strings.TrimSpace(args[len(k[0]):])}
		switch k[1] {
		case "url":
			d.URL = k[2]
			d.Name = strings.TrimSuffix(k[2][strings.LastIndexAny(k[2], "/:")+1:], ".git")
		default:
			d.Name = k[2]
		}
		if d.Name != "" {
			out = append(out, d)
		}
	}
	return out
}

// hostedIdentifiers lists the packages of hosted rp whose metadata names u
// among its repositoryURLs (compared without case, a trailing "/" or ".git").
// ponytail: reads every package's rows; an index if repositories grow large.
func (h *Handler) hostedIdentifiers(ctx context.Context, rp *repo.Repository, u string) ([]string, error) {
	names, err := h.Store.DB.ListPackageNames(ctx, rp.Name)
	if err != nil {
		return nil, err
	}
	want := normalizeURL(u)
	var ids []string
	for _, name := range names {
		rows, err := h.rows(ctx, rp, name)
		if err != nil {
			return nil, err
		}
	rows:
		for _, r := range rows {
			var m struct {
				URLs []string `json:"repositoryURLs"`
			}
			json.Unmarshal(r.attrs.Metadata, &m)
			for _, ru := range m.URLs {
				if normalizeURL(ru) == want {
					ids = append(ids, r.attrs.ID)
					break rows
				}
			}
		}
	}
	return ids, nil
}

func normalizeURL(u string) string {
	u = strings.ToLower(strings.TrimSpace(u))
	return strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
}

// DeleteComponent removes one version of a hosted package: its row and
// archive, in one incarnation-guarded transaction under the package's
// lock, for the web UI's delete and cleanup policies. The documents are
// derived from the rows, so nothing else changes. A version that is gone
// already is not an error, and one that changed since an evaluation is
// skipped (cleanup.CheckUnchanged). Proxies use the generic proxy deleter.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "swift" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("swift component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	scope, name, ok := SplitID(strings.ToLower(c.Name))
	if !ok || c.Version == "" {
		return component.Result{}, fmt.Errorf("swift component %q is incomplete", c.Label())
	}
	id := ID(scope, name)
	unlock := h.locks.Lock(rp.Name + "\x00" + id)
	defer unlock()
	rowGone := false
	var res component.Result
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		rowGone, res = false, component.Result{}
		p := archivePath(scope, name, c.Version)
		var have []meta.Asset
		switch a, err := tx.GetAsset(ctx, rp.Name, p); {
		case err == nil:
			have = append(have, a)
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
		if err := cleanup.CheckUnchanged(c, have); err != nil {
			return err
		}
		switch err := tx.DeletePackage(ctx, rp.Name, id, c.Version); {
		case err == nil:
			rowGone = true
		case !errors.Is(err, meta.ErrNotFound):
			return err
		}
		switch size, err := tx.DeleteAsset(ctx, rp.Name, p); {
		case err == nil:
			res.Add(size)
		case !errors.Is(err, meta.ErrNotFound):
			return err
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
