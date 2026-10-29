package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
)

// components lists the components of the mvn repository, as labels.
func (h *harness) components(t *testing.T) []string {
	t.Helper()
	as, err := h.db.ListAssets(context.Background(), "mvn", "")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range as {
		if g, n, v, ok := component.DeriveAsset("maven", a.Path); ok {
			out = append(out, g+":"+n+":"+v)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// The preview lists exactly what the cleanup runs delete, with one scan
// covering the repository and with scans resumed across runs.
func TestPreviewMatchesCleanupRuns(t *testing.T) {
	for _, budget := range []int{0, 6} { // 6: three scans, each name whole in one
		t.Run(fmt.Sprint("budget ", budget), func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			h.clock.Set(t0.AddDate(0, 0, -60))
			for _, v := range []string{"1.0", "1.1", "1.2", "1.3", "1.4"} {
				h.put(t, "mvn", "org/example/lib/"+v+"/lib-"+v+".jar", "old")
			}
			h.put(t, "mvn", "org/example/app/1.0/app-1.0.jar", "old")
			h.put(t, "mvn", "org/example/app/2.0/app-2.0.jar", "old")
			h.put(t, "mvn", "org/example/tool/0.9/tool-0.9.jar", "old")
			h.put(t, "mvn", "org/example/tool/1.0-rc1/tool-1.0-rc1.jar", "old")
			h.put(t, "mvn", "org/example/tool/1.0/tool-1.0.jar", "old")
			h.put(t, "mvn", "org/example/tool/1.1/tool-1.1.jar", "old")
			h.clock.Set(t0)
			h.put(t, "mvn", "org/example/lib/1.10/lib-1.10.jar", "new")
			p := cleanup.Policy{Name: "old", Format: "maven", Criteria: cleanup.Criteria{ComponentAgeDays: 30, Retain: 2,
				Exclude: cleanup.Exclusion{VersionPattern: `0\..*`}}}
			if err := cleanup.UpdatePolicy(ctx, h.db, p); err != nil {
				t.Fatal(err)
			}
			h.s.ScanBudget = budget
			p, _ = cleanup.GetPolicy(ctx, h.db, "old")

			var preview []string
			err := h.s.PreviewCleanup(ctx, h.reg.Get("mvn"), p, func(c cleanup.Candidate) error {
				preview = append(preview, c.Label())
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(preview)
			// lib keeps 1.10 and 1.4; tool keeps 1.1 and 1.0, and 0.9 is excluded.
			want := []string{"org.example:lib:1.0", "org.example:lib:1.1", "org.example:lib:1.2", "org.example:lib:1.3", "org.example:tool:1.0-rc1"}
			if !slices.Equal(preview, want) {
				t.Fatalf("preview: %v, want %v", preview, want)
			}

			before := h.components(t)
			tk := h.create(t, Def{Name: "c", Type: TypeCleanup, Schedule: "manual", Params: json.RawMessage(`{"repos":["mvn"]}`)})
			for range 4 { // enough runs to cover the repository with budget 6
				if out, err := h.s.RunNow(tk.ID, "admin"); err != nil || out.Result != ResultOK {
					t.Fatalf("run: %+v %v", out, err)
				}
			}
			after := h.components(t)
			var deleted []string
			for _, c := range before {
				if !slices.Contains(after, c) {
					deleted = append(deleted, c)
				}
			}
			if !slices.Equal(deleted, preview) {
				t.Fatalf("deleted %v, preview %v", deleted, preview)
			}
		})
	}
}
