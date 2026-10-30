package maven

import (
	"testing"

	"commandcenter/internal/repo"
)

func TestParsePath(t *testing.T) {
	cases := []struct {
		in   string
		hash string
		meta bool
		c    *Coordinates
	}{
		{"org/example/app/1.0/app-1.0.jar", "", false,
			&Coordinates{GroupID: "org.example", ArtifactID: "app", BaseVersion: "1.0", Version: "1.0", Extension: "jar"}},
		{"org/example/app/1.0/app-1.0-sources.jar.sha1", "sha1", false,
			&Coordinates{GroupID: "org.example", ArtifactID: "app", BaseVersion: "1.0", Version: "1.0", Classifier: "sources", Extension: "jar"}},
		{"org/example/app/1.0/app-1.0.tar.gz", "", false,
			&Coordinates{GroupID: "org.example", ArtifactID: "app", BaseVersion: "1.0", Version: "1.0", Extension: "tar.gz"}},
		{"org/example/app/1.0-SNAPSHOT/app-1.0-20261014.120000-3.pom", "", false,
			&Coordinates{GroupID: "org.example", ArtifactID: "app", BaseVersion: "1.0-SNAPSHOT", Version: "1.0-20261014.120000-3", Extension: "pom", Snapshot: true}},
		{"org/example/app/1.0-SNAPSHOT/app-1.0-SNAPSHOT.jar", "", false,
			&Coordinates{GroupID: "org.example", ArtifactID: "app", BaseVersion: "1.0-SNAPSHOT", Version: "1.0-SNAPSHOT", Extension: "jar", Snapshot: true}},
		{"org/example/app/maven-metadata.xml", "", true, nil},
		{"org/example/app/maven-metadata.xml.sha256", "sha256", true, nil},
		{"org/example/app/1.0/other-1.0.jar", "", false, nil}, // file does not match artifactId
		{"org/example/app/1.0/app-1.0.1.jar", "", false, nil}, // file version 1.0.1 is not directory version 1.0
		// The extension is what follows the last dot (tar.* kept whole), so a
		// classifier may hold dots; an extension may start with a digit.
		{"org/example/app/1.0/app-1.0-linux.x86_64.zip", "", false,
			&Coordinates{GroupID: "org.example", ArtifactID: "app", BaseVersion: "1.0", Version: "1.0", Classifier: "linux.x86_64", Extension: "zip"}},
		{"org/example/app/1.0/app-1.0-bin.tar.gz", "", false,
			&Coordinates{GroupID: "org.example", ArtifactID: "app", BaseVersion: "1.0", Version: "1.0", Classifier: "bin", Extension: "tar.gz"}},
		{"org/example/app/1.0/app-1.0.7z", "", false,
			&Coordinates{GroupID: "org.example", ArtifactID: "app", BaseVersion: "1.0", Version: "1.0", Extension: "7z"}},
		{"org/example/app/1.0/app-1.0-linux.x86_64.zip.asc", "", false,
			&Coordinates{GroupID: "org.example", ArtifactID: "app", BaseVersion: "1.0", Version: "1.0", Classifier: "linux.x86_64", Extension: "zip.asc"}},
		{"short/x.jar", "", false, nil},
	}
	for _, c := range cases {
		p, err := ParsePath(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if p.Hash != c.hash || p.Metadata != c.meta {
			t.Errorf("%s: hash=%q meta=%v", c.in, p.Hash, p.Metadata)
		}
		switch {
		case c.c == nil && p.Coords != nil:
			t.Errorf("%s: want no coords, got %+v", c.in, *p.Coords)
		case c.c != nil && (p.Coords == nil || *p.Coords != *c.c):
			t.Errorf("%s: coords = %+v, want %+v", c.in, p.Coords, *c.c)
		}
	}
}

func TestParsePathRejectsUnsafe(t *testing.T) {
	for _, in := range []string{"", "/abs", "a/../b", "a//b", `a\b`, "./a"} {
		if _, err := ParsePath(in); repo.StatusOf(err) != 400 {
			t.Errorf("%q: err = %v", in, err)
		}
	}
}

func TestParsePathRejectsControlAndQueryChars(t *testing.T) {
	for _, in := range []string{
		"org/example/odd/1.0/odd-1.0.jar\x00.pom",
		"org/example/odd/1.0/odd-1.0.jar\x01",
		"org/example/odd/1.0/odd-1.0.jar\x7f",
		"org/example/odd/1.0/odd-1.0.jar\n",
		"org/example/odd/1.0/odd-1.0.jar?q=1",
		"org/example/odd/1.0/odd-1.0.jar#frag",
		"org/example/odd/1.0/odd-1.0.jar;jsessionid=1",
		"org/ex?ample/odd/1.0/odd-1.0.jar",
		"org/example/odd/1.0;x/odd-1.0.jar",
		"org/example/odd/maven-metadata.xml#x",
		"org/example/caf\xe9/1.0/caf\xe9-1.0.jar", // not UTF-8
	} {
		if err := validateWritePath(in); repo.StatusOf(err) != 400 {
			t.Errorf("%q: err = %v", in, err)
		}
		// Reads keep parsing such names: legacy assets stay reachable.
		if _, err := ParsePath(in); err != nil {
			t.Errorf("ParsePath(%q) must accept legacy names: %v", in, err)
		}
	}
	if err := validateWritePath("org/example/app/1.0/app-1.0-sources.jar"); err != nil {
		t.Errorf("valid path rejected: %v", err)
	}
}

func TestContentType(t *testing.T) {
	cases := map[string]string{
		"a.pom": "application/xml", "maven-metadata.xml": "application/xml", "a.jar": "application/java-archive",
		"a.jar.sha1": "text/plain", "a.asc": "application/pgp-signature", "a.jar.asc": "application/pgp-signature", "a.zip": "application/zip", "a.bin": "application/octet-stream",
	}
	for in, want := range cases {
		if got := ContentType(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
