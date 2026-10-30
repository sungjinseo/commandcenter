package tasks

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/meta"
)

// seedBuilds stores builds of org.example:app:1.0-SNAPSHOT in mvn-none, the
// i-th days[i] days old, each a jar and a POM, and rebuilds the metadata.
func (h *harness) seedBuilds(t *testing.T, days ...int) []string {
	t.Helper()
	var versions []string
	for i, d := range days {
		v := fmt.Sprintf("1.0-202607%02d.120000-%d", i+1, i+1)
		versions = append(versions, v)
		h.clock.Set(t0.AddDate(0, 0, -d))
		for _, ext := range []string{"jar", "pom"} {
			p := "org/example/app/1.0-SNAPSHOT/app-" + v + "." + ext
			h.put(t, "mvn-none", p, p)
		}
	}
	h.clock.Set(t0)
	if _, err := h.mvn.RebuildMetadata(context.Background(), h.reg.Get("mvn-none"), 0, nil); err != nil {
		t.Fatal(err)
	}
	return versions
}

// builds lists the builds of 1.0-SNAPSHOT left in mvn-none.
func (h *harness) builds(t *testing.T) string {
	t.Helper()
	as, err := h.db.ListAssets(context.Background(), "mvn-none", "org/example/app/1.0-SNAPSHOT/")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range as {
		if v, ok := strings.CutSuffix(strings.TrimPrefix(a.Path, "org/example/app/1.0-SNAPSHOT/app-"), ".jar"); ok {
			out = append(out, v)
		}
	}
	return strings.Join(out, " ")
}

func (h *harness) versionDoc(t *testing.T) string {
	t.Helper()
	a, err := h.db.GetAsset(context.Background(), "mvn-none", "org/example/app/1.0-SNAPSHOT/maven-metadata.xml")
	if err != nil {
		return err.Error()
	}
	f, _ := h.store.Open(a)
	defer f.Close()
	b, _ := io.ReadAll(f)
	return string(b)
}

// Scope build (Nexus's remove-snapshots, core-18): the newest
// minimumRetained builds of each SNAPSHOT version are kept, older builds
// go when past the retention, and the version document follows.
func TestPurgeSnapshotBuilds(t *testing.T) {
	ctx := context.Background()
	t.Run("retention per version", func(t *testing.T) {
		h := newHarness(t)
		v := h.seedBuilds(t, 90, 80, 70, 60, 50)
		out, tk := h.run(t, TypePurge, `{"repository":"mvn-none","minimumRetained":2}`)
		if out.Result != ResultOK || out.Removed != 3 || !strings.HasPrefix(out.Message, "mvn-none: removed 3 of 5 SNAPSHOT builds (6 files, ") {
			t.Fatalf("%+v\n%s", out, tk.LastLog)
		}
		if got := h.builds(t); got != v[3]+" "+v[4] {
			t.Fatalf("left: %s", got)
		}
		if doc := h.versionDoc(t); !strings.Contains(doc, "<buildNumber>5</buildNumber>") || strings.Contains(doc, v[2]) {
			t.Fatalf("version document:\n%s", doc)
		}
		if tk.Params != `{"repository":"mvn-none","scope":"build","minimumRetained":2,"snapshotRetentionDays":30}` {
			t.Fatalf("canonical: %s", tk.Params)
		}
	})
	t.Run("fresh builds count toward the minimum", func(t *testing.T) {
		h := newHarness(t)
		v := h.seedBuilds(t, 90, 80, 5)
		if out, _ := h.run(t, TypePurge, `{"repository":"mvn-none","minimumRetained":2}`); out.Removed != 1 || h.builds(t) != v[1]+" "+v[2] {
			t.Fatalf("%+v: %s", out, h.builds(t))
		}
	})
	t.Run("minimumRetained -1 removes none by age", func(t *testing.T) {
		h := newHarness(t)
		h.seedBuilds(t, 90, 80)
		if out, _ := h.run(t, TypePurge, `{"repository":"mvn-none","minimumRetained":-1,"snapshotRetentionDays":0}`); out.Removed != 0 {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("released in another repository", func(t *testing.T) {
		h := newHarness(t)
		h.seedBuilds(t, 90, 3)
		h.put(t, "mvn", "org/example/app/1.0/app-1.0.jar", "release")
		// Grace 30: the build 3 days old stays, retained or not.
		out, tk := h.run(t, TypePurge, `{"repository":"mvn-none","minimumRetained":5,"removeIfReleased":true,"gracePeriodDays":30}`)
		if out.Removed != 1 || !strings.Contains(tk.LastLog, "(released;") || h.builds(t) != "1.0-20260702.120000-2" {
			t.Fatalf("%+v: %s\n%s", out, h.builds(t), tk.LastLog)
		}
		if out, _ := h.run(t, TypePurge, `{"repository":"mvn-none","minimumRetained":5,"removeIfReleased":true}`); out.Removed != 1 || h.builds(t) != "" {
			t.Fatalf("grace 0: %+v %s", out, h.builds(t))
		}
		if _, err := h.db.GetAsset(ctx, "mvn-none", "org/example/app/1.0-SNAPSHOT/maven-metadata.xml"); err == nil {
			t.Fatal("the version document of a version without builds stays")
		}
	})
	t.Run("star and groups", func(t *testing.T) {
		h := newHarness(t)
		h.seedBuilds(t, 90, 80)
		h.put(t, "mvn", "org/example/lib/2.0-SNAPSHOT/lib-2.0-20260101.000000-1.jar", "old")
		h.put(t, "mvn", "org/example/lib/2.0-SNAPSHOT/lib-2.0-20260102.000000-2.jar", "old")
		if out, _ := h.run(t, TypePurge, `{"repository":"mvn-group","minimumRetained":1,"snapshotRetentionDays":0}`); out.Removed != 1 || h.builds(t) == "" {
			t.Fatalf("group: %+v", out)
		}
		out, _ := h.run(t, TypePurge, `{"repository":"*","minimumRetained":1,"snapshotRetentionDays":0}`)
		if out.Removed != 1 || h.builds(t) != "1.0-20260702.120000-2" || !strings.Contains(out.Message, "repositories: removed 1 of ") {
			t.Fatalf("star: %+v %s", out, h.builds(t))
		}
	})
	t.Run("unused", func(t *testing.T) {
		h := newHarness(t)
		v := h.seedBuilds(t, 90, 80, 5)
		if err := h.db.FlushDownloads(ctx, []meta.DownloadDelta{{Repo: "mvn-none", Path: "org/example/app/1.0-SNAPSHOT/app-" + v[0] + ".jar", N: 1, Last: t0.Add(-time.Hour)}}); err != nil {
			t.Fatal(err)
		}
		// v0 downloaded an hour ago, v1 never (stored 80 days ago), v2 stored 5 days ago.
		out, tk := h.run(t, TypeMavenPurgeUnused, `{"repository":"mvn-none","lastUsed":10}`)
		if out.Removed != 1 || h.builds(t) != v[0]+" "+v[2] || !strings.Contains(tk.LastLog, "not used for 10 days") {
			t.Fatalf("%+v: %s\n%s", out, h.builds(t), tk.LastLog)
		}
		for params, want := range map[string]string{
			`{"repository":"mvn-none","lastUsed":0}`:  "params.lastUsed must be at least 1",
			`{"repository":"mvn-proxy","lastUsed":1}`: "takes a Maven hosted repository or group",
		} {
			if _, err := h.s.Create(ctx, Def{Name: "x", Type: TypeMavenPurgeUnused, Schedule: "manual", Params: []byte(params)}); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %v, want ~%q", params, err, want)
			}
		}
	})
}
