package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

// readPackageJSON returns the package.json of an npm tarball that checkTarball
// accepted: the regular file that lands on package.json once npm strips the
// first path component. At most maxPackageJSON bytes of it are read.
func readPackageJSON(r io.Reader) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, repo.Errorf(http.StatusBadRequest, "invalid tarball: not a gzip archive")
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, maxTarballBytes))
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, repo.Errorf(http.StatusBadRequest, "invalid tarball: no package.json in its top directory")
		}
		if _, rest, ok := strings.Cut(h.Name, "/"); !ok || path.Clean("/"+rest) != "/package.json" || h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxPackageJSON+1))
		if err != nil {
			return nil, repo.Errorf(http.StatusBadRequest, "invalid tarball: %v", err)
		}
		if int64(len(b)) > maxPackageJSON {
			return nil, repo.Errorf(http.StatusBadRequest, "invalid tarball: package.json is larger than %d bytes", maxPackageJSON)
		}
		return bytes.TrimPrefix(b, []byte("\uFEFF")), nil
	}
}

// Ingest stores one npm tarball in the hosted repository rp, as a publish
// would, and returns its version. name and version are what the source says
// the tarball is (its npm attributes or its path); both are required. They
// are checked as a publish checks a new version (a SemVer 2.0 version without
// build metadata), and the tarball is streamed to a blob and checked as a
// publish checks it (checkTarball: one package.json in the top directory,
// with this name and version). A tarball that fails is refused before
// anything is recorded. The stored manifest is the tarball's package.json
// with that name and version and the server's dist. An identical version
// (same tarball) is Skipped, a different tarball for the same version is a
// conflict (repo.IsIngestConflict); nothing is ever overwritten. It makes no
// privilege checks: the caller is the admin import, and the repository's
// write policy does not apply. dist-tags are set separately by ApplyTags.
func (h *Handler) Ingest(ctx context.Context, rp *repo.Repository, name, version string, tarball io.Reader, actor string) (string, repo.IngestResult, error) {
	v := version
	if name == "" || v == "" {
		return "", "", repo.Errorf(http.StatusBadRequest, "the source does not say which package and version this tarball is (%q %q)", name, v)
	}
	if !ValidName(name) {
		return "", "", invalidName(name)
	}
	if !semver.Valid(v) {
		return "", "", repo.Errorf(http.StatusBadRequest, "invalid version %q: must be a SemVer 2.0 version such as 1.2.3 or 1.2.3-beta.1", v)
	}
	if core, _, ok := strings.Cut(v, "+"); ok {
		return "", "", repo.Errorf(http.StatusBadRequest, "invalid version %q: build metadata (+...) is not allowed; npm reads it as %s", v, core)
	}
	sums, err := h.Store.WriteBlob(rp.Name, tarball, nil)
	if err != nil {
		return "", "", err
	}
	left := maxTarballBytes
	if err := h.validateTarball(ctx, rp, sums, name, v, &left); err != nil {
		return "", "", err
	}
	bs, err := h.Store.BlobStore(rp.Name)
	if err != nil {
		return "", "", err
	}
	f, err := bs.Open(sums.SHA256)
	if err != nil {
		return "", "", err
	}
	raw, err := readPackageJSON(f)
	f.Close()
	if err != nil {
		return "", "", err
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest == nil {
		return "", "", repo.Errorf(http.StatusBadRequest, "invalid tarball for %s@%s: package.json is not a JSON object", name, v)
	}
	manifest["name"], _ = json.Marshal(name)
	manifest["version"], _ = json.Marshal(v) // as npm publish cleans it (cleanVersion)
	raw, err = json.Marshal(manifest)
	if err != nil {
		return "", "", err
	}
	file := TarballFile(name, v)
	attrs, err := withServerDist(raw, file, sums)
	if err != nil {
		return "", "", err
	}

	unlock := h.locks.Lock(rp.Name + "\x00" + name)
	defer unlock()
	res := repo.Ingested
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		res = repo.Ingested
		pkgs, err := tx.ListPackages(ctx, rp.Name, name)
		if err != nil {
			return err
		}
		for _, p := range pkgs {
			if p.Version != v {
				continue
			}
			a, err := tx.GetAsset(ctx, rp.Name, TarballKey(name, file))
			if err == nil && a.BlobSHA256 == sums.SHA256 {
				res = repo.Skipped
				return nil
			}
			if err != nil && !errors.Is(err, meta.ErrNotFound) {
				return err
			}
			return repo.IngestConflict("%s@%s already exists with different content", name, v)
		}
		if err := tx.InsertPackage(ctx, meta.Package{Repo: rp.Name, Format: "npm", Name: name, Version: v, Attrs: attrs, CreatedAt: h.Now()}); err != nil {
			if errors.Is(err, meta.ErrExists) {
				return repo.IngestConflict("%s@%s already exists", name, v)
			}
			return err
		}
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, TarballKey(name, file), "application/octet-stream", sums)); err != nil {
			return err
		}
		return h.writePackument(ctx, tx, rp, name)
	})
	if err != nil {
		return "", "", err
	}
	if res == repo.Ingested {
		h.Store.EmitAs("upload", actor, rp.Name, name+"@"+v, "")
	}
	return v, res, nil
}

// TagResult reports what ApplyTags did.
type TagResult struct {
	Tags       int      // dist-tags set
	Deprecated int      // versions whose deprecation message was set
	Skipped    []string // entries not applied (unknown version, invalid tag)
	Kept       []string // changes not made: the target already has its own value
}

// ApplyTags sets dist-tags and deprecation messages of package name from the
// source registry. Only versions that exist are touched: a tag or deprecation
// for a missing version, or a tag with an invalid name, is skipped and listed
// in the result. When the package ends up without a "latest" tag, the newest
// release is tagged. It never moves what the target already has: a dist-tag
// is set only when the target has no such tag (an equal one is left alone), a
// deprecation only when the version has none; the skipped changes are listed
// in Kept ("kept target value"). It makes no privilege checks (admin import) and emits no
// event; actor is accepted for symmetry with Ingest.
func (h *Handler) ApplyTags(ctx context.Context, rp *repo.Repository, name string, distTags, deprecations map[string]string, actor string) (TagResult, error) {
	var res TagResult
	unlock := h.locks.Lock(rp.Name + "\x00" + name)
	defer unlock()
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		res = TagResult{}
		pkgs, err := tx.ListPackages(ctx, rp.Name, name)
		if err != nil {
			return err
		}
		if len(pkgs) == 0 {
			return repo.Errorf(http.StatusNotFound, "package %q not found", name)
		}
		byVersion := map[string]meta.Package{}
		for _, p := range pkgs {
			byVersion[p.Version] = p
		}
		for _, v := range sortedKeys(deprecations) {
			p, ok := byVersion[v]
			if !ok {
				res.Skipped = append(res.Skipped, "deprecated "+v)
				continue
			}
			var cur struct {
				Deprecated string `json:"deprecated"`
			}
			json.Unmarshal([]byte(p.Attrs), &cur)
			if cur.Deprecated != "" {
				if cur.Deprecated != deprecations[v] {
					res.Kept = append(res.Kept, "deprecation of "+v+": kept target value")
				}
				continue
			}
			msg, _ := json.Marshal(map[string]string{"deprecated": deprecations[v]})
			attrs, err := applyDeprecation(msg, p.Attrs)
			if err != nil {
				return err
			}
			if err := tx.UpdatePackage(ctx, rp.Name, name, v, attrs); err != nil {
				return err
			}
			res.Deprecated++
		}
		existing, err := tx.ListDistTags(ctx, rp.Name, name)
		if err != nil {
			return err
		}
		have := map[string]string{}
		for _, t := range existing {
			have[t.Tag] = t.Version
		}
		for _, tag := range sortedKeys(distTags) {
			v := distTags[tag]
			if _, ok := byVersion[v]; !ok || (!ValidTagName(tag) && tag != "latest") {
				res.Skipped = append(res.Skipped, "tag "+tag)
				continue
			}
			if cur, ok := have[tag]; ok {
				if cur != v {
					res.Kept = append(res.Kept, "dist-tag "+tag+" (target "+cur+", source "+v+"): kept target value")
				}
				continue
			}
			if err := tx.SetDistTag(ctx, rp.Name, name, tag, v); err != nil {
				return err
			}
			res.Tags++
		}
		tags, err := tx.ListDistTags(ctx, rp.Name, name)
		if err != nil {
			return err
		}
		hasLatest := false
		for _, t := range tags {
			hasLatest = hasLatest || t.Tag == "latest"
		}
		if !hasLatest {
			newest := ""
			for _, p := range pkgs {
				newest = semver.NewerRelease(newest, p.Version)
			}
			if err := tx.SetDistTag(ctx, rp.Name, name, "latest", newest); err != nil {
				return err
			}
		}
		return h.writePackument(ctx, tx, rp, name)
	})
	return res, err
}
