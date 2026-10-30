package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// blobPath is where the file store keeps blob sha.
func (h *harness) blobPath(sha string) string {
	return filepath.Join(h.dir, "blobs", "sha256", sha[:2], sha[2:4], sha)
}

func (h *harness) run(t *testing.T, typ, params string) (Outcome, meta.Task) {
	t.Helper()
	tk := h.create(t, Def{Name: typ, Type: typ, Schedule: "manual", Params: json.RawMessage(params)})
	out, err := h.s.RunNow(tk.ID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	return out, h.get(t, tk.ID)
}

func TestVerifyBlobsTask(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i, p := range []string{"a/x/1.0/x-1.0.jar", "a/x/1.0/x-1.0.pom", "a/y/2.0/y-2.0.jar"} {
		h.put(t, "mvn", p, fmt.Sprintf("content %d", i))
	}
	h.put(t, "mvn-none", "b/z/1.0/z-1.0.jar", "other repository")
	out, _ := h.run(t, TypeVerify, `{}`)
	if out.Result != ResultOK || out.Message != "checked 4 assets in 9 repositories: 0 missing, 0 mismatched" {
		t.Fatalf("clean: %+v", out)
	}

	// One blob deleted, one corrupted (same length: only the checksum tells).
	gone, _ := h.db.GetAsset(ctx, "mvn", "a/x/1.0/x-1.0.pom")
	if err := os.Remove(h.blobPath(gone.BlobSHA256)); err != nil {
		t.Fatal(err)
	}
	bad, _ := h.db.GetAsset(ctx, "mvn", "a/y/2.0/y-2.0.jar")
	os.Chmod(h.blobPath(bad.BlobSHA256), 0o644)
	if err := os.WriteFile(h.blobPath(bad.BlobSHA256), []byte("CONTENT 2"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, tk := h.run(t, TypeVerify, `{"repository":"mvn"}`)
	if out.Result != ResultFailed || !strings.Contains(out.Message, "1 blobs missing, 0 mismatched") || !strings.Contains(out.Message, "checked 3 assets in 1 repositories: 1 missing, 0 mismatched") {
		t.Fatalf("stat only: %+v", out)
	}
	if !strings.Contains(tk.LastLog, "ERROR missing: mvn/a/x/1.0/x-1.0.pom (blob "+gone.BlobSHA256) {
		t.Fatalf("log:\n%s", tk.LastLog)
	}
	out, tk = h.run(t, TypeVerify, `{"repository":"mvn","checksum":true}`)
	if out.Result != ResultFailed || !strings.Contains(out.Message, "1 blobs missing, 1 mismatched") || out.Bytes != int64(len("content 0")+len("content 2")) {
		t.Fatalf("checksum: %+v", out)
	}
	if !strings.Contains(tk.LastLog, "ERROR mismatch: mvn/a/y/2.0/y-2.0.jar: the blob's SHA-256 is ") || !strings.Contains(tk.LastLog, ", not "+bad.BlobSHA256) {
		t.Fatalf("log:\n%s", tk.LastLog)
	}
	if got, _ := h.db.GetAsset(ctx, "mvn", "a/y/2.0/y-2.0.jar"); got.Path == "" {
		t.Fatal("verify deleted an asset")
	}

	// The byte budget: the first asset is always read, then the run stops
	// and stores where the next one resumes; the next run reaches the end.
	tk = h.create(t, Def{Name: "budget", Type: TypeVerify, Schedule: "manual", Params: json.RawMessage(`{"repository":"mvn","checksum":true,"maxBytes":1}`)})
	out, _ = h.s.RunNow(tk.ID, "admin")
	cur, _ := h.db.GetSetting(ctx, cursorKey(tk.ID, "mvn"))
	if out.Result != ResultOK || cur != "a/x/1.0/x-1.0.pom" || !strings.Contains(h.get(t, tk.ID).LastLog, `the next run resumes at "a/x/1.0/x-1.0.pom"`) {
		t.Fatalf("budget: %+v cursor %q", out, cur)
	}
	out, _ = h.s.RunNow(tk.ID, "admin")
	cur, _ = h.db.GetSetting(ctx, cursorKey(tk.ID, "mvn"))
	if out.Result != ResultFailed || cur != "a/y/2.0/y-2.0.jar" || !strings.Contains(out.Message, "1 blobs missing, 0 mismatched") {
		t.Fatalf("resumed: %+v cursor %q", out, cur)
	}
	out, _ = h.s.RunNow(tk.ID, "admin")
	if cur, err := h.db.GetSetting(ctx, cursorKey(tk.ID, "mvn")); out.Result != ResultFailed || cur != "" || (err != nil && !errors.Is(err, meta.ErrNotFound)) {
		t.Fatalf("end: %+v cursor %q %v", out, cur, err)
	}
	// Over every repository the budget stops the later ones.
	out, tk = h.run(t, TypeVerify, `{"checksum":true,"maxBytes":1}`)
	if !strings.Contains(tk.LastLog, "mvn-none: not checked: the byte budget was used up") {
		t.Fatalf("all: %+v\n%s", out, tk.LastLog)
	}

	// Validation and the canonical form.
	if _, err := h.s.Create(ctx, Def{Name: "x", Type: TypeVerify, Schedule: "manual", Params: json.RawMessage(`{"repository":"nope"}`)}); err == nil || !strings.Contains(err.Error(), `"nope" does not exist`) {
		t.Fatalf("missing repository: %v", err)
	}
	if _, err := h.s.Create(ctx, Def{Name: "x", Type: TypeVerify, Schedule: "manual", Params: json.RawMessage(`{"maxBytes":-1}`)}); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("negative: %v", err)
	}
	if got := h.create(t, Def{Name: "x", Type: TypeVerify, Schedule: "manual"}); got.Params != `{"maxBytes":10737418240}` {
		t.Fatalf("canonical: %s", got.Params)
	}
}

// seedSnapshots stores a GA with three SNAPSHOT versions of different ages
// and a release, in mvn-none.
func (h *harness) seedSnapshots(t *testing.T) {
	t.Helper()
	for _, s := range []struct {
		days int
		path string
	}{
		{90, "org/example/app/1.0-SNAPSHOT/app-1.0-20260720.120000-1.jar"},
		{89, "org/example/app/1.0-SNAPSHOT/app-1.0-20260721.120000-2.jar"},
		{89, "org/example/app/1.0-SNAPSHOT/app-1.0-20260721.120000-2.pom"},
		{80, "org/example/app/1.0/app-1.0.jar"},
		{40, "org/example/app/1.1-SNAPSHOT/app-1.1-20260909.120000-1.jar"},
		{5, "org/example/app/1.2-SNAPSHOT/app-1.2-20261015.120000-1.jar"},
		{5, "org/example/app/1.2-SNAPSHOT/app-1.2-20261015.120000-1.pom"},
	} {
		h.clock.Set(t0.AddDate(0, 0, -s.days))
		h.put(t, "mvn-none", s.path, s.path)
	}
	h.clock.Set(t0)
	// The version documents, as uploads leave them.
	if _, err := h.mvn.RebuildMetadata(context.Background(), h.reg.Get("mvn-none"), 0, nil); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) versions(t *testing.T) string {
	t.Helper()
	ents, _, err := h.db.ListDir(context.Background(), "mvn-none", "org/example/app/", 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if e.Dir {
			out = append(out, e.Name)
		}
	}
	return strings.Join(out, " ")
}

// Scope version: the behaviour of tasks created before 0.3 (whole SNAPSHOT
// versions per GA), which the schema 8 upgrade stamps on them.
func TestPurgeSnapshotsTask(t *testing.T) {
	ctx := context.Background()
	t.Run("preview then defaults", func(t *testing.T) {
		h := newHarness(t)
		h.seedSnapshots(t)
		h.sink.Reset()
		out, tk := h.run(t, TypePurge, `{"repository":"mvn-none","scope":"version","preview":true}`)
		if out.Result != ResultOK || out.Message != "mvn-none: preview: 2 of 3 SNAPSHOT versions would be removed (4 files, "+fmt.Sprint(bytesOf(h, "1.0-SNAPSHOT", "1.1-SNAPSHOT"))+" bytes)" || out.Removed != 0 {
			t.Fatalf("preview: %+v", out)
		}
		if !strings.Contains(tk.LastLog, "would remove org.example:app:1.0-SNAPSHOT (older than 30 days; 3 files") || !strings.Contains(tk.LastLog, "would remove org.example:app:1.1-SNAPSHOT (older than 30 days; 1 files") {
			t.Fatalf("log:\n%s", tk.LastLog)
		}
		if v := h.versions(t); v != "1.0-SNAPSHOT 1.0 1.1-SNAPSHOT 1.2-SNAPSHOT" {
			t.Fatalf("preview deleted: %s", v)
		}
		if tk.Params != `{"repository":"mvn-none","scope":"version","minimumRetained":1,"snapshotRetentionDays":30,"preview":true}` {
			t.Fatalf("canonical: %s", tk.Params)
		}
		out, tk = h.run(t, TypePurge, `{"repository":"mvn-none","scope":"version"}`)
		if out.Result != ResultOK || out.Removed != 2 || !strings.HasPrefix(out.Message, "mvn-none: removed 2 of 3 SNAPSHOT versions (4 files, ") {
			t.Fatalf("defaults: %+v", out)
		}
		if v := h.versions(t); v != "1.0 1.2-SNAPSHOT" {
			t.Fatalf("left: %s", v)
		}
		// The GA metadata was regenerated by the component delete.
		a, err := h.db.GetAsset(ctx, "mvn-none", "org/example/app/maven-metadata.xml")
		if err != nil {
			t.Fatal(err)
		}
		f, _ := h.store.Open(a)
		b, _ := io.ReadAll(f)
		f.Close()
		if strings.Contains(string(b), "1.1-SNAPSHOT") || !strings.Contains(string(b), "<version>1.2-SNAPSHOT</version>") {
			t.Fatalf("metadata:\n%s", b)
		}
		if _, err := h.db.GetAsset(ctx, "mvn-none", "org/example/app/1.1-SNAPSHOT/maven-metadata.xml"); !errors.Is(err, meta.ErrNotFound) {
			t.Fatalf("version document left: %v", err)
		}
		deletes := 0
		for _, e := range h.sink.Events() {
			if e.Kind == "delete" && e.Actor == "admin" {
				deletes++
			}
		}
		if deletes != 3 { // one per build: 1.0-SNAPSHOT had two
			t.Fatalf("events: %s", h.sink)
		}
	})
	t.Run("count only", func(t *testing.T) {
		h := newHarness(t)
		h.seedSnapshots(t)
		if out, _ := h.run(t, TypePurge, `{"repository":"mvn-none","scope":"version","minimumRetained":2,"snapshotRetentionDays":0}`); out.Removed != 1 || h.versions(t) != "1.0 1.1-SNAPSHOT 1.2-SNAPSHOT" {
			t.Fatalf("%+v: %s", out, h.versions(t))
		}
	})
	t.Run("released", func(t *testing.T) {
		h := newHarness(t)
		h.seedSnapshots(t)
		// Retained by count, removed because 1.0 was released: the grace
		// period (release 80 days old) decides.
		if out, _ := h.run(t, TypePurge, `{"repository":"mvn-none","scope":"version","minimumRetained":5,"removeIfReleased":true,"gracePeriodDays":100}`); out.Removed != 0 {
			t.Fatalf("within grace: %+v", out)
		}
		out, tk := h.run(t, TypePurge, `{"repository":"mvn-none","scope":"version","minimumRetained":5,"removeIfReleased":true,"gracePeriodDays":60}`)
		if out.Removed != 1 || h.versions(t) != "1.0 1.1-SNAPSHOT 1.2-SNAPSHOT" || !strings.Contains(tk.LastLog, "removed org.example:app:1.0-SNAPSHOT (released;") {
			t.Fatalf("%+v: %s\n%s", out, h.versions(t), tk.LastLog)
		}
	})
	t.Run("unused", func(t *testing.T) {
		h := newHarness(t)
		h.seedSnapshots(t)
		if err := h.db.FlushDownloads(ctx, []meta.DownloadDelta{{Repo: "mvn-none", Path: "org/example/app/1.1-SNAPSHOT/app-1.1-20260909.120000-1.jar", N: 1, Last: t0.Add(-time.Hour)}}); err != nil {
			t.Fatal(err)
		}
		// 1.0-SNAPSHOT: never downloaded, stored 89 days ago. 1.1: downloaded
		// an hour ago. 1.2: never downloaded, stored 5 days ago.
		out, tk := h.run(t, TypePurge, `{"repository":"mvn-none","scope":"version","minimumRetained":0,"snapshotRetentionDays":0,"lastDownloadedDays":10}`)
		if out.Removed != 1 || h.versions(t) != "1.0 1.1-SNAPSHOT 1.2-SNAPSHOT" || !strings.Contains(tk.LastLog, "(not downloaded for 10 days;") {
			t.Fatalf("%+v: %s", out, h.versions(t))
		}
	})
	t.Run("validation", func(t *testing.T) {
		h := newHarness(t)
		for params, want := range map[string]string{
			`{}`:                    "params.repository is required",
			`{"repository":"nope"}`: `"nope" does not exist`,
			`{"repository":"mvn-group","scope":"version"}`:                "scope version takes a Maven hosted repository",
			`{"repository":"crates"}`:                                     "the task takes a Maven hosted or proxy repository or group",
			`{"repository":"mvn-ro"}`:                                     "read-only",
			`{"repository":"*","scope":"version"}`:                        `"*" does not exist`,
			`{"repository":"mvn","scope":"x"}`:                            `params.scope "x"`,
			`{"repository":"mvn","minimumRetained":-2}`:                   "params.minimumRetained must be at least -1",
			`{"repository":"mvn","scope":"version","minimumRetained":-1}`: "params.minimumRetained must be at least 0",
			`{"repository":"mvn","snapshotRetentionDays":-1}`:             "params.snapshotRetentionDays must be at least 0",
		} {
			_, err := h.s.Create(ctx, Def{Name: "x", Type: TypePurge, Schedule: "manual", Params: json.RawMessage(params)})
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %v, want ~%q", params, err, want)
			}
		}
	})
}

func bytesOf(h *harness, versions ...string) int64 {
	var n int64
	for _, v := range versions {
		as, _ := h.db.ListAssets(context.Background(), "mvn-none", "org/example/app/"+v+"/")
		for _, a := range as {
			if !strings.HasSuffix(a.Path, "maven-metadata.xml") {
				n += a.Size
			}
		}
	}
	return n
}

func TestBlobstoreTasks(t *testing.T) {
	h := newHarness(t)
	h.put(t, "mvn", "a/x/1.0/x-1.0.jar", "one")
	h.put(t, "mvn", "a/x/1.0/x-1.0.pom", "two")
	if _, err := h.store.Blobs[repo.DefaultStore].Write(strings.NewReader("unreferenced")); err != nil {
		t.Fatal(err)
	}
	out, tk := h.run(t, TypeRecalc, `{}`)
	if out.Result != ResultOK || out.Message != "3 blobs in 1 blob stores; the sizes shown are computed live from the asset rows, nothing to recalculate" ||
		!strings.Contains(tk.LastLog, "default: 3 blobs in the store, 2 referenced by assets (6 bytes); 1 would go at the next compaction") {
		t.Fatalf("recalc: %+v\n%s", out, tk.LastLog)
	}

	tmp := filepath.Join(h.dir, "tmp")
	for name, age := range map[string]time.Duration{"up-old": 2 * time.Hour, "up-new": time.Minute, "ses-old": 25 * time.Hour, "ses-new": 23 * time.Hour, "other": 48 * time.Hour} {
		p := filepath.Join(tmp, name)
		os.WriteFile(p, []byte("x"), 0o644)
		os.Chtimes(p, time.Now().Add(-age), time.Now().Add(-age))
	}
	out, _ = h.run(t, TypeTemp, `{"blobStore":"default"}`)
	if out.Result != ResultOK || out.Removed != 2 || out.Message != "removed 2 stale temp files and upload sessions from 1 blob stores" {
		t.Fatalf("temp: %+v", out)
	}
	left, _ := os.ReadDir(tmp)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != "other ses-new up-new" {
		t.Fatalf("left: %v", names)
	}
	for _, typ := range []string{TypeRecalc, TypeTemp} {
		if _, err := h.s.Create(context.Background(), Def{Name: "x", Type: typ, Schedule: "manual", Params: json.RawMessage(`{"blobStore":"nope"}`)}); err == nil || !strings.Contains(err.Error(), `"nope" does not exist`) {
			t.Fatalf("%s: %v", typ, err)
		}
	}
}

// fakeRebuilder records the repositories it was asked to rebuild.
type fakeRebuilder struct {
	calls []string
	err   error
}

func (f *fakeRebuilder) RebuildMetadata(_ context.Context, rp *repo.Repository, logf func(string, ...any)) error {
	f.calls = append(f.calls, rp.Name)
	logf("%s: fake rebuild", rp.Name)
	return f.err
}

func TestRebuildMetadataAllFormats(t *testing.T) {
	h := newHarness(t)
	fake := &fakeRebuilder{}
	h.s.Rebuilders = map[string]repo.MetadataRebuilder{"helm": fake}
	h.put(t, "mvn", "org/example/app/1.0/app-1.0.jar", "jar")

	out, tk := h.run(t, TypeRebuild, `{"repository":"charts"}`)
	if out.Result != ResultOK || out.Message != "charts: rebuilt the helm metadata" || tk.Params != `{"repo":"charts"}` || strings.Join(fake.calls, ",") != "charts" {
		t.Fatalf("one: %+v %s %v", out, tk.Params, fake.calls)
	}
	fake.calls = nil
	out, tk = h.run(t, TypeRebuild, `{"format":"helm"}`)
	if out.Result != ResultOK || out.Message != "rebuilt the metadata of 1 repositories (0 failed)" || strings.Join(fake.calls, ",") != "charts" {
		t.Fatalf("format: %+v %v", out, fake.calls)
	}
	fake.calls = nil
	var want int
	for _, rp := range h.reg.Snapshot().Sorted {
		if (rp.Format == "maven" && rp.Type != "group") || rp.Name == "charts" {
			want++
		}
	}
	out, tk = h.run(t, TypeRebuild, `{}`)
	if out.Result != ResultOK || out.Message != fmt.Sprintf("rebuilt the metadata of %d repositories (0 failed)", want) || strings.Join(fake.calls, ",") != "charts" ||
		!strings.Contains(tk.LastLog, "mvn: rebuilt 1 GA metadata documents") || !strings.Contains(tk.LastLog, "mvn-proxy: removed 0 cached files") || !strings.Contains(tk.LastLog, "charts: fake rebuild") {
		t.Fatalf("all: %+v %v\n%s", out, fake.calls, tk.LastLog)
	}
	fake.err = errors.New("boom")
	out, tk = h.run(t, TypeRebuild, `{"format":"helm"}`)
	if out.Result != ResultFailed || !strings.Contains(out.Message, "1 repositories failed") || !strings.Contains(tk.LastLog, "ERROR charts: boom") {
		t.Fatalf("failed: %+v\n%s", out, tk.LastLog)
	}
	if _, err := h.s.Create(context.Background(), Def{Name: "x", Type: TypeRebuild, Schedule: "manual", Params: json.RawMessage(`{"repo":"mvn","format":"helm"}`)}); err == nil || !strings.Contains(err.Error(), "is maven, not helm") {
		t.Fatalf("format mismatch: %v", err)
	}
	// A format without a rebuilder is refused.
	if _, err := h.s.Create(context.Background(), Def{Name: "x", Type: TypeRebuild, Schedule: "manual", Params: json.RawMessage(`{"repo":"crates"}`)}); err == nil || !strings.Contains(err.Error(), "hosted repository of helm, maven") {
		t.Fatalf("cargo: %v", err)
	}
}

// olderThanDays keeps the temp files changed in the last days, never fewer
// than the minimum ages (core-22).
func TestDeleteTempFilesOlderThanDays(t *testing.T) {
	h := newHarness(t)
	tmp := filepath.Join(h.dir, "tmp")
	for name, age := range map[string]time.Duration{"up-2d": 48 * time.Hour, "up-4d": 96 * time.Hour, "ses-2d": 49 * time.Hour, "ses-5d": 120 * time.Hour, "up-new": time.Minute} {
		p := filepath.Join(tmp, name)
		os.WriteFile(p, []byte("x"), 0o644)
		os.Chtimes(p, time.Now().Add(-age), time.Now().Add(-age))
	}
	for _, bad := range []string{`{"olderThanDays":-1}`, `{"olderThanDays":"2"}`, `{"olderThan":2}`} {
		if _, err := h.s.Create(context.Background(), Def{Name: "x", Type: TypeTemp, Schedule: "manual", Params: json.RawMessage(bad)}); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	out, tk := h.run(t, TypeTemp, `{"olderThanDays":3}`)
	if out.Result != ResultOK || out.Removed != 2 || tk.Params != `{"olderThanDays":3}` {
		t.Fatalf("3 days: %+v %s", out, tk.Params)
	}
	left, _ := os.ReadDir(tmp)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != "ses-2d up-2d up-new" {
		t.Fatalf("left: %v", names)
	}
	if out, _ := h.run(t, TypeTemp, `{}`); out.Removed != 2 { // the 1 hour and 24 hour minimums
		t.Fatalf("default: %+v", out)
	}
}

// resetProxyMetadata drops an apt proxy's cached dists/ files and keeps its
// packages (core-45); without it an apt proxy is refused, and "every
// repository" leaves apt proxies alone.
func TestRebuildResetsAptProxy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.storedRepos(t)
	if _, err := h.reg.CreateDef(ctx, config.Repository{Name: "apt-px", Format: "apt", Type: "proxy", Remote: "https://deb.example.org/debian"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"dists/stable/InRelease", "dists/stable/main/binary-amd64/Packages.gz", "pool/main/h/hello/hello_1.0_amd64.deb"} {
		h.put(t, "apt-px", p, "cached")
	}
	if _, err := h.s.Create(ctx, Def{Name: "x", Type: TypeRebuild, Schedule: "manual", Params: json.RawMessage(`{"repo":"apt-px"}`)}); err == nil ||
		!strings.Contains(err.Error(), "set resetProxyMetadata") {
		t.Fatalf("without the flag: %v", err)
	}
	if out, tk := h.run(t, TypeRebuild, `{"format":"maven"}`); out.Result != ResultOK || strings.Contains(tk.LastLog, "apt-px") {
		t.Fatalf("every maven repository: %+v\n%s", out, tk.LastLog)
	}
	out, tk := h.run(t, TypeRebuild, `{"repo":"apt-px","resetProxyMetadata":true}`)
	if out.Result != ResultOK || out.Removed != 2 || !strings.Contains(tk.LastLog, "apt-px: dropped 2 cached metadata files") {
		t.Fatalf("reset: %+v\n%s", out, tk.LastLog)
	}
	as, _ := h.db.ListAssets(ctx, "apt-px", "")
	if len(as) != 1 || as[0].Path != "pool/main/h/hello/hello_1.0_amd64.deb" {
		t.Fatalf("left: %+v", as)
	}
}
