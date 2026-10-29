package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"mime/multipart"
	"regexp"
	"strings"
	"sync"
	"testing"

	"commandcenter/internal/component"
)

// hostedFormat is how the matrix writes and reads one hosted format: put
// uploads version i through a node, del is version i as a component (the
// UI's delete), index reads the derived document that lists the versions,
// and shows is what it holds for version i.
type hostedFormat struct {
	repo, def string
	put       func(t *testing.T, n *node, i int)
	del       func(i int) component.Component
	index     func(t *testing.T, n *node) string
	shows     func(i int) string
}

func v(i int) string { return fmt.Sprintf("1.%d.0", i) }

// tarGzOf is a .tar.gz of name, body pairs.
func tarGzOf(files ...string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(tarOf(files...))
	gz.Close()
	return buf.Bytes()
}

func tarOf(files ...string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i := 0; i+1 < len(files); i += 2 {
		tw.WriteHeader(&tar.Header{Name: files[i], Mode: 0o644, Size: int64(len(files[i+1])), Typeflag: tar.TypeReg})
		tw.Write([]byte(files[i+1]))
	}
	tw.Close()
	return buf.Bytes()
}

// zstdRaw is b as a zstd frame of raw blocks.
func zstdRaw(b []byte) []byte {
	out := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x38}
	for {
		n := min(len(b), 128<<10)
		h := n << 3
		if n == len(b) {
			h |= 1
		}
		out = append(append(out, byte(h), byte(h>>8), byte(h>>16)), b[:n]...)
		if b = b[n:]; len(b) == 0 {
			return out
		}
	}
}

// deb is a minimal .deb of pkg version (amd64).
func deb(pkg, version string) []byte {
	var buf bytes.Buffer
	buf.WriteString("!<arch>\n")
	member := func(name string, data []byte) {
		fmt.Fprintf(&buf, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, 0, 0, 0, "100644", len(data))
		buf.Write(data)
		if len(data)%2 == 1 {
			buf.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("control.tar.gz", tarGzOf("./control", "Package: "+pkg+"\nVersion: "+version+"\nArchitecture: amd64\nMaintainer: QA <qa@example.test>\nDescription: "+pkg+"\n"))
	member("data.tar.gz", tarGzOf("./usr/bin/"+pkg, "#!/bin/sh\n"))
	return buf.Bytes()
}

// gem is a minimal .gem of name version.
func gem(name, version string) []byte {
	gzipped := func(s string) string {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		gz.Write([]byte(s))
		gz.Close()
		return buf.String()
	}
	spec := "--- !ruby/object:Gem::Specification\nname: " + name + "\nversion: !ruby/object:Gem::Version\n  version: " + version +
		"\nplatform: ruby\nauthors:\n- QA\ndependencies: []\nfiles:\n- lib/" + name + ".rb\nrequire_paths:\n- lib\nsummary: A demo\n"
	return tarOf("metadata.gz", gzipped(spec), "data.tar.gz", gzipped(string(tarOf("lib/"+name+".rb", "puts 1\n"))),
		"checksums.yaml.gz", gzipped("---\n"))
}

// form is a multipart body of fields (name, value pairs) and one file.
func form(file, filename string, content []byte, fields ...string) ([]byte, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i := 0; i+1 < len(fields); i += 2 {
		mw.WriteField(fields[i], fields[i+1])
	}
	fw, _ := mw.CreateFormFile(file, filename)
	fw.Write(content)
	mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func hostedFormats() []hostedFormat {
	var dockerMu sync.Mutex
	dockerDigests := map[int]string{}
	get := func(path string) func(t *testing.T, n *node) string {
		return func(t *testing.T, n *node) string { return n.want(t, "GET", path, nil, ok(200)) }
	}
	return []hostedFormat{
		{repo: "maven-h", def: "format: maven",
			put: func(t *testing.T, n *node, i int) {
				n.want(t, "PUT", fmt.Sprintf("/repository/maven-h/org/ex/lib/%s/lib-%s.jar", v(i), v(i)), "jar "+v(i), ok(201))
			},
			del: func(i int) component.Component {
				return component.Component{Group: "org.ex", Name: "lib", Version: v(i)}
			},
			index: get("/repository/maven-h/org/ex/lib/maven-metadata.xml"),
			shows: func(i int) string { return "<version>" + v(i) + "</version>" }},
		{repo: "raw-h", def: "format: raw",
			put: func(t *testing.T, n *node, i int) {
				n.want(t, "PUT", "/repository/raw-h/dir/f-"+v(i)+".txt", v(i), ok(201))
			},
			del:   func(i int) component.Component { return component.Component{Name: "dir/f-" + v(i) + ".txt"} },
			index: get("/repository/raw-h/dir/"),
			shows: func(i int) string { return "f-" + v(i) + ".txt" }},
		{repo: "npm-h", def: "format: npm",
			put: func(t *testing.T, n *node, i int) {
				n.want(t, "PUT", "/repository/npm-h/demo", npmPublish("demo", v(i)), ok(200, 201), "Content-Type", "application/json")
			},
			del:   func(i int) component.Component { return component.Component{Name: "demo", Version: v(i)} },
			index: get("/repository/npm-h/demo"),
			shows: func(i int) string { return `"version":"` + v(i) + `"` }},
		{repo: "pypi-h", def: "format: pypi",
			put: func(t *testing.T, n *node, i int) {
				whl := "demo_pkg-" + v(i) + "-py3-none-any.whl"
				body, ct := form("content", whl, zipOf("demo_pkg-"+v(i)+".dist-info/METADATA", "Metadata-Version: 2.1\nName: demo-pkg\nVersion: "+v(i)+"\n"),
					":action", "file_upload", "protocol_version", "1", "name", "demo-pkg", "version", v(i), "filetype", "bdist_wheel")
				n.want(t, "POST", "/repository/pypi-h/", body, ok(200), "Content-Type", ct)
			},
			del:   func(i int) component.Component { return component.Component{Name: "demo-pkg", Version: v(i)} },
			index: get("/repository/pypi-h/simple/demo-pkg/"),
			shows: func(i int) string { return "demo_pkg-" + v(i) + "-py3-none-any.whl" }},
		{repo: "go-h", def: "format: go",
			put: func(t *testing.T, n *node, i int) {
				pre := "example.com/demo@v" + v(i) + "/"
				n.want(t, "PUT", "/repository/go-h/example.com/demo/@v/v"+v(i)+".zip", zipOf(pre+"go.mod", "module example.com/demo\n", pre+"demo.go", "package demo\n"), ok(201))
			},
			del: func(i int) component.Component {
				return component.Component{Name: "example.com/demo", Version: "v" + v(i)}
			},
			index: get("/repository/go-h/example.com/demo/@v/list"),
			shows: func(i int) string { return "v" + v(i) + "\n" }},
		{repo: "cargo-h", def: "format: cargo",
			put: func(t *testing.T, n *node, i int) {
				meta := `{"name":"demo","vers":"` + v(i) + `","deps":[],"features":{},"authors":[],"description":"d"}`
				crate := tarGzOf("demo-"+v(i)+"/Cargo.toml", "[package]\nname = \"demo\"\nversion = \""+v(i)+"\"\n", "demo-"+v(i)+"/src/lib.rs", "")
				var body bytes.Buffer
				binary.Write(&body, binary.LittleEndian, uint32(len(meta)))
				body.WriteString(meta)
				binary.Write(&body, binary.LittleEndian, uint32(len(crate)))
				body.Write(crate)
				n.want(t, "PUT", "/repository/cargo-h/api/v1/crates/new", body.Bytes(), ok(200))
			},
			del:   func(i int) component.Component { return component.Component{Name: "demo", Version: v(i)} },
			index: get("/repository/cargo-h/de/mo/demo"),
			shows: func(i int) string { return `"vers":"` + v(i) + `"` }},
		{repo: "docker-h", def: "format: docker",
			put: func(t *testing.T, n *node, i int) {
				d := pushManifest(t, n, "docker-h/app", v(i), v(i), []byte("layer of "+v(i)))
				dockerMu.Lock()
				dockerDigests[i] = d
				dockerMu.Unlock()
			},
			del: func(i int) component.Component {
				dockerMu.Lock()
				defer dockerMu.Unlock()
				return component.Component{Name: "app", Version: dockerDigests[i]}
			},
			index: get("/v2/docker-h/app/tags/list"),
			shows: func(i int) string { return `"` + v(i) + `"` }},
		{repo: "helm-h", def: "format: helm",
			put: func(t *testing.T, n *node, i int) {
				chart := tgz("demo/Chart.yaml", "apiVersion: v2\nname: demo\nversion: "+v(i)+"\n", "demo/values.yaml", "a: 1\n")
				n.want(t, "PUT", "/repository/helm-h/demo-"+v(i)+".tgz", chart, ok(201))
			},
			del:   func(i int) component.Component { return component.Component{Name: "demo", Version: v(i)} },
			index: get("/repository/helm-h/index.yaml"),
			shows: func(i int) string { return "version: " + v(i) + "\n" }},
		{repo: "yum-h", def: "format: yum",
			put: func(t *testing.T, n *node, i int) {
				ver := fmt.Sprintf("1.%d", i)
				n.want(t, "PUT", "/repository/yum-h/demo-"+ver+"-1.x86_64.rpm", rpm("demo", ver, "1", "x86_64"), ok(201))
			},
			del: func(i int) component.Component {
				return component.Component{Name: "demo", Version: fmt.Sprintf("1.%d-1.x86_64", i)}
			},
			index: func(t *testing.T, n *node) string {
				repomd := n.want(t, "GET", "/repository/yum-h/repodata/repomd.xml", nil, ok(200))
				m := regexp.MustCompile(`href="(repodata/[^"]*primary[^"]*)"`).FindStringSubmatch(repomd)
				if m == nil {
					t.Errorf("repomd.xml: %s", repomd)
					return ""
				}
				return gunzip(t, n.want(t, "GET", "/repository/yum-h/"+m[1], nil, ok(200)))
			},
			shows: func(i int) string { return fmt.Sprintf(`ver="1.%d"`, i) }},
		{repo: "apt-h", def: "format: apt",
			put: func(t *testing.T, n *node, i int) {
				n.want(t, "PUT", "/repository/apt-h/pool/demo_"+v(i)+"_amd64.deb", deb("demo", v(i)), ok(201))
			},
			del: func(i int) component.Component {
				return component.Component{Group: "amd64", Name: "demo", Version: v(i)}
			},
			index: get("/repository/apt-h/dists/stable/main/binary-amd64/Packages"),
			shows: func(i int) string { return "Version: " + v(i) + "\n" }},
		{repo: "conda-h", def: "format: conda",
			put: func(t *testing.T, n *node, i int) {
				stem := "foo-" + v(i) + "-py_0"
				pkg := zipOf("metadata.json", `{"conda_pkg_format_version": 2}`,
					"info-"+stem+".tar.zst", string(zstdRaw(tarOf("info/index.json", `{"name": "foo", "version": "`+v(i)+`", "build": "py_0", "build_number": 0, "depends": [], "license": "MIT"}`))),
					"pkg-"+stem+".tar.zst", string(zstdRaw(tarOf("lib/x.py", "print(1)"))))
				n.want(t, "PUT", "/repository/conda-h/noarch/"+stem+".conda", pkg, ok(201))
			},
			del: func(i int) component.Component {
				return component.Component{Group: "noarch", Name: "foo", Version: v(i) + "-py_0"}
			},
			index: get("/repository/conda-h/noarch/repodata.json"),
			shows: func(i int) string { return "foo-" + v(i) + "-py_0.conda" }},
		// An R directory's PACKAGES names each package once: the versions
		// here are packages of their own.
		{repo: "r-h", def: "format: r",
			put: func(t *testing.T, n *node, i int) {
				pkg := fmt.Sprintf("pk%d", i)
				src := tarGzOf(pkg+"/DESCRIPTION", "Package: "+pkg+"\nVersion: 1.0.0\nTitle: T\nLicense: MIT\n", pkg+"/NAMESPACE", "")
				n.want(t, "PUT", "/repository/r-h/src/contrib/"+pkg+"_1.0.0.tar.gz", src, ok(201))
			},
			del: func(i int) component.Component {
				return component.Component{Group: "src/contrib", Name: fmt.Sprintf("pk%d", i), Version: "1.0.0"}
			},
			index: get("/repository/r-h/src/contrib/PACKAGES"),
			shows: func(i int) string { return fmt.Sprintf("Package: pk%d\n", i) }},
		{repo: "gems-h", def: "format: rubygems",
			put: func(t *testing.T, n *node, i int) {
				n.want(t, "POST", "/repository/gems-h/api/v1/gems", gem("demo", v(i)), ok(200), "Content-Type", "application/octet-stream")
			},
			del:   func(i int) component.Component { return component.Component{Name: "demo", Version: v(i)} },
			index: get("/repository/gems-h/info/demo"),
			shows: func(i int) string { return v(i) + " |" }},
		{repo: "nuget-h", def: "format: nuget",
			put: func(t *testing.T, n *node, i int) {
				nuspec := `<?xml version="1.0"?><package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd"><metadata>` +
					`<id>Demo.Pkg</id><version>` + v(i) + `</version><authors>QA</authors><description>demo</description></metadata></package>`
				body, ct := form("package", "package.nupkg", zipOf("[Content_Types].xml", "<Types/>", "Demo.Pkg.nuspec", nuspec, "lib/net6.0/Demo.Pkg.dll", "MZ"))
				n.want(t, "PUT", "/repository/nuget-h/api/v2/package", body, ok(201), "Content-Type", ct)
			},
			del:   func(i int) component.Component { return component.Component{Name: "Demo.Pkg", Version: v(i)} },
			index: get("/repository/nuget-h/v3-flatcontainer/demo.pkg/index.json"),
			shows: func(i int) string { return `"` + v(i) + `"` }},
		{repo: "composer-h", def: "format: composer",
			put: func(t *testing.T, n *node, i int) {
				z := zipOf("composer.json", `{"name":"acme/demo","description":"d","type":"library","license":"MIT"}`, "src/Demo.php", "<?php")
				n.want(t, "PUT", "/repository/composer-h/packages/upload/acme/demo/"+v(i), z, ok(201))
			},
			del:   func(i int) component.Component { return component.Component{Name: "acme/demo", Version: v(i) + ".0"} }, // normalized
			index: get("/repository/composer-h/p2/acme/demo.json"),
			shows: func(i int) string { return `"version":"` + v(i) + `"` }},
		// A Conan recipe's revisions are the versions here: one lock and
		// one revision list per reference.
		{repo: "conan-h", def: "format: conan",
			put: func(t *testing.T, n *node, i int) {
				n.want(t, "PUT", fmt.Sprintf("/repository/conan-h/v2/conans/zlib/1.3/_/_/revisions/a00%d/files/conanfile.py", i), "from conan import ConanFile\n", ok(201))
			},
			del: func(i int) component.Component {
				return component.Component{Name: "zlib", Version: fmt.Sprintf("1.3#a00%d", i)}
			},
			index: get("/repository/conan-h/v2/conans/zlib/1.3/_/_/revisions"),
			shows: func(i int) string { return fmt.Sprintf(`"a00%d"`, i) }},
		{repo: "swift-h", def: "format: swift",
			put: func(t *testing.T, n *node, i int) {
				z := zipOf("LinkedList/Package.swift", "// swift-tools-version:5.7\nimport PackageDescription\nlet package = Package(name: \"LinkedList\")\n",
					"LinkedList/Sources/LinkedList/LinkedList.swift", "public struct LinkedList {}")
				n.want(t, "PUT", "/repository/swift-h/mona/LinkedList/"+v(i), z, ok(201), "Content-Type", "application/zip")
			},
			del:   func(i int) component.Component { return component.Component{Name: "mona.LinkedList", Version: v(i)} },
			index: get("/repository/swift-h/mona/LinkedList.json"),
			shows: func(i int) string { return `"` + v(i) + `"` }},
		{repo: "tf-h", def: "format: terraform",
			put: func(t *testing.T, n *node, i int) {
				n.want(t, "PUT", "/repository/tf-h/v1/modules/acme/demo/aws/"+v(i), tarGzOf("main.tf", "# "+v(i)+"\n"), ok(201))
			},
			del:   func(i int) component.Component { return component.Component{Name: "acme/demo/aws", Version: v(i)} },
			index: get("/repository/tf-h/v1/modules/acme/demo/aws/versions"),
			shows: func(i int) string { return `"` + v(i) + `"` }},
	}
}

// Every hosted format whose writes take the per-key locks, on two nodes:
// versions uploaded at once, alternating between the nodes, all land in
// the derived index both nodes serve; then deletes (the UI's component
// delete) interleaved with more uploads leave exactly the survivors.
func TestClusterHostedFormatsConcurrent(t *testing.T) {
	formats := hostedFormats()
	var defs strings.Builder
	for _, f := range formats {
		fmt.Fprintf(&defs, "  - { name: %s, %s, type: hosted, writePolicy: allow }\n", f.repo, f.def)
	}
	a, b := startCluster(t, defs.String())
	nodes := []*node{a, b}
	const n = 20 // uploads at once; then n/2 deletes with n/2 more uploads
	span := func(from, to int) []int {
		var out []int
		for i := from; i <= to; i++ {
			out = append(out, i)
		}
		return out
	}
	for _, f := range formats {
		t.Run(f.repo, func(t *testing.T) {
			check := func(listed, gone []int) {
				t.Helper()
				for _, nd := range nodes {
					ix := f.index(t, nd)
					for _, i := range listed {
						if !strings.Contains(ix, f.shows(i)) {
							t.Errorf("node %s: the index lacks version %d (%q): %.2000s", nd.Node, i, f.shows(i), ix)
						}
					}
					for _, i := range gone {
						if strings.Contains(ix, f.shows(i)) {
							t.Errorf("node %s: the index still lists deleted version %d: %.2000s", nd.Node, i, ix)
						}
					}
				}
			}
			var wg sync.WaitGroup
			for i := 1; i <= n; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); f.put(t, nodes[i%2], i) }()
			}
			wg.Wait()
			if t.Failed() {
				return
			}
			check(span(1, n), nil)
			for i := 1; i <= n/2; i++ {
				wg.Add(2)
				go func() {
					defer wg.Done()
					nd := nodes[i%2]
					c := f.del(i)
					c.Format, c.Repo = nd.Registry.Get(f.repo).Format, f.repo
					res, err := nd.Deleters.Delete(context.Background(), nd.Registry.Get(f.repo), c, "root")
					if err != nil || res.Assets == 0 {
						t.Errorf("delete version %d on %s: %+v %v", i, nd.Node, res, err)
					}
				}()
				go func() { defer wg.Done(); f.put(t, nodes[(i+1)%2], n+i) }()
			}
			wg.Wait()
			check(span(n/2+1, n+n/2), span(1, n/2))
		})
	}
}
