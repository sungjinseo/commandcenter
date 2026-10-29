// Package vuln reports the known vulnerabilities and malware of components
// from OSV (https://osv.dev) open data, and runs the proxy firewall that
// quarantines them: Command Center's free counterpart of Nexus Pro's
// repository health check and firewall.
//
// Data comes from the OSV API (online mode: POST /v1/querybatch, then GET
// /v1/vulns/{id}, both cached in the metadata database) or from the OSV
// dumps (<ecosystem>/all.zip), downloaded by the vulnerability.sync task or
// uploaded for air-gapped sites (offline mode). Affected ranges are
// evaluated here, with each ecosystem's version order (internal/vercmp).
package vuln

import (
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"commandcenter/internal/format/pypi"
	"commandcenter/internal/meta"
	"commandcenter/internal/semver"
	"commandcenter/internal/vercmp"
)

// Ecosystems maps the repository formats with OSV data to their OSV
// ecosystem. Not covered: docker (images are not OSV packages), conda (no
// OSV ecosystem), apt and yum (OSV's Debian:<release> and distro ecosystems
// need the release, which a repository does not name), helm, raw, gitlfs.
var Ecosystems = map[string]string{
	"npm": "npm", "pypi": "PyPI", "maven": "Maven", "go": "Go", "cargo": "crates.io", "nuget": "NuGet",
	"rubygems": "RubyGems", "composer": "Packagist", "conan": "ConanCenter", "r": "CRAN",
}

// covered is the set of OSV ecosystems stored, from Ecosystems.
var covered = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range Ecosystems {
		m[e] = true
	}
	return m
}()

// CoveredEcosystems lists the OSV ecosystems stored, sorted.
func CoveredEcosystems() []string {
	out := make([]string, 0, len(covered))
	for e := range covered {
		out = append(out, e)
	}
	slices.Sort(out)
	return out
}

// Package is a component as OSV names it: Maven groupId:artifactId, npm
// @scope/name, Packagist vendor/package; Version as OSV writes it (Go
// without its "v", RubyGems without the platform, Conan without the
// recipe revision).
type Package struct {
	Ecosystem, Name, Version string
}

// FromComponent maps a component of a repository format to its OSV
// package; false for a format OSV does not cover.
func FromComponent(format, group, name, version string) (Package, bool) {
	eco := Ecosystems[format]
	if eco == "" || name == "" || version == "" {
		return Package{}, false
	}
	switch format {
	case "maven":
		name = group + ":" + name
	case "npm":
		if group != "" {
			name = group + "/" + name
		}
	case "go":
		version = strings.TrimPrefix(version, "v")
	case "rubygems": // the stored version holds the platform: 1.0.0-java
		version, _, _ = strings.Cut(version, "-")
	case "conan": // <version>#<recipe revision>
		version, _, _ = strings.Cut(version, "#")
	}
	return Package{Ecosystem: eco, Name: name, Version: version}, true
}

// Coordinate is the package as allowList entries and the quarantine name
// it: name@version.
func (p Package) Coordinate() string { return p.Name + "@" + p.Version }

// Key is the name a package's advisories are stored and looked up under:
// PEP 503 normalized for PyPI, lower case where the ecosystem ignores case.
func Key(ecosystem, name string) string {
	switch ecosystem {
	case "PyPI":
		return pypi.Normalize(name)
	case "NuGet", "crates.io", "Packagist":
		return strings.ToLower(name)
	}
	return name
}

func (p Package) key() string { return Key(p.Ecosystem, p.Name) }

// ---- the OSV schema (https://ossf.github.io/osv-schema/), as far as read ----

// Entry is one OSV advisory.
type Entry struct {
	ID               string         `json:"id"`
	Modified         string         `json:"modified"`
	Published        string         `json:"published"`
	Withdrawn        string         `json:"withdrawn"`
	Aliases          []string       `json:"aliases"`
	Summary          string         `json:"summary"`
	Details          string         `json:"details"`
	Severity         []Severity     `json:"severity"`
	Affected         []Affected     `json:"affected"`
	DatabaseSpecific map[string]any `json:"database_specific"`
}

// Severity is a scored severity: CVSS_V3 and CVSS_V4 vectors are read.
type Severity struct {
	Type  string `json:"type"`
	Score string `json:"score"`
}

// Affected is one affected package with its ranges and versions.
type Affected struct {
	Package struct {
		Ecosystem string `json:"ecosystem"`
		Name      string `json:"name"`
	} `json:"package"`
	Severity          []Severity     `json:"severity,omitempty"`
	Ranges            []Range        `json:"ranges,omitempty"`
	Versions          []string       `json:"versions,omitempty"`
	EcosystemSpecific map[string]any `json:"ecosystem_specific,omitempty"`
	DatabaseSpecific  map[string]any `json:"database_specific,omitempty"`
}

// Range is a SEMVER, ECOSYSTEM or GIT range (GIT ranges are not evaluated).
type Range struct {
	Type   string  `json:"type"`
	Events []Event `json:"events"`
}

// Event is one range event; exactly one field is set.
type Event struct {
	Introduced   string `json:"introduced,omitempty"`
	Fixed        string `json:"fixed,omitempty"`
	LastAffected string `json:"last_affected,omitempty"`
	Limit        string `json:"limit,omitempty"`
}

// stored is what a row keeps of an affected entry.
type stored struct {
	Ranges   []Range  `json:"ranges,omitempty"`
	Versions []string `json:"versions,omitempty"`
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// Malicious reports an OpenSSF malicious-packages advisory: a MAL- id or alias.
func (e Entry) Malicious() bool {
	if strings.HasPrefix(e.ID, "MAL-") {
		return true
	}
	return slices.ContainsFunc(e.Aliases, func(a string) bool { return strings.HasPrefix(a, "MAL-") })
}

// baseEcosystem drops an ecosystem's ":<suffix>" (Debian:12, Ubuntu:22.04:LTS).
func baseEcosystem(e string) string {
	b, _, _ := strings.Cut(e, ":")
	return b
}

// Rows maps an entry to its rows: one per covered package it affects, with
// that package's affected entries.
func (e Entry) Rows() []meta.VulnAdvisory {
	label, score10, vector := e.severity()
	summary := e.Summary
	if summary == "" {
		summary, _, _ = strings.Cut(strings.TrimSpace(e.Details), "\n")
	}
	if len(summary) > 500 {
		summary = summary[:500]
	}
	byPkg := map[[2]string][]stored{}
	var order [][2]string
	for _, a := range e.Affected {
		eco := baseEcosystem(a.Package.Ecosystem)
		if !covered[eco] || a.Package.Name == "" {
			continue
		}
		k := [2]string{eco, Key(eco, a.Package.Name)}
		if _, ok := byPkg[k]; !ok {
			order = append(order, k)
		}
		byPkg[k] = append(byPkg[k], stored{Ranges: a.Ranges, Versions: a.Versions})
	}
	out := make([]meta.VulnAdvisory, 0, len(order))
	for _, k := range order {
		aff, _ := json.Marshal(byPkg[k])
		out = append(out, meta.VulnAdvisory{
			ID: e.ID, Ecosystem: k[0], Package: k[1], Aliases: e.Aliases, Summary: summary,
			Severity: label, Score10: score10, Vector: vector, Malicious: e.Malicious(), Affected: string(aff),
			Published: parseTime(e.Published), Modified: parseTime(e.Modified), Withdrawn: parseTime(e.Withdrawn),
		})
	}
	return out
}

// Severity ranks, as vuln_findings stores them.
var ranks = map[string]int{"critical": 4, "high": 3, "medium": 2, "low": 1}

// Rank orders severities: critical 4, high 3, medium 2, low 1, anything
// else (none, unknown) 0.
func Rank(severity string) int { return ranks[severity] }

// bandOf is the CVSS qualitative rating of a base score.
func bandOf(score float64) string {
	switch {
	case score >= 9:
		return "critical"
	case score >= 7:
		return "high"
	case score >= 4:
		return "medium"
	case score > 0:
		return "low"
	}
	return "none"
}

// severity is the entry's label, score (times ten, -1 unknown) and vector:
// the highest CVSS 3 base score computed from a vector, else a CVSS 4
// vector with the score the advisory gives in database_specific, else the
// label it gives ("MODERATE" is medium).
func (e Entry) severity() (string, int, string) {
	all := slices.Clone(e.Severity)
	specs := []map[string]any{e.DatabaseSpecific}
	for _, a := range e.Affected {
		all = append(all, a.Severity...)
		specs = append(specs, a.DatabaseSpecific, a.EcosystemSpecific)
	}
	best, vector, v4 := -1.0, "", ""
	for _, s := range all {
		switch s.Type {
		case "CVSS_V3":
			if sc, ok := CVSS3(s.Score); ok && sc > best {
				best, vector = sc, s.Score
			}
		case "CVSS_V4":
			if v4 == "" {
				v4 = s.Score
			}
		}
	}
	if best < 0 && v4 != "" {
		vector = v4
		for _, m := range specs {
			for _, k := range []string{"cvss_score", "score", "cvss"} {
				if f, ok := m[k].(float64); ok && f >= 0 && f <= 10 {
					best = f
					break
				}
			}
			if best >= 0 {
				break
			}
		}
	}
	if best >= 0 {
		return bandOf(best), int(math.Round(best * 10)), vector
	}
	for _, m := range specs {
		if s, ok := m["severity"].(string); ok {
			switch l := strings.ToLower(s); l {
			case "critical", "high", "medium", "low":
				return l, -1, vector
			case "moderate":
				return "medium", -1, vector
			}
		}
	}
	return "unknown", -1, vector
}

// ---- CVSS 3.x base score (FIRST CVSS v3.1 specification, section 7) ----

var cvssWeights = map[string]map[string]float64{
	"AV": {"N": 0.85, "A": 0.62, "L": 0.55, "P": 0.2},
	"AC": {"L": 0.77, "H": 0.44},
	"PR": {"N": 0.85, "L": 0.62, "H": 0.27},
	"UI": {"N": 0.85, "R": 0.62},
	"C":  {"H": 0.56, "L": 0.22, "N": 0},
	"I":  {"H": 0.56, "L": 0.22, "N": 0},
	"A":  {"H": 0.56, "L": 0.22, "N": 0},
}

// CVSS3 computes the base score of a CVSS 3.0 or 3.1 vector
// ("CVSS:3.1/AV:N/AC:L/..."); false when it lacks a base metric or has an
// unknown value. Temporal and environmental metrics are ignored.
func CVSS3(vector string) (float64, bool) {
	rest, ok := strings.CutPrefix(vector, "CVSS:3.1/")
	if !ok {
		if rest, ok = strings.CutPrefix(vector, "CVSS:3.0/"); !ok {
			return 0, false
		}
	}
	m := map[string]string{}
	for _, part := range strings.Split(rest, "/") {
		k, v, _ := strings.Cut(part, ":")
		m[k] = v
	}
	scope := m["S"]
	if scope != "U" && scope != "C" {
		return 0, false
	}
	w := map[string]float64{}
	for k, vals := range cvssWeights {
		f, ok := vals[m[k]]
		if !ok {
			return 0, false
		}
		w[k] = f
	}
	changed := scope == "C"
	if changed && m["PR"] == "L" {
		w["PR"] = 0.68
	} else if changed && m["PR"] == "H" {
		w["PR"] = 0.5
	}
	iss := 1 - (1-w["C"])*(1-w["I"])*(1-w["A"])
	impact := 6.42 * iss
	if changed {
		impact = 7.52*(iss-0.029) - 3.25*math.Pow(iss-0.02, 15)
	}
	expl := 8.22 * w["AV"] * w["AC"] * w["PR"] * w["UI"]
	if impact <= 0 {
		return 0, true
	}
	if changed {
		return roundup(math.Min(1.08*(impact+expl), 10)), true
	}
	return roundup(math.Min(impact+expl, 10)), true
}

// roundup is CVSS 3.1's Roundup: the smallest one-decimal number at or
// above x, computed on integers to avoid floating-point surprises.
func roundup(x float64) float64 {
	i := int64(math.Round(x * 100000))
	if i%10000 == 0 {
		return float64(i) / 100000
	}
	return (math.Floor(float64(i)/10000) + 1) / 10
}

// ---- affected ranges ----

// compare orders versions within a range: SEMVER ranges by semver (OSV
// writes them without "v"), ECOSYSTEM ones by the ecosystem's order.
func compare(ecosystem, rangeType, a, b string) int {
	if rangeType == "SEMVER" {
		x, okA := semver.Parse(a)
		y, okB := semver.Parse(b)
		if okA && okB {
			return semver.Compare(x, y)
		}
	}
	return vercmp.Compare(ecosystem, a, b)
}

// eventVersion is the version an event names.
func eventVersion(e Event) string {
	switch {
	case e.Introduced != "":
		return e.Introduced
	case e.Fixed != "":
		return e.Fixed
	case e.LastAffected != "":
		return e.LastAffected
	}
	return e.Limit
}

// inRange evaluates one range as the OSV schema's pseudo-code does: the
// events sorted by version ("0" first), each introduced at or below the
// version makes it affected, each fixed at or below and each last_affected
// below it makes it not affected. Limit events are ignored.
func inRange(ecosystem string, r Range, version string) bool {
	evs := slices.Clone(r.Events)
	slices.SortStableFunc(evs, func(a, b Event) int {
		va, vb := eventVersion(a), eventVersion(b)
		switch {
		case va == vb:
			return 0
		case a.Introduced == "0":
			return -1
		case b.Introduced == "0":
			return 1
		}
		return compare(ecosystem, r.Type, va, vb)
	})
	affected := false
	for _, e := range evs {
		switch {
		case e.Introduced != "":
			if e.Introduced == "0" || compare(ecosystem, r.Type, version, e.Introduced) >= 0 {
				affected = true
			}
		case e.Fixed != "":
			if compare(ecosystem, r.Type, version, e.Fixed) >= 0 {
				affected = false
			}
		case e.LastAffected != "":
			if compare(ecosystem, r.Type, version, e.LastAffected) > 0 {
				affected = false
			}
		}
	}
	return affected
}

// Affects reports whether version is affected by a package's stored
// affected entries (JSON, as Rows stores them): listed in versions or
// within a SEMVER or ECOSYSTEM range. fixedIn are the fixed versions above
// it in the ranges that hold it, sorted.
func Affects(ecosystem, version, affected string) (hit bool, fixedIn []string, err error) {
	var entries []stored
	if err := json.Unmarshal([]byte(affected), &entries); err != nil {
		return false, nil, err
	}
	for _, a := range entries {
		for _, v := range a.Versions {
			if v == version || vercmp.Compare(ecosystem, v, version) == 0 {
				hit = true
			}
		}
		for _, r := range a.Ranges {
			if (r.Type != "SEMVER" && r.Type != "ECOSYSTEM") || !inRange(ecosystem, r, version) {
				continue
			}
			hit = true
			for _, e := range r.Events {
				if e.Fixed != "" && compare(ecosystem, r.Type, e.Fixed, version) > 0 && !slices.Contains(fixedIn, e.Fixed) {
					fixedIn = append(fixedIn, e.Fixed)
				}
			}
		}
	}
	slices.SortFunc(fixedIn, func(a, b string) int { return vercmp.Compare(ecosystem, a, b) })
	return hit, fixedIn, nil
}

// idRE is an OSV id as it may appear in a URL path: letters, digits and
// "-", "_", ".", ":" (GHSA-xxxx-xxxx-xxxx, PYSEC-2021-1, MAL-2024-1, GO-2023-1).
var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
