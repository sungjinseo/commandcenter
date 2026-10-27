package maven

import (
	"encoding/xml"
	"io"
	"sort"
	"strings"
	"time"
)

// Metadata is a maven-metadata.xml document (GA, snapshot or plugin-group level).
type Metadata struct {
	XMLName      xml.Name    `xml:"metadata"`
	ModelVersion string      `xml:"modelVersion,attr,omitempty"`
	GroupID      string      `xml:"groupId,omitempty"`
	ArtifactID   string      `xml:"artifactId,omitempty"`
	Version      string      `xml:"version,omitempty"`
	Versioning   *Versioning `xml:"versioning,omitempty"`
	Plugins      []Plugin    `xml:"plugins>plugin,omitempty"`
}

type Versioning struct {
	Latest           string            `xml:"latest,omitempty"`
	Release          string            `xml:"release,omitempty"`
	Snapshot         *Snapshot         `xml:"snapshot,omitempty"`
	Versions         []string          `xml:"versions>version,omitempty"`
	LastUpdated      string            `xml:"lastUpdated,omitempty"`
	SnapshotVersions []SnapshotVersion `xml:"snapshotVersions>snapshotVersion,omitempty"`
}

type Snapshot struct {
	Timestamp   string `xml:"timestamp,omitempty"`
	BuildNumber int    `xml:"buildNumber,omitempty"`
	LocalCopy   bool   `xml:"localCopy,omitempty"`
}

type SnapshotVersion struct {
	Classifier string `xml:"classifier,omitempty"`
	Extension  string `xml:"extension,omitempty"`
	Value      string `xml:"value,omitempty"`
	Updated    string `xml:"updated,omitempty"`
}

type Plugin struct {
	Name       string `xml:"name,omitempty"`
	Prefix     string `xml:"prefix"`
	ArtifactID string `xml:"artifactId"`
}

// maxMetadataSize bounds a stored or proxied document.
const maxMetadataSize = 10 << 20

// maxUploadedMetadata bounds an uploaded document (413 beyond it). Real ones
// are a few KiB; 1 MiB still holds some 30000 versions or a large
// plugin-group document.
const maxUploadedMetadata = 1 << 20

func ParseMetadata(r io.Reader) (*Metadata, error) {
	var m Metadata
	if err := xml.NewDecoder(io.LimitReader(r, maxMetadataSize)).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

func WriteMetadata(w io.Writer, m *Metadata) error {
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(m); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// Merge combines metadata documents. Versions are unioned and re-sorted,
// latest/release are recomputed, and the newest snapshot entries win.
func Merge(ms ...*Metadata) *Metadata {
	out := &Metadata{ModelVersion: "1.1.0"}
	versions := map[string]bool{}
	snaps := map[string]SnapshotVersion{}
	plugins := map[string]bool{}
	var v Versioning
	hasVersioning := false

	for _, m := range ms {
		if m == nil {
			continue
		}
		if out.GroupID == "" {
			out.GroupID = m.GroupID
		}
		if out.ArtifactID == "" {
			out.ArtifactID = m.ArtifactID
		}
		if out.Version == "" {
			out.Version = m.Version
		}
		for _, p := range m.Plugins {
			if !plugins[p.Prefix] {
				plugins[p.Prefix] = true
				out.Plugins = append(out.Plugins, p)
			}
		}
		mv := m.Versioning
		if mv == nil {
			continue
		}
		hasVersioning = true
		for _, x := range mv.Versions {
			versions[x] = true
		}
		if mv.LastUpdated > v.LastUpdated {
			v.LastUpdated = mv.LastUpdated
		}
		if mv.Snapshot != nil && (v.Snapshot == nil || mv.Snapshot.Timestamp > v.Snapshot.Timestamp) {
			s := *mv.Snapshot
			v.Snapshot = &s
		}
		for _, sv := range mv.SnapshotVersions {
			k := sv.Classifier + "\x00" + sv.Extension
			if cur, ok := snaps[k]; !ok || sv.Updated > cur.Updated {
				snaps[k] = sv
			}
		}
	}
	if !hasVersioning {
		return out
	}
	for x := range versions {
		v.Versions = append(v.Versions, x)
	}
	sort.Slice(v.Versions, func(i, j int) bool {
		if c := Compare(v.Versions[i], v.Versions[j]); c != 0 {
			return c < 0
		}
		return strings.Compare(v.Versions[i], v.Versions[j]) < 0
	})
	if n := len(v.Versions); n > 0 {
		v.Latest = v.Versions[n-1]
	}
	for i := len(v.Versions) - 1; i >= 0; i-- {
		if !strings.HasSuffix(v.Versions[i], "-SNAPSHOT") {
			v.Release = v.Versions[i]
			break
		}
	}
	keys := make([]string, 0, len(snaps))
	for k := range snaps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v.SnapshotVersions = append(v.SnapshotVersions, snaps[k])
	}
	out.Versioning = &v
	return out
}

// ForVersions builds GA-level metadata listing the given versions.
func ForVersions(groupID, artifactID string, versions []string, now time.Time) *Metadata {
	return Merge(&Metadata{
		GroupID:    groupID,
		ArtifactID: artifactID,
		Versioning: &Versioning{
			Versions:    versions,
			LastUpdated: now.UTC().Format("20060102150405"),
		},
	})
}
