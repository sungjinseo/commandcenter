package cargo

import (
	"bytes"
	"testing"

	"commandcenter/internal/repo/repotest"
)

func TestEventsCargoPublish(t *testing.T) {
	e := newEnv(t)
	sink := &repotest.Sink{}
	e.h.Store.Events = sink
	body := publishCrate(t, helloMeta, []byte("hello crate"))
	if w := e.do("PUT", "cargo-all", "api/v1/crates/new", bytes.NewReader(body)); w.Code != 200 {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	got := sink.Events()
	if len(got) != 1 || got[0] != (repotest.Event{Kind: "upload", Repo: "cargo-hosted", Subject: "smoke-hello@1.0.0"}) {
		t.Fatalf("events: %v", got)
	}
	sink.Reset()
	if w := e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(body)); w.Code != 409 {
		t.Fatalf("republish: %d", w.Code)
	}
	if got := sink.Events(); len(got) != 0 {
		t.Fatalf("events after conflict: %v", got)
	}
	// The subject keeps the published spelling of a mixed-case name (QA L-20).
	mixed := publishCrate(t, `{"name":"Foo-Bar","vers":"1.0.0","deps":[],"features":{}}`, []byte("x"))
	if w := e.do("PUT", "cargo-hosted", "api/v1/crates/new", bytes.NewReader(mixed)); w.Code != 200 {
		t.Fatalf("publish Foo-Bar: %d %s", w.Code, w.Body.String())
	}
	if got := sink.Events(); len(got) != 1 || got[0].Subject != "Foo-Bar@1.0.0" {
		t.Fatalf("events: %v", got)
	}
}
