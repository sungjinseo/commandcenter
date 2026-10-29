package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
)

// TestTerraformUI: components come from the assets (a module is its
// archive, a provider version its files); the metadata API reads a
// module's README paragraph and a provider's protocols and platforms; the
// general setting terraformDefaultRepository takes terraform repositories
// only.
func TestTerraformUI(t *testing.T) {
	e := newEnvWith(t, "http://repo.example", func(c *config.Config) {
		c.Repositories = append(c.Repositories, config.Repository{Name: "tf-hosted", Format: "terraform", Type: "hosted"})
	})
	ctx := context.Background()
	for _, p := range []meta.Package{
		{Name: "acme/vpc/aws", Version: "1.0.0", Attrs: `{"ext":"tgz","description":"Creates a VPC."}`},
		{Name: "acme/demo", Version: "2.0.0", Attrs: `{}`},
		{Name: "acme/demo", Version: "3.0.0", Attrs: `{"protocols":["6.0"]}`},
	} {
		p.Repo, p.Format, p.CreatedAt = "tf-hosted", "terraform", t0
		if err := e.db.Tx(ctx, func(tx *meta.Tx) error { return tx.InsertPackage(ctx, p) }); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{
		"modules/acme/vpc/aws/1.0.0.tgz",
		"providers/acme/demo/2.0.0/terraform-provider-demo_2.0.0_SHA256SUMS",
		"providers/acme/demo/2.0.0/terraform-provider-demo_2.0.0_darwin_arm64.zip",
		"providers/acme/demo/2.0.0/terraform-provider-demo_2.0.0_linux_amd64.zip",
	} {
		putAsset(t, e.db, "tf-hosted", p, 1)
	}
	c := searchComponents(t, e, "repo=tf-hosted&keyword=acme", "admin", 200)
	var got []string
	for _, x := range c.Components {
		got = append(got, fmt.Sprintf("%s@%s:%d", x.Name, x.Version, len(x.Assets)))
	}
	if strings.Join(got, " ") != "acme/demo@2.0.0:3 acme/vpc/aws@1.0.0:1" {
		t.Fatalf("components: %v", got)
	}

	md := func(query string, want int) componentMetadata {
		t.Helper()
		r := e.req(t, "GET", "/-/api/repositories/tf-hosted/components/metadata?"+query, "", "admin", nil, false)
		var m componentMetadata
		if r.status != want || (want == 200 && json.Unmarshal([]byte(r.body), &m) != nil) {
			t.Fatalf("%s: %d %s", query, r.status, r.body)
		}
		return m
	}
	if m := md("name=acme/vpc/aws&version=1.0.0", 200); !m.Collected || *m.Description != "Creates a VPC." || m.Platforms != nil {
		t.Fatalf("module: %+v", m)
	}
	if m := md("name=acme/demo&version=2.0.0", 200); !m.Collected || m.Description != nil || strings.Join(m.Platforms, ",") != "darwin_arm64,linux_amd64" || strings.Join(m.Protocols, ",") != "5.0" {
		t.Fatalf("provider: %+v", m)
	}
	if m := md("name=acme/demo&version=3.0.0", 200); strings.Join(m.Protocols, ",") != "6.0" || len(m.Platforms) != 0 {
		t.Fatalf("provider without files: %+v", m)
	}
	md("name=acme/demo&version=9.0.0", 404)

	for body, want := range map[string]int{
		`{"terraformDefaultRepository":"maven-releases"}`: http.StatusBadRequest,
		`{"terraformDefaultRepository":"nosuch"}`:         http.StatusBadRequest,
		`{"terraformDefaultRepository":" tf-hosted "}`:    http.StatusOK,
	} {
		wantStatus(t, e.req(t, "PUT", "/-/api/settings", body, "admin", nil, true), want)
	}
	r := e.get(t, "/-/api/settings", "admin")
	var dto settingsDTO
	if json.Unmarshal([]byte(r.body), &dto) != nil || dto.TerraformDefaultRepository != "tf-hosted" {
		t.Fatalf("settings: %s", r.body)
	}
	wantStatus(t, e.req(t, "PUT", "/-/api/settings", `{"terraformDefaultRepository":""}`, "admin", nil, true), http.StatusOK)
	if s, _ := e.db.LoadSettings(ctx); s.TerraformDefaultRepository != "" {
		t.Fatalf("cleared: %q", s.TerraformDefaultRepository)
	}
}
