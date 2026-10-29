package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/tasks"
)

// The nodes of a cluster in one process (HA design, P2 tests): apps on one
// PostgreSQL database and one data directory, each with its own HTTP
// server. They are registered as nodes only with AsServer (as a test that
// needs the other nodes listed, such as Store.Sync's, passes); each runs
// follow, the cache loop Serve runs, unless the test leaves it off.
// Package globals (proxy settings, the trust store) are shared, as they
// would not be across processes.

type node struct {
	*App
	url string
}

// clusterConfig is testConfig plus extra repositories and users root, adm1
// and adm2 (admins, password "pw"); the deployer "ci" only writes
// maven-releases.
func clusterConfig(t *testing.T, extra string) *config.Config {
	t.Helper()
	if !metatest.Postgres() {
		t.Skip("the nodes of a cluster share a PostgreSQL database")
	}
	cfg := testConfig(t, extra)
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	for _, u := range []string{"root", "adm1", "adm2"} {
		cfg.Users = append(cfg.Users, config.User{Name: u, PasswordHash: string(h), Roles: []string{"admin"}})
	}
	return cfg
}

// startNode starts one node on cfg; follow runs its cache loop.
func startNode(t *testing.T, cfg *config.Config, follow bool, opts ...Option) *node {
	t.Helper()
	a, err := New(cfg, quiet(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	if follow {
		go func() { defer close(done); a.follow(ctx) }()
	} else {
		close(done)
	}
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(func() {
		ts.Close()
		cancel()
		<-done
		a.Tasks.Stop()
		a.Close()
	})
	return &node{a, ts.URL}
}

// startCluster starts two following nodes.
func startCluster(t *testing.T, extra string) (a, b *node) {
	t.Helper()
	cfg := clusterConfig(t, extra)
	return startNode(t, cfg, true), startNode(t, cfg, true)
}

// do sends method path as user (password "pw") with body (string or
// bytes) and headers as name, value pairs; /-/api/ calls carry the UI header.
func (n *node) do(t *testing.T, user, method, path string, body any, hdr ...string) (int, string, http.Header) {
	t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case string:
		r = strings.NewReader(b)
	case []byte:
		r = bytes.NewReader(b)
	case io.Reader:
		r = b
	}
	if !strings.HasPrefix(path, "http") {
		path = n.url + path
	}
	req, err := http.NewRequest(method, path, r)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.SetBasicAuth(user, "pw")
	}
	if strings.Contains(path, "/-/api/") {
		req.Header.Set("X-Command-Center-UI", "1")
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return 0, "", nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// want is do as root, failing unless the status is one of statuses.
func (n *node) want(t *testing.T, method, path string, body any, statuses []int, hdr ...string) string {
	t.Helper()
	st, b, _ := n.do(t, "root", method, path, body, hdr...)
	for _, s := range statuses {
		if st == s {
			return b
		}
	}
	t.Errorf("%s %s: %d, want %v: %.300s", method, path, st, statuses, b)
	return b
}

func ok(codes ...int) []int { return codes }

// eventually polls cond for up to wait.
func eventually(wait time.Duration, cond func() bool) bool {
	for end := time.Now().Add(wait); ; time.Sleep(10 * time.Millisecond) {
		if cond() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Creating a repository on one node is visible on the other: by
// notification within a second, and at once to a request that names it
// (the registry miss looks in the database), on /repository/ and /v2/.
func TestClusterRegistryCreateVisible(t *testing.T) {
	a, b := startCluster(t, "")
	a.want(t, "POST", "/-/api/repositories", `{"name":"raw-new1","format":"raw","type":"hosted"}`, ok(201))
	if !eventually(time.Second, func() bool { return b.Registry.Get("raw-new1") != nil }) {
		t.Fatal("a repository created on A did not reach B within 1 s")
	}
	a.want(t, "POST", "/-/api/repositories", `{"name":"raw-new2","format":"raw","type":"hosted"}`, ok(201))
	b.want(t, "PUT", "/repository/raw-new2/x.txt", "x", ok(201))
	a.want(t, "GET", "/repository/raw-new2/x.txt", nil, ok(200))
	a.want(t, "POST", "/-/api/repositories", `{"name":"docker-new","format":"docker","type":"hosted"}`, ok(201))
	b.want(t, "POST", "/v2/docker-new/img/blobs/uploads/", nil, ok(202))

	// An update and a delete on B reach A too.
	b.want(t, "PUT", "/-/api/repositories/raw-new1", `{"writePolicy":"deny"}`, ok(200))
	if !eventually(time.Second, func() bool { rp := a.Registry.Get("raw-new1"); return rp != nil && rp.WritePolicy == "deny" }) {
		t.Fatal("an update on B did not reach A within 1 s")
	}
	b.want(t, "DELETE", "/-/api/repositories/raw-new1", nil, ok(204))
	if !eventually(time.Second, func() bool { return a.Registry.Get("raw-new1") == nil }) {
		t.Fatal("a delete on B did not reach A within 1 s")
	}
	a.want(t, "GET", "/repository/raw-new1/x.txt", nil, ok(404))
}

// A repository deleted and created again on A while B streams an upload
// into it: B's write fails with 503, found by the stored gen (B's
// registry, without its cache loop here, still has the old incarnation),
// instead of landing in the new repository.
func TestClusterRecreateDuringUploadFailsByGen(t *testing.T) {
	cfg := clusterConfig(t, "  - { name: docker-g, format: docker, type: hosted }\n")
	a, b := startNode(t, cfg, true), startNode(t, cfg, false)
	content := bytes.Repeat([]byte("layer"), 1<<14)
	pr, pw := io.Pipe()
	res := make(chan int, 1)
	go func() {
		st, body, _ := b.do(t, "root", "POST", "/v2/docker-g/img/blobs/uploads/?digest=sha256:"+sha256Hex(content), pr)
		if st != http.StatusServiceUnavailable {
			t.Logf("upload: %s", body)
		}
		res <- st
	}()
	if _, err := pw.Write(content[:1024]); err != nil { // returns once B reads the body
		t.Fatal(err)
	}
	a.want(t, "DELETE", "/-/api/repositories/docker-g", nil, ok(204))
	a.want(t, "POST", "/-/api/repositories", `{"name":"docker-g","format":"docker","type":"hosted"}`, ok(201))
	pw.Write(content[1024:])
	pw.Close()
	if st := <-res; st != http.StatusServiceUnavailable {
		t.Fatalf("upload into the re-created repository: %d, want 503", st)
	}
	if as, err := a.DB.ListAssets(context.Background(), "docker-g", ""); err != nil || len(as) != 0 {
		t.Fatalf("the new docker-g holds %d assets (%v)", len(as), err)
	}
}

// Deleting a hosted repository on A while B adds it to a group: exactly one
// of the two fails, whichever came second, and no group names a missing
// member.
func TestClusterDeleteVersusGroupAdd(t *testing.T) {
	a, b := startCluster(t, "")
	ctx := context.Background()
	for i := range 6 {
		h, g := fmt.Sprintf("h%d", i), fmt.Sprintf("g%d", i)
		a.want(t, "POST", "/-/api/repositories", `{"name":"`+h+`","format":"maven","type":"hosted"}`, ok(201))
		a.want(t, "POST", "/-/api/repositories", `{"name":"`+g+`","format":"maven","type":"group","members":["maven-releases"]}`, ok(201))
		var wg sync.WaitGroup
		var del, add int
		wg.Add(2)
		go func() { defer wg.Done(); del, _, _ = a.do(t, "root", "DELETE", "/-/api/repositories/"+h, nil) }()
		go func() {
			defer wg.Done()
			add, _, _ = b.do(t, "root", "PUT", "/-/api/repositories/"+g, `{"members":["maven-releases","`+h+`"]}`)
		}()
		wg.Wait()
		switch {
		case del == 204 && add == 400, del == 409 && add == 200:
		default:
			t.Fatalf("round %d: delete %d, group update %d: want exactly one to fail", i, del, add)
		}
		defs, err := a.DB.ListRepositories(ctx)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, d := range defs {
			names[d.Name] = true
		}
		for _, d := range defs {
			for _, m := range d.Members {
				if !names[m] {
					t.Fatalf("group %s names the missing member %s", d.Name, m)
				}
			}
		}
	}
}

// Two admins on two nodes disable each other: the access lock is the
// cluster's, so the second sees the first's change and is refused (409);
// one admin is left. Holding the lock on A makes B's request wait.
func TestClusterLockoutRace(t *testing.T) {
	cfg := clusterConfig(t, "")
	cfg.Users = slicesDeleteUser(cfg.Users, "root")
	a, b := startNode(t, cfg, true), startNode(t, cfg, true)
	unlock, err := a.DB.Lock(context.Background(), "access")
	if err != nil {
		t.Fatal(err)
	}
	res := make(chan int, 2)
	go func() {
		st, _, _ := a.do(t, "adm1", "PUT", "/-/api/users/adm2", `{"roles":["admin"],"status":"disabled"}`)
		res <- st
	}()
	go func() {
		st, _, _ := b.do(t, "adm2", "PUT", "/-/api/users/adm1", `{"roles":["admin"],"status":"disabled"}`)
		res <- st
	}()
	select {
	case st := <-res:
		unlock()
		t.Fatalf("an access change did not wait for the access lock held on A: %d", st)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	got := map[int]int{}
	for range 2 {
		got[<-res]++
	}
	if got[200] != 1 || got[409] != 1 {
		t.Fatalf("statuses %v, want one 200 and one 409", got)
	}
}

func slicesDeleteUser(us []config.User, name string) []config.User {
	var out []config.User
	for _, u := range us {
		if u.Name != name {
			out = append(out, u)
		}
	}
	return out
}

// A routing rule created on A can be named by a proxy updated on B, and a
// blob store created on A takes uploads on B.
func TestClusterRoutingRuleAndBlobStore(t *testing.T) {
	a, b := startCluster(t, "  - { name: mvn-proxy, format: maven, type: proxy, remote: \"https://repo.example.org/maven2\" }\n")
	a.want(t, "POST", "/-/api/routing-rules", `{"name":"block-x","mode":"block","matchers":["^/x/"]}`, ok(201))
	b.want(t, "PUT", "/-/api/repositories/mvn-proxy", `{"remote":"https://repo.example.org/maven2","routingRule":"block-x"}`, ok(200))
	if b.Registry.Route("block-x") == nil || b.Registry.Get("mvn-proxy").RoutingRule != "block-x" {
		t.Fatal("B does not apply the rule it was just given")
	}
	if !eventually(time.Second, func() bool { return a.Registry.Get("mvn-proxy").RoutingRule == "block-x" }) {
		t.Fatal("the proxy's rule did not reach A")
	}

	a.want(t, "POST", "/-/api/blobstores", `{"name":"s2"}`, ok(201))
	b.want(t, "POST", "/-/api/repositories", `{"name":"raw-s2","format":"raw","type":"hosted","blobStore":"s2"}`, ok(201))
	b.want(t, "PUT", "/repository/raw-s2/f.txt", "in s2", ok(201))
	if got := a.want(t, "GET", "/repository/raw-s2/f.txt", nil, ok(200)); got != "in s2" {
		t.Fatalf("read on A: %q", got)
	}
	if _, err := a.Store.StoreByName("s2").Stat(sha256Hex([]byte("in s2"))); err != nil {
		t.Fatalf("the blob is not in s2: %v", err)
	}
	b.want(t, "DELETE", "/-/api/repositories/raw-s2", nil, ok(204))
	b.want(t, "DELETE", "/-/api/blobstores/s2", nil, ok(204))
	if !eventually(time.Second, func() bool { return !a.Store.HasStore("s2") }) {
		t.Fatal("a blob store deleted on B is still open on A")
	}
}

// The same path PUT to a maven allow_once repository on A and B at once:
// exactly one 201.
func TestClusterMavenAllowOnce(t *testing.T) {
	a, b := startCluster(t, "  - { name: mvn-once, format: maven, type: hosted, writePolicy: allow_once }\n")
	for i := range 10 {
		p := fmt.Sprintf("/repository/mvn-once/org/ex/once/1.%d/once-1.%d.jar", i, i)
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for j, n := range []*node{a, b} {
			wg.Add(1)
			go func() { defer wg.Done(); codes[j], _, _ = n.do(t, "root", "PUT", p, fmt.Sprintf("jar from %d", j)) }()
		}
		wg.Wait()
		if !(codes[0] == 201 && codes[1] == 400 || codes[0] == 400 && codes[1] == 201) {
			t.Fatalf("%s: %v, want exactly one 201", p, codes)
		}
	}
}

// pushBlob uploads b as a blob of repository/image on n (monolithic).
func pushBlob(t *testing.T, n *node, name string, b []byte) string {
	t.Helper()
	d := "sha256:" + sha256Hex(b)
	n.want(t, "POST", "/v2/"+name+"/blobs/uploads/?digest="+d, b, ok(201))
	return d
}

// manifest is a schema2 manifest of name with a config of its own and the
// given layer, pushed to ref on n; it returns the digest.
func pushManifest(t *testing.T, n *node, name, ref, conf string, layer []byte) string {
	t.Helper()
	cfgB := []byte(`{"architecture":"amd64","os":"linux","config":{"Labels":{"v":"` + conf + `"}}}`)
	m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
		"config": map[string]any{"mediaType": "application/vnd.docker.container.image.v1+json", "digest": pushBlob(t, n, name, cfgB), "size": len(cfgB)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "digest": pushBlob(t, n, name, layer), "size": len(layer)}}})
	n.want(t, "PUT", "/v2/"+name+"/manifests/"+ref, m, ok(201), "Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
	return "sha256:" + sha256Hex(m)
}

// A manifest PUT on A while B deletes another manifest of the image that
// shares its layer: the new one stays whole, the deleted one is gone.
func TestClusterDockerManifestPutVersusDelete(t *testing.T) {
	a, b := startCluster(t, "  - { name: docker-h, format: docker, type: hosted }\n")
	layer := []byte("shared layer")
	for i := range 5 {
		old := pushManifest(t, a, "docker-h/img", fmt.Sprintf("old%d", i), fmt.Sprintf("old%d", i), layer)
		var wg sync.WaitGroup
		var del int
		var put string
		wg.Add(2)
		go func() {
			defer wg.Done()
			put = pushManifest(t, a, "docker-h/img", fmt.Sprintf("new%d", i), fmt.Sprintf("new%d", i), layer)
		}()
		go func() { defer wg.Done(); del, _, _ = b.do(t, "root", "DELETE", "/v2/docker-h/img/manifests/"+old, nil) }()
		wg.Wait()
		if del != 202 {
			t.Fatalf("delete: %d", del)
		}
		for _, n := range []*node{a, b} {
			n.want(t, "GET", "/v2/docker-h/img/manifests/"+fmt.Sprintf("new%d", i), nil, ok(200))
			n.want(t, "GET", "/v2/docker-h/img/manifests/"+put, nil, ok(200))
			n.want(t, "GET", "/v2/docker-h/img/blobs/sha256:"+sha256Hex(layer), nil, ok(200))
			n.want(t, "GET", "/v2/docker-h/img/manifests/"+old, nil, ok(404))
		}
	}
}

// One chunked blob upload with each step on the other node (POST on A,
// PATCH on B, PATCH on A, PUT on B), and two PATCHes of one session at once
// on A and B: one is taken, the other answered 416, and the blob's digest is
// that of what was taken.
func TestClusterDockerSessionAcrossNodes(t *testing.T) {
	a, b := startCluster(t, "  - { name: docker-s, format: docker, type: hosted }\n")
	parts := [][]byte{bytes.Repeat([]byte("a"), 1000), bytes.Repeat([]byte("b"), 2000), []byte("tail")}
	all := bytes.Join(parts, nil)
	st, body, h := a.do(t, "root", "POST", "/v2/docker-s/img/blobs/uploads/", nil)
	if st != 202 {
		t.Fatalf("POST: %d %s", st, body)
	}
	loc := h.Get("Location")
	b.want(t, "PATCH", loc, parts[0], ok(202), "Content-Range", "0-999")
	a.want(t, "PATCH", loc, parts[1], ok(202), "Content-Range", "1000-2999")
	b.want(t, "PUT", loc+"?digest=sha256:"+sha256Hex(all), parts[2], ok(201))
	if got := a.want(t, "GET", "/v2/docker-s/img/blobs/sha256:"+sha256Hex(all), nil, ok(200)); got != string(all) {
		t.Fatalf("blob read on A: %d bytes, want %d", len(got), len(all))
	}

	for i := range 5 {
		st, body, h := b.do(t, "root", "POST", "/v2/docker-s/img/blobs/uploads/", nil)
		if st != 202 {
			t.Fatalf("POST: %d %s", st, body)
		}
		loc := h.Get("Location")
		chunks := [][]byte{bytes.Repeat([]byte{'A' + byte(i)}, 4096), bytes.Repeat([]byte{'a' + byte(i)}, 4096)}
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for j, n := range []*node{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[j], _, _ = n.do(t, "root", "PATCH", loc, chunks[j], "Content-Range", "0-4095")
			}()
		}
		wg.Wait()
		taken := -1
		switch {
		case codes[0] == 202 && codes[1] == 416:
			taken = 0
		case codes[0] == 416 && codes[1] == 202:
			taken = 1
		default:
			t.Fatalf("concurrent PATCHes: %v, want one 202 and one 416", codes)
		}
		a.want(t, "PUT", loc+"?digest=sha256:"+sha256Hex(chunks[taken]), nil, ok(201))
		b.want(t, "GET", "/v2/docker-s/img/blobs/sha256:"+sha256Hex(chunks[taken]), nil, ok(200))
	}
}

// Users, webhooks and the log level changed on A apply on B at once
// (notifications), well before the 5 s reload.
func TestClusterAccessWebhooksLogLevel(t *testing.T) {
	cfg := clusterConfig(t, "  - { name: raw-w, format: raw, type: hosted }\n")
	lvlA, lvlB := new(slog.LevelVar), new(slog.LevelVar)
	a, b := startNode(t, cfg, true, WithLogLevel(lvlA)), startNode(t, cfg, true, WithLogLevel(lvlB))

	a.want(t, "POST", "/-/api/users", `{"id":"newbie","password":"longenough","roles":["admin"]}`, ok(201))
	if !eventually(time.Second, func() bool { _, ok := b.Auth.Lookup("newbie"); return ok }) {
		t.Fatal("a user created on A is unknown on B after 1 s")
	}

	out := a.want(t, "POST", "/-/api/webhooks", `{"name":"h","url":"http://127.0.0.1:1/hook","events":["*"]}`, ok(201))
	var hook struct{ ID string }
	json.Unmarshal([]byte(out), &hook)
	tries := 0
	if !eventually(2*time.Second, func() bool { // within a second of B's reload, which follows the notification
		tries++
		b.want(t, "PUT", fmt.Sprintf("/repository/raw-w/f%d.txt", tries), "x", ok(201))
		time.Sleep(50 * time.Millisecond) // the dispatcher stores events asynchronously
		dls, err := a.DB.ListDeliveries(context.Background(), hook.ID, 10)
		return err == nil && len(dls) > 0
	}) {
		t.Fatal("an upload on B made no delivery for a webhook created on A")
	}

	a.want(t, "PUT", "/-/api/logging", `{"level":"debug"}`, ok(200))
	if !eventually(time.Second, func() bool { return lvlB.Level() == slog.LevelDebug }) {
		t.Fatalf("the log level set on A is %v on B", lvlB.Level())
	}
}

// inStore reports whether n's blob store holds body.
func (n *node) inStore(store, body string) bool {
	bs := n.Store.StoreByName(store)
	if bs == nil {
		return false
	}
	_, err := bs.Stat(sha256Hex([]byte(body)))
	return err == nil
}

// fastSync makes syncNodes ask the nodes that have not answered again
// every 50 ms.
func fastSync(t *testing.T) {
	d := syncResend
	syncResend = 50 * time.Millisecond
	t.Cleanup(func() { syncResend = d })
}

// A blob store move started on A: B learns of it at once and writes the
// repository to the target; the copy waits for a write B began to the
// source before the move and copies it; meanwhile B reads from both
// stores and shows it as copying, the state of A's task; at the end both
// nodes use the target (HA P5).
func TestClusterMoveAcrossNodes(t *testing.T) {
	fastSync(t)
	cfg := clusterConfig(t, "")
	a, b := startNode(t, cfg, true, AsServer()), startNode(t, cfg, true, AsServer())
	a.Tasks.MoveSettle = time.Millisecond
	a.want(t, "POST", "/-/api/blobstores", `{"name":"s1"}`, ok(201))
	a.want(t, "POST", "/-/api/blobstores", `{"name":"s2"}`, ok(201))
	a.want(t, "POST", "/-/api/repositories", `{"name":"raw-m","format":"raw","type":"hosted","blobStore":"s1"}`, ok(201))
	files := map[string]string{"a.txt": "alpha", "b.txt": "beta"}
	b.want(t, "PUT", "/repository/raw-m/a.txt", files["a.txt"], ok(201))
	a.want(t, "PUT", "/repository/raw-m/b.txt", files["b.txt"], ok(201))

	slow := strings.Repeat("slow", 1<<14)
	pr, pw := io.Pipe()
	res := make(chan int, 1)
	go func() { st, _, _ := b.do(t, "root", "PUT", "/repository/raw-m/slow.bin", pr); res <- st }()
	if _, err := io.WriteString(pw, slow[:1024]); err != nil { // returns once B streams it to s1
		t.Fatal(err)
	}
	var copies atomic.Int32
	paused, resume := make(chan struct{}), make(chan struct{})
	a.Tasks.AfterMoveCopy = func(string, string) {
		if copies.Add(1) == 1 {
			close(paused)
			<-resume
		}
	}
	a.want(t, "POST", "/-/api/repositories/raw-m/blobstore", `{"blobStore":"s2"}`, ok(202))
	if !eventually(time.Second, func() bool { _, ok := b.Store.MoveOf("raw-m"); return ok }) {
		t.Fatal("B did not learn of the move within 1 s")
	}
	time.Sleep(300 * time.Millisecond)
	if n := copies.Load(); n != 0 {
		t.Fatalf("the move copied %d blobs while B's write to the old store was going", n)
	}
	io.WriteString(pw, slow[1024:])
	pw.Close()
	if st := <-res; st != 201 {
		t.Fatalf("B's upload begun before the move: %d", st)
	}
	files["slow.bin"] = slow
	select {
	case <-paused:
	case <-time.After(10 * time.Second):
		t.Fatal("the move did not start copying after B's write ended")
	}
	if m, _ := b.Store.MoveOf("raw-m"); m.State != "interrupted" ||
		!strings.Contains(b.want(t, "GET", "/-/api/repositories", nil, ok(200)), `"source":"s1","target":"s2","state":"copying"`) {
		t.Fatalf("B shows the move A copies as %q, not copying", m.State)
	}
	b.want(t, "PUT", "/repository/raw-m/c.txt", "gamma", ok(201))
	files["c.txt"] = "gamma"
	if a.inStore("s1", "gamma") || !a.inStore("s2", "gamma") {
		t.Fatal("B's upload during the move did not go to the target only")
	}
	for p, body := range files {
		if got := b.want(t, "GET", "/repository/raw-m/"+p, nil, ok(200)); got != body {
			t.Fatalf("B reads %s during the move: %d bytes", p, len(got))
		}
	}
	close(resume)
	a.Tasks.Wait()
	for _, n := range []*node{a, b} {
		if !eventually(time.Second, func() bool {
			_, moving := n.Store.MoveOf("raw-m")
			return !moving && n.Registry.Get("raw-m").BlobStore == "s2"
		}) {
			t.Fatalf("node %s: the move did not end on s2", n.Node)
		}
		for p, body := range files {
			if !n.inStore("s2", body) {
				t.Fatalf("%s is not in s2", p)
			}
			if got := n.want(t, "GET", "/repository/raw-m/"+p, nil, ok(200)); got != body {
				t.Fatalf("node %s reads %s after the move: %d bytes", n.Node, p, len(got))
			}
		}
	}
}

// Removing a member from a group waits until every node made it
// read-only: B, which does not follow the database at first, keeps
// writing to the member, and the task does not copy until B applied the
// change; then the member's blobs, B's late one too, move to the other
// member and it leaves the group on both nodes (HA P5).
func TestClusterGroupMemberRemovalWaitsForNodes(t *testing.T) {
	fastSync(t)
	cfg := clusterConfig(t, "")
	a, b := startNode(t, cfg, true, AsServer()), startNode(t, cfg, false, AsServer())
	for _, s := range []string{"m1", "m2"} {
		a.want(t, "POST", "/-/api/blobstores", `{"name":"`+s+`"}`, ok(201))
	}
	a.want(t, "POST", "/-/api/blobstores", `{"name":"g","type":"group","members":["m1","m2"],"fillPolicy":"writeToFirst"}`, ok(201))
	a.want(t, "POST", "/-/api/repositories", `{"name":"raw-g","format":"raw","type":"hosted","blobStore":"g"}`, ok(201))
	b.want(t, "PUT", "/repository/raw-g/x.txt", "ex", ok(201)) // B opens the group on the registry miss
	if !a.inStore("m1", "ex") {
		t.Fatal("writeToFirst did not write to m1")
	}
	ctx := context.Background()
	task, err := a.Tasks.Create(ctx, tasks.Def{Name: "rm", Type: tasks.TypeGroupRemove, Schedule: tasks.Manual,
		Params: json.RawMessage(`{"group":"g","member":"m1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Tasks.RunAsync(task.ID, "root"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if a.Tasks.Running() != task.ID {
		t.Fatal("the removal did not wait for B")
	}
	b.want(t, "PUT", "/repository/raw-g/y.txt", "why", ok(201))
	if !a.inStore("m1", "why") {
		t.Fatal("B, not following, did not write to m1 (its stale group)")
	}
	fctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); b.follow(fctx) }()
	t.Cleanup(func() { stop(); <-done })
	a.Tasks.Wait()
	if got, err := a.DB.GetTask(ctx, task.ID); err != nil || got.LastResult != tasks.ResultOK {
		t.Fatalf("task: %+v %v", got, err)
	}
	for _, n := range []*node{a, b} {
		if !eventually(time.Second, func() bool {
			cfg, _ := n.Store.StoreByName("g").Group()
			return len(cfg.Members) == 1 && cfg.Members[0].Name == "m2"
		}) {
			cfg, _ := n.Store.StoreByName("g").Group()
			t.Fatalf("node %s: group members %+v", n.Node, cfg.Members)
		}
		for p, body := range map[string]string{"x.txt": "ex", "y.txt": "why"} {
			if !n.inStore("m2", body) {
				t.Fatalf("%s is not in m2", p)
			}
			if got := n.want(t, "GET", "/repository/raw-g/"+p, nil, ok(200)); got != body {
				t.Fatalf("node %s reads %s: %q", n.Node, p, got)
			}
		}
	}
}
