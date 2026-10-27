// Command backup is the plan 14 backup load check: it fills a running
// commandcenter with about -gib GiB of distinct blobs, then runs an online
// backup through the API while uploads continue, and samples the server's
// RSS through /-/metrics the whole time. It fails when the peak RSS exceeds
// -limit-mib, the backup fails, or a deep verify of the backup finds a problem.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
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
	gib      = flag.Float64("gib", 1, "data set size in GiB")
	blobKiB  = flag.Int64("blob-kib", 256, "size of each blob")
	limitMiB = flag.Int64("limit-mib", 100, "maximum allowed RSS of commandcenter")
	workers  = flag.Int("workers", 8, "parallel uploaders for the fill")
)

type noise struct{ r *rand.ChaCha8 }

func (n noise) Read(p []byte) (int, error) { return n.r.Read(p) }

func blobReader(id int64, size int64) io.Reader {
	var seed [32]byte
	for i := range 8 {
		seed[i] = byte(id >> (8 * i))
	}
	return io.LimitReader(noise{rand.NewChaCha8(seed)}, size)
}

func do(method, path string, body io.Reader, size int64, ui bool) (*http.Response, error) {
	req, err := http.NewRequest(method, *target+path, body)
	if err != nil {
		return nil, err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.SetBasicAuth(*user, *pass)
	if ui {
		req.Header.Set("X-Command-Center-UI", "1")
	}
	return http.DefaultClient.Do(req)
}

func put(id, size int64, prefix string) error {
	path := fmt.Sprintf("/repository/maven-hosted/org/load/%s/%d/%s-%d.jar", prefix, id, prefix, id)
	resp, err := do("PUT", path, blobReader(id, size), size, false)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("PUT %s: %d", path, resp.StatusCode)
	}
	return nil
}

func getJSON(method, path string, ui bool, out any) (int, error) {
	resp, err := do(method, path, nil, -1, ui)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode %q: %v", b, err)
		}
	}
	return resp.StatusCode, nil
}

func sampleRSS() (int64, error) {
	var m struct {
		RSSBytes int64 `json:"rssBytes"`
	}
	st, err := getJSON("GET", "/-/metrics", false, &m)
	if err != nil || st != 200 || m.RSSBytes <= 0 {
		return 0, fmt.Errorf("metrics: %d %v", st, err)
	}
	return m.RSSBytes, nil
}

func main() {
	flag.Parse()
	var peak atomic.Int64
	var samples, failedSamples atomic.Int64
	stopSampler := make(chan struct{})
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stopSampler:
				return
			case <-t.C:
			}
			rss, err := sampleRSS()
			if err != nil {
				failedSamples.Add(1)
				continue
			}
			samples.Add(1)
			for {
				cur := peak.Load()
				if rss <= cur || peak.CompareAndSwap(cur, rss) {
					break
				}
			}
		}
	}()

	// 1. Fill.
	size := *blobKiB << 10
	n := int64(*gib * float64(1<<30) / float64(size))
	start := time.Now()
	var next atomic.Int64
	var wg sync.WaitGroup
	var fillErr atomic.Value
	for range *workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				id := next.Add(1)
				if id > n {
					return
				}
				if err := put(id, size, "fill"); err != nil {
					fillErr.Store(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err, _ := fillErr.Load().(error); err != nil {
		log.Fatalf("fill: %v", err)
	}
	log.Printf("filled %d blobs of %d KiB (%.2f GiB) in %.0fs, peak RSS so far %d MiB",
		n, *blobKiB, float64(n*size)/(1<<30), time.Since(start).Seconds(), peak.Load()>>20)
	fillPeak := peak.Load()
	peak.Store(0)

	// 2. Uploads continue during the backup.
	stopUploads := make(chan struct{})
	var uploads atomic.Int64
	var upWG sync.WaitGroup
	for w := range 2 {
		upWG.Add(1)
		go func() {
			defer upWG.Done()
			for i := int64(0); ; i++ {
				select {
				case <-stopUploads:
					return
				default:
				}
				if err := put(int64(w+1)*1_000_000_000+i, 64<<10, "live"); err != nil {
					log.Printf("live upload: %v", err)
					return
				}
				uploads.Add(1)
			}
		}()
	}

	// 3. Back up and wait.
	start = time.Now()
	var started struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	if st, err := getJSON("POST", "/-/api/backup", false, &started); err != nil || st != http.StatusAccepted {
		log.Fatalf("start backup: %d %v %s", st, err, started.Error)
	}
	var run struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	for {
		if _, err := getJSON("GET", "/-/api/backup/runs/"+started.ID, false, &run); err != nil {
			log.Fatalf("poll: %v", err)
		}
		if run.Status != "Running" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	close(stopUploads)
	upWG.Wait()
	backupSecs := time.Since(start).Seconds()
	backupPeak := peak.Load()
	log.Printf("backup %s in %.0fs (%d uploads during it): %s", run.Status, backupSecs, uploads.Load(), run.Message)

	// 4. Deep verify of the new backup.
	var list struct {
		Backups []struct {
			Name  string `json:"name"`
			Blobs int64  `json:"blobs"`
			Bytes int64  `json:"bytes"`
		} `json:"backups"`
	}
	if _, err := getJSON("GET", "/-/api/backups", false, &list); err != nil || len(list.Backups) == 0 {
		log.Fatalf("list: %v %+v", err, list)
	}
	b := list.Backups[0]
	type verifyResult struct {
		OK       bool     `json:"ok"`
		Blobs    int64    `json:"blobs"`
		Problems []string `json:"problems"`
	}
	var vr verifyResult
	start = time.Now()
	// A deep verify is a task run: 202, poll the run, then read the result
	// the backup list shows.
	var vstart struct {
		ID string `json:"id"`
	}
	st, err := getJSON("POST", "/-/api/backups/"+b.Name+"/verify?deep=true", true, &vstart)
	if err == nil && st == http.StatusAccepted {
		var vrun struct {
			Status string `json:"status"`
		}
		for {
			if _, err = getJSON("GET", "/-/api/backup/runs/"+vstart.ID, false, &vrun); err != nil || vrun.Status != "Running" {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		var vlist struct {
			Backups []struct {
				Name     string        `json:"name"`
				Verified *verifyResult `json:"verified"`
			} `json:"backups"`
		}
		if _, err = getJSON("GET", "/-/api/backups", false, &vlist); err == nil {
			for _, x := range vlist.Backups {
				if x.Name == b.Name && x.Verified != nil {
					vr, st = *x.Verified, 200
				}
			}
		}
	}
	verifyPeak := peak.Load()
	close(stopSampler)
	log.Printf("deep verify of %s (%d blobs, %d bytes): ok=%v in %.0fs", b.Name, b.Blobs, b.Bytes, vr.OK, time.Since(start).Seconds())

	var failures []string
	if run.Status != "OK" {
		failures = append(failures, "backup run: "+run.Status+": "+run.Message)
	}
	if err != nil || st != 200 || !vr.OK || vr.Blobs != b.Blobs {
		failures = append(failures, fmt.Sprintf("verify: %d %v ok=%v blobs=%d problems=%s", st, err, vr.OK, vr.Blobs, strings.Join(vr.Problems, "; ")))
	}
	if b.Bytes < int64(*gib*float64(1<<30))*9/10 {
		failures = append(failures, fmt.Sprintf("backup holds only %d bytes", b.Bytes))
	}
	limit := *limitMiB << 20
	for _, p := range []struct {
		name string
		v    int64
	}{{"fill", fillPeak}, {"backup", backupPeak}, {"verify", verifyPeak}} {
		if p.v > limit {
			failures = append(failures, fmt.Sprintf("peak RSS during %s %d MiB > %d MiB", p.name, p.v>>20, *limitMiB))
		}
	}
	if samples.Load() == 0 || failedSamples.Load()*20 > samples.Load()+failedSamples.Load() {
		failures = append(failures, fmt.Sprintf("RSS samples: %d good, %d failed", samples.Load(), failedSamples.Load()))
	}
	summary := fmt.Sprintf("peak-rss fill=%dMiB backup=%dMiB verify=%dMiB samples=%d", fillPeak>>20, backupPeak>>20, verifyPeak>>20, samples.Load())
	if len(failures) > 0 {
		for _, f := range failures {
			log.Print("FAIL: ", f)
		}
		fmt.Println("BACKUP LOAD FAILED " + summary)
		os.Exit(1)
	}
	fmt.Println("BACKUP LOAD OK " + summary)
}
