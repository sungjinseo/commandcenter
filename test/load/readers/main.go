// Command readers serves synthetic upstream documents of the sizes the public
// registries have (-serve), and sends concurrent reads of one document to a
// commandcenter (-get). test/load/readers.sh runs both.
//
// The documents (real sizes measured 2026-11):
//
//	/pypi/bigpy/json                 3.9 MB, like pypi.org's botocore
//	/simple/bigpy/                   a page with one file, for the owner check
//	/api/models/acme/big/tree/main   1000 entries, 300 KB, a hub tree page
//	/v1/providers/acme/big/versions  600 versions, 230 KB, hashicorp/aws
//	/src/contrib/PACKAGES            22000 packages, 7.5 MB, CRAN
//	/<subdir>/repodata.json          -conda entries (conda-forge linux-64: 470 MB)
//	/channeldata.json                the names of those
//	/com/acme/big/maven-metadata.xml 3000 versions, 100 KB
//	/index.yaml                      300 charts x 50 versions, 15 MB, bitnami's size
//	/bignpm                          2000 versions, 20 MB
//
// Under /small/ each is a few entries long.
//
// Every fetch of the R, conda, Maven and Helm documents differs (a counter), so
// that invalidating a proxy's cache makes a group merge again.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	serve      = flag.String("serve", "", "address to serve the synthetic upstream on")
	condaN     = flag.Int("conda", 100000, "entries of a synthetic repodata.json")
	target     = flag.String("target", "http://commandcenter:8081", "")
	path       = flag.String("get", "", "path to read")
	accept     = flag.String("accept", "", "Accept header of the reads")
	clients    = flag.Int("clients", 16, "")
	seconds    = flag.Int("seconds", 10, "how long the clients read")
	waves      = flag.Int("waves", 0, "instead: this many waves of one read per client, each after invalidating -invalidate")
	invalidate = flag.String("invalidate", "", "proxy whose cache a wave invalidates")
	user       = flag.String("user", "admin", "")
	pass       = flag.String("pass", "", "")
	wait       = flag.Bool("wait", false, "only wait until the server answers its status")
)

var gen atomic.Int64

func main() {
	flag.Parse()
	if *serve != "" {
		log.Fatal(http.ListenAndServe(*serve, http.HandlerFunc(upstream)))
	}
	if *wait {
		for range 120 {
			if resp, err := http.Get(*target + "/service/rest/v1/status"); err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
		os.Exit(1)
	}
	read()
}

func hex(seed, n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n)
	x := uint64(seed)*0x9E3779B97F4A7C15 + 1
	for i := range b {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b[i] = digits[x&15]
	}
	return string(b)
}

func upstream(w http.ResponseWriter, r *http.Request) {
	bw := bufio.NewWriterSize(w, 64<<10)
	defer bw.Flush()
	// Under /small/ the same documents are a few entries long: a second
	// member, so that a group has two documents to merge.
	p, small := strings.CutPrefix(r.URL.Path, "/small")
	scale := func(n int) int {
		if small {
			return min(n, 3)
		}
		return n
	}
	g := gen.Add(1)
	switch {
	case p == "/simple/bigpy/":
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(bw, `<html><body><a href="https://files.example/bigpy-1.0.0.tar.gz#sha256=%s">bigpy-1.0.0.tar.gz</a></body></html>`, hex(1, 64))
	case p == "/pypi/bigpy/json":
		w.Header().Set("Content-Type", "application/json")
		pypiJSON(bw)
	case strings.HasPrefix(p, "/api/models/acme/big/tree/"):
		w.Header().Set("Content-Type", "application/json")
		bw.WriteString("[")
		for i := range 1000 {
			if i > 0 {
				bw.WriteString(",")
			}
			fmt.Fprintf(bw, `{"type":"file","oid":"%s","size":%d,"path":"data/train-%05d-of-01000.parquet","lfs":{"oid":"%s","size":%d,"pointerSize":134},"xetHash":"%s"}`,
				hex(i, 40), 400000000+i, i, hex(i+1, 64), 400000000+i, hex(i+2, 64))
		}
		bw.WriteString("]")
	case p == "/.well-known/terraform.json":
		w.Header().Set("Content-Type", "application/json")
		bw.WriteString(`{"providers.v1":"/v1/providers/","modules.v1":"/v1/modules/"}`)
	case p == "/v1/providers/acme/big/versions":
		w.Header().Set("Content-Type", "application/json")
		bw.WriteString(`{"id":"acme/big","versions":[`)
		for i := range 600 {
			if i > 0 {
				bw.WriteString(",")
			}
			fmt.Fprintf(bw, `{"version":"%d.%d.0","protocols":["5.0"],"platforms":[`, i/50, i%50)
			for j, pl := range []string{"darwin_amd64", "darwin_arm64", "freebsd_386", "freebsd_amd64", "freebsd_arm", "linux_386", "linux_amd64", "linux_arm", "linux_arm64", "openbsd_amd64", "windows_386", "windows_amd64"} {
				if j > 0 {
					bw.WriteString(",")
				}
				goos, arch, _ := strings.Cut(pl, "_")
				fmt.Fprintf(bw, `{"os":%q,"arch":%q}`, goos, arch)
			}
			bw.WriteString("]}")
		}
		bw.WriteString(`],"warnings":null}`)
	case p == "/src/contrib/PACKAGES":
		w.Header().Set("Content-Type", "text/plain")
		for i := range scale(22000) {
			fmt.Fprintf(bw, "Package: pkg%05d\nVersion: 1.%d.%d\nDepends: R (>= 3.5.0), methods\nImports: pkg%05d, pkg%05d, stats, utils, graphics,\n        grDevices\nSuggests: testthat (>= 3.0.0), knitr, rmarkdown, covr\nLicense: GPL-2 | GPL-3\nMD5sum: %s\nNeedsCompilation: %s\n\n",
				i, i%7, g, (i+1)%22000, (i+2)%22000, hex(i, 32), []string{"no", "yes"}[i%2])
		}
	case strings.HasSuffix(p, "/repodata.json"):
		w.Header().Set("Content-Type", "application/json")
		subdir := strings.Trim(strings.TrimSuffix(p, "repodata.json"), "/")
		n := scale(*condaN)
		if subdir != "linux-64" {
			n = 3
		}
		fmt.Fprintf(bw, `{"info":{"subdir":%q},"packages":{},"packages.conda":{`, subdir)
		for i := range n {
			if i > 0 {
				bw.WriteString(",")
			}
			name := fmt.Sprintf("pkg%05d", i%25000)
			fmt.Fprintf(bw, `"%s-1.%d.0-py312h%s_%d.conda":{"build":"py312h%s_%d","build_number":%d,"depends":["python >=3.12,<3.13.0a0","python_abi 3.12.* *_cp312","libgcc >=13","numpy >=1.23"],"license":"BSD-3-Clause","md5":"%s","name":"%s","sha256":"%s","size":%d,"subdir":%q,"timestamp":%d,"version":"1.%d.0"}`,
				name, i/25000, hex(i, 7), g, hex(i, 7), g, g, hex(i, 32), name, hex(i+1, 64), 100000+i, subdir, 1700000000000+g, i/25000)
		}
		bw.WriteString(`},"removed":[],"repodata_version":1}`)
	case p == "/channeldata.json":
		w.Header().Set("Content-Type", "application/json")
		bw.WriteString(`{"channeldata_version":1,"packages":{`)
		for i := range scale(min(*condaN, 25000)) {
			if i > 0 {
				bw.WriteString(",")
			}
			fmt.Fprintf(bw, `"pkg%05d":{"subdirs":["linux-64"],"version":"1.0.0"}`, i)
		}
		bw.WriteString(`},"subdirs":["linux-64","noarch"]}`)
	case p == "/com/acme/big/maven-metadata.xml":
		w.Header().Set("Content-Type", "application/xml")
		bw.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<metadata>\n  <groupId>com.acme</groupId>\n  <artifactId>big</artifactId>\n  <versioning>\n    <latest>2.999.0</latest>\n    <release>2.999.0</release>\n    <versions>\n")
		for i := range scale(3000) {
			fmt.Fprintf(bw, "      <version>%d.%d.0</version>\n", i/1000, i%1000)
		}
		fmt.Fprintf(bw, "    </versions>\n    <lastUpdated>2026%010d</lastUpdated>\n  </versioning>\n</metadata>\n", g)
	case p == "/index.yaml":
		w.Header().Set("Content-Type", "application/x-yaml")
		bw.WriteString("apiVersion: v1\nentries:\n")
		for c := range scale(300) {
			fmt.Fprintf(bw, "  chart%03d:\n", c)
			for v := range 50 {
				fmt.Fprintf(bw, "  - annotations:\n      category: Infrastructure\n      licenses: Apache-2.0\n    apiVersion: v2\n    appVersion: 1.%d.0\n    created: \"2026-01-01T00:00:00.%06dZ\"\n    dependencies:\n    - name: common\n      repository: oci://registry.example/charts\n      tags:\n      - common\n      version: 2.x.x\n    description: Chart %d is a synthetic chart of the size a real one has, with a description of a sentence or two like bitnami's charts.\n    digest: %s\n    home: https://charts.example/chart%03d\n    icon: https://charts.example/chart%03d.svg\n    keywords:\n    - synthetic\n    - load\n    maintainers:\n    - name: Load Test\n      url: https://example.com\n    name: chart%03d\n    sources:\n    - https://example.com/src/chart%03d\n    urls:\n    - https://charts.example/chart%03d-1.%d.0.tgz\n    version: 1.%d.0\n",
					v, g, c, hex(c*100+v, 64), c, c, c, c, c, v, v)
			}
		}
		fmt.Fprintf(bw, "generated: \"2026-01-01T00:00:%02dZ\"\n", g%60)
	case p == "/bignpm":
		w.Header().Set("Content-Type", "application/json")
		bw.WriteString(`{"_id":"bignpm","name":"bignpm","dist-tags":{"latest":"1.1999.0"},"versions":{`)
		pad := strings.Repeat("x", 9000)
		for i := range 2000 {
			if i > 0 {
				bw.WriteString(",")
			}
			fmt.Fprintf(bw, `"1.%d.0":{"name":"bignpm","version":"1.%d.0","description":"%s","dist":{"shasum":"%s","tarball":"http://upstream:8000/bignpm/-/bignpm-1.%d.0.tgz","integrity":"sha512-%s"}}`,
				i, i, pad, hex(i, 40), i, hex(i, 86))
		}
		bw.WriteString(`},"time":{"created":"2020-01-01T00:00:00.000Z","modified":"2026-01-01T00:00:00.000Z"}}`)
	default:
		http.NotFound(w, r)
	}
}

func pypiJSON(w io.Writer) {
	file := func(v int, kind string) string {
		name := fmt.Sprintf("bigpy-1.%d.0.tar.gz", v)
		pt, pv := "sdist", "source"
		if kind == "whl" {
			name, pt, pv = fmt.Sprintf("bigpy-1.%d.0-py3-none-any.whl", v), "bdist_wheel", "py3"
		}
		return fmt.Sprintf(`{"comment_text":"","digests":{"blake2b_256":"%s","md5":"%s","sha256":"%s"},"downloads":-1,"filename":"%s","has_sig":false,"md5_digest":"%s","packagetype":"%s","python_version":"%s","requires_python":">= 3.9","size":%d,"upload_time":"2024-01-01T00:00:00","upload_time_iso_8601":"2024-01-01T00:00:00.000000Z","url":"https://files.pythonhosted.org/packages/%s/%s/%s/%s","yanked":false,"yanked_reason":null}`,
			hex(v, 64), hex(v+1, 32), hex(v+2, 64), name, hex(v+1, 32), pt, pv, 13000000+v, hex(v, 2), hex(v+3, 2), hex(v+4, 60), name)
	}
	fmt.Fprintf(w, `{"info":{"author":"Load Test","description":"%s","name":"bigpy","summary":"A synthetic project the size of botocore","version":"1.1999.0","requires_python":">= 3.9"},"last_serial":1,"releases":{`, strings.Repeat("d", 3000))
	for v := range 2000 {
		if v > 0 {
			io.WriteString(w, ",")
		}
		fmt.Fprintf(w, `"1.%d.0":[%s,%s]`, v, file(v, "whl"), file(v, "sdist"))
	}
	fmt.Fprintf(w, `},"urls":[%s,%s],"vulnerabilities":[]}`, file(1999, "whl"), file(1999, "sdist"))
}

// read sends the reads and prints one line: requests by status, failures,
// bytes and latencies.
func read() {
	c := &http.Client{Timeout: 120 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: *clients}}
	var (
		mu       sync.Mutex
		statuses = map[int]int{}
		failures = map[string]int{}
		lat      []time.Duration
		bytes    atomic.Int64
	)
	get := func() {
		req, _ := http.NewRequest(http.MethodGet, *target+*path, nil)
		req.SetBasicAuth(*user, *pass)
		if *accept != "" {
			req.Header.Set("Accept", *accept)
		}
		start := time.Now()
		resp, err := c.Do(req)
		var n int64
		if err == nil {
			n, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			msg := err.Error()
			for _, class := range []string{"connection refused", "connection reset", "EOF", "Timeout", "no route"} {
				if strings.Contains(msg, class) {
					msg = class
				}
			}
			failures[msg]++
			return
		}
		statuses[resp.StatusCode]++
		lat = append(lat, time.Since(start))
		bytes.Add(n)
	}
	var wg sync.WaitGroup
	if *waves > 0 {
		for range *waves {
			if *invalidate != "" {
				req, _ := http.NewRequest(http.MethodPost, *target+"/service/rest/v1/repositories/"+*invalidate+"/invalidate-cache", nil)
				req.SetBasicAuth(*user, *pass)
				if resp, err := c.Do(req); err != nil || resp.StatusCode/100 != 2 {
					log.Fatalf("invalidate %s: %v %v", *invalidate, err, resp)
				}
			}
			start := make(chan struct{})
			for range *clients {
				wg.Add(1)
				go func() { defer wg.Done(); <-start; get() }()
			}
			close(start)
			wg.Wait()
			time.Sleep(time.Second)
		}
	} else {
		end := time.Now().Add(time.Duration(*seconds) * time.Second)
		for range *clients {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(end) {
					get()
				}
			}()
		}
		wg.Wait()
	}
	slices.Sort(lat)
	q := func(f float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[int(f*float64(len(lat)-1))].Round(time.Millisecond)
	}
	var out []string
	for s, n := range statuses {
		out = append(out, fmt.Sprintf("%d=%d", s, n))
	}
	for m, n := range failures {
		out = append(out, fmt.Sprintf("failed=%d (%s)", n, m))
	}
	slices.Sort(out)
	avg := int64(0)
	if len(lat) > 0 {
		avg = bytes.Load() / int64(len(lat))
	}
	fmt.Printf("%s; %d KiB per answer; p50 %v, max %v\n", strings.Join(out, " "), avg>>10, q(0.5), q(1))
	if len(failures) > 0 || len(statuses) != 1 || statuses[200] == 0 {
		os.Exit(1)
	}
}
