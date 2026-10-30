package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// A google store through the real server, against fake-gcs-server: created
// with a service account key (its token from a test token endpoint), a raw
// hosted repository on it, upload, download whole and by range, delete and
// GC from the bucket, a soft quota, a group with a google member, the
// Nexus-compatible list, and a restart that opens it again.
func TestGCSStoreEndToEnd(t *testing.T) {
	u := strings.TrimSuffix(os.Getenv("COMMANDCENTER_TEST_FAKEGCS_URL"), "/")
	if u == "" {
		t.Skip("COMMANDCENTER_TEST_FAKEGCS_URL is not set (a fake-gcs-server, e.g. http://fakegcs:4443)")
	}
	rb := make([]byte, 6)
	rand.Read(rb)
	bucket := "cc-app-" + hex.EncodeToString(rb)
	resp, err := http.Post(u+"/storage/v1/b?project=test", "application/json", strings.NewReader(`{"name":"`+bucket+`"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("create bucket: %v %v", resp, err)
	}
	resp.Body.Close()
	inBucket := func(name string) bool {
		resp, err := http.Get(u + "/storage/v1/b/" + bucket + "/o/" + strings.ReplaceAll(name, "/", "%2F"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	var tokens atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || strings.Count(r.FormValue("assertion"), ".") != 2 {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		tokens.Add(1)
		io.WriteString(w, `{"access_token":"tok","expires_in":3600}`)
	}))
	defer ts.Close()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	key, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "cc@p.iam.gserviceaccount.com",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": ts.URL})
	store := func(name, prefix string) string {
		b, _ := json.Marshal(map[string]any{"name": name, "type": "google",
			"google": map[string]any{"bucket": bucket, "prefix": prefix, "endpoint": u, "accountKey": string(key)}})
		return string(b)
	}

	e := newGroupEnv(t)
	ctx := context.Background()
	v := decode(t, e.api("POST", "/-/api/blobstores", store("gcs", "cc"), 201))
	if v["type"] != "google" || tokens.Load() == 0 || strings.Contains(e.api("GET", "/-/api/blobstores/gcs", "", 200), "PRIVATE KEY") {
		t.Fatalf("created %v after %d token requests", v, tokens.Load())
	}
	e.api("POST", "/-/api/repositories", `{"name":"raw-gcs","format":"raw","type":"hosted","blobStore":"gcs"}`, 201)
	e.api("PUT", "/repository/raw-gcs/a/keep.txt", "keep me", 201)
	e.api("PUT", "/repository/raw-gcs/a/gone.txt", "gone soon", 201)
	keep, gone := e.sha("raw-gcs", "a/keep.txt"), e.sha("raw-gcs", "a/gone.txt")
	if !inBucket("cc/"+keep) || !inBucket("cc/"+gone) {
		t.Fatal("the blobs are not in the bucket")
	}
	if got := e.api("GET", "/repository/raw-gcs/a/keep.txt", "", 200); got != "keep me" {
		t.Fatalf("GET %q", got)
	}
	req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/repository/raw-gcs/a/keep.txt", nil)
	req.SetBasicAuth("root", "pw")
	req.Header.Set("Range", "bytes=2-4")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(b) != "ep " {
		t.Fatalf("range GET: %s %q", resp.Status, b)
	}
	e.api("DELETE", "/repository/raw-gcs/a/gone.txt", "", 204)
	removed, freed, err := e.a.GCStore(ctx, "gcs", 0)
	if err != nil || removed != 1 || freed != 9 {
		t.Fatalf("GCStore = %d, %d, %v; want 1 blob of 9 bytes", removed, freed, err)
	}
	if inBucket("cc/"+gone) || !inBucket("cc/"+keep) {
		t.Fatal("GC removed the wrong objects")
	}

	if v := decode(t, e.api("PUT", "/-/api/blobstores/gcs", `{"quota":{"type":"spaceUsedBytes","limitBytes":1}}`, 200)); v["quotaViolated"] != true {
		t.Fatalf("quota: %v", v)
	}
	e.api("POST", "/-/api/blobstores", store("gcs2", "two"), 201)
	e.api("POST", "/-/api/blobstores", `{"name":"gg","type":"group","members":["gcs2"]}`, 201)
	e.api("POST", "/-/api/repositories", `{"name":"raw-gg","format":"raw","type":"hosted","blobStore":"gg"}`, 201)
	e.api("PUT", "/repository/raw-gg/x.txt", "in the group", 201)
	if !inBucket("two/" + e.sha("raw-gg", "x.txt")) {
		t.Fatal("the group's blob is not under its member's prefix")
	}
	h := decode(t, e.api("GET", "/-/api/blobstores/gg/health", "", 200))
	if checks, _ := h["checks"].([]any); h["healthy"] != true || len(checks) != 1 ||
		!strings.Contains(checks[0].(map[string]any)["target"].(string), "member gcs2: bucket "+u+"/"+bucket+"/two") {
		t.Fatalf("group health: %v", h)
	}
	if out := e.api("GET", "/service/rest/v1/blobstores", "", 200); !strings.Contains(out, `"type":"Google Cloud Storage"`) {
		t.Fatalf("Nexus blob store list: %s", out)
	}

	e.stop()
	e.start()
	if got := e.api("GET", "/repository/raw-gcs/a/keep.txt", "", 200); got != "keep me" {
		t.Fatalf("after a restart: %q", got)
	}
	if got := e.api("GET", "/repository/raw-gg/x.txt", "", 200); got != "in the group" {
		t.Fatalf("group after a restart: %q", got)
	}
}
