package conda

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"commandcenter/internal/meta"
)

// The rebuild-metadata task regenerates the index files byte for byte.
func TestRebuildMetadata(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if w := e.do("PUT", "conda-hosted", "noarch/foo-1.0-py_0.conda", condaFile("foo-1.0-py_0")); w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	rp := e.repos["conda-hosted"]
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
	// Not for a proxy or group.
	for _, name := range []string{"conda-proxy", "conda-all"} {
		if err := e.h.RebuildMetadata(ctx, e.repos[name], logf); err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Errorf("%s: %v", name, err)
		}
	}
	_ = meta.ErrNotFound
}

func isIndexPath(p string) bool { return strings.HasSuffix(p, ".json") }
