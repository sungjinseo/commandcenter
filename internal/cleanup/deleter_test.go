package cleanup

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/component"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
)

type env struct {
	store *repo.Store
	repos map[string]*repo.Repository
	sink  *repotest.Sink
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	b, err := blob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := meta.Open(metatest.Target(t, filepath.Join(dir, "meta.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sink := &repotest.Sink{}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: time.Now, Events: sink}
	var defs []config.Repository
	for _, f := range []string{"maven", "npm", "pypi", "go", "cargo"} {
		defs = append(defs,
			config.Repository{Name: f + "-proxy", Format: f, Type: "proxy", Remote: "http://remote.example/"},
			config.Repository{Name: f + "-hosted", Format: f, Type: "hosted"},
			config.Repository{Name: f + "-group", Format: f, Type: "group", Members: []string{f + "-hosted"}})
	}
	return &env{store: store, repos: repo.Build(&config.Config{Repositories: defs}), sink: sink}
}

// put stores each path with the path itself as content, so its size is len(path).
func (e *env) put(t *testing.T, repoName string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := e.store.Put(context.Background(), repoName, p, "", strings.NewReader(p)); err != nil {
			t.Fatal(err)
		}
	}
}

// paths lists every asset path of repoName, sorted.
func (e *env) paths(t *testing.T, repoName string) []string {
	t.Helper()
	as, err := e.store.DB.ListAssets(context.Background(), repoName, "")
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, a := range as {
		out = append(out, a.Path)
	}
	return out
}

func sorted(paths []string) []string {
	out := append([]string{}, paths...)
	sort.Strings(out)
	return out
}

func samePaths(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(sorted(want), "\n") {
		t.Fatalf("%s:\n got  %q\n want %q", what, got, sorted(want))
	}
}

func assets(paths ...string) []component.Asset {
	out := make([]component.Asset, len(paths))
	for i, p := range paths {
		out[i] = component.Asset{Path: p}
	}
	return out
}

func bytesOf(paths []string) int64 {
	var n int64
	for _, p := range paths {
		n += int64(len(p))
	}
	return n
}

func TestProxyDeleterRemovesExactlyTheComponent(t *testing.T) {
	tests := []struct {
		name    string
		repo    string
		deleted []string // the component's files
		keep    []string // neighbours that must stay
		comp    component.Component
	}{
		{
			name: "maven",
			repo: "maven-proxy",
			deleted: []string{"org/example/app/1.0/app-1.0.jar", "org/example/app/1.0/app-1.0.jar.sha1",
				"org/example/app/1.0/app-1.0.pom", "org/example/app/1.0/app-1.0-sources.jar"},
			keep: []string{"org/example/app/1.0.1/app-1.0.1.jar", "org/example/app/maven-metadata.xml",
				"org/example/app-extra/1.0/app-extra-1.0.jar", "org/example/app/1.0/sub/1.0/sub-1.0.jar",
				"org/example/app/1.0/app/1.0/app-1.0.jar"},
			// only one file is named: the sidecars and the other files of the
			// component in its directory go too
			comp: component.Component{Format: "maven", Group: "org.example", Name: "app", Version: "1.0", Assets: assets("org/example/app/1.0/app-1.0.jar")},
		},
		{
			name:    "npm scoped",
			repo:    "npm-proxy",
			deleted: []string{"@s/hello/-/hello-1.0.0.tgz"},
			keep: []string{"@s/hello/-/hello-1.0.1.tgz", "@s/hello/package.json", "@s/hello-world/-/hello-world-1.0.0.tgz",
				"hello/-/hello-1.0.0.tgz", "hello-world/-/hello-world-1.0.0.tgz"},
			comp: component.Component{Format: "npm", Group: "@s", Name: "hello", Version: "1.0.0", Assets: assets("@s/hello/-/hello-1.0.0.tgz")},
		},
		{
			name:    "npm unscoped",
			repo:    "npm-proxy",
			deleted: []string{"hello/-/hello-1.0.0.tgz"},
			keep:    []string{"@s/hello/-/hello-1.0.0.tgz", "@s/hello-world/-/hello-world-1.0.0.tgz", "hello-world/-/hello-world-1.0.0.tgz", "hello/package.json"},
			comp:    component.Component{Format: "npm", Name: "hello", Version: "1.0.0", Assets: assets("hello/-/hello-1.0.0.tgz")},
		},
		{
			name:    "pypi",
			repo:    "pypi-proxy",
			deleted: []string{"packages/demo-pkg/demo_pkg-1.0.tar.gz", "packages/demo-pkg/demo_pkg-1.0-py3-none-any.whl"},
			keep:    []string{"packages/demo-pkg/demo_pkg-1.0.1.tar.gz", "simple/demo-pkg/"},
			comp:    component.Component{Format: "pypi", Name: "demo-pkg", Version: "1.0", Assets: assets("packages/demo-pkg/demo_pkg-1.0.tar.gz")},
		},
		{
			name:    "go",
			repo:    "go-proxy",
			deleted: []string{"example.com/!m/@v/v1.0.0.mod", "example.com/!m/@v/v1.0.0.info", "example.com/!m/@v/v1.0.0.zip"},
			keep:    []string{"example.com/!m/@v/v1.1.0.mod", "example.com/!m/@v/list"},
			comp:    component.Component{Format: "go", Name: "example.com/M", Version: "v1.0.0", Assets: assets("example.com/!m/@v/v1.0.0.zip")},
		},
		{
			name:    "go major neighbour",
			repo:    "go-proxy",
			deleted: []string{"example.com/m/@v/v1.0.0.mod", "example.com/m/@v/v1.0.0.zip"},
			keep:    []string{"example.com/m/v2/@v/v1.0.0.mod", "example.com/m/v2/@v/v1.0.0.zip", "example.com/m/v2/@v/v2.0.0.zip"},
			comp:    component.Component{Format: "go", Name: "example.com/m", Version: "v1.0.0", Assets: assets("example.com/m/@v/v1.0.0.zip")},
		},
		{
			name:    "cargo",
			repo:    "cargo-proxy",
			deleted: []string{"crates/serde/1.0.0.crate"},
			keep:    []string{"crates/serde/1.0.1.crate", "index/se/rd/serde"},
			comp:    component.Component{Format: "cargo", Name: "serde", Version: "1.0.0", Assets: assets("crates/serde/1.0.0.crate")},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			all := append(append([]string{}, tc.deleted...), tc.keep...)
			hosted := strings.Replace(tc.repo, "proxy", "hosted", 1)
			e.put(t, tc.repo, all...)
			e.put(t, hosted, all...) // the same paths in another repository stay
			r := NewRegistry(e.store)
			res, err := r.Delete(context.Background(), e.repos[tc.repo], tc.comp, "system")
			if err != nil {
				t.Fatal(err)
			}
			if want := (Result{Assets: len(tc.deleted), Bytes: bytesOf(tc.deleted)}); res != want {
				t.Fatalf("result = %+v, want %+v", res, want)
			}
			samePaths(t, "remaining", e.paths(t, tc.repo), tc.keep)
			samePaths(t, "other repository", e.paths(t, hosted), all)
			want := repotest.Event{Kind: "delete", Actor: "system", Repo: tc.repo, Subject: tc.comp.Label(), Detail: "assets=" + strconv.Itoa(len(tc.deleted))}
			if got := e.sink.Events(); len(got) != 1 || got[0] != want {
				t.Fatalf("events = %v, want %v", got, want)
			}
		})
	}
}

func TestProxyDeleterCountsOnlyDeletedRows(t *testing.T) {
	e := newEnv(t)
	e.put(t, "npm-proxy", "left-pad/-/left-pad-1.0.0.tgz")
	d := &ProxyDeleter{Store: e.store}
	// listed twice, plus a listed file that is not cached
	c := component.Component{Format: "maven", Group: "g", Name: "a", Version: "1.0",
		Assets: assets("g/a/1.0/a-1.0.jar", "g/a/1.0/a-1.0.jar", "g/a/1.0/a-1.0.pom")}
	e.put(t, "maven-proxy", "g/a/1.0/a-1.0.jar")
	res, err := d.DeleteComponent(context.Background(), e.repos["maven-proxy"], c, "system")
	if err != nil || res != (Result{Assets: 1, Bytes: int64(len("g/a/1.0/a-1.0.jar"))}) {
		t.Fatalf("result = %+v %v", res, err)
	}
	// nothing cached: no rows, no event
	e.sink.Reset()
	npm := component.Component{Format: "npm", Name: "left-pad", Version: "2.0.0", Assets: assets("left-pad/-/left-pad-2.0.0.tgz")}
	res, err = d.DeleteComponent(context.Background(), e.repos["npm-proxy"], npm, "system")
	if err != nil || res != (Result{}) {
		t.Fatalf("nothing cached: %+v %v", res, err)
	}
	if len(e.sink.Events()) != 0 {
		t.Fatalf("events: %v", e.sink.Events())
	}
	samePaths(t, "npm-proxy", e.paths(t, "npm-proxy"), []string{"left-pad/-/left-pad-1.0.0.tgz"})
}

func TestProxyDeleterReplacedRepository(t *testing.T) {
	e := newEnv(t)
	e.put(t, "npm-proxy", "left-pad/-/left-pad-1.0.0.tgz")
	e.store.Live = func(*repo.Repository) bool { return false }
	c := component.Component{Format: "npm", Name: "left-pad", Version: "1.0.0", Assets: assets("left-pad/-/left-pad-1.0.0.tgz")}
	res, err := (&ProxyDeleter{Store: e.store}).DeleteComponent(context.Background(), e.repos["npm-proxy"], c, "system")
	if repo.StatusOf(err) != 503 || res != (Result{}) {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	samePaths(t, "after refused delete", e.paths(t, "npm-proxy"), []string{"left-pad/-/left-pad-1.0.0.tgz"})
	if len(e.sink.Events()) != 0 {
		t.Fatalf("events: %v", e.sink.Events())
	}
}

func TestProxyDeleterGuards(t *testing.T) {
	e := newEnv(t)
	e.put(t, "npm-proxy", "left-pad/-/left-pad-1.0.0.tgz", "right-pad/-/right-pad-1.0.0.tgz")
	d := &ProxyDeleter{Store: e.store}
	c := component.Component{Format: "npm", Name: "left-pad", Version: "1.0.0", Assets: assets("left-pad/-/left-pad-1.0.0.tgz")}
	for _, name := range []string{"npm-hosted", "npm-group"} {
		if _, err := d.DeleteComponent(context.Background(), e.repos[name], c, "system"); !errors.Is(err, ErrNotSupported) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	otherRepo := c
	otherRepo.Repo = "npm-hosted"
	noFormat := c
	noFormat.Format = ""
	foreign := c // a listed path of another component
	foreign.Assets = assets("left-pad/-/left-pad-1.0.0.tgz", "right-pad/-/right-pad-1.0.0.tgz")
	for _, bad := range []component.Component{otherRepo, noFormat, foreign} {
		if _, err := d.DeleteComponent(context.Background(), e.repos["npm-proxy"], bad, "system"); err == nil {
			t.Fatalf("%+v: deleted", bad)
		}
	}
	if _, err := d.DeleteComponent(context.Background(), e.repos["maven-proxy"], c, "system"); err == nil {
		t.Fatal("npm component deleted from a maven proxy")
	}
	samePaths(t, "npm-proxy", e.paths(t, "npm-proxy"), []string{"left-pad/-/left-pad-1.0.0.tgz", "right-pad/-/right-pad-1.0.0.tgz"})
	matching := c
	matching.Repo = "npm-proxy"
	if res, err := d.DeleteComponent(context.Background(), e.repos["npm-proxy"], matching, "system"); err != nil || res.Assets != 1 {
		t.Fatalf("matching Repo: %+v %v", res, err)
	}
}

type fakeDeleter struct{ calls []string }

func (f *fakeDeleter) DeleteComponent(_ context.Context, rp *repo.Repository, c component.Component, actor string) (Result, error) {
	f.calls = append(f.calls, rp.Name+" "+c.Label()+" "+actor)
	return Result{Assets: 2, Bytes: 7}, nil
}

func TestRegistry(t *testing.T) {
	e := newEnv(t)
	r := NewRegistry(e.store)
	hosted := &fakeDeleter{}
	r.Register("maven", "hosted", hosted)
	for _, f := range []string{"maven", "npm", "pypi", "go", "cargo"} {
		d, err := r.For(e.repos[f+"-proxy"])
		if _, ok := d.(*ProxyDeleter); err != nil || !ok {
			t.Fatalf("%s proxy: %T %v", f, d, err)
		}
		if _, err := r.For(e.repos[f+"-group"]); !errors.Is(err, ErrNotSupported) {
			t.Fatalf("%s group: %v", f, err)
		}
	}
	if d, err := r.For(e.repos["maven-hosted"]); err != nil || d != hosted {
		t.Fatalf("maven hosted: %T %v", d, err)
	}
	if _, err := r.For(e.repos["npm-hosted"]); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("unregistered hosted: %v", err)
	}
	c := component.Component{Format: "maven", Group: "g", Name: "a", Version: "1"}
	res, err := r.Delete(context.Background(), e.repos["maven-hosted"], c, "system")
	if err != nil || res != (Result{Assets: 2, Bytes: 7}) {
		t.Fatalf("Delete = %+v %v", res, err)
	}
	if len(hosted.calls) != 1 || hosted.calls[0] != "maven-hosted g:a:1 system" {
		t.Fatalf("calls = %v", hosted.calls)
	}
}
