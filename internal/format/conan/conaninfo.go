package conan

import (
	"regexp"
	"sort"
	"strings"
)

// ParseConanInfo parses a conaninfo.txt into what the package search
// answers: "settings" and "options" (the [settings] and [options] key=value
// lines), "requires" (the [requires] lines), "full_requires" and
// "recipe_hash" when present (Conan 1), and "content", the file itself.
func ParseConanInfo(content string) map[string]any {
	settings, options := map[string]string{}, map[string]string{}
	lists := map[string][]string{}
	hash := ""
	section := ""
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = line[1 : len(line)-1]
		case section == "settings" || section == "options":
			k, v, _ := strings.Cut(line, "=")
			m := settings
			if section == "options" {
				m = options
			}
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		case section == "recipe_hash":
			hash = line
		case section != "":
			lists[section] = append(lists[section], line)
		}
	}
	requires := lists["requires"]
	if requires == nil {
		requires = []string{}
	}
	out := map[string]any{"settings": settings, "options": options, "requires": requires, "content": content}
	if full := lists["full_requires"]; len(full) > 0 {
		out["full_requires"] = full
	}
	if hash != "" {
		out["recipe_hash"] = hash
	}
	return out
}

// ParseManifest parses a conanmanifest.txt (a timestamp line, then
// "file: md5" lines) into {file: md5}.
func ParseManifest(content string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		name, sum, ok := strings.Cut(line, ": ")
		if ok && name != "" {
			out[strings.TrimSpace(name)] = strings.TrimSpace(sum)
		}
	}
	return out
}

// Conanfile is what a tolerant scan of a conanfile.py finds: the string
// class attributes description, license (a string, or a tuple joined
// with ", "), homepage and url, and the references the requires,
// tool_requires, build_requires and test_requires attributes list
// (strings, tuples or lists) or self.<kind>("ref") calls add, by kind.
// Conditional logic is not evaluated: a requirement added in a method
// behind an if is listed too, and one computed from a variable is not.
type Conanfile struct {
	Description, License, Homepage, URL string
	Requires                            map[string][]string
}

// RequireKinds are the requirement attributes ParseConanfile reads, in
// the order their references are listed.
var RequireKinds = []string{"requires", "tool_requires", "build_requires", "test_requires"}

var (
	attrRE   = regexp.MustCompile(`(?m)^[ \t]*(description|license|homepage|url|requires|tool_requires|build_requires|test_requires)[ \t]*=[ \t]*`)
	callRE   = regexp.MustCompile(`self\.(requires|tool_requires|build_requires|test_requires)\(\s*(?:f?["'])([^"'\n]+)["']`)
	stringRE = regexp.MustCompile(`"""(?s:(.*?))"""|'''(?s:(.*?))'''|"([^"\n]*)"|'([^'\n]*)'`)
)

// ParseConanfile scans src (a conanfile.py) for Conanfile. Only the first
// assignment of each attribute counts.
func ParseConanfile(src []byte) Conanfile {
	cf := Conanfile{Requires: map[string][]string{}}
	seen := map[string]bool{}
	for _, m := range attrRE.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		if seen[name] {
			continue
		}
		seen[name] = true
		value := attrValue(src[m[1]:])
		strs := quoted(value)
		switch name {
		case "description":
			cf.Description = first(strs)
		case "homepage":
			cf.Homepage = first(strs)
		case "url":
			cf.URL = first(strs)
		case "license":
			cf.License = strings.Join(strs, ", ")
		default:
			cf.Requires[name] = append(cf.Requires[name], strs...)
		}
	}
	for _, m := range callRE.FindAllSubmatch(src, -1) {
		kind, ref := string(m[1]), string(m[2])
		if !contains(cf.Requires[kind], ref) {
			cf.Requires[kind] = append(cf.Requires[kind], ref)
		}
	}
	for _, refs := range cf.Requires {
		sort.Strings(refs)
	}
	return cf
}

// attrValue is the text of a value starting at s: a bracketed sequence up
// to its closing bracket, else the rest of the line (a triple-quoted
// string may span lines).
func attrValue(s []byte) []byte {
	if len(s) == 0 {
		return nil
	}
	switch s[0] {
	case '(', '[', '{':
		depth := 0
		for i, c := range s {
			switch c {
			case '(', '[', '{':
				depth++
			case ')', ']', '}':
				depth--
				if depth == 0 {
					return s[:i+1]
				}
			}
		}
		return s
	}
	if loc := stringRE.FindIndex(s); loc != nil && loc[0] == 0 {
		return s[:loc[1]]
	}
	if i := strings.IndexByte(string(s), '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// quoted returns the string literals in s, in order.
func quoted(s []byte) []string {
	var out []string
	for _, m := range stringRE.FindAllSubmatch(s, -1) {
		for _, g := range m[1:] {
			if g != nil {
				out = append(out, strings.TrimSpace(string(g)))
				break
			}
		}
	}
	return out
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
