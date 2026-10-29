package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ImportParams are a repository.import run's.
type ImportParams struct {
	Source string // the export's directory (holding manifest.json), under the export root
	// Repositories maps exported names to the repositories they go to; empty
	// imports every exported repository under its own name.
	Repositories  map[string]string
	CreateMissing bool // create a missing repository from the exported definition
	Overwrite     bool // replace files whose content differs; otherwise they are kept and reported
}

// maxUnit bounds the files one transaction writes: a larger component (a
// SNAPSHOT version with many builds) is written in several.
const maxUnit = 1000

// errDeferred puts a docker index aside until the manifests it lists are in.
var errDeferred = errors.New("deferred")

// ReadManifest reads and checks <source>/manifest.json.
func (s *Service) ReadManifest(source string) (Manifest, error) {
	var m Manifest
	src, err := s.openSource(source)
	if err != nil {
		return m, err
	}
	defer src.Close()
	return readManifest(src)
}

func (s *Service) openSource(source string) (*os.Root, error) {
	rel, err := s.rel(source)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(s.Root)
	if err != nil {
		return nil, fmt.Errorf("export root: %w", err)
	}
	defer root.Close()
	src, err := root.OpenRoot(rel)
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", source, err)
	}
	return src, nil
}

func readManifest(src *os.Root) (Manifest, error) {
	var m Manifest
	b, err := src.ReadFile("manifest.json")
	if errors.Is(err, os.ErrNotExist) {
		return m, errors.New("the source has no manifest.json: it is not an export, or its export did not finish")
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("manifest.json: %w", err)
	}
	if m.FormatVersion != FormatVersion {
		return m, fmt.Errorf("manifest.json has format version %d; this version reads %d", m.FormatVersion, FormatVersion)
	}
	return m, nil
}

// Import writes the repositories of the export at p.Source into this
// server, for actor.
func (s *Service) Import(ctx context.Context, p ImportParams, actor string, log Logger) (Result, error) {
	var res Result
	src, err := s.openSource(p.Source)
	if err != nil {
		return res, err
	}
	defer src.Close()
	m, err := readManifest(src)
	if err != nil {
		return res, err
	}
	entries := m.Repositories
	if len(p.Repositories) > 0 {
		entries = nil
		for _, e := range m.Repositories {
			if _, ok := p.Repositories[e.Name]; ok {
				entries = append(entries, e)
			}
		}
		for name := range p.Repositories {
			if !slices.ContainsFunc(entries, func(e RepoEntry) bool { return e.Name == name }) {
				return res, fmt.Errorf("the export has no repository %q", name)
			}
		}
	}
	log.Infof("importing %d repositories from %s (export %s of %s)", len(entries), p.Source, m.ID, m.Created.Format(time.RFC3339))
	var failedRepos int64
	for _, e := range entries {
		dest := e.Name
		if d := p.Repositories[e.Name]; d != "" {
			dest = d
		}
		im := &importer{s: s, src: src, m: m, e: e, dest: dest, p: p, log: log, actor: actor}
		err := im.run(ctx)
		res.Assets, res.Bytes, res.Failed = res.Assets+im.written, res.Bytes+im.bytes, res.Failed+im.failed
		if err != nil {
			if ctx.Err() != nil {
				return res, err
			}
			failedRepos++
			log.Errorf("%s: %v", dest, err)
			continue
		}
		res.Repos++
	}
	log.Infof("imported %d files (%d bytes) into %d repositories", res.Assets, res.Bytes, res.Repos)
	switch {
	case failedRepos > 0:
		return res, fmt.Errorf("%d repositories could not be imported, %d components were skipped (see the log)", failedRepos, res.Failed)
	case res.Failed > 0:
		return res, fmt.Errorf("%d components were skipped (see the log)", res.Failed)
	}
	return res, nil
}

// importer is the import of one repository.
type importer struct {
	s     *Service
	src   *os.Root
	m     Manifest
	e     RepoEntry
	dest  string
	p     ImportParams
	log   Logger
	actor string

	rp      *repo.Repository
	bs      *blob.Store
	locker  Locker
	indexed bool // a hosted repository of a format with derived index documents
	dirty   bool // they need regenerating
	// written files, their bytes, unchanged ones, conflicts kept, rows written,
	// components skipped (failed), problems logged
	written, bytes, same, conflicts, rows, failed int64
	logged, progress                              int64
	deferred                                      [][]Line // docker indexes waiting for their manifests
}

// note logs a problem, at most maxLogged per repository.
func (im *importer) note(format string, args ...any) {
	if im.logged++; im.logged <= maxLogged {
		im.log.Warnf(im.dest+": "+format, args...)
	}
}

func (im *importer) run(ctx context.Context) error {
	if !filepath.IsLocal(im.e.Name) || strings.ContainsAny(im.e.Name, `/\`) {
		return fmt.Errorf("the export names a repository %q", im.e.Name)
	}
	if err := im.target(ctx); err != nil {
		return err
	}
	if sum, _, err := hashFile(im.src, im.e.AssetList); err != nil {
		return fmt.Errorf("%s: %w", im.e.AssetList, err)
	} else if sum != im.e.AssetListSHA256 {
		return fmt.Errorf("%s does not match the manifest (sha256 %s, not %s): the export is damaged", im.e.AssetList, sum, im.e.AssetListSHA256)
	}
	rebuild := im.s.Rebuild[im.rp.Format]
	im.indexed = im.rp.Type == "hosted" && derivedFormats[im.rp.Format]
	if im.indexed {
		empty, err := im.s.DB.ScanAssets(ctx, im.dest, "", 1)
		if err != nil {
			return err
		}
		// A copy into an empty repository from a full export keeps the
		// exported index documents; anything else regenerates them, as do
		// apt (signed with this server's key) and yum at another depth.
		im.dirty = len(empty) > 0 || !im.m.Since.IsZero() || im.rp.Format == "apt" ||
			im.rp.Format == "yum" && im.rp.Depth != im.e.Config.Depth
		if im.dirty && rebuild == nil {
			return fmt.Errorf("%s repositories are only imported into an empty repository from a full export here: "+
				"their index documents cannot be regenerated", im.rp.Format)
		}
	}
	err := im.pass(ctx, func(l Line) bool { return !im.isDerived(l) })
	for err == nil && len(im.deferred) > 0 { // docker indexes, once their manifests are in
		units, before := im.deferred, len(im.deferred)
		im.deferred = nil
		im.log.Infof("%s: importing %d manifest lists after the manifests they list", im.dest, before)
		for _, u := range units {
			if err = im.unit(ctx, u); err != nil {
				break
			}
		}
		if len(im.deferred) == before {
			for _, u := range im.deferred {
				im.failed++
				im.note("%s skipped: it lists manifests that are not in the repository", u[0].Path)
			}
			im.deferred, im.dirty = nil, true
		}
	}
	if err == nil && im.indexed {
		err = im.pass(ctx, im.isDerived)
	}
	if im.logged > maxLogged {
		im.log.Warnf("%s: %d more problems not listed", im.dest, im.logged-maxLogged)
	}
	switch {
	case !im.indexed || im.written+im.rows == 0 || !im.dirty && err == nil:
	case rebuild == nil:
		im.log.Warnf("%s: the %s index documents cannot be regenerated here: import into an empty repository again", im.dest, im.rp.Format)
	default:
		// After a stop too: what was imported must be listed.
		im.log.Infof("%s: regenerating the %s index documents", im.dest, im.rp.Format)
		if rerr := rebuild(context.WithoutCancel(ctx), im.rp, im.log.Infof); rerr != nil {
			err = errors.Join(err, fmt.Errorf("regenerating the index documents: %w", rerr))
		}
	}
	msg := fmt.Sprintf("%s: imported %d files (%d bytes) and %d package rows from %s; %d unchanged, %d kept (different content, overwrite off), %d components skipped",
		im.dest, im.written, im.bytes, im.rows, im.e.Name, im.same, im.conflicts, im.failed)
	im.log.Infof("%s", msg)
	im.s.emit("repo.import", im.actor, im.dest, im.m.ID, fmt.Sprintf("files=%d bytes=%d rows=%d unchanged=%d kept=%d skipped=%d",
		im.written, im.bytes, im.rows, im.same, im.conflicts, im.failed))
	return err
}

func (im *importer) isDerived(l Line) bool { return im.indexed && derived(im.rp.Format, l.Path) }

// target finds (or creates) the repository the import goes to.
func (im *importer) target(ctx context.Context) error {
	e := im.e
	rp := im.s.Repos.Get(im.dest)
	if rp == nil {
		if !im.p.CreateMissing {
			return fmt.Errorf("repository %q does not exist: create it, or set createMissing", im.dest)
		}
		if e.Type != "hosted" && e.Type != "proxy" {
			return fmt.Errorf("the export's repository %q is %s", e.Name, e.Type)
		}
		def := e.Config
		def.Name, def.Format, def.Type = im.dest, e.Format, e.Type
		if def.BlobStore != "" && !im.s.Store.HasStore(def.BlobStore) {
			im.log.Warnf("%s: blob store %q does not exist here: using the default store", im.dest, def.BlobStore)
			def.BlobStore = ""
		}
		if def.CleanupPolicy != "" && !im.policyExists(ctx, def.CleanupPolicy) {
			im.log.Warnf("%s: cleanup policy %q does not exist here: created without one", im.dest, def.CleanupPolicy)
			def.CleanupPolicy = ""
		}
		if def.RoutingRule != "" && im.s.Repos.Route(def.RoutingRule) == nil {
			im.log.Warnf("%s: routing rule %q does not exist here: created without one", im.dest, def.RoutingRule)
			def.RoutingRule = ""
		}
		def, err := im.s.Repos.CreateDef(ctx, def)
		if err != nil {
			return fmt.Errorf("creating repository %q: %w", im.dest, err)
		}
		im.s.emit("repo.create", im.actor, def.Name, def.Name, def.Format+"/"+def.Type)
		im.log.Infof("%s: created %s %s repository", im.dest, def.Format, def.Type)
		if rp = im.s.Repos.Get(im.dest); rp == nil {
			return fmt.Errorf("repository %q was deleted while it was imported into", im.dest)
		}
	}
	switch {
	case rp.Format != e.Format || rp.Type != e.Type:
		return fmt.Errorf("repository %q is %s %s; the export's %q is %s %s", im.dest, rp.Format, rp.Type, e.Name, e.Format, e.Type)
	case rp.Format == "maven" && rp.Type == "hosted" && rp.VersionPolicy != "mixed" && rp.VersionPolicy != e.Config.VersionPolicy:
		return fmt.Errorf("repository %q takes %s versions; the export's %q holds %s versions", im.dest, rp.VersionPolicy, e.Name, e.Config.VersionPolicy)
	}
	im.locker = im.s.Lockers[rp.Format]
	if im.locker == nil {
		return fmt.Errorf("%s repositories cannot be imported: the format offers no import locks", rp.Format)
	}
	bs, err := im.s.Store.BlobStore(rp.Name)
	if err != nil {
		return err
	}
	im.rp, im.bs = rp, bs
	return nil
}

func (im *importer) policyExists(ctx context.Context, name string) bool {
	if im.s.Repos.Policies == nil {
		return false
	}
	_, ok, err := im.s.Repos.Policies(ctx, name)
	return err == nil && ok
}

// pass reads assets.jsonl and imports the lines keep selects, the adjacent
// lines of one component together.
func (im *importer) pass(ctx context.Context, keep func(Line) bool) error {
	f, err := im.src.Open(im.e.AssetList)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var unit []Line
	flush := func() error {
		if len(unit) == 0 {
			return nil
		}
		err := im.unit(ctx, unit)
		unit = nil
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var l Line
		if err := dec.Decode(&l); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("%s: %w", im.e.AssetList, err)
		}
		if !keep(l) {
			continue
		}
		if len(unit) > 0 && (l.Component == "" || l.Component != unit[0].Component || len(unit) == maxUnit) {
			if err := flush(); err != nil {
				return err
			}
		}
		unit = append(unit, l)
	}
	return flush()
}

// item is one file of a unit as the import sees it.
type item struct {
	l     Line
	sums  blob.Sums
	write bool // its blob is in the store; otherwise it is there with the same content
}

// unit imports the files of one component in one transaction, under the
// locks an upload of them takes. A file whose blob is missing or does not
// match its hash skips the whole component.
func (im *importer) unit(ctx context.Context, lines []Line) error {
	label := lines[0].Path
	if lines[0].Component != "" {
		label = lines[0].Component
	}
	skip := func(format string, args ...any) error {
		im.failed++
		im.dirty = true
		im.note("%s skipped: "+format, append([]any{label}, args...)...)
		return nil
	}
	items := make([]item, 0, len(lines))
	for _, l := range lines {
		if err := im.check(l); err != nil {
			return skip("%s: %v", l.Path, err)
		}
		cur, err := im.s.DB.GetAsset(ctx, im.dest, l.Path)
		switch {
		case err == nil && cur.BlobSHA256 == l.SHA256:
			items = append(items, item{l: l})
			continue
		case err == nil && im.isDerived(l):
			im.dirty = true // regenerated afterwards, never a conflict
			continue
		case err == nil && !im.p.Overwrite:
			im.conflict(l.Path)
			continue
		case err != nil && !errors.Is(err, meta.ErrNotFound):
			return err
		}
		sums, err := im.copyBlob(l)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return skip("%s: %v", l.Path, err)
		}
		items = append(items, item{l: l, sums: sums, write: true})
	}
	if len(items) == 0 {
		return nil
	}
	paths := make([]string, len(items))
	for i, it := range items {
		paths[i] = it.l.Path
	}
	unlock := im.locker.LockPaths(im.rp, paths)
	defer unlock()
	var written, bytes, same, conflicts, rows int64
	var changed, keptTags []string
	err := im.s.Store.Tx(ctx, im.rp, func(tx *meta.Tx) error {
		written, bytes, same, conflicts, rows, changed, keptTags = 0, 0, 0, 0, 0, nil, nil
		for _, it := range items {
			cur, err := tx.GetAsset(ctx, im.dest, it.l.Path)
			switch {
			case err == nil && cur.BlobSHA256 == it.l.SHA256:
				it.write = false
			case err != nil && !errors.Is(err, meta.ErrNotFound):
				return err
			case !it.write || err == nil && !im.p.Overwrite: // written meanwhile
				conflicts++
				changed = append(changed, it.l.Path)
				continue
			}
			if im.rp.Format == "docker" && strings.Contains(it.l.Path, "/manifests/") {
				if err := dockerRefs(ctx, tx, im.dest, it.l); err != nil {
					return err
				}
			}
			if it.write {
				if err := tx.ImportAsset(ctx, im.asset(it)); err != nil {
					return err
				}
				written, bytes = written+1, bytes+it.l.Size
			} else {
				same++
			}
			n, err := im.writeRows(ctx, tx, it, &keptTags)
			if err != nil {
				return err
			}
			rows += n
		}
		return nil
	})
	var missing *missingRef
	switch {
	case errors.Is(err, errDeferred):
		im.deferred = append(im.deferred, lines)
		return nil
	case errors.As(err, &missing):
		return skip("%v", err)
	case err != nil:
		return err
	}
	for _, p := range changed {
		im.dirty = true
		im.note("%s kept: it was written while it was imported", p)
	}
	for _, k := range keptTags {
		im.conflicts++
		im.note("%s kept (set overwrite to move it)", k)
	}
	im.written, im.bytes, im.same, im.rows = im.written+written, im.bytes+bytes, im.same+same, im.rows+rows
	im.conflicts += conflicts
	if n := (im.written + im.same) / 10000; n > im.progress {
		im.progress = n
		im.log.Infof("%s: %d files so far", im.dest, im.written+im.same)
	}
	return nil
}

func (im *importer) conflict(p string) {
	im.conflicts++
	im.dirty = true
	im.note("%s kept: it has different content here (set overwrite to replace it)", p)
}

// check refuses a line no upload could have stored.
func (im *importer) check(l Line) error {
	switch {
	case !validPath(l.Path):
		return errors.New("invalid path")
	case !validSHA(l.SHA256) || l.Size < 0:
		return errors.New("invalid sha256 or size")
	}
	for _, r := range l.Packages {
		if r.Name == "" || r.Version == "" || !json.Valid([]byte(r.Attrs)) {
			return fmt.Errorf("invalid package row %q %q", r.Name, r.Version)
		}
	}
	return nil
}

// copyBlob writes the line's blob into the repository's blob store and
// checks it is the content the line names.
func (im *importer) copyBlob(l Line) (blob.Sums, error) {
	f, err := im.src.Open(blobPath(im.e.Name, l.SHA256))
	if errors.Is(err, os.ErrNotExist) {
		return blob.Sums{}, errors.New("its blob is missing from the export")
	}
	if err != nil {
		return blob.Sums{}, err
	}
	defer f.Close()
	sums, err := im.bs.Write(f)
	if err != nil {
		return blob.Sums{}, err
	}
	if sums.SHA256 != l.SHA256 || sums.Size != l.Size {
		// The blob stays unreferenced until the compact task removes it.
		return blob.Sums{}, fmt.Errorf("its blob in the export is corrupt (%d bytes, sha256 %s; expected %d bytes, %s)", sums.Size, sums.SHA256, l.Size, l.SHA256)
	}
	return sums, nil
}

// asset is the record of a written item, with the exported timestamps.
func (im *importer) asset(it item) meta.Asset {
	l, now := it.l, im.s.now().UTC().Truncate(time.Millisecond)
	a := meta.Asset{Repo: im.dest, Path: l.Path, BlobSHA256: it.sums.SHA256, Size: it.sums.Size, ContentType: l.ContentType,
		SHA1: it.sums.SHA1, MD5: it.sums.MD5, SHA512: it.sums.SHA512,
		CreatedAt: l.Created, LastModified: l.LastUpdated, CheckedAt: l.Attrs.Checked,
		RemoteETag: l.Attrs.RemoteETag, RemoteLastModified: l.Attrs.RemoteLastModified,
		Downloads: l.Attrs.Downloads, LastDownloaded: l.LastDownloaded}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	if a.LastModified.IsZero() {
		a.LastModified = a.CreatedAt
	}
	return a
}

// writeRows writes an item's package rows and tags: a row replaces the one
// there when its file was written and is only added when missing
// otherwise; a tag naming another version here is moved with overwrite and
// kept (and reported) without.
func (im *importer) writeRows(ctx context.Context, tx *meta.Tx, it item, kept *[]string) (int64, error) {
	var n int64
	for _, r := range it.l.Packages {
		p := meta.Package{Repo: im.dest, Format: im.rp.Format, Name: r.Name, Version: r.Version, Attrs: r.Attrs, Yanked: r.Yanked, CreatedAt: r.Created}
		if err := tx.ImportPackage(ctx, p, it.write); err != nil {
			return n, err
		}
		n++
		if len(r.Tags) == 0 {
			continue
		}
		tags, err := tx.ListDistTags(ctx, im.dest, r.Name)
		if err != nil {
			return n, err
		}
		for _, tag := range r.Tags {
			i := slices.IndexFunc(tags, func(t meta.DistTag) bool { return t.Tag == tag })
			switch {
			case i >= 0 && tags[i].Version == r.Version:
			case i >= 0 && !im.p.Overwrite:
				*kept = append(*kept, fmt.Sprintf("tag %s of %s (%s here, %s in the export)", tag, r.Name, tags[i].Version, r.Version))
			default:
				if err := tx.SetDistTag(ctx, im.dest, r.Name, tag, r.Version); err != nil {
					return n, err
				}
			}
		}
	}
	return n, nil
}

// missingRef skips a docker manifest whose blobs are not in the repository.
type missingRef struct{ manifest, blob string }

func (e *missingRef) Error() string {
	return fmt.Sprintf("manifest %s lists blob %s, which is not in the repository", e.manifest, e.blob)
}

// dockerRefs checks, as a push does, that a manifest's blobs and (for an
// index) manifests are in the repository: errDeferred when a manifest is
// not in yet, *missingRef when a blob is not.
func dockerRefs(ctx context.Context, tx *meta.Tx, repoName string, l Line) error {
	image := l.Path[:strings.LastIndex(l.Path, "/manifests/")]
	for _, r := range l.Packages {
		var refs struct {
			Blobs     []string `json:"blobs"`
			Manifests []string `json:"manifests"`
		}
		json.Unmarshal([]byte(r.Attrs), &refs)
		for _, d := range refs.Blobs {
			if _, err := tx.GetAsset(ctx, repoName, image+"/blobs/"+d); errors.Is(err, meta.ErrNotFound) {
				return &missingRef{l.Path, d}
			} else if err != nil {
				return err
			}
		}
		sort.Strings(refs.Manifests)
		for _, d := range refs.Manifests {
			if _, err := tx.GetAsset(ctx, repoName, image+"/manifests/"+d); errors.Is(err, meta.ErrNotFound) {
				return errDeferred
			} else if err != nil {
				return err
			}
		}
	}
	return nil
}
