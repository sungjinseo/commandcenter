package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
	"commandcenter/internal/format/cocoapods"
	"commandcenter/internal/repo"
)

// TestProxyOnlyFormatsMetadata: the metadata API reads a cocoapods proxy's
// cached podspec and a huggingface proxy's cached API document.
func TestProxyOnlyFormatsMetadata(t *testing.T) {
	e := newEnvWith(t, "http://repo.example", func(c *config.Config) {
		c.Repositories = append(c.Repositories,
			config.Repository{Name: "pods", Format: "cocoapods", Type: "proxy", Remote: "https://cdn.cocoapods.org/"},
			config.Repository{Name: "hf", Format: "huggingface", Type: "proxy", Remote: "https://huggingface.co/"})
	})
	bs, err := blob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: bs}, DB: e.db, Now: e.ui.Started.Local}
	e.ui.Store = store
	ctx := context.Background()
	put := func(rp, p, body string) {
		t.Helper()
		if _, err := store.Put(ctx, rp, p, "application/json", strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	put("pods", cocoapods.SpecPath("Alamofire", "5.0.0"), `{"summary":"Elegant HTTP","license":"MIT","homepage":"https://alamofire.org","source":{"http":"https://x/a.zip"},"dependencies":{"Foo":["~> 1.0"],"Bar":[]}}`)
	r := e.req(t, "GET", "/-/api/repositories/pods/components/metadata?name=Alamofire&version=5.0.0", "", "admin", nil, false)
	var m componentMetadata
	if r.status != 200 || json.Unmarshal([]byte(r.body), &m) != nil {
		t.Fatalf("pods metadata: %d %s", r.status, r.body)
	}
	if !m.Collected || *m.Description != "Elegant HTTP" || *m.License != "MIT" || *m.Homepage != "https://alamofire.org" || *m.RepositoryURL != "https://x/a.zip" ||
		len(m.Dependencies) != 2 || m.Dependencies[0].Name != "Bar" || m.Dependencies[1].Requirement != "~> 1.0" || m.Dependencies[1].Scope != "dependencies" {
		t.Fatalf("pods metadata: %+v", m)
	}
	if r := e.req(t, "GET", "/-/api/repositories/pods/components/metadata?name=Alamofire&version=4.0.0", "", "admin", nil, false); r.status != 404 {
		t.Fatalf("missing pod: %d", r.status)
	}

	sha := strings.Repeat("a", 40)
	put("hf", "org/model/resolve/"+sha+"/model.bin", "weights")
	put("hf", "api/models/org/model", `{"sha":"`+sha+`","pipeline_tag":"text-generation","library_name":"transformers","tags":["license:mit"]}`)
	put("hf", "datasets/org/ds/resolve/"+sha+"/data.csv", "a,b")
	r = e.req(t, "GET", "/-/api/repositories/hf/components/metadata?name=org/model&version="+sha, "", "admin", nil, false)
	if r.status != 200 || json.Unmarshal([]byte(r.body), &m) != nil {
		t.Fatalf("hf metadata: %d %s", r.status, r.body)
	}
	if !m.Collected || *m.Description != "text-generation (transformers)" || *m.License != "mit" || *m.Homepage != "https://huggingface.co/org/model" || len(m.Dependencies) != 0 {
		t.Fatalf("hf metadata: %+v", m)
	}
	r = e.req(t, "GET", "/-/api/repositories/hf/components/metadata?group=datasets&name=org/ds&version="+sha, "", "admin", nil, false)
	if r.status != 200 || json.Unmarshal([]byte(r.body), &m) != nil || m.Collected || *m.Homepage != "https://huggingface.co/datasets/org/ds" {
		t.Fatalf("hf dataset without an api document: %d %s", r.status, r.body)
	}
	if r := e.req(t, "GET", "/-/api/repositories/hf/components/metadata?name=org/model&version="+strings.Repeat("b", 40), "", "admin", nil, false); r.status != 404 {
		t.Fatalf("missing commit: %d", r.status)
	}
}
