package npm

import (
	"testing"

	"commandcenter/internal/repo/repotest"
)

func TestEventsNpmPublish(t *testing.T) {
	e := newEnv(t)
	sink := &repotest.Sink{}
	e.h.Store.Events = sink
	if w := e.do("PUT", "npm-all", "@s/hello", publishBody("@s/hello", "1.0.0", []byte("tgz"), false), ci); w.Code != 201 {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	got := sink.Events()
	if len(got) != 1 || got[0] != (repotest.Event{Kind: "upload", Actor: "ci", Repo: "npm-hosted", Subject: "@s/hello@1.0.0"}) {
		t.Fatalf("events: %v", got)
	}
	// A rejected publish emits nothing.
	sink.Reset()
	if w := e.do("PUT", "npm-hosted", "@s/hello", publishBody("@s/hello", "1.0.0", []byte("other"), false), ci); w.Code != 403 {
		t.Fatalf("republish: %d", w.Code)
	}
	if got := sink.Events(); len(got) != 0 {
		t.Fatalf("events after rejected publish: %v", got)
	}
}
