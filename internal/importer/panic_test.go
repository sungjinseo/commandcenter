package importer

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"commandcenter/internal/importer/nexus"
	"commandcenter/internal/importer/nexus/nexustest"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// A panicking ingester fails its asset only: the job goes on, the asset is
// recorded as failed with "internal error", and the details go to the
// server log (redacted) with a stack.
func TestIngestPanicFailsOnlyThatAsset(t *testing.T) {
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var logs bytes.Buffer
	src := nexustest.New(t, "u", "pw")
	e := &Engine{DB: db, Repos: repo.NewRegistry(db, nil), Log: slog.New(slog.NewTextHandler(&logs, nil))}
	e.ingestOverride = func(ctx context.Context, rp *repo.Repository, a nexus.Asset, in io.Reader) (repo.IngestResult, error) {
		io.Copy(io.Discard, in)
		if strings.HasSuffix(a.Path, ".jar") {
			panic("boom at http://user:secret@internal.example/x")
		}
		return repo.Ingested, nil
	}
	p, errs := e.Normalize(Params{URL: src.URL, Username: "u", Repositories: []string{"mvn-h"}, ImportDefinitions: true, ImportContent: true, Job: "p"})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	rep, err := e.Run(context.Background(), p, Credentials{Password: "pw"}, "admin", nil)
	if err == nil || rep.Status != StatusFailed {
		t.Fatalf("%v %+v", err, rep)
	}
	rr := rep.Repositories[0]
	if rr.Counts.Imported != 1 || rr.Counts.Failed != 1 || len(rr.Errors) != 1 || !strings.Contains(rr.Errors[0], "internal error") {
		t.Fatalf("%+v", rr)
	}
	st, _ := db.ListImportState(context.Background(), "p", 10)
	if len(st) != 2 || st[0].Status != meta.ImportFailed || !strings.Contains(st[0].Error, "internal error") || st[1].Status != meta.ImportDone {
		t.Fatalf("states %+v", st)
	}
	l := logs.String()
	if !strings.Contains(l, "panic while importing") || !strings.Contains(l, "goroutine") || strings.Contains(l, "secret") {
		t.Fatalf("log: %s", l)
	}
}
