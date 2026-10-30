package ui

import "testing"

// Each Maven SNAPSHOT build is a component; a search for the base version
// lists its builds, baseVersion matches the base, and classifier and
// extension follow the build's file names.
func TestComponentsMavenSnapshotBuilds(t *testing.T) {
	e := newEnv(t, "http://x")
	d := "org/example/snap/1.0-SNAPSHOT/"
	for _, p := range []string{d + "snap-1.0-20261014.120000-1.jar", d + "snap-1.0-20261015.120000-2.jar", d + "snap-1.0-20261015.120000-2-linux.x86_64.zip"} {
		putAsset(t, e.db, "maven-releases", p, 1)
	}
	want := "maven-releases:org.example:snap:1.0-20261014.120000-1,maven-releases:org.example:snap:1.0-20261015.120000-2"
	for _, q := range []string{"name=snap&version=1.0-SNAPSHOT", "name=snap&baseVersion=1.0-SNAPSHOT"} {
		if got := names(searchComponents(t, e, q, "admin", 200)); got != want {
			t.Fatalf("%s: %s", q, got)
		}
	}
	if got := names(searchComponents(t, e, "name=snap&version=1.0-20261014.120000-1", "admin", 200)); got != "maven-releases:org.example:snap:1.0-20261014.120000-1" {
		t.Fatalf("one build: %s", got)
	}
	c := searchComponents(t, e, "name=snap&classifier=linux.x86_64&extension=zip", "admin", 200)
	if len(c.Components) != 1 || len(c.Components[0].Assets) != 1 || c.Components[0].Version != "1.0-20261015.120000-2" {
		t.Fatalf("classifier: %+v", c.Components)
	}
}
