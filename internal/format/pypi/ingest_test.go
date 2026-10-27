package pypi

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"commandcenter/internal/repo"
)

type recSink struct{ got []string }

func (s *recSink) Emit(kind, actor, repoName, subject, detail string) {
	s.got = append(s.got, kind+"|"+actor+"|"+repoName+"|"+subject)
}

func wheelOf(t *testing.T, name, version, extra string) []byte {
	meta := "Metadata-Version: 2.1\nName: " + name + "\nVersion: " + version + "\nSummary: s\nRequires-Python: >=3.8\n\nbody" + extra
	return wheelBytes(t, map[string]string{
		strings.ReplaceAll(name, "-", "_") + "-" + version + ".dist-info/METADATA": meta,
	})
}

func TestIngestFreshIdenticalAndConflict(t *testing.T) {
	e := newEnv(t)
	sink := &recSink{}
	e.h.Store.Events = sink
	ctx := context.Background()
	rp := e.repos["py-ro"] // write policy deny does not stop an import
	whl := wheelOf(t, "Smoke-Hello", "1.0.0", "")
	file := "smoke_hello-1.0.0-py3-none-any.whl"

	res, err := e.h.Ingest(ctx, rp, file, bytes.NewReader(whl), sha(string(whl)), "system")
	if err != nil || res != repo.Ingested {
		t.Fatalf("fresh: %q %v", res, err)
	}
	w := e.do("GET", "py-ro", "simple/smoke-hello/", "")
	if !strings.Contains(w.Body.String(), "/packages/smoke-hello/"+file+"#sha256="+sha(string(whl))) ||
		!strings.Contains(w.Body.String(), `data-requires-python="&gt;=3.8"`) {
		t.Fatalf("page: %s", w.Body.String())
	}
	if w := e.do("GET", "py-ro", "packages/smoke-hello/"+file, ""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), whl) {
		t.Fatalf("download: %d", w.Code)
	}

	res, err = e.h.Ingest(ctx, rp, file, bytes.NewReader(whl), "", "system")
	if err != nil || res != repo.Skipped {
		t.Fatalf("identical: %q %v", res, err)
	}
	other := wheelOf(t, "Smoke-Hello", "1.0.0", "different")
	_, err = e.h.Ingest(ctx, rp, file, bytes.NewReader(other), "", "system")
	if err == nil || !repo.IsIngestConflict(err) {
		t.Fatalf("different content: %v", err)
	}
	if len(sink.got) != 1 || sink.got[0] != "upload|system|py-ro|"+file {
		t.Fatalf("events = %v", sink.got)
	}
}

func TestIngestSdistAndFileWithoutMetadata(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["py-hosted"]
	sd := sdistBytes(t, map[string]string{"pkg-2.0/PKG-INFO": "Name: Pkg\nVersion: 2.0\nRequires-Python: >=3.9\n\n"})
	if res, err := e.h.Ingest(ctx, rp, "pkg-2.0.tar.gz", bytes.NewReader(sd), "", "system"); err != nil || res != repo.Ingested {
		t.Fatalf("sdist: %q %v", res, err)
	}
	// A file without readable metadata is refused, as an upload refuses it.
	if _, err := e.h.Ingest(ctx, rp, "old_pkg-1.0.tar.gz", strings.NewReader("not an archive"), "", "system"); err == nil {
		t.Fatal("a file without metadata was accepted")
	}
	if pk, _ := e.h.Store.DB.ListPackages(ctx, "py-hosted", "old-pkg"); len(pk) != 0 {
		t.Fatal("row stored by a refused ingest")
	}
}

func TestIngestRefuses(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["py-hosted"]
	whl := wheelOf(t, "x", "1.0", "")
	if _, err := e.h.Ingest(ctx, rp, "x-1.0-py3-none-any.whl", bytes.NewReader(whl), sha("other"), "system"); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("sha mismatch: %v", err)
	}
	if _, err := e.h.Ingest(ctx, rp, "x-1.0.egg", bytes.NewReader(whl), "", "system"); err == nil {
		t.Fatal("unsupported type accepted")
	}
	if _, err := e.h.Ingest(ctx, rp, "../x-1.0.whl", bytes.NewReader(whl), "", "system"); err == nil {
		t.Fatal("bad file name accepted")
	}
	if _, err := e.h.Ingest(ctx, rp, "y-1.0-py3-none-any.whl", bytes.NewReader(whl), "", "system"); err == nil {
		t.Fatal("file of another project accepted")
	}
	mism := wheelOf(t, "x", "2.0", "")
	if _, err := e.h.Ingest(ctx, rp, "x-1.0-py3-none-any.whl", bytes.NewReader(mism), "", "system"); err == nil {
		t.Fatal("metadata version mismatch accepted")
	}
	if pk, _ := e.h.Store.DB.ListPackages(ctx, "py-hosted", "x"); len(pk) != 0 {
		t.Fatal("row stored by a refused ingest")
	}
}

func TestIngestRefusesOversizedMetadata(t *testing.T) {
	e := newEnv(t)
	huge := "Name: big\nVersion: 1.0\nX-Pad: " + strings.Repeat("a", maxMetadata+10) + "\n\n"
	whl := wheelBytes(t, map[string]string{"big-1.0.dist-info/METADATA": huge})
	_, err := e.h.Ingest(context.Background(), e.repos["py-hosted"], "big-1.0-py3-none-any.whl", bytes.NewReader(whl), "", "system")
	if err == nil || !strings.Contains(err.Error(), "metadata is larger") {
		t.Fatalf("wheel: %v", err)
	}
	sd := sdistBytes(t, map[string]string{"big-1.0/PKG-INFO": huge})
	_, err = e.h.Ingest(context.Background(), e.repos["py-hosted"], "big-1.0.tar.gz", bytes.NewReader(sd), "", "system")
	if err == nil || !strings.Contains(err.Error(), "metadata is larger") {
		t.Fatalf("sdist: %v", err)
	}
}
