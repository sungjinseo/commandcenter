package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// storeLegacy rewrites stored definitions the way older builds accepted them
// (final review C1): it runs between two starts on the same data directory.
func storeLegacy(t *testing.T, cfg *config.Config, edit func(*config.Repository)) {
	t.Helper()
	db, err := meta.Open(filepath.Join(cfg.DataDir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	defs, err := db.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		edit(&d)
		if err := db.UpdateRepository(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
}

// C1: rows stored by older builds with a negative duration or a group that
// lists a member twice must not stop the server from starting; they are
// served (negative = 0-length cache, members deduped) with one warning each,
// and they do not block edits of unrelated repositories.
func TestStartWithLegacyStoredRows(t *testing.T) {
	cfg := testConfig(t, "")
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	storeLegacy(t, cfg, func(d *config.Repository) {
		switch d.Name {
		case "maven-releases":
			d.NegativeCacheTTL = config.Duration(-5 * time.Minute)
		case "maven-public":
			d.Members = []string{"maven-releases", "maven-releases"}
		}
	})

	var buf strings.Builder
	a, err = New(cfg, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatalf("start with legacy rows: %v", err)
	}
	defer a.Close()
	logs := buf.String()
	for _, want := range []string{"repo=maven-releases", "repo=maven-public", "negativeCacheTTL", "duplicate member"} {
		if !strings.Contains(logs, want) {
			t.Errorf("no warning containing %q: %s", want, logs)
		}
	}
	if n := strings.Count(logs, "repo=maven-public"); n != 1 {
		t.Errorf("maven-public warned %d times: %s", n, logs)
	}
	if rp := a.Registry.Get("maven-releases"); rp == nil || rp.NegativeCacheTTL != 0 {
		t.Fatalf("maven-releases = %+v", rp)
	}
	if rp := a.Registry.Get("maven-public"); rp == nil || !slices.Equal(rp.Members, []string{"maven-releases"}) {
		t.Fatalf("maven-public = %+v", rp)
	}

	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	put, _ := http.NewRequest("PUT", ts.URL+"/repository/maven-releases/org/example/app/1.0/app-1.0.jar", strings.NewReader("jar"))
	put.SetBasicAuth("ci", "pw")
	resp, err := http.DefaultClient.Do(put)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("PUT: %v %v", resp, err)
	}
	resp.Body.Close()
	for _, r := range []string{"maven-releases", "maven-public"} {
		resp, err = http.Get(ts.URL + "/repository/" + r + "/org/example/app/1.0/app-1.0.jar")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(b) != "jar" {
			t.Fatalf("GET %s: %d %q", r, resp.StatusCode, b)
		}
	}

	// Unrelated create and update succeed with the legacy rows present.
	ctx := context.Background()
	if err := a.Registry.Create(ctx, config.Repository{Name: "npm-hosted", Format: "npm", Type: "hosted"}); err != nil {
		t.Fatalf("create unrelated: %v", err)
	}
	if err := a.Registry.Update(ctx, "npm-hosted", config.Repository{WritePolicy: "allow"}, repo.ClearSecrets{}); err != nil {
		t.Fatalf("update unrelated: %v", err)
	}

	// The definition being created or updated is still validated.
	var ve *repo.ValidationError
	err = a.Registry.Create(ctx, config.Repository{Name: "neg", Format: "maven", Type: "hosted",
		NegativeCacheTTL: config.Duration(-5 * time.Minute)})
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "negativeCacheTTL") {
		t.Fatalf("create with a negative duration: %v", err)
	}
	err = a.Registry.Create(ctx, config.Repository{Name: "dup", Format: "maven", Type: "group",
		Members: []string{"maven-releases", "maven-releases"}})
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "duplicate member") {
		t.Fatalf("create with a duplicate member: %v", err)
	}
	err = a.Registry.Update(ctx, "npm-hosted", config.Repository{MetadataMaxAge: config.Duration(-time.Hour)}, repo.ClearSecrets{})
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "metadataMaxAge") {
		t.Fatalf("update with a negative duration: %v", err)
	}
}
