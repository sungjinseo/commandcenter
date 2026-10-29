package p2

import (
	"archive/zip"
	"io"
	"strings"
)

// maxManifest bounds a META-INF/MANIFEST.MF read from a cached bundle.
const maxManifest = 1 << 20

// ReadManifest returns the main attributes of the META-INF/MANIFEST.MF of
// the jar r (a cached plugin), or nil when it has none. Continuation
// lines (a leading space) are joined as the JAR specification says.
func ReadManifest(r io.ReaderAt, size int64) map[string]string {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil
	}
	for _, f := range zr.File {
		if f.Name != "META-INF/MANIFEST.MF" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxManifest))
		rc.Close()
		if err != nil {
			return nil
		}
		return parseManifest(string(b))
	}
	return nil
}

func parseManifest(s string) map[string]string {
	out := map[string]string{}
	last := ""
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		switch {
		case line == "":
			if len(out) > 0 {
				return out // the main section ends at the first blank line
			}
		case line[0] == ' ' && last != "":
			out[last] += line[1:]
		default:
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			last = k
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

// SplitHeader splits an OSGi header into its clauses, each a name with its
// directives and attributes dropped, and the bundle-version attribute if
// any: Require-Bundle "a;bundle-version=\"[1,2)\",b;resolution:=optional"
// gives (a, [1,2)) and (b, ""). Quoted parts may hold the separators.
func SplitHeader(h string) (names, versions []string) {
	var clause strings.Builder
	quoted := false
	flush := func() {
		c := strings.TrimSpace(clause.String())
		clause.Reset()
		if c == "" {
			return
		}
		parts := strings.Split(c, ";")
		version := ""
		for _, p := range parts[1:] {
			if k, v, ok := strings.Cut(p, "="); ok && strings.TrimSpace(k) == "bundle-version" {
				version = strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
		names, versions = append(names, strings.TrimSpace(parts[0])), append(versions, version)
	}
	for _, r := range h {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			flush()
			continue
		}
		clause.WriteRune(r)
	}
	flush()
	return names, versions
}
