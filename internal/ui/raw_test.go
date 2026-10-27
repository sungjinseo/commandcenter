package ui

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/config"
	"commandcenter/internal/format/raw"
	"commandcenter/internal/repo"
)

// A raw repository browses by path like Maven, and its components are its
// files: name is the path, there is no version.
func TestRawBrowseAndComponents(t *testing.T) {
	e := newEnvWith(t, "http://repo.example", func(c *config.Config) {
		c.Repositories = append(c.Repositories, config.Repository{Name: "raw-hosted", Format: "raw", Type: "hosted", WritePolicy: "allow"})
	})
	ctx := context.Background()
	bs, err := blob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: bs}, DB: e.db, Now: time.Now}
	for _, p := range []string{"docs/a.txt", "docs/sub/b.txt", "top.txt"} {
		if _, err := store.Put(ctx, "raw-hosted", p, "text/plain", strings.NewReader(p)); err != nil {
			t.Fatal(err)
		}
	}

	r, b := browse(t, e, "raw-hosted", "", "admin")
	wantStatus(t, r, http.StatusOK)
	if len(b.Entries) != 2 || b.Entries[0].Name != "docs" || !b.Entries[0].Dir || b.Entries[1].Name != "top.txt" || b.Entries[1].Dir {
		t.Fatalf("root: %s", r.body)
	}
	r, b = browse(t, e, "raw-hosted", "docs/", "admin")
	wantStatus(t, r, http.StatusOK)
	if len(b.Entries) != 2 || b.Entries[0].Name != "a.txt" || *b.Entries[0].Size != 10 || b.Entries[1].Name != "sub" {
		t.Fatalf("docs/: %s", r.body)
	}

	r = e.get(t, "/-/api/search/components?name=a.txt&repo=raw-hosted", "admin")
	wantStatus(t, r, http.StatusOK)
	comps := r.json(t)["components"].([]any)
	if len(comps) != 1 {
		t.Fatalf("components: %s", r.body)
	}
	c := comps[0].(map[string]any)
	assets := c["assets"].([]any)
	if c["format"] != "raw" || c["name"] != "docs/a.txt" || c["version"] != "" || c["group"] != "" || len(assets) != 1 ||
		assets[0].(map[string]any)["path"] != "docs/a.txt" {
		t.Fatalf("component: %s", r.body)
	}
	r = e.get(t, "/-/api/search/components?keyword=txt&repo=raw-hosted", "admin")
	wantStatus(t, r, http.StatusOK)
	if n := len(r.json(t)["components"].([]any)); n != 3 {
		t.Fatalf("keyword: %d components in %s", n, r.body)
	}
	r = e.get(t, "/-/api/repositories/raw-hosted/components/metadata?name=docs/a.txt&version=x", "admin")
	wantStatus(t, r, http.StatusBadRequest)

	reg := cleanup.NewRegistry(store)
	reg.Register("raw", "hosted", raw.New(store, nil))
	e.ui.Deleters = reg
	r = e.req(t, "DELETE", "/-/api/repositories/raw-hosted/components?name=docs/a.txt", "", "admin", nil, true)
	wantStatus(t, r, http.StatusOK)
	if m := r.json(t); m["assets"] != float64(1) || m["bytes"] != float64(10) {
		t.Fatalf("delete: %s", r.body)
	}
	r = e.req(t, "DELETE", "/-/api/repositories/raw-hosted/components?name=docs/a.txt", "", "admin", nil, true)
	wantStatus(t, r, http.StatusNotFound)
	r = e.req(t, "DELETE", "/-/api/repositories/raw-hosted/components?name=docs/a.txt&version=1", "", "admin", nil, true)
	wantStatus(t, r, http.StatusBadRequest)
	r, b = browse(t, e, "raw-hosted", "docs/", "admin")
	wantStatus(t, r, http.StatusOK)
	if len(b.Entries) != 1 || b.Entries[0].Name != "sub" {
		t.Fatalf("after delete: %s", r.body)
	}
}
