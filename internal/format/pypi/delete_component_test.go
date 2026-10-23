package pypi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/component"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
)

func pypiState(t *testing.T, e *env) (rows, assets string) {
	t.Helper()
	ctx := context.Background()
	pkgs, err := e.h.Store.DB.ListPackages(ctx, "py-hosted", "demo-pkg")
	if err != nil {
		t.Fatal(err)
	}
	var r []string
	for _, p := range pkgs {
		r = append(r, p.Version)
	}
	as, err := e.h.Store.DB.ListAssets(ctx, "py-hosted", "")
	if err != nil {
		t.Fatal(err)
	}
	var a []string
	for _, x := range as {
		a = append(a, x.Path)
	}
	return strings.Join(r, ","), strings.Join(a, ",")
}

func TestDeleteComponentPyPI(t *testing.T) {
	e := newEnv(t)
	e.seedHosted(t, "demo-pkg", "demo_pkg-1.0-py3-none-any.whl", "w10")
	e.seedHosted(t, "demo-pkg", "demo_pkg-1.0.tar.gz", "s10")
	e.seedHosted(t, "demo-pkg", "demo_pkg-1.0.1.tar.gz", "s101")
	e.seedHosted(t, "other", "other-1.0.tar.gz", "o10")
	sink := &repotest.Sink{}
	e.h.Store.Events = sink

	c := component.Component{Format: "pypi", Name: "demo-pkg", Version: "1.0"}
	res, err := e.h.DeleteComponent(context.Background(), e.repos["py-hosted"], c, "system")
	if err == nil && res != (component.Result{Assets: 2, Bytes: int64(len("w10") + len("s10"))}) {
		t.Fatalf("result = %+v", res)
	}
	if err != nil {
		t.Fatal(err)
	}
	rows, assets := pypiState(t, e)
	if rows != "demo_pkg-1.0.1.tar.gz" {
		t.Fatalf("rows = %s", rows)
	}
	if assets != "packages/demo-pkg/demo_pkg-1.0.1.tar.gz,packages/other/other-1.0.tar.gz" {
		t.Fatalf("assets = %s", assets)
	}
	if other, _ := e.h.Store.DB.ListPackages(context.Background(), "py-hosted", "other"); len(other) != 1 {
		t.Fatalf("other rows = %+v", other)
	}
	body := e.do("GET", "py-hosted", "simple/demo-pkg/", "").Body.String()
	if strings.Contains(body, "demo_pkg-1.0.tar.gz") || strings.Contains(body, "demo_pkg-1.0-py3") || !strings.Contains(body, "demo_pkg-1.0.1.tar.gz") {
		t.Fatalf("index = %s", body)
	}
	want := repotest.Event{Kind: "delete", Actor: "system", Repo: "py-hosted", Subject: "demo-pkg@1.0", Detail: "assets=2"}
	if got := sink.Events(); len(got) != 1 || got[0] != want {
		t.Fatalf("events: %v", got)
	}
}

func TestDeleteComponentPyPIReplacedRepository(t *testing.T) {
	e := newEnv(t)
	e.seedHosted(t, "demo-pkg", "demo_pkg-1.0.tar.gz", "s10")
	e.h.Store.Live = func(*repo.Repository) bool { return false }
	c := component.Component{Format: "pypi", Name: "demo-pkg", Version: "1.0"}
	if _, err := e.h.DeleteComponent(context.Background(), e.repos["py-hosted"], c, "system"); repo.StatusOf(err) != 503 {
		t.Fatalf("err = %v", err)
	}
	if rows, assets := pypiState(t, e); rows != "demo_pkg-1.0.tar.gz" || assets != "packages/demo-pkg/demo_pkg-1.0.tar.gz" {
		t.Fatalf("after refused delete: %s / %s", rows, assets)
	}
}

func TestDeleteComponentPyPIRefusesOtherRepos(t *testing.T) {
	e := newEnv(t)
	c := component.Component{Format: "pypi", Name: "demo-pkg", Version: "1.0"}
	for _, name := range []string{"py-proxy", "py-all"} {
		if _, err := e.h.DeleteComponent(context.Background(), e.repos[name], c, "system"); !errors.Is(err, component.ErrNotSupported) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestDeleteComponentPyPIGuards(t *testing.T) {
	e := newEnv(t)
	e.seedHosted(t, "demo-pkg", "demo_pkg-1.0.tar.gz", "s10")
	for _, c := range []component.Component{
		{Repo: "py-ro", Format: "pypi", Name: "demo-pkg", Version: "1.0"},
		{Format: "npm", Name: "demo-pkg", Version: "1.0"},
	} {
		if _, err := e.h.DeleteComponent(context.Background(), e.repos["py-hosted"], c, "system"); err == nil {
			t.Fatalf("%+v: deleted", c)
		}
	}
	if rows, _ := pypiState(t, e); rows != "demo_pkg-1.0.tar.gz" {
		t.Fatalf("rows = %s", rows)
	}
}

// A version that gained a file since cleanup evaluated it is skipped.
func TestDeleteComponentPyPISkipsChanged(t *testing.T) {
	e := newEnv(t)
	e.seedHosted(t, "demo-pkg", "demo_pkg-1.0.tar.gz", "s10")
	e.seedHosted(t, "demo-pkg", "demo_pkg-1.0-py3-none-any.whl", "w10") // uploaded after the evaluation
	future := time.Now().Add(time.Hour)
	c := component.Component{Format: "pypi", Name: "demo-pkg", Version: "1.0",
		Evaluated: &component.Evaluated{Paths: map[string]bool{"packages/demo-pkg/demo_pkg-1.0.tar.gz": true}, MaxCreated: future, MaxModified: future}}
	if _, err := e.h.DeleteComponent(context.Background(), e.repos["py-hosted"], c, "system"); !errors.Is(err, component.ErrChanged) {
		t.Fatalf("changed: %v", err)
	}
	if rows, _ := pypiState(t, e); rows != "demo_pkg-1.0-py3-none-any.whl,demo_pkg-1.0.tar.gz" {
		t.Fatalf("rows = %s", rows)
	}
	c.Evaluated.Paths["packages/demo-pkg/demo_pkg-1.0-py3-none-any.whl"] = true
	if res, err := e.h.DeleteComponent(context.Background(), e.repos["py-hosted"], c, "system"); err != nil || res.Assets != 2 {
		t.Fatalf("unchanged: %+v %v", res, err)
	}
}
