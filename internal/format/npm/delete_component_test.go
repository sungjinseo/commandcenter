package npm

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

func npmComp(version string) component.Component {
	return component.Component{Format: "npm", Group: "@s", Name: "hello", Version: version}
}

func (e *env) hasAsset(t *testing.T, repoName, path string) bool {
	t.Helper()
	_, err := e.h.Store.DB.GetAsset(context.Background(), repoName, path)
	return err == nil
}

func tagsOf(t *testing.T, e *env, name string) map[string]string {
	t.Helper()
	tags, err := e.h.Store.DB.ListDistTags(context.Background(), "npm-hosted", name)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, tg := range tags {
		out[tg.Tag] = tg.Version
	}
	return out
}

func TestDeleteComponentNpm(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rp := e.repos["npm-hosted"]
	// published in this order, so latest ends at 1.1.0 although a newer
	// prerelease exists
	for _, v := range []string{"1.0.0", "2.0.0-beta.1", "1.1.0"} {
		if w := e.do("PUT", "npm-hosted", "@s/hello", publishBody("@s/hello", v, []byte("tgz-"+v), false), ci); w.Code != 201 {
			t.Fatalf("publish %s: %d %s", v, w.Code, w.Body.String())
		}
	}
	if err := e.h.setDistTag(ctx, rp, "@s/hello", "next", "2.0.0-beta.1"); err != nil {
		t.Fatal(err)
	}
	if err := e.h.setDistTag(ctx, rp, "@s/hello", "stable", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	sink := &repotest.Sink{}
	e.h.Store.Events = sink

	res, err := e.h.DeleteComponent(ctx, rp, npmComp("1.1.0"), "system")
	if err != nil || res != (component.Result{Assets: 1, Bytes: int64(len("tgz-1.1.0"))}) {
		t.Fatalf("result = %+v %v", res, err)
	}
	if e.hasAsset(t, "npm-hosted", "@s/hello/-/hello-1.1.0.tgz") {
		t.Fatal("1.1.0 tarball left")
	}
	for _, v := range []string{"1.0.0", "2.0.0-beta.1"} {
		if !e.hasAsset(t, "npm-hosted", "@s/hello/-/hello-"+v+".tgz") {
			t.Fatalf("%s tarball removed", v)
		}
	}
	pkgs, _ := e.h.Store.DB.ListPackages(ctx, "npm-hosted", "@s/hello")
	if len(pkgs) != 2 || pkgs[0].Version != "1.0.0" || pkgs[1].Version != "2.0.0-beta.1" {
		t.Fatalf("packages = %+v", pkgs)
	}
	// latest moves to the newest release, not the newer prerelease; the tag
	// that pointed at the deleted version goes, the others stay
	if got := tagsOf(t, e, "@s/hello"); len(got) != 2 || got["latest"] != "1.0.0" || got["next"] != "2.0.0-beta.1" {
		t.Fatalf("tags = %v", got)
	}
	m := jsonBody(t, e.do("GET", "npm-hosted", "@s/hello", "", anon))
	if get(m, "dist-tags", "latest") != "1.0.0" || get(m, "versions", "1.1.0") != nil || get(m, "versions", "1.0.0") == nil || get(m, "dist-tags", "stable") != nil {
		t.Fatalf("packument = %v", m)
	}
	want := repotest.Event{Kind: "delete", Actor: "system", Repo: "npm-hosted", Subject: "@s/hello@1.1.0", Detail: "assets=1"}
	if got := sink.Events(); len(got) != 1 || got[0] != want {
		t.Fatalf("events: %v", got)
	}

	// only prereleases left: latest takes one of them
	if _, err := e.h.DeleteComponent(ctx, rp, npmComp("1.0.0"), "system"); err != nil {
		t.Fatal(err)
	}
	if got := tagsOf(t, e, "@s/hello"); got["latest"] != "2.0.0-beta.1" || got["next"] != "2.0.0-beta.1" {
		t.Fatalf("tags with only a prerelease = %v", got)
	}

	// the last version: the package 404s
	if _, err := e.h.DeleteComponent(ctx, rp, npmComp("2.0.0-beta.1"), "system"); err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", "npm-hosted", "@s/hello", "", anon); w.Code != 404 {
		t.Fatalf("packument after last delete: %d %s", w.Code, w.Body.String())
	}
	if got := tagsOf(t, e, "@s/hello"); len(got) != 0 {
		t.Fatalf("tags left: %v", got)
	}
	if as, _ := e.h.Store.DB.ListAssets(ctx, "npm-hosted", "@s/hello/"); len(as) != 0 {
		t.Fatalf("assets left: %+v", as)
	}
	// it can be published again from scratch
	if w := e.do("PUT", "npm-hosted", "@s/hello", publishBody("@s/hello", "3.0.0", []byte("tgz-3"), false), ci); w.Code != 201 {
		t.Fatalf("republish: %d %s", w.Code, w.Body.String())
	}
	if got := tagsOf(t, e, "@s/hello"); len(got) != 1 || got["latest"] != "3.0.0" {
		t.Fatalf("tags after republish = %v", got)
	}
}

func TestDeleteComponentNpmUnscopedKeepsOtherPackages(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, p := range [][2]string{{"left-pad", "1.0.0"}, {"left-pad", "1.0.1"}, {"right-pad", "1.0.0"}} {
		if w := e.do("PUT", "npm-hosted", p[0], publishBody(p[0], p[1], []byte(p[0]+p[1]), false), ci); w.Code != 201 {
			t.Fatalf("publish %v: %d", p, w.Code)
		}
	}
	c := component.Component{Format: "npm", Name: "left-pad", Version: "1.0.0"}
	if _, err := e.h.DeleteComponent(ctx, e.repos["npm-hosted"], c, "system"); err != nil {
		t.Fatal(err)
	}
	if e.hasAsset(t, "npm-hosted", "left-pad/-/left-pad-1.0.0.tgz") || !e.hasAsset(t, "npm-hosted", "left-pad/-/left-pad-1.0.1.tgz") ||
		!e.hasAsset(t, "npm-hosted", "right-pad/-/right-pad-1.0.0.tgz") {
		t.Fatal("wrong tarballs removed")
	}
	if got := tagsOf(t, e, "left-pad"); got["latest"] != "1.0.1" {
		t.Fatalf("left-pad tags = %v", got)
	}
	if pkgs, _ := e.h.Store.DB.ListPackages(ctx, "npm-hosted", "right-pad"); len(pkgs) != 1 {
		t.Fatalf("right-pad rows = %+v", pkgs)
	}
}

func TestDeleteComponentNpmReplacedRepository(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if w := e.do("PUT", "npm-hosted", "@s/hello", publishBody("@s/hello", "1.0.0", []byte("t"), false), ci); w.Code != 201 {
		t.Fatalf("publish: %d", w.Code)
	}
	sink := &repotest.Sink{}
	e.h.Store.Events = sink
	e.h.Store.Live = func(*repo.Repository) bool { return false }
	if _, err := e.h.DeleteComponent(ctx, e.repos["npm-hosted"], npmComp("1.0.0"), "system"); repo.StatusOf(err) != 503 {
		t.Fatalf("err = %v", err)
	}
	if !e.hasAsset(t, "npm-hosted", "@s/hello/-/hello-1.0.0.tgz") {
		t.Fatal("tarball removed by a refused delete")
	}
	if pkgs, _ := e.h.Store.DB.ListPackages(ctx, "npm-hosted", "@s/hello"); len(pkgs) != 1 {
		t.Fatalf("rows = %+v", pkgs)
	}
	if len(sink.Events()) != 0 {
		t.Fatalf("events: %v", sink.Events())
	}
}

func TestDeleteComponentNpmRefusesOtherRepos(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"npm-proxy", "npm-all"} {
		if _, err := e.h.DeleteComponent(context.Background(), e.repos[name], npmComp("1.0.0"), "system"); !errors.Is(err, component.ErrNotSupported) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

// Packages whose names share a prefix with the component stay: the scoped
// @s/hello-world and the unscoped hello have tarballs named hello-1.0.0.tgz
// or hello-world-1.0.0.tgz too.
func TestDeleteComponentNpmNeighbours(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, p := range [][2]string{{"@s/hello", "1.0.0"}, {"@s/hello", "1.0.1"}, {"@s/hello-world", "1.0.0"}, {"hello", "1.0.0"}} {
		if w := e.do("PUT", "npm-hosted", p[0], publishBody(p[0], p[1], []byte(p[0]+p[1]), false), ci); w.Code != 201 {
			t.Fatalf("publish %v: %d", p, w.Code)
		}
	}
	res, err := e.h.DeleteComponent(ctx, e.repos["npm-hosted"], npmComp("1.0.0"), "system")
	if err != nil || res.Assets != 1 {
		t.Fatalf("result = %+v %v", res, err)
	}
	as, _ := e.h.Store.DB.ListAssets(ctx, "npm-hosted", "")
	var got []string
	for _, a := range as {
		got = append(got, a.Path)
	}
	want := "@s/hello-world/-/hello-world-1.0.0.tgz,@s/hello-world/package.json,@s/hello/-/hello-1.0.1.tgz,@s/hello/package.json," +
		"hello/-/hello-1.0.0.tgz,hello/package.json"
	if strings.Join(got, ",") != want {
		t.Fatalf("assets = %s\nwant     %s", strings.Join(got, ","), want)
	}
	for _, name := range []string{"@s/hello-world", "hello"} {
		if pkgs, _ := e.h.Store.DB.ListPackages(ctx, "npm-hosted", name); len(pkgs) != 1 || pkgs[0].Version != "1.0.0" {
			t.Fatalf("%s rows = %+v", name, pkgs)
		}
		if got := tagsOf(t, e, name); got["latest"] != "1.0.0" {
			t.Fatalf("%s tags = %v", name, got)
		}
	}
	if got := tagsOf(t, e, "@s/hello"); got["latest"] != "1.0.1" {
		t.Fatalf("@s/hello tags = %v", got)
	}
}

func TestDeleteComponentNpmGuards(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "npm-hosted", "@s/hello", publishBody("@s/hello", "1.0.0", []byte("t"), false), ci); w.Code != 201 {
		t.Fatalf("publish: %d", w.Code)
	}
	other := npmComp("1.0.0")
	other.Repo = "npm-proxy"
	wrongFormat := npmComp("1.0.0")
	wrongFormat.Format = "maven"
	for _, c := range []component.Component{other, wrongFormat} {
		if _, err := e.h.DeleteComponent(context.Background(), e.repos["npm-hosted"], c, "system"); err == nil {
			t.Fatalf("%+v: deleted", c)
		}
	}
	if !e.hasAsset(t, "npm-hosted", "@s/hello/-/hello-1.0.0.tgz") {
		t.Fatal("tarball removed by a refused delete")
	}
}

// A version re-published since cleanup evaluated it (deleted and published
// again, so created later) is skipped.
func TestDeleteComponentNpmSkipsChanged(t *testing.T) {
	e := newEnv(t)
	if w := e.do("PUT", "npm-hosted", "@s/hello", publishBody("@s/hello", "1.0.0", []byte("tgz-1.0.0"), false), ci); w.Code != 201 {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	tgz := "@s/hello/-/hello-1.0.0.tgz"
	c := npmComp("1.0.0")
	c.Evaluated = &component.Evaluated{Paths: map[string]bool{tgz: true}, MaxCreated: time.Unix(0, 0), MaxModified: time.Now().Add(time.Hour)}
	if _, err := e.h.DeleteComponent(context.Background(), e.repos["npm-hosted"], c, "system"); !errors.Is(err, component.ErrChanged) {
		t.Fatalf("changed: %v", err)
	}
	if !e.hasAsset(t, "npm-hosted", tgz) {
		t.Fatal("deleted")
	}
	c.Evaluated.MaxCreated = time.Now().Add(time.Hour)
	if res, err := e.h.DeleteComponent(context.Background(), e.repos["npm-hosted"], c, "system"); err != nil || res.Assets != 1 {
		t.Fatalf("unchanged: %+v %v", res, err)
	}
}
