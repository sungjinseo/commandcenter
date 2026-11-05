package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/meta"
)

// What no tag reaches goes, once it has gone unused long enough: an index's
// manifests after the index was deleted, and their layers. What a tag reaches
// directly or through an index stays, and so does a manifest pulled by digest.
func TestCollectUnused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ci := bearer(e.token("ci", "repository:dh/app:pull,push,delete"))
	rp := e.h.Repos.Get("dh")
	collect := func(preview bool) int64 {
		t.Helper()
		n, err := e.h.CollectUnused(ctx, rp, e.now.Add(-24*time.Hour), preview, "admin", t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	has := func(path string) bool {
		_, err := e.h.Store.DB.GetAsset(ctx, "dh", path)
		return err == nil
	}
	index := func(tag string, digests ...string) string {
		var m []string
		for _, d := range digests {
			m = append(m, `{"mediaType":"`+mtOCIManifest+`","digest":"`+d+`","size":1}`)
		}
		body := []byte(`{"schemaVersion":2,"mediaType":"` + mtOCIIndex + `","manifests":[` + strings.Join(m, ",") + `]}`)
		resp, b := e.do("PUT", "/v2/dh/app/manifests/"+tag, body, ci, hdr("Content-Type", mtOCIIndex))
		e.check(resp, b, 201, "")
		return digestOf(body)
	}

	_, tagged := e.pushImage("dh/app", "v1", [][]byte{[]byte("tagged-layer")}, ci)
	_, kept := ociManifest(e, "dh/app", []byte("kept-layer"), ci)
	index("multi", kept)
	_, gone := ociManifest(e, "dh/app", []byte("gone-layer"), ci)
	goneIndex := index("old", gone)
	_, pulled := ociManifest(e, "dh/app", []byte("pulled-layer"), ci)
	goneLayer := digestOf([]byte("gone-layer"))
	if !has(blobKey("app", goneLayer)) {
		t.Fatal("the layer was not stored")
	}
	// Deleting the index leaves its manifest, with nothing that reaches it.
	resp, b := e.do("DELETE", "/v2/dh/app/manifests/"+goneIndex, nil, ci)
	e.check(resp, b, 202, "")
	if collect(false) != 0 || !has(manifestKey("app", gone)) {
		t.Fatal("removed a manifest stored just now")
	}

	// A push of an index stores its manifests first: not before the offset.
	e.now = e.now.Add(48 * time.Hour)
	// Pulled by digest a moment ago: in use, though no tag names it.
	if err := e.h.Store.DB.FlushDownloads(ctx, []meta.DownloadDelta{{Repo: "dh", Path: manifestKey("app", pulled), N: 1, Last: e.now}}); err != nil {
		t.Fatal(err)
	}
	if n := collect(true); n != 1 || !has(manifestKey("app", gone)) {
		t.Fatalf("preview: %d, or it removed something", n)
	}
	if n := collect(false); n != 1 {
		t.Fatalf("removed %d manifests, want 1", n)
	}
	if has(manifestKey("app", gone)) || has(blobKey("app", goneLayer)) {
		t.Fatal("the unused manifest or its layer is still there")
	}
	for _, d := range []string{tagged, kept, pulled} {
		if !has(manifestKey("app", d)) {
			t.Fatalf("manifest %s was removed", d)
		}
	}
	if !has(blobKey("app", digestOf([]byte("tagged-layer")))) || !has(blobKey("app", digestOf([]byte("kept-layer")))) {
		t.Fatal("a layer in use was removed")
	}
	resp, b = e.do("GET", "/v2/dh/app/manifests/multi", nil)
	e.check(resp, b, 200, "")
	if n := collect(false); n != 0 {
		t.Fatalf("a second run removed %d", n)
	}

	// A layer of a push that never finished: unused like the rest, after the
	// time a push may take even with a shorter offset.
	ci = bearer(e.token("ci", "repository:dh/app:pull,push,delete")) // the first one expired
	stray := e.pushBlob("dh/app", []byte("stray-layer"), ci)
	e.now = e.now.Add(2 * time.Hour)
	if _, err := e.h.CollectUnused(ctx, rp, e.now.Add(-time.Hour), false, "admin", t.Logf); err != nil {
		t.Fatal(err)
	}
	if !has(blobKey("app", stray)) {
		t.Fatal("a layer of a push that may still be going on was removed")
	}
	e.now = e.now.Add(25 * time.Hour)
	if _, err := e.h.CollectUnused(ctx, rp, e.now.Add(-time.Hour), false, "admin", t.Logf); err != nil {
		t.Fatal(err)
	}
	if has(blobKey("app", stray)) {
		t.Fatal("the stray layer is still there")
	}
}
