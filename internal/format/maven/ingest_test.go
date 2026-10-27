package maven

import (
	"context"
	"strings"
	"testing"

	"commandcenter/internal/repo"
)

type recSink struct{ got []string }

func (s *recSink) Emit(kind, actor, repoName, subject, detail string) {
	s.got = append(s.got, kind+"|"+actor+"|"+repoName+"|"+subject)
}

func TestIngestFreshIdenticalAndConflict(t *testing.T) {
	e := newEnv(t)
	sink := &recSink{}
	e.h.Store.Events = sink
	ctx := context.Background()
	rp := e.repos["releases"] // allow_once
	const jar = "org/example/app/1.0/app-1.0.jar"

	res, err := e.h.Ingest(ctx, rp, jar, strings.NewReader("jar-bytes"), "system")
	if err != nil || res != repo.Ingested {
		t.Fatalf("fresh: %q %v", res, err)
	}
	if w := e.do("GET", "releases", jar, ""); w.Code != 200 || w.Body.String() != "jar-bytes" {
		t.Fatalf("get: %d %q", w.Code, w.Body)
	}
	// An identical re-ingest is skipped even though allow_once refuses a PUT.
	res, err = e.h.Ingest(ctx, rp, jar, strings.NewReader("jar-bytes"), "system")
	if err != nil || res != repo.Skipped {
		t.Fatalf("identical: %q %v", res, err)
	}
	_, err = e.h.Ingest(ctx, rp, jar, strings.NewReader("other"), "system")
	if err == nil || !repo.IsIngestConflict(err) {
		t.Fatalf("different content: %v", err)
	}
	if w := e.do("GET", "releases", jar, ""); w.Body.String() != "jar-bytes" {
		t.Fatalf("overwritten: %q", w.Body)
	}
	if len(sink.got) != 1 || sink.got[0] != "upload|system|releases|"+jar {
		t.Fatalf("events = %v", sink.got)
	}
}

func TestIngestSkipsGeneratedFilesAndChecksVersionPolicy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["releases"]
	for _, p := range []string{
		"org/example/app/1.0/app-1.0.jar.sha1", "org/example/app/1.0/app-1.0.pom.md5",
		"org/example/app/1.0/app-1.0.pom.sha256", "org/example/app/1.0/app-1.0.pom.sha512",
		"org/example/app/maven-metadata.xml", "org/example/app/maven-metadata.xml.sha1",
		"org/example/app/1.0-SNAPSHOT/maven-metadata.xml",
	} {
		res, err := e.h.Ingest(ctx, rp, p, strings.NewReader("generated"), "system")
		if err != nil || res != repo.Skipped {
			t.Errorf("%s: %q %v", p, res, err)
		}
		if _, err := e.h.Store.Get(ctx, "releases", p); err == nil {
			t.Errorf("%s was stored", p)
		}
	}
	if _, err := e.h.Ingest(ctx, rp, "org/example/app/1.0-SNAPSHOT/app-1.0-SNAPSHOT.jar", strings.NewReader("x"), "system"); err == nil {
		t.Error("snapshot in a release repository accepted")
	}
	if _, err := e.h.Ingest(ctx, rp, "not/a/file.txt", strings.NewReader("x"), "system"); err == nil {
		t.Error("non-artifact path accepted")
	}
	if _, err := e.h.Ingest(ctx, rp, "../x/app-1.0.jar", strings.NewReader("x"), "system"); err == nil {
		t.Error("unsafe path accepted")
	}
}

func TestRebuildGAAfterIngest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rel := e.repos["releases"]
	for _, v := range []string{"1.0", "1.1"} {
		for _, ext := range []string{"jar", "pom"} {
			p := "org/example/app/" + v + "/app-" + v + "." + ext
			if _, err := e.h.Ingest(ctx, rel, p, strings.NewReader(v+ext), "system"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if w := e.do("GET", "releases", "org/example/app/maven-metadata.xml", ""); w.Code != 404 {
		t.Fatalf("metadata before rebuild: %d", w.Code)
	}
	if err := e.h.RebuildGA(ctx, rel, "org/example/app"); err != nil {
		t.Fatal(err)
	}
	w := e.do("GET", "releases", "org/example/app/maven-metadata.xml", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<version>1.0</version>") || !strings.Contains(w.Body.String(), "<latest>1.1</latest>") {
		t.Fatalf("metadata: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "releases", "org/example/app/maven-metadata.xml.sha1", ""); w.Code != 200 {
		t.Fatalf("metadata checksum: %d", w.Code)
	}

	snap := e.repos["snapshots"]
	sp := "org/example/app/2.0-SNAPSHOT/app-2.0-20260101.120000-1.jar"
	if _, err := e.h.Ingest(ctx, snap, sp, strings.NewReader("s"), "system"); err != nil {
		t.Fatal(err)
	}
	if err := e.h.RebuildGA(ctx, snap, "org/example/app"); err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", "snapshots", "org/example/app/2.0-SNAPSHOT/maven-metadata.xml", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "20260101.120000") {
		t.Fatalf("snapshot metadata: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "snapshots", "org/example/app/maven-metadata.xml", ""); w.Code != 200 {
		t.Fatalf("snapshot GA metadata: %d", w.Code)
	}
	if err := e.h.RebuildGA(ctx, rel, "app"); err == nil {
		t.Fatal("bad GA directory accepted")
	}
}
