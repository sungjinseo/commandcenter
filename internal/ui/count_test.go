package ui

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/meta"
)

// TestCountComponentsHoldsOneUnit: the dashboard's component count holds
// the components of one package or one unit of paths at a time, and counts
// them all. A set of every component of a repository of a million took the
// server past 128 MiB, from the dashboard and the leader's usage refresh.
func TestCountComponentsHoldsOneUnit(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	ctx := context.Background()
	const n = 10000
	long := strings.Repeat("g", 600)
	before := map[string]int64{}
	for _, name := range []string{"maven-releases", "pypi-hosted"} {
		c, err := e.ui.countComponents(ctx, e.ui.Repos.Get(name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = c
	}
	err := e.db.Tx(ctx, func(tx *meta.Tx) error {
		for i := range n {
			for _, f := range []string{".jar", ".pom"} { // two files, one component
				p := fmt.Sprintf("com/%s%d/a/1.%d/a-1.%d%s", long, i, i%3, i%3, f)
				if err := tx.PutAsset(ctx, meta.Asset{Repo: "maven-releases", Path: p, BlobSHA256: fmt.Sprintf("%064d", i), Size: 1}); err != nil {
					return err
				}
			}
			for _, f := range []string{".tar.gz", "-py3-none-any.whl"} {
				name := fmt.Sprintf("p%s%d", long, i)
				if err := tx.InsertPackage(ctx, meta.Package{Repo: "pypi-hosted", Format: "pypi", Name: name, Version: name + "-1.0" + f, Attrs: "{}"}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer debug.SetGCPercent(debug.SetGCPercent(5)) // garbage aside
	for _, name := range []string{"maven-releases", "pypi-hosted"} {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		base := ms.HeapInuse
		var peak atomic.Uint64
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			var ms runtime.MemStats
			for {
				select {
				case <-stop:
					return
				case <-time.After(time.Millisecond):
				}
				runtime.ReadMemStats(&ms)
				peak.Store(max(peak.Load(), ms.HeapInuse))
			}
		}()
		got, err := e.ui.countComponents(ctx, e.ui.Repos.Get(name))
		close(stop)
		<-done
		if err != nil || got != before[name]+n {
			t.Fatalf("%s: %d components, %v; want %d", name, got, err, before[name]+n)
		}
		if held := int64(peak.Load()) - int64(base); held > 3<<20 && !raceEnabled {
			t.Errorf("%s: %d KiB held counting %d components", name, held>>10, n)
		}
	}
}
