package maven

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/resolve"
)

// Handler serves Maven 2 repositories.
type Handler struct {
	Store    *repo.Store
	Resolver *resolve.Resolver
	Now      func() time.Time

	// Auth checks overwrites for Edit; nil leaves every check to the server.
	Auth *auth.Authenticator
}

// prefixesPath is the repository prefix list Maven 3.10+ consults before
// asking a repository for a groupId.
const prefixesPath = ".meta/prefixes.txt"

func New(store *repo.Store, resolver *resolve.Resolver) *Handler {
	return &Handler{Store: store, Resolver: resolver, Now: time.Now}
}

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, rp *repo.Repository, path string) {
	p, err := ParsePath(path)
	if err != nil {
		repo.WriteError(w, r, err)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.get(w, r, rp, p)
	case http.MethodPut, http.MethodDelete:
		target, err := repo.WriteTarget(rp)
		if err != nil {
			repo.WriteError(w, r, repo.WithAllow(err, "GET, HEAD"))
			return
		}
		if r.Method == http.MethodPut {
			err = validateWritePath(p.Path)
			if err == nil {
				err = h.put(r, target, p)
			}
			if err == nil {
				w.WriteHeader(http.StatusCreated)
			}
		} else {
			err = h.delete(r, target, p)
			if err == nil {
				w.WriteHeader(http.StatusNoContent)
			}
		}
		if err != nil {
			repo.WriteError(w, r, err)
		}
	default:
		allow := "GET, HEAD"
		if repo.AcceptsWrites(rp) {
			allow += ", PUT, DELETE"
		}
		repo.WriteError(w, r, repo.MethodNotAllowed(allow, "method %s not allowed", r.Method))
	}
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, rp *repo.Repository, p Path) {
	ctx := r.Context()
	var (
		a   meta.Asset
		res proxy.Result
		err error
	)
	if rp.Type == "group" && p.Main == prefixesPath {
		// A member's prefix list (Central's, say) would make Maven refuse
		// every groupId of the other members: a group has none (QA H-1).
		repo.WriteError(w, r, repo.ErrNotFound)
		return
	}
	if rp.Type != "hosted" && !p.fetchable() {
		// Only Maven files are fetched and cached: a directory path would
		// cache the upstream's HTML listing as a file (QA L-12).
		repo.WriteError(w, r, repo.ErrNotFound)
		return
	}
	// A text/html answer for a file not named .html is an upstream directory
	// listing or error page: it is not found and not cached (QA L-12).
	t := proxy.Target{Key: p.Main, Remote: p.Main, NotHTML: !strings.HasSuffix(p.Main, ".html")}
	if p.Metadata && rp.Type == "group" {
		a, err = h.Resolver.MergedTarget(ctx, rp, t, "application/xml", mergeMetadata)
	} else {
		kind := proxy.Content
		if p.Metadata {
			kind = proxy.Metadata
		}
		res, err = h.Resolver.LookupTarget(ctx, rp, t, kind)
		a = res.Asset
	}
	if err != nil {
		repo.WriteError(w, r, err)
		return
	}
	proxy.SetStaleHeader(w, res)
	if p.Hash != "" {
		repo.ServeText(w, r, checksum(a, p.Hash), "text/plain", a.LastModified)
		return
	}
	a.ContentType = ContentType(p.Main)
	repo.ServeAsset(w, r, h.Store, a)
}

func checksum(a meta.Asset, hash string) string {
	switch hash {
	case "sha1":
		return a.SHA1
	case "md5":
		return a.MD5
	case "sha256":
		return a.BlobSHA256
	case "sha512":
		return a.SHA512
	}
	return ""
}

// mergeMetadata is the resolve.MergeFunc for group maven-metadata.xml.
func mergeMetadata(_ context.Context, members []io.ReadSeeker, w io.Writer) error {
	docs := make([]*Metadata, 0, len(members))
	for _, m := range members {
		md, err := ParseMetadata(m)
		if err != nil {
			slog.Warn("skipping unparsable maven-metadata.xml in group merge", "err", err)
			continue
		}
		docs = append(docs, md)
	}
	var buf bytes.Buffer
	if err := WriteMetadata(&buf, Merge(docs...)); err != nil {
		return err
	}
	_, err := buf.WriteTo(w)
	return err
}

func (h *Handler) put(r *http.Request, rp *repo.Repository, p Path) error {
	ctx := r.Context()
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	if p.Hash != "" {
		// Checksums are generated from the stored content; uploaded ones are ignored.
		io.Copy(io.Discard, io.LimitReader(r.Body, 4096))
		return nil
	}
	if err := checkVersionPolicy(rp.VersionPolicy, p); err != nil {
		return err
	}
	if p.Metadata {
		return h.putMetadata(ctx, rp, p.Main, r.Body)
	}
	c := p.Coords
	if c == nil {
		c = sbtLegacy(p.Main)
	}
	if c == nil {
		return repo.Errorf(http.StatusBadRequest, "%q is not a valid maven artifact path", p.Main)
	}
	body, err := repo.StrictBody(rp, p.Main, r.Header.Get("Content-Type"), r.Body)
	if err != nil {
		return err
	}
	if err := h.storeArtifact(ctx, rp, p, c, body); err != nil {
		return err
	}
	if !strings.HasSuffix(p.Main, ".asc") { // signatures are sidecars, not components
		h.Store.Emit(r, "upload", rp.Name, p.Main, "")
	}
	if p.Coords == nil {
		return nil // an sbt legacy-layout file: no version of the GA, so no metadata changes
	}
	// Best-effort: the artifact is already stored, so failing the PUT would make
	// a retry hit allow_once; the next write to this GA, or the GA
	// maven-metadata.xml upload that ends mvn deploy, rebuilds the metadata anyway.
	if err := h.rebuildGA(ctx, rp, p.Coords.GroupID, p.Coords.ArtifactID, false); err != nil {
		slog.Warn("maven-metadata.xml rebuild failed", "repo", rp.Name, "path", p.Main, "err", err)
	}
	if p.Coords.Snapshot {
		if err := h.rebuildSnapshotVersion(ctx, rp, dir(p.Main)); err != nil {
			slog.Warn("snapshot version metadata rebuild failed", "repo", rp.Name, "path", p.Main, "err", err)
		}
	}
	return nil
}

// storeArtifact writes the asset. The per-asset lock makes the writePolicy
// and privilege checks and the write atomic: overwriting an existing asset
// (writePolicy allow) needs Edit, a new one only the Add the server checked
// (plan 12 Decision 2). A POM is checked once on disk, before it is recorded,
// against c: p.Coords, or sbtLegacy's for a file in sbt's legacy layout.
func (h *Handler) storeArtifact(ctx context.Context, rp *repo.Repository, p Path, c *Coordinates, body io.Reader) error {
	unlock, err := h.Store.Lock(ctx, "maven\x00asset\x00"+rp.Name+"\x00"+p.Main)
	if err != nil {
		return err
	}
	defer unlock()
	_, err = h.Store.Get(ctx, rp.Name, p.Main)
	switch {
	case err == nil && rp.WritePolicy == "allow_once":
		return repo.Errorf(http.StatusBadRequest, "repository %q does not allow updating assets: %s", rp.Name, p.Main)
	case err == nil && !h.Auth.Permits(ctx, rp.Name, auth.Edit):
		return repo.Errorf(http.StatusForbidden, "user %q may not overwrite %s", auth.FromContext(ctx).User, p.Main)
	case err != nil && !errors.Is(err, repo.ErrNotFound):
		return err
	}
	var verify func(blob.Sums) error
	if c.Extension == "pom" && c.Classifier == "" {
		body = http.MaxBytesReader(nil, io.NopCloser(body), maxPOMSize)
		verify = func(s blob.Sums) error {
			select {
			case pomSem <- struct{}{}:
				defer func() { <-pomSem }()
			case <-ctx.Done():
				return ctx.Err()
			}
			f, err := h.Store.Open(meta.Asset{Repo: rp.Name, BlobSHA256: s.SHA256})
			if err != nil {
				return err
			}
			defer f.Close()
			return checkPOM(f, c)
		}
	}
	sums, err := h.Store.WriteBlob(rp.Name, body, verify)
	if err != nil {
		return err
	}
	return h.Store.DB.PutAsset(ctx, h.Store.NewAsset(rp.Name, p.Main, ContentType(p.Main), sums))
}

func (h *Handler) delete(r *http.Request, rp *repo.Repository, p Path) error {
	if rp.WritePolicy == "deny" {
		return repo.Errorf(http.StatusBadRequest, "repository %q is read-only", rp.Name)
	}
	if p.Hash != "" {
		return nil // checksums are generated, nothing is stored
	}
	ctx := r.Context()
	if err := h.Store.Delete(ctx, rp.Name, p.Main); err != nil {
		return err
	}
	if !p.Metadata && !strings.HasSuffix(p.Main, ".asc") {
		h.Store.Emit(r, "delete", rp.Name, p.Main, "")
	}
	if p.Coords != nil {
		// Best-effort, as for PUT: the artifact is already gone, so a metadata
		// failure is logged instead of failing the delete.
		if err := h.rebuildGA(ctx, rp, p.Coords.GroupID, p.Coords.ArtifactID, true); err != nil {
			slog.Warn("maven-metadata.xml rebuild failed", "repo", rp.Name, "path", p.Main, "err", err)
		}
		if p.Coords.Snapshot {
			if err := h.rebuildSnapshotVersion(ctx, rp, dir(p.Main)); err != nil {
				slog.Warn("snapshot version metadata rebuild failed", "repo", rp.Name, "path", p.Main, "err", err)
			}
		}
	}
	return nil
}

func checkVersionPolicy(policy string, p Path) error {
	snapshot := isSnapshotPath(p)
	switch {
	case policy == "release" && snapshot:
		return repo.Errorf(http.StatusBadRequest, "repository version policy RELEASE does not allow SNAPSHOT: %s", p.Main)
	case policy == "release" && timestampedVersionRE.MatchString(versionDir(p)):
		return repo.Errorf(http.StatusBadRequest, "repository version policy RELEASE does not allow timestamped SNAPSHOT versions: %s", p.Main)
	case policy == "snapshot" && !snapshot && !p.Metadata:
		return repo.Errorf(http.StatusBadRequest, "repository version policy SNAPSHOT does not allow releases: %s", p.Main)
	}
	return nil
}

func isSnapshotPath(p Path) bool {
	if p.Coords != nil {
		return p.Coords.Snapshot
	}
	return strings.HasSuffix(versionDir(p), "-SNAPSHOT")
}

// versionDir is the version directory of an artifact path, or the parent
// directory name of any other path ("" at the top level).
func versionDir(p Path) string {
	if p.Coords != nil {
		return p.Coords.BaseVersion
	}
	segs := strings.Split(p.Main, "/")
	if len(segs) < 2 {
		return ""
	}
	return segs[len(segs)-2]
}

// metadataSem bounds how many maven-metadata.xml uploads are processed at
// once: decoding allocates about 20 times the document's size, and four
// concurrent 10 MiB documents OOM-killed a 128 MiB container.
var metadataSem = make(chan struct{}, 2)

// metadataWaitTimeout bounds how long an upload waits for a slot (503 on expiry).
var metadataWaitTimeout = 30 * time.Second

// metadataReadTimeout bounds how long an upload holding a slot may take to
// send its body, however steadily it sends (408 on expiry).
var metadataReadTimeout = 10 * time.Second

// putMetadata accepts an uploaded maven-metadata.xml. Versioning is owned by
// the server: GA documents are built from the stored artifacts (rebuildGA,
// QA L-11) and SNAPSHOT version-level documents from the stored builds
// (rebuildSnapshotVersion, plan 13 Decision 6), so a stale or invented
// version, <release> or groupId never reaches clients. A GA document that
// names the GA of its directory has that GA's document rebuilt, and the
// <plugins> of a plugin-group document are merged into the stored one; the
// rest of an upload is accepted and dropped.
func (h *Handler) putMetadata(ctx context.Context, rp *repo.Repository, path string, body io.Reader) error {
	select {
	case metadataSem <- struct{}{}:
		defer func() { <-metadataSem }()
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(metadataWaitTimeout):
		return repo.Errorf(http.StatusServiceUnavailable, "too many concurrent maven-metadata.xml uploads, retry")
	}
	// The body is read only in a slot, so at most cap(metadataSem) bodies are
	// in memory however many connections upload, and by a deadline, so a
	// client that sends slowly loses the slot instead of keeping it.
	if rc, ok := body.(io.ReadCloser); ok {
		body = repo.WithDeadline(rc, time.Now().Add(metadataReadTimeout))
	}
	// Parse the client body before taking the lock so slow uploads do not block the GA directory.
	up, err := ParseMetadata(http.MaxBytesReader(nil, io.NopCloser(body), maxUploadedMetadata))
	if err != nil {
		switch {
		case errors.As(err, new(*http.MaxBytesError)):
			return err
		case errors.Is(err, os.ErrDeadlineExceeded):
			return repo.Errorf(http.StatusRequestTimeout, "maven-metadata.xml not received within %v", metadataReadTimeout)
		}
		return repo.Errorf(http.StatusBadRequest, "invalid maven-metadata.xml: %v", err)
	}
	if isSnapshotPath(Path{Main: path}) {
		return nil
	}
	// mvn deploy uploads the GA document after the artifacts: rebuilding it
	// then repairs a rebuild that failed after an artifact PUT. Only a
	// document naming the GA of its directory does (a plugin-group document
	// names none), and its versions are still ignored.
	d := dir(path)
	if gaDir(up.GroupID, up.ArtifactID) == d {
		if err := h.rebuildGA(ctx, rp, up.GroupID, up.ArtifactID, false); err != nil {
			return err
		}
	}
	if len(up.Plugins) == 0 {
		return nil
	}
	unlock, err := h.Store.Lock(ctx, "maven\x00"+rp.Name+"\x00"+d)
	if err != nil {
		return err
	}
	defer unlock()
	existing, err := h.loadMetadata(ctx, rp, path)
	if err != nil {
		return err
	}
	return h.storeMetadata(ctx, rp, path, Merge(existing, &Metadata{Plugins: up.Plugins}))
}

// rebuildGA updates groupId/artifactId/maven-metadata.xml from the stored artifacts.
// Everything but <plugins> is the server's and regenerated (QA L-11); the
// plugins of the stored document (a groupId directory that is also a GA) are
// kept. With replace (used after deletes), a GA without artifacts loses its
// versioning: the document is removed, or keeps only its plugins; without, a
// GA without artifacts gets no document.
func (h *Handler) rebuildGA(ctx context.Context, rp *repo.Repository, groupID, artifactID string, replace bool) error {
	gaDir := strings.ReplaceAll(groupID, ".", "/") + "/" + artifactID
	unlock, err := h.Store.Lock(ctx, "maven\x00"+rp.Name+"\x00"+gaDir)
	if err != nil {
		return err
	}
	defer unlock()

	// The paths are streamed: memory grows with the GA's versions, not with
	// its files (a SNAPSHOT GA that CI deploys to can hold 10^5 of them).
	seen := map[string]bool{}
	var versions []string
	err = h.Store.DB.WalkAssetPaths(ctx, rp.Name, gaDir+"/", func(path string) {
		pp, err := ParsePath(path)
		if err != nil || pp.Coords == nil || pp.Coords.GroupID != groupID || pp.Coords.ArtifactID != artifactID {
			return
		}
		if !seen[pp.Coords.BaseVersion] {
			seen[pp.Coords.BaseVersion] = true
			versions = append(versions, pp.Coords.BaseVersion)
		}
	})
	if err != nil {
		return err
	}
	path := gaDir + "/maven-metadata.xml"
	if len(versions) == 0 && !replace {
		return nil
	}
	existing, err := h.loadMetadata(ctx, rp, path)
	if err != nil {
		return err
	}
	var plugins []Plugin
	if existing != nil {
		plugins = existing.Plugins
	}
	if len(versions) == 0 {
		if len(plugins) > 0 {
			return h.storeMetadata(ctx, rp, path, Merge(&Metadata{Plugins: plugins}))
		}
		if err := h.Store.Delete(ctx, rp.Name, path); err != nil && !errors.Is(err, repo.ErrNotFound) {
			return err
		}
		return nil
	}
	m := ForVersions(groupID, artifactID, versions, h.Now())
	m.Plugins = plugins
	return h.storeMetadata(ctx, rp, path, m)
}

// loadMetadata returns the stored document, or nil if there is none or it is unreadable.
func (h *Handler) loadMetadata(ctx context.Context, rp *repo.Repository, path string) (*Metadata, error) {
	a, err := h.Store.Get(ctx, rp.Name, path)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f, err := h.Store.Open(a)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := ParseMetadata(f)
	if err != nil {
		slog.Warn("ignoring unparsable stored maven-metadata.xml", "repo", rp.Name, "path", path, "err", err)
		return nil, nil
	}
	return m, nil
}

func (h *Handler) storeMetadata(ctx context.Context, rp *repo.Repository, path string, m *Metadata) error {
	var buf bytes.Buffer
	if err := WriteMetadata(&buf, m); err != nil {
		return err
	}
	_, err := h.Store.Put(ctx, rp.Name, path, "application/xml", &buf)
	return err
}

func dir(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}
