package nexusapi_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"commandcenter/internal/blob/s3test"
)

// subset reports where want is not contained in got (JSON values): every
// key of a want object must be in got with a contained value.
func subset(want, got any, at string) string {
	if w, ok := want.(map[string]any); ok {
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: %v is not an object", at, got)
		}
		for k, v := range w {
			if d := subset(v, g[k], at+"."+k); d != "" {
				return d
			}
		}
		return ""
	}
	if !reflect.DeepEqual(want, got) {
		return fmt.Sprintf("%s: got %v, want %v", at, got, want)
	}
	return ""
}

func decodeMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return m
}

// wantField answers status with a Nexus validation array whose first id is id.
func (e *env) wantField(method, path, body string, id string) {
	e.t.Helper()
	b := e.want(method, path, "admin", body, 400)
	var errs []struct{ ID, Message string }
	if err := json.Unmarshal(b, &errs); err != nil || len(errs) == 0 || errs[0].ID != id {
		e.t.Fatalf("%s %s: want the id %q: %s", method, path, id, b)
	}
}

// The Terraform provider's life cycle of the closed formats' repositories:
// create with its body, read back every section it sent with the same
// values, update with the same body (no change), and the values Command
// Center cannot honour refused with the field as the id.
func TestFormatSectionsRoundTrip(t *testing.T) {
	e := start(t, true)
	const docker = `"docker":{"v1Enabled":false,"forceBasicAuth":true,"httpPort":null,"httpsPort":null,"subdomain":null}`
	const remote = `"proxy":{"remoteUrl":"%s","contentMaxAge":1440,"metadataMaxAge":1440},"negativeCache":{"enabled":true,"timeToLive":1440},"httpClient":{"blocked":false,"autoBlock":true}`
	hosted := func(name, extra string) string {
		return `{"name":"` + name + `","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW"},` +
			`"cleanup":{"policyNames":[]},"component":{"proprietaryComponents":false}` + extra + `}`
	}
	// Proxies and groups: strictContentTypeValidation is a hosted setting
	// here, so true is accepted and answered false (a known difference).
	proxy := func(name, url, extra string) string {
		return `{"name":"` + name + `","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":false},` +
			`"cleanup":{"policyNames":[]},` + fmt.Sprintf(remote, url) + extra + `}`
	}
	cases := []struct{ path, name, body string }{
		{"docker/hosted", "docker-h", strings.Replace(hosted("docker-h", ","+docker), `"writePolicy":"ALLOW"`, `"writePolicy":"ALLOW","latestPolicy":false`, 1)},
		{"docker/proxy", "docker-p", proxy("docker-p", "https://registry-1.docker.io", ","+docker+
			`,"dockerProxy":{"indexType":"HUB","indexUrl":null,"cacheForeignLayers":false,"foreignLayerUrlWhitelist":[]}`)},
		{"docker/group", "docker-g", `{"name":"docker-g","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":false},` +
			`"group":{"memberNames":["docker-h","docker-p"],"writableMember":"docker-h"},` + docker + `}`},
		{"yum/hosted", "yum-h", hosted("yum-h", `,"yum":{"repodataDepth":1,"deployPolicy":"STRICT"}`)},
		{"nuget/proxy", "nuget-p", proxy("nuget-p", "https://api.nuget.org/v3/index.json", `,"nugetProxy":{"queryCacheItemMaxAge":3600,"nugetVersion":"V3"}`)},
		{"npm/proxy", "npm-p", proxy("npm-p", "https://registry.npmjs.org", `,"npm":{"removeNonCataloged":false,"removeQuarantined":false}`)},
		{"pypi/proxy", "pypi-p", proxy("pypi-p", "https://pypi.org", `,"pypi":{"removeQuarantined":false}`)},
		{"cargo/hosted", "cargo-h", hosted("cargo-h", `,"cargo":{"requireAuthentication":true}`)},
		{"maven/group", "mvn-g", `{"name":"mvn-g","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":false},` +
			`"group":{"memberNames":["maven-releases"],"writableMember":null}}`},
	}
	for _, c := range cases {
		e.want("POST", v1+"/repositories/"+c.path, "admin", c.body, 201)
		got := decodeMap(t, e.want("GET", v1+"/repositories/"+c.path+"/"+c.name, "admin", "", 200))
		want := decodeMap(t, []byte(c.body))
		delete(want, "proxy") // remoteUrl comes back with a trailing slash
		if d := subset(want, got, c.name); d != "" {
			t.Errorf("read back: %s", d)
		}
		e.want("PUT", v1+"/repositories/"+c.path+"/"+c.name, "admin", c.body, 204)
		if again := decodeMap(t, e.want("GET", v1+"/repositories/"+c.path+"/"+c.name, "admin", "", 200)); !reflect.DeepEqual(again, got) {
			t.Errorf("%s changed on a PUT of the same body:\n%v\n%v", c.name, got, again)
		}
	}
	g := decodeMap(t, e.want("GET", v1+"/repositories/maven/group/mvn-g", "admin", "", 200))
	if _, ok := g["cleanup"]; ok {
		t.Errorf("a group has no cleanup: %v", g)
	}
	r := decodeMap(t, e.want("GET", v1+"/repositories/docker/proxy/docker-p", "admin", "", 200))
	if r["dockerProxy"].(map[string]any)["indexType"] != "HUB" {
		t.Errorf("dockerProxy = %v", r["dockerProxy"])
	}

	// They act: depth 1 refuses an RPM at the root; requireAuthentication
	// hides the repository from anonymous callers until it is false again.
	if st, b, _ := e.do("PUT", "/repository/yum-h/a-1.0-1.noarch.rpm", "admin", "x"); st != 400 || !strings.Contains(string(b), "depth 1") {
		t.Fatalf("root RPM at depth 1: %d %s", st, b)
	}
	e.want("GET", v1+"/repositories/cargo-h", "", "", 401)
	cargoOpen := strings.Replace(cases[7].body, `"requireAuthentication":true`, `"requireAuthentication":false`, 1)
	e.want("PUT", v1+"/repositories/cargo/hosted/cargo-h", "admin", cargoOpen, 204)
	e.want("GET", v1+"/repositories/cargo-h", "", "", 200)

	// What Command Center cannot do is refused, naming the field.
	for _, c := range []struct{ path, body, id string }{
		{"docker/hosted", strings.Replace(hosted("d1", ","+docker), `"httpPort":null`, `"httpPort":8082`, 1), "docker.httpPort"},
		{"docker/hosted", strings.Replace(hosted("d2", ","+docker), `"v1Enabled":false`, `"v1Enabled":true`, 1), "docker.v1Enabled"},
		{"docker/hosted", strings.Replace(hosted("d3", ","+docker), `"writePolicy":"ALLOW"`, `"writePolicy":"ALLOW","latestPolicy":true`, 1), "storage.latestPolicy"},
		{"docker/proxy", proxy("d4", "https://registry-1.docker.io", ","+docker+`,"dockerProxy":{"indexType":"HUB","cacheForeignLayers":true}`), "dockerProxy.cacheForeignLayers"},
		{"docker/proxy", proxy("d5", "https://quay.io", ","+docker+`,"dockerProxy":{"indexType":"CUSTOM","indexUrl":"https://i.example"}`), "dockerProxy.indexType"},
		{"nuget/proxy", proxy("n1", "https://www.nuget.org/api/v2/", `,"nugetProxy":{"nugetVersion":"V2"}`), "nugetProxy.nugetVersion"},
		{"npm/proxy", proxy("n2", "https://registry.npmjs.org", `,"npm":{"removeQuarantined":true}`), "npm.removeQuarantined"},
		{"pypi/proxy", proxy("p1", "https://pypi.org", `,"pypi":{"removeQuarantined":true}`), "pypi.removeQuarantined"},
		{"yum/hosted", hosted("y1", `,"yum":{"repodataDepth":1,"deployPolicy":"PERMISSIVE"}`), "yum.deployPolicy"},
		{"yum/proxy", proxy("y2", "https://mirror.example/el9/", `,"yumSigning":{"keypair":"-----BEGIN PGP"}`), "yumSigning.keypair"},
	} {
		e.wantField("POST", v1+"/repositories/"+c.path, c.body, c.id)
	}
	e.want("POST", v1+"/repositories/yum/hosted", "admin", hosted("y3", `,"yum":{"repodataDepth":3}`), 400)
	// A PUT cannot rename.
	e.wantField("PUT", v1+"/repositories/yum/hosted/yum-h", strings.Replace(cases[3].body, `"yum-h"`, `"yum-x"`, 1), "name")
}

// A Nexus PUT keeps what its body has no field for: the anonymous-read
// override and a yum repository's depth (earlier builds reset them).
func TestRepositoryPutKeepsCommandCenterSettings(t *testing.T) {
	e := start(t, true)
	if st, b := e.ui("POST", "/-/api/repositories", `{"name":"raw-private","format":"raw","type":"hosted","anonymousRead":false}`); st != 201 {
		t.Fatalf("create: %d %s", st, b)
	}
	if st, b := e.ui("POST", "/-/api/repositories", `{"name":"yum-deep","format":"yum","type":"hosted","depth":1}`); st != 201 {
		t.Fatalf("create: %d %s", st, b)
	}
	e.want("GET", v1+"/repositories/raw-private", "", "", 401)
	e.want("PUT", v1+"/repositories/raw/hosted/raw-private", "admin",
		`{"name":"raw-private","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":false,"writePolicy":"ALLOW"}}`, 204)
	e.want("GET", v1+"/repositories/raw-private", "", "", 401)
	e.want("PUT", v1+"/repositories/yum/hosted/yum-deep", "admin",
		`{"name":"yum-deep","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW"}}`, 204)
	s := decodeMap(t, e.want("GET", v1+"/repositories/yum/hosted/yum-deep", "admin", "", 200))
	if s["yum"].(map[string]any)["repodataDepth"] != float64(1) {
		t.Fatalf("depth reset: %v", s["yum"])
	}
}

// httpClient.authentication type bearerToken: stored as the remote's bearer
// token, never returned, sent as Authorization: Bearer; switching type
// clears the other credential.
func TestProxyBearerToken(t *testing.T) {
	e := start(t, true)
	var mu sync.Mutex
	var auth string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		io.WriteString(w, "hello")
	}))
	defer remote.Close()
	body := func(a string) string {
		return `{"name":"raw-tok","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true},` +
			`"proxy":{"remoteUrl":"` + remote.URL + `/","contentMaxAge":-1,"metadataMaxAge":1440},"negativeCache":{"enabled":true,"timeToLive":1440},` +
			`"httpClient":{"blocked":false,"autoBlock":true,"authentication":` + a + `}}`
	}
	sent := func(file string) string {
		t.Helper()
		e.want("GET", "/repository/raw-tok/"+file, "admin", "", 200)
		mu.Lock()
		defer mu.Unlock()
		return auth
	}
	authOf := func() any {
		t.Helper()
		s := decodeMap(t, e.want("GET", v1+"/repositories/raw/proxy/raw-tok", "admin", "", 200))
		return s["httpClient"].(map[string]any)["authentication"]
	}
	e.want("POST", v1+"/repositories/raw/proxy", "admin", body(`{"type":"bearerToken","bearerToken":"tok-1"}`), 201)
	if a := authOf(); !reflect.DeepEqual(a, map[string]any{"type": "bearerToken"}) {
		t.Fatalf("authentication = %v", a)
	}
	if got := sent("a.txt"); got != "Bearer tok-1" {
		t.Fatalf("sent %q", got)
	}
	e.want("PUT", v1+"/repositories/raw/proxy/raw-tok", "admin", body(`{"type":"bearerToken"}`), 204) // the token is write-only: kept
	if got := sent("b.txt"); got != "Bearer tok-1" {
		t.Fatalf("after a PUT without the token: %q", got)
	}
	e.want("PUT", v1+"/repositories/raw/proxy/raw-tok", "admin", body(`{"type":"username","username":"bob","password":"pw2"}`), 204)
	if a := authOf(); !reflect.DeepEqual(a, map[string]any{"type": "username", "username": "bob"}) {
		t.Fatalf("authentication = %v", a)
	}
	if got := sent("c.txt"); got != "Basic "+base64.StdEncoding.EncodeToString([]byte("bob:pw2")) {
		t.Fatalf("sent %q", got)
	}
	e.want("PUT", v1+"/repositories/raw/proxy/raw-tok", "admin", body(`{"type":"bearerToken","bearerToken":"tok-2"}`), 204)
	if got := sent("d.txt"); got != "Bearer tok-2" {
		t.Fatalf("sent %q", got)
	}
	if st, b := e.ui("GET", "/-/api/repositories/raw-tok/config", ""); st != 200 || !strings.Contains(string(b), `"remoteAuth":{"username":"","passwordSet":false,"bearerTokenSet":true}`) {
		t.Fatalf("the password survived the switch: %s", b)
	}
}

// The file blob store resource: create (limit in bytes), read, update,
// refuse a new path, delete (400 while in use), and the Beta path.
func TestFileBlobStoreLifecycle(t *testing.T) {
	e := start(t, false)
	e.want("POST", v1+"/blobstores/file", "admin", `{"name":"tf","path":"tf","softQuota":{"type":"spaceUsedQuota","limit":1073741824}}`, 204)
	if b := e.want("GET", v1+"/blobstores/file/tf", "admin", "", 200); string(b) != `{"path":"tf","softQuota":{"type":"spaceUsedQuota","limit":1073741824}}`+"\n" {
		t.Fatalf("GET = %s", b)
	}
	e.want("GET", v1+"/blobstores/file/tf", "ci", "", 403)
	e.want("GET", v1+"/blobstores/file/nope", "admin", "", 404)
	e.want("PUT", v1+"/blobstores/file/tf", "admin", `{"path":"tf","softQuota":null}`, 204)
	if b := e.want("GET", v1+"/blobstores/file/tf", "admin", "", 200); string(b) != `{"path":"tf","softQuota":null}`+"\n" {
		t.Fatalf("GET = %s", b)
	}
	e.wantField("PUT", v1+"/blobstores/file/tf", `{"path":"/elsewhere"}`, "path")
	if d := decodeMap(t, e.want("GET", v1+"/blobstores/file/default", "admin", "", 200)); !strings.HasPrefix(d["path"].(string), "/") {
		t.Fatalf("default store = %v", d)
	}
	if a, b := e.want("GET", "/service/rest/beta/blobstores/file/file/tf", "admin", "", 200), e.want("GET", v1+"/blobstores/file/tf", "admin", "", 200); string(a) != string(b) {
		t.Fatalf("beta %s, v1 %s", a, b)
	}

	e.want("POST", v1+"/repositories/raw/hosted", "admin", `{"name":"raw-tf","online":true,"storage":{"blobStoreName":"tf","strictContentTypeValidation":false,"writePolicy":"ALLOW"}}`, 201)
	e.want("DELETE", v1+"/blobstores/tf", "ci", "", 403)
	if m := decodeMap(t, e.want("DELETE", v1+"/blobstores/tf", "admin", "", 400)); m["id"] != "*" || !strings.Contains(m["message"].(string), "raw-tf") {
		t.Fatalf("in use: %v", m)
	}
	e.want("DELETE", v1+"/repositories/raw-tf", "admin", "", 204)
	e.want("DELETE", v1+"/blobstores/tf", "admin", "", 204)
	e.want("GET", v1+"/blobstores/file/tf", "admin", "", 404)
	e.want("DELETE", v1+"/blobstores/tf", "admin", "", 404)
}

// The S3 blob store resource in Nexus's JSON: create (201), read (the secret
// masked), the read body PUT back with a new quota (204), the Terraform
// body with the real secret, and a changed bucket refused.
func TestS3BlobStoreLifecycle(t *testing.T) {
	e := start(t, false)
	srv := s3test.New(t)
	c := srv.Config()
	body := func(name, prefix, security, sec string) string {
		return fmt.Sprintf(`{"name":%q,"softQuota":{"type":"spaceUsedQuota","limit":5000},"bucketConfiguration":{`+
			`"bucket":{"region":%q,"name":%q,"prefix":%q},%q:{"accessKeyId":%q,"secretAccessKey":%q},`+
			`"encryption":{"encryptionType":"s3ManagedEncryption"},"advancedConnection":{"endpoint":%q,"forcePathStyle":true}}}`,
			name, c.Region, c.Bucket, prefix, security, c.AccessKey, sec, c.Endpoint)
	}
	e.want("POST", v1+"/blobstores/s3", "ci", body("s3a", "cc", "bucketSecurity", c.SecretKey), 403)
	e.want("POST", v1+"/blobstores/s3", "admin", body("s3a", "cc", "bucketSecurity", c.SecretKey), 201)
	e.want("POST", v1+"/blobstores/s3", "admin", body("s3b", "cc2", "security", c.SecretKey), 201) // the request spelling
	b := e.want("GET", v1+"/blobstores/s3/s3a", "admin", "", 200)
	if strings.Contains(string(b), c.SecretKey) {
		t.Fatalf("the secret is shown: %s", b)
	}
	want := map[string]any{"name": "s3a", "type": "S3", "softQuota": map[string]any{"type": "spaceUsedQuota", "limit": float64(5000)},
		"bucketConfiguration": map[string]any{
			"bucket":                   map[string]any{"region": c.Region, "name": c.Bucket, "prefix": "cc"},
			"bucketSecurity":           map[string]any{"accessKeyId": c.AccessKey, "secretAccessKey": "#~NXRM~PLACEHOLDER~PASSWORD~#"},
			"encryption":               map[string]any{"encryptionType": "s3ManagedEncryption"},
			"advancedBucketConnection": map[string]any{"endpoint": c.Endpoint, "forcePathStyle": true},
			"activeRegion":             c.Region,
		}}
	if got := decodeMap(t, b); !reflect.DeepEqual(got, want) {
		t.Fatalf("GET:\n%v\nwant\n%v", got, want)
	}
	if got := decodeMap(t, e.want("GET", v1+"/blobstores/s3/s3b", "admin", "", 200)); got["bucketConfiguration"].(map[string]any)["bucketSecurity"] == nil {
		t.Fatalf("s3b = %v", got)
	}
	e.want("PUT", v1+"/blobstores/s3/s3a", "admin", strings.Replace(string(b), `"limit":5000`, `"limit":7000`, 1), 204) // the placeholder keeps the secret
	if got := decodeMap(t, e.want("GET", v1+"/blobstores/s3/s3a", "admin", "", 200)); got["softQuota"].(map[string]any)["limit"] != float64(7000) {
		t.Fatalf("quota = %v", got["softQuota"])
	}
	e.want("PUT", v1+"/blobstores/s3/s3a", "admin", body("s3a", "cc", "bucketSecurity", c.SecretKey), 204)
	e.wantField("PUT", v1+"/blobstores/s3/s3a", body("s3a", "other", "bucketSecurity", c.SecretKey), "bucketConfiguration")
	e.wantField("PUT", v1+"/blobstores/s3/s3a", body("s3a", "cc", "bucketSecurity", "wrong-secret"), "bucketConfiguration")
	e.wantField("PUT", v1+"/blobstores/s3/s3a", body("s3x", "cc", "bucketSecurity", c.SecretKey), "name")
	e.wantField("POST", v1+"/blobstores/s3", strings.Replace(body("s3c", "cc3", "bucketSecurity", c.SecretKey), `"accessKeyId"`, `"role":"arn:aws:iam::1:role/x","accessKeyId"`, 1),
		"bucketConfiguration.bucketSecurity.role")
	e.want("GET", v1+"/blobstores/s3/default", "admin", "", 400)
	e.want("GET", v1+"/blobstores/file/s3a", "admin", "", 400)
	e.want("GET", v1+"/blobstores/s3/nope", "admin", "", 404)
	if a, b := e.want("GET", "/service/rest/beta/blobstores/s3/s3/s3a", "admin", "", 200), e.want("GET", v1+"/blobstores/s3/s3a", "admin", "", 200); string(a) != string(b) {
		t.Fatalf("beta %s, v1 %s", a, b)
	}

	// Blobs go to the bucket.
	e.want("POST", v1+"/repositories/raw/hosted", "admin", `{"name":"raw-s3","online":true,"storage":{"blobStoreName":"s3a","strictContentTypeValidation":false,"writePolicy":"ALLOW"}}`, 201)
	before := len(srv.Keys())
	e.want("PUT", "/repository/raw-s3/a.txt", "admin", "on s3", 201)
	if len(srv.Keys()) <= before {
		t.Fatalf("no object written: %v", srv.Keys())
	}
	e.want("DELETE", v1+"/blobstores/s3b", "admin", "", 204)
}

// /v1/email over the SMTP settings: the TLS switches map onto none,
// starttls and tls, the password never comes back, verify reports the send.
func TestEmailCompat(t *testing.T) {
	e := start(t, false)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close() // nothing listens there
	plain := fmt.Sprintf(`{"enabled":true,"host":"127.0.0.1","port":%d,"username":"u","password":"p","fromAddress":"cc@example.com",`+
		`"subjectPrefix":null,"startTlsEnabled":false,"startTlsRequired":false,"sslOnConnectEnabled":false,`+
		`"sslServerIdentityCheckEnabled":false,"nexusTrustStoreEnabled":false}`, port)
	e.want("PUT", v1+"/email", "ci", plain, 403)
	e.want("PUT", v1+"/email", "admin", plain, 204)
	got := decodeMap(t, e.want("GET", v1+"/email", "admin", "", 200))
	want := decodeMap(t, []byte(plain))
	want["password"] = nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GET:\n%v\nwant\n%v", got, want)
	}
	if st, b := e.ui("GET", "/-/api/settings/smtp", ""); st != 200 || !strings.Contains(string(b), `"passwordSet":true`) {
		t.Fatalf("smtp: %s", b)
	}
	if m := decodeMap(t, e.want("POST", v1+"/email/verify", "admin", "admin@example.com", 400)); m["success"] != false || !strings.Contains(m["reason"].(string), "could not send") {
		t.Fatalf("verify = %v", m)
	}

	for _, c := range []struct{ from, to, id string }{
		{`"startTlsEnabled":false`, `"startTlsEnabled":true`, "startTlsRequired"}, // opportunistic STARTTLS
		{`"sslOnConnectEnabled":false`, `"sslOnConnectEnabled":true`, "sslServerIdentityCheckEnabled"},
		{`"subjectPrefix":null`, `"subjectPrefix":"[cc]"`, "subjectPrefix"},
		{fmt.Sprintf(`"port":%d,`, port), ``, "port"},
	} {
		e.wantField("PUT", v1+"/email", strings.Replace(plain, c.from, c.to, 1), c.id)
	}
	starttls := strings.NewReplacer(`"startTlsEnabled":false`, `"startTlsEnabled":true`, `"startTlsRequired":false`, `"startTlsRequired":true`,
		`"sslServerIdentityCheckEnabled":false`, `"sslServerIdentityCheckEnabled":true`, `"nexusTrustStoreEnabled":false`, `"nexusTrustStoreEnabled":true`).Replace(plain)
	e.want("PUT", v1+"/email", "admin", starttls, 204)
	got = decodeMap(t, e.want("GET", v1+"/email", "admin", "", 200))
	if d := subset(map[string]any{"startTlsEnabled": true, "startTlsRequired": true, "sslOnConnectEnabled": false, "sslServerIdentityCheckEnabled": true}, got, "email"); d != "" {
		t.Fatal(d)
	}
	if a, b := e.want("GET", "/service/rest/beta/email", "admin", "", 200), e.want("GET", v1+"/email", "admin", "", 200); string(a) != string(b) {
		t.Fatalf("beta %s, v1 %s", a, b)
	}
	e.want("DELETE", v1+"/email", "admin", "", 204)
	if got = decodeMap(t, e.want("GET", v1+"/email", "admin", "", 200)); got["enabled"] != false || got["host"] != "" {
		t.Fatalf("after DELETE: %v", got)
	}
	if _, b := e.ui("GET", "/-/api/settings/smtp", ""); !strings.Contains(string(b), `"passwordSet":false`) {
		t.Fatalf("password kept: %s", b)
	}
}

// Nexus's metrics paths: the old ones redirect, ping/prometheus/data follow
// the prometheus endpoint's rule, threads is admin only.
func TestMetricsCompat(t *testing.T) {
	e := start(t, false)
	for from, to := range map[string]string{
		"/service/metrics/ping":                            "/service/rest/metrics/ping",
		"/service/metrics/healthcheck":                     "/service/rest/v1/status/check",
		"/service/metrics/prometheus?name[]=go_goroutines": "/service/rest/metrics/prometheus?name[]=go_goroutines",
	} {
		if st, _, h := e.do("GET", from, "", ""); st != 301 || h.Get("Location") != to {
			t.Errorf("GET %s: %d %q", from, st, h.Get("Location"))
		}
	}
	if st, _, h := e.do("GET", "/service/rest/metrics/ping", "", ""); st != 401 || h.Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous ping: %d", st)
	}
	e.want("GET", "/service/rest/metrics/ping", "ci", "", 403)
	e.want("GET", "/service/rest/metrics/prometheus", "ci", "", 403)
	e.want("GET", "/service/rest/metrics/threads", "ci", "", 403)
	if b := e.want("GET", "/service/rest/metrics/ping", "admin", "", 200); string(b) != "pong" {
		t.Fatalf("ping = %q", b)
	}
	if b := e.want("GET", "/service/rest/metrics/prometheus", "admin", "", 200); !strings.Contains(string(b), "\ngo_goroutines ") || !strings.Contains(string(b), "commandcenter_build_info") {
		t.Fatalf("prometheus = %.300s", b)
	}
	b := e.want("GET", "/service/rest/metrics/prometheus?name[]=go_goroutines", "admin", "", 200)
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); len(lines) != 3 || !strings.HasPrefix(lines[2], "go_goroutines ") {
		t.Fatalf("filtered = %q", b)
	}
	st, b, h := e.do("GET", "/service/rest/metrics/data?pretty=true&download=true", "admin", "")
	m := decodeMap(t, b)
	gauges, _ := m["gauges"].(map[string]any)
	if st != 200 || m["version"] == nil || gauges["jvm.memory.heap.used"].(map[string]any)["value"].(float64) <= 0 ||
		!strings.Contains(h.Get("Content-Disposition"), "metrics.json") || !strings.Contains(string(b), "\n  ") {
		t.Fatalf("data: %d %s %v", st, b, h)
	}
	if b := e.want("GET", "/service/rest/metrics/threads", "admin", "", 200); !strings.Contains(string(b), "goroutine ") {
		t.Fatalf("threads = %.200s", b)
	}
}

// /service/rest/beta serves the v1 twins; GET /beta/repositories is the
// settings list; paths without a twin are 404.
func TestBetaAliases(t *testing.T) {
	e := start(t, false)
	same := func(beta, v1path string) {
		t.Helper()
		if a, b := e.want("GET", "/service/rest/beta"+beta, "admin", "", 200), e.want("GET", v1+v1path, "admin", "", 200); string(a) != string(b) {
			t.Fatalf("beta %s: %s\nv1 %s: %s", beta, a, v1path, b)
		}
	}
	same("/repositories", "/repositorySettings")
	same("/security/content-selectors", "/security/content-selectors")
	same("/routing-rules", "/routing-rules")
	same("/blobstores", "/blobstores")
	e.want("GET", "/service/rest/beta/repositories", "", "", 401)
	e.want("POST", "/service/rest/beta/repositories/raw/hosted", "admin", `{"name":"raw-beta","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":false,"writePolicy":"ALLOW"}}`, 201)
	same("/repositories/raw/hosted/raw-beta", "/repositories/raw/hosted/raw-beta")
	e.want("DELETE", "/service/rest/beta/repositories/raw-beta", "admin", "", 204)
	if m := decodeMap(t, e.want("GET", "/service/rest/beta/components?repository=maven-releases", "admin", "", 404)); m["id"] != "*" {
		t.Fatalf("no twin: %v", m)
	}
}

// The field-level shapes: single-message errors carry id "*", dates have
// milliseconds and +00:00, userId is a prefix, and the system information
// has system-environment, system-network and system-properties.
func TestNexusFieldShapes(t *testing.T) {
	e := start(t, false)
	if m := decodeMap(t, e.want("GET", v1+"/nope", "admin", "", 404)); m["id"] != "*" || m["message"] != "not found" {
		t.Fatalf("404 body = %v", m)
	}
	e.upload("maven-releases/org/x/a/1.0/a-1.0.jar")
	var p page[map[string]any]
	e.get(v1+"/assets?repository=maven-releases", "admin", &p)
	for _, a := range p.Items {
		if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}\+00:00$`).MatchString(a["lastModified"].(string)) {
			t.Fatalf("asset = %v", a)
		}
	}
	var users []map[string]any
	e.get(v1+"/security/users?userId=AD", "admin", &users)
	if len(users) != 1 || users[0]["userId"] != "admin" {
		t.Fatalf("users = %v", users)
	}
	e.get(v1+"/security/users?userId=dmin", "admin", &users)
	if len(users) != 0 {
		t.Fatalf("not a prefix: %v", users)
	}
	var info map[string]map[string]any
	if err := json.Unmarshal(e.want("GET", "/service/rest/atlas/system-information", "admin", "", 200), &info); err != nil {
		t.Fatal(err)
	}
	if info["system-environment"]["HTTPS_PROXY"] == nil || len(info["system-network"]) == 0 || info["system-properties"]["go.version"] == nil {
		t.Fatalf("sections: env %v network %v properties %v", info["system-environment"], info["system-network"], info["system-properties"])
	}

	// Recovery mode refuses Nexus-API deletes in Nexus's error shape.
	if st, b := e.ui("PUT", "/-/api/recovery", `{"enabled":true,"message":"restoring"}`); st != 200 {
		t.Fatalf("recovery: %d %s", st, b)
	}
	if m := decodeMap(t, e.want("DELETE", v1+"/components/abc", "admin", "", 503)); m["id"] != "*" || !strings.Contains(m["message"].(string), "restoring") {
		t.Fatalf("recovery refusal = %v", m)
	}
}
