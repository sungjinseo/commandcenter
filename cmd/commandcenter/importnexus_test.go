package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/app"
	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
)

// fakeNexus is a Nexus 3 REST API v1 with hosted maven2, npm, pypi, raw, helm
// and docker repositories, a proxy and a go repository, one component per
// page, and asset downloads under /repository/. It counts downloads.
type fakeNexus struct {
	url   string
	repos []nexusRepo
	comps map[string][]nexusComponent
	files map[string][]byte // "/repository/<repo>/<path>"

	mu        sync.Mutex
	downloads int
}

func (n *fakeNexus) asset(repo, p string, data []byte) nexusAsset {
	n.files["/repository/"+repo+"/"+p] = data
	a := nexusAsset{DownloadURL: n.url + "/repository/" + repo + "/" + p, Path: p, FileSize: int64(len(data))}
	s1, s256, m5 := sha1.Sum(data), sha256.Sum256(data), md5.Sum(data)
	a.Checksum.SHA1, a.Checksum.SHA256, a.Checksum.MD5 = hex.EncodeToString(s1[:]), hex.EncodeToString(s256[:]), hex.EncodeToString(m5[:])
	return a
}

func (n *fakeNexus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if u, p, ok := r.BasicAuth(); !ok || u != "nx" || p != "nx-secret" {
		w.WriteHeader(401)
		return
	}
	switch {
	case r.URL.Path == "/service/rest/v1/repositories":
		json.NewEncoder(w).Encode(n.repos)
	case r.URL.Path == "/service/rest/v1/components":
		comps := n.comps[r.URL.Query().Get("repository")]
		i, _ := strconv.Atoi(r.URL.Query().Get("continuationToken"))
		page := map[string]any{"items": comps[i:min(i+1, len(comps))], "continuationToken": nil}
		if i+1 < len(comps) {
			page["continuationToken"] = strconv.Itoa(i + 1)
		}
		json.NewEncoder(w).Encode(page)
	case r.URL.Path == "/repository/npm-hosted/pkg":
		io.WriteString(w, `{"name":"pkg","dist-tags":{"latest":"1.0.0","next":"1.1.0"}}`)
	default:
		data, ok := n.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		n.mu.Lock()
		n.downloads++
		n.mu.Unlock()
		w.Write(data)
	}
}

// fakeRegistry is a read-only registry with one image, app:v1, an index of
// two manifests that each have a config and a layer.
type fakeRegistry struct {
	manifests map[string][]byte // tag or digest
	types     map[string]string
	blobs     map[string][]byte
	index     string // digest of app:v1
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func newFakeRegistry() *fakeRegistry {
	reg := &fakeRegistry{manifests: map[string][]byte{}, types: map[string]string{}, blobs: map[string][]byte{}}
	var children []map[string]any
	for _, arch := range []string{"amd64", "arm64"} {
		cfg, layer := []byte(`{"architecture":"`+arch+`","os":"linux"}`), []byte("layer-"+arch)
		reg.blobs[digestOf(cfg)], reg.blobs[digestOf(layer)] = cfg, layer
		m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
			"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": digestOf(cfg), "size": len(cfg)},
			"layers": []map[string]any{{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": digestOf(layer), "size": len(layer)}}})
		reg.manifests[digestOf(m)], reg.types[digestOf(m)] = m, "application/vnd.oci.image.manifest.v1+json"
		children = append(children, map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": digestOf(m), "size": len(m),
			"platform": map[string]string{"os": "linux", "architecture": arch}})
	}
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": children})
	reg.index = digestOf(idx)
	for _, ref := range []string{"v1", reg.index} {
		reg.manifests[ref], reg.types[ref] = idx, "application/vnd.oci.image.index.v1+json"
	}
	return reg
}

func (reg *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v2/app/tags/list":
		io.WriteString(w, `{"name":"app","tags":["v1"]}`)
	case strings.HasPrefix(r.URL.Path, "/v2/app/manifests/"):
		ref := strings.TrimPrefix(r.URL.Path, "/v2/app/manifests/")
		m, ok := reg.manifests[ref]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", reg.types[ref])
		w.Header().Set("Docker-Content-Digest", digestOf(m))
		w.Write(m)
	case strings.HasPrefix(r.URL.Path, "/v2/app/blobs/"):
		b, ok := reg.blobs[strings.TrimPrefix(r.URL.Path, "/v2/app/blobs/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var names []string
	for n := range files {
		names = append(names, n)
	}
	return tarGz(names, files)
}

func TestImportNexus(t *testing.T) {
	nx := &fakeNexus{comps: map[string][]nexusComponent{}, files: map[string][]byte{}}
	nxs := httptest.NewServer(nx)
	defer nxs.Close()
	nx.url = nxs.URL
	nx.repos = []nexusRepo{
		{"maven-hosted", "maven2", "hosted"}, {"maven-central", "maven2", "proxy"}, {"go-hosted", "go", "hosted"},
		{"npm-hosted", "npm", "hosted"}, {"pypi-hosted", "pypi", "hosted"}, {"raw-hosted", "raw", "hosted"},
		{"helm-hosted", "helm", "hosted"}, {"docker-hosted", "docker", "hosted"}, {"apt-hosted", "apt", "hosted"},
	}
	pom := func(v string) []byte {
		return []byte(`<project><modelVersion>4.0.0</modelVersion><groupId>org.example</groupId><artifactId>app</artifactId><version>` + v + `</version></project>`)
	}
	for _, v := range []string{"1.0", "1.1"} {
		dir := "org/example/app/" + v + "/app-" + v
		nx.comps["maven-hosted"] = append(nx.comps["maven-hosted"], nexusComponent{Name: "app", Version: v, Assets: []nexusAsset{
			nx.asset("maven-hosted", dir+".jar", []byte("jar "+v)), nx.asset("maven-hosted", dir+".pom", pom(v)),
			nx.asset("maven-hosted", dir+".jar.sha1", []byte("deadbeef")),
			nx.asset("maven-hosted", "org/example/app/maven-metadata.xml", []byte("<metadata/>")),
		}})
	}
	for _, v := range []string{"1.0.0", "1.1.0"} {
		pj, _ := json.Marshal(map[string]any{"name": "pkg", "version": v, "description": "demo"})
		nx.comps["npm-hosted"] = append(nx.comps["npm-hosted"], nexusComponent{Name: "pkg", Version: v, Assets: []nexusAsset{
			nx.asset("npm-hosted", "pkg/-/pkg-"+v+".tgz", tgz(t, map[string]string{"package/package.json": string(pj)}))}})
	}
	sdist := tgz(t, map[string]string{"demo-1.0/PKG-INFO": "Metadata-Version: 2.1\nName: demo\nVersion: 1.0\n"})
	nx.comps["pypi-hosted"] = []nexusComponent{{Name: "demo", Version: "1.0", Assets: []nexusAsset{nx.asset("pypi-hosted", "packages/demo/1.0/demo-1.0.tar.gz", sdist)}}}
	nx.comps["raw-hosted"] = []nexusComponent{{Name: "docs/readme.txt", Version: "", Assets: []nexusAsset{nx.asset("raw-hosted", "docs/readme.txt", []byte("hello raw"))}}}
	chart := tgz(t, map[string]string{"mychart/Chart.yaml": "apiVersion: v2\nname: mychart\nversion: 0.1.0\n"})
	nx.comps["helm-hosted"] = []nexusComponent{{Name: "mychart", Version: "0.1.0", Assets: []nexusAsset{nx.asset("helm-hosted", "mychart-0.1.0.tgz", chart)}}}
	nx.comps["docker-hosted"] = []nexusComponent{{Name: "app", Version: "v1"}}
	for _, v := range []string{"1.0", "1:2.0-1"} { // Nexus's pool layout, the epoch as it is
		nx.comps["apt-hosted"] = append(nx.comps["apt-hosted"], nexusComponent{Name: "hello", Version: v, Assets: []nexusAsset{
			nx.asset("apt-hosted", "pool/h/hello/hello_"+v+"_amd64.deb", debPackage("hello", v, "amd64"))}})
	}
	reg := newFakeRegistry()
	regs := httptest.NewServer(reg)
	defer regs.Close()

	// The target: a Command Center with matching repositories.
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	dataDir := t.TempDir()
	cfg, err := config.Parse([]byte(`
dataDir: ` + filepath.ToSlash(dataDir) + `
database: {url: ` + strconv.Quote(metatest.URL(t, dataDir)) + `}
anonymousRead: true
users:
  - { name: ci, passwordHash: "` + string(h) + `", roles: [deployer] }
roles:
  deployer: { write: ["*"] }
repositories:
  - { name: maven-releases, format: maven, type: hosted, writePolicy: allow_once }
  - { name: npm-hosted, format: npm, type: hosted }
  - { name: pypi-hosted, format: pypi, type: hosted }
  - { name: raw-hosted, format: raw, type: hosted }
  - { name: helm-hosted, format: helm, type: hosted }
  - { name: docker-hosted, format: docker, type: hosted }
  - { name: apt-hosted, format: apt, type: hosted, distribution: bookworm }
`))
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	_, token, err := a.Auth.CreateToken(context.Background(), "ci")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pwFile, tokFile := filepath.Join(dir, "nx.pw"), filepath.Join(dir, "cc.tok")
	os.WriteFile(pwFile, []byte("nx-secret\n"), 0o600)
	os.WriteFile(tokFile, []byte(token+"\n"), 0o600)
	get := func(p string, accept string) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+p, nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	base := []string{"import-nexus", "-from", nxs.URL, "-from-user", "nx", "-from-password-file", pwFile,
		"-to", ts.URL, "-to-user", "ci", "-to-token-file", tokFile, "-docker-from", regs.URL, "-concurrency", "3",
		"-repo", "maven-hosted=maven-releases", "-repo", "npm-hosted", "-repo", "pypi-hosted", "-repo", "raw-hosted",
		"-repo", "helm-hosted", "-repo", "docker-hosted", "-repo", "apt-hosted", "-repo", "maven-central", "-repo", "go-hosted", "-repo", "missing"}
	run := func(extra ...string) (string, error) {
		var out bytes.Buffer
		err := run(append(append([]string{}, base...), extra...), nil, &out)
		t.Log(out.String())
		return out.String(), err
	}

	// Dry run: everything listed, nothing written, nothing downloaded.
	out, err := run("-dry-run")
	if err == nil || !strings.Contains(err.Error(), "1 errors") { // the missing repository
		t.Fatalf("dry run: %v", err)
	}
	for _, want := range []string{"maven-hosted\tmaven\tapp\t1.0\t2\t", "would copy", "npm-hosted\tnpm\tpkg\t1.1.0\t1\t",
		"docker-hosted\tdocker\tapp\tv1\t7\t", "maven-central: skipped (proxy repository", "go-hosted: skipped (format go is not supported)",
		"missing: error: not found", "dry run: nothing was written"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q", want)
		}
	}
	if st, _, _ := get("/repository/maven-releases/org/example/app/1.0/app-1.0.jar", ""); st != 404 || nx.downloads != 0 {
		t.Fatalf("dry run wrote: %d, %d downloads", st, nx.downloads)
	}

	// The import.
	out, err = run()
	if err == nil || !strings.Contains(err.Error(), "1 errors") {
		t.Fatalf("import: %v", err)
	}
	if strings.Contains(out, "nx-secret") || strings.Contains(out, token) {
		t.Fatal("a secret was printed")
	}
	for _, want := range []string{"maven-hosted\tmaven\tapp\t1.0\t2\t", "maven-hosted\tmaven\tapp\t1.1\t2\t", "npm-hosted\tnpm\tpkg\t1.0.0\t1\t",
		"pypi-hosted\tpypi\tdemo\t1.0\t1\t", "raw-hosted\traw\tdocs/readme.txt\t\t1\t9\tok", "helm-hosted\thelm\tmychart\t0.1.0\t1\t",
		"docker-hosted\tdocker\tapp\tv1\t7\t", "maven-hosted -> maven-releases (maven): 2 copied, 0 skipped, 0 failed",
		"apt-hosted -> apt-hosted (apt): 2 copied, 0 skipped, 0 failed",
		"docker-hosted -> docker-hosted (docker): 1 copied, 0 skipped, 0 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if strings.Contains(out, "error:") && strings.Count(out, "error:") != 1 {
		t.Errorf("unexpected errors in output")
	}
	if st, b, _ := get("/repository/maven-releases/org/example/app/1.1/app-1.1.jar", ""); st != 200 || b != "jar 1.1" {
		t.Errorf("maven jar: %d %q", st, b)
	}
	if st, b, _ := get("/repository/maven-releases/org/example/app/maven-metadata.xml", ""); st != 200 || !strings.Contains(b, "<version>1.1</version>") || strings.Contains(b, "<metadata/>") {
		t.Errorf("maven metadata: %d %q", st, b)
	}
	if st, b, _ := get("/repository/maven-releases/org/example/app/1.0/app-1.0.jar.sha1", ""); st != 200 || b == "deadbeef" {
		t.Errorf("maven checksum must be generated: %d %q", st, b)
	}
	_, b, _ := get("/repository/npm-hosted/pkg", "")
	var packument struct {
		Versions map[string]struct {
			Description string `json:"description"`
		} `json:"versions"`
		DistTags map[string]string `json:"dist-tags"`
	}
	if err := json.Unmarshal([]byte(b), &packument); err != nil || len(packument.Versions) != 2 || packument.Versions["1.0.0"].Description != "demo" ||
		packument.DistTags["latest"] != "1.0.0" || packument.DistTags["next"] != "1.1.0" {
		t.Errorf("npm packument: %v %s", err, b)
	}
	if st, b, _ := get("/repository/npm-hosted/pkg/-/pkg-1.1.0.tgz", ""); st != 200 || !bytes.Equal([]byte(b), nx.files["/repository/npm-hosted/pkg/-/pkg-1.1.0.tgz"]) {
		t.Errorf("npm tarball: %d", st)
	}
	if st, b, _ := get("/repository/pypi-hosted/simple/demo/", ""); st != 200 || !strings.Contains(b, "demo-1.0.tar.gz") {
		t.Errorf("pypi simple: %d %q", st, b)
	}
	if st, b, _ := get("/repository/pypi-hosted/packages/demo/demo-1.0.tar.gz", ""); st != 200 || !bytes.Equal([]byte(b), sdist) {
		t.Errorf("pypi file: %d", st)
	}
	if st, b, _ := get("/repository/raw-hosted/docs/readme.txt", ""); st != 200 || b != "hello raw" {
		t.Errorf("raw: %d %q", st, b)
	}
	if st, b, _ := get("/repository/apt-hosted/dists/bookworm/main/binary-amd64/Packages", ""); st != 200 ||
		!strings.Contains(b, "Version: 1.0\n") || !strings.Contains(b, "Version: 1:2.0-1\n") || !strings.Contains(b, "pool/main/h/hello/hello_1%3a2.0-1_amd64.deb") {
		t.Errorf("apt Packages: %d %q", st, b)
	}
	if st, b, _ := get("/repository/apt-hosted/pool/h/hello/hello_1:2.0-1_amd64.deb", ""); st != 200 || b != string(nx.files["/repository/apt-hosted/pool/h/hello/hello_1:2.0-1_amd64.deb"]) {
		t.Errorf("apt package at its Nexus path: %d", st)
	}
	if st, b, _ := get("/repository/helm-hosted/index.yaml", ""); st != 200 || !strings.Contains(b, "mychart-0.1.0.tgz") {
		t.Errorf("helm index: %d %q", st, b)
	}
	if st, _, hdr := get("/v2/docker-hosted/app/manifests/v1", "application/vnd.oci.image.index.v1+json"); st != 200 || hdr.Get("Docker-Content-Digest") != reg.index {
		t.Errorf("docker index: %d %q", st, hdr.Get("Docker-Content-Digest"))
	}
	for ref, m := range reg.manifests {
		if ref == "v1" {
			continue
		}
		if st, b, _ := get("/v2/docker-hosted/app/manifests/"+ref, reg.types[ref]); st != 200 || b != string(m) {
			t.Errorf("docker manifest %s: %d", ref, st)
		}
	}
	for d, blob := range reg.blobs {
		if st, b, _ := get("/v2/docker-hosted/app/blobs/"+d, ""); st != 200 || b != string(blob) {
			t.Errorf("docker blob %s: %d", d, st)
		}
	}

	// A second run skips everything: nothing is downloaded again.
	downloads := nx.downloads
	out, err = run()
	if err == nil || !strings.Contains(err.Error(), "1 errors") {
		t.Fatalf("second run: %v", err)
	}
	if nx.downloads != downloads {
		t.Errorf("second run downloaded %d files", nx.downloads-downloads)
	}
	for _, want := range []string{"maven-hosted\tmaven\tapp\t1.0\t0\t0\tskipped", "docker-hosted\tdocker\tapp\tv1\t0\t0\tskipped",
		"npm-hosted -> npm-hosted (npm): 0 copied, 2 skipped, 0 failed", "pypi-hosted -> pypi-hosted (pypi): 0 copied, 1 skipped",
		"apt-hosted -> apt-hosted (apt): 0 copied, 2 skipped, 0 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("second run output lacks %q", want)
		}
	}
	if strings.Contains(out, "\tok") {
		t.Errorf("second run copied something:\n%s", out)
	}

	// Wrong Nexus password: exit 1 with a readable message, no secret in it.
	os.WriteFile(pwFile, []byte("wrong\n"), 0o600)
	_, err = run()
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "-from-user") || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("auth failure: %v", err)
	}
}

func TestImportNexusFlags(t *testing.T) {
	for _, args := range [][]string{
		{"import-nexus"},
		{"import-nexus", "-from", "http://nx", "-to", "http://cc"},
		{"import-nexus", "-from", "http://u:p@nx", "-to", "http://cc", "-all"},
		{"import-nexus", "-from", "http://nx", "-to", "http://cc", "-all", "-to-user", "ci"},
		{"import-nexus", "-from", "http://nx", "-to", "http://cc", "-repo", "a=b/c"},
	} {
		t.Setenv("COMMANDCENTER_TOKEN", "")
		if err := run(args, nil, &bytes.Buffer{}); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
}

// debPackage is a minimal .deb: debian-binary, control.tar.gz with
// ./control, an empty data.tar.gz.
func debPackage(pkg, version, arch string) []byte {
	var b bytes.Buffer
	b.WriteString("!<arch>\n")
	for _, m := range []struct {
		name string
		data []byte
	}{
		{"debian-binary", []byte("2.0\n")},
		{"control.tar.gz", tarGz([]string{"./control"}, map[string]string{"./control": "Package: " + pkg + "\nVersion: " + version + "\nArchitecture: " + arch + "\nMaintainer: QA <qa@example.test>\nDescription: test\n"})},
		{"data.tar.gz", tarGz(nil, nil)},
	} {
		fmt.Fprintf(&b, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", m.name, 0, 0, 0, "100644", len(m.data))
		b.Write(m.data)
		if len(m.data)%2 == 1 {
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

func tarGz(names []string, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg})
		tw.Write([]byte(files[n]))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}
