package r

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestDerivedMemory: building PACKAGES.rds and PACKAGES.gz of a CRAN-sized
// merge (22000 packages, 7.5 MB) holds the stanzas (15 MiB), not also the
// file built from them (27 to 30 MiB in all).
func TestDerivedMemory(t *testing.T) {
	e := newEnv(t)
	var b strings.Builder
	for i := range 22000 {
		fmt.Fprintf(&b, "Package: pkg%05d\nVersion: 1.0.%d\nDepends: R (>= 3.5.0), methods\nImports: pkg%05d, stats, utils, graphics,\n        grDevices\nSuggests: testthat (>= 3.0.0), knitr, rmarkdown\nLicense: GPL-2 | GPL-3\nMD5sum: %032d\nNeedsCompilation: no\n\n", i, i, (i+1)%22000, i)
	}
	merged, err := e.h.Store.Put(context.Background(), "r-all", "src/contrib/PACKAGES", ctText, strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	b = strings.Builder{}
	defer debug.SetGCPercent(debug.SetGCPercent(5))
	for _, file := range []string{"PACKAGES.rds", "PACKAGES.gz"} {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		base := ms.HeapInuse
		var peak atomic.Uint64
		stop := make(chan struct{})
		go func() {
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
		_, err := e.h.derived(context.Background(), e.repos["r-all"], "src/contrib", file, merged)
		close(stop)
		if err != nil {
			t.Fatal(err)
		}
		if got := (peak.Load() - base) >> 20; got > 20 && !raceEnabled {
			t.Errorf("%s: %d MiB of heap building it from 7.5 MB", file, got)
		}
	}
}
