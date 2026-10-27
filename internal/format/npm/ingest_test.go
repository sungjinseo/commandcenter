package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// tgzOf builds an npm tarball with the given entries (path -> content).
func tgzOf(t *testing.T, files map[string]string, order ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if len(order) == 0 {
		for k := range files {
			order = append(order, k)
		}
	}
	for _, k := range order {
		if err := tw.WriteHeader(&tar.Header{Name: k, Mode: 0o644, Size: int64(len(files[k])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(files[k]))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func pkgTgz(t *testing.T, name, version, extra string) []byte {
	pj := `{"name":"` + name + `","version":"` + version + `","description":"d"}`
	return tgzOf(t, map[string]string{"package/package.json": pj, "package/index.js": "x" + extra})
}

func TestIngestFreshIdenticalAndConflict(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["npm-hosted"]
	tgz := pkgTgz(t, "@s/hello", "1.0.0", "")

	v, res, err := e.h.Ingest(ctx, rp, "@s/hello", "1.0.0", bytes.NewReader(tgz), "system")
	if err != nil || v != "1.0.0" || res != repo.Ingested {
		t.Fatalf("fresh: %q %q %v", v, res, err)
	}
	if _, err := e.h.ApplyTags(ctx, rp, "@s/hello", map[string]string{"latest": "1.0.0"}, nil, "system"); err != nil {
		t.Fatal(err)
	}
	m := jsonBody(t, e.do("GET", "npm-hosted", "@s/hello", "", anon))
	if get(m, "versions", "1.0.0", "dist", "tarball") != base+"/repository/npm-hosted/@s/hello/-/hello-1.0.0.tgz" || get(m, "versions", "1.0.0", "dist", "shasum") == nil {
		t.Fatalf("packument = %v", m)
	}
	if w := e.do("GET", "npm-hosted", "@s/hello/-/hello-1.0.0.tgz", "", anon); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), tgz) {
		t.Fatalf("tarball: %d", w.Code)
	}

	v, res, err = e.h.Ingest(ctx, rp, "@s/hello", "1.0.0", bytes.NewReader(tgz), "system")
	if err != nil || v != "1.0.0" || res != repo.Skipped {
		t.Fatalf("identical: %q %q %v", v, res, err)
	}
	_, _, err = e.h.Ingest(ctx, rp, "@s/hello", "1.0.0", bytes.NewReader(pkgTgz(t, "@s/hello", "1.0.0", "different")), "system")
	if err == nil || !repo.IsIngestConflict(err) {
		t.Fatalf("different content: %v", err)
	}
}

func TestIngestRefusesOversizedPackageJSON(t *testing.T) {
	e := newEnv(t)
	old := maxPackageJSON
	maxPackageJSON = 64
	t.Cleanup(func() { maxPackageJSON = old })
	pj := `{"name":"big","version":"1.0.0","description":"` + strings.Repeat("x", 200) + `"}`
	tgz := tgzOf(t, map[string]string{"package/package.json": pj})
	_, _, err := e.h.Ingest(context.Background(), e.repos["npm-hosted"], "big", "1.0.0", bytes.NewReader(tgz), "system")
	if err == nil || !strings.Contains(err.Error(), "larger than 64 bytes") {
		t.Fatalf("err = %v", err)
	}
	if pk, _ := e.h.Store.DB.ListPackages(context.Background(), "npm-hosted", "big"); len(pk) != 0 {
		t.Fatal("row stored")
	}
}

func TestIngestRefusesBadTarball(t *testing.T) {
	e := newEnv(t)
	rp := e.repos["npm-hosted"]
	for name, data := range map[string][]byte{
		"not gzip":    []byte("plain"),
		"no manifest": tgzOf(t, map[string]string{"package/index.js": "x"}),
		"bad version": tgzOf(t, map[string]string{"package/package.json": `{"name":"p","version":"1.x"}`}),
	} {
		if _, _, err := e.h.Ingest(context.Background(), rp, "p", "1.0.0", bytes.NewReader(data), "system"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestIngestEmitsUploadEvent(t *testing.T) {
	e := newEnv(t)
	sink := &recSink{}
	e.h.Store.Events = sink
	rp := e.repos["npm-hosted"]
	tgz := pkgTgz(t, "ev", "1.0.0", "")
	e.h.Ingest(context.Background(), rp, "ev", "1.0.0", bytes.NewReader(tgz), "system")
	e.h.Ingest(context.Background(), rp, "ev", "1.0.0", bytes.NewReader(tgz), "system")
	if len(sink.got) != 1 || sink.got[0] != "upload|system|npm-hosted|ev@1.0.0" {
		t.Fatalf("events = %v", sink.got)
	}
}

type recSink struct{ got []string }

func (s *recSink) Emit(kind, actor, repoName, subject, detail string) {
	s.got = append(s.got, kind+"|"+actor+"|"+repoName+"|"+subject)
}

func TestApplyTagsOnlyExistingVersions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["npm-hosted"]
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if _, _, err := e.h.Ingest(ctx, rp, "tagged", v, bytes.NewReader(pkgTgz(t, "tagged", v, "")), "system"); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.h.ApplyTags(ctx, rp, "tagged",
		map[string]string{"latest": "2.0.0", "beta": "3.0.0-beta.1", "old": "1.0.0"},
		map[string]string{"1.0.0": "use 2", "9.9.9": "ghost"}, "system")
	if err != nil {
		t.Fatal(err)
	}
	if res.Tags != 2 || res.Deprecated != 1 || len(res.Skipped) != 2 {
		t.Fatalf("result = %+v", res)
	}
	m := jsonBody(t, e.do("GET", "npm-hosted", "tagged", "", anon))
	tags, _ := json.Marshal(m["dist-tags"])
	if string(tags) != `{"latest":"2.0.0","old":"1.0.0"}` {
		t.Fatalf("dist-tags = %s", tags)
	}
	if get(m, "versions", "1.0.0", "deprecated") != "use 2" || get(m, "versions", "2.0.0", "deprecated") != nil || get(m, "versions", "9.9.9") != nil {
		t.Fatalf("versions = %v", m["versions"])
	}
	if _, err := e.h.ApplyTags(ctx, rp, "missing", nil, nil, "system"); err == nil {
		t.Fatal("tags on a missing package accepted")
	}
	// No latest from the source: the newest release is tagged.
	e.h.Ingest(ctx, rp, "nolatest", "1.0.0", bytes.NewReader(pkgTgz(t, "nolatest", "1.0.0", "")), "system")
	e.h.Ingest(ctx, rp, "nolatest", "1.2.0", bytes.NewReader(pkgTgz(t, "nolatest", "1.2.0", "")), "system")
	e.h.ApplyTags(ctx, rp, "nolatest", nil, nil, "system")
	m = jsonBody(t, e.do("GET", "npm-hosted", "nolatest", "", anon))
	if get(m, "dist-tags", "latest") != "1.2.0" {
		t.Fatalf("latest = %v", m["dist-tags"])
	}
}

func TestIngestRefusesNameOrVersionMismatch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["npm-hosted"]
	tgz := pkgTgz(t, "evil", "1.0.0", "")
	if _, _, err := e.h.Ingest(ctx, rp, "left-pad", "1.0.0", bytes.NewReader(tgz), "system"); err == nil || !strings.Contains(err.Error(), "left-pad") {
		t.Fatalf("name mismatch accepted: %v", err)
	}
	if _, _, err := e.h.Ingest(ctx, rp, "evil", "2.0.0", bytes.NewReader(tgz), "system"); err == nil || !strings.Contains(err.Error(), "2.0.0") {
		t.Fatalf("version mismatch accepted: %v", err)
	}
	if w := e.do("GET", "npm-hosted", "evil", "", anon); w.Code != 404 {
		t.Fatalf("a refused tarball left a package: %d", w.Code)
	}
	if _, res, err := e.h.Ingest(ctx, rp, "evil", "1.0.0", bytes.NewReader(tgz), "system"); err != nil || res != repo.Ingested {
		t.Fatalf("matching: %q %v", res, err)
	}
}

func TestApplyTagsKeepsTargetValues(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["npm-hosted"]
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if _, _, err := e.h.Ingest(ctx, rp, "kept", v, bytes.NewReader(pkgTgz(t, "kept", v, "")), "system"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.h.ApplyTags(ctx, rp, "kept", map[string]string{"latest": "1.0.0"}, map[string]string{"1.0.0": "first"}, "system"); err != nil {
		t.Fatal(err)
	}
	// The target moves latest and changes a deprecation; a re-run keeps them.
	if err := e.h.Store.Tx(ctx, rp, func(tx *meta.Tx) error {
		return tx.SetDistTag(ctx, rp.Name, "kept", "latest", "2.0.0")
	}); err != nil {
		t.Fatal(err)
	}
	res, err := e.h.ApplyTags(ctx, rp, "kept", map[string]string{"latest": "1.0.0", "next": "2.0.0", "same": "2.0.0"}, map[string]string{"1.0.0": "other", "2.0.0": "new"}, "system")
	if err != nil {
		t.Fatal(err)
	}
	if res.Tags != 2 || res.Deprecated != 1 || len(res.Kept) != 2 {
		t.Fatalf("result = %+v", res)
	}
	m := jsonBody(t, e.do("GET", "npm-hosted", "kept", "", anon))
	tags, _ := json.Marshal(m["dist-tags"])
	if string(tags) != `{"latest":"2.0.0","next":"2.0.0","same":"2.0.0"}` {
		t.Fatalf("dist-tags = %s", tags)
	}
	if get(m, "versions", "1.0.0", "deprecated") != "first" || get(m, "versions", "2.0.0", "deprecated") != "new" {
		t.Fatalf("versions = %v", m["versions"])
	}
	// Same values again: nothing is changed, nothing reported.
	res, err = e.h.ApplyTags(ctx, rp, "kept", map[string]string{"next": "2.0.0"}, map[string]string{"1.0.0": "first"}, "system")
	if err != nil || len(res.Kept) != 0 {
		t.Fatalf("same values: %+v %v", res, err)
	}
}

// I1: the import says which package and version a tarball holds (the source's
// npm attributes or its path); without them, or for a version npm cannot
// tell apart from another (build metadata), the tarball is refused before
// anything is recorded.
func TestIngestNeedsExpectedNameAndVersion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["npm-hosted"]
	for _, c := range []struct{ name, version, want string }{
		{"", "1.0.0", "which package"},
		{"plain", "", "which package"},
	} {
		_, _, err := e.h.Ingest(ctx, rp, c.name, c.version, bytes.NewReader(pkgTgz(t, "plain", "1.0.0", "")), "system")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q %q: err = %v", c.name, c.version, err)
		}
	}
	_, _, err := e.h.Ingest(ctx, rp, "plain", "1.0.0+b", bytes.NewReader(pkgTgz(t, "plain", "1.0.0+b", "")), "system")
	if err == nil || !strings.Contains(err.Error(), "build metadata") {
		t.Errorf("build metadata: err = %v", err)
	}
	if pk, _ := e.h.Store.DB.ListPackages(ctx, "npm-hosted", "plain"); len(pk) != 0 {
		t.Fatalf("rows stored: %+v", pk)
	}
	// package.json may spell the version as npm publish cleans it.
	tgz := tgzOf(t, map[string]string{"package/package.json": `{"name":"plain","version":"v1.0.0+ci.5"}`})
	v, res, err := e.h.Ingest(ctx, rp, "plain", "1.0.0", bytes.NewReader(tgz), "system")
	if err != nil || v != "1.0.0" || res != repo.Ingested {
		t.Fatalf("cleaned version: %q %q %v", v, res, err)
	}
	m := jsonBody(t, e.do("GET", "npm-hosted", "plain", "", anon))
	if get(m, "versions", "1.0.0", "version") != "1.0.0" {
		t.Fatalf("stored manifest: %v", m["versions"])
	}
}
