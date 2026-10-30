package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/tasks"
)

// contentTables are the tables a restore of the database brings back.
var contentTables = []string{"assets", "packages", "dist_tags"}

// backup copies the content tables, as a pg_dump taken now would hold
// them; restore puts that copy back, dropping what was written since.
func backup(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, tb := range contentTables {
		if _, err := db.Exec(`DROP TABLE IF EXISTS bk_` + tb + `; CREATE TABLE bk_` + tb + ` AS SELECT * FROM ` + tb); err != nil {
			t.Fatal(err)
		}
	}
}

func restore(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, tb := range contentTables {
		if _, err := db.Exec(`DELETE FROM ` + tb + `; INSERT INTO ` + tb + ` SELECT * FROM bk_` + tb); err != nil {
			t.Fatal(err)
		}
	}
}

func rawDB(t *testing.T, a *App) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", a.Config.Database.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func (e *stagingEnv) flushJournal() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.a.Store.Journal.Flush(ctx); err != nil {
		e.t.Fatal(err)
	}
}

// runTask creates a manual task of typ with params and runs it to its end.
func (e *stagingEnv) runTask(typ, params string) tasks.Outcome {
	e.t.Helper()
	task := e.json(201, "POST", "/-/api/tasks", "admin", fmt.Sprintf(`{"name":"%s","type":"%s","schedule":"manual","params":%s}`, typ, typ, params))
	out, err := e.a.Tasks.RunNow(task["id"].(string), "admin")
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// planItems returns the items of a plan as "action repository path".
func (e *stagingEnv) planItems(id, query string) []string {
	e.t.Helper()
	var out []string
	token := ""
	for {
		q := "/-/api/reconcile/plans/" + id + "/items?limit=3" + query
		if token != "" {
			q += "&continuationToken=" + token
		}
		page := e.json(200, "GET", q, "admin", "")
		for _, x := range page["items"].([]any) {
			it := x.(map[string]any)
			out = append(out, fmt.Sprint(it["action"], " ", it["repository"], " ", it["path"]))
		}
		next, _ := page["continuationToken"].(string)
		if next == "" {
			sort.Strings(out)
			return out
		}
		token = next
	}
}

// latestPlan returns the newest plan.
func (e *stagingEnv) latestPlan() map[string]any {
	e.t.Helper()
	plans := e.json(200, "GET", "/-/api/reconcile/plans", "admin", "")["items"].([]any)
	if len(plans) == 0 {
		e.t.Fatal("no plan")
	}
	return plans[0].(map[string]any)
}

// The database is restored from a backup older than the blob store: the
// raw, Maven and npm uploads since are recovered with their metadata, a
// file deleted since is deleted again, and the instance refuses uploads
// meanwhile in recovery mode.
func TestReconcileAfterRestore(t *testing.T) {
	e := newStagingEnv(t)
	ctx := t.Context()
	db := rawDB(t, e.a)

	e.want(201, "PUT", "/repository/raw-a/docs/old.txt", "ci", "old")
	e.want(201, "PUT", "/repository/raw-a/docs/gone.txt", "ci", "deleted after the backup")
	e.putMaven("maven-staging", "org.ex", "app", "1.0", "jar-1.0")
	e.want(201, "PUT", "/repository/npm-staging/demo", "ci", npmPublish("demo", "1.0.0"))
	e.flushJournal()
	backup(t, db)

	e.want(201, "PUT", "/repository/raw-a/docs/new.txt", "ci", "new after the backup")
	e.putMaven("maven-staging", "org.ex", "app", "2.0", "jar-2.0")
	e.want(201, "PUT", "/repository/npm-staging/demo", "ci", npmPublish("demo", "2.0.0"))
	e.want(204, "DELETE", "/repository/raw-a/docs/gone.txt", "ci", "")
	before, err := e.a.DB.GetAsset(ctx, "raw-a", "docs/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	e.flushJournal()

	restore(t, db)
	e.want(404, "GET", "/repository/raw-a/docs/new.txt", "ci", "")
	e.want(200, "GET", "/repository/raw-a/docs/gone.txt", "ci", "")
	e.want(404, "GET", "/repository/maven-staging/org/ex/app/2.0/app-2.0.jar", "ci", "")

	// Recovery mode: uploads and deletes answer 503 with the reason;
	// downloads, compaction and cleanup do not run.
	rec := e.json(200, "PUT", "/-/api/recovery", "admin", `{"enabled":true,"message":"restoring the backup of last night"}`)
	if rec["enabled"] != true || rec["by"] != "admin" || rec["since"] == "" || rec["journalRetentionDays"] != float64(30) {
		t.Fatalf("recovery doc %v", rec)
	}
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/repository/raw-a/docs/x.txt", "x"},
		{"DELETE", "/repository/raw-a/docs/old.txt", ""},
		{"PUT", "/repository/npm-staging/demo", npmPublish("demo", "3.0.0")},
		{"POST", "/v2/docker-a/blobs/uploads/", ""},
		{"DELETE", "/service/rest/v1/components/abc", ""},
		{"POST", "/-/api/staging/delete?repository=raw-a&name=docs/old.txt", ""},
	} {
		st, body := e.do(c.method, c.path, "ci", c.body)
		if st != 503 || !strings.Contains(body, "recovery mode") || !strings.Contains(body, "restoring the backup of last night") {
			t.Errorf("%s %s in recovery mode: %d %s", c.method, c.path, st, body)
		}
	}
	e.want(200, "GET", "/repository/raw-a/docs/old.txt", "ci", "")
	if _, _, err := e.a.GC(ctx, time.Hour); !errors.Is(err, errRecoveryGC) {
		t.Fatalf("GC in recovery mode: %v", err)
	}
	if out := e.runTask("blobstore.compact", "{}"); out.Result != tasks.ResultOK || !strings.Contains(out.Message, "skipped: recovery mode is on") {
		t.Fatalf("compact in recovery mode: %+v", out)
	}
	e.json(200, "GET", "/-/api/recovery", "admin", "")

	// The plan lists what the restore lost, and changes nothing.
	out := e.runTask(tasks.TypeReconcilePlan, `{"sinceHours":1}`)
	if out.Result != tasks.ResultOK {
		t.Fatalf("plan: %+v", out)
	}
	plan := e.latestPlan()
	id := plan["id"].(string)
	if plan["state"] != meta.PlanPlanned || plan["createdBy"] != "admin" {
		t.Fatalf("plan %v", plan)
	}
	items := e.planItems(id, "")
	for _, want := range []string{
		"ADD raw-a docs/new.txt",
		"DELETE raw-a docs/gone.txt",
		"ADD maven-staging org/ex/app/2.0/app-2.0.jar",
		"ADD maven-staging org/ex/app/2.0/app-2.0.pom",
		"ADD npm-staging demo/-/demo-2.0.0.tgz",
		"ADD npm-staging demo@2.0.0",
		"ADD npm-staging demo:latest",
	} {
		if !slices.Contains(items, want) {
			t.Errorf("plan items lack %q: %v", want, items)
		}
	}
	for _, it := range items {
		for _, unchanged := range []string{"docs/old.txt", "app-1.0.jar", "demo-1.0.0.tgz", "demo@1.0.0"} {
			if strings.HasSuffix(it, " "+unchanged) {
				t.Errorf("an unchanged row is in the plan: %s", it)
			}
		}
	}
	if del := e.planItems(id, "&action=DELETE"); len(del) != 1 {
		t.Fatalf("DELETE items %v", del)
	}
	e.want(404, "GET", "/repository/raw-a/docs/new.txt", "ci", "")

	// Executing it brings the rows back with their metadata.
	if out := e.runTask(tasks.TypeReconcileExecute, `{"planId":"`+id+`"}`); out.Result != tasks.ResultOK {
		t.Fatalf("execute: %+v", out)
	}
	if got := e.want(200, "GET", "/repository/raw-a/docs/new.txt", "ci", ""); got != "new after the backup" {
		t.Fatalf("new.txt = %q", got)
	}
	after, err := e.a.DB.GetAsset(ctx, "raw-a", "docs/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if after.BlobSHA256 != before.BlobSHA256 || after.SHA1 != before.SHA1 || after.MD5 != before.MD5 || after.Size != before.Size ||
		after.ContentType != before.ContentType || !after.CreatedAt.Equal(before.CreatedAt) || !after.LastModified.Equal(before.LastModified) {
		t.Fatalf("recovered row %+v, was %+v", after, before)
	}
	e.want(404, "GET", "/repository/raw-a/docs/gone.txt", "ci", "")
	e.want(200, "GET", "/repository/raw-a/docs/old.txt", "ci", "")
	if got := e.want(200, "GET", "/repository/maven-staging/org/ex/app/2.0/app-2.0.jar", "ci", ""); got != "jar-2.0" {
		t.Fatalf("jar = %q", got)
	}
	if md := e.want(200, "GET", "/repository/maven-staging/org/ex/app/maven-metadata.xml", "ci", ""); !strings.Contains(md, "<version>1.0</version>") ||
		!strings.Contains(md, "<version>2.0</version>") {
		t.Fatalf("maven-metadata.xml after reconcile:\n%s", md)
	}
	pk := e.json(200, "GET", "/repository/npm-staging/demo", "ci", "")
	if vs := pk["versions"].(map[string]any); vs["1.0.0"] == nil || vs["2.0.0"] == nil || pk["dist-tags"].(map[string]any)["latest"] != "2.0.0" {
		t.Fatalf("packument after reconcile: %v", pk)
	}
	e.want(200, "GET", "/repository/npm-staging/demo/-/demo-2.0.0.tgz", "ci", "")
	plan = e.json(200, "GET", "/-/api/reconcile/plans/"+id, "admin", "")
	if sum := plan["summary"].(map[string]any); plan["state"] != meta.PlanExecuted || sum["failed"] != nil || sum["done"] == nil || plan["executed"] == "" {
		t.Fatalf("executed plan %v", plan)
	}
	if left := e.planItems(id, "&state=PLANNED"); len(left) != 0 {
		t.Fatalf("items left PLANNED: %v", left)
	}

	// Executing it again changes nothing; a new plan finds nothing to do.
	if out := e.runTask(tasks.TypeReconcileExecute, `{"planId":"`+id+`"}`); out.Result != tasks.ResultOK || !strings.Contains(out.Message, "added 0, deleted 0, failed 0") {
		t.Fatalf("second execute: %+v", out)
	}
	e.runTask(tasks.TypeReconcilePlan, `{"all":true}`)
	if again := e.planItems(e.latestPlan()["id"].(string), ""); len(again) != 0 {
		t.Fatalf("a plan after the execution: %v", again)
	}

	// Off again: uploads go through.
	e.json(200, "PUT", "/-/api/recovery", "admin", `{"enabled":false}`)
	e.want(201, "PUT", "/repository/raw-a/docs/x.txt", "ci", "x")
	e.want(204, "DELETE", "/-/api/reconcile/plans/"+id, "admin", "")
	e.want(404, "GET", "/-/api/reconcile/plans/"+id, "admin", "")
	e.want(403, "GET", "/-/api/reconcile/plans", "ci", "")
	e.want(403, "PUT", "/-/api/recovery", "ci", `{"enabled":true}`)
	for _, want := range []string{"settings.update recoveryMode", "reconcile.plan " + id, "reconcile.plan.delete " + id, "upload docs/new.txt", "delete docs/gone.txt"} {
		var kinds []string
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) { // the events are written in the background
			kinds = nil
			for _, ev := range e.json(200, "GET", "/-/api/events?limit=200", "admin", "")["events"].([]any) {
				m := ev.(map[string]any)
				kinds = append(kinds, fmt.Sprint(m["kind"], " ", m["subject"]))
			}
			if slices.Contains(kinds, want) || time.Now().After(deadline) {
				break
			}
		}
		if !slices.Contains(kinds, want) {
			t.Errorf("no %q event: %v", want, kinds)
		}
	}
}

// A blob whose row a restore lost is kept by compaction while the journal
// of the last day puts it, or while a pending plan adds it; a blob deleted
// normally is not kept; the journal is pruned past its retention.
func TestReconcileGCProtection(t *testing.T) {
	e := newStagingEnv(t)
	ctx := t.Context()
	db := rawDB(t, e.a)
	e.want(201, "PUT", "/repository/raw-a/keep.txt", "ci", "kept by the journal")
	e.want(201, "PUT", "/repository/raw-a/del.txt", "ci", "deleted normally")
	keep, _ := e.a.DB.GetAsset(ctx, "raw-a", "keep.txt")
	del, _ := e.a.DB.GetAsset(ctx, "raw-a", "del.txt")
	e.want(204, "DELETE", "/repository/raw-a/del.txt", "ci", "")
	e.flushJournal()
	if _, err := db.Exec(`DELETE FROM assets WHERE repo = 'raw-a' AND path = 'keep.txt'`); err != nil { // the restore
		t.Fatal(err)
	}
	root := e.a.Config.DataDir
	ageBlob(t, root, keep.BlobSHA256)
	ageBlob(t, root, del.BlobSHA256)
	bs := e.a.Store.StoreByName(repo.DefaultStore)
	old := repo.JournalKey(time.Now().Add(-40*24*time.Hour), "aaaaaaaaaaaa")
	if err := bs.PutMeta(old, []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}

	if _, _, err := e.a.GC(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if !present(t, bs, keep.BlobSHA256) {
		t.Fatal("compaction deleted a blob the journal of the last day puts")
	}
	if present(t, bs, del.BlobSHA256) {
		t.Fatal("compaction kept a blob deleted normally")
	}
	if _, err := bs.GetMeta(old); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("a journal record past the retention is kept: %v", err)
	}

	// Without the journal, a pending plan's ADD keeps it.
	if _, err := repo.PruneJournal(bs, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	e.a.DB.CreateReconcilePlan(ctx, meta.ReconcilePlan{ID: "p", State: meta.PlanPlanned, Created: time.Now()})
	e.a.DB.AddReconcileItems(ctx, []meta.ReconcileItem{{PlanID: "p", Seq: 1, Repo: "raw-a", Kind: meta.ChangeAsset, Label: "keep.txt",
		Action: meta.ItemAdd, State: meta.ItemPlanned, BlobSHA256: keep.BlobSHA256}})
	e.a.GC(ctx, time.Hour)
	if !present(t, bs, keep.BlobSHA256) {
		t.Fatal("compaction deleted a blob a pending plan adds")
	}
	e.want(204, "DELETE", "/-/api/reconcile/plans/p", "admin", "")
	e.a.GC(ctx, time.Hour)
	if present(t, bs, keep.BlobSHA256) {
		t.Fatal("compaction keeps a blob no plan or journal record names")
	}
}

// A journal that cannot be written fails no upload: the failure is logged
// and counted.
func TestJournalFailureKeepsUploads(t *testing.T) {
	e := newStagingEnv(t)
	// The default store's meta directory cannot be made.
	if err := os.WriteFile(filepath.Join(e.a.Config.DataDir, "meta"), []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.want(201, "PUT", "/repository/raw-a/a.txt", "ci", "a")
	e.want(201, "PUT", "/repository/npm-staging/demo", "ci", npmPublish("demo", "1.0.0"))
	e.flushJournal()
	if st := e.a.Store.Journal.Stats(); st.Failed < 2 || st.Written != 0 {
		t.Fatalf("journal stats %+v", st)
	}
	e.want(200, "GET", "/repository/raw-a/a.txt", "ci", "")
	if doc := e.json(200, "GET", "/-/api/recovery", "admin", ""); doc["journal"].(map[string]any)["failed"].(float64) < 2 {
		t.Fatalf("recovery doc %v", doc)
	}
	prom := e.want(200, "GET", "/-/metrics/prometheus", "admin", "")
	if !strings.Contains(prom, `commandcenter_reconcile_journal_writes_total{result="failed"}`) || !strings.Contains(prom, "commandcenter_recovery_mode 0") {
		t.Fatalf("metrics lack the journal counters:\n%s", prom)
	}
}

// What the journal costs an upload. Run with COMMANDCENTER_JOURNAL_BENCH=1
// (no race detector): raw uploads with the journal and without it.
func TestJournalUploadLatency(t *testing.T) {
	if os.Getenv("COMMANDCENTER_JOURNAL_BENCH") == "" {
		t.Skip("set COMMANDCENTER_JOURNAL_BENCH=1 to measure")
	}
	const n = 400
	measure := func(journal bool) []time.Duration {
		e := newStagingEnv(t)
		if !journal {
			e.a.DB.OnCommit = nil
		}
		for i := range 20 { // warm up
			e.want(201, "PUT", fmt.Sprintf("/repository/raw-a/warm/%d", i), "ci", "w")
		}
		out := make([]time.Duration, n)
		body := strings.Repeat("x", 4096)
		for i := range n {
			start := time.Now()
			e.want(201, "PUT", fmt.Sprintf("/repository/raw-a/f/%d.bin", i), "ci", body)
			out[i] = time.Since(start)
		}
		e.flushJournal()
		slices.Sort(out)
		return out
	}
	stats := func(d []time.Duration) string {
		var sum time.Duration
		for _, x := range d {
			sum += x
		}
		return fmt.Sprintf("mean %v p50 %v p95 %v p99 %v", sum/time.Duration(len(d)), d[len(d)/2], d[len(d)*95/100], d[len(d)*99/100])
	}
	without, with := measure(false), measure(true)
	without2, with2 := measure(false), measure(true)
	t.Logf("%d raw PUTs of 4 KiB, file store, PostgreSQL:", n)
	t.Logf("  without journal: %s | %s", stats(without), stats(without2))
	t.Logf("  with journal:    %s | %s", stats(with), stats(with2))
}
