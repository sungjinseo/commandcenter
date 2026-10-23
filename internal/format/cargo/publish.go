package cargo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// maxCrate bounds a published .crate (crates.io allows 10 MiB); tests may lower it.
var maxCrate int64 = 50 << 20

// publish handles `cargo publish`: the crate streams to disk, then its index
// line and asset are recorded together.
func (h *Handler) publish(w http.ResponseWriter, r *http.Request, rp *repo.Repository) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMeta+maxCrate+8)
	m, crate, size, err := readPublish(r.Body)
	if err != nil {
		return err
	}
	if size == 0 {
		return badPublish("publish body has an empty crate")
	}
	if size > maxCrate {
		return repo.Errorf(http.StatusRequestEntityTooLarge, "crate is larger than %d bytes", maxCrate)
	}
	// The cksum has a fixed length, so the final line size is known before the crate is stored.
	if probe, err := m.indexLine(strings.Repeat("0", 64)).marshal(); err != nil {
		return err
	} else if len(probe) > maxLine {
		return repo.Errorf(http.StatusRequestEntityTooLarge, "index entry is larger than %d bytes", maxLine)
	}
	name := strings.ToLower(m.Name)
	ctx := r.Context()

	unlock := h.locks.Lock(rp.Name + "\x00" + canonicalName(name))
	defer unlock()
	if err := h.checkDuplicate(ctx, rp, m); err != nil {
		return err
	}

	sums, err := h.Store.WriteBlob(rp.Name, crate, func(s blob.Sums) error {
		if s.Size != size {
			return badPublish("crate is truncated: got %d of %d bytes", s.Size, size)
		}
		return nil
	})
	if err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			return repo.Errorf(http.StatusRequestEntityTooLarge, "publish body is too large")
		case errors.Is(err, io.ErrUnexpectedEOF):
			return badPublish("publish body ended early: %v", err)
		}
		return err
	}
	if err := h.validateCrate(rp, sums, m.Name, m.Vers); err != nil {
		return err
	}
	line, err := m.indexLine(sums.SHA256).marshal()
	if err != nil {
		return err
	}
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		err := tx.InsertPackage(ctx, meta.Package{Repo: rp.Name, Format: "cargo", Name: name, Version: m.Vers, Attrs: string(line), CreatedAt: h.Now()})
		if errors.Is(err, meta.ErrExists) {
			return repo.Errorf(http.StatusConflict, "crate version `%s@%s` is already uploaded", m.Name, m.Vers)
		}
		if err != nil {
			return err
		}
		return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, crateKey(name, m.Vers), "application/octet-stream", sums))
	})
	if err != nil {
		return err
	}
	h.Store.Emit(r, "upload", rp.Name, name+"@"+m.Vers, "")
	writeJSON(w, http.StatusOK, map[string]any{"warnings": map[string][]string{"invalid_categories": {}, "invalid_badges": {}, "other": {}}})
	return nil
}

// yank sets the yanked flag of a hosted crate version.
func (h *Handler) yank(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route, yanked bool) error {
	ctx := r.Context()
	unlock := h.locks.Lock(rp.Name + "\x00" + canonicalName(rt.Name))
	defer unlock()
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		pkgs, err := tx.ListPackages(ctx, rp.Name, rt.Name)
		if err != nil {
			return err
		}
		for _, p := range pkgs {
			if p.Version != rt.Version {
				continue
			}
			var line map[string]json.RawMessage
			if err := json.Unmarshal([]byte(p.Attrs), &line); err != nil {
				return err
			}
			line["yanked"], _ = json.Marshal(yanked)
			b, err := json.Marshal(line)
			if err != nil {
				return err
			}
			if err := tx.UpdatePackage(ctx, rp.Name, rt.Name, rt.Version, string(b)); err != nil {
				return err
			}
			return tx.SetYanked(ctx, rp.Name, rt.Name, rt.Version, yanked)
		}
		return repo.Errorf(http.StatusNotFound, "crate version `%s@%s` not found", rt.Name, rt.Version)
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// canonicalName folds the characters crates.io treats as equal in a crate name.
func canonicalName(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "_", "-")
}

// stripBuild drops semver build metadata.
func stripBuild(v string) string {
	before, _, _ := strings.Cut(v, "+")
	return before
}

// checkDuplicate applies the crates.io rules: a name differing only by case or
// `-`/`_` is taken, and versions equal apart from build metadata collide.
func (h *Handler) checkDuplicate(ctx context.Context, rp *repo.Repository, m publishMeta) error {
	name := strings.ToLower(m.Name)
	names, err := h.Store.DB.ListPackageNamesCanonical(ctx, rp.Name, canonicalName(name))
	if err != nil {
		return err
	}
	for _, n := range names {
		// Stored names are lowercased; the index line keeps the published spelling,
		// which a new version must match exactly.
		existing, err := h.Store.DB.ListPackages(ctx, rp.Name, n)
		if err != nil {
			return err
		}
		for _, p := range existing {
			var line struct {
				Name string `json:"name"`
			}
			if json.Unmarshal([]byte(p.Attrs), &line) != nil || line.Name == "" {
				line.Name = n
			}
			if line.Name != m.Name {
				return repo.Errorf(http.StatusConflict, "crate name %q conflicts with existing crate %q", m.Name, line.Name)
			}
			break
		}
	}
	pkgs, err := h.Store.DB.ListPackages(ctx, rp.Name, name)
	if err != nil {
		return err
	}
	for _, p := range pkgs {
		if stripBuild(p.Version) == stripBuild(m.Vers) {
			return repo.Errorf(http.StatusConflict, "crate version `%s@%s` is already uploaded", m.Name, m.Vers)
		}
	}
	return nil
}

// validateCrate checks the stored crate against the name and version it was
// published as. The blob is read back from disk, so nothing is buffered.
func (h *Handler) validateCrate(rp *repo.Repository, sums blob.Sums, name, vers string) error {
	bs, err := h.Store.BlobStore(rp.Name)
	if err != nil {
		return err
	}
	f, err := bs.Open(sums.SHA256)
	if err != nil {
		return err
	}
	defer f.Close()
	return checkCrate(f, name, vers)
}
