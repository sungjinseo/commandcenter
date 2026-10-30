package ui

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// gcsKeyJSON is a service account key file whose token_uri is tokenURI.
func gcsKeyJSON(t *testing.T, tokenURI string) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	b, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "cc@p.iam.gserviceaccount.com",
		"private_key_id": "k1", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": tokenURI})
	return string(b)
}

// A google store is created with its bucket checked through a token from
// its account key, shows its settings but never the key, keeps the key
// sealed, and is refused on a bucket and prefix another store (google or
// S3) has.
func TestBlobStoreGoogle(t *testing.T) {
	e := newEnv(t, "http://repo.example")
	withBlobStore(t, e)
	if err := e.db.SetSecretKey([]byte(strings.Repeat("k", 32))); err != nil {
		t.Fatal(err)
	}
	var bearer atomic.Value
	gcs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/token":
			io.WriteString(w, `{"access_token":"tok","expires_in":3600}`)
		case r.Method == http.MethodGet && r.URL.Path == "/storage/v1/b/bkt":
			bearer.Store(r.Header.Get("Authorization"))
			io.WriteString(w, `{"name":"bkt"}`)
		default:
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
		}
	}))
	defer gcs.Close()
	key := gcsKeyJSON(t, gcs.URL+"/token")
	body := func(name string, mutate func(g map[string]any, m map[string]any)) string {
		g := map[string]any{"bucket": "bkt", "prefix": "cc/", "endpoint": gcs.URL, "accountKey": key}
		m := map[string]any{"name": name, "type": "google", "google": g}
		if mutate != nil {
			mutate(g, m)
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	r := e.admin(t, "POST", "/-/api/blobstores", body("gcs", nil))
	wantStatus(t, r, http.StatusCreated)
	m := r.json(t)
	g, _ := m["google"].(map[string]any)
	if m["type"] != "google" || m["fsTotal"] != nil || g == nil || g["bucket"] != "bkt" || g["prefix"] != "cc" || g["endpoint"] != gcs.URL ||
		g["authentication"] != "accountKey" || g["accountKeySet"] != true || g["serviceAccount"] != "cc@p.iam.gserviceaccount.com" {
		t.Fatalf("create: %s", r.body)
	}
	if bearer.Load() != "Bearer tok" {
		t.Fatalf("bucket checked with Authorization %v", bearer.Load())
	}
	for _, path := range []string{"/-/api/blobstores", "/-/api/blobstores/gcs"} {
		r := e.admin(t, "GET", path, "")
		wantStatus(t, r, http.StatusOK)
		if strings.Contains(r.body, "PRIVATE KEY") || strings.Contains(r.body, `"accountKey":`) || !strings.Contains(r.body, `"type":"google"`) {
			t.Fatalf("GET %s: %s", path, r.body)
		}
	}
	if n, err := e.db.SealSecrets(context.Background()); err != nil || n != 0 {
		t.Fatalf("SealSecrets = %d, %v; the account key was stored in plain text", n, err)
	}
	if !e.ui.Store.HasStore("gcs") {
		t.Fatal("store not live")
	}

	for name, c := range map[string]struct {
		mutate func(g, m map[string]any)
		want   string
	}{
		"same bucket": {nil, `already the "gcs" store's`},
		"same bucket by s3": {func(g, m map[string]any) {
			m["type"], m["s3"] = "s3", map[string]any{"bucket": "bkt", "prefix": "cc", "endpoint": gcs.URL + "/", "pathStyle": true, "accessKeyId": "a", "secretAccessKey": "b"}
			delete(m, "google")
		}, `already the "gcs" store's`},
		"no google":      {func(g, m map[string]any) { delete(m, "google") }, `needs "google"`},
		"google on file": {func(g, m map[string]any) { m["type"] = "file" }, `"google" is a google store's`},
		"bad auth":       {func(g, m map[string]any) { g["authentication"] = "hmac" }, "authentication must be"},
		"bad key":        {func(g, m map[string]any) { g["accountKey"] = "{}" }, "service_account"},
		"none, no ep": {func(g, m map[string]any) {
			g["authentication"] = "none"
			delete(g, "endpoint")
			delete(g, "accountKey")
		}, "emulator"},
		"no bucket there": {func(g, m map[string]any) { g["bucket"] = "elsewhere" }, "not usable"},
	} {
		r := e.admin(t, "POST", "/-/api/blobstores", body("bad", c.mutate))
		if r.status != http.StatusBadRequest || !strings.Contains(fmt.Sprint(r.json(t)["error"]), c.want) || strings.Contains(r.body, "PRIVATE KEY") {
			t.Errorf("%s: %d %s (want 400 with %q)", name, r.status, r.body, c.want)
		}
	}
	if e.ui.Store.HasStore("bad") {
		t.Fatal("a refused store was added")
	}
	// Another prefix is another namespace.
	wantStatus(t, e.admin(t, "POST", "/-/api/blobstores", body("gcs2", func(g, m map[string]any) { g["prefix"] = "two" })), http.StatusCreated)
	wantStatus(t, e.admin(t, "DELETE", "/-/api/blobstores/gcs2", ""), http.StatusNoContent)
}
