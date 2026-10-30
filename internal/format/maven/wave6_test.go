package maven

import (
	"context"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	ordered := []string{"0.9", "1.0-alpha-1", "1.0-SNAPSHOT", "1.0-20261014.120000-2", "1.0-20261014.120000-10", "1.0-20261015.000000-1", "1.0", "1.0.1", "1.10"}
	for i := range ordered {
		for j := range ordered {
			if got, want := CompareVersions(ordered[i], ordered[j]), sign(i-j); got != want {
				t.Errorf("CompareVersions(%s, %s) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func TestDefaultGoalPrefix(t *testing.T) {
	for a, want := range map[string]string{"maven-compiler-plugin": "compiler", "jacoco-maven-plugin": "jacoco", "maven-plugin-plugin": "plugin", "tool-plugin": "tool"} {
		if got := defaultGoalPrefix(a); got != want {
			t.Errorf("%s: %q, want %q", a, got, want)
		}
	}
}

// maven-07: an artifactId scope rebuilds that GA's document alone, and a
// baseVersion scope that version's document alone.
func TestRebuildMetadataScope(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.putAll(t, "snapshots", "org/example/app/1.0-SNAPSHOT/app-1.0-20261014.120000-1.jar", "org/example/lib/2.0-SNAPSHOT/lib-2.0-20261014.120000-1.jar")
	stale := `<metadata><groupId>org.example</groupId><artifactId>x</artifactId><versioning><versions><version>9.9</version></versions></versioning></metadata>`
	for _, d := range []string{"org/example/app", "org/example/lib", "org/example/app/1.0-SNAPSHOT", "org/example/lib/2.0-SNAPSHOT"} {
		e.seed(t, "snapshots", d+"/maven-metadata.xml", stale)
	}
	body := func(d string) string { return e.do("GET", "snapshots", d+"/maven-metadata.xml", "").Body.String() }
	res, err := e.h.RebuildMetadataScope(ctx, e.repos["snapshots"], 0, RebuildScope{GroupID: "org.example", ArtifactID: "app", NoCascade: true}, nil)
	if err != nil || res.GAs != 1 || res.Snapshots != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if strings.Contains(body("org/example/app"), "9.9") || !strings.Contains(body("org/example/lib"), "9.9") || !strings.Contains(body("org/example/app/1.0-SNAPSHOT"), "9.9") {
		t.Fatal("only the GA document of app is rebuilt")
	}
	res, err = e.h.RebuildMetadataScope(ctx, e.repos["snapshots"], 0, RebuildScope{GroupID: "org.example", ArtifactID: "lib", BaseVersion: "2.0-SNAPSHOT"}, nil)
	if err != nil || res.GAs != 0 || res.Snapshots != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(body("org/example/lib"), "9.9") || strings.Contains(body("org/example/lib/2.0-SNAPSHOT"), "9.9") {
		t.Fatal("only the version document of lib 2.0-SNAPSHOT is rebuilt")
	}
}

// A permissive proxy keeps what it cached: rebuild-metadata does not purge it.
func TestPurgeUnfetchableKeepsPermissive(t *testing.T) {
	e := newEnv(t)
	rp := *e.repos["central"]
	rp.LayoutPolicy = "permissive"
	e.seed(t, "central", "docs/readme.txt", "x")
	if n, err := e.h.PurgeUnfetchable(context.Background(), &rp, nil); err != nil || n != 0 {
		t.Fatalf("permissive: %d %v", n, err)
	}
	if n, err := e.h.PurgeUnfetchable(context.Background(), e.repos["central"], nil); err != nil || n != 1 {
		t.Fatalf("strict: %d %v", n, err)
	}
}
