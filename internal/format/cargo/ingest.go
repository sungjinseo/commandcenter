package cargo

import (
	"bytes"
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

// Ingest stores one .crate in the hosted repository rp together with its
// sparse-index line (as served by the source registry), and reports whether it
// was stored or skipped. The crate is streamed to a blob; the line's cksum must
// equal the crate's sha256 and its name and vers must be valid and match the
// crate's contents, checked as a publish checks them. The line is stored in
// Command Center's own index form with yanked preserved. A version already
// stored with the same crate is Skipped, a different crate for it is a
// conflict (repo.IsIngestConflict); nothing is overwritten. It makes no
// privilege checks and ignores the repository's write policy: the caller is
// the admin import.
func (h *Handler) Ingest(ctx context.Context, rp *repo.Repository, crate io.Reader, indexLineJSON []byte, actor string) (repo.IngestResult, error) {
	if len(indexLineJSON) > maxLine {
		return "", repo.Errorf(http.StatusRequestEntityTooLarge, "index entry is larger than %d bytes", maxLine)
	}
	var l indexLine
	dec := json.NewDecoder(bytes.NewReader(indexLineJSON))
	if err := dec.Decode(&l); err != nil {
		return "", badPublish("invalid index line: %v", err)
	}
	if !ValidName(l.Name) {
		return "", badPublish("invalid crate name %q", l.Name)
	}
	if !validVersion(l.Vers) {
		return "", badPublish("invalid version %q", l.Vers)
	}
	if len(l.Cksum) != 64 {
		return "", badPublish("index line of %s@%s has no sha256 cksum", l.Name, l.Vers)
	}
	if l.Deps == nil {
		l.Deps = []indexDep{}
	}
	if l.Features == nil {
		l.Features = map[string][]string{}
	}
	name := strings.ToLower(l.Name)

	sums, err := h.Store.WriteBlob(rp.Name, io.LimitReader(crate, maxCrate+1), func(s blob.Sums) error {
		if s.Size == 0 {
			return badPublish("%s@%s is an empty crate", l.Name, l.Vers)
		}
		if s.Size > maxCrate {
			return repo.Errorf(http.StatusRequestEntityTooLarge, "crate is larger than %d bytes", maxCrate)
		}
		if !strings.EqualFold(s.SHA256, l.Cksum) {
			return badPublish("cksum of %s@%s does not match the crate (index %s, crate %s)", l.Name, l.Vers, l.Cksum, s.SHA256)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if err := h.validateCrate(rp, sums, l.Name, l.Vers); err != nil {
		return "", err
	}
	l.Cksum = sums.SHA256
	line, err := l.marshal()
	if err != nil {
		return "", err
	}
	if len(line) > maxLine {
		return "", repo.Errorf(http.StatusRequestEntityTooLarge, "index entry is larger than %d bytes", maxLine)
	}

	unlock := h.locks.Lock(rp.Name + "\x00" + canonicalName(name))
	defer unlock()
	if err := h.checkName(ctx, rp, l.Name); err != nil {
		return "", repo.IngestConflict("%v", err)
	}
	res := repo.Ingested
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		res = repo.Ingested
		pkgs, err := tx.ListPackages(ctx, rp.Name, name)
		if err != nil {
			return err
		}
		if len(pkgs) == 0 && reservedName(l.Name) { // as a publish refuses it
			return badPublish("crate name %q is reserved", l.Name)
		}
		for _, p := range pkgs {
			if stripBuild(p.Version) != stripBuild(l.Vers) {
				continue
			}
			a, err := tx.GetAsset(ctx, rp.Name, crateKey(name, p.Version))
			if p.Version == l.Vers && err == nil && a.BlobSHA256 == sums.SHA256 {
				res = repo.Skipped
				return nil
			}
			if err != nil && !errors.Is(err, meta.ErrNotFound) {
				return err
			}
			return repo.IngestConflict("crate version `%s@%s` already exists with different content", l.Name, l.Vers)
		}
		err = tx.InsertPackage(ctx, meta.Package{Repo: rp.Name, Format: "cargo", Name: name, Version: l.Vers, Attrs: string(line), Yanked: l.Yanked, CreatedAt: h.Now()})
		if errors.Is(err, meta.ErrExists) {
			return repo.IngestConflict("crate version `%s@%s` already exists", l.Name, l.Vers)
		}
		if err != nil {
			return err
		}
		return tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, crateKey(name, l.Vers), "application/octet-stream", sums))
	})
	if err != nil {
		return "", err
	}
	if res == repo.Ingested {
		h.Store.EmitAs("upload", actor, rp.Name, name+"@"+l.Vers, "")
	}
	return res, nil
}
