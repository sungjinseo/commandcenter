package maven

import (
	"testing"

	"commandcenter/internal/repo/repotest"
)

func TestEventsMaven(t *testing.T) {
	e := newEnv(t)
	sink := &repotest.Sink{}
	e.h.Store.Events = sink
	const jar = "org/example/app/1.0/app-1.0.jar"

	// Main artifacts emit upload (via the group, the event names the hosted target).
	if w := e.do("PUT", "public", jar, "jar"); w.Code != 201 {
		t.Fatalf("PUT: %d", w.Code)
	}
	if w := e.do("PUT", "releases", "org/example/app/1.0/app-1.0.pom", "<project/>"); w.Code != 201 {
		t.Fatalf("PUT pom: %d", w.Code)
	}
	got := sink.Events()
	if len(got) != 2 || got[0] != (repotest.Event{Kind: "upload", Repo: "releases", Subject: jar}) {
		t.Fatalf("after PUTs: %v", got)
	}

	// Sidecars, signatures, maven-metadata and failed or rejected writes emit nothing.
	sink.Reset()
	e.do("PUT", "releases", jar+".sha1", "ignored")
	e.do("PUT", "releases", jar+".md5", "ignored")
	e.do("PUT", "releases", jar+".asc", "sig")
	e.do("PUT", "releases", "org/example/app/maven-metadata.xml", `<metadata><groupId>org.example</groupId><artifactId>app</artifactId></metadata>`)
	if w := e.do("PUT", "releases", jar, "again"); w.Code != 400 { // allow_once
		t.Fatalf("redeploy: %d", w.Code)
	}
	if got := sink.Events(); len(got) != 0 {
		t.Fatalf("unexpected events: %v", got)
	}

	// DELETE of an asset emits delete; checksum and metadata deletes and 404s do not.
	if w := e.do("DELETE", "releases", jar+".sha1", ""); w.Code != 204 {
		t.Fatalf("DELETE sha1: %d", w.Code)
	}
	if w := e.do("DELETE", "releases", "org/example/app/9.9/app-9.9.jar", ""); w.Code != 404 {
		t.Fatalf("DELETE missing: %d", w.Code)
	}
	if got := sink.Events(); len(got) != 0 {
		t.Fatalf("unexpected events: %v", got)
	}
	if w := e.do("DELETE", "releases", jar, ""); w.Code != 204 {
		t.Fatalf("DELETE: %d", w.Code)
	}
	got = sink.Events()
	if len(got) != 1 || got[0] != (repotest.Event{Kind: "delete", Repo: "releases", Subject: jar}) {
		t.Fatalf("after DELETE: %v", got)
	}
}
