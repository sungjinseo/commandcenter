package terraform

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/pgp"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
	"commandcenter/internal/repo/repotest"
)

const ccBase = "http://cc.example"

func tgzBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// publicKey is the test key's public block and its id.
func publicKey(t *testing.T) (string, string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "pgp", "testdata", "rsa2048.asc"))
	if err != nil {
		t.Fatal(err)
	}
	k, err := pgp.ParsePrivateKey(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return k.PublicKey(), k.KeyID()
}

type env struct {
	h       *Handler
	repos   map[string]*repo.Repository
	sink    *repotest.Sink
	up      string // the upstream registry
	arc     string // the archive and release host
	hits    atomic.Int32
	down    atomic.Bool  // the registry answers 503
	regAuth atomic.Value // the last Authorization the registry saw
	arcAuth atomic.Value // any Authorization the archive host saw
	files   map[string][]byte
	now     time.Time
}

// upstream is a fake registry.terraform.io; its archive and release URLs
// point at archives, a fake GitHub and releases.hashicorp.com.
func (e *env) upstream(w http.ResponseWriter, r *http.Request) {
	e.regAuth.Store(r.Header.Get("Authorization"))
	if e.down.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	j := func(v string) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, v)
	}
	rel := e.arc + "/releases/"
	switch p := r.URL.Path; p {
	case "/.well-known/terraform.json":
		j(`{"modules.v1":"/v1/modules/","providers.v1":"/v1/providers/"}`)
	case "/v1/modules/hashicorp/consul/aws/versions":
		j(`{"modules":[{"source":"hashicorp/consul/aws","versions":[{"version":"0.11.0"},{"version":"0.10.0"}]}]}`)
	case "/v1/modules/hashicorp/consul/aws/0.11.0/download":
		w.Header().Set("X-Terraform-Get", e.arc+"/archive/consul-0.11.0.tar.gz//*?archive=tgz")
		w.WriteHeader(204)
	case "/v1/modules/hashicorp/consul/aws/0.10.0/download":
		w.Header().Set("X-Terraform-Get", "git::https://github.com/hashicorp/terraform-aws-consul?ref=v0.10.0")
		w.WriteHeader(204)
	case "/v1/modules/acme/demo/aws/versions": // also hosted: the group's hosted member wins
		j(`{"modules":[{"source":"acme/demo/aws","versions":[{"version":"9.0.0"}]}]}`)
	case "/v1/providers/hashicorp/random/versions":
		j(`{"versions":[{"version":"3.6.0","protocols":["5.0"],"platforms":[{"os":"linux","arch":"amd64"},{"os":"darwin","arch":"arm64"}]}]}`)
	case "/v1/providers/hashicorp/random/3.6.0/download/linux/amd64", "/v1/providers/hashicorp/random/3.6.0/download/darwin/arm64":
		os, arch := "linux", "amd64"
		sum := sha256hex(e.files["terraform-provider-random_3.6.0_linux_amd64.zip"])
		if strings.HasSuffix(p, "darwin/arm64") {
			os, arch, sum = "darwin", "arm64", strings.Repeat("0", 64) // a wrong shasum
		}
		file := "terraform-provider-random_3.6.0_" + os + "_" + arch + ".zip"
		// signed URLs (a query) the proxy does not keep in its own names
		j(`{"protocols":["5.0"],"os":"` + os + `","arch":"` + arch + `","filename":"` + file + `","download_url":"` + rel + file + `?sig=1` +
			`","shasums_url":"` + rel + `terraform-provider-random_3.6.0_SHA256SUMS?sig=2","shasums_signature_url":"` + rel + `terraform-provider-random_3.6.0_SHA256SUMS.sig","shasum":"` + sum +
			`","signing_keys":{"gpg_public_keys":[{"key_id":"34365D9472D7468F","ascii_armor":"-----BEGIN PGP PUBLIC KEY BLOCK-----"}]}}`)
	default:
		http.NotFound(w, r)
	}
}

// archives serves e.files under /archive/ and /releases/.
func (e *env) archives(w http.ResponseWriter, r *http.Request) {
	if a := r.Header.Get("Authorization"); a != "" {
		e.arcAuth.Store(a)
	}
	var b []byte
	switch p := r.URL.Path; {
	case strings.HasPrefix(p, "/archive/"):
		b = e.files[strings.TrimPrefix(p, "/archive/")]
	case strings.HasPrefix(p, "/releases/"):
		b = e.files[strings.TrimPrefix(p, "/releases/")]
	}
	if b == nil {
		http.NotFound(w, r)
		return
	}
	e.hits.Add(1)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(b)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), files: map[string][]byte{}}
	e.files["consul-0.11.0.tar.gz"] = tgzBytes(t, map[string]string{"terraform-aws-consul-0.11.0/main.tf": "resource {}", "terraform-aws-consul-0.11.0/README.md": "# Consul\n"})
	e.files["terraform-provider-random_3.6.0_linux_amd64.zip"] = zipBytes(t, map[string]string{"terraform-provider-random_v3.6.0": "ELF"})
	e.files["terraform-provider-random_3.6.0_darwin_arm64.zip"] = zipBytes(t, map[string]string{"terraform-provider-random_v3.6.0": "MACHO"})
	e.files["terraform-provider-random_3.6.0_SHA256SUMS"] = []byte(sha256hex(e.files["terraform-provider-random_3.6.0_linux_amd64.zip"]) + "  terraform-provider-random_3.6.0_linux_amd64.zip\n")
	e.files["terraform-provider-random_3.6.0_SHA256SUMS.sig"] = []byte("SIG")
	ts := httptest.NewServer(http.HandlerFunc(e.upstream))
	t.Cleanup(ts.Close)
	arc := httptest.NewServer(http.HandlerFunc(e.archives))
	t.Cleanup(arc.Close)
	e.up, e.arc = ts.URL, arc.URL
	dir := t.TempDir()
	b, _ := blob.Open(dir)
	db, err := meta.Open(metatest.URL(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.sink = &repotest.Sink{}
	store := &repo.Store{Blobs: map[string]*blob.Store{repo.DefaultStore: b}, DB: db, Now: func() time.Time { return e.now }, Events: e.sink}
	pub, _ := publicKey(t)
	cfg := &config.Config{AnonymousRead: true, Repositories: []config.Repository{
		{Name: "tf-hosted", Format: "terraform", Type: "hosted", WritePolicy: "allow_once", GPGPublicKey: pub},
		{Name: "tf-allow", Format: "terraform", Type: "hosted", WritePolicy: "allow"},
		{Name: "tf-ro", Format: "terraform", Type: "hosted", WritePolicy: "deny"},
		{Name: "tf-proxy", Format: "terraform", Type: "proxy", Remote: ts.URL + "/", RemoteAuth: &config.RemoteAuth{BearerToken: "up-token"},
			MetadataMaxAge: config.Duration(time.Hour), ContentMaxAge: config.Never, NegativeCacheTTL: config.Duration(time.Minute)},
		{Name: "tf-all", Format: "terraform", Type: "group", Members: []string{"tf-hosted", "tf-proxy"}, WriteMember: "tf-hosted"},
		{Name: "tf-proxy-first", Format: "terraform", Type: "group", Members: []string{"tf-proxy", "tf-hosted"}},
	}}
	f := proxy.New(store, ts.Client(), 4)
	f.Now = func() time.Time { return e.now }
	e.h = New(store, f, auth.New(cfg, db), func() string { return ccBase + "/" })
	e.h.Now = func() time.Time { return e.now }
	e.repos = repo.Build(cfg)
	return e
}

func (e *env) do(method, repoName, path string, body []byte) *httptest.ResponseRecorder {
	return e.doAs(auth.Principal{}, method, repoName, path, body)
}

func (e *env) doAs(p auth.Principal, method, repoName, path string, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repoName+"/"+path, bytes.NewReader(body))
	r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	path, _, _ = strings.Cut(path, "?")
	e.h.Serve(w, r, e.repos[repoName], path)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if w.Code != 200 || w.Header().Get("Content-Type") != ctJSON {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
}

type moduleVersions struct {
	Modules []struct {
		Source   string
		Versions []struct{ Version string }
	}
}

func (m moduleVersions) list() string {
	var out []string
	for _, v := range m.Modules[0].Versions {
		out = append(out, v.Version)
	}
	return strings.Join(out, ",")
}

type providerVersions struct {
	Versions []struct {
		Version   string
		Protocols []string
		Platforms []struct{ OS, Arch string }
	}
}

type download struct {
	Protocols                                                []string
	OS, Arch, Filename, DownloadURL, ShasumsURL, SigURL, Sum string
	Keys                                                     struct {
		GPG []struct {
			KeyID string `json:"key_id"`
			Armor string `json:"ascii_armor"`
		} `json:"gpg_public_keys"`
	} `json:"signing_keys"`
}

func (d *download) UnmarshalJSON(b []byte) error {
	type raw struct {
		Protocols []string `json:"protocols"`
		OS        string   `json:"os"`
		Arch      string   `json:"arch"`
		Filename  string   `json:"filename"`
		Download  string   `json:"download_url"`
		Shasums   string   `json:"shasums_url"`
		Sig       string   `json:"shasums_signature_url"`
		Shasum    string   `json:"shasum"`
		Keys      struct {
			GPG []struct {
				KeyID string `json:"key_id"`
				Armor string `json:"ascii_armor"`
			} `json:"gpg_public_keys"`
		} `json:"signing_keys"`
	}
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	d.Protocols, d.OS, d.Arch, d.Filename, d.DownloadURL, d.ShasumsURL, d.SigURL, d.Sum = r.Protocols, r.OS, r.Arch, r.Filename, r.Download, r.Shasums, r.Sig, r.Shasum
	d.Keys = r.Keys
	return nil
}

func TestRoutes(t *testing.T) {
	for _, c := range []struct {
		path string
		want Route
		bad  bool
	}{
		{".well-known/terraform.json", Route{Kind: KindDiscovery}, false},
		{"v1/modules/acme/demo/aws/versions", Route{Kind: KindModuleVersions, Name: "acme/demo/aws"}, false},
		{"v1/modules/acme/demo/aws/1.2.0/download", Route{Kind: KindModuleDownload, Name: "acme/demo/aws", Version: "1.2.0"}, false},
		{"v1/modules/acme/demo/aws/1.2.0", Route{Kind: KindModuleUpload, Name: "acme/demo/aws", Version: "1.2.0"}, false},
		{"modules/acme/demo/aws/1.2.0-rc.1.tgz", Route{Kind: KindModuleArchive, Name: "acme/demo/aws", Version: "1.2.0-rc.1", Ext: "tgz"}, false},
		{"v1/providers/acme/demo/versions", Route{Kind: KindProviderVersions, Name: "acme/demo"}, false},
		{"v1/providers/acme/demo/2.0.0/download/linux/amd64", Route{Kind: KindProviderDownload, Name: "acme/demo", Version: "2.0.0", OS: "linux", Arch: "amd64"}, false},
		{"v1/providers/acme/demo/2.0.0/linux/amd64", Route{Kind: KindProviderUpload, Name: "acme/demo", Version: "2.0.0", Part: "zip", OS: "linux", Arch: "amd64"}, false},
		{"v1/providers/acme/demo/2.0.0/shasums.sig", Route{Kind: KindProviderUpload, Name: "acme/demo", Version: "2.0.0", Part: "sig"}, false},
		{"v1/providers/acme/demo/2.0.0", Route{Kind: KindProviderUpload, Name: "acme/demo", Version: "2.0.0"}, false},
		{"providers/acme/demo/2.0.0/terraform-provider-demo_2.0.0_SHA256SUMS", Route{Kind: KindProviderFile, Name: "acme/demo", Version: "2.0.0", File: "terraform-provider-demo_2.0.0_SHA256SUMS"}, false},
		{"v1/modules/acme/demo/aws/v1.2.0/download", Route{}, true},
		{"v1/modules/../demo/aws/versions", Route{}, true},
		{"modules/acme/demo/aws/1.2.0.tar", Route{}, true},
		{"providers/acme/demo/2.0.0/../x", Route{}, true},
	} {
		got, err := ParseRoute(c.path)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("%s: %+v %v", c.path, got, err)
		}
	}
}

func TestDiscovery(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"tf-hosted", "tf-proxy", "tf-all"} {
		var d map[string]string
		decode(t, e.do("GET", name, discoveryPath, nil), &d)
		if d["modules.v1"] != ccBase+"/repository/"+name+"/v1/modules/" || d["providers.v1"] != ccBase+"/repository/"+name+"/v1/providers/" {
			t.Fatalf("%s: %v", name, d)
		}
	}
}

func TestRootDiscovery(t *testing.T) {
	sorted := []*repo.Repository{
		{Repository: config.Repository{Name: "a-maven", Format: "maven"}},
		{Repository: config.Repository{Name: "b-tf", Format: "terraform"}},
		{Repository: config.Repository{Name: "c-tf", Format: "terraform"}},
	}
	for def, want := range map[string]string{"": "b-tf", "c-tf": "c-tf", "a-maven": "b-tf", "gone": "b-tf"} {
		if got := string(RootDiscovery(sorted, def, "https://cc.example")); got != string(DiscoveryDoc("https://cc.example/repository/"+want)) {
			t.Errorf("%q: %s", def, got)
		}
	}
	if got := string(RootDiscovery(sorted[2:], "", "")); got != `{"modules.v1":"/repository/c-tf/v1/modules/","providers.v1":"/repository/c-tf/v1/providers/"}`+"\n" {
		t.Errorf("no base URL: %s", got)
	}
	if RootDiscovery(sorted[:1], "a-maven", "") != nil {
		t.Error("a document without a terraform repository")
	}
}

func TestFirstParagraph(t *testing.T) {
	for readme, want := range map[string]string{
		"# AWS VPC\n\n[![CI](x)](y)\n\nCreates VPC\nresources.\n\nMore.": "Creates VPC resources.",
		"<!-- BEGIN -->\nPlain text first\n# Later":                      "Plain text first",
		"\n# Only a title\n\n## Usage\n":                                 "Only a title",
		"":                                                               "",
	} {
		if got := firstParagraph(readme); got != want {
			t.Errorf("%q: %q", readme, got)
		}
	}
}

func TestHostedModules(t *testing.T) {
	e := newEnv(t)
	root := tgzBytes(t, map[string]string{"main.tf": "x", "README.md": "\n# Demo module\n\nText\n"})
	nested := zipBytes(t, map[string]string{"demo-2.0.0/main.tf": "x", "demo-2.0.0/README.md": "Nested\n"})
	if w := e.do("PUT", "tf-hosted", "v1/modules/acme/demo/aws/1.0.0", root); w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "tf-hosted", "modules/acme/demo/aws/2.0.0.zip", nested); w.Code != 201 { // the path form
		t.Fatalf("path upload: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "tf-hosted", "v1/modules/acme/demo/aws/1.1.0-rc.1", root); w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	for name, c := range map[string]struct {
		path string
		body []byte
		want int
	}{
		"no tf":          {"v1/modules/acme/bad/aws/1.0.0", tgzBytes(t, map[string]string{"README.md": "x"}), 400},
		"two dirs":       {"v1/modules/acme/bad/aws/1.0.0", zipBytes(t, map[string]string{"a/main.tf": "x", "b/main.tf": "x"}), 400},
		"not an archive": {"v1/modules/acme/bad/aws/1.0.0", []byte("hello"), 400},
		"wrong ext":      {"modules/acme/bad/aws/1.0.0.tgz", nested, 400},
		"empty":          {"v1/modules/acme/bad/aws/1.0.0", nil, 400},
		"same again":     {"v1/modules/acme/demo/aws/1.0.0", root, 201}, // a retry: allow_once refuses only other content
		"other content":  {"v1/modules/acme/demo/aws/1.0.0", tgzBytes(t, map[string]string{"main.tf": "changed"}), 409},
	} {
		if w := e.do("PUT", "tf-hosted", c.path, c.body); w.Code != c.want {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	var mv moduleVersions
	decode(t, e.do("GET", "tf-hosted", "v1/modules/acme/demo/aws/versions", nil), &mv)
	if mv.Modules[0].Source != "acme/demo/aws" || mv.list() != "2.0.0,1.1.0-rc.1,1.0.0" {
		t.Fatalf("versions: %+v", mv)
	}
	w := e.do("GET", "tf-hosted", "v1/modules/acme/demo/aws/1.0.0/download", nil)
	if w.Code != 204 || w.Header().Get("X-Terraform-Get") != ccBase+"/repository/tf-hosted/modules/acme/demo/aws/1.0.0.tgz?archive=tgz" {
		t.Fatalf("download: %d %q", w.Code, w.Header().Get("X-Terraform-Get"))
	}
	w = e.do("GET", "tf-hosted", "v1/modules/acme/demo/aws/2.0.0/download", nil)
	if w.Code != 204 || w.Header().Get("X-Terraform-Get") != ccBase+"/repository/tf-hosted/modules/acme/demo/aws/2.0.0.zip//*?archive=zip" {
		t.Fatalf("nested download: %d %q", w.Code, w.Header().Get("X-Terraform-Get"))
	}
	if w := e.do("GET", "tf-hosted", "modules/acme/demo/aws/1.0.0.tgz", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), root) || w.Header().Get("Content-Type") != ctTgz {
		t.Fatalf("archive: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w := e.do("GET", "tf-hosted", "modules/acme/demo/aws/1.0.0.zip", nil); w.Code != 404 {
		t.Fatalf("other extension: %d", w.Code)
	}
	for _, p := range []string{"v1/modules/acme/nope/aws/versions", "v1/modules/acme/demo/aws/3.0.0/download"} {
		if w := e.do("GET", "tf-hosted", p, nil); w.Code != 404 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	pkgs, _ := e.h.Store.DB.ListPackages(context.Background(), "tf-hosted", "acme/demo/aws")
	attrs := map[string]string{}
	for _, p := range pkgs {
		attrs[p.Version] = p.Attrs
	}
	if attrs["1.0.0"] != `{"ext":"tgz","description":"Text"}` || attrs["2.0.0"] != `{"ext":"zip","subdir":"*","description":"Nested"}` {
		t.Fatalf("attrs: %v", attrs)
	}
	if len(e.sink.Got) == 0 || e.sink.Got[0].Kind != "upload" || e.sink.Got[0].Subject != "acme/demo/aws@1.0.0" {
		t.Fatalf("events: %+v", e.sink.Got)
	}
}

func TestHostedProviders(t *testing.T) {
	e := newEnv(t)
	linux := e.files["terraform-provider-random_3.6.0_linux_amd64.zip"]
	put := func(path string, body []byte, want int) {
		t.Helper()
		if w := e.do("PUT", "tf-hosted", path, body); w.Code != want {
			t.Fatalf("PUT %s: %d %s", path, w.Code, w.Body)
		}
	}
	v := "v1/providers/acme/random/2.0.0/"
	put(v+"linux/amd64", linux, 201)
	put(v+"darwin/arm64", e.files["terraform-provider-random_3.6.0_darwin_arm64.zip"], 201)
	put(v+"shasums", []byte(sha256hex(linux)+"  terraform-provider-random_2.0.0_linux_amd64.zip\n"), 201)
	put(v+"shasums.sig", []byte("SIG"), 201)
	put(v+"manifest", []byte(`{"version":1,"metadata":{"protocol_versions":["5.0","6.0"]}}`), 201)
	put("v1/providers/acme/random/1.0.0/linux/amd64", linux, 201) // no manifest: the default protocols
	put(v+"linux/amd64", linux, 201)                              // the same file again
	put(v+"manifest", []byte(`{"version":1,"metadata":{"protocol_versions":["5.0","6.0"]}}`), 201)
	put(v+"linux/amd64", e.files["terraform-provider-random_3.6.0_darwin_arm64.zip"], 409) // allow_once
	put(v+"manifest", []byte(`{"version":1,"metadata":{"protocol_versions":["6.0"]}}`), 409)
	put(v+"manifest", []byte(`{"version":1,"metadata":{"protocol_versions":["six"]}}`), 400)
	put(v+"linux/arm64", []byte("not a zip"), 400)
	put(v+"shasums", []byte("garbage\n"), 400)
	put(v+"manifest", []byte(`{"version":1}`), 400)

	var pv providerVersions
	decode(t, e.do("GET", "tf-hosted", "v1/providers/acme/random/versions", nil), &pv)
	if len(pv.Versions) != 2 || pv.Versions[0].Version != "2.0.0" || strings.Join(pv.Versions[0].Protocols, ",") != "5.0,6.0" ||
		len(pv.Versions[0].Platforms) != 2 || pv.Versions[0].Platforms[0].OS != "darwin" || pv.Versions[0].Platforms[1].Arch != "amd64" ||
		strings.Join(pv.Versions[1].Protocols, ",") != "5.0" || len(pv.Versions[1].Platforms) != 1 {
		t.Fatalf("versions: %+v", pv)
	}
	var d download
	decode(t, e.do("GET", "tf-hosted", v+"download/linux/amd64", nil), &d)
	base := ccBase + "/repository/tf-hosted/providers/acme/random/2.0.0/"
	_, keyID := publicKey(t)
	if d.OS != "linux" || d.Arch != "amd64" || d.Filename != "terraform-provider-random_2.0.0_linux_amd64.zip" || d.DownloadURL != base+d.Filename ||
		d.ShasumsURL != base+"terraform-provider-random_2.0.0_SHA256SUMS" || d.SigURL != base+"terraform-provider-random_2.0.0_SHA256SUMS.sig" ||
		d.Sum != sha256hex(linux) || strings.Join(d.Protocols, ",") != "5.0,6.0" || len(d.Keys.GPG) != 1 || d.Keys.GPG[0].KeyID != keyID || !strings.HasPrefix(d.Keys.GPG[0].Armor, "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
		t.Fatalf("download: %+v", d)
	}
	if w := e.do("GET", "tf-hosted", v+"download/windows/amd64", nil); w.Code != 404 {
		t.Fatalf("missing platform: %d", w.Code)
	}
	for file, want := range map[string][]byte{
		"terraform-provider-random_2.0.0_linux_amd64.zip": linux,
		"terraform-provider-random_2.0.0_SHA256SUMS":      []byte(sha256hex(linux) + "  terraform-provider-random_2.0.0_linux_amd64.zip\n"),
		"terraform-provider-random_2.0.0_SHA256SUMS.sig":  []byte("SIG"),
	} {
		if w := e.do("GET", "tf-hosted", "providers/acme/random/2.0.0/"+file, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), want) {
			t.Fatalf("%s: %d", file, w.Code)
		}
	}
	// A repository without a key lists none (terraform then refuses the provider).
	put2 := e.do("PUT", "tf-allow", v+"linux/amd64", linux)
	if put2.Code != 201 {
		t.Fatal(put2.Code)
	}
	decode(t, e.do("GET", "tf-allow", v+"download/linux/amd64", nil), &d)
	if len(d.Keys.GPG) != 0 {
		t.Fatalf("keys without a key: %+v", d.Keys)
	}
}

func TestWritePolicyAndDelete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mod := tgzBytes(t, map[string]string{"main.tf": "x"})
	if w := e.do("PUT", "tf-ro", "v1/modules/acme/demo/aws/1.0.0", mod); w.Code != 400 {
		t.Fatalf("deny: %d", w.Code)
	}
	if w := e.do("PUT", "tf-proxy", "v1/modules/acme/demo/aws/1.0.0", mod); w.Code != 405 || w.Header().Get("Allow") != "" {
		t.Fatalf("proxy upload: %d %q", w.Code, w.Header().Get("Allow"))
	}
	if w := e.do("GET", "tf-hosted", "v1/modules/acme/demo/aws/1.0.0", nil); w.Code != 405 || w.Header().Get("Allow") != "PUT, DELETE" {
		t.Fatalf("GET upload path: %d %q", w.Code, w.Header().Get("Allow"))
	}
	// allow: an overwrite needs Edit; an archive of the other format replaces the old one.
	a, principals := repotest.Principals(t, e.h.Store.DB, "tf-allow", map[string][]string{"adder": {meta.ActionRead, meta.ActionAdd}, "editor": {meta.ActionRead, meta.ActionAdd, meta.ActionEdit}})
	e.h.Auth = a
	if w := e.doAs(principals["adder"], "PUT", "tf-allow", "v1/modules/acme/demo/aws/1.0.0", mod); w.Code != 201 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	zipMod := zipBytes(t, map[string]string{"main.tf": "y"})
	if w := e.doAs(principals["adder"], "PUT", "tf-allow", "v1/modules/acme/demo/aws/1.0.0", zipMod); w.Code != 403 {
		t.Fatalf("adder overwrite: %d", w.Code)
	}
	if w := e.doAs(principals["editor"], "PUT", "tf-allow", "v1/modules/acme/demo/aws/1.0.0", zipMod); w.Code != 201 {
		t.Fatalf("editor overwrite: %d %s", w.Code, w.Body)
	}
	assets, _ := e.h.Store.DB.ListAssets(ctx, "tf-allow", "modules/")
	if len(assets) != 1 || assets[0].Path != "modules/acme/demo/aws/1.0.0.zip" {
		t.Fatalf("assets after overwrite: %+v", assets)
	}
	// So does changing a provider's protocols.
	man := "v1/providers/acme/random/1.0.0/manifest"
	for _, c := range []struct {
		who, protocols string
		want           int
	}{{"adder", `["5.0"]`, 201}, {"adder", `["5.0"]`, 201}, {"adder", `["6.0"]`, 403}, {"editor", `["6.0"]`, 201}} {
		if w := e.doAs(principals[c.who], "PUT", "tf-allow", man, []byte(`{"version":1,"metadata":{"protocol_versions":`+c.protocols+`}}`)); w.Code != c.want {
			t.Fatalf("%s manifest %s: %d %s", c.who, c.protocols, w.Code, w.Body)
		}
	}
	// Group writes go to the writeMember; a group without one accepts none.
	if w := e.do("PUT", "tf-all", "v1/modules/acme/grp/aws/1.0.0", mod); w.Code != 201 {
		t.Fatalf("group upload: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "tf-proxy-first", "v1/modules/acme/grp/aws/1.0.0", mod); w.Code != 405 {
		t.Fatalf("group without writeMember: %d", w.Code)
	}
	// DELETE: the version's row and files.
	linux := e.files["terraform-provider-random_3.6.0_linux_amd64.zip"]
	e.do("PUT", "tf-hosted", "v1/providers/acme/random/2.0.0/linux/amd64", linux)
	e.do("PUT", "tf-hosted", "v1/providers/acme/random/2.0.0/shasums", []byte(sha256hex(linux)+"  x\n"))
	if w := e.do("DELETE", "tf-hosted", "v1/providers/acme/random/2.0.0/shasums", nil); w.Code != 404 {
		t.Fatalf("delete a part: %d", w.Code)
	}
	if w := e.do("DELETE", "tf-hosted", "v1/providers/acme/random/2.0.0", nil); w.Code != 204 {
		t.Fatalf("delete provider: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "tf-hosted", "v1/providers/acme/random/2.0.0", nil); w.Code != 404 {
		t.Fatalf("delete again: %d", w.Code)
	}
	if assets, _ := e.h.Store.DB.ListAssets(ctx, "tf-hosted", "providers/"); len(assets) != 0 {
		t.Fatalf("provider files left: %+v", assets)
	}
	if w := e.do("GET", "tf-hosted", "v1/providers/acme/random/versions", nil); w.Code != 404 {
		t.Fatalf("versions after delete: %d", w.Code)
	}
	// DeleteComponent (the UI and cleanup): a module; the wrong repository kind is refused.
	res, err := e.h.DeleteComponent(ctx, e.repos["tf-all"].WriteTarget, component.Component{Format: "terraform", Name: "acme/grp/aws", Version: "1.0.0"}, "admin")
	if err != nil || res.Assets != 1 {
		t.Fatalf("DeleteComponent: %+v %v", res, err)
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["tf-proxy"], component.Component{Format: "terraform", Name: "x/y/z", Version: "1.0.0"}, "admin"); err == nil {
		t.Fatal("proxy delete accepted")
	}
	if w := e.do("GET", "tf-hosted", "v1/modules/acme/grp/aws/versions", nil); w.Code != 404 {
		t.Fatalf("module after delete: %d", w.Code)
	}
	var kinds []string
	for _, ev := range e.sink.Got {
		kinds = append(kinds, ev.Kind+":"+ev.Subject)
	}
	if got := strings.Join(kinds, " "); !strings.Contains(got, "delete:acme/random@2.0.0") || !strings.Contains(got, "delete:acme/grp/aws@1.0.0") {
		t.Fatalf("events: %s", got)
	}
}

func TestProxyModules(t *testing.T) {
	e := newEnv(t)
	var mv moduleVersions
	decode(t, e.do("GET", "tf-proxy", "v1/modules/hashicorp/consul/aws/versions", nil), &mv)
	if mv.list() != "0.11.0,0.10.0" {
		t.Fatalf("versions: %+v", mv)
	}
	w := e.do("GET", "tf-proxy", "v1/modules/hashicorp/consul/aws/0.11.0/download", nil)
	want := ccBase + "/repository/tf-proxy/modules/hashicorp/consul/aws/0.11.0.tgz//*?archive=tgz"
	if w.Code != 204 || w.Header().Get("X-Terraform-Get") != want {
		t.Fatalf("download: %d %q", w.Code, w.Header().Get("X-Terraform-Get"))
	}
	if e.hits.Load() != 0 {
		t.Fatal("the archive was fetched before it was asked for")
	}
	// git:: sources pass through unchanged.
	w = e.do("GET", "tf-proxy", "v1/modules/hashicorp/consul/aws/0.10.0/download", nil)
	if w.Code != 204 || w.Header().Get("X-Terraform-Get") != "git::https://github.com/hashicorp/terraform-aws-consul?ref=v0.10.0" {
		t.Fatalf("git download: %d %q", w.Code, w.Header().Get("X-Terraform-Get"))
	}
	if w := e.do("GET", "tf-proxy", "modules/hashicorp/consul/aws/0.10.0.tgz", nil); w.Code != 404 {
		t.Fatalf("git archive: %d", w.Code)
	}
	for i := 0; i < 2; i++ {
		w := e.do("GET", "tf-proxy", "modules/hashicorp/consul/aws/0.11.0.tgz", nil)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), e.files["consul-0.11.0.tar.gz"]) {
			t.Fatalf("archive %d: %d", i, w.Code)
		}
	}
	if e.hits.Load() != 1 {
		t.Fatalf("archive fetched %d times", e.hits.Load())
	}
	if w := e.do("GET", "tf-proxy", "v1/modules/hashicorp/consul/aws/9.9.9/download", nil); w.Code != 404 {
		t.Fatalf("unknown version: %d", w.Code)
	}
	if w := e.do("GET", "tf-proxy", "v1/modules/acme/nope/aws/versions", nil); w.Code != 404 {
		t.Fatalf("unknown module: %d", w.Code)
	}
	// The remote's credentials go to the registry only, never to the archive host.
	if e.regAuth.Load() != "Bearer up-token" || e.arcAuth.Load() != nil {
		t.Fatalf("credentials: registry %v, archive host %v", e.regAuth.Load(), e.arcAuth.Load())
	}
	// The registry down: the last source of a version stands in, and the cached archive is served.
	e.down.Store(true)
	if w := e.do("GET", "tf-proxy", "v1/modules/hashicorp/consul/aws/0.11.0/download", nil); w.Code != 204 || w.Header().Get("X-Terraform-Get") != want {
		t.Fatalf("download while down: %d %q", w.Code, w.Header().Get("X-Terraform-Get"))
	}
	if w := e.do("GET", "tf-proxy", "modules/hashicorp/consul/aws/0.11.0.tgz", nil); w.Code != 200 {
		t.Fatalf("archive while down: %d", w.Code)
	}
	if w := e.do("GET", "tf-proxy", "v1/modules/hashicorp/consul/aws/0.9.0/download", nil); w.Code != 502 {
		t.Fatalf("never asked before, while down: %d", w.Code)
	}
}

func TestParseSource(t *testing.T) {
	rt := Route{Name: "a/b/c", Version: "1.0.0"}
	ep := "https://registry.example/v1/modules/a/b/c/1.0.0/download"
	for _, c := range []struct {
		raw  string
		want source
	}{
		{"https://github.com/a/b/archive/v1.0.0.tar.gz//*?archive=tgz", source{url: "https://github.com/a/b/archive/v1.0.0.tar.gz", key: "modules/a/b/c/1.0.0.tgz", ext: "tgz", subdir: "//*"}},
		{"https://api.github.com/repos/a/b/tarball/v1.0.0//*?archive=tar.gz", source{url: "https://api.github.com/repos/a/b/tarball/v1.0.0", key: "modules/a/b/c/1.0.0.tgz", ext: "tgz", subdir: "//*"}},
		{"https://x.example/m.zip?token=1", source{url: "https://x.example/m.zip?token=1", key: "modules/a/b/c/1.0.0.zip", ext: "zip"}},
		{"/files/m.tgz", source{url: "https://registry.example/files/m.tgz", key: "modules/a/b/c/1.0.0.tgz", ext: "tgz"}},
		{"git::https://github.com/a/b?ref=v1", source{}},
		{"s3::https://s3.amazonaws.com/b/m.zip", source{}},
		{"https://x.example/m.tar.xz", source{}},
	} {
		got := parseSource(ep, c.raw, rt)
		got.raw = ""
		if got != c.want {
			t.Errorf("%s: %+v", c.raw, got)
		}
	}
}

func TestProxyProviders(t *testing.T) {
	e := newEnv(t)
	var pv providerVersions
	decode(t, e.do("GET", "tf-proxy", "v1/providers/hashicorp/random/versions", nil), &pv)
	if len(pv.Versions) != 1 || pv.Versions[0].Version != "3.6.0" || len(pv.Versions[0].Platforms) != 2 {
		t.Fatalf("versions: %+v", pv)
	}
	var d download
	decode(t, e.do("GET", "tf-proxy", "v1/providers/hashicorp/random/3.6.0/download/linux/amd64", nil), &d)
	base := ccBase + "/repository/tf-proxy/providers/hashicorp/random/3.6.0/"
	linux := e.files["terraform-provider-random_3.6.0_linux_amd64.zip"]
	if d.DownloadURL != base+"terraform-provider-random_3.6.0_linux_amd64.zip" || d.ShasumsURL != base+"terraform-provider-random_3.6.0_SHA256SUMS" ||
		d.SigURL != base+"terraform-provider-random_3.6.0_SHA256SUMS.sig" || d.Sum != sha256hex(linux) || d.Keys.GPG[0].KeyID != "34365D9472D7468F" {
		t.Fatalf("download: %+v", d)
	}
	for i := 0; i < 2; i++ {
		for file, want := range map[string][]byte{
			"terraform-provider-random_3.6.0_linux_amd64.zip": linux,
			"terraform-provider-random_3.6.0_SHA256SUMS":      e.files["terraform-provider-random_3.6.0_SHA256SUMS"],
			"terraform-provider-random_3.6.0_SHA256SUMS.sig":  []byte("SIG"),
		} {
			if w := e.do("GET", "tf-proxy", "providers/hashicorp/random/3.6.0/"+file, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), want) {
				t.Fatalf("%s: %d %s", file, w.Code, w.Body)
			}
		}
	}
	if e.hits.Load() != 3 {
		t.Fatalf("files fetched %d times", e.hits.Load())
	}
	// A zip whose bytes do not match the document's shasum is refused and not cached.
	if w := e.do("GET", "tf-proxy", "providers/hashicorp/random/3.6.0/terraform-provider-random_3.6.0_darwin_arm64.zip", nil); w.Code != 502 {
		t.Fatalf("bad shasum: %d %s", w.Code, w.Body)
	}
	if _, err := e.h.Store.Get(context.Background(), "tf-proxy", "providers/hashicorp/random/3.6.0/terraform-provider-random_3.6.0_darwin_arm64.zip"); err == nil {
		t.Fatal("a mismatching zip was cached")
	}
	if w := e.do("GET", "tf-proxy", "providers/hashicorp/random/3.6.0/terraform-provider-random_3.6.0_windows_amd64.zip", nil); w.Code != 404 {
		t.Fatalf("unknown platform: %d", w.Code)
	}
	if w := e.do("GET", "tf-proxy", "providers/hashicorp/random/9.0.0/terraform-provider-random_9.0.0_SHA256SUMS", nil); w.Code != 404 {
		t.Fatalf("sums without a download document: %d", w.Code)
	}
}

func TestGroup(t *testing.T) {
	e := newEnv(t)
	mod := tgzBytes(t, map[string]string{"main.tf": "x"})
	if w := e.do("PUT", "tf-hosted", "v1/modules/acme/demo/aws/1.0.0", mod); w.Code != 201 {
		t.Fatal(w.Code)
	}
	// hosted first: its versions, not the upstream's 9.0.0.
	var mv moduleVersions
	decode(t, e.do("GET", "tf-all", "v1/modules/acme/demo/aws/versions", nil), &mv)
	if mv.list() != "1.0.0" {
		t.Fatalf("owner rule: %+v", mv)
	}
	if w := e.do("GET", "tf-all", "v1/modules/acme/demo/aws/9.0.0/download", nil); w.Code != 404 {
		t.Fatalf("a later member's version: %d", w.Code)
	}
	// proxy first: the upstream wins.
	decode(t, e.do("GET", "tf-proxy-first", "v1/modules/acme/demo/aws/versions", nil), &mv)
	if mv.list() != "9.0.0" {
		t.Fatalf("proxy-first: %+v", mv)
	}
	// A proxied module through the group carries the group's URLs.
	w := e.do("GET", "tf-all", "v1/modules/hashicorp/consul/aws/0.11.0/download", nil)
	if w.Code != 204 || w.Header().Get("X-Terraform-Get") != ccBase+"/repository/tf-all/modules/hashicorp/consul/aws/0.11.0.tgz//*?archive=tgz" {
		t.Fatalf("group download: %d %q", w.Code, w.Header().Get("X-Terraform-Get"))
	}
	if w := e.do("GET", "tf-all", "modules/hashicorp/consul/aws/0.11.0.tgz", nil); w.Code != 200 {
		t.Fatalf("group archive: %d", w.Code)
	}
	if _, err := e.h.Store.Get(context.Background(), "tf-proxy", "v1/modules/hashicorp/consul/aws/versions"); err != nil {
		t.Fatalf("the owner lookup's version list is not cached as a module's: %v", err)
	}
	var d download
	decode(t, e.do("GET", "tf-all", "v1/providers/hashicorp/random/3.6.0/download/linux/amd64", nil), &d)
	if !strings.HasPrefix(d.DownloadURL, ccBase+"/repository/tf-all/providers/hashicorp/random/3.6.0/") {
		t.Fatalf("group provider: %+v", d)
	}
	if w := e.do("GET", "tf-all", "v1/providers/acme/nope/versions", nil); w.Code != 404 {
		t.Fatalf("unknown in group: %d", w.Code)
	}
	// An offline member is skipped.
	off := false
	e.repos["tf-hosted"].Online = &off
	decode(t, e.do("GET", "tf-all", "v1/modules/acme/demo/aws/versions", nil), &mv)
	if mv.list() != "9.0.0" {
		t.Fatalf("offline member: %+v", mv)
	}
}

// TestCleanup: a cleanup policy sees a module version as its archive and a
// provider version as its files, and DeleteComponent refuses a component
// that changed since the evaluation.
func TestCleanup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	linux := e.files["terraform-provider-random_3.6.0_linux_amd64.zip"]
	for path, body := range map[string][]byte{
		"v1/modules/acme/demo/aws/1.0.0":             tgzBytes(t, map[string]string{"main.tf": "x"}),
		"v1/providers/acme/random/2.0.0/linux/amd64": linux,
		"v1/providers/acme/random/2.0.0/shasums":     []byte(sha256hex(linux) + "  terraform-provider-random_2.0.0_linux_amd64.zip\n"),
	} {
		if w := e.do("PUT", "tf-hosted", path, body); w.Code != 201 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	e.now = e.now.Add(48 * time.Hour)
	ev := &cleanup.Evaluator{DB: e.h.Store.DB, Now: func() time.Time { return e.now }}
	pol := cleanup.Policy{Name: "p", Format: "terraform", Criteria: cleanup.Criteria{ComponentAgeDays: 1}}
	if err := pol.Validate(); err != nil {
		t.Fatal(err)
	}
	evaluate := func() []cleanup.Candidate {
		cands, _, _, _, err := ev.Evaluate(ctx, e.repos["tf-hosted"], pol, cleanup.DefaultScanBudget)
		if err != nil || len(cands) != 2 || cands[0].Label() != "acme/demo/aws@1.0.0" || len(cands[0].Assets) != 1 || cands[1].Label() != "acme/random@2.0.0" || len(cands[1].Assets) != 2 {
			t.Fatalf("candidates: %+v %v", cands, err)
		}
		return cands
	}
	cands := evaluate()
	zip := "providers/acme/random/2.0.0/terraform-provider-random_2.0.0_linux_amd64.zip"
	if err := e.h.Store.DB.FlushDownloads(ctx, []meta.DownloadDelta{{Repo: "tf-hosted", Path: zip, N: 1, Last: e.now}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DeleteComponent(ctx, e.repos["tf-hosted"], cands[1].Component, "system"); !errors.Is(err, cleanup.ErrChanged) {
		t.Fatalf("changed: %v", err)
	}
	for _, c := range evaluate() {
		if res, err := e.h.DeleteComponent(ctx, e.repos["tf-hosted"], c.Component, "system"); err != nil || res.Assets != len(c.Assets) {
			t.Fatalf("%s: %+v %v", c.Label(), res, err)
		}
	}
	for _, p := range []string{"v1/modules/acme/demo/aws/versions", "v1/providers/acme/random/versions", zip} {
		if w := e.do("GET", "tf-hosted", p, nil); w.Code != 404 {
			t.Fatalf("%s after cleanup: %d", p, w.Code)
		}
	}
}
