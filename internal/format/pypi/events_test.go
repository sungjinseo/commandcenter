package pypi

import (
	"testing"

	"commandcenter/internal/repo/repotest"
)

func TestEventsPypiUpload(t *testing.T) {
	e := newEnv(t)
	sink := &repotest.Sink{}
	e.h.Store.Events = sink
	ct, body := uploadBody(t, goodFields("sdist"), "smoke_hello-1.0.0.tar.gz", "sdist")
	if w := e.upload("py-all", ct, body); w.Code != 200 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	got := sink.Events()
	if len(got) != 1 || got[0] != (repotest.Event{Kind: "upload", Repo: "py-hosted", Subject: "smoke_hello-1.0.0.tar.gz"}) {
		t.Fatalf("events: %v", got)
	}
	sink.Reset()
	ct, body = uploadBody(t, goodFields("sdist"), "smoke_hello-1.0.0.tar.gz", "sdist")
	if w := e.upload("py-hosted", ct, body); w.Code != 400 {
		t.Fatalf("duplicate: %d", w.Code)
	}
	if got := sink.Events(); len(got) != 0 {
		t.Fatalf("events after duplicate: %v", got)
	}
}
