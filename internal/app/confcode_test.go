package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob/s3test"
	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/ui"
)

const ccPass = "correct horse battery staple"

// ccNode is one Command Center instance on its own database (or a second
// node on another's).
type ccNode struct {
	a   *App
	url string
	cfg *config.Config
}

// ccConfig is an empty instance: one admin, no repositories, a secret key.
func ccConfig(t *testing.T, hash string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "secret.key")
	k := make([]byte, 32)
	rand.Read(k)
	if err := os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(k)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte("dataDir: " + filepath.Join(dir, "data") + "\nsecretKeyFile: " + key +
		"\nusers:\n  - { name: admin, passwordHash: \"" + hash + "\", roles: [admin] }\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.URL = metatest.URL(t, cfg.DataDir)
	return cfg
}

func ccStart(t *testing.T, cfg *config.Config) *ccNode {
	t.Helper()
	a, err := New(cfg, quiet(), WithLogLevel(new(slog.LevelVar)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(ts.Close)
	return &ccNode{a: a, url: ts.URL, cfg: cfg}
}

// call sends an admin's request (Basic auth: a password login) and returns
// the status and body.
func (n *ccNode) call(t *testing.T, method, path, body string, hdr ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, n.url+path, strings.NewReader(body))
	req.SetBasicAuth("admin", "admin-password")
	req.Header.Set(auth.UIHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (n *ccNode) must(t *testing.T, method, path, body string, want int) string {
	t.Helper()
	st, b := n.call(t, method, path, body)
	if st != want {
		t.Fatalf("%s %s: %d, want %d: %s", method, path, st, want, b)
	}
	return b
}

func (n *ccNode) export(t *testing.T, query string) string {
	t.Helper()
	st, b := n.call(t, "GET", "/-/api/configuration?"+query, "", ui.ConfigPassphraseHeader, ccPass)
	if st != 200 {
		t.Fatalf("export: %d %s", st, b)
	}
	return b
}

func (n *ccNode) apply(t *testing.T, query, doc string) (int, ui.ConfigResult, string) {
	t.Helper()
	st, b := n.call(t, "POST", "/-/api/configuration?"+query, doc, ui.ConfigPassphraseHeader, ccPass)
	var res ui.ConfigResult
	json.Unmarshal([]byte(b), &res)
	return st, res, b
}

func testCertPEM(t *testing.T) string {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "cc-test-ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// The secrets every section holds; none may appear in an export.
var ccSecrets = []string{"s3cr3t-proxy-pw", "s3cr3t-remote-pw", "s3cr3t-url-pw", "s3cr3t-bearer", "s3cr3t-smtp", "s3cr3t-ldap",
	"s3cr3t-oidc", "s3cr3t-crowd", "s3cr3t-webhook", "s3cr3t-repl-pw", "BEGIN PRIVATE KEY"}

// populate fills every section of n through the admin API.
func populate(t *testing.T, n *ccNode, s3 *s3test.Server) {
	t.Helper()
	q := strconv.Quote
	n.must(t, "POST", "/-/api/ssl/certificates", `{"pem":`+q(testCertPEM(t))+`}`, 201)
	n.must(t, "POST", "/-/api/blobstores", `{"name":"fast","quota":{"type":"spaceUsedBytes","limitBytes":1000000}}`, 201)
	n.must(t, "POST", "/-/api/blobstores", `{"name":"m1"}`, 201)
	n.must(t, "POST", "/-/api/blobstores", `{"name":"m2"}`, 201)
	n.must(t, "POST", "/-/api/blobstores", `{"name":"grp","type":"group","members":["m1","m2"],"fillPolicy":"writeToFirst"}`, 201)
	c := s3.Config()
	n.must(t, "POST", "/-/api/blobstores", `{"name":"s3","type":"s3","s3":{"bucket":`+q(c.Bucket)+`,"prefix":"cc","region":"us-east-1",`+
		`"endpoint":`+q(c.Endpoint)+`,"pathStyle":true,"accessKeyId":`+q(c.AccessKey)+`,"secretAccessKey":`+q(c.SecretKey)+`}}`, 201)
	n.must(t, "POST", "/-/api/routing-rules", `{"name":"no-snapshots","description":"d","mode":"BLOCK","matchers":[".*-SNAPSHOT.*"]}`, 201)
	n.must(t, "POST", "/-/api/content-selectors", `{"name":"acme","expression":"path =^ \"/org/acme/\""}`, 201)
	n.must(t, "POST", "/-/api/cleanup/policies", `{"name":"old","format":"maven","componentAgeDays":30}`, 201)
	n.must(t, "POST", "/-/api/repositories", `{"name":"rel","format":"maven","type":"hosted","blobStore":"fast","cleanupPolicy":"old","evaluation":"enabled"}`, 201)
	n.must(t, "POST", "/-/api/repositories", `{"name":"central","format":"maven","type":"proxy",`+
		`"remote":"https://bot:s3cr3t-url-pw@repo.example.com/maven2/","contentMaxAge":"1h","routingRule":"no-snapshots",`+
		`"remoteAuth":{"username":"ru","password":"s3cr3t-remote-pw"},"firewall":{"enabled":true,"blockSeverityAtLeast":"high"}}`, 201)
	n.must(t, "POST", "/-/api/repositories", `{"name":"site-a","format":"maven","type":"proxy",`+
		`"remote":"https://nexus-a.example.com/repository/maven-releases/","remoteAuth":{"username":"replicator","password":"s3cr3t-repl-pw"},`+
		`"replication":{"preemptivePullEnabled":true,"assetPathRegex":"^com/acme/","verifyChecksums":true}}`, 201)
	n.must(t, "POST", "/-/api/repositories", `{"name":"hub","format":"docker","type":"proxy","remote":"https://registry-1.docker.io",`+
		`"remoteAuth":{"bearerToken":"s3cr3t-bearer"},"blobStore":"s3"}`, 201)
	n.must(t, "POST", "/-/api/repositories", `{"name":"public","format":"maven","type":"group","members":["rel","central"]}`, 201)
	n.must(t, "POST", "/-/api/repositories", `{"name":"tf","format":"terraform","type":"hosted"}`, 201)
	n.must(t, "POST", "/-/api/roles", `{"id":"dev","description":"developers","privileges":[{"target":"repo:rel","actions":["read","browse"]},`+
		`{"target":"selector:acme","actions":["read"],"repos":["*"]}]}`, 201)
	n.must(t, "POST", "/-/api/users", `{"id":"alice","name":"Alice","email":"alice@example.com","password":"alice-password-1","roles":["dev"]}`, 201)
	n.must(t, "POST", "/-/api/users", `{"id":"bob","source":"ldap","roles":["dev"]}`, 201)
	n.must(t, "PUT", "/-/api/settings", `{"baseURL":"https://cc.example.com","maxUploadMiB":99,"anonymousRead":true,"metricsAnonymous":true,`+
		`"http":{"connectTimeoutSeconds":10,"timeoutSeconds":40,"retries":2,"proxy":{"host":"proxy.example.com","port":3128,"username":"pu","password":"s3cr3t-proxy-pw","nonProxyHosts":["*.local"]}},`+
		`"userTokens":{"enabled":true,"expirationDays":30},"defaultRoles":["dev"],"terraformDefaultRepository":"tf"}`, 200)
	n.must(t, "PUT", "/-/api/settings/smtp", `{"host":"smtp.example.com","port":587,"security":"starttls","username":"m",`+
		`"password":"s3cr3t-smtp","from":"cc@example.com","enabled":true}`, 200)
	n.must(t, "PUT", "/-/api/realms/ldap", `{"url":"ldap://ldap.example.com","bindDN":"cn=cc","bindPassword":"s3cr3t-ldap",`+
		`"userBaseDN":"ou=people,dc=example,dc=com","groupMappings":[{"group":"devs","roles":["dev"]}]}`, 200)
	n.must(t, "PUT", "/-/api/realms/oidc", `{"issuer":"https://idp.example.com","clientId":"cc","clientSecret":"s3cr3t-oidc","scopes":["openid","email"]}`, 200)
	n.must(t, "PUT", "/-/api/realms/saml", `{"displayName":"Corp SSO","defaultRoles":["dev"]}`, 200) // creates the SP key pair
	n.must(t, "PUT", "/-/api/realms/crowd", `{"url":"https://crowd.example.com","applicationName":"cc","applicationPassword":"s3cr3t-crowd"}`, 200)
	n.must(t, "PUT", "/-/api/realms/rutauth", `{"header":"X-Remote-User","trustedProxies":["10.0.0.0/8"]}`, 200)
	n.must(t, "PUT", "/-/api/realms", `{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"ldap","enabled":true},{"id":"crowd","enabled":false}]}`, 200)
	n.must(t, "POST", "/-/api/tasks", `{"name":"rebuild rel","type":"repository.rebuild-metadata","schedule":"daily:02:00","params":{"repo":"rel"},"notifyEmail":["ops@example.com"]}`, 201)
	n.must(t, "POST", "/-/api/tasks", `{"name":"purge tokens","type":"security.purge-expired-user-tokens","schedule":"manual","enabled":false}`, 201)
	n.must(t, "PUT", "/-/api/tasks/compact-blobstore", `{"schedule":"daily:04:30"}`, 200)
	n.must(t, "POST", "/-/api/webhooks", `{"name":"hook","url":"https://hooks.example.com/cc","secret":"s3cr3t-webhook","events":["repository.created"],"repo":"rel"}`, 201)
	n.must(t, "PUT", "/-/api/vulnerability/settings", `{"cacheHours":12,"evaluateHosted":true}`, 200)
	n.must(t, "PUT", "/-/api/logging", `{"level":"debug"}`, 200)
}

// withoutSecrets is a YAML document without its secrets block.
func withoutSecrets(t *testing.T, doc string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "secrets")
	return m
}

func TestConfigurationAsCode(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("admin-password"), bcrypt.MinCost)
	s3 := s3test.New(t)
	src := ccStart(t, ccConfig(t, string(h)))
	populate(t, src, s3)

	full := src.export(t, "secrets=encrypted&passwords=true")
	for _, s := range []string{"trustStore:", "blobStores:", "routingRules:", "contentSelectors:", "cleanupPolicies:", "repositories:",
		"roles:", "users:", "settings:", "smtp:", "ldap:", "oidc:", "saml:", "crowd:", "rutauth:", "realms:", "tasks:", "webhooks:",
		"vulnerability:", "logging:", "secrets:", "passwordHash:", "builtin: true", "kdf: scrypt"} {
		if !strings.Contains(full, s) {
			t.Errorf("export lacks %q", s)
		}
	}
	for _, s := range ccSecrets {
		if strings.Contains(full, s) {
			t.Errorf("export shows the secret %q", s)
		}
	}
	plain := src.export(t, "")
	if strings.Contains(plain, "passwordHash") || strings.Contains(plain, "secrets:") {
		t.Fatalf("a plain export holds secrets:\n%s", plain)
	}
	if !strings.Contains(plain, "remote: https://repo.example.com/maven2/") {
		t.Errorf("the remote is not shown without its credentials:\n%s", plain)
	}

	// Another empty instance takes the whole document, secrets included.
	dst := ccStart(t, ccConfig(t, string(h)))
	// A second node on its database: it must follow every change at once
	// (the notifications), not on the periodic reload.
	defer func(d time.Duration) { accessReloadEvery = d }(accessReloadEvery)
	accessReloadEvery = time.Hour
	dcfg := *dst.cfg
	dcfg.Listen = "127.0.0.1:0"
	peer, err := New(&dcfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- peer.Serve(ctx) }()
	defer func() { stop(); <-served; peer.Close() }()
	time.Sleep(200 * time.Millisecond) // the peer subscribes

	if st, _, b := dst.apply(t, "dryRun=true", full); st != 200 || !strings.Contains(b, `"action":"create"`) {
		t.Fatalf("dry run: %d %s", st, b)
	}
	if dst.a.Registry.Get("rel") != nil {
		t.Fatal("the dry run created a repository")
	}
	if st, b := dst.call(t, "POST", "/-/api/configuration?dryRun=true", full, ui.ConfigPassphraseHeader, "not the passphrase"); st != 400 || !strings.Contains(b, "do not decrypt") {
		t.Fatalf("wrong passphrase: %d %s", st, b)
	}
	if st, b := dst.call(t, "POST", "/-/api/configuration?dryRun=true", full); st != 400 || !strings.Contains(b, "passphrase") {
		t.Fatalf("no passphrase: %d %s", st, b)
	}
	st, res, b := dst.apply(t, "", full)
	if st != 200 || !res.OK {
		t.Fatalf("apply: %d %s", st, b)
	}

	// Round trip: the same documents back, the secrets restored.
	if got := dst.export(t, ""); got != plain {
		t.Fatalf("the round trip changed the document:\n--- source\n%s\n--- copy\n%s", plain, got)
	}
	if got := dst.export(t, "secrets=encrypted&passwords=true"); !reflect.DeepEqual(withoutSecrets(t, got), withoutSecrets(t, full)) {
		t.Fatalf("the round trip changed the document with secrets:\n%s", got)
	}
	ss, _ := src.a.DB.LoadSettings(context.Background())
	ds, _ := dst.a.DB.LoadSettings(context.Background())
	if ds.SMTP.Password != "s3cr3t-smtp" || ds.LDAP.BindPassword != "s3cr3t-ldap" || ds.OIDC.ClientSecret != "s3cr3t-oidc" ||
		ds.Crowd.ApplicationPassword != "s3cr3t-crowd" || ds.HTTP.Proxy.Password != "s3cr3t-proxy-pw" ||
		ds.SAML.SPPrivateKey == "" || ds.SAML.SPPrivateKey != ss.SAML.SPPrivateKey || ds.SAML.SPCertificate != ss.SAML.SPCertificate {
		t.Fatalf("secrets not restored: %+v", ds)
	}
	central := dst.a.Registry.Get("central")
	if central.Remote != "https://bot:s3cr3t-url-pw@repo.example.com/maven2/" || central.RemoteAuth.Password != "s3cr3t-remote-pw" ||
		dst.a.Registry.Get("hub").RemoteAuth.BearerToken != "s3cr3t-bearer" {
		t.Fatalf("repository secrets not restored: %+v %+v", central.Remote, central.RemoteAuth)
	}
	if vs := dst.a.Vuln.Settings(context.Background()); !vs.EvaluateHosted || vs.CacheHours != 12 {
		t.Fatalf("the vulnerability settings were not carried over: %+v", vs)
	}
	if ev := dst.a.Registry.Get("rel").Evaluation; ev != "enabled" {
		t.Fatalf("rel's evaluation override was not carried over: %q", ev)
	}
	if r := dst.a.Registry.Get("site-a").Replication; r == nil || !r.PreemptivePullEnabled || r.AssetPathRegex != "^com/acme/" || !r.VerifyChecksums {
		t.Fatalf("site-a's pull replication was not carried over: %+v", r)
	}
	if p := dst.a.Registry.Get("site-a").RemoteAuth; p == nil || p.Password != "s3cr3t-repl-pw" {
		t.Fatalf("site-a's remote password was not restored: %+v", p)
	}
	hooks, _ := dst.a.DB.ListWebhooks(context.Background())
	bs, _ := dst.a.DB.GetBlobStore(context.Background(), "s3")
	if len(hooks) != 1 || hooks[0].Secret != "s3cr3t-webhook" || bs.S3 == nil || bs.S3.SecretKey != s3.SecretKey {
		t.Fatalf("webhook or s3 secret not restored: %+v %+v", hooks, bs.S3)
	}
	su, _ := src.a.DB.GetUser(context.Background(), "alice")
	du, _ := dst.a.DB.GetUser(context.Background(), "alice")
	if du.PasswordHash != su.PasswordHash {
		t.Fatal("alice's password hash not restored")
	}
	if st, _ := dst.call(t, "GET", "/-/api/me", ""); st != 200 { // the admin still signs in
		t.Fatalf("admin cannot sign in after the apply: %d", st)
	}
	// The peer picked every change up through the notifications.
	deadline := time.Now().Add(5 * time.Second)
	for peer.Registry.Get("public") == nil || !peer.Auth.UserExists("alice") || peer.Store.StoreByName("grp") == nil {
		if time.Now().After(deadline) {
			t.Fatal("the second node did not follow the apply")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The same document again: nothing to do.
	if st, res, b := dst.apply(t, "dryRun=true&prune=true", full); st != 200 || res.Summary["create"]+res.Summary["update"]+res.Summary["delete"] != 0 {
		t.Fatalf("second dry run: %d %s", st, b)
	}

	// Plan accuracy: one field changed, one object added, one removed (prune).
	var doc map[string]any
	if err := json.Unmarshal([]byte(dst.export(t, "format=json&passwords=true")), &doc); err != nil {
		t.Fatal(err)
	}
	for _, r := range doc["repositories"].([]any) {
		if r := r.(map[string]any); r["name"] == "central" {
			r["contentMaxAge"] = "2h"
		}
	}
	doc["contentSelectors"] = append(doc["contentSelectors"].([]any), map[string]any{"name": "beta", "expression": `path =^ "/beta/"`})
	doc["webhooks"] = []any{}
	edited, _ := json.Marshal(doc)
	st, res, b = dst.apply(t, "dryRun=true&prune=true", string(edited))
	if st != 200 {
		t.Fatalf("dry run: %d %s", st, b)
	}
	var changed []string
	for _, s := range res.Steps {
		if s.Action != "unchanged" {
			changed = append(changed, s.Action+" "+s.Section+" "+s.Key)
		}
	}
	if strings.Join(changed, "; ") != "update repositories central; create contentSelectors beta; delete webhooks hook" &&
		strings.Join(changed, "; ") != "create contentSelectors beta; update repositories central; delete webhooks hook" {
		t.Fatalf("plan: %v", changed)
	}
	for _, s := range res.Steps {
		if s.Key == "central" && (len(s.Changes) != 1 || s.Changes[0].Field != "contentMaxAge" || string(s.Changes[0].From) != `"1h"` || string(s.Changes[0].To) != `"2h"`) {
			t.Fatalf("central's changes: %+v", s.Changes)
		}
	}
	if st, res, b := dst.apply(t, "prune=true", string(edited)); st != 200 || !res.OK {
		t.Fatalf("apply: %d %s", st, b)
	}
	if dst.a.Registry.Get("central").ContentMaxAge != config.Duration(2*time.Hour) {
		t.Fatal("contentMaxAge not applied")
	}
	if hooks, _ := dst.a.DB.ListWebhooks(context.Background()); len(hooks) != 0 {
		t.Fatal("the pruned webhook is still there")
	}
	if st, res, b := dst.apply(t, "dryRun=true&prune=true", string(edited)); st != 200 || res.Summary["create"]+res.Summary["update"]+res.Summary["delete"] != 0 {
		t.Fatalf("dry run after the apply: %d %s", st, b)
	}
	for deadline := time.Now().Add(5 * time.Second); peer.Registry.Get("central").ContentMaxAge != config.Duration(2*time.Hour); {
		if time.Now().After(deadline) {
			t.Fatal("the second node did not follow the update")
		}
		time.Sleep(50 * time.Millisecond)
	}

	before := dst.export(t, "")

	// Prune never takes the last admin: the lockout check refuses the
	// document before anything changes.
	users := []any{}
	for _, u := range doc["users"].([]any) {
		if u.(map[string]any)["id"] != "admin" {
			users = append(users, u)
		}
	}
	lock := map[string]any{"version": 1, "users": users, "routingRules": doc["routingRules"]}
	lb, _ := json.Marshal(lock)
	if st, _, b := dst.apply(t, "prune=true", string(lb)); st != 400 || !strings.Contains(b, "leave no admin") {
		t.Fatalf("lockout: %d %s", st, b)
	}
	if !dst.a.Auth.UserExists("admin") {
		t.Fatal("the admin was deleted")
	}

	// An invalid document is refused whole, every problem listed.
	bad := `{"version":1,"routingRules":[{"name":"ok-rule","mode":"ALLOW","matchers":["x"]},{"name":"bad rule","mode":"MAYBE","matchers":["("]}],` +
		`"repositories":[{"name":"broken","format":"maven","type":"hosted","blobStore":"nowhere"},{"name":"typo","format":"maven","type":"hosted","colour":"red"}]}`
	st, _, b = dst.apply(t, "", bad)
	for _, want := range []string{"invalid routing rule name", "mode must be", "invalid regular expression", `blobStores \"nowhere\" would not exist`, "colour"} {
		if st != 400 || !strings.Contains(b, want) {
			t.Errorf("invalid document: %d, want %q in %s", st, want, b)
		}
	}
	if got := dst.export(t, ""); got != before {
		t.Fatal("a refused document changed the configuration")
	}

	// A step the handler refuses midway undoes the steps before it: the
	// LDAP server changes without its bind password (secrets left out).
	ldapDoc := doc["ldap"].(map[string]any)
	ldapDoc["url"] = "ldap://other.example.com"
	part := map[string]any{"version": 1, "routingRules": append(doc["routingRules"].([]any),
		map[string]any{"name": "added", "mode": "BLOCK", "matchers": []any{"x"}}), "ldap": ldapDoc}
	pb, _ := json.Marshal(part)
	st, res, b = dst.apply(t, "", string(pb))
	if st != 409 || res.OK || !res.RolledBack || !strings.Contains(res.Error, "re-enter the bind password") {
		t.Fatalf("failing apply: %d %s", st, b)
	}
	status := map[string]string{}
	for _, s := range res.Steps {
		status[s.Section+" "+s.Key] = s.Status
	}
	if status["routingRules added"] != "rolledBack" || status["ldap "] != "failed" {
		t.Fatalf("statuses: %v", status)
	}
	if got := dst.export(t, ""); got != before {
		t.Fatal("the rolled back apply left changes")
	}

	// Nexus's configuration API: export with its header, upload without
	// overwriting.
	if st, _ := dst.call(t, "GET", "/service/rest/v1/configuration", ""); st != 400 {
		t.Fatalf("Nexus export without the protocol header: %d", st)
	}
	st, b = dst.call(t, "GET", "/service/rest/v1/configuration", "", "X-Nexus-Migration-Protocol-Version", "1")
	if st != 200 || !json.Valid([]byte(b)) || !strings.Contains(b, `"repositories"`) {
		t.Fatalf("Nexus export: %d %s", st, b)
	}
	part = map[string]any{"version": 1, "routingRules": []any{map[string]any{"name": "no-snapshots", "mode": "ALLOW", "matchers": []any{"y"}},
		map[string]any{"name": "nexus-added", "mode": "BLOCK", "matchers": []any{"z"}}}}
	pb, _ = json.Marshal(part)
	if st, b := dst.call(t, "PUT", "/service/rest/v1/configuration", string(pb)); st != 204 {
		t.Fatalf("Nexus upload: %d %s", st, b)
	}
	rules, _ := dst.a.DB.ListRoutingRules(context.Background())
	if len(rules) != 2 || rules[0].Name != "nexus-added" || rules[1].Name != "no-snapshots" || rules[1].Mode != "BLOCK" {
		t.Fatalf("the upload overwrote or missed rules: %+v", rules)
	}
}

// An admin token may dry-run a document without secrets (drift checks),
// never apply one.
func TestConfigurationTokens(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("admin-password"), bcrypt.MinCost)
	n := ccStart(t, ccConfig(t, string(h)))
	_, tok, err := n.a.Auth.CreateToken(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	doc := `{"version":1,"routingRules":[{"name":"r","mode":"BLOCK","matchers":["x"]}]}`
	send := func(q string, doc string) int {
		req, _ := http.NewRequest("POST", n.url+"/-/api/configuration?"+q, strings.NewReader(doc))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set(auth.UIHeader, "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if st := send("dryRun=true", doc); st != 200 {
		t.Fatalf("token dry run: %d", st)
	}
	if st := send("", doc); st != 403 {
		t.Fatalf("token apply: %d", st)
	}
	// A secret in the document would let the token compare stored ones.
	if st := send("dryRun=true", `{"version":1,"smtp":{"host":"smtp.example.com","password":"a guess"}}`); st != 400 {
		t.Fatalf("token dry run with a secret: %d", st)
	}
	req, _ := http.NewRequest("GET", n.url+"/-/api/configuration?secrets=encrypted", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set(ui.ConfigPassphraseHeader, ccPass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("token export of secrets: %d", resp.StatusCode)
	}
}
