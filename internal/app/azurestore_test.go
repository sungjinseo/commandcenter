package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/blob"
	"commandcenter/internal/config"
)

// The Azurite development account's published name and key.
const (
	azuriteAccount = "devstoreaccount1"
	azuriteKey     = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
)

// azuriteContainer creates a fresh container on the Azurite at ep and
// removes it when the test ends.
func azuriteContainer(t *testing.T, ep string) string {
	t.Helper()
	b := make([]byte, 6)
	rand.Read(b)
	name := "e" + hex.EncodeToString(b)
	key, _ := base64.StdEncoding.DecodeString(azuriteKey)
	call := func(method string, want int) {
		r, _ := http.NewRequest(method, ep+"/"+name+"?restype=container", nil)
		r.Header.Set("x-ms-version", "2025-05-05")
		r.Header.Set("x-ms-date", time.Now().UTC().Format(http.TimeFormat))
		blob.SignSharedKey(r, azuriteAccount, key)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s container %s: %s", method, name, resp.Status)
		}
	}
	call(http.MethodPut, http.StatusCreated)
	t.Cleanup(func() { call(http.MethodDelete, http.StatusAccepted) })
	return name
}

// An azure blob store through the real server and Azurite
// (COMMANDCENTER_TEST_AZURITE_URL): created with its container checked and
// its key sealed and never shown; a raw hosted repository on it uploads and
// downloads (ranges too) and its GC removes only what nothing references; its
// credentials change in place; the Nexus REST API creates, reads and updates
// one; a group takes one as a member; and it opens again at startup.
func TestAzureStoreEndToEnd(t *testing.T) {
	ep := os.Getenv("COMMANDCENTER_TEST_AZURITE_URL")
	if ep == "" {
		t.Skip("COMMANDCENTER_TEST_AZURITE_URL is not set")
	}
	c1, c2 := azuriteContainer(t, ep), azuriteContainer(t, ep)
	cfg := testConfig(t, "")
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg.Users = append(cfg.Users, config.User{Name: "root", PasswordHash: string(h), Roles: []string{"admin"}})
	cfg.SecretKeyFile = filepath.Join(t.TempDir(), "secret.key")
	os.WriteFile(cfg.SecretKeyFile, []byte(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))), 0o600)
	e := &groupEnv{t: t, cfg: cfg}
	e.start()
	t.Cleanup(e.stop)
	ctx := context.Background()

	body := func(name, container, key string) string {
		return `{"name":"` + name + `","type":"azure","azure":{"accountName":"` + azuriteAccount + `","containerName":"` + container +
			`","prefix":"/cc/","endpoint":"` + ep + `","accountKey":"` + key + `"}}`
	}
	out := e.api("POST", "/-/api/blobstores", body("az", c1, azuriteKey), 201)
	m := decode(t, out)
	az, _ := m["azure"].(map[string]any)
	if m["type"] != "azure" || m["fsTotal"] != nil || az == nil || az["containerName"] != c1 || az["prefix"] != "cc" ||
		az["authenticationMethod"] != "accountKey" || az["accountKeySet"] != true || az["sasTokenSet"] != false {
		t.Fatalf("created: %s", out)
	}
	for _, path := range []string{"/-/api/blobstores", "/-/api/blobstores/az", "/service/rest/v1/blobstores/azure/az"} {
		if out := e.api("GET", path, "", 200); strings.Contains(out, azuriteKey) || strings.Contains(out, `"accountKey":"`) {
			t.Fatalf("GET %s shows the key: %s", path, out)
		}
	}
	if n, err := e.a.DB.SealSecrets(ctx); err != nil || n != 0 {
		t.Fatalf("SealSecrets = %d, %v: the account key was stored in plain text", n, err)
	}
	for b, want := range map[string]string{
		body("az2", c1, azuriteKey):                  `already the \"az\" store's`,
		body("az2", c2, "d3Jvbmc="):                  "is not usable",
		body("az2", "missing-container", azuriteKey): "not found",
		`{"name":"az2","type":"azure"}`:              `needs \"azure\"`,
		body("az2", "Bad_Name", azuriteKey):          "containerName",
	} {
		if out := e.api("POST", "/-/api/blobstores", b, 400); !strings.Contains(out, want) {
			t.Errorf("%s: %s, want ~%s", b, out, want)
		}
	}

	e.api("POST", "/-/api/repositories", `{"name":"raw-az","format":"raw","type":"hosted","blobStore":"az"}`, 201)
	for _, f := range []string{"keep.txt", "gone.txt"} {
		if st, out := do(t, "PUT", e.ts.URL+"/repository/raw-az/"+f, "root", "content of "+f, false); st != 201 {
			t.Fatalf("upload %s: %d %s", f, st, out)
		}
	}
	keep, gone := e.sha("raw-az", "keep.txt"), e.sha("raw-az", "gone.txt")
	if !e.has("az", keep) || !e.has("az", gone) || e.has("default", keep) {
		t.Fatal("the blobs are not in the azure store alone")
	}
	if st, out := do(t, "GET", e.ts.URL+"/repository/raw-az/keep.txt", "root", "", false); st != 200 || out != "content of keep.txt" {
		t.Fatalf("download: %d %q", st, out)
	}
	r, _ := http.NewRequest("GET", e.ts.URL+"/repository/raw-az/keep.txt", nil)
	r.SetBasicAuth("root", "pw")
	r.Header.Set("Range", "bytes=11-14")
	if resp, err := http.DefaultClient.Do(r); err != nil || resp.StatusCode != 206 {
		t.Fatalf("range download: %v %v", resp, err)
	} else {
		b := make([]byte, 16)
		n, _ := resp.Body.Read(b)
		resp.Body.Close()
		if string(b[:n]) != "keep" {
			t.Fatalf("range %q", b[:n])
		}
	}
	if st, out := do(t, "DELETE", e.ts.URL+"/repository/raw-az/gone.txt", "root", "", false); st != 204 {
		t.Fatalf("delete: %d %s", st, out)
	}
	removed, freed, err := e.a.GCStore(ctx, "az", 0)
	if err != nil || removed != 1 || freed != int64(len("content of gone.txt")) {
		t.Fatalf("GCStore = %d %d %v", removed, freed, err)
	}
	if e.has("az", gone) || !e.has("az", keep) {
		t.Fatal("GC removed the wrong blob")
	}
	if m := decode(t, e.api("GET", "/-/api/blobstores/az", "", 200)); m["blobCount"] != float64(1) || m["repoCount"] != float64(1) {
		t.Fatalf("usage: %v", m)
	}
	if m := decode(t, e.api("GET", "/-/api/blobstores/az/health", "", 200)); m["healthy"] != true {
		t.Fatalf("health: %v", m)
	}

	// Credentials: a wrong key is refused before it is stored, the location
	// cannot change, an empty key keeps the stored one.
	if out := e.api("PUT", "/-/api/blobstores/az", `{"azure":{"accountKey":"d3Jvbmc="}}`, 400); !strings.Contains(out, "not usable") {
		t.Fatalf("wrong key: %s", out)
	}
	if out := e.api("PUT", "/-/api/blobstores/az", `{"azure":{"containerName":"`+c2+`"}}`, 400); !strings.Contains(out, "cannot change") {
		t.Fatalf("new container: %s", out)
	}
	if out := e.api("PUT", "/-/api/blobstores/az", `{"azure":{"authenticationMethod":"accountKey"},"quota":{"type":"spaceUsedBytes","limitBytes":1}}`, 200); !strings.Contains(out, `"accountKeySet":true`) ||
		!strings.Contains(out, `"quotaViolated":true`) {
		t.Fatalf("kept key and quota: %s", out)
	}
	if st, out := do(t, "GET", e.ts.URL+"/repository/raw-az/keep.txt", "root", "", false); st != 200 || out != "content of keep.txt" {
		t.Fatalf("download after the update: %d %q", st, out)
	}

	// The Nexus REST API.
	nx := `{"name":"aznx","softQuota":{"type":"spaceUsedQuota","limit":10},"bucketConfiguration":{"accountName":"` + azuriteAccount +
		`","containerName":"` + c2 + `","endpoint":"` + ep + `","authentication":{"authenticationMethod":"ACCOUNTKEY","accountKey":"` + azuriteKey + `"}}}`
	e.api("POST", "/service/rest/v1/blobstores/azure", nx, 204)
	got := decode(t, e.api("GET", "/service/rest/v1/blobstores/azure/aznx", "", 200))
	bc, _ := got["bucketConfiguration"].(map[string]any)
	auth, _ := bc["authentication"].(map[string]any)
	if got["name"] != "aznx" || bc["containerName"] != c2 || bc["accountName"] != azuriteAccount || auth["authenticationMethod"] != "ACCOUNTKEY" ||
		auth["accountKey"] != nil || got["softQuota"].(map[string]any)["limit"] != float64(10) {
		t.Fatalf("nexus GET: %v", got)
	}
	e.api("PUT", "/service/rest/v1/blobstores/azure/aznx", `{"name":"aznx","bucketConfiguration":{"accountName":"`+azuriteAccount+
		`","containerName":"`+c2+`","authentication":{"authenticationMethod":"ACCOUNTKEY"}}}`, 204)
	if got := decode(t, e.api("GET", "/service/rest/v1/blobstores/azure/aznx", "", 200)); got["softQuota"] != nil {
		t.Fatalf("nexus PUT without softQuota: %v", got)
	}
	e.api("GET", "/service/rest/v1/blobstores/azure/default", "", 404)
	if out := e.api("GET", "/service/rest/v1/blobstores", "", 200); strings.Count(out, `"type":"Azure Cloud Storage"`) != 2 {
		t.Fatalf("nexus list: %s", out)
	}
	// A group takes an azure member.
	e.api("POST", "/-/api/blobstores", `{"name":"gaz","type":"group","members":["aznx"]}`, 201)

	e.stop()
	e.start()
	if !e.a.Store.HasStore("az") || !e.a.Store.HasStore("gaz") {
		t.Fatal("azure store not opened at startup")
	}
	if st, out := do(t, "GET", e.ts.URL+"/repository/raw-az/keep.txt", "root", "", false); st != 200 || out != "content of keep.txt" {
		t.Fatalf("download after a restart: %d %q", st, out)
	}
}

// An azure store created on A takes uploads on B, and new credentials set on
// A reach B's open store in place.
func TestAzureStoreAcrossNodes(t *testing.T) {
	ep := os.Getenv("COMMANDCENTER_TEST_AZURITE_URL")
	if ep == "" {
		t.Skip("COMMANDCENTER_TEST_AZURITE_URL is not set")
	}
	c := azuriteContainer(t, ep)
	a, b := startCluster(t, "")
	a.want(t, "POST", "/-/api/blobstores", `{"name":"az","type":"azure","azure":{"accountName":"`+azuriteAccount+`","containerName":"`+c+
		`","endpoint":"`+ep+`","accountKey":"`+azuriteKey+`"}}`, ok(201))
	b.want(t, "POST", "/-/api/repositories", `{"name":"raw-az","format":"raw","type":"hosted","blobStore":"az"}`, ok(201))
	b.want(t, "PUT", "/repository/raw-az/f.txt", "in azure", ok(201))
	if got := a.want(t, "GET", "/repository/raw-az/f.txt", nil, ok(200)); got != "in azure" {
		t.Fatalf("read on A: %q", got)
	}
	a.want(t, "PUT", "/-/api/blobstores/az", `{"azure":{"tenantId":"tenant-2"}}`, ok(200))
	if !eventually(2*time.Second, func() bool { cfg, _ := b.Store.StoreByName("az").Azure(); return cfg.TenantID == "tenant-2" }) {
		t.Fatal("the new credentials did not reach B")
	}
	if got := b.want(t, "GET", "/repository/raw-az/f.txt", nil, ok(200)); got != "in azure" {
		t.Fatalf("read on B after the update: %q", got)
	}
}
