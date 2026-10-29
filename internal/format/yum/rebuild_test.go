package yum

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// The rebuild-metadata task regenerates the index files byte for byte.
func TestRebuildMetadata(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "yum-hosted", "Packages/d/demo-1.0-1.el9.x86_64.rpm", demoRPM()); w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	// Depth 1: one repodata/ per top-level directory.
	for _, d := range []string{"el9/", "el8/"} {
		if w := e.do("PUT", "yum-deep", d+"x/demo-1.0-1.noarch.rpm", nvra("demo", "1.0", "1", "noarch")); w.Code != 201 {
			t.Fatalf("upload %s: %d %s", d, w.Code, w.Body)
		}
	}
	for _, name := range []string{"yum-hosted", "yum-deep"} {
		t.Run(name, func(t *testing.T) { roundTrip(t, e, e.repos[name]) })
	}
}

func roundTrip(t *testing.T, e *env, rp *repo.Repository) {
	ctx := context.Background()
	db := e.h.Store.DB
	indexes := func() map[string][]byte {
		out := map[string][]byte{}
		all, err := db.ListAssets(ctx, rp.Name, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range all {
			if !isIndexPath(a.Path) {
				continue
			}
			f, err := e.h.Store.Open(a)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(f)
			f.Close()
			out[a.Path] = b
		}
		return out
	}
	before := indexes()
	if len(before) == 0 {
		t.Fatal("no index files after the upload")
	}
	for p := range before {
		if err := db.DeleteAsset(ctx, rp.Name, p); err != nil {
			t.Fatal(err)
		}
	}
	if got := indexes(); len(got) != 0 {
		t.Fatalf("still there: %v", got)
	}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, format) }
	if err := e.h.RebuildMetadata(ctx, rp, logf); err != nil {
		t.Fatal(err)
	}
	after := indexes()
	if len(after) != len(before) || len(logged) == 0 {
		t.Fatalf("rebuilt %d of %d index files (log %d lines)", len(after), len(before), len(logged))
	}
	for p, b := range before {
		if !bytes.Equal(after[p], b) {
			t.Errorf("%s differs after the rebuild:\n%s\n---\n%s", p, b, after[p])
		}
	}
	if rp.Depth == 1 && len(before) != 8 {
		t.Fatalf("depth 1: %d index files, want 8", len(before))
	}
	// Not for a proxy or group.
	for _, name := range []string{"yum-proxy", "yum-all"} {
		if err := e.h.RebuildMetadata(ctx, e.repos[name], logf); err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Errorf("%s: %v", name, err)
		}
	}
	_ = meta.ErrNotFound
}

func isIndexPath(p string) bool { return isRepodata(p) }
