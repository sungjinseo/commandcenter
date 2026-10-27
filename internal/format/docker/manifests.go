package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// maxManifest bounds a manifest (Distribution's limit).
const maxManifest = 4 << 20

// Manifest media types.
const (
	mtSchema2      = "application/vnd.docker.distribution.manifest.v2+json"
	mtManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	mtOCIManifest  = "application/vnd.oci.image.manifest.v1+json"
	mtOCIIndex     = "application/vnd.oci.image.index.v1+json"
)

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform,omitempty"`
}

// manifestAttrs is a manifest row's Attrs: its media type, size and the
// digests it references, so a delete can tell which blobs became garbage.
type manifestAttrs struct {
	MediaType string   `json:"mediaType"`
	Size      int64    `json:"size"`                // of the manifest document
	Blobs     []string `json:"blobs,omitempty"`     // config and layers
	Manifests []string `json:"manifests,omitempty"` // an index's platform manifests
	// For the web UI: the bytes the image's layers add up to (0 for an
	// index, whose children carry their own) and the platforms, os/arch
	// [/variant], an index lists or a manifest's config implies.
	LayerBytes int64    `json:"layerBytes,omitempty"`
	Platforms  []string `json:"platforms,omitempty"`
}

// parseManifest validates body as a schema2 manifest or list, or an OCI
// image manifest or index, and returns what it references. The media type
// is the document's, else the request's Content-Type (OCI manifests may
// omit it).
func parseManifest(body []byte, contentType string) (manifestAttrs, error) {
	invalid := func(format string, args ...any) error {
		return Err(http.StatusBadRequest, CodeManifestInvalid, fmt.Sprintf(format, args...))
	}
	var doc struct {
		SchemaVersion int          `json:"schemaVersion"`
		MediaType     string       `json:"mediaType"`
		Config        *descriptor  `json:"config"`
		Layers        []descriptor `json:"layers"`
		Manifests     []descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return manifestAttrs{}, invalid("manifest is not valid JSON: %v", err)
	}
	mt := doc.MediaType
	if mt == "" {
		mt, _, _ = mime.ParseMediaType(contentType)
	}
	if doc.SchemaVersion == 1 || strings.HasPrefix(mt, "application/vnd.docker.distribution.manifest.v1+") {
		return manifestAttrs{}, invalid("schema 1 manifests are not supported; push with a current client")
	}
	if doc.SchemaVersion != 2 {
		return manifestAttrs{}, invalid("unsupported schemaVersion %d", doc.SchemaVersion)
	}
	m := manifestAttrs{MediaType: mt, Size: int64(len(body))}
	switch mt {
	case mtSchema2, mtOCIManifest:
		if doc.Config == nil {
			return manifestAttrs{}, invalid("manifest has no config")
		}
		for _, d := range append([]descriptor{*doc.Config}, doc.Layers...) {
			if !ValidDigest(d.Digest) {
				return manifestAttrs{}, invalid("invalid digest %q in manifest", d.Digest)
			}
			m.Blobs = append(m.Blobs, d.Digest)
		}
		for _, d := range doc.Layers {
			m.LayerBytes += d.Size
		}
	case mtManifestList, mtOCIIndex:
		for _, d := range doc.Manifests {
			if !ValidDigest(d.Digest) {
				return manifestAttrs{}, invalid("invalid digest %q in manifest list", d.Digest)
			}
			m.Manifests = append(m.Manifests, d.Digest)
			if p := d.Platform; p != nil && p.OS != "" && p.Architecture != "" {
				s := p.OS + "/" + p.Architecture
				if p.Variant != "" {
					s += "/" + p.Variant
				}
				m.Platforms = append(m.Platforms, s)
			}
		}
	default:
		return manifestAttrs{}, Err(http.StatusUnsupportedMediaType, CodeUnsupported, "unsupported manifest media type "+mt)
	}
	return m, nil
}

// putManifest stores the manifest bytes verbatim (the digest is theirs) once
// every blob and manifest it references exists in the image, then points
// the tag at it. A tag an allow_once repository already has can only be
// pushed again with the same digest.
func (h *Handler) putManifest(w http.ResponseWriter, r *http.Request, rp *repo.Repository, rt Route, c caller) error {
	if !ValidDigest(rt.Reference) && !ValidTag(rt.Reference) {
		return Err(http.StatusBadRequest, CodeManifestInvalid, "invalid tag "+rt.Reference)
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxManifest))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return Err(http.StatusRequestEntityTooLarge, CodeManifestInvalid, fmt.Sprintf("manifest is larger than %d bytes", maxManifest))
		}
		return err
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if ValidDigest(rt.Reference) && rt.Reference != digest {
		return Err(http.StatusBadRequest, CodeDigestInvalid, "manifest digest is "+digest+", not "+rt.Reference)
	}
	m, err := parseManifest(body, r.Header.Get("Content-Type"))
	if err != nil {
		return err
	}
	ctx := r.Context()
	unlock := h.locks.Lock(lockKey(rp, rt.Image))
	defer unlock()
	for _, d := range m.Blobs {
		if _, err := h.Store.DB.GetAsset(ctx, rp.Name, blobKey(rt.Image, d)); err != nil {
			return unknownRef(err, "blob", d)
		}
	}
	for _, d := range m.Manifests {
		if _, err := h.Store.DB.GetAsset(ctx, rp.Name, manifestKey(rt.Image, d)); err != nil {
			return unknownRef(err, "manifest", d)
		}
	}
	sums, err := h.Store.WriteBlob(rp.Name, bytes.NewReader(body), nil)
	if err != nil {
		return err
	}
	attrs, _ := json.Marshal(m)
	err = h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		err := tx.InsertPackage(ctx, meta.Package{Repo: rp.Name, Format: "docker", Name: rt.Image, Version: digest, Attrs: string(attrs), CreatedAt: h.now()})
		if err != nil && !errors.Is(err, meta.ErrExists) {
			return err
		}
		if err := tx.PutAsset(ctx, h.Store.NewAsset(rp.Name, manifestKey(rt.Image, digest), m.MediaType, sums)); err != nil {
			return err
		}
		if ValidDigest(rt.Reference) {
			return nil
		}
		if rp.WritePolicy == "allow_once" {
			tags, err := tx.ListDistTags(ctx, rp.Name, rt.Image)
			if err != nil {
				return err
			}
			for _, t := range tags {
				if t.Tag == rt.Reference && t.Version != digest {
					return Err(http.StatusConflict, CodeDenied, fmt.Sprintf("tag %s already exists in repository %s (writePolicy allow_once)", rt.Reference, rp.Name))
				}
			}
		}
		return tx.SetDistTag(ctx, rp.Name, rt.Image, rt.Reference, digest)
	})
	if err != nil {
		return err
	}
	h.Store.EmitAs("upload", c.actor(), rp.Name, subject(rt.Image, rt.Reference), digest)
	w.Header().Set("Location", "/v2/"+rt.Name()+"/manifests/"+digest)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
	return nil
}

func unknownRef(err error, kind, digest string) error {
	if !errors.Is(err, meta.ErrNotFound) {
		return err
	}
	return &Error{Status: http.StatusNotFound, Code: CodeManifestBlobUnknown,
		Message: kind + " unknown to registry: " + digest, Detail: map[string]string{"digest": digest}}
}

// deleteManifest removes a manifest, the tags that point at it, and the
// blob rows no other manifest of the image references (the compact task
// reclaims the blobs). actor is recorded on the event.
func (h *Handler) deleteManifest(ctx context.Context, rp *repo.Repository, image, digest, actor string) error {
	unlock := h.locks.Lock(lockKey(rp, image))
	defer unlock()
	err := h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		pkgs, err := tx.ListPackages(ctx, rp.Name, image)
		if err != nil {
			return err
		}
		var gone *manifestAttrs
		used := map[string]bool{}
		for _, p := range pkgs {
			var m manifestAttrs
			json.Unmarshal([]byte(p.Attrs), &m)
			if p.Version == digest {
				gone = &m
				continue
			}
			for _, d := range m.Blobs {
				used[d] = true
			}
		}
		if gone == nil {
			return Err(http.StatusNotFound, CodeManifestUnknown, "manifest unknown: "+digest)
		}
		if err := tx.DeletePackage(ctx, rp.Name, image, digest); err != nil {
			return err
		}
		if _, err := tx.DeleteAsset(ctx, rp.Name, manifestKey(image, digest)); offline(err) != nil {
			return err
		}
		tags, err := tx.ListDistTags(ctx, rp.Name, image)
		if err != nil {
			return err
		}
		for _, t := range tags {
			if t.Version == digest {
				if err := tx.DeleteDistTag(ctx, rp.Name, image, t.Tag); offline(err) != nil {
					return err
				}
			}
		}
		for _, d := range gone.Blobs {
			if used[d] {
				continue
			}
			if _, err := tx.DeleteAsset(ctx, rp.Name, blobKey(image, d)); offline(err) != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	h.Store.EmitAs("delete", actor, rp.Name, image+"@"+digest, "")
	return nil
}

// DeleteComponent removes one manifest (Name is the image, Version its
// digest) for the web UI's delete; cleanup policies never apply to docker
// repositories.
func (h *Handler) DeleteComponent(ctx context.Context, rp *repo.Repository, c component.Component, actor string) (component.Result, error) {
	if rp.Format != "docker" || rp.Type != "hosted" {
		return component.Result{}, fmt.Errorf("docker component delete in %s %s repository %q: %w", rp.Format, rp.Type, rp.Name, component.ErrNotSupported)
	}
	if err := c.CheckTarget(rp.Name, rp.Format); err != nil {
		return component.Result{}, err
	}
	if c.Name == "" || !ValidDigest(c.Version) {
		return component.Result{}, fmt.Errorf("docker component %q is incomplete", c.Label())
	}
	a, err := h.Store.DB.GetAsset(ctx, rp.Name, manifestKey(c.Name, c.Version))
	if err != nil {
		return component.Result{}, err
	}
	if err := h.deleteManifest(ctx, rp, c.Name, c.Version, actor); err != nil {
		return component.Result{}, err
	}
	var res component.Result
	res.Add(a.Size)
	return res, nil
}
