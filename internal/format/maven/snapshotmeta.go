package maven

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"

	"commandcenter/internal/repo"
)

// rebuildSnapshotVersion regenerates the version-level maven-metadata.xml of
// SNAPSHOT directory verDir (groupPath/artifactId/X-SNAPSHOT) from the
// timestamped builds stored in it, replacing whatever was there: the server
// owns this document (plan 13 Decision 6). <snapshot> names the highest build,
// and <snapshotVersions> the newest file of each classifier and extension.
// Without timestamped files the document is removed.
func (h *Handler) rebuildSnapshotVersion(ctx context.Context, rp *repo.Repository, verDir string) error {
	segs := strings.Split(verDir, "/")
	if len(segs) < 3 || !strings.HasSuffix(segs[len(segs)-1], "-SNAPSHOT") {
		return nil
	}
	n := len(segs)
	groupID, artifactID, baseVersion := strings.Join(segs[:n-2], "."), segs[n-2], segs[n-1]
	path := verDir + "/maven-metadata.xml"

	unlock, err := h.Store.Lock(ctx, "maven\x00"+rp.Name+"\x00"+verDir)
	if err != nil {
		return err
	}
	defer unlock()
	type build struct {
		timestamp string // yyyyMMdd.HHmmss
		number    int
	}
	newer := func(a, b build) bool {
		if a.number != b.number {
			return a.number > b.number
		}
		return a.timestamp > b.timestamp
	}
	var (
		latest build
		found  bool
		files  = map[string]SnapshotVersion{}
		builds = map[string]build{} // per file key, the build of files[key]
	)
	base := strings.TrimSuffix(baseVersion, "SNAPSHOT")
	err = h.Store.DB.WalkAssetPaths(ctx, rp.Name, verDir+"/", func(p string) { // streamed, as in rebuildGA
		if dir(p) != verDir {
			return
		}
		pp, err := ParsePath(p)
		if err != nil || pp.Hash != "" || pp.Coords == nil || pp.Coords.GroupID != groupID ||
			pp.Coords.ArtifactID != artifactID || pp.Coords.BaseVersion != baseVersion {
			return
		}
		ts := strings.TrimPrefix(pp.Coords.Version, base)
		if ts == pp.Coords.Version || !timestampRE.MatchString(ts) {
			return // a non-unique X-SNAPSHOT file
		}
		i := strings.IndexByte(ts, '-')
		num, err := strconv.Atoi(ts[i+1:])
		if err != nil {
			return
		}
		b := build{timestamp: ts[:i], number: num}
		if !found || newer(b, latest) {
			latest, found = b, true
		}
		key := pp.Coords.Classifier + "\x00" + pp.Coords.Extension
		if cur, ok := builds[key]; !ok || newer(b, cur) {
			builds[key] = b
			files[key] = SnapshotVersion{
				Classifier: pp.Coords.Classifier,
				Extension:  pp.Coords.Extension,
				Value:      pp.Coords.Version,
				Updated:    strings.Replace(b.timestamp, ".", "", 1),
			}
		}
	})
	if err != nil {
		return err
	}
	if !found {
		if err := h.Store.Delete(ctx, rp.Name, path); err != nil && !errors.Is(err, repo.ErrNotFound) {
			return err
		}
		return nil
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	v := &Versioning{
		Snapshot:    &Snapshot{Timestamp: latest.timestamp, BuildNumber: latest.number},
		LastUpdated: h.Now().UTC().Format("20060102150405"),
	}
	for _, k := range keys {
		v.SnapshotVersions = append(v.SnapshotVersions, files[k])
	}
	m := &Metadata{
		ModelVersion: "1.1.0",
		GroupID:      groupID,
		ArtifactID:   artifactID,
		Version:      baseVersion,
		Versioning:   v,
	}
	// Keep the stored document (and its Last-Modified) when only
	// lastUpdated would change: PUTs of sidecars and rebuilds are frequent.
	existing, err := h.loadMetadata(ctx, rp, path)
	if err != nil {
		return err
	}
	if existing != nil && sameIgnoringLastUpdated(existing, m) {
		return nil
	}
	return h.storeMetadata(ctx, rp, path, m)
}

// sameIgnoringLastUpdated reports whether a and b serialize alike apart from
// <lastUpdated>.
func sameIgnoringLastUpdated(a, b *Metadata) bool {
	norm := func(m *Metadata) []byte {
		c := *m
		if m.Versioning != nil {
			v := *m.Versioning
			v.LastUpdated = ""
			c.Versioning = &v
		}
		var buf bytes.Buffer
		if err := WriteMetadata(&buf, &c); err != nil {
			return nil
		}
		return buf.Bytes()
	}
	x, y := norm(a), norm(b)
	return x != nil && bytes.Equal(x, y)
}
