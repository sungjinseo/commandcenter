package maven

import (
	"bufio"
	"encoding/xml"
	"io"
	"strings"
)

// POMInfo is what the web UI shows about a stored POM: the project's own
// description, license and URLs, and its declared dependencies. References
// (${name}) are replaced from the POM's own <properties> only; parents and
// BOMs are not read, so versions are as declared, not as Maven resolves them.
type POMInfo struct {
	Description, License, Homepage, SCM string
	Dependencies                        []POMDep
}

// POMDep is one <dependency> of <project><dependencies>.
type POMDep struct {
	GroupID, ArtifactID, Version, Scope string
	Optional                            bool
}

// maxPOMDeps bounds the dependencies kept from one POM.
const maxPOMDeps = 2000

// ReadPOMInfo reads r as a POM (at most maxPOMSize bytes are expected; the
// caller bounds it). Malformed XML ends the read with what was found so far:
// the UI shows partial information rather than nothing.
func ReadPOMInfo(r io.Reader) POMInfo {
	br := bufio.NewReader(r)
	if b, _ := br.Peek(3); string(b) == "\xef\xbb\xbf" {
		br.Discard(3)
	}
	d := xml.NewDecoder(br)
	d.CharsetReader = latin1
	var (
		info  POMInfo
		path  []string // open elements below <project>
		text  strings.Builder
		props = map[string]string{}
		dep   *POMDep
		lic   string
	)
	at := func(p ...string) bool {
		if len(path) != len(p) {
			return false
		}
		for i := range p {
			if path[i] != p[i] {
				return false
			}
		}
		return true
	}
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(path) == 0 && t.Name.Local == "project" {
				path = append(path, "")
				continue
			}
			if len(path) == 0 || len(path) > maxPOMDepth {
				continue
			}
			path = append(path, t.Name.Local)
			text.Reset()
			if at("", "dependencies", "dependency") {
				dep = &POMDep{}
			}
		case xml.CharData:
			if text.Len() < 4096 {
				text.Write(t)
			}
		case xml.EndElement:
			if len(path) == 0 {
				continue
			}
			v := strings.TrimSpace(text.String())
			switch {
			case at("", "description"):
				info.Description = v
			case at("", "url"):
				info.Homepage = v
			case at("", "scm", "url"):
				info.SCM = v
			case at("", "licenses", "license", "name"):
				lic = v
			case at("", "licenses", "license"):
				if lic != "" {
					if info.License != "" {
						info.License += ", "
					}
					info.License += lic
					lic = ""
				}
			case at("", "properties") || (len(path) == 3 && path[1] == "properties"):
				if len(path) == 3 && len(props) < maxPOMValues {
					props[path[2]] = v
				}
			case dep != nil && len(path) == 4 && path[1] == "dependencies":
				switch path[3] {
				case "groupId":
					dep.GroupID = v
				case "artifactId":
					dep.ArtifactID = v
				case "version":
					dep.Version = v
				case "scope":
					dep.Scope = v
				case "optional":
					dep.Optional = v == "true"
				}
			case dep != nil && at("", "dependencies", "dependency"):
				if len(info.Dependencies) < maxPOMDeps {
					info.Dependencies = append(info.Dependencies, *dep)
				}
				dep = nil
			}
			path = path[:len(path)-1]
			text.Reset()
		}
	}
	for i := range info.Dependencies {
		dp := &info.Dependencies[i]
		dp.GroupID, _ = expand(propVals(props), dp.GroupID)
		dp.ArtifactID, _ = expand(propVals(props), dp.ArtifactID)
		dp.Version, _ = expand(propVals(props), dp.Version)
	}
	return info
}

// propVals gives readPOM's expand the "${name}" keys it expects.
func propVals(props map[string]string) map[string]string {
	m := make(map[string]string, len(props))
	for k, v := range props {
		m["${"+k+"}"] = v
	}
	return m
}
