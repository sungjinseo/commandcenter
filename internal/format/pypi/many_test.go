package pypi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"commandcenter/internal/meta"
)

// TestHostedManyFiles: the JSON API document and the project page of a
// hosted project with many files hold a batch of rows at a time, and list
// every file. Holding them all per request took a 128 MiB server down at 16
// clients on a project of 1500 releases of three files.
func TestHostedManyFiles(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const releases = 1000
	err := e.h.Store.DB.Tx(ctx, func(tx *meta.Tx) error {
		for i := range releases {
			v := fmt.Sprintf("1.%d.0", i)
			for _, f := range []string{"many-" + v + ".tar.gz", "many-" + v + "-py3-none-any.whl", "many-" + v + "-cp312-cp312-manylinux_2_17_x86_64.whl"} {
				a, _ := json.Marshal(fileAttrs{Filename: f, Version: v, SHA256: fmt.Sprintf("%064d", i), RequiresPython: ">=3.8", MetadataSHA256: noMetadata})
				if err := tx.InsertPackage(ctx, meta.Package{Repo: "py-hosted", Format: "pypi", Name: "many", Version: f, Attrs: string(a), CreatedAt: time.UnixMilli(int64(1700000000000 + i))}); err != nil {
					return err
				}
				if err := tx.PutAsset(ctx, meta.Asset{Repo: "py-hosted", Path: fileKey("many", f), BlobSHA256: fmt.Sprintf("%064d", i), Size: 1000, MD5: fmt.Sprintf("%032d", i)}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		w := liveDuring(t, func(w http.ResponseWriter) {
			e.h.Serve(w, httptest.NewRequest("GET", "/repository/py-hosted/"+path, nil), e.repos["py-hosted"], path)
		}, 768<<10) // the releases' file names; holding the document was 1.8 MiB
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return w
	}
	d := decodeDoc(t, get("pypi/many/json").Body.Bytes())
	if len(d.Releases) != releases || len(d.URLs) != 3 || d.Info["version"] != "1.999.0" || len(d.Releases["1.5.0"]) != 3 {
		t.Fatalf("doc: %d releases, %d urls, %v", len(d.Releases), len(d.URLs), d.Info["version"])
	}
	if f := d.Releases["1.5.0"][1]; f["filename"] != "many-1.5.0-py3-none-any.whl" || f["md5_digest"] != fmt.Sprintf("%032d", 5) || f["size"] != float64(1000) {
		t.Fatalf("file: %v", f)
	}
	d = decodeDoc(t, get("pypi/many/1.5.0/json").Body.Bytes())
	if d.Releases != nil || len(d.URLs) != 3 || d.Info["version"] != "1.5.0" {
		t.Fatalf("version doc: %v %d", d.Info["version"], len(d.URLs))
	}
}

// liveDuring runs serve and fails t when more than limit bytes of heap, the
// body aside, are live at one of its writes.
func liveDuring(t *testing.T, serve func(http.ResponseWriter), limit int64) *httptest.ResponseRecorder {
	t.Helper()
	lw := &liveHeap{ResponseRecorder: httptest.NewRecorder()}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	serve(lw)
	if live := int64(lw.max) - int64(ms.HeapAlloc); live > limit {
		t.Errorf("%d KiB live while writing a body of %d KiB", live>>10, lw.Body.Len()>>10)
	}
	return lw.ResponseRecorder
}

// liveHeap records the most heap live at a write of the response, less the
// body written before it.
type liveHeap struct {
	*httptest.ResponseRecorder
	max uint64
}

func (l *liveHeap) Write(p []byte) (int, error) {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	l.max = max(l.max, ms.HeapAlloc-uint64(l.Body.Cap()))
	return l.ResponseRecorder.Write(p)
}
