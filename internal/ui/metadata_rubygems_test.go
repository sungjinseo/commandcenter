package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

// TestRubygemsMetadataAndVersions: the metadata API reads the gem's
// specification fields a push stored (dependencies scoped runtime or
// development), and the versions list carries the summary.
func TestRubygemsMetadataAndVersions(t *testing.T) {
	e := newEnvWith(t, "http://repo.example", func(c *config.Config) {
		c.Repositories = append(c.Repositories, config.Repository{Name: "gems-hosted", Format: "rubygems", Type: "hosted"})
	})
	ctx := context.Background()
	attrs := `{"name":"demo","version":"1.0.0","platform":"java","authors":["Jane"],"summary":"A demo","description":"A demo gem","homepage":"https://example.org/demo","licenses":["MIT","Apache-2.0"],"required_ruby_version":">= 2.7.0","dependencies":[{"name":"rack","requirement":">= 2.0","type":"runtime"},{"name":"rake","requirement":"~> 13.0, >= 13.0.1","type":"development"}],"metadata":{"source_code_uri":"https://git.example.org/demo"},"sha256":"x"}`
	if err := e.db.Tx(ctx, func(tx *meta.Tx) error {
		return tx.InsertPackage(ctx, meta.Package{Repo: "gems-hosted", Format: "rubygems", Name: "demo", Version: "1.0.0-java", Attrs: attrs, CreatedAt: t0})
	}); err != nil {
		t.Fatal(err)
	}
	r := e.req(t, "GET", "/-/api/repositories/gems-hosted/components/metadata?name=demo&version=1.0.0-java", "", "admin", nil, false)
	var m componentMetadata
	if r.status != 200 || json.Unmarshal([]byte(r.body), &m) != nil {
		t.Fatalf("metadata: %d %s", r.status, r.body)
	}
	if !m.Collected || *m.Description != "A demo gem" || *m.License != "MIT, Apache-2.0" || *m.Homepage != "https://example.org/demo" || *m.RepositoryURL != "https://git.example.org/demo" {
		t.Fatalf("metadata: %+v", m)
	}
	var deps []string
	for _, d := range m.Dependencies {
		deps = append(deps, d.Scope+":"+d.Name+":"+d.Requirement)
	}
	if got := strings.Join(deps, " "); got != "runtime:rack:>= 2.0 development:rake:~> 13.0, >= 13.0.1" {
		t.Fatalf("deps: %s", got)
	}
	if r := e.req(t, "GET", "/-/api/repositories/gems-hosted/components/metadata?name=demo&version=2.0.0", "", "admin", nil, false); r.status != 404 {
		t.Fatalf("missing: %d", r.status)
	}
	r = e.req(t, "GET", "/-/api/repositories/gems-hosted/packages/demo", "", "admin", nil, false)
	var v struct {
		Versions []struct{ Version, Description string }
	}
	if r.status != 200 || json.Unmarshal([]byte(r.body), &v) != nil || len(v.Versions) != 1 || v.Versions[0].Version != "1.0.0-java" || v.Versions[0].Description != "A demo" {
		t.Fatalf("versions: %d %s", r.status, r.body)
	}
}
