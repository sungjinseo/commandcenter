// Command load drives a running commandcenter with large transfers and adversarial
// uploads while sampling its resident memory through /-/metrics. It also
// serves a fake npm upstream (a ~30 MB package document) on :8000.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	target   = flag.String("target", "http://commandcenter:8081", "commandcenter base URL")
	user     = flag.String("user", "admin", "admin user")
	pass     = flag.String("pass", "", "admin password")
	sizeMiB  = flag.Int64("size-mib", 100, "artifact size for the transfer phases")
	limitMiB = flag.Int64("limit-mib", 100, "maximum allowed RSS of commandcenter")
)

// noise is a fast deterministic reader of pseudo-random bytes.
type noise struct{ r *rand.ChaCha8 }

func newNoise(seed byte) io.Reader { return noise{rand.NewChaCha8([32]byte{seed})} }

func (n noise) Read(p []byte) (int, error) { return n.r.Read(p) }

var failures []string
var mu sync.Mutex

func fail(format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	failures = append(failures, msg)
	log.Print("FAIL: ", msg)
}

func do(method, path string, body io.Reader, header http.Header) (*http.Response, error) {
	return doWith(http.DefaultClient, method, path, body, header)
}

func doWith(c *http.Client, method, path string, body io.Reader, header http.Header) (*http.Response, error) {
	req, err := http.NewRequest(method, *target+path, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(*user, *pass)
	for k, v := range header {
		req.Header[k] = v
	}
	return c.Do(req)
}

// expect performs a request and checks its status; it drains and closes the body.
func expect(name, method, path string, body io.Reader, header http.Header, want int) int64 {
	return expectBody(name, method, path, body, header, want, "")
}

// expectBody is expect that also requires the response body to contain substr
// (when substr is not empty). Only the first 64 KiB of the body are kept for the
// check; the rest is drained and counted.
func expectBody(name, method, path string, body io.Reader, header http.Header, want int, substr string) int64 {
	resp, err := do(method, path, body, header)
	if err != nil {
		fail("%s: %v", name, err)
		return 0
	}
	defer resp.Body.Close()
	var head bytes.Buffer
	n, _ := io.Copy(&head, io.LimitReader(resp.Body, 64<<10))
	m, _ := io.Copy(io.Discard, resp.Body)
	n += m
	if resp.StatusCode != want {
		fail("%s: status %d, want %d (body %.200q)", name, resp.StatusCode, want, head.String())
	}
	if substr != "" && !strings.Contains(head.String(), substr) {
		fail("%s: body %.200q does not mention %q", name, head.String(), substr)
	}
	return n
}

// samples counts the outcomes of the RSS sampler.
type samples struct {
	good, failed atomic.Int64
	max          atomic.Int64 // peak RSS in bytes
}

func sampler(stop <-chan struct{}, s *samples) {
	client := &http.Client{Timeout: 2 * time.Second}
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		rss, err := sampleRSS(client)
		if err != nil {
			s.failed.Add(1)
			log.Printf("rss sample failed: %v", err)
			continue
		}
		s.good.Add(1)
		for {
			cur := s.max.Load()
			if rss <= cur || s.max.CompareAndSwap(cur, rss) {
				break
			}
		}
	}
}

// sampleRSS reads rssBytes from /-/metrics; it fails unless the status is 200,
// the JSON decodes and the value is positive.
func sampleRSS(client *http.Client) (int64, error) {
	resp, err := doWith(client, "GET", "/-/metrics", nil, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	var m struct {
		RSSBytes int64 `json:"rssBytes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return 0, fmt.Errorf("decode: %v", err)
	}
	if m.RSSBytes <= 0 {
		return 0, fmt.Errorf("rssBytes is %d", m.RSSBytes)
	}
	return m.RSSBytes, nil
}

// expectLength downloads path, streaming the body, and checks its length.
func expectLength(name, path string, want int64) {
	if n := expect(name, "GET", path, nil, nil, 200); n != want {
		fail("%s: %d bytes, want %d", name, n, want)
	}
}

// upstream serves the "huge" npm package: 3000 versions with 10 KB readmes each.
func upstream() {
	http.HandleFunc("/huge", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		readme := strings.Repeat("r", 10<<10)
		fmt.Fprint(w, `{"name":"huge","dist-tags":{"latest":"1.0.2999"},"versions":{`)
		for i := 0; i < 3000; i++ {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `"1.0.%d":{"name":"huge","version":"1.0.%d","readme":"%s","dist":{"tarball":"http://load:8000/huge/-/huge-1.0.%d.tgz"}}`, i, i, readme, i)
		}
		fmt.Fprint(w, `}}`)
	})
	log.Fatal(http.ListenAndServe(":8000", nil))
}

// writeTarball writes an npm tarball for name@version, as npm pack does:
// package/package.json and package/data holding size bytes of r.
func writeTarball(w io.Writer, name, version string, r io.Reader, size int64) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	pj := fmt.Sprintf(`{"name":%q,"version":%q}`, name, version)
	tw.WriteHeader(&tar.Header{Name: "package/package.json", Mode: 0o644, Size: int64(len(pj))})
	io.WriteString(tw, pj)
	tw.WriteHeader(&tar.Header{Name: "package/data", Mode: 0o644, Size: size})
	io.Copy(tw, io.LimitReader(r, size))
	tw.Close()
	gz.Close()
}

// byteCount is an io.Writer that counts what is written to it.
type byteCount int64

func (c *byteCount) Write(p []byte) (int, error) { *c += byteCount(len(p)); return len(p), nil }

func npmPublishSmall() {
	var buf bytes.Buffer
	writeTarball(&buf, "huge", "0.0.1", strings.NewReader("small tarball"), 13)
	tgz := buf.Bytes()
	sum := sha1.Sum(tgz)
	doc := fmt.Sprintf(`{"name":"huge","versions":{"0.0.1":{"name":"huge","version":"0.0.1","dist":{"shasum":"%s"}}},"dist-tags":{"beta":"0.0.1"},"_attachments":{"huge-0.0.1.tgz":{"data":"%s"}}}`,
		hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(tgz))
	expect("npm small publish", "PUT", "/repository/npm-hosted/huge", strings.NewReader(doc), nil, 201)
}

// npmPublishBig streams a publish document whose attachment is a tarball
// holding size bytes of noise, and returns the tarball's length. The
// attachment comes first so its sha1 is known when the manifest is written.
func npmPublishBig(size int64) int64 {
	pr, pw := io.Pipe()
	length := make(chan int64, 1)
	go func() {
		h := sha1.New()
		var n byteCount
		io.WriteString(pw, `{"name":"bigpkg","_attachments":{"bigpkg-1.0.0.tgz":{"data":"`)
		enc := base64.NewEncoder(base64.StdEncoding, pw)
		writeTarball(io.MultiWriter(enc, h, &n), "bigpkg", "1.0.0", newNoise(7), size)
		enc.Close()
		length <- int64(n)
		fmt.Fprintf(pw, `"}},"versions":{"1.0.0":{"name":"bigpkg","version":"1.0.0","dist":{"shasum":"%x"}}},"dist-tags":{"latest":"1.0.0"}}`, h.Sum(nil))
		pw.Close()
	}()
	expect("npm 50 MiB publish", "PUT", "/repository/npm-hosted/bigpkg", pr, nil, 201)
	pr.Close() // a refused publish must not leave the writer blocked
	return <-length
}

func goZipBomb() {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < 60000; i++ {
		zw.Create(fmt.Sprintf("example.com/bomb@v1.0.0/f%d", i))
	}
	zw.Close()
	expectBody("go many-entry zip", "PUT", "/repository/go-hosted/example.com/bomb/@v/v1.0.0.zip", &buf, nil, 400, "entries, limit is")
}

// cargoStalled opens n publishes that declare 1 MiB of metadata and then stall.
func cargoStalled(n int) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pr, pw := io.Pipe()
			go func() {
				binary.Write(pw, binary.LittleEndian, uint32(1<<20))
				time.Sleep(3 * time.Second)
				pw.Close()
			}()
			expectBody("cargo stalled publish", "PUT", "/repository/cargo-hosted/api/v1/crates/new", pr, nil, 400, "publish metadata is truncated")
		}()
	}
	wg.Wait()
}

func pypiHugeField() {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		mw.WriteField(":action", "file_upload")
		fw, _ := mw.CreateFormField("description")
		io.Copy(fw, io.LimitReader(newNoise(9), 20<<20))
		mw.Close()
		pw.Close()
	}()
	expect("pypi 20 MiB field", "POST", "/repository/pypi-hosted/", pr, http.Header{"Content-Type": {mw.FormDataContentType()}}, 413)
}

func main() {
	flag.Parse()
	go upstream()
	for i := 0; ; i++ {
		if resp, err := http.Get(*target + "/service/rest/v1/status"); err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			break
		}
		if i == 60 {
			log.Fatal("commandcenter did not become ready")
		}
		time.Sleep(time.Second)
	}
	var rss samples
	stop := make(chan struct{})
	sampled := make(chan struct{})
	runStart := time.Now()
	go func() { sampler(stop, &rss); close(sampled) }()
	size := *sizeMiB << 20
	phase := func(name string, f func()) {
		start := time.Now()
		f()
		log.Printf("phase %-34s %6.1fs  max-rss so far %d MiB", name, time.Since(start).Seconds(), rss.max.Load()>>20)
	}

	phase("maven upload", func() {
		expect("maven upload", "PUT", "/repository/maven-hosted/load/big/1.0/big-1.0.jar", io.LimitReader(newNoise(1), size), nil, 201)
	})
	phase("50 downloads + 5 uploads", func() {
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				expectLength("maven download", "/repository/maven-hosted/load/big/1.0/big-1.0.jar", size)
			}()
		}
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				path := fmt.Sprintf("/repository/maven-hosted/load/big/2.%d/big-2.%d.jar", i, i)
				expect("maven concurrent upload", "PUT", path, io.LimitReader(newNoise(byte(10+i)), size), nil, 201)
			}(i)
		}
		wg.Wait()
		for i := 0; i < 5; i++ {
			path := fmt.Sprintf("/repository/maven-hosted/load/big/2.%d/big-2.%d.jar", i, i)
			expectLength("maven uploaded jar "+path, path, size)
		}
	})
	phase("npm group merge of a 30 MB document", func() {
		npmPublishSmall()
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if n := expect("npm group packument", "GET", "/repository/npm-all/huge", nil, nil, 200); n < 20<<20 {
					fail("npm group packument: only %d bytes", n)
				}
			}()
		}
		wg.Wait()
	})
	phase("npm 50 MiB publish", func() {
		n := npmPublishBig(50 << 20)
		expectLength("npm big tarball", "/repository/npm-hosted/bigpkg/-/bigpkg-1.0.0.tgz", n)
	})
	phase("go 60k-entry zip", goZipBomb)
	phase("cargo 20 stalled publishes", func() { cargoStalled(20) })
	phase("pypi 20 MiB form field", pypiHugeField)

	time.Sleep(time.Second)
	close(stop)
	<-sampled
	elapsed := time.Since(runStart).Seconds()
	good, failed := rss.good.Load(), rss.failed.Load()
	peakBytes := rss.max.Load()
	peak := peakBytes >> 20
	if good == 0 {
		fail("no RSS samples (is /-/metrics reachable for %s?)", *user)
	}
	if attempts := good + failed; attempts > 0 && failed*20 > attempts {
		fail("%d of %d RSS samples failed (more than 5%%)", failed, attempts)
	}
	if float64(good) < 2*elapsed {
		fail("only %d good RSS samples in %.1fs, want at least %.0f", good, elapsed, 2*elapsed)
	}
	if peakBytes >= *limitMiB<<20 {
		fail("max RSS %d bytes, limit %d bytes (%d MiB)", peakBytes, *limitMiB<<20, *limitMiB)
	}
	if len(failures) > 0 {
		fmt.Printf("LOAD FAILED (%d problems, max-rss=%dMiB samples=%d failed-samples=%d)\n", len(failures), peak, good, failed)
		os.Exit(1)
	}
	fmt.Printf("LOAD OK max-rss=%dMiB samples=%d failed-samples=%d elapsed=%.0fs\n", peak, good, failed, elapsed)
}
