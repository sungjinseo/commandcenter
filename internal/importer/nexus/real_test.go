package nexus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests decode responses recorded from a real Nexus Repository 3.97
// (testdata/real-*.json).

func realServer(t *testing.T) *httptest.Server {
	t.Helper()
	serve := func(file string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, err := os.ReadFile(filepath.Join("testdata", file))
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/repositories", serve("real-repositories.json"))
	mux.HandleFunc("/service/rest/v1/repositorySettings", serve("real-repository-settings.json"))
	mux.HandleFunc("/service/rest/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		f := map[string]string{"mvn-h": "maven", "npm-h": "npm", "pypi-h": "pypi", "cargo-h": "cargo"}[r.URL.Query().Get("repository")]
		if f == "" {
			w.Write([]byte(`{"items":[],"continuationToken":null}`))
			return
		}
		serve("real-assets-"+f+".json")(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRealSettingsDecode(t *testing.T) {
	c := newClient(t, realServer(t).URL)
	st, err := c.RepositorySettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]RepoSettings{}
	for _, s := range st {
		by[s.Name] = s
	}
	if len(by) != 13 {
		t.Fatalf("%d repositories: %v", len(by), by)
	}
	mvn := by["maven-releases"]
	if mvn.Format != "maven2" || mvn.Type != "hosted" || mvn.Storage.WritePolicy != "ALLOW_ONCE" || mvn.Maven == nil || mvn.Maven.VersionPolicy != "RELEASE" {
		t.Fatalf("maven-releases: %+v", mvn)
	}
	if s := by["maven-snapshots"]; s.Maven.VersionPolicy != "SNAPSHOT" || s.Storage.WritePolicy != "ALLOW" {
		t.Fatalf("maven-snapshots: %+v", s)
	}
	central := by["maven-central"]
	if central.Proxy == nil || central.Proxy.RemoteURL != "https://repo1.maven.org/maven2/" || central.Proxy.ContentMaxAge != -1 || central.Proxy.MetadataMaxAge != 1440 {
		t.Fatalf("maven-central proxy: %+v", central.Proxy)
	}
	if central.NegativeCache == nil || !central.NegativeCache.Enabled || central.NegativeCache.TimeToLive != 1440 {
		t.Fatalf("maven-central negativeCache: %+v", central.NegativeCache)
	}
	if central.HTTPClient == nil || central.HTTPClient.Authentication != nil {
		t.Fatalf("maven-central httpClient: %+v", central.HTTPClient)
	}
	if g := by["maven-public"]; g.Group == nil || strings.Join(g.Group.MemberNames, ",") != "maven-releases,maven-snapshots,maven-central" {
		t.Fatalf("maven-public: %+v", g.Group)
	}
	if s := by["nuget-hosted"]; s.Format != "nuget" || s.Type != "hosted" {
		t.Fatalf("nuget-hosted: %+v", s)
	}
}

func TestRealAssetsDecode(t *testing.T) {
	c := newClient(t, realServer(t).URL)
	get := func(repo string) []Asset {
		var out []Asset
		if err := c.Assets(context.Background(), repo, func(a Asset) error { out = append(out, a); return nil }); err != nil {
			t.Fatal(err)
		}
		return out
	}
	mvn := get("mvn-h")
	if len(mvn) != 2 {
		t.Fatalf("maven assets: %+v", mvn)
	}
	jar := mvn[0]
	// The real path has a leading '/'; the client strips it.
	if jar.Path != "com/ex/app/1.0/app-1.0.jar" || jar.Format != "maven2" || jar.Repository != "mvn-h" ||
		jar.Checksum.SHA256 != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" || jar.FileSize != 0 ||
		jar.ModTime().IsZero() || jar.Maven2 == nil || jar.Maven2.ArtifactID != "app" || jar.Maven2.GroupID != "com.ex" {
		t.Fatalf("jar: %+v", jar)
	}
	npm := get("npm-h")
	var tgz []Asset
	for _, a := range npm {
		if strings.HasPrefix(a.Path, "/") {
			t.Fatalf("leading slash kept: %q", a.Path)
		}
		if strings.HasSuffix(a.Path, ".tgz") {
			tgz = append(tgz, a)
		}
	}
	if len(npm) != 5 || len(tgz) != 3 || tgz[0].Path != "demo/-/demo-1.0.0.tgz" || tgz[0].Npm == nil || tgz[0].Npm.Name != "demo" || tgz[0].Npm.Version != "1.0.0" {
		t.Fatalf("npm: %+v", npm)
	}
	if npm[0].Path != "demo" || npm[0].Checksum.SHA256 != "" {
		t.Fatalf("npm packument asset: %+v", npm[0])
	}
	py := get("pypi-h")
	if len(py) != 4 || py[0].Path != "packages/demo-pkg/1.0.0/demo_pkg-1.0.0-py3-none-any.whl" || py[0].Pypi == nil || py[0].Pypi.Name != "demo-pkg" {
		t.Fatalf("pypi: %+v", py)
	}
	cg := get("cargo-h")
	if len(cg) != 2 || cg[0].Path != "crates/democrate/0.1.0/download" || cg[0].Cargo == nil || cg[0].Cargo.Name != "democrate" || cg[0].Cargo.Version != "0.1.0" {
		t.Fatalf("cargo: %+v", cg)
	}
	// downloadUrl paths still resolve against the source.
	if u := c.downloadURL(cg[0]); !strings.HasSuffix(u, "/repository/cargo-h/crates/democrate/0.1.0/download") {
		t.Fatalf("download url %q", u)
	}
}

// A repository missing from repositorySettings whose per-repository endpoint
// does not exist (an unsupported format) is reported from the listing only.
func TestSettingsFallbackNotFoundUsesListing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"name":"raw-h","format":"raw","type":"hosted","url":"u"}]`))
	})
	mux.HandleFunc("/service/rest/v1/repositorySettings", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	st, err := newClient(t, srv.URL).RepositorySettings(context.Background())
	if err != nil || len(st) != 1 || st[0].Name != "raw-h" || st[0].Format != "raw" || st[0].Type != "hosted" {
		t.Fatalf("%+v %v", st, err)
	}
}
