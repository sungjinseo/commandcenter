package golang

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
)

func goAssets(t *testing.T, e *env) string {
	t.Helper()
	as, err := e.h.Store.DB.ListAssets(context.Background(), "go-hosted", "")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range as {
		out = append(out, a.Path)
	}
	return strings.Join(out, ",")
}

func seedGoVersion(t *testing.T, e *env, module, version string) {
	t.Helper()
	for _, ext := range []string{".mod", ".info", ".zip"} {
		e.seed(t, "go-hosted", module+"/@v/"+version+ext, "x")
	}
}

func TestDeleteComponentGo(t *testing.T) {
	e := newEnv(t)
	seedGoVersion(t, e, "example.com/!hello", "v1.0.0")
	seedGoVersion(t, e, "example.com/!hello", "v1.1.0")
	seedGoVersion(t, e, "example.com/other", "v1.0.0")
	if w := e.do("GET", "go-hosted", "example.com/!hello/@v/list", nil); w.Body.String() != "v1.0.0\nv1.1.0\n" {
		t.Fatalf("list before: %q", w.Body.String())
	}
	sink := &repotest.Sink{}
	e.h.Store.Events = sink

	c := component.Component{Format: "go", Name: "example.com/Hello", Version: "v1.0.0"}
	if res, err := e.h.DeleteComponent(context.Background(), e.repos["go-hosted"], c, "system"); err != nil || res != (component.Result{Assets: 3, Bytes: 3}) {
		t.Fatalf("result = %+v %v", res, err)
	}
	want := "example.com/!hello/@v/v1.1.0.info,example.com/!hello/@v/v1.1.0.mod,example.com/!hello/@v/v1.1.0.zip," +
		"example.com/other/@v/v1.0.0.info,example.com/other/@v/v1.0.0.mod,example.com/other/@v/v1.0.0.zip"
	if got := goAssets(t, e); got != want {
		t.Fatalf("assets = %s", got)
	}
	if w := e.do("GET", "go-hosted", "example.com/!hello/@v/list", nil); w.Code != 200 || w.Body.String() != "v1.1.0\n" {
		t.Fatalf("list after: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("GET", "go-hosted", "example.com/!hello/@v/v1.0.0.zip", nil); w.Code != 404 {
		t.Fatalf("deleted zip: %d", w.Code)
	}
	wantEv := repotest.Event{Kind: "delete", Actor: "system", Repo: "go-hosted", Subject: "example.com/Hello@v1.0.0", Detail: "assets=3"}
	if got := sink.Events(); len(got) != 1 || got[0] != wantEv {
		t.Fatalf("events: %v", got)
	}
}

func TestDeleteComponentGoReplacedRepository(t *testing.T) {
	e := newEnv(t)
	seedGoVersion(t, e, "example.com/m", "v1.0.0")
	e.h.Store.Live = func(*repo.Repository) bool { return false }
	c := component.Component{Format: "go", Name: "example.com/m", Version: "v1.0.0"}
	if _, err := e.h.DeleteComponent(context.Background(), e.repos["go-hosted"], c, "system"); repo.StatusOf(err) != 503 {
		t.Fatalf("err = %v", err)
	}
	if got := goAssets(t, e); strings.Count(got, ",") != 2 {
		t.Fatalf("assets after refused delete: %s", got)
	}
}

func TestDeleteComponentGoRefusesOtherRepos(t *testing.T) {
	e := newEnv(t)
	c := component.Component{Format: "go", Name: "example.com/m", Version: "v1.0.0"}
	for _, name := range []string{"go-proxy", "go-all"} {
		if _, err := e.h.DeleteComponent(context.Background(), e.repos[name], c, "system"); !errors.Is(err, component.ErrNotSupported) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

// example.com/m/v2 lives under example.com/m/ but is another module.
func TestDeleteComponentGoMajorNeighbour(t *testing.T) {
	e := newEnv(t)
	seedGoVersion(t, e, "example.com/m", "v1.0.0")
	seedGoVersion(t, e, "example.com/m/v2", "v2.0.0")
	seedGoVersion(t, e, "example.com/m/v2", "v1.0.0") // same version string under the other module
	c := component.Component{Format: "go", Name: "example.com/m", Version: "v1.0.0"}
	res, err := e.h.DeleteComponent(context.Background(), e.repos["go-hosted"], c, "system")
	if err != nil || res != (component.Result{Assets: 3, Bytes: 3}) {
		t.Fatalf("result = %+v %v", res, err)
	}
	want := "example.com/m/v2/@v/v1.0.0.info,example.com/m/v2/@v/v1.0.0.mod,example.com/m/v2/@v/v1.0.0.zip," +
		"example.com/m/v2/@v/v2.0.0.info,example.com/m/v2/@v/v2.0.0.mod,example.com/m/v2/@v/v2.0.0.zip"
	if got := goAssets(t, e); got != want {
		t.Fatalf("assets = %s\nwant     %s", got, want)
	}
	if w := e.do("GET", "go-hosted", "example.com/m/v2/@v/list", nil); w.Body.String() != "v1.0.0\nv2.0.0\n" {
		t.Fatalf("v2 list: %q", w.Body.String())
	}
}

func TestDeleteComponentGoGuards(t *testing.T) {
	e := newEnv(t)
	seedGoVersion(t, e, "example.com/m", "v1.0.0")
	for _, c := range []component.Component{
		{Repo: "go-proxy", Format: "go", Name: "example.com/m", Version: "v1.0.0"},
		{Format: "maven", Name: "example.com/m", Version: "v1.0.0"},
	} {
		if _, err := e.h.DeleteComponent(context.Background(), e.repos["go-hosted"], c, "system"); err == nil {
			t.Fatalf("%+v: deleted", c)
		}
	}
	if got := goAssets(t, e); strings.Count(got, ",") != 2 {
		t.Fatalf("assets: %s", got)
	}
}

// A version downloaded since cleanup evaluated it is skipped.
func TestDeleteComponentGoSkipsChanged(t *testing.T) {
	e := newEnv(t)
	seedGoVersion(t, e, "example.com/other", "v1.0.0")
	paths := map[string]bool{}
	for _, ext := range []string{".mod", ".info", ".zip"} {
		paths["example.com/other/@v/v1.0.0"+ext] = true
	}
	if err := e.h.Store.DB.FlushDownloads(context.Background(), []meta.DownloadDelta{{Repo: "go-hosted", Path: "example.com/other/@v/v1.0.0.zip", N: 1, Last: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	c := component.Component{Format: "go", Name: "example.com/other", Version: "v1.0.0",
		Evaluated: &component.Evaluated{Paths: paths, MaxCreated: future, MaxModified: future}} // never downloaded when evaluated
	if _, err := e.h.DeleteComponent(context.Background(), e.repos["go-hosted"], c, "system"); !errors.Is(err, component.ErrChanged) {
		t.Fatalf("changed: %v", err)
	}
	if got := goAssets(t, e); strings.Count(got, "v1.0.0") != 3 {
		t.Fatalf("deleted: %s", got)
	}
	c.Evaluated.LastDownloaded = future
	if res, err := e.h.DeleteComponent(context.Background(), e.repos["go-hosted"], c, "system"); err != nil || res.Assets != 3 {
		t.Fatalf("unchanged: %+v %v", res, err)
	}
}
