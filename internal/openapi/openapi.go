// Package openapi builds the OpenAPI 3.0 description of the HTTP APIs from
// hand-written route descriptions that live next to the route registrations
// (ui.apiDocs, nexusapi.apiDocs): each package registers its routes at
// startup and the document is rendered once, on first use.
package openapi

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Route describes one registered pattern ("GET /-/api/users/{id}").
type Route struct {
	Pattern string // the mux pattern as registered: "METHOD /path" or "/path" for any method
	Summary string
	Tag     string
	Auth    string   // who may call it: "", "user", "admin", "admin (interactive)"
	Query   []string // query parameter names (free-form)
	Request string   // a schema name of the JSON body, "" for none
	Reply   string   // a schema name of the JSON answer, "" for none or non-JSON
	Replies map[int]string
}

var (
	mu     sync.Mutex
	routes []Route
	once   sync.Once
	doc    []byte
)

// Register adds route descriptions (called from package initialisers).
func Register(rs ...Route) {
	mu.Lock()
	routes = append(routes, rs...)
	mu.Unlock()
}

// Routes returns the registered descriptions (for tests).
func Routes() []Route {
	mu.Lock()
	defer mu.Unlock()
	return append([]Route{}, routes...)
}

// Document is the OpenAPI 3.0 document as JSON, rendered once.
func Document() []byte {
	once.Do(func() { doc, _ = json.MarshalIndent(build(Routes()), "", " ") })
	return doc
}

var paramRE = regexp.MustCompile(`\{([A-Za-z0-9_]+)(\.\.\.)?\}`)

// Split returns the method ("" for any) and path of a mux pattern.
func Split(pattern string) (method, path string) {
	if m, p, ok := strings.Cut(pattern, " "); ok && !strings.HasPrefix(pattern, "/") {
		return m, p
	}
	return "", pattern
}

func build(rs []Route) map[string]any {
	paths := map[string]map[string]any{}
	schemas := map[string]any{}
	for _, r := range rs {
		method, p := Split(r.Pattern)
		p = strings.TrimSuffix(p, "{$}")
		var params []map[string]any
		for _, m := range paramRE.FindAllStringSubmatch(p, -1) {
			desc := "path segment"
			if m[2] != "" {
				desc = "the rest of the path (may contain slashes)"
			}
			params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "description": desc, "schema": map[string]any{"type": "string"}})
		}
		p = paramRE.ReplaceAllString(p, "{$1}")
		for _, q := range r.Query {
			params = append(params, map[string]any{"name": q, "in": "query", "required": false, "schema": map[string]any{"type": "string"}})
		}
		op := map[string]any{"summary": r.Summary, "tags": []string{r.Tag}}
		if r.Auth != "" {
			op["description"] = "Requires: " + r.Auth + "."
			op["security"] = []map[string]any{{"basicAuth": []string{}}, {"bearerToken": []string{}}, {"session": []string{}}}
		}
		if params == nil {
			params = []map[string]any{}
		}
		op["parameters"] = params
		if r.Request != "" {
			schemas[r.Request] = schema(r.Request)
			op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": ref(r.Request)}}}
		}
		responses := map[string]any{}
		if r.Reply != "" {
			schemas[r.Reply] = schema(r.Reply)
			responses["200"] = map[string]any{"description": "OK", "content": map[string]any{"application/json": map[string]any{"schema": ref(r.Reply)}}}
		}
		for code, name := range r.Replies {
			desc := name
			if name != "" && !strings.ContainsAny(name, " ") {
				schemas[name] = schema(name)
				responses[strconv.Itoa(code)] = map[string]any{"description": name, "content": map[string]any{"application/json": map[string]any{"schema": ref(name)}}}
				continue
			}
			responses[strconv.Itoa(code)] = map[string]any{"description": desc}
		}
		if len(responses) == 0 {
			responses["default"] = map[string]any{"description": "see the guide"}
		}
		op["responses"] = responses
		if paths[p] == nil {
			paths[p] = map[string]any{}
		}
		if method == "" {
			for _, m := range []string{"get", "put", "post", "delete"} {
				if _, taken := paths[p][m]; !taken {
					paths[p][m] = op
				}
			}
		} else {
			paths[p][strings.ToLower(method)] = op
		}
	}
	tags := map[string]bool{}
	for _, r := range rs {
		tags[r.Tag] = true
	}
	tagList := make([]map[string]any, 0, len(tags))
	for t := range tags {
		tagList = append(tagList, map[string]any{"name": t})
	}
	sort.Slice(tagList, func(i, j int) bool { return tagList[i]["name"].(string) < tagList[j]["name"].(string) })
	schemas["Error"] = map[string]any{"type": "object", "properties": map[string]any{
		"error": map[string]any{"type": "string"}, "errors": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"message": map[string]any{"type": "string", "description": "Nexus compat errors"}}}
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{"title": "Command Center API", "version": "1",
			"description": "The Command Center JSON API (/-/api/*) and the Nexus Repository 3 REST API compatibility layer (/service/rest/v1/*). " +
				"Nexus publishes its own description as Swagger 2.0 at /service/rest/swagger.json; this server answers that path with this OpenAPI 3.0 document. " +
				"Schemas are free-form: the field lists are in the guide (docs/guide.md)."},
		"paths": paths,
		"tags":  tagList,
		"components": map[string]any{"schemas": schemas, "securitySchemes": map[string]any{
			"basicAuth":   map[string]any{"type": "http", "scheme": "basic"},
			"bearerToken": map[string]any{"type": "http", "scheme": "bearer"},
			"session":     map[string]any{"type": "apiKey", "in": "cookie", "name": "cc_session"}}},
	}
}

func ref(name string) map[string]any { return map[string]any{"$ref": "#/components/schemas/" + name} }

func schema(name string) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": true, "description": name + " (free-form; see the guide)"}
}
