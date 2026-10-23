package golang

import (
	"bytes"
	"strings"
	"testing"

	"commandcenter/internal/repo/repotest"
)

func TestEventsGoZip(t *testing.T) {
	e := newEnv(t)
	sink := &repotest.Sink{}
	e.h.Store.Events = sink
	mod := "module example.com/Hello\n\ngo 1.21\n"
	// .mod and .info uploads are parts of a version, not the module upload.
	if w := e.do("PUT", "go-all", "example.com/!hello/@v/v1.0.0.mod", strings.NewReader(mod)); w.Code != 201 {
		t.Fatalf("mod: %d %s", w.Code, w.Body.String())
	}
	if got := sink.Events(); len(got) != 0 {
		t.Fatalf("events after .mod: %v", got)
	}
	zipBytes := moduleZip(t, "example.com/Hello", "v1.0.0", mod)
	if w := e.do("PUT", "go-hosted", "example.com/!hello/@v/v1.0.0.zip", bytes.NewReader(zipBytes)); w.Code != 201 {
		t.Fatalf("zip: %d %s", w.Code, w.Body.String())
	}
	got := sink.Events()
	if len(got) != 1 || got[0] != (repotest.Event{Kind: "upload", Repo: "go-hosted", Subject: "example.com/Hello@v1.0.0"}) {
		t.Fatalf("events: %v", got)
	}
	sink.Reset()
	if w := e.do("PUT", "go-hosted", "example.com/!hello/@v/v1.0.0.zip", bytes.NewReader(zipBytes)); w.Code != 409 {
		t.Fatalf("re-upload: %d", w.Code)
	}
	if got := sink.Events(); len(got) != 0 {
		t.Fatalf("events after conflict: %v", got)
	}
}
