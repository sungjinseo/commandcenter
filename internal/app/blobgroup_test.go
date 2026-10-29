package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/blob"
	"commandcenter/internal/blob/s3test"
	"commandcenter/internal/config"
	"commandcenter/internal/repo"
)

// groupEnv is a running app with the admin "root" (password pw).
type groupEnv struct {
	t   *testing.T
	cfg *config.Config
	a   *App
	ts  *httptest.Server
}

func newGroupEnv(t *testing.T) *groupEnv {
	t.Helper()
	cfg := testConfig(t, "")
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg.Users = append(cfg.Users, config.User{Name: "root", PasswordHash: string(h), Roles: []string{"admin"}})
	e := &groupEnv{t: t, cfg: cfg}
	e.start()
	t.Cleanup(func() { e.stop() })
	return e
}

func (e *groupEnv) start() {
	a, err := New(e.cfg, quiet())
	if err != nil {
		e.t.Fatal(err)
	}
	e.a, e.ts = a, httptest.NewServer(a.Handler)
}

func (e *groupEnv) stop() {
	if e.a != nil {
		e.ts.Close()
		e.a.Close()
		e.a = nil
	}
}

// api calls the server as root with the UI header and wants status.
func (e *groupEnv) api(method, path, body string, status int) string {
	e.t.Helper()
	st, out := do(e.t, method, e.ts.URL+path, "root", body, true)
	if st != status {
		e.t.Fatalf("%s %s: %d, want %d: %s", method, path, st, status, out)
	}
	return out
}

func (e *groupEnv) has(store, sha string) bool {
	_, err := e.a.Store.StoreByName(store).Stat(sha)
	return err == nil
}

// sha of what a raw repository holds at path.
func (e *groupEnv) sha(repoName, path string) string {
	e.t.Helper()
	asset, err := e.a.DB.GetAsset(context.Background(), repoName, path)
	if err != nil {
		e.t.Fatal(err)
	}
	return asset.BlobSHA256
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("%v: %s", err, s)
	}
	return m
}

// A group of two file stores through the real server: validation, routing
// round robin and after an update writeToFirst with a read-only member, the
// view's sizes, the member and delete rules, health, compaction over the
// members and a restart.
func TestBlobGroupAPI(t *testing.T) {
	e := newGroupEnv(t)
	ctx := context.Background()
	for _, n := range []string{"a", "b", "c", "e"} {
		e.api("POST", "/-/api/blobstores", `{"name":"`+n+`"}`, 201)
	}
	g := decode(t, e.api("POST", "/-/api/blobstores", `{"name":"g","type":"group","members":["a",{"name":"b"}]}`, 201))
	if g["type"] != "group" || g["fillPolicy"] != "roundRobin" || len(g["members"].([]any)) != 2 ||
		g["members"].([]any)[1].(map[string]any)["readOnly"] != false {
		t.Fatalf("created: %v", g)
	}
	e.api("POST", "/-/api/repositories", `{"name":"raw-c","format":"raw","type":"hosted","blobStore":"c"}`, 201)
	for body, want := range map[string]string{
		`{"name":"g2","type":"group","members":["nope"]}`:                       `member \"nope\": no such blob store`,
		`{"name":"g2","type":"group","members":["a"]}`:                          `already belongs to the group \"g\"`,
		`{"name":"g2","type":"group","members":["g"]}`:                          "groups do not nest",
		`{"name":"g2","type":"group","members":["g2"]}`:                         "cannot be its own member",
		`{"name":"g2","type":"group","members":["default"]}`:                    "the default store cannot be a member",
		`{"name":"g2","type":"group","members":["c"]}`:                          "is used by repositories raw-c",
		`{"name":"g2","type":"group","members":[{"name":"e","readOnly":true}]}`: "at least one member must be writable",
		`{"name":"g2","type":"group","members":["e"],"fillPolicy":"random"}`:    "fillPolicy must be",
		`{"name":"g2","type":"group","members":[]}`:                             "at least one member",
		`{"name":"g2","type":"file","members":["e"]}`:                           "members and fillPolicy are a group's",
		`{"name":"g2","type":"group","members":["e"],"s3":{"bucket":"x"}}`:      `\"s3\" is an s3 store's`,
	} {
		if out := e.api("POST", "/-/api/blobstores", body, 400); !strings.Contains(out, want) {
			t.Errorf("%s: %s, want ~%s", body, out, want)
		}
	}
	if e.a.Store.HasStore("g2") {
		t.Fatal("a refused group is live")
	}

	e.api("POST", "/-/api/repositories", `{"name":"raw-g","format":"raw","type":"hosted","blobStore":"g"}`, 201)
	if out := e.api("POST", "/-/api/repositories", `{"name":"raw-a","format":"raw","type":"hosted","blobStore":"a"}`, 400); !strings.Contains(out, "unknown blob store") {
		t.Fatalf("a repository on a member: %s", out)
	}
	files := map[string]string{"f/1.txt": "one", "f/2.txt": "two!", "f/3.txt": "three", "f/4.txt": "four!!"}
	for p, body := range files {
		e.api("PUT", "/repository/raw-g/"+p, body, 201)
	}
	inA, inB := 0, 0
	for p, body := range files {
		sha := e.sha("raw-g", p)
		if e.has("a", sha) {
			inA++
		}
		if e.has("b", sha) {
			inB++
		}
		if got := e.api("GET", "/repository/raw-g/"+p, "", 200); got != body {
			t.Fatalf("GET %s = %q", p, got)
		}
	}
	if inA != 2 || inB != 2 {
		t.Fatalf("round robin: %d in a, %d in b", inA, inB)
	}
	v := decode(t, e.api("GET", "/-/api/blobstores/g", "", 200))
	if v["blobCount"] != 4.0 || v["usedBytes"] != 18.0 || v["repoCount"] != 1.0 || v["fsTotal"] != nil {
		t.Fatalf("group view: %v", v)
	}
	if v := decode(t, e.api("GET", "/-/api/blobstores/a", "", 200)); v["memberOf"] != "g" {
		t.Fatalf("member view: %v", v)
	}
	if out := e.api("DELETE", "/-/api/blobstores/a", "", 409); !strings.Contains(out, `member of the group \"g\"`) {
		t.Fatalf("delete a member: %s", out)
	}
	if out := e.api("PUT", "/-/api/blobstores/g", `{"members":["b"]}`, 400); !strings.Contains(out, "holds blobs; remove it with the blobstore.group-remove-member task") {
		t.Fatalf("drop a member with blobs: %s", out)
	}
	if out := e.api("PUT", "/-/api/blobstores/a", `{"members":["b"]}`, 400); !strings.Contains(out, "only a group's members") {
		t.Fatalf("PUT a file store: %s", out)
	}
	e.api("PUT", "/-/api/blobstores/nope", `{"members":["b"]}`, 404)

	// a read-only, b first writable: every new blob goes to b.
	v = decode(t, e.api("PUT", "/-/api/blobstores/g", `{"members":[{"name":"a","readOnly":true},"b","e"],"fillPolicy":"writeToFirst"}`, 200))
	if v["fillPolicy"] != "writeToFirst" || len(v["members"].([]any)) != 3 {
		t.Fatalf("updated: %v", v)
	}
	for _, p := range []string{"w/1", "w/2", "w/3"} {
		e.api("PUT", "/repository/raw-g/"+p, "written "+p, 201)
		if sha := e.sha("raw-g", p); !e.has("b", sha) || e.has("a", sha) || e.has("e", sha) {
			t.Fatalf("%s did not go to b only", p)
		}
	}
	// e is empty: it can be dropped without the task.
	e.api("PUT", "/-/api/blobstores/g", `{"members":[{"name":"a","readOnly":true},"b"]}`, 200)
	if v := decode(t, e.api("GET", "/-/api/blobstores/g", "", 200)); v["fillPolicy"] != "writeToFirst" {
		t.Fatalf("an empty fillPolicy should keep it: %v", v)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if out := e.api("GET", "/-/api/events?kind=blobstore.update", "", 200); strings.Contains(out, "members=a(read-only),b fillPolicy=writeToFirst") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no blobstore.update event")
		}
	}
	h := decode(t, e.api("GET", "/-/api/blobstores/g/health", "", 200))
	if h["healthy"] != true || len(h["checks"].([]any)) != 2 {
		t.Fatalf("health: %v", h)
	}

	// Compaction: a deleted file's blob goes from its member, a stray blob in
	// both members goes from both, the referenced ones stay; a member is not
	// compacted on its own.
	gone := e.sha("raw-g", "f/1.txt")
	e.api("DELETE", "/repository/raw-g/f/1.txt", "", 204)
	var stray string
	for _, m := range []string{"a", "b"} {
		sums, err := e.a.Store.StoreByName(m).Write(strings.NewReader("stray"))
		if err != nil {
			t.Fatal(err)
		}
		stray = sums.SHA256
	}
	if _, _, err := e.a.GCStore(ctx, "a", 0); err == nil || !strings.Contains(err.Error(), `member of the group "g"`) {
		t.Fatalf("GCStore on a member: %v", err)
	}
	n, _, err := e.a.GC(ctx, 0)
	if err != nil || n != 3 {
		t.Fatalf("GC removed %d blobs (%v), want 3: one deleted file, two copies of the stray", n, err)
	}
	if e.has("a", gone) || e.has("b", gone) || e.has("a", stray) || e.has("b", stray) {
		t.Fatal("an unreferenced blob survived")
	}
	for _, p := range []string{"f/2.txt", "f/3.txt", "f/4.txt", "w/1"} {
		e.api("GET", "/repository/raw-g/"+p, "", 200)
	}

	// Restart: the group opens after its members, with its configuration.
	e.stop()
	e.start()
	if cfg, ok := e.a.Store.StoreByName("g").Group(); !ok || len(cfg.Members) != 2 || !cfg.Members[0].ReadOnly || cfg.FillPolicy != blob.FillWriteToFirst {
		t.Fatalf("after restart: %+v %v", cfg, ok)
	}
	for p, body := range map[string]string{"f/2.txt": "two!", "w/2": "written w/2"} {
		if got := e.api("GET", "/repository/raw-g/"+p, "", 200); got != body {
			t.Fatalf("after restart GET %s = %q", p, got)
		}
	}
	if e.a.Store.Routable("a") || e.a.Store.Routable("b") {
		t.Fatal("a member is routable after restart")
	}
	e.api("DELETE", "/-/api/repositories/raw-g", "", 204)
	e.api("DELETE", "/-/api/blobstores/g", "", 204)
	if e.a.Store.GroupOf("a") != "" || !e.a.Store.Routable("a") {
		t.Fatal("a is still a member after its group was deleted")
	}
}

// The Nexus API's group endpoints: list, get, create, update (keeping a
// member's read-only flag), errors, and the type in the blob store list.
func TestBlobGroupNexusAPI(t *testing.T) {
	e := newGroupEnv(t)
	for _, n := range []string{"a", "b", "c"} {
		e.api("POST", "/-/api/blobstores", `{"name":"`+n+`"}`, 201)
	}
	e.api("POST", "/-/api/blobstores", `{"name":"g","type":"group","members":[{"name":"a","readOnly":true},"b"],"fillPolicy":"writeToFirst"}`, 201)
	const p = "/service/rest/v1/blobstores"
	if out := e.api("GET", p+"/group", "", 200); strings.TrimSpace(out) != `[{"fillPolicy":"writeToFirst","members":["a","b"],"name":"g","softQuota":null}]` {
		t.Fatalf("list: %s", out)
	}
	if out := e.api("GET", p+"/group/g", "", 200); strings.TrimSpace(out) != `{"fillPolicy":"writeToFirst","members":["a","b"],"name":"g","softQuota":null}` {
		t.Fatalf("get: %s", out)
	}
	e.api("GET", p+"/group/a", "", 404)
	e.api("GET", p+"/group/nope", "", 404)
	e.api("POST", p+"/group", `{"name":"g3","members":["c"],"fillPolicy":"roundRobin"}`, 204)
	if v := decode(t, e.api("GET", "/-/api/blobstores/g3", "", 200)); v["type"] != "group" || v["fillPolicy"] != "roundRobin" {
		t.Fatalf("created via the Nexus API: %v", v)
	}
	if out := e.api("POST", p+"/group", `{"name":"g4","members":["a"],"fillPolicy":"roundRobin"}`, 400); !strings.Contains(out, `"message"`) || !strings.Contains(out, "already belongs") {
		t.Fatalf("invalid create: %s", out)
	}
	e.api("PUT", p+"/group/g", `{"members":["a","b"],"fillPolicy":"roundRobin"}`, 204)
	v := decode(t, e.api("GET", "/-/api/blobstores/g", "", 200))
	if m := v["members"].([]any); v["fillPolicy"] != "roundRobin" || m[0].(map[string]any)["readOnly"] != true {
		t.Fatalf("after the Nexus PUT: %v", v)
	}
	e.api("PUT", p+"/group/a", `{"members":["b"],"fillPolicy":"roundRobin"}`, 404)
	var list []map[string]any
	json.Unmarshal([]byte(e.api("GET", p, "", 200)), &list)
	for _, s := range list {
		if s["name"] == "g" && s["type"] != "Group" {
			t.Fatalf("blob store list: %v", s)
		}
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", p + "/group", ""},
		{"PUT", p + "/group/g", `{"members":["b"],"fillPolicy":"roundRobin"}`},
		{"PUT", "/-/api/blobstores/g", `{"members":["b"]}`},
		{"POST", "/-/api/blobstores/a/convert-to-group", `{"newMemberName":"x"}`},
		{"GET", "/-/api/blobstores/g/health", ""},
	} {
		if st, _ := do(t, c.method, e.ts.URL+c.path, "ci", c.body, true); st != 403 {
			t.Errorf("a non-admin: %s %s answered %d", c.method, c.path, st)
		}
	}
}

// Converting a store to a group keeps its repositories and their blobs where
// they are, for a named store and for the default one, across a restart.
func TestBlobGroupConvert(t *testing.T) {
	e := newGroupEnv(t)
	ctx := context.Background()
	e.api("POST", "/-/api/blobstores", `{"name":"old"}`, 201)
	e.api("POST", "/-/api/repositories", `{"name":"raw-old","format":"raw","type":"hosted","blobStore":"old"}`, 201)
	e.api("PUT", "/repository/raw-old/x.txt", "before", 201)
	e.api("PUT", "/repository/maven-releases/org/x/1/x-1.jar", "jar", 201)
	before, _ := e.a.DB.GetBlobStore(ctx, "old")
	rootBefore := repo.StoreRoot(e.cfg.DataDir, before)

	for path, tc := range map[string]struct {
		body   string
		status int
		want   string
	}{
		"/-/api/blobstores/old/convert-to-group":  {`{"newMemberName":"bad name!"}`, 400, "blob store"},
		"/-/api/blobstores/nope/convert-to-group": {`{"newMemberName":"x"}`, 404, "not found"},
	} {
		if out := e.api("POST", path, tc.body, tc.status); !strings.Contains(out, tc.want) {
			t.Errorf("%s: %s", path, out)
		}
	}
	if out := e.api("POST", "/-/api/blobstores/old/convert-to-group", `{"newMemberName":"default"}`, 409); !strings.Contains(out, "already exists") {
		t.Fatalf("a taken member name: %s", out)
	}
	v := decode(t, e.api("POST", "/-/api/blobstores/old/convert-to-group", `{"newMemberName":"old-1"}`, 200))
	if v["type"] != "group" || v["repoCount"] != 1.0 || len(v["members"].([]any)) != 1 {
		t.Fatalf("converted: %v", v)
	}
	member, _ := e.a.DB.GetBlobStore(ctx, "old-1")
	if repo.StoreRoot(e.cfg.DataDir, member) != rootBefore || member.Path != before.Path {
		t.Fatalf("the member moved: %q, was %q", repo.StoreRoot(e.cfg.DataDir, member), rootBefore)
	}
	if got := e.api("GET", "/repository/raw-old/x.txt", "", 200); got != "before" {
		t.Fatalf("after convert: %q", got)
	}
	if out := e.api("POST", "/-/api/blobstores/old/convert-to-group", `{"newMemberName":"x"}`, 400); !strings.Contains(out, "a group already") {
		t.Fatalf("convert a group: %s", out)
	}
	if out := e.api("POST", "/-/api/blobstores/old-1/convert-to-group", `{"newMemberName":"x"}`, 409); !strings.Contains(out, "member of the group") {
		t.Fatalf("convert a member: %s", out)
	}
	// Members can be added at once.
	e.api("POST", "/-/api/blobstores", `{"name":"more"}`, 201)
	e.api("PUT", "/-/api/blobstores/old", `{"members":["old-1","more"]}`, 200)
	e.api("PUT", "/repository/raw-old/y.txt", "after", 201)
	e.api("PUT", "/repository/raw-old/z.txt", "after too", 201)
	if !e.has("more", e.sha("raw-old", "y.txt")) && !e.has("more", e.sha("raw-old", "z.txt")) {
		t.Fatal("the added member got none of the new blobs")
	}

	// The default store: its root is the data directory whatever its row
	// says, so the member's path is "." (relative to it).
	e.api("POST", "/-/api/blobstores/default/convert-to-group", `{"newMemberName":"default-1"}`, 200)
	if m, _ := e.a.DB.GetBlobStore(ctx, "default-1"); m.Path != "." || repo.StoreRoot(e.cfg.DataDir, m) != e.cfg.DataDir {
		t.Fatalf("default member row: %+v", m)
	}
	e.stop()
	e.start()
	for p, body := range map[string]string{"/repository/maven-releases/org/x/1/x-1.jar": "jar", "/repository/raw-old/x.txt": "before", "/repository/raw-old/y.txt": "after"} {
		if got := e.api("GET", p, "", 200); got != body {
			t.Fatalf("after restart GET %s = %q", p, got)
		}
	}
	if _, ok := e.a.Store.StoreByName(repo.DefaultStore).Group(); !ok {
		t.Fatal("default is not a group after restart")
	}
	// Compaction of the default group keeps every referenced blob.
	if _, _, err := e.a.GC(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got := e.api("GET", "/repository/maven-releases/org/x/1/x-1.jar", "", 200); got != "jar" {
		t.Fatalf("after GC: %q", got)
	}
	e.api("PUT", "/repository/maven-releases/org/x/2/x-2.jar", "jar 2", 201)
	if !e.has("default-1", e.sha("maven-releases", "org/x/2/x-2.jar")) {
		t.Fatal("a new default-store blob is not in its member")
	}
}

// A group over a file and an S3 member, and an S3 store with a failover
// bucket and SSE-KMS through the API: when the primary dies, a repository
// still reads from the replica and health shows which bucket answers.
func TestBlobGroupS3(t *testing.T) {
	e := newGroupEnv(t)
	primary, replica := s3test.New(t), s3test.New(t)
	replica.Bucket = "replica"
	s3Body := func(name string, extra string) string {
		c := primary.Config()
		return `{"name":"` + name + `","type":"s3","s3":{"bucket":"` + c.Bucket + `","prefix":"` + name + `","endpoint":"` + c.Endpoint +
			`","pathStyle":true,"accessKeyId":"` + c.AccessKey + `","secretAccessKey":"` + c.SecretKey + `"` + extra + `}}`
	}
	e.api("POST", "/-/api/blobstores", `{"name":"disk"}`, 201)
	e.api("POST", "/-/api/blobstores", s3Body("cloud", ""), 201)
	e.api("POST", "/-/api/blobstores", `{"name":"mixed","type":"group","members":["disk","cloud"]}`, 201)
	e.api("POST", "/-/api/repositories", `{"name":"raw-mixed","format":"raw","type":"hosted","blobStore":"mixed"}`, 201)
	e.api("PUT", "/repository/raw-mixed/1", "first", 201)
	e.api("PUT", "/repository/raw-mixed/2", "second", 201)
	if !e.has("disk", e.sha("raw-mixed", "1")) || !e.has("cloud", e.sha("raw-mixed", "2")) {
		t.Fatalf("want one blob per member; bucket %v", primary.Keys())
	}
	if got := e.api("GET", "/repository/raw-mixed/2", "", 200); got != "second" {
		t.Fatalf("from the S3 member: %q", got)
	}
	h := e.api("GET", "/-/api/blobstores/mixed/health", "", 200)
	if !strings.Contains(h, `"target":"member cloud: primary `) || !strings.Contains(h, `"healthy":true`) {
		t.Fatalf("group health: %s", h)
	}

	const kms = "alias/cc-blobs"
	e.api("POST", "/-/api/blobstores", s3Body("ha", `,"encryption":{"type":"kmsManagedEncryption","kmsKeyId":"`+kms+
		`"},"failover":[{"region":"eu-west-1","bucket":"replica","endpoint":"`+replica.URL+`"}]`), 201)
	v := decode(t, e.api("GET", "/-/api/blobstores/ha", "", 200))
	s3 := v["s3"].(map[string]any)
	if s3["sse"] != "aws:kms" || s3["encryption"].(map[string]any)["kmsKeyId"] != kms ||
		s3["failover"].([]any)[0].(map[string]any)["bucket"] != "replica" {
		t.Fatalf("view: %v", s3)
	}
	if out := e.api("POST", "/-/api/blobstores", s3Body("bad", `,"failover":[{"bucket":"NO"}]`), 400); !strings.Contains(out, "failover[0].bucket") {
		t.Fatalf("bad failover: %s", out)
	}
	e.api("POST", "/-/api/repositories", `{"name":"raw-ha","format":"raw","type":"hosted","blobStore":"ha"}`, 201)
	e.api("PUT", "/repository/raw-ha/f.bin", "replicated", 201)
	key := "ha/" + e.sha("raw-ha", "f.bin")
	data, ok := primary.Object(key)
	if !ok || primary.SSE(key) != "aws:kms" || primary.KMSKey(key) != kms {
		t.Fatalf("object %q: sse %q kms %q", key, primary.SSE(key), primary.KMSKey(key))
	}
	replica.Put(key, data, time.Now())
	if h := decode(t, e.api("GET", "/-/api/blobstores/ha/health", "", 200)); h["healthy"] != true || len(h["checks"].([]any)) != 2 {
		t.Fatalf("health with both up: %v", h)
	}

	primary.Close()
	if got := e.api("GET", "/repository/raw-ha/f.bin", "", 200); got != "replicated" {
		t.Fatalf("read with the primary down: %q", got)
	}
	h2 := decode(t, e.api("GET", "/-/api/blobstores/ha/health", "", 200))
	checks := h2["checks"].([]any)
	if h2["healthy"] != false || checks[0].(map[string]any)["ok"] != false || checks[1].(map[string]any)["ok"] != true ||
		!strings.HasPrefix(checks[1].(map[string]any)["target"].(string), "failover 1 ") {
		t.Fatalf("health with the primary down: %v", h2)
	}
	if st, _ := do(t, "PUT", e.ts.URL+"/repository/raw-ha/g.bin", "root", "new", false); st < 500 {
		t.Fatalf("a write with the primary down answered %d", st)
	}
}
