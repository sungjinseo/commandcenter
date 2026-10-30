package maven

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"strings"

	"commandcenter/internal/repo"
)

// UploadPath is where the component upload API stores the file of
// groupID:artifactID:version with classifier ("" for none) and extension:
// g/r/o/u/p/artifactID/version/artifactID-version[-classifier].extension.
// It is a 400 when that path names other coordinates (a dot in the
// classifier, say), is no artifact path a write may use, or rp's version
// policy refuses the version. The PUT of the path checks it all again.
func UploadPath(rp *repo.Repository, groupID, artifactID, version, classifier, extension string) (string, error) {
	file := artifactID + "-" + version
	if classifier != "" {
		file += "-" + classifier
	}
	p := strings.ReplaceAll(groupID, ".", "/") + "/" + artifactID + "/" + version + "/" + file + "." + extension
	pp, err := ParsePath(p)
	if err == nil {
		err = validateWritePath(p)
	}
	if err != nil {
		return "", err
	}
	if c := pp.Coords; pp.Hash != "" || c == nil || c.GroupID != groupID || c.ArtifactID != artifactID ||
		c.Version != version || c.Classifier != classifier || c.Extension != extension {
		return "", repo.Errorf(http.StatusBadRequest, "%.200s:%.200s:%.100s with classifier %.100q and extension %.100q makes no artifact path (%.300s)",
			groupID, artifactID, version, classifier, extension, p)
	}
	return p, checkVersionPolicy(rp.VersionPolicy, pp)
}

// POMCoordinates reads the groupId (else the parent's), artifactId and
// version (else the parent's) of the POM r, its own ${name} properties
// expanded. A POM over 1 MiB is a 413; one that is not well-formed, lacks
// a coordinate or keeps a ${...} reference in one is a 400.
func POMCoordinates(ctx context.Context, r io.Reader) (groupID, artifactID, version string, err error) {
	b, err := io.ReadAll(io.LimitReader(r, maxPOMSize+1))
	if err != nil {
		return "", "", "", err
	}
	if len(b) > maxPOMSize {
		return "", "", "", repo.Errorf(http.StatusRequestEntityTooLarge, "the .pom is larger than %d bytes", maxPOMSize)
	}
	select {
	case pomSem <- struct{}{}:
		defer func() { <-pomSem }()
	case <-ctx.Done():
		return "", "", "", ctx.Err()
	}
	vals, err := parsePOM(b)
	if err != nil {
		return "", "", "", err
	}
	g, okG := expand(vals, inherited(vals, "groupId"))
	a, okA := expand(vals, vals["artifactId"])
	v, okV := expand(vals, inherited(vals, "version"))
	if g == "" || a == "" || v == "" || !okG || !okA || !okV {
		return "", "", "", repo.Errorf(http.StatusBadRequest, "the .pom names no complete coordinates: %.200q:%.200q:%.100q", g, a, v)
	}
	return g, a, v, nil
}

// GeneratePOM is a minimal POM of the coordinates; packaging "" leaves
// Maven's default (jar).
func GeneratePOM(groupID, artifactID, version, packaging string) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header + "<project xmlns=\"http://maven.apache.org/POM/4.0.0\">\n  <modelVersion>4.0.0</modelVersion>\n")
	for _, kv := range [][2]string{{"groupId", groupID}, {"artifactId", artifactID}, {"version", version}, {"packaging", packaging}} {
		if kv[1] != "" {
			b.WriteString("  <" + kv[0] + ">")
			xml.EscapeText(&b, []byte(kv[1]))
			b.WriteString("</" + kv[0] + ">\n")
		}
	}
	b.WriteString("</project>\n")
	return b.Bytes()
}
