// Command ha-client is the client side of test/ha/ha-smoke.sh: it asks two
// Command Center nodes on one PostgreSQL database and one shared data
// directory in turn, as a load balancer without session affinity would, and
// receives their webhooks.
//
//	ha-client receive -listen :9999                 webhook receiver and a slow OSV dump; GET /stats
//	ha-client wait -a URL [-b URL]                  until each answers status/writable
//	ha-client check -a URL -b URL -recv URL -db URL      formats, a session, Docker chunks, one task run, webhooks once
//	ha-client load -a URL -b URL -for 60s                requests in turn, each retried on the other node; fails if one is never served
//	ha-client sync-start -a URL -b URL -recv URL         a vulnerability.sync against the slow dump; prints "RUNNING <host> <task>"
//	ha-client sync-check -a URL -recv URL -db URL -task ID  that task ran again on another node and finished; every delivery was sent
//
// The admin is "admin" with the password in HA_PASS; -db is the nodes'
// PostgreSQL, read for the deliveries and runs the API pages.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	user   = "admin"
	pass   = os.Getenv("HA_PASS")
	client = &http.Client{Timeout: 60 * time.Second}
	db     *sql.DB
)

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		log.Fatal("usage: ha-client receive|wait|check|load|sync-start|sync-check [flags]")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	a := fs.String("a", "", "node A")
	b := fs.String("b", "", "node B")
	recv := fs.String("recv", "", "the receiver, as the nodes reach it")
	listen := fs.String("listen", ":9999", "receive: listen address")
	dur := fs.Duration("for", time.Minute, "load: how long")
	task := fs.String("task", "", "sync-check: the task")
	dbURL := fs.String("db", "", "check, sync-check: the PostgreSQL URL")
	fs.Parse(os.Args[2:])
	var err error
	if *dbURL != "" {
		if db, err = sql.Open("pgx", *dbURL); err != nil {
			log.Fatal(err)
		}
	}
	switch os.Args[1] {
	case "receive":
		err = receive(*listen)
	case "wait":
		err = waitReady(*a, *b)
	case "check":
		err = check(*a, *b, *recv)
	case "load":
		err = load(*a, *b, *dur)
	case "sync-start":
		err = syncStart(*a, *b, *recv)
	case "sync-check":
		err = syncCheck(*a, *recv, *task)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatalf("FAIL %s: %v", os.Args[1], err)
	}
}

// ---- HTTP ----

type reply struct {
	status int
	body   []byte
	hdr    http.Header
}

func (r reply) String() string { return fmt.Sprintf("%d %.200s", r.status, r.body) }

// call sends method url as the admin (basic auth) with body and headers as
// name, value pairs; /-/api/ calls carry the UI header and JSON. A
// "Cookie" header replaces the basic auth.
func call(method, url string, body []byte, hdr ...string) (reply, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return reply{}, err
	}
	if strings.Contains(url, "/-/api/") {
		req.Header.Set("X-Command-Center-UI", "1")
		req.Header.Set("Content-Type", "application/json")
	}
	basic := true
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
		basic = basic && hdr[i] != "Cookie"
	}
	if basic {
		req.SetBasicAuth(user, pass)
	}
	resp, err := client.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, out, resp.Header}, err
}

// want is call failing unless the status is one of statuses.
func want(statuses []int, method, url string, body []byte, hdr ...string) (reply, error) {
	r, err := call(method, url, body, hdr...)
	if err != nil {
		return r, fmt.Errorf("%s %s: %w", method, url, err)
	}
	for _, s := range statuses {
		if r.status == s {
			return r, nil
		}
	}
	return r, fmt.Errorf("%s %s: %s, want %v", method, url, r, statuses)
}

func codes(c ...int) []int { return c }

func js(v any) []byte { b, _ := json.Marshal(v); return b }

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func until(what string, wait time.Duration, cond func() (bool, error)) error {
	end := time.Now().Add(wait)
	for {
		ok, err := cond()
		if ok {
			return nil
		}
		if time.Now().After(end) {
			return fmt.Errorf("%s: not within %s (last error: %v)", what, wait, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// ---- receive ----

// receive counts the webhook deliveries by X-Nexus-Webhook-Delivery and
// serves <path>/<ecosystem>/all.zip under /osv after 20 s (an empty zip),
// so a vulnerability.sync runs long enough to kill its node.
func receive(addr string) error {
	var mu sync.Mutex
	deliveries := map[string]int{}
	var osv []string // remote host of each dump request
	http.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		deliveries[r.Header.Get("X-Nexus-Webhook-Delivery")]++
		mu.Unlock()
	})
	http.HandleFunc("/osv/", func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if names, err := net.LookupAddr(host); err == nil && len(names) > 0 {
			host = strings.SplitN(names[0], ".", 2)[0]
		}
		mu.Lock()
		osv = append(osv, host)
		mu.Unlock()
		log.Printf("OSV dump request from %s", host)
		time.Sleep(20 * time.Second)
		w.Write(append([]byte("PK\x05\x06"), make([]byte, 18)...)) // an empty zip
	})
	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Write(js(map[string]any{"deliveries": deliveries, "osv": osv}))
	})
	log.Printf("receiving on %s", addr)
	return http.ListenAndServe(addr, nil)
}

type stats struct {
	Deliveries map[string]int `json:"deliveries"`
	OSV        []string       `json:"osv"`
}

func getStats(recv string) (stats, error) {
	var s stats
	r, err := want(codes(200), "GET", recv+"/stats", nil)
	if err == nil {
		err = json.Unmarshal(r.body, &s)
	}
	return s, err
}

// ---- wait ----

func waitReady(nodes ...string) error {
	for _, n := range nodes {
		if n == "" {
			continue
		}
		if err := until(n+" ready", 3*time.Minute, func() (bool, error) {
			r, err := call("GET", n+"/service/rest/v1/status/writable", nil)
			return err == nil && r.status == 200, err
		}); err != nil {
			return err
		}
	}
	return nil
}

// ---- check ----

func check(a, b, recv string) error {
	if err := waitReady(a, b); err != nil {
		return err
	}
	nodes := []string{a, b}

	// A task due in 3 s runs once, on the leader, though both nodes tick.
	due := time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339)
	r, err := want(codes(201), "POST", a+"/-/api/tasks", js(map[string]any{"name": "ha-once", "type": "blobstore.compact", "schedule": "once:" + due, "params": map[string]any{}}))
	if err != nil {
		return err
	}
	var task struct{ ID string }
	json.Unmarshal(r.body, &task)

	// A webhook created on A gets B's events too, each delivery once.
	r, err = want(codes(201), "POST", a+"/-/api/webhooks", js(map[string]any{"name": "ha", "url": recv + "/hook", "events": []string{"*"}}))
	if err != nil {
		return err
	}
	var hook struct{ ID string }
	json.Unmarshal(r.body, &hook)
	log.Printf("ok: task %s due at %s, webhook %s", task.ID, due, hook.ID)

	// A session started on A works on B; a logout on B ends it on A.
	r, err = want(codes(200), "POST", a+"/-/api/login", js(map[string]string{"user": user, "password": pass}), "Cookie", "")
	if err != nil {
		return err
	}
	var cookie string
	for _, c := range (&http.Response{Header: r.hdr}).Cookies() {
		if c.Name != "" && c.Value != "" {
			cookie = c.Name + "=" + c.Value
		}
	}
	if r, err = want(codes(200), "GET", b+"/-/api/me", nil, "Cookie", cookie); err != nil || !bytes.Contains(r.body, []byte(`"user":"admin"`)) {
		return fmt.Errorf("B does not take A's session: %v %s", err, r)
	}
	if _, err = want(codes(204), "POST", b+"/-/api/logout", nil, "Cookie", cookie); err != nil {
		return err
	}
	if r, err = want(codes(200), "GET", a+"/-/api/me", nil, "Cookie", cookie); err != nil || bytes.Contains(r.body, []byte(`"user":"admin"`)) {
		return fmt.Errorf("A still takes the session B ended: %v %s", err, r)
	}
	log.Print("ok: session started on A works on B, logout on B ends it on A")

	// Maven: versions deployed through either node, maven-metadata.xml lists both on both.
	for i, n := range nodes {
		v := fmt.Sprintf("1.%d.0", i)
		base := fmt.Sprintf("%s/repository/maven-releases/org/example/ha/%s/ha-%s", n, v, v)
		pom := fmt.Sprintf(`<project><modelVersion>4.0.0</modelVersion><groupId>org.example</groupId><artifactId>ha</artifactId><version>%s</version></project>`, v)
		if _, err := want(codes(201), "PUT", base+".jar", []byte("jar "+v)); err != nil {
			return err
		}
		if _, err := want(codes(201), "PUT", base+".pom", []byte(pom)); err != nil {
			return err
		}
	}
	for _, n := range nodes {
		r, err := want(codes(200), "GET", n+"/repository/maven-releases/org/example/ha/maven-metadata.xml", nil)
		if err != nil {
			return err
		}
		if !bytes.Contains(r.body, []byte("<version>1.0.0</version>")) || !bytes.Contains(r.body, []byte("<version>1.1.0</version>")) {
			return fmt.Errorf("maven-metadata.xml on %s: %s", n, r.body)
		}
	}
	log.Print("ok: maven deploy through A and B, maven-metadata.xml lists both versions on both")

	// npm: one version published through each node; the packument lists both, tarballs from the other node.
	for i, n := range nodes {
		if _, err := want(codes(200, 201), "PUT", n+"/repository/npm-hosted/ha-demo", npmPublish("ha-demo", fmt.Sprintf("1.%d.0", i)), "Content-Type", "application/json"); err != nil {
			return err
		}
	}
	for i, n := range nodes {
		r, err := want(codes(200), "GET", n+"/repository/npm-hosted/ha-demo", nil)
		if err != nil {
			return err
		}
		var doc struct {
			Versions map[string]struct {
				Dist struct{ Tarball string } `json:"dist"`
			} `json:"versions"`
		}
		if err := json.Unmarshal(r.body, &doc); err != nil || len(doc.Versions) != 2 {
			return fmt.Errorf("packument on %s: %v %.300s", n, err, r.body)
		}
		tgz := fmt.Sprintf("%s/repository/npm-hosted/ha-demo/-/ha-demo-1.%d.0.tgz", nodes[1-i], i)
		if _, err := want(codes(200), "GET", tgz, nil); err != nil {
			return err
		}
	}
	log.Print("ok: npm publish through A and B, install metadata and tarballs on both")

	// Docker: one blob in chunks, each step on the other node (POST A, PATCH
	// B, PATCH A, PUT B), then an image.
	layer := bytes.Repeat([]byte("layer-bytes "), 40000)
	r, err = want(codes(202), "POST", a+"/v2/docker-hosted/ha/app/blobs/uploads/", nil)
	if err != nil {
		return err
	}
	loc := r.hdr.Get("Location")
	parts := [][]byte{layer[:100000], layer[100000:300000], layer[300000:]}
	off := 0
	for i, p := range parts[:2] {
		rng := fmt.Sprintf("%d-%d", off, off+len(p)-1)
		if _, err := want(codes(202), "PATCH", nodes[(i+1)%2]+loc, p, "Content-Range", rng, "Content-Type", "application/octet-stream"); err != nil {
			return err
		}
		off += len(p)
	}
	ld := "sha256:" + sha256Hex(layer)
	if _, err := want(codes(201), "PUT", b+loc+"?digest="+ld, parts[2], "Content-Type", "application/octet-stream"); err != nil {
		return err
	}
	conf := []byte(`{"architecture":"amd64","os":"linux","config":{}}`)
	cd := "sha256:" + sha256Hex(conf)
	if _, err := want(codes(201), "POST", b+"/v2/docker-hosted/ha/app/blobs/uploads/?digest="+cd, conf, "Content-Type", "application/octet-stream"); err != nil {
		return err
	}
	man := js(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
		"config": map[string]any{"mediaType": "application/vnd.docker.container.image.v1+json", "digest": cd, "size": len(conf)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "digest": ld, "size": len(layer)}}})
	if _, err := want(codes(201), "PUT", a+"/v2/docker-hosted/ha/app/manifests/v1", man, "Content-Type", "application/vnd.docker.distribution.manifest.v2+json"); err != nil {
		return err
	}
	for _, n := range nodes {
		if _, err := want(codes(200), "GET", n+"/v2/docker-hosted/ha/app/manifests/v1", nil, "Accept", "application/vnd.docker.distribution.manifest.v2+json"); err != nil {
			return err
		}
		r, err := want(codes(200), "GET", n+"/v2/docker-hosted/ha/app/blobs/"+ld, nil)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.body, layer) {
			return fmt.Errorf("layer read on %s: %d bytes, want %d", n, len(r.body), len(layer))
		}
	}
	log.Print("ok: docker chunked push with POST/PATCH/PATCH/PUT alternating nodes, manifest and blob on both")

	// The task ran once; another scheduler tick on each node (30 s) runs it no more.
	if err := until("the once task ran", 90*time.Second, func() (bool, error) {
		r, err := want(codes(200), "GET", a+"/-/api/tasks/"+task.ID, nil)
		return err == nil && bytes.Contains(r.body, []byte(`"lastResult":"OK"`)), err
	}); err != nil {
		return err
	}
	time.Sleep(35 * time.Second)
	var n int
	err = db.QueryRow(`SELECT count(*) FROM events WHERE kind = 'task.run' AND subject = $1`, task.ID).Scan(&n)
	if err != nil || n != 1 {
		return fmt.Errorf("task %s ran %d times (%v), want once", task.ID, n, err)
	}
	log.Print("ok: the task due once ran once across both nodes")

	return checkDeliveries(recv, hook.ID, true)
}

// checkDeliveries waits until every stored delivery of the webhook was
// sent (status ok) and the receiver saw each: exactly once, or (after a
// crash) at least once.
func checkDeliveries(recv, hook string, once bool) error {
	var list []string
	if err := until("every delivery sent", 3*time.Minute, func() (bool, error) {
		var pending int
		if err := db.QueryRow(`SELECT count(*) FROM webhook_deliveries WHERE webhook_id = $1 AND status <> 'ok'`, hook).Scan(&pending); err != nil || pending > 0 {
			return false, fmt.Errorf("%d deliveries not sent (%v)", pending, err)
		}
		rows, err := db.Query(`SELECT id FROM webhook_deliveries WHERE webhook_id = $1`, hook)
		if err != nil {
			return false, err
		}
		defer rows.Close()
		list = nil
		for rows.Next() {
			var id string
			rows.Scan(&id)
			list = append(list, id)
		}
		return len(list) > 0, rows.Err()
	}); err != nil {
		return err
	}
	s, err := getStats(recv)
	if err != nil {
		return err
	}
	dups := 0
	for _, id := range list {
		switch n := s.Deliveries[id]; {
		case n == 0:
			return fmt.Errorf("delivery %s is ok but the receiver never got it", id)
		case n > 1 && once:
			return fmt.Errorf("delivery %s received %d times, want once", id, n)
		case n > 1:
			dups++
		}
	}
	log.Printf("ok: %d webhook deliveries, each received %s (%d received more than once)", len(list), map[bool]string{true: "once", false: "at least once"}[once], dups)
	return nil
}

func npmPublish(name, version string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range [][2]string{{"package/package.json", string(js(map[string]string{"name": name, "version": version}))}, {"package/index.js", version}} {
		tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1])), Typeflag: tar.TypeReg})
		tw.Write([]byte(f[1]))
	}
	tw.Close()
	gz.Close()
	t := buf.Bytes()
	s1, s512 := sha1.Sum(t), sha512.Sum512(t)
	return js(map[string]any{
		"_id": name, "name": name, "dist-tags": map[string]string{"latest": version},
		"versions": map[string]any{version: map[string]any{"name": name, "version": version,
			"dist": map[string]any{"shasum": hex.EncodeToString(s1[:]), "integrity": "sha512-" + base64.StdEncoding.EncodeToString(s512[:])}}},
		"_attachments": map[string]any{name + "-" + version + ".tgz": map[string]any{
			"content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(t), "length": len(t)}},
	})
}

// ---- load ----

// load runs 4 workers for d. Each repeats: PUT a raw file (writePolicy
// allow, so a retry may write again), read it back from the other node and
// compare, read the maven-metadata.xml and the npm packument of check. A
// step that fails with a connection error or a 5xx is retried on the other
// node, up to 8 tries over about 20 s, as a client behind a load balancer
// would; one no node served fails the run.
func load(a, b string, d time.Duration) error {
	nodes := []string{a, b}
	var total, retried, failed atomic.Int64
	end := time.Now().Add(d)
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; time.Now().Before(end); i++ {
				path := fmt.Sprintf("/repository/raw-load/w%d/f%d.txt", w, i%20)
				body := fmt.Sprintf("worker %d step %d", w, i)
				steps := []func(node string) error{
					func(n string) error { _, err := want(codes(201), "PUT", n+path, []byte(body)); return err },
					func(n string) error {
						r, err := want(codes(200), "GET", n+path, nil)
						if err == nil && string(r.body) != body {
							return fmt.Errorf("GET %s: %q, want %q (fatal)", path, r.body, body)
						}
						return err
					},
					func(n string) error {
						_, err := want(codes(200), "GET", n+"/repository/maven-releases/org/example/ha/maven-metadata.xml", nil)
						return err
					},
					func(n string) error {
						_, err := want(codes(200), "GET", n+"/repository/npm-hosted/ha-demo", nil)
						return err
					},
				}
				for s, step := range steps {
					total.Add(1)
					var err error
					for try := 0; try < 8; try++ {
						if err = step(nodes[(w+i+s+try)%2]); err == nil || !retryable(err) {
							break
						}
						retried.Add(1)
						time.Sleep(time.Duration(try+1) * 500 * time.Millisecond)
					}
					if err != nil {
						failed.Add(1)
						log.Printf("FAILED after retries: %v", err)
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
		}()
	}
	wg.Wait()
	log.Printf("load: %d requests, %d retries, %d failed", total.Load(), retried.Load(), failed.Load())
	if failed.Load() > 0 {
		return fmt.Errorf("%d requests failed on both nodes", failed.Load())
	}
	return nil
}

// retryable: the node is down or going (a connection error, 502-504), or
// answered 500 while its database session ended.
func retryable(err error) bool {
	var ne net.Error
	s := err.Error()
	return errors.As(err, &ne) || strings.Contains(s, "connection refused") || strings.Contains(s, "connection reset") ||
		strings.Contains(s, "EOF") || strings.Contains(s, ": 50")
}

// ---- sync ----

// syncStart points vulnerability.sync at the receiver's slow dump and
// schedules it once, now; it prints the host of the node that runs it.
func syncStart(a, b, recv string) error {
	if _, err := want(codes(200), "PUT", a+"/-/api/vulnerability/settings", js(map[string]any{"enabled": true, "mode": "offline", "dumpURL": recv + "/osv", "ecosystems": []string{"npm"}})); err != nil {
		return err
	}
	due := time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339)
	r, err := want(codes(201), "POST", b+"/-/api/tasks", js(map[string]any{"name": "ha-sync", "type": "vulnerability.sync", "schedule": "once:" + due, "params": map[string]any{}}))
	if err != nil {
		return err
	}
	var task struct{ ID string }
	json.Unmarshal(r.body, &task)
	var host string
	if err := until("a node runs the sync", 90*time.Second, func() (bool, error) {
		r, err := want(codes(200), "GET", a+"/-/api/nodes", nil)
		if err != nil {
			return false, err
		}
		var out struct {
			Nodes []struct{ Hostname, RunningTask string }
		}
		json.Unmarshal(r.body, &out)
		for _, n := range out.Nodes {
			if n.RunningTask == task.ID {
				host = n.Hostname
			}
		}
		return host != "", nil
	}); err != nil {
		return err
	}
	fmt.Printf("RUNNING %s %s\n", host, task.ID)
	return nil
}

// syncCheck waits until the sync task, whose node was killed mid-run,
// finished on the node that is left (the receiver saw a second request,
// from another host), then until every webhook delivery was sent.
func syncCheck(a, recv, id string) error {
	if err := until("the sync ran again elsewhere and finished", 4*time.Minute, func() (bool, error) {
		s, err := getStats(recv)
		if err != nil || len(s.OSV) < 2 || s.OSV[0] == s.OSV[len(s.OSV)-1] {
			return false, err
		}
		r, err := want(codes(200), "GET", a+"/-/api/tasks/"+id, nil)
		return err == nil && bytes.Contains(r.body, []byte(`"lastResult":"OK"`)), err
	}); err != nil {
		return err
	}
	s, _ := getStats(recv)
	log.Printf("ok: the sync killed on %s ran again on %s and finished (at least once)", s.OSV[0], s.OSV[len(s.OSV)-1])
	r, err := want(codes(200), "GET", a+"/-/api/webhooks", nil)
	if err != nil {
		return err
	}
	var out struct{ Webhooks []struct{ ID string } }
	json.Unmarshal(r.body, &out)
	if len(out.Webhooks) != 1 {
		return fmt.Errorf("webhooks: %s", r.body)
	}
	return checkDeliveries(recv, out.Webhooks[0].ID, false)
}
