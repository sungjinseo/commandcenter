package cargo

import (
	"context"
	"errors"
	"testing"
	"time"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo/repotest"
)

// TestDeleteComponentCargo: a hosted crate version can be deleted (it was
// refused before: crates were treated as immutable). The index, hosted and
// through the group, stops listing it, its ETag changes, the download 404s,
// and other versions and crates stay.
func TestDeleteComponentCargo(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.seedHosted(t, "serde", "1.0.0", "crate-1.0.0")
	e.seedHosted(t, "serde", "1.1.0", "crate-1.1.0")
	e.seedHosted(t, "serde_json", "1.0.0", "json")
	etag := e.get("cargo-hosted", IndexPath("serde"), nil).Header().Get("ETag")
	sink := &repotest.Sink{}
	e.h.Store.Events = sink

	c := component.Component{Format: "cargo", Name: "serde", Version: "1.0.0"}
	res, err := e.h.DeleteComponent(ctx, e.repos["cargo-hosted"], c, "admin")
	if err != nil || res != (component.Result{Assets: 1, Bytes: int64(len("crate-1.0.0"))}) {
		t.Fatalf("result = %+v %v", res, err)
	}
	for _, repoName := range []string{"cargo-hosted", "cargo-all"} {
		w := e.get(repoName, IndexPath("serde"), map[string]string{"If-None-Match": etag})
		if l := lines(w.Body.String()); w.Code != 200 || len(l) != 1 || l[0]["vers"] != "1.1.0" {
			t.Fatalf("%s index: %d %s", repoName, w.Code, w.Body.String())
		}
		if w := e.do("GET", repoName, "crates/serde/1.0.0/download", nil); w.Code != 404 {
			t.Fatalf("%s: deleted crate download: %d", repoName, w.Code)
		}
		if w := e.do("GET", repoName, "crates/serde/1.1.0/download", nil); w.Code != 200 || w.Body.String() != "crate-1.1.0" {
			t.Fatalf("%s: kept crate download: %d", repoName, w.Code)
		}
	}
	if w := e.get("cargo-hosted", IndexPath("serde_json"), nil); len(lines(w.Body.String())) != 1 {
		t.Fatalf("other crate: %s", w.Body.String())
	}
	want := repotest.Event{Kind: "delete", Actor: "admin", Repo: "cargo-hosted", Subject: "serde@1.0.0", Detail: "assets=1"}
	if got := sink.Events(); len(got) != 1 || got[0] != want {
		t.Fatalf("events: %v", got)
	}
	// Gone already: no error, no event.
	if res, err := e.h.DeleteComponent(ctx, e.repos["cargo-hosted"], c, "admin"); err != nil || res.Assets != 0 || len(sink.Events()) != 1 {
		t.Fatalf("second delete: %+v %v %v", res, err, sink.Events())
	}
	// The last version: the crate is gone from the index.
	c.Version = "1.1.0"
	if _, err := e.h.DeleteComponent(ctx, e.repos["cargo-hosted"], c, "admin"); err != nil {
		t.Fatal(err)
	}
	if w := e.get("cargo-hosted", IndexPath("serde"), nil); w.Code != 404 {
		t.Fatalf("index after the last version: %d %s", w.Code, w.Body.String())
	}
}

func TestDeleteComponentCargoRefusesOtherRepos(t *testing.T) {
	e := newEnv(t)
	c := component.Component{Format: "cargo", Name: "serde", Version: "1.0.0"}
	for _, name := range []string{"cargo-proxy", "cargo-all"} {
		if _, err := e.h.DeleteComponent(context.Background(), e.repos[name], c, "system"); !errors.Is(err, component.ErrNotSupported) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestDeleteComponentCargoGuardFirst(t *testing.T) {
	e := newEnv(t)
	c := component.Component{Format: "npm", Name: "serde", Version: "1.0.0"}
	_, err := e.h.DeleteComponent(context.Background(), e.repos["cargo-hosted"], c, "system")
	if err == nil || errors.Is(err, component.ErrNotSupported) {
		t.Fatalf("wrong-format component: %v", err)
	}
}

// A crate downloaded since an evaluation is skipped, as by the other deleters.
func TestDeleteComponentCargoSkipsChanged(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.seedHosted(t, "serde", "1.0.0", "crate")
	key := crateKey("serde", "1.0.0")
	if err := e.h.Store.DB.FlushDownloads(ctx, []meta.DownloadDelta{{Repo: "cargo-hosted", Path: key, N: 1, Last: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	c := component.Component{Format: "cargo", Name: "serde", Version: "1.0.0",
		Evaluated: &component.Evaluated{Paths: map[string]bool{key: true}, MaxCreated: future, MaxModified: future}}
	if _, err := e.h.DeleteComponent(ctx, e.repos["cargo-hosted"], c, "system"); !errors.Is(err, component.ErrChanged) {
		t.Fatalf("changed: %v", err)
	}
	if pkgs, _ := e.h.Store.DB.ListPackages(ctx, "cargo-hosted", "serde"); len(pkgs) != 1 {
		t.Fatalf("rows = %+v", pkgs)
	}
}
