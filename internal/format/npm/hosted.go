package npm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/semver"
)

// publish handles `npm publish` and whole-document updates such as `npm deprecate`.
// New versions must carry their tarball; existing versions can only change metadata.
// published is called once for each new version after the transaction committed
// (a deprecation-only publish adds none).
func (h *Handler) publish(ctx context.Context, w http.ResponseWriter, rp *repo.Repository, name string, r *http.Request, published func(version string)) error {
	unlock, err := h.Store.Lock(ctx, "npm\x00"+rp.Name+"\x00"+name)
	if err != nil {
		return err
	}
	defer unlock()

	// The attachment blob is streamed to the store while the document is being
	// scanned, and the decoded manifest stays live until the transaction ends,
	// so the slot is held for the whole publish.
	release, err := acquirePublish(ctx)
	if err != nil {
		return err
	}
	defer release()

	// A stalled client must not hold the slot forever: the read deadline moves
	// forward as data arrives (the server clears it when the request ends).
	body := repo.IdleBody(w, r, publishReadTimeout)

	tarballs := map[string]blob.Sums{} // tarball file name -> stored content
	manifest, err := splitPublish(body, func(att string, data io.Reader) error {
		// An attachment is a new version: without Add, refuse before any
		// blob is written.
		if !h.Auth.Permits(ctx, rp.Name, auth.Add) {
			return repo.Errorf(http.StatusForbidden, "user %q is not allowed to do this", auth.FromContext(ctx).User)
		}
		sums, err := h.Store.WriteBlob(rp.Name, data, nil)
		if err != nil {
			return err
		}
		tarballs[baseName(att)] = sums
		return nil
	})
	if err != nil {
		return err
	}
	var doc publishDoc
	if err := json.Unmarshal(manifest, &doc); err != nil {
		return badJSON(err)
	}
	if doc.Name != name {
		return repo.Errorf(http.StatusBadRequest, "package name %q does not match URL %q", doc.Name, name)
	}
	existing, err := h.Store.DB.ListPackages(ctx, rp.Name, name)
	if err != nil {
		return err
	}
	// Names stored by older builds or read through proxies stay valid; only
	// a new package needs a name npm accepts for new packages (QA L-15).
	if len(existing) == 0 && !validNewName(name) {
		return repo.Errorf(http.StatusBadRequest, "invalid package name %q: a new package name must be lower case and not node_modules or favicon.ico", name)
	}
	old := map[string]meta.Package{}
	for _, p := range existing {
		old[p.Version] = p
	}
	// Every version is checked before any tarball is walked.
	var fresh []string // new versions
	for _, v := range sortedKeys(doc.Versions) {
		var vm versionManifest
		if err := json.Unmarshal(doc.Versions[v], &vm); err != nil {
			return badJSON(err)
		}
		if vm.Version != v {
			return repo.Errorf(http.StatusBadRequest, "version key %q does not match manifest version %q", v, vm.Version)
		}
		sums, hasTarball := tarballs[TarballFile(name, v)]
		if _, ok := old[v]; ok {
			if hasTarball {
				return repo.Errorf(http.StatusForbidden, "cannot publish over the previously published version %s", v)
			}
			continue
		}
		// Only new versions are checked: rows stored by older builds
		// keep loading and can still be deprecated (QA M-5).
		if vm.Name != name {
			return repo.Errorf(http.StatusBadRequest, "version %s: manifest name %q does not match package name %q", v, vm.Name, name)
		}
		if !semver.Valid(v) {
			return repo.Errorf(http.StatusBadRequest, "invalid version %q: must be a SemVer 2.0 version such as 1.2.3 or 1.2.3-beta.1", v)
		}
		// npm ignores build metadata when it matches versions, so
		// 3.0.0+a would be a second 3.0.0 that breaks installs (QA N-12).
		if core, _, ok := strings.Cut(v, "+"); ok {
			return repo.Errorf(http.StatusBadRequest, "invalid version %q: build metadata (+...) is not allowed; npm reads it as %s", v, core)
		}
		if !hasTarball {
			return repo.Errorf(http.StatusBadRequest, "version %s has no tarball attachment", v)
		}
		if err := verifyTarball(vm.Dist.Shasum, vm.Dist.Integrity, sums); err != nil {
			return err
		}
		fresh = append(fresh, v)
	}
	// New versions' tarballs are read back before the transaction, so the
	// walk never holds the database write lock. They share one budget, so a
	// document of many versions costs no more than one large tarball.
	left := maxTarballBytes
	for _, v := range fresh {
		if err := h.validateTarball(ctx, rp, tarballs[TarballFile(name, v)], name, v, &left); err != nil {
			return err
		}
	}
	// A new version is newer than every stored one, by a millisecond if the
	// clock has not moved on: unpublish refuses to delete a version newer than
	// the _rev a client read (packumentRev).
	// ponytail: only stored versions count, so the newest version, deleted and
	// published again before the clock passes its first publish time, is no
	// newer than a _rev read in between. A per-package high-water mark fixes it.
	now := h.Now()
	for _, p := range existing {
		if now.UnixMilli() <= p.CreatedAt.UnixMilli() {
			now = p.CreatedAt.Add(time.Millisecond)
		}
	}

	var added []string
	edited := false // an existing version's deprecation or a dist-tag changed
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		added, edited = added[:0], false
		known := map[string]bool{}
		for v := range old {
			known[v] = true
		}
		for _, v := range sortedKeys(doc.Versions) {
			raw := doc.Versions[v]
			if p, ok := old[v]; ok {
				attrs, err := applyDeprecation(raw, p.Attrs)
				if err != nil {
					return err
				}
				if deprecationOf(attrs) != deprecationOf(p.Attrs) {
					edited = true
				}
				if err := tx.UpdatePackage(ctx, rp.Name, name, v, attrs); err != nil {
					return err
				}
				continue
			}
			file := TarballFile(name, v)
			sums := tarballs[file]
			attrs, err := withServerDist(raw, file, sums)
			if err != nil {
				return err
			}
			err = tx.InsertPackage(ctx, meta.Package{Repo: rp.Name, Format: "npm", Name: name, Version: v, Attrs: attrs, CreatedAt: now})
			if errors.Is(err, meta.ErrExists) {
				return repo.Errorf(http.StatusForbidden, "cannot publish over the previously published version %s", v)
			}
			if err != nil {
				return err
			}
			if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, TarballKey(name, file), "application/octet-stream", sums)); err != nil {
				return err
			}
			added = append(added, v)
			known[v] = true
		}
		if len(known) == 0 {
			return repo.Errorf(http.StatusBadRequest, "package %q has no versions", name)
		}
		oldTags, err := tx.ListDistTags(ctx, rp.Name, name)
		if err != nil {
			return err
		}
		current := map[string]string{}
		for _, t := range oldTags {
			current[t.Tag] = t.Version
		}
		isAdded := map[string]bool{}
		for _, v := range added {
			isAdded[v] = true
		}
		for _, tag := range sortedKeys(doc.DistTags) {
			v := doc.DistTags[tag]
			if !known[v] {
				return repo.Errorf(http.StatusBadRequest, "dist-tag %q points to unknown version %q", tag, v)
			}
			// A tag stored by an older build comes back in every whole-
			// document PUT (npm deprecate); only new tag names are checked.
			if _, stored := current[tag]; !stored && !ValidTagName(tag) {
				return invalidTag(tag)
			}
			// A tag on a version added by this document is part of the publish
			// (`npm publish --tag x`); moving a tag to an existing version is
			// an edit. Re-sending an unchanged tag changes nothing.
			if !isAdded[v] && current[tag] != v {
				edited = true
			}
			if err := tx.SetDistTag(ctx, rp.Name, name, tag, v); err != nil {
				return err
			}
		}
		// New versions need Add; a changed deprecation or dist-tag, or a
		// document that adds nothing (only metadata and dist-tags), needs Edit
		// (Decision 2).
		if !h.publishAllowed(ctx, rp, len(added) > 0, edited || len(added) == 0) {
			return repo.Errorf(http.StatusForbidden, "user %q is not allowed to do this", auth.FromContext(ctx).User)
		}
		return h.writePackument(ctx, tx, rp, name)
	})
	if err != nil {
		return err
	}
	for _, v := range added {
		published(v)
	}
	return nil
}

// validateTarball checks the stored tarball of name@version (checkTarball,
// which charges it to left). The blob is read back from disk, so nothing is
// buffered.
func (h *Handler) validateTarball(ctx context.Context, rp *repo.Repository, sums blob.Sums, name, version string, left *int64) error {
	bs, err := h.Store.BlobStore(rp.Name)
	if err != nil {
		return err
	}
	f, err := bs.Open(sums.SHA256)
	if err != nil {
		return err
	}
	defer f.Close()
	return checkTarball(ctx, f, name, version, left)
}

func (h *Handler) setDistTag(ctx context.Context, rp *repo.Repository, name, tag, version string) error {
	unlock, err := h.Store.Lock(ctx, "npm\x00"+rp.Name+"\x00"+name)
	if err != nil {
		return err
	}
	defer unlock()
	return h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		pkgs, err := tx.ListPackages(ctx, rp.Name, name)
		if err != nil {
			return err
		}
		if len(pkgs) == 0 {
			return repo.Errorf(http.StatusNotFound, "package %q not found", name)
		}
		found := false
		for _, p := range pkgs {
			found = found || p.Version == version
		}
		if !found {
			return repo.Errorf(http.StatusBadRequest, "version %q of %q not found", version, name)
		}
		if !ValidTagName(tag) {
			// Moving a tag stored by an older build is still allowed.
			tags, err := tx.ListDistTags(ctx, rp.Name, name)
			if err != nil {
				return err
			}
			stored := false
			for _, t := range tags {
				stored = stored || t.Tag == tag
			}
			if !stored {
				return invalidTag(tag)
			}
		}
		if err := tx.SetDistTag(ctx, rp.Name, name, tag, version); err != nil {
			return err
		}
		return h.writePackument(ctx, tx, rp, name)
	})
}

func (h *Handler) deleteDistTag(ctx context.Context, rp *repo.Repository, name, tag string) error {
	if tag == "latest" {
		return repo.Errorf(http.StatusBadRequest, "the latest tag cannot be removed")
	}
	unlock, err := h.Store.Lock(ctx, "npm\x00"+rp.Name+"\x00"+name)
	if err != nil {
		return err
	}
	defer unlock()
	return h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		err := tx.DeleteDistTag(ctx, rp.Name, name, tag)
		if errors.Is(err, meta.ErrNotFound) {
			return repo.Errorf(http.StatusNotFound, "dist-tag %q not found", tag)
		}
		if err != nil {
			return err
		}
		return h.writePackument(ctx, tx, rp, name)
	})
}

// writePackument regenerates the stored package document from the rows in tx.
func (h *Handler) writePackument(ctx context.Context, tx *meta.Tx, rp *repo.Repository, name string) error {
	pkgs, err := tx.ListPackages(ctx, rp.Name, name)
	if err != nil {
		return err
	}
	tags, err := tx.ListDistTags(ctx, rp.Name, name)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(buildPackument(pw, name, pkgs, tags))
	}()
	sums, err := h.Store.WriteBlob(rp.Name, pr, nil)
	pr.Close()
	<-done
	if err != nil {
		return err
	}
	return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, PackumentKey(name), "application/json", sums))
}

// buildPackument writes the npm package document for the given rows. Tarball
// URLs inside the stored manifests are replaced when the document is served.
func buildPackument(w io.Writer, name string, pkgs []meta.Package, tags []meta.DistTag) error {
	tw := newTokenWriter(w)
	tw.Begin(true)
	tw.Key("_id")
	tw.Value(name)
	tw.Key("_rev")
	tw.Value(packumentRev(pkgs))
	tw.Key("name")
	tw.Value(name)
	tw.Key("dist-tags")
	tw.Begin(true)
	for _, t := range tags {
		tw.Key(t.Tag)
		tw.Value(t.Version)
	}
	tw.End()
	tw.Key("versions")
	tw.Begin(true)
	for _, p := range pkgs {
		tw.Key(p.Version)
		tw.RawValue([]byte(p.Attrs))
	}
	tw.End()
	tw.Key("time")
	tw.Begin(true)
	if len(pkgs) > 0 {
		created, modified := pkgs[0].CreatedAt, pkgs[0].CreatedAt
		for _, p := range pkgs {
			if p.CreatedAt.After(modified) {
				modified = p.CreatedAt
			}
		}
		tw.Key("created")
		tw.Value(npmTime(created))
		tw.Key("modified")
		tw.Value(npmTime(modified))
		for _, p := range pkgs {
			tw.Key(p.Version)
			tw.Value(npmTime(p.CreatedAt))
		}
	}
	tw.End()
	tw.End()
	return tw.Flush()
}

// packumentRev is the _rev of the document for pkgs: when its newest version
// was published, in Unix milliseconds. npm sends it back in unpublish URLs
// ({pkg}/-rev/{rev}), and unpublish refuses to delete a version published
// after it; removals and dist-tag and deprecation edits never do.
func packumentRev(pkgs []meta.Package) string {
	var newest int64
	for _, p := range pkgs {
		newest = max(newest, p.CreatedAt.UnixMilli())
	}
	return strconv.FormatInt(newest, 10)
}

func npmTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
