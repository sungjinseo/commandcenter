package maven

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

func assetPaths(t *testing.T, e *env, repoName, prefix string) string {
	t.Helper()
	as, err := e.h.Store.DB.ListAssets(context.Background(), repoName, prefix)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range as {
		out = append(out, a.Path)
	}
	return strings.Join(out, ",")
}

func mavenComp(version string) component.Component {
	return component.Component{Format: "maven", Group: "org.example", Name: "app", Version: version}
}

func (e *env) putAll(t *testing.T, repoName string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if w := e.do("PUT", repoName, p, fileBody(p)); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", p, w.Code, w.Body)
		}
	}
}

// fileBody is what putAll uploads to p: the path itself, or for a POM one
// that names p's coordinates (QA N-6), sbtLegacy's in sbt's legacy layout.
func fileBody(p string) string {
	pp, _ := ParsePath(p)
	c := pp.Coords
	if c == nil {
		c = sbtLegacy(pp.Main)
	}
	if c != nil && c.Extension == "pom" && c.Classifier == "" {
		return "<project><groupId>" + c.GroupID + "</groupId><artifactId>" + c.ArtifactID +
			"</artifactId><version>" + c.BaseVersion + "</version></project>"
	}
	return p
}

func TestDeleteComponentRelease(t *testing.T) {
	e := newEnv(t)
	sink := &repotest.Sink{}
	e.h.Store.Events = sink
	deleted := []string{
		"org/example/app/1.0/app-1.0.jar", "org/example/app/1.0/app-1.0.pom",
		"org/example/app/1.0/app-1.0-sources.jar", "org/example/app/1.0/app-1.0.jar.asc",
	}
	e.putAll(t, "releases", deleted...)
	e.putAll(t, "releases", "org/example/app/2.0/app-2.0.jar", "org/example/lib/1.0/lib-1.0.jar")
	if got := versionsOf(t, e, "releases"); got != "1.0,2.0 release=2.0" {
		t.Fatalf("before: %s", got)
	}
	sink.Reset()
	res, err := e.h.DeleteComponent(context.Background(), e.repos["releases"], mavenComp("1.0"), "system")
	if err != nil {
		t.Fatal(err)
	}
	var bytes int64
	for _, p := range deleted {
		bytes += int64(len(fileBody(p)))
	}
	if res != (component.Result{Assets: 4, Bytes: bytes}) {
		t.Fatalf("result = %+v, want 4 assets %d bytes", res, bytes)
	}
	want := "org/example/app/2.0/app-2.0.jar,org/example/app/maven-metadata.xml," +
		"org/example/lib/1.0/lib-1.0.jar,org/example/lib/maven-metadata.xml"
	if got := assetPaths(t, e, "releases", ""); got != want {
		t.Fatalf("assets = %s\nwant     %s", got, want)
	}
	if got := versionsOf(t, e, "releases"); got != "2.0 release=2.0" {
		t.Fatalf("metadata after delete: %s", got)
	}
	wantEv := repotest.Event{Kind: "delete", Actor: "system", Repo: "releases", Subject: "org.example:app:1.0", Detail: "assets=4"}
	if got := sink.Events(); len(got) != 1 || got[0] != wantEv {
		t.Fatalf("events: %v", got)
	}

	// the last version: the GA metadata goes too
	if _, err := e.h.DeleteComponent(context.Background(), e.repos["releases"], mavenComp("2.0"), "system"); err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", "releases", "org/example/app/maven-metadata.xml", ""); w.Code != 404 {
		t.Fatalf("GA metadata after last delete: %d", w.Code)
	}

	// a component that is gone already deletes nothing and emits nothing
	sink.Reset()
	res, err = e.h.DeleteComponent(context.Background(), e.repos["releases"], mavenComp("1.0"), "system")
	if err != nil || res != (component.Result{}) {
		t.Fatalf("missing component: %+v %v", res, err)
	}
	if got := sink.Events(); len(got) != 0 {
		t.Fatalf("events for a missing component: %v", got)
	}
}

// An sbt plugin version's files in both layouts are one component: deleting
// it leaves no legacy-layout file, which sbt falls back to, behind.
func TestDeleteComponentSbtPluginBothLayouts(t *testing.T) {
	e := newEnv(t)
	const dir = "com/example/sbt-foo_2.12_1.0/1.0.0/"
	e.putAll(t, "releases", dir+"sbt-foo_2.12_1.0-1.0.0.pom", dir+"sbt-foo_2.12_1.0-1.0.0.jar", dir+"sbt-foo-1.0.0.pom", dir+"sbt-foo-1.0.0.jar")
	c := component.Component{Format: "maven", Group: "com.example", Name: "sbt-foo_2.12_1.0", Version: "1.0.0"}
	res, err := e.h.DeleteComponent(context.Background(), e.repos["releases"], c, "system")
	if err != nil || res.Assets != 4 {
		t.Fatalf("DeleteComponent: %+v %v, want 4 assets", res, err)
	}
	if got := assetPaths(t, e, "releases", ""); got != "" {
		t.Fatalf("left behind: %s", got)
	}
}

// Neighbours that share prefixes with the component must survive: version
// 1.0.1 next to 1.0, artifact app-extra next to app, and GAs nested under the
// version directory (groups org.example.app.1.0).
func TestDeleteComponentMavenNeighbours(t *testing.T) {
	e := newEnv(t)
	e.putAll(t, "releases",
		"org/example/app/1.0/app-1.0.jar",
		"org/example/app/1.0.1/app-1.0.1.jar",
		"org/example/app-extra/1.0/app-extra-1.0.jar",
		"org/example/app/1.0/sub/1.0/sub-1.0.jar",
		"org/example/app/1.0/app/1.0/app-1.0.jar",
	)
	res, err := e.h.DeleteComponent(context.Background(), e.repos["releases"], mavenComp("1.0"), "system")
	if err != nil {
		t.Fatal(err)
	}
	if res.Assets != 1 {
		t.Fatalf("result = %+v", res)
	}
	var left []string
	for _, p := range strings.Split(assetPaths(t, e, "releases", ""), ",") {
		if !strings.HasSuffix(p, "maven-metadata.xml") {
			left = append(left, p)
		}
	}
	want := "org/example/app-extra/1.0/app-extra-1.0.jar,org/example/app/1.0.1/app-1.0.1.jar," +
		"org/example/app/1.0/app/1.0/app-1.0.jar,org/example/app/1.0/sub/1.0/sub-1.0.jar"
	if got := strings.Join(left, ","); got != want {
		t.Fatalf("left = %s\nwant   %s", got, want)
	}
	if got := versionsOf(t, e, "releases"); got != "1.0.1 release=1.0.1" {
		t.Fatalf("metadata: %s", got)
	}
}

func TestDeleteComponentSnapshot(t *testing.T) {
	e := newEnv(t)
	base := "org/example/app/1.0-SNAPSHOT/"
	vmeta := `<metadata><groupId>org.example</groupId><artifactId>app</artifactId><version>1.0-SNAPSHOT</version>
<versioning><snapshot><timestamp>20261019.120000</timestamp><buildNumber>2</buildNumber></snapshot><lastUpdated>20261019120000</lastUpdated></versioning></metadata>`
	e.putAll(t, "snapshots", base+"app-1.0-20261019.110000-1.jar", base+"app-1.0-20261019.120000-2.jar", base+"app-1.0-20261019.120000-2.pom")
	if w := e.do("PUT", "snapshots", base+"maven-metadata.xml", vmeta); w.Code != 201 {
		t.Fatalf("PUT version metadata: %d", w.Code)
	}
	e.putAll(t, "snapshots", "org/example/app/2.0-SNAPSHOT/app-2.0-SNAPSHOT.jar")
	res, err := e.h.DeleteComponent(context.Background(), e.repos["snapshots"], mavenComp("1.0-SNAPSHOT"), "system")
	if err != nil || res.Assets != 3 {
		t.Fatalf("result = %+v %v", res, err)
	}
	if got := assetPaths(t, e, "snapshots", base); got != "" {
		t.Fatalf("snapshot folder left: %s", got)
	}
	if got := versionsOf(t, e, "snapshots"); !strings.HasPrefix(got, "2.0-SNAPSHOT ") {
		t.Fatalf("metadata: %s", got)
	}
}

func TestDeleteComponentReplacedRepository(t *testing.T) {
	e := newEnv(t)
	sink := &repotest.Sink{}
	e.do("PUT", "releases", "org/example/app/1.0/app-1.0.jar", "x")
	e.h.Store.Events = sink
	e.h.Store.Live = func(*repo.Repository) bool { return false }
	res, err := e.h.DeleteComponent(context.Background(), e.repos["releases"], mavenComp("1.0"), "system")
	if err == nil || repo.StatusOf(err) != 503 || res != (component.Result{}) {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	e.h.Store.Live = nil
	if got := assetPaths(t, e, "releases", ""); got != "org/example/app/1.0/app-1.0.jar,org/example/app/maven-metadata.xml" {
		t.Fatalf("assets after refused delete: %s", got)
	}
	if got := sink.Events(); len(got) != 0 {
		t.Fatalf("events: %v", got)
	}
}

func TestDeleteComponentRefusesOtherRepos(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"central", "public"} {
		_, err := e.h.DeleteComponent(context.Background(), e.repos[name], mavenComp("1.0"), "system")
		if !errors.Is(err, component.ErrNotSupported) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestDeleteComponentGuards(t *testing.T) {
	e := newEnv(t)
	e.putAll(t, "releases", "org/example/app/1.0/app-1.0.jar")
	wrongRepo := mavenComp("1.0")
	wrongRepo.Repo = "snapshots"
	wrongFormat := mavenComp("1.0")
	wrongFormat.Format = "npm"
	noFormat := mavenComp("1.0")
	noFormat.Format = ""
	for _, c := range []component.Component{wrongRepo, wrongFormat, noFormat} {
		if _, err := e.h.DeleteComponent(context.Background(), e.repos["releases"], c, "system"); err == nil {
			t.Fatalf("%+v: deleted", c)
		}
	}
	if got := assetPaths(t, e, "releases", "org/example/app/1.0/"); got != "org/example/app/1.0/app-1.0.jar" {
		t.Fatalf("assets: %s", got)
	}
	ok := mavenComp("1.0")
	ok.Repo = "releases"
	if res, err := e.h.DeleteComponent(context.Background(), e.repos["releases"], ok, "system"); err != nil || res.Assets != 1 {
		t.Fatalf("matching repo: %+v %v", res, err)
	}
}

// A component that changed since cleanup evaluated it is skipped with
// ErrChanged: a file the evaluation did not see, or one created later.
func TestDeleteComponentSkipsChanged(t *testing.T) {
	e := newEnv(t)
	files := []string{"org/example/app/1.0/app-1.0.jar", "org/example/app/1.0/app-1.0.pom"}
	e.putAll(t, "releases", files...)
	future := time.Now().Add(time.Hour)
	for _, ev := range []*component.Evaluated{
		{Paths: map[string]bool{files[0]: true}, MaxCreated: future, MaxModified: future},                          // the pom is new
		{Paths: map[string]bool{files[0]: true, files[1]: true}, MaxCreated: time.Unix(0, 0), MaxModified: future}, // created later
	} {
		c := mavenComp("1.0")
		c.Evaluated = ev
		if _, err := e.h.DeleteComponent(context.Background(), e.repos["releases"], c, "system"); !errors.Is(err, component.ErrChanged) {
			t.Fatalf("%+v: %v", ev, err)
		}
		if got := assetPaths(t, e, "releases", "org/example/app/1.0/"); got != strings.Join(files, ",") {
			t.Fatalf("deleted: %s", got)
		}
	}
	c := mavenComp("1.0")
	c.Evaluated = &component.Evaluated{Paths: map[string]bool{files[0]: true, files[1]: true}, MaxCreated: future, MaxModified: future}
	if res, err := e.h.DeleteComponent(context.Background(), e.repos["releases"], c, "system"); err != nil || res.Assets != 2 {
		t.Fatalf("unchanged: %+v %v", res, err)
	}
}
