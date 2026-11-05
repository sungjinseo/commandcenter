package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/app"
	"commandcenter/internal/blob/s3test"
	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
)

// No Nexus Repository runs in these tests. The source is nexusConfig, a
// stand-in whose answers follow the models of Nexus 3.91.1's public source
// (and, for LDAP, whose plugin is not public, its documented REST API), or
// a second Command Center, whose Nexus API answers the same shapes.

// ccServer is a Command Center with an administrator who signs in with a
// password, as -config needs.
type ccServer struct {
	app    *app.App
	url    string
	pwFile string
}

const adminPassword = "admin-password-1"

func newCC(t *testing.T) *ccServer {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.MinCost)
	dataDir := t.TempDir()
	cfg, err := config.Parse([]byte(`
dataDir: ` + filepath.ToSlash(dataDir) + `
database: {url: ` + strconv.Quote(metatest.URL(t, dataDir)) + `}
users:
  - { name: admin, passwordHash: "` + string(h) + `", roles: [admin] }
`))
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(ts.Close)
	pwFile := filepath.Join(t.TempDir(), "admin.pw")
	os.WriteFile(pwFile, []byte(adminPassword+"\n"), 0o600)
	return &ccServer{a, ts.URL, pwFile}
}

// do makes a request as user (the administrator for ""), as the web UI does.
func (c *ccServer) do(t *testing.T, user, password, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, c.url+path, rd)
	if user == "" {
		user, password = "admin", adminPassword
	}
	req.SetBasicAuth(user, password)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Command-Center-UI", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// must makes an administrator's request that has to succeed.
func (c *ccServer) must(t *testing.T, method, path string, body any) []byte {
	t.Helper()
	st, b := c.do(t, "", "", method, path, body)
	if st/100 != 2 {
		t.Fatalf("%s %s: %d %s", method, path, st, b)
	}
	return b
}

// get decodes an administrator's GET.
func (c *ccServer) get(t *testing.T, path string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(c.must(t, "GET", path, nil), &v); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return v
}

// named returns the object of the list at path whose field key is name.
func (c *ccServer) named(t *testing.T, path, key, name string) obj {
	t.Helper()
	l, _ := c.get(t, path).([]any)
	for _, x := range l {
		if o, _ := x.(obj); str(o, key) == name {
			return o
		}
	}
	return nil
}

// nexusConfig is the stand-in Nexus: the answer of each GET by its path and
// query, and the data of each ExtDirect method. It refuses every write but
// the ExtDirect calls, which read.
type nexusConfig struct {
	get      map[string]string // JSON
	direct   map[string]string // "action.method": JSON
	fallback http.Handler      // the repositories' content
	writes   []string
}

func (n *nexusConfig) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if u, p, ok := r.BasicAuth(); !ok || u != "nx" || p != "nx-secret" {
		w.WriteHeader(401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "POST" && r.URL.Path == "/service/extdirect" && n.direct != nil {
		var call struct {
			Action, Method, Type string
			Tid                  int
		}
		json.NewDecoder(r.Body).Decode(&call)
		data, ok := n.direct[call.Action+"."+call.Method]
		if !ok || call.Type != "rpc" {
			fmt.Fprintf(w, `{"tid":%d,"action":%q,"method":%q,"type":"exception","message":"no such method"}`, call.Tid, call.Action, call.Method)
			return
		}
		fmt.Fprintf(w, `{"tid":%d,"action":%q,"method":%q,"result":{"success":true,"data":%s},"type":"rpc"}`, call.Tid, call.Action, call.Method, data)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.URL.Path != "/service/extdirect" {
		n.writes = append(n.writes, r.Method+" "+r.URL.Path)
		w.WriteHeader(405)
		return
	}
	if body, ok := n.get[r.URL.RequestURI()]; ok {
		io.WriteString(w, body)
		return
	}
	if n.fallback != nil {
		n.fallback.ServeHTTP(w, r)
		return
	}
	w.WriteHeader(404)
	io.WriteString(w, `{"message":"not found"}`)
}

// importConfig runs import-nexus from the stand-in (or another server) to cc.
func importConfig(t *testing.T, from string, cc *ccServer, extra ...string) (string, error) {
	t.Helper()
	nxPw := filepath.Join(t.TempDir(), "nx.pw")
	os.WriteFile(nxPw, []byte("nx-secret\n"), 0o600)
	args := []string{"import-nexus", "-from", from, "-from-user", "nx", "-from-password-file", nxPw,
		"-to", cc.url, "-to-user", "admin", "-to-token-file", cc.pwFile}
	var out bytes.Buffer
	err := run(append(args, extra...), nil, &out)
	t.Logf("import-nexus %s: %v\n%s", strings.Join(extra, " "), err, out.String())
	return out.String(), err
}

// lineOf is the config line of kind and name in out, without its prefix.
func lineOf(out, kind, name string) string {
	for _, l := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(l, "config\t"+kind+"\t"+name+"\t"); ok {
			return rest
		}
	}
	return ""
}

func lastLine(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return lines[len(lines)-1]
}

const placeholder = "#~NXRM~PLACEHOLDER~PASSWORD~#" // what Nexus answers for a stored secret

// nexusFixture is a Nexus with one of everything, the awkward cases
// included: masked secrets, values only Nexus Pro has, a format Command
// Center lacks, a group in a group, a role listed before the role it
// contains, a user id Command Center refuses.
func nexusFixture(t *testing.T, s3 *s3test.Server) *nexusConfig {
	t.Helper()
	key := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "internal", "pgp", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		q, _ := json.Marshal(string(b))
		return string(q)
	}
	store := func(name, typ, quota string) string {
		return `{"softQuota":` + quota + `,"name":"` + name + `","type":"` + typ + `","unavailable":false,"blobCount":7,"totalSizeInBytes":4096,"availableSpaceInBytes":53687091200}`
	}
	hosted := func(name, format, store, policy, rest string) string {
		return `{"name":"` + name + `","format":"` + format + `","type":"hosted","url":"http://nexus:8081/repository/` + name + `","online":true,
			"storage":{"blobStoreName":"` + store + `","strictContentTypeValidation":true,"writePolicy":"` + policy + `"},
			"cleanup":null,"component":{"proprietaryComponents":true}` + rest + `}`
	}
	proxy := func(name, format, remote, auth, rest string) string {
		return `{"name":"` + name + `","format":"` + format + `","type":"proxy","url":"http://nexus:8081/repository/` + name + `","online":true,
			"storage":{"blobStoreName":"default","strictContentTypeValidation":true},"cleanup":{"policyNames":[]},
			"proxy":{"remoteUrl":"` + remote + `","contentMaxAge":-1,"metadataMaxAge":1440,"preserveEncodedCharacters":false},
			"negativeCache":{"enabled":true,"timeToLive":1440},
			"httpClient":{"blocked":false,"autoBlock":true,"connection":null,"authentication":` + auth + `},"routingRuleName":null` + rest + `}`
	}
	group := func(name, format, members string) string {
		return `{"name":"` + name + `","format":"` + format + `","type":"group","url":"http://nexus:8081/repository/` + name + `","online":true,
			"storage":{"blobStoreName":"default","strictContentTypeValidation":true},"group":{"memberNames":[` + members + `]}}`
	}
	priv := func(typ, name, rest string) string {
		return `{"type":"` + typ + `","name":"` + name + `","description":"made in Nexus","readOnly":false` + rest + `}`
	}
	task := func(typ, name, schedule, rest, props string) string {
		return `{"id":"0f6c1a2e-` + name + `","enabled":true,"name":"` + name + `","typeId":"` + typ + `","typeName":"x","status":"WAITING","statusDescription":"Waiting",
			"nextRun":null,"lastRun":null,"lastRunResult":null,"runnable":true,"stoppable":false,"timeZoneOffset":null,"alertEmail":null,"runPreviousPlan":false,
			"notificationCondition":"FAILURE","schedule":"` + schedule + `","startDate":null,"recurringDays":null,"cronExpression":null,"isReadOnlyUi":false` + rest + `,
			"properties":{".id":"0f6c1a2e-` + name + `",".name":"` + name + `",".typeId":"` + typ + `",".enabled":"true",".message":"x","lastRunState.endState":"OK"` + props + `}}`
	}
	c := s3.Config()
	n := &nexusConfig{direct: map[string]string{}, get: map[string]string{
		nx + "/blobstores": `[` + store("default", "File", "null") + `,` + store("releases", "File", `{"type":"spaceRemainingQuota","limit":10737418240}`) + `,` +
			store("pool", "Group", "null") + `,` + store("member-a", "File", "null") + `,` + store("member-b", "File", "null") + `,` +
			store("s3-keys", "S3", "null") + `,` + store("s3-role", "S3", `{"type":"spaceUsedQuota","limit":5368709120}`) + `,` +
			store("azure", "Azure Cloud Storage", "null") + `]`,
		nx + "/blobstores/group/pool": `{"softQuota":null,"members":["member-a","member-b"],"fillPolicy":"writeToFirst"}`,
		nx + "/blobstores/s3/s3-keys": `{"name":"s3-keys","softQuota":null,"type":"S3","bucketConfiguration":{"bucket":{"region":"eu-west-1","name":"corp-artifacts","prefix":"nexus"},
			"bucketSecurity":{"accessKeyId":"AKIAEXAMPLEKEYID","secretAccessKey":"` + placeholder + `"},"encryption":{"encryptionType":"s3ManagedEncryption"},
			"advancedBucketConnection":{"endpoint":"","forcePathStyle":false,"maxConnectionPoolSize":50}}}`,
		nx + "/blobstores/s3/s3-role": `{"name":"s3-role","softQuota":{"type":"spaceUsedQuota","limit":5368709120},"type":"S3","bucketConfiguration":{
			"bucket":{"region":"` + c.Region + `","name":"` + c.Bucket + `","prefix":"role"},
			"advancedBucketConnection":{"endpoint":"` + c.Endpoint + `","forcePathStyle":true,"maxConnectionPoolSize":50},"preSignedUrlEnabled":false}}`,

		// The web UI's resource: Nexus has no public REST one for policies.
		"/service/rest/internal/cleanup-policies": `[
			{"name":"old-prereleases","format":"maven2","notes":"30 days","criteriaLastBlobUpdated":30,"criteriaLastDownloaded":null,"retain":null,"sortBy":null,
			 "criteriaReleaseType":"PRERELEASES","criteriaAssetRegex":null,"inUseCount":1},
			{"name":"unused","format":"*","notes":null,"criteriaLastBlobUpdated":null,"criteriaLastDownloaded":90,"retain":null,"sortBy":null,
			 "criteriaReleaseType":null,"criteriaAssetRegex":null,"inUseCount":0},
			{"name":"bower-old","format":"bower","notes":null,"criteriaLastBlobUpdated":7,"criteriaLastDownloaded":null,"retain":null,"sortBy":null,
			 "criteriaReleaseType":null,"criteriaAssetRegex":null,"inUseCount":0}]`,
		nx + "/routing-rules": `[{"name":"no-internal","description":"keeps our own group off the mirror","mode":"BLOCK","matchers":["^/com/example/.*","^/org/example/.*"]}]`,
		nx + "/security/content-selectors": `[
			{"name":"team-a","type":"csel","description":"team A's artifacts","expression":"format == \"maven2\" and path =^ \"/com/example/a/\""},
			{"name":"docker-team","type":"csel","description":null,"expression":"path =~ \"/v2/team/.*\""},
			{"name":"legacy","type":"jexl","description":null,"expression":"repository.name == 'x'"}]`,

		nx + "/repositorySettings": `[` + strings.Join([]string{
			group("maven-all", "maven2", `"maven-public","maven-snapshots"`), // listed before the group it contains
			group("maven-public", "maven2", `"maven-releases","maven-central"`),
			hosted("maven-releases", "maven2", "releases", "ALLOW_ONCE", `,"cleanup":{"policyNames":["old-prereleases","bower-old"]},
				"maven":{"versionPolicy":"RELEASE","layoutPolicy":"STRICT","contentDisposition":"INLINE"}`),
			hosted("maven-snapshots", "maven2", "default", "ALLOW", `,"maven":{"versionPolicy":"SNAPSHOT","layoutPolicy":"PERMISSIVE","contentDisposition":"ATTACHMENT"}`),
			proxy("maven-central", "maven2", "https://repo1.maven.org/maven2/",
				`{"type":"username","username":"mirror","ntlmHost":null,"ntlmDomain":null,"preemptive":false}`,
				`,"routingRuleName":"no-internal","maven":{"versionPolicy":"RELEASE","layoutPolicy":"PERMISSIVE","contentDisposition":"INLINE"},
				 "httpClient":{"blocked":false,"autoBlock":false,"connection":{"retries":3,"userAgentSuffix":null,"timeout":60,"enableCircularRedirects":false,
				 "enableCookies":true,"useTrustStore":false},"authentication":{"type":"username","username":"mirror","ntlmHost":null,"ntlmDomain":null,"preemptive":false}}`),
			proxy("npm-proxy", "npm", "https://registry.npmjs.org", `{"type":"bearerToken","username":null,"ntlmHost":null,"ntlmDomain":null}`,
				`,"npm":{"removeNonCataloged":false,"removeQuarantined":true}`),
			proxy("raw-ntlm", "raw", "https://files.example.org/", `{"type":"ntlm","username":"svc","ntlmHost":"wks","ntlmDomain":"CORP"}`,
				`,"raw":{"contentDisposition":"ATTACHMENT"}`),
			proxy("nuget-v2", "nuget", "https://www.nuget.org/api/v2/", "null", `,"nugetProxy":{"queryCacheItemMaxAge":3600,"nugetVersion":"V2"}`),
			hosted("raw-offline", "raw", "pool", "DENY", `,"online":false,"raw":{"contentDisposition":"INLINE"}`),
			hosted("raw-s3", "raw", "s3-keys", "ALLOW", `,"raw":{"contentDisposition":"ATTACHMENT"}`),
			hosted("raw-role", "raw", "s3-role", "ALLOW", `,"raw":{"contentDisposition":"ATTACHMENT"}`),
			hosted("docker-hosted", "docker", "default", "ALLOW_ONCE", `,"docker":{"v1Enabled":true,"forceBasicAuth":false,"httpPort":8082,"httpsPort":null,"subdomain":null},
				"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW_ONCE","latestPolicy":true}`),
			proxy("docker-hub", "docker", "https://registry-1.docker.io", "null", `,"docker":{"v1Enabled":false,"forceBasicAuth":true,"httpPort":null,"httpsPort":null,"subdomain":null},
				"dockerProxy":{"indexType":"HUB","indexUrl":null,"cacheForeignLayers":true,"foreignLayerUrlWhitelist":[".*"]}`),
			hosted("apt-signed", "apt", "default", "ALLOW", `,"apt":{"distribution":"bookworm"},"aptSigning":{"keypair":`+key("ed25519.asc")+`,"passphrase":null}`),
			hosted("apt-locked", "apt", "default", "ALLOW", `,"apt":{"distribution":"jammy"},"aptSigning":{"keypair":`+key("ed25519-pass.asc")+`,"passphrase":null}`),
			proxy("apt-debian", "apt", "https://deb.debian.org/debian", "null", `,"apt":{"distribution":"bookworm","flat":false,"enforceDistribution":true},"aptSigning":null`),
			hosted("yum-hosted", "yum", "default", "ALLOW", `,"yum":{"repodataDepth":1,"deployPolicy":"PERMISSIVE"},"yumSigning":{"keypair":"k","passphrase":null}`),
			hosted("bower-hosted", "bower", "default", "ALLOW", ""),
			group("bower-all", "bower", `"bower-hosted"`),
		}, ",") + `]`,

		nx + "/security/privileges": `[
			{"type":"wildcard","name":"nx-all","description":"All permissions","readOnly":true,"pattern":"nexus:*"},
			{"type":"repository-view","name":"nx-repository-view-maven2-maven-releases-read","description":"x","readOnly":true,"format":"maven2","repository":"maven-releases","actions":["READ"]},` +
			priv("repository-view", "team-a-deploy", `,"format":"maven2","repository":"maven-releases","actions":["READ","EDIT"]`) + `,` +
			priv("repository-content-selector", "team-a-read", `,"format":"maven2","repository":"*-maven2","contentSelector":"team-a","actions":["BROWSE","READ"]`) + `,` +
			priv("application", "see-blobstores", `,"domain":"blobstores","actions":["READ"]`) + `,` +
			priv("wildcard", "see-tasks", `,"pattern":"nexus:tasks:read"`) + `,` +
			priv("script", "run-scripts", `,"scriptName":"*","actions":["RUN"]`) + `,` +
			priv("repository-view", "bower-read", `,"format":"bower","repository":"*","actions":["READ"]`) + `,` +
			priv("repository-admin", "raw-s3-admin", `,"format":"raw","repository":"raw-s3","actions":["ALL"]`) + `]`,
		nx + "/security/roles?source=default": `[
			{"id":"developers","source":"default","name":"Developers","description":"deploys to maven-releases","readOnly":false,
			 "privileges":["team-a-deploy","nx-repository-view-maven2-maven-releases-read","nx-repository-view-bower-*-read","nx-apikey-all","run-scripts"],"roles":["z-base"]},
			{"id":"nx-admin","source":"default","name":"nx-admin","description":"Administrator Role","readOnly":false,"privileges":["nx-all"],"roles":[]},
			{"id":"nx-anonymous","source":"default","name":"nx-anonymous","description":"Anonymous Role","readOnly":false,
			 "privileges":["nx-healthcheck-read","nx-search-read","nx-repository-view-*-*-read","nx-repository-view-*-*-browse"],"roles":[]},
			{"id":"ops","source":"default","name":"ops","description":"ops","readOnly":false,"privileges":["see-tasks","see-blobstores","nx-capabilities-all"],"roles":["nx-admin"]},
			{"id":"z-base","source":"default","name":"Base","description":"what everyone gets","readOnly":false,
			 "privileges":["nx-repository-view-*-*-browse","nx-component-upload","team-a-read"],"roles":[]}]`,
		nx + "/security/users?source=default": `[
			{"userId":"admin","firstName":"Administrator","lastName":"User","emailAddress":"admin@example.org","source":"default","status":"active","readOnly":false,"roles":["nx-admin"],"externalRoles":[]},
			{"userId":"anonymous","firstName":"Anonymous","lastName":"User","emailAddress":"anonymous@example.org","source":"default","status":"active","readOnly":false,"roles":["nx-anonymous"],"externalRoles":[]},
			{"userId":"alice","firstName":"Alice","lastName":"Kim","emailAddress":"alice@example.org","source":"default","status":"active","readOnly":false,"roles":["developers"],"externalRoles":[]},
			{"userId":"bob","firstName":"Bob","lastName":"Lee","emailAddress":"bob@example.org","source":"default","status":"changepassword","readOnly":false,"roles":["ops","ghosts"],"externalRoles":[]},
			{"userId":"carol","firstName":"Carol","lastName":"Park","emailAddress":"carol@example.org","source":"default","status":"locked","readOnly":false,"roles":["developers"],"externalRoles":[]},
			{"userId":"john doe","firstName":"John","lastName":"Doe","emailAddress":"john@example.org","source":"default","status":"active","readOnly":false,"roles":[],"externalRoles":[]}]`,
		nx + "/security/anonymous": `{"enabled":true,"userId":"anonymous","realmName":"NexusAuthorizingRealm"}`,
		// The LDAP plugin's source is not public: this is its documented REST shape.
		nx + "/security/ldap": `[
			{"id":"b2","order":2,"name":"backup","protocol":"ldap","useTrustStore":false,"host":"ldap2.example.org","port":389,"searchBase":"dc=example,dc=org",
			 "authScheme":"NONE","connectionTimeoutSeconds":30,"connectionRetryDelaySeconds":300,"maxIncidentsCount":3,"userBaseDn":"ou=people","userSubtree":false,
			 "userObjectClass":"inetOrgPerson","userIdAttribute":"uid","userRealNameAttribute":"cn","userEmailAddressAttribute":"mail","ldapGroupsAsRoles":false},
			{"id":"a1","order":1,"name":"corp","protocol":"ldaps","useTrustStore":true,"host":"ldap.example.org","port":636,"searchBase":"dc=example,dc=org",
			 "authScheme":"SIMPLE","authRealm":null,"authUsername":"cn=nexus,ou=services,dc=example,dc=org","connectionTimeoutSeconds":30,
			 "connectionRetryDelaySeconds":300,"maxIncidentsCount":3,"userBaseDn":"ou=people","userSubtree":true,"userObjectClass":"inetOrgPerson",
			 "userLdapFilter":"(employeeType=engineer)","userIdAttribute":"uid","userRealNameAttribute":"displayName","userEmailAddressAttribute":"mail",
			 "userPasswordAttribute":"","ldapGroupsAsRoles":true,"groupType":"dynamic","userMemberOfAttribute":"memberOf"}]`,
		nx + "/security/realms/active": `["NexusAuthenticatingRealm","LdapRealm","NpmToken","SamlRealm","DefaultRole","NexusAuthorizingRealm"]`,
		nx + "/email": `{"enabled":true,"host":"smtp.example.org","port":587,"username":"mailer","password":null,"fromAddress":"nexus@example.org","subjectPrefix":"[nexus]",
			"startTlsEnabled":true,"startTlsRequired":false,"sslOnConnectEnabled":false,"sslServerIdentityCheckEnabled":false,"nexusTrustStoreEnabled":false}`,
	}}
	n.direct["coreui_Task.read"] = `[` + strings.Join([]string{
		task("blobstore.compact", "compact-default", "daily", `,"startDate":"2026-01-05T02:30:00.000+09:00","alertEmail":"ops@example.org"`, `,"blobstoreName":"default"`),
		task("repository.maven.remove-snapshots", "old-snapshots", "weekly", `,"startDate":"2026-01-05T04:00:00.000+09:00","recurringDays":[1,4]`,
			`,"repositoryName":"maven-snapshots","minimumRetained":"2","snapshotRetentionDays":"30","removeIfReleased":"true","gracePeriodInDays":"7"`),
		task("repository.cleanup", "cleanup", "advanced", `,"cronExpression":"0 0 3 * * ?"`, ""),
		task("blobstore.delete-temp-files", "temp", "manual", "", `,"blobstoreName":"default"`),
		task("script", "groovy", "manual", "", `,"language":"groovy","source":"log.info('x')"`),
		task("firewall.audit", "audit", "manual", "", ""),
		task("repository.cleanup", "one-off", "internal", "", ""),
	}, ",") + `]`
	n.direct["coreui_HttpSettings.read"] = `{"userAgentSuffix":"corp-mirror","timeout":20,"retries":2,
		"httpEnabled":true,"httpHost":"proxy.example.org","httpPort":3128,"httpAuthEnabled":true,"httpAuthUsername":"proxyuser","httpAuthPassword":"` + placeholder + `",
		"httpAuthNtlmHost":null,"httpAuthNtlmDomain":null,
		"httpsEnabled":true,"httpsHost":"sproxy.example.org","httpsPort":3129,"httpsAuthEnabled":null,"httpsAuthUsername":null,"httpsAuthPassword":null,
		"httpsAuthNtlmHost":null,"httpsAuthNtlmDomain":null,"nonProxyHosts":["*.example.org","localhost","127.0.0.1"]}`
	return n
}

// One run makes the configuration of the stand-in Nexus on an empty
// Command Center; a dry run before it changes nothing, a run after it finds
// nothing to do, -update changes what differs and keeps what was entered by
// hand, and no secret is printed.
func TestImportNexusConfig(t *testing.T) {
	bucket := s3test.New(t)
	t.Setenv("AWS_ACCESS_KEY_ID", bucket.Config().AccessKey) // the role an S3 store without keys uses
	t.Setenv("AWS_SECRET_ACCESS_KEY", bucket.Config().SecretKey)
	// This process is the importer and both servers: the proxy the target
	// is given applies to the importer's requests too, hence 127.0.0.1 in
	// the fixture's nonProxyHosts.
	nexus := nexusFixture(t, bucket)
	src := httptest.NewServer(nexus)
	defer src.Close()
	cc := newCC(t)
	pwOut := filepath.Join(t.TempDir(), "passwords.txt")

	// A dry run: the plan, and nothing written anywhere.
	out, err := importConfig(t, src.URL, cc, "-config-only", "-dry-run", "-passwords-out", pwOut)
	if err != nil { // what Command Center would refuse shows in the run, not in the plan
		t.Fatalf("dry run: %v", err)
	}
	for kindName, want := range map[string]string{
		"blobstore\treleases": "would create", "repository\tmaven-all": "would create", "role\tdevelopers": "would create",
		"user\talice": "would create", "repository\tbower-hosted": "skipped (format bower is not supported)",
	} {
		k, n, _ := strings.Cut(kindName, "\t")
		if got := lineOf(out, k, n); !strings.HasPrefix(got, want) {
			t.Errorf("dry run: %s = %q, want %q", kindName, got, want)
		}
	}
	if !strings.HasPrefix(lastLine(out), "configuration (dry run, nothing was written): ") {
		t.Errorf("dry run: last line %q", lastLine(out))
	}
	if l, _ := cc.get(t, nx+"/repositorySettings").([]any); len(l) != 0 {
		t.Fatalf("the dry run made repositories: %v", l)
	}
	if _, err := os.Stat(pwOut); err == nil {
		t.Fatal("the dry run wrote the passwords file")
	}

	// The migration.
	out, err = importConfig(t, src.URL, cc, "-config-only", "-passwords-out", pwOut)
	first := out
	if err == nil || !strings.Contains(err.Error(), "2 errors") { // nuget-v2 and the user "john doe": refused, and the rest goes on
		t.Fatalf("import: %v", err)
	}
	for kindName, want := range map[string]string{
		"blobstore\tdefault":              "unchanged",
		"blobstore\treleases":             "created",
		"blobstore\tpool":                 "created",
		"blobstore\ts3-role":              "created",
		"blobstore\ts3-keys":              "skipped (its secret access key is not readable from Nexus)",
		"blobstore\tazure":                "skipped (the credentials of a Azure Cloud Storage blob store are not readable from Nexus)",
		"cleanup-policy\tunused":          "created",
		"cleanup-policy\told-prereleases": "created",
		"cleanup-policy\tbower-old":       "skipped (format bower is not supported)",
		"routing-rule\tno-internal":       "created",
		"content-selector\tteam-a":        "created",
		"content-selector\tdocker-team":   "created\tit names docker paths under /v2/",
		"content-selector\tlegacy":        "skipped (a jexl selector",
		"repository\tmaven-releases":      "created\twithout the cleanup policy bower-old, which is not on the target",
		"repository\tmaven-central":       "created",
		"repository\tmaven-public":        "created",
		"repository\tmaven-all":           "created",
		"repository\tnpm-proxy":           "created",
		"repository\traw-ntlm":            "created\tNTLM authentication to the remote (user svc) has no equivalent",
		"repository\tnuget-v2":            "error: POST /service/rest/v1/repositories/nuget/proxy: HTTP 400: nugetProxy.nugetVersion",
		"repository\traw-offline":         "created",
		"repository\traw-s3":              "skipped (its blob store s3-keys is missing on the target)",
		"repository\traw-role":            "created",
		"repository\tdocker-hosted":       "created\tits connector (httpPort 8082) is not carried over",
		"repository\tdocker-hub":          "created\tforeign layers are not cached",
		"repository\tapt-signed":          "created",
		"repository\tapt-locked":          "created",
		"repository\tapt-debian":          "created",
		"repository\tyum-hosted":          "created\tdeployPolicy PERMISSIVE has no equivalent",
		"repository\tbower-hosted":        "skipped (format bower is not supported)",
		"repository\tbower-all":           "skipped (format bower is not supported)",
		"privilege\tteam-a-deploy":        "created",
		"privilege\tteam-a-read":          "created",
		"privilege\tsee-blobstores":       "created",
		"privilege\tsee-tasks":            "created",
		"privilege\trun-scripts":          "skipped (a script privilege has no equivalent)",
		"privilege\tbower-read":           "skipped (format bower is not supported)",
		"privilege\traw-s3-admin":         "skipped (its repository raw-s3 is missing on the target)",
		"role\tz-base":                    "created",
		"role\tdevelopers":                "created\twithout privileges that the target does not have: nx-repository-view-bower-*-read, run-scripts",
		"role\tops":                       "created\twithout privileges that the target does not have: nx-capabilities-all",
		"role\tnx-anonymous":              "created",
		"user\tadmin":                     "differs: emailAddress, firstName, lastName (left as it is; -update changes it)",
		"user\tanonymous":                 "skipped (anonymous access is a setting of Command Center, not a user)",
		"user\talice":                     "created",
		"user\tbob":                       "created\twithout the role ghosts, which is not on the target",
		"user\tcarol":                     "created",
		"user\tjohn doe":                  "error: POST /service/rest/v1/security/users: HTTP 400: ",
		"anonymous\taccess":               "updated",
		"ldap\tcorp":                      "updated\tan LDAP group gives the role of its name",
		"ldap\tbackup":                    "skipped (Command Center has one LDAP server",
		"realms\tactive":                  "updated\tNexusAuthorizingRealm has no equivalent",
		"email\tsettings":                 "updated\tSTARTTLS is required once it is on",
		"task\tcompact-default":           "created",
		"task\told-snapshots":             "created",
		"task\tcleanup":                   "created",
		"task\ttemp":                      "created",
		"task\tgroovy":                    "skipped (no equivalent of script: Command Center has no script API)",
		"task\taudit":                     "skipped (no equivalent of firewall.audit: Command Center has no task of this type)",
		"task\tone-off":                   "skipped (a task Nexus runs by itself (schedule internal))",
		"http\tsettings":                  "updated\tNexus's HTTPS proxy sproxy.example.org:3129 is not carried over",
	} {
		k, n, _ := strings.Cut(kindName, "\t")
		if got := lineOf(out, k, n); !strings.HasPrefix(got, want) {
			t.Errorf("%s = %q, want %q", kindName, got, want)
		}
	}
	for _, want := range []string{
		"needs manual action: blobstore s3-keys: make this S3 blob store on Command Center (bucket corp-artifacts, region eu-west-1, prefix \"nexus\")",
		"needs manual action: blobstore azure: make this Azure Cloud Storage blob store",
		"needs manual action: repository maven-central: enter the password of its remote's user \"mirror\"",
		"needs manual action: repository npm-proxy: enter the bearer token of its remote",
		"needs manual action: repository apt-locked: enter its apt signing key and passphrase",
		"needs manual action: user alice: hand over the generated password from " + pwOut,
		"needs manual action: ldap corp: enter the bind DN \"cn=nexus,ou=services,dc=example,dc=org\" and its password",
		"needs manual action: realms: SamlRealm is on in Nexus",
		"needs manual action: realms: Nexus's Default Role realm is on",
		"needs manual action: email: enter the password of the SMTP user \"mailer\"",
		"needs manual action: http: enter the proxy's user \"proxyuser\" and its password",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	// The target had nobody but its administrator: it gets Nexus's rule that
	// a privilege, not a sign-in, lets someone read.
	if got := lineOf(out, "setting", "authenticatedRead"); !strings.HasPrefix(got, "updated\tsigned-in users read what their roles allow") {
		t.Errorf("setting authenticatedRead = %q", got)
	}
	if st, _ := cc.get(t, native+"/settings").(obj); st["authenticatedRead"] != false {
		t.Errorf("authenticatedRead is still on: %v", st["authenticatedRead"])
	}
	if strings.Contains(out, "needs manual action: repository apt-signed") {
		t.Error("the key that needs no passphrase was not carried over")
	}
	if !strings.HasPrefix(lastLine(out), "configuration: ") || !strings.Contains(lastLine(out), " 2 failed, ") {
		t.Errorf("last line %q", lastLine(out))
	}
	if len(nexus.writes) != 0 {
		t.Errorf("the source was written to: %v", nexus.writes)
	}

	// What Command Center holds now.
	repo := func(name string) obj { return cc.named(t, nx+"/repositorySettings", "name", name) }
	central := repo("maven-central")
	if hc := sub(central, "httpClient"); hc["authentication"] != nil || str(central, "routingRule") != "no-internal" ||
		sub(hc, "connection")["enableCookies"] != true || sub(central, "maven")["layoutPolicy"] != "PERMISSIVE" || sub(central, "proxy")["contentMaxAge"] != -1.0 {
		t.Errorf("maven-central: %v", central)
	}
	if g := list(sub(repo("maven-all"), "group"), "memberNames"); !reflect.DeepEqual(g, []string{"maven-public", "maven-snapshots"}) {
		t.Errorf("maven-all members: %v", g)
	}
	if r := repo("raw-offline"); r["online"] != false || sub(r, "storage")["blobStoreName"] != "pool" || sub(r, "storage")["writePolicy"] != "DENY" {
		t.Errorf("raw-offline: %v", r)
	}
	if repo("apt-signed")["aptSigning"] == nil || repo("apt-locked")["aptSigning"] != nil || sub(repo("apt-locked"), "apt")["distribution"] != "jammy" {
		t.Errorf("apt signing: %v, %v", repo("apt-signed"), repo("apt-locked"))
	}
	if y := sub(repo("yum-hosted"), "yum"); y["repodataDepth"] != 1.0 {
		t.Errorf("yum-hosted: %v", y)
	}
	if repo("nuget-v2") != nil || repo("raw-s3") != nil || repo("bower-hosted") != nil {
		t.Error("a repository that could not be migrated was made")
	}
	if s := cc.named(t, nx+"/blobstores", "name", "releases"); sub(s, "softQuota")["limit"] != 10737418240.0 {
		t.Errorf("releases quota: %v", s)
	}
	role := cc.named(t, nx+"/security/roles", "id", "developers")
	if p := list(role, "privileges"); !reflect.DeepEqual(p, []string{"team-a-deploy", "nx-repository-view-maven2-maven-releases-read", "nx-apikey-all"}) ||
		!reflect.DeepEqual(list(role, "roles"), []string{"z-base"}) {
		t.Errorf("developers: %v", role)
	}
	for id, want := range map[string]string{"alice": "active", "bob": "active", "carol": "disabled"} {
		if u := cc.named(t, nx+"/security/users", "userId", id); str(u, "status") != want || str(u, "lastName") == "" {
			t.Errorf("user %s: %v", id, u)
		}
	}
	if a, _ := cc.get(t, nx+"/security/anonymous").(obj); a["enabled"] != true {
		t.Errorf("anonymous: %v", a)
	}
	ldap, _ := cc.get(t, native+"/realms/ldap").(obj)
	if str(ldap, "url") != "ldaps://ldap.example.org:636" || str(ldap, "bindDN") != "" || str(ldap, "userBaseDN") != "ou=people,dc=example,dc=org" ||
		str(ldap, "userFilter") != "(&(objectClass=inetOrgPerson)(uid={user})(employeeType=engineer))" || str(ldap, "nameAttr") != "displayName" ||
		!strings.Contains(fmt.Sprint(ldap["groupMappings"]), "developers") || strings.Contains(fmt.Sprint(ldap["groupMappings"]), "admin") {
		t.Errorf("ldap: %v", ldap)
	}
	realms, _ := cc.get(t, nx+"/security/realms/active").([]any)
	if got := fmt.Sprint(realms); !strings.HasPrefix(got, "[NexusAuthenticatingRealm LdapRealm NpmToken DefaultRole ") || strings.Contains(got, "Saml") {
		t.Errorf("realms: %v", got)
	}
	if e, _ := cc.get(t, nx+"/email").(obj); e["host"] != "smtp.example.org" || e["startTlsRequired"] != true || e["enabled"] != true {
		t.Errorf("email: %v", e)
	}
	settings, _ := cc.get(t, native+"/settings").(obj)
	if h := sub(settings, "http"); h["userAgentSuffix"] != "corp-mirror" || h["timeoutSeconds"] != 20.0 || h["retries"] != 2.0 ||
		sub(h, "proxy")["host"] != "proxy.example.org" || sub(h, "proxy")["username"] != "" || len(list(sub(h, "proxy"), "nonProxyHosts")) != 3 {
		t.Errorf("http: %v", h)
	}
	tasks, _ := cc.get(t, native+"/tasks").(obj)
	schedules := map[string]string{}
	for _, x := range tasks["tasks"].([]any) {
		schedules[str(x.(obj), "name")] = str(x.(obj), "schedule")
	}
	if schedules["cleanup"] != "daily:03:00" || !strings.HasPrefix(schedules["old-snapshots"], "weekly:") || schedules["temp"] != "manual" {
		t.Errorf("task schedules: %v", schedules)
	}

	// The generated passwords: in the file (0600) and nowhere else, and they sign in.
	fi, err := os.Stat(pwOut)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("passwords file: %v %v", fi, err)
	}
	pwFile, _ := os.ReadFile(pwOut)
	passwords := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(pwFile)), "\n") {
		id, pw, _ := strings.Cut(l, "\t")
		passwords[id] = pw
	}
	if len(passwords) != 3 || len(passwords["alice"]) < 20 || passwords["alice"] == passwords["bob"] {
		t.Fatalf("passwords file holds %d users", len(passwords))
	}
	if st, b := cc.do(t, "alice", passwords["alice"], "GET", native+"/me", nil); st != 200 || !strings.Contains(string(b), `"user":"alice"`) {
		t.Errorf("alice cannot sign in with the generated password: %d %s", st, b)
	}
	keyText, _ := os.ReadFile(filepath.Join("..", "..", "internal", "pgp", "testdata", "ed25519.asc"))
	for _, secret := range []string{placeholder, "nx-secret", adminPassword, passwords["alice"], passwords["bob"], passwords["carol"],
		strings.Split(string(keyText), "\n")[2], "PRIVATE KEY", bucket.Config().SecretKey} {
		if strings.Contains(out, secret) {
			t.Errorf("the output holds a secret (%.12s...)", secret)
		}
	}

	// A second run finds nothing to do, and nothing new to ask for.
	out, err = importConfig(t, src.URL, cc, "-config-only", "-passwords-out", pwOut)
	if err == nil || !strings.Contains(err.Error(), "2 errors") {
		t.Fatalf("second run: %v", err)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "config\t") && !strings.Contains(l, "\tunchanged") && !strings.Contains(l, "\tskipped (") &&
			!strings.Contains(l, "\terror: ") && !strings.HasPrefix(l, "config\tuser\tadmin\tdiffers: ") {
			t.Errorf("second run: %s", l)
		}
	}
	if !strings.Contains(lastLine(out), ": 0 created, 0 updated, ") || !strings.Contains(lastLine(out), " 1 differ, ") {
		t.Errorf("second run: last line %q", lastLine(out))
	}
	if strings.Contains(out, "needs manual action: user ") || strings.Count(out, "needs manual action: ") >= strings.Count(first, "needs manual action: ") {
		t.Errorf("second run asks again for what the first made:\n%s", out)
	}
	if again, _ := os.ReadFile(pwOut); string(again) != string(pwFile) {
		t.Error("the second run changed the passwords file")
	}

	// The administrator enters a credential by hand; a change on the target
	// is reported and left alone, then made with -update, which keeps the
	// credential and what else the target has.
	cc.must(t, "PUT", nx+"/repositories/maven/proxy/maven-central", obj{"name": "maven-central", "online": true,
		"storage":       obj{"blobStoreName": "default", "strictContentTypeValidation": true},
		"proxy":         obj{"remoteUrl": "https://repo1.maven.org/maven2/", "contentMaxAge": -1, "metadataMaxAge": 5},
		"negativeCache": obj{"enabled": true, "timeToLive": 1440},
		"httpClient":    obj{"blocked": false, "autoBlock": false, "authentication": obj{"type": "username", "username": "mirror", "password": "remote-secret"}},
		"maven":         obj{"versionPolicy": "RELEASE", "layoutPolicy": "PERMISSIVE"}})
	cc.must(t, "PUT", nx+"/routing-rules/no-internal", obj{"name": "no-internal", "description": "changed here", "mode": "ALLOW", "matchers": []string{"^/x/.*"}})
	// The other manual actions: the LDAP bind, the proxy's credentials (and
	// another number of retries), the S3 store that needed its secret key.
	bind, _ := cc.get(t, native+"/realms/ldap").(obj)
	delete(bind, "bindPasswordSet")
	bind["bindDN"], bind["bindPassword"] = "cn=nexus,ou=services,dc=example,dc=org", "bind-secret"
	cc.must(t, "PUT", native+"/realms/ldap", bind)
	cc.must(t, "PUT", native+"/settings", obj{"http": obj{"userAgentSuffix": "corp-mirror", "connectTimeoutSeconds": 20, "timeoutSeconds": 20, "retries": 5,
		"proxy": obj{"host": "proxy.example.org", "port": 3128, "username": "proxyuser", "password": "proxy-secret", "nonProxyHosts": []string{"*.example.org", "localhost", "127.0.0.1"}}}})
	c := bucket.Config()
	cc.must(t, "POST", nx+"/blobstores/s3", obj{"name": "s3-keys", "bucketConfiguration": obj{"bucket": obj{"region": c.Region, "name": c.Bucket, "prefix": "keys"},
		"bucketSecurity": obj{"accessKeyId": c.AccessKey, "secretAccessKey": c.SecretKey}, "advancedBucketConnection": obj{"endpoint": c.Endpoint, "forcePathStyle": true}}})
	out, _ = importConfig(t, src.URL, cc, "-config-only")
	for kindName, want := range map[string]string{"blobstore\ts3-keys": "unchanged", "repository\traw-s3": "created", "privilege\traw-s3-admin": "created",
		"ldap\tcorp": "unchanged", "http\tsettings": "differs: retries (left as it is; -update changes it)"} {
		k, n, _ := strings.Cut(kindName, "\t")
		if got := lineOf(out, k, n); !strings.HasPrefix(got, want) {
			t.Errorf("after the manual actions: %s = %q, want %q", kindName, got, want)
		}
	}
	for _, done := range []string{"blobstore s3-keys", "ldap corp", "http: ", "repository maven-central"} {
		if strings.Contains(out, "needs manual action: "+done) {
			t.Errorf("%q is asked for again:\n%s", done, out)
		}
	}
	if got := lineOf(out, "routing-rule", "no-internal"); got != "differs: description, matchers, mode (left as it is; -update changes it)" {
		t.Errorf("routing rule: %q", got)
	}
	if got := lineOf(out, "repository", "maven-central"); !strings.HasPrefix(got, "differs: ") || !strings.Contains(got, "proxy.metadataMaxAge") {
		t.Errorf("maven-central: %q", got)
	}
	out, _ = importConfig(t, src.URL, cc, "-config-only", "-update")
	for kindName, want := range map[string]string{"routing-rule\tno-internal": "updated", "repository\tmaven-central": "updated", "user\tadmin": "updated",
		"http\tsettings": "updated"} {
		k, n, _ := strings.Cut(kindName, "\t")
		if got := lineOf(out, k, n); !strings.HasPrefix(got, want) {
			t.Errorf("-update: %s = %q", kindName, got)
		}
	}
	central = repo("maven-central")
	if a := sub(sub(central, "httpClient"), "authentication"); str(a, "username") != "mirror" || sub(central, "proxy")["metadataMaxAge"] != 1440.0 {
		t.Errorf("-update: maven-central %v", central)
	}
	if r := cc.named(t, nx+"/routing-rules", "name", "no-internal"); r["mode"] != "BLOCK" || len(list(r, "matchers")) != 2 {
		t.Errorf("-update: routing rule %v", r)
	}
	settings, _ = cc.get(t, native+"/settings").(obj)
	if h := sub(settings, "http"); h["retries"] != 2.0 || sub(h, "proxy")["username"] != "proxyuser" || sub(h, "proxy")["passwordSet"] != true {
		t.Errorf("-update: http %v", h)
	}
	if l, _ := cc.get(t, native+"/realms/ldap").(obj); l["bindPasswordSet"] != true || str(l, "bindDN") == "" {
		t.Errorf("-update: ldap %v", l)
	}
	out, _ = importConfig(t, src.URL, cc, "-config-only", "-update")
	if !strings.Contains(lastLine(out), ": 0 created, 0 updated, ") || !strings.Contains(lastLine(out), " 0 differ, ") {
		t.Errorf("after -update: last line %q", lastLine(out))
	}
}

// Anonymous access that Nexus limits to some repositories is not widened
// to all of them: the switch stays off and the administrator is told.
func TestImportNexusConfigAnonymousLimited(t *testing.T) {
	nexus := &nexusConfig{get: map[string]string{
		nx + "/security/anonymous": `{"enabled":true,"userId":"anonymous","realmName":"NexusAuthorizingRealm"}`,
		nx + "/security/roles?source=default": `[{"id":"public-only","source":"default","name":"public-only","description":"x","readOnly":false,
			"privileges":["nx-repository-view-maven2-maven-public-read"],"roles":[]}]`,
		nx + "/security/users?source=default": `[{"userId":"anonymous","firstName":"Anonymous","lastName":"User","emailAddress":"a@example.org","source":"default",
			"status":"active","readOnly":false,"roles":["public-only"],"externalRoles":[]}]`,
	}}
	src := httptest.NewServer(nexus)
	defer src.Close()
	cc := newCC(t)
	out, _ := importConfig(t, src.URL, cc, "-config-only")
	if got := lineOf(out, "anonymous", "access"); got != "unchanged" {
		t.Errorf("anonymous: %q", got)
	}
	if !strings.Contains(out, "needs manual action: anonymous: Nexus lets anonymous callers read what the roles of its user \"anonymous\" allow") {
		t.Errorf("no manual action:\n%s", out)
	}
	if a, _ := cc.get(t, nx+"/security/anonymous").(obj); a["enabled"] != false {
		t.Errorf("anonymous read was turned on: %v", a)
	}
	// What the stand-in does not serve is skipped, not an error.
	if got := lineOf(out, "blobstore", "*"); !strings.HasPrefix(got, "skipped (the source does not serve /service/rest/v1/blobstores)") {
		t.Errorf("blobstore: %q", got)
	}
}

// A target that has users of its own keeps authenticatedRead: turning it off
// could take their reads away, so it is left to the administrator.
func TestImportNexusConfigKeepsAuthenticatedReadOfATargetInUse(t *testing.T) {
	src := httptest.NewServer(&nexusConfig{get: map[string]string{
		nx + "/security/anonymous":            `{"enabled":false,"userId":"anonymous","realmName":"NexusAuthorizingRealm"}`,
		nx + "/security/users?source=default": `[]`,
	}})
	defer src.Close()
	cc := newCC(t)
	cc.must(t, http.MethodPost, native+"/users", obj{"id": "resident", "password": "longenough-1", "roles": []string{}})
	out, _ := importConfig(t, src.URL, cc, "-config-only")
	if !strings.Contains(out, "needs manual action: settings: Command Center lets every signed-in user read every repository (authenticatedRead)") ||
		lineOf(out, "setting", "authenticatedRead") != "" {
		t.Errorf("output:\n%s", out)
	}
	if st, _ := cc.get(t, native+"/settings").(obj); st["authenticatedRead"] != true {
		t.Errorf("authenticatedRead was changed: %v", st["authenticatedRead"])
	}
}

func TestImportNexusConfigFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-config-only", "-all"},
		{"-config-only", "-repo", "a"},
		{"-all", "-update"},
		{"-all", "-passwords-out", "x"},
	} {
		args = append([]string{"import-nexus", "-from", "http://nx", "-to", "http://cc"}, args...)
		if err := run(args, nil, &bytes.Buffer{}); err == nil || strings.Contains(err.Error(), "dial") || strings.Contains(err.Error(), "lookup") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestDiffers(t *testing.T) {
	want := obj{"name": "a", "online": false, "n": 5, "storage": obj{"blobStoreName": "default", "writePolicy": "ALLOW"}, "list": []string{"b", "a"},
		"memberNames": []string{"x", "y"}, "empty": obj{"a": nil, "b": false, "c": ""}, "absent": nil, "zero": 0}
	cur := obj{"name": "a", "online": false, "n": 5.0, "storage": obj{"blobStoreName": "default", "writePolicy": "ALLOW", "extra": 1.0},
		"list": []any{"a", "b"}, "memberNames": []any{"x", "y"}, "url": "http://x"}
	if d := differs(want, cur); len(d) != 0 {
		t.Errorf("equal objects differ: %v", d)
	}
	cur = obj{"name": "b", "online": true, "n": 6.0, "storage": obj{"blobStoreName": "default", "writePolicy": "DENY"}, "list": []any{"a"},
		"memberNames": []any{"y", "x"}, "empty": obj{"b": true}, "absent": "x", "zero": 1.0}
	got := differs(want, cur)
	sort.Strings(got)
	if exp := []string{"absent", "empty.b", "list", "memberNames", "n", "name", "online", "storage.writePolicy", "zero"}; !reflect.DeepEqual(got, exp) {
		t.Errorf("differs = %v, want %v", got, exp)
	}
	if d := differs(want, nil); len(d) == 0 {
		t.Error("an object equals none")
	}
}

func TestDependFirst(t *testing.T) {
	objs := []obj{{"id": "a", "roles": []any{"c", "gone"}}, {"id": "b", "roles": []any{"a"}}, {"id": "c", "roles": []any{"b"}}} // a cycle ends
	var got []string
	for _, o := range dependFirst(objs, "id", func(o obj) []string { return list(o, "roles") }) {
		got = append(got, str(o, "id"))
	}
	if !reflect.DeepEqual(got, []string{"b", "c", "a"}) {
		t.Errorf("order: %v", got)
	}
}

// Command Center as the source, end to end: one instance is configured
// through its API, import-nexus makes its configuration on a second, empty
// one, and the two are compared, through the Nexus API both answer and
// through the configuration document each exports.
func TestImportNexusConfigBetweenCommandCenters(t *testing.T) {
	bucket := s3test.New(t)
	a, b := newCC(t), newCC(t)
	key, _ := os.ReadFile(filepath.Join("..", "..", "internal", "pgp", "testdata", "ed25519.asc"))
	c := bucket.Config()
	storage := func(store string, hosted bool) obj {
		s := obj{"blobStoreName": store, "strictContentTypeValidation": true}
		if hosted {
			s["writePolicy"] = "ALLOW_ONCE"
		}
		return s
	}
	proxy := func(name, remote string, more obj) obj {
		r := obj{"name": name, "online": true, "storage": storage("default", false), "proxy": obj{"remoteUrl": remote, "contentMaxAge": 720, "metadataMaxAge": 15},
			"negativeCache": obj{"enabled": true, "timeToLive": 30}, "httpClient": obj{"blocked": false, "autoBlock": true}}
		for k, v := range more {
			r[k] = v
		}
		return r
	}
	for _, step := range []struct {
		method, path string
		body         any
	}{
		{"POST", nx + "/blobstores/file", obj{"name": "fast", "path": "fast", "softQuota": obj{"type": "spaceUsedQuota", "limit": 1 << 30}}},
		{"POST", nx + "/blobstores/file", obj{"name": "m1", "path": "m1"}},
		{"POST", nx + "/blobstores/file", obj{"name": "m2", "path": "m2"}},
		{"POST", nx + "/blobstores/group", obj{"name": "pool", "members": []string{"m2", "m1"}, "fillPolicy": "writeToFirst"}},
		{"POST", nx + "/blobstores/s3", obj{"name": "cloud", "bucketConfiguration": obj{"bucket": obj{"region": c.Region, "name": c.Bucket, "prefix": "cc"},
			"bucketSecurity": obj{"accessKeyId": c.AccessKey, "secretAccessKey": c.SecretKey}, "advancedBucketConnection": obj{"endpoint": c.Endpoint, "forcePathStyle": true}}}},
		{"POST", nx + "/cleanup-policies", obj{"name": "old-prereleases", "format": "maven2", "notes": "a month", "criteriaLastBlobUpdated": 30, "criteriaReleaseType": "PRERELEASES"}},
		{"POST", nx + "/cleanup-policies", obj{"name": "unused", "format": "*", "criteriaLastDownloaded": 90}},
		{"POST", nx + "/routing-rules", obj{"name": "no-internal", "description": "ours stay here", "mode": "BLOCK", "matchers": []string{"^/com/example/.*"}}},
		{"POST", nx + "/security/content-selectors", obj{"name": "team-a", "description": "team A", "expression": `format == "maven2" and path =^ "/com/example/a/"`}},
		{"POST", nx + "/repositories/maven/hosted", obj{"name": "maven-releases", "online": true, "storage": storage("fast", true),
			"cleanup": obj{"policyNames": []string{"old-prereleases", "unused"}}, "maven": obj{"versionPolicy": "RELEASE", "layoutPolicy": "STRICT", "contentDisposition": "ATTACHMENT"}}},
		{"POST", nx + "/repositories/maven/hosted", obj{"name": "maven-snapshots", "online": false, "storage": storage("default", true),
			"maven": obj{"versionPolicy": "SNAPSHOT", "layoutPolicy": "PERMISSIVE"}}},
		{"POST", nx + "/repositories/maven/proxy", proxy("maven-central", "https://repo1.maven.org/maven2/", obj{"routingRule": "no-internal",
			"maven": obj{"versionPolicy": "RELEASE", "layoutPolicy": "PERMISSIVE"},
			"httpClient": obj{"blocked": false, "autoBlock": false, "connection": obj{"retries": 2, "timeout": 45, "userAgentSuffix": "cc", "enableCookies": true},
				"authentication": obj{"type": "username", "username": "mirror", "password": "remote-secret"}}})},
		{"POST", nx + "/repositories/maven/group", obj{"name": "maven-public", "online": true,
			"group": obj{"memberNames": []string{"maven-releases", "maven-central"}, "writableMember": "maven-releases"}}},
		{"POST", nx + "/repositories/maven/group", obj{"name": "maven-all", "online": true, "group": obj{"memberNames": []string{"maven-snapshots", "maven-public"}}}},
		{"POST", nx + "/repositories/npm/proxy", proxy("npm-proxy", "https://registry.npmjs.org", obj{
			"httpClient": obj{"blocked": true, "autoBlock": true, "authentication": obj{"type": "bearerToken", "bearerToken": "npm-token"}}})},
		{"POST", nx + "/repositories/raw/hosted", obj{"name": "raw-pool", "online": true, "storage": storage("pool", true), "raw": obj{"contentDisposition": "INLINE"}}},
		{"POST", nx + "/repositories/raw/hosted", obj{"name": "raw-cloud", "online": true, "storage": storage("cloud", true)}},
		{"POST", nx + "/repositories/raw/group", obj{"name": "raw-all", "online": true, "group": obj{"memberNames": []string{"raw-pool"}}}},
		{"POST", nx + "/repositories/docker/hosted", obj{"name": "docker-hosted", "online": true, "storage": storage("default", true)}},
		{"POST", nx + "/repositories/docker/proxy", proxy("docker-hub", "https://registry-1.docker.io", nil)},
		{"POST", nx + "/repositories/apt/hosted", obj{"name": "apt-hosted", "online": true, "storage": storage("default", true),
			"apt": obj{"distribution": "bookworm"}, "aptSigning": obj{"keypair": string(key)}}},
		{"POST", nx + "/repositories/apt/proxy", proxy("apt-debian", "https://deb.debian.org/debian", obj{"apt": obj{"distribution": "bookworm", "flat": false, "enforceDistribution": true}})},
		{"POST", nx + "/repositories/yum/hosted", obj{"name": "yum-hosted", "online": true, "storage": storage("default", true), "yum": obj{"repodataDepth": 1, "deployPolicy": "STRICT"}}},
		{"POST", nx + "/repositories/nuget/proxy", proxy("nuget-org", "https://api.nuget.org/v3/index.json", obj{"nugetProxy": obj{"nugetVersion": "V3"}})},
		{"POST", nx + "/repositories/helm/hosted", obj{"name": "helm-hosted", "online": true, "storage": storage("default", true)}},
		{"POST", nx + "/repositories/pypi/proxy", proxy("pypi-proxy", "https://pypi.org/", nil)},
		{"POST", nx + "/repositories/go/proxy", proxy("go-proxy", "https://proxy.golang.org/", nil)},
		{"POST", nx + "/repositories/raw/proxy", proxy("raw-replica", "https://upstream.example.org/repository/raw-hosted/",
			obj{"replication": obj{"preemptivePullEnabled": true, "assetPathRegex": "^/docs/.*"}})},
		{"POST", nx + "/repositories/cargo/hosted", obj{"name": "cargo-hosted", "online": true, "storage": storage("default", true), "cargo": obj{"requireAuthentication": true}}},
		{"POST", nx + "/security/privileges/repository-view", obj{"name": "deploy-releases", "description": "deploy", "format": "maven2", "repository": "maven-releases", "actions": []string{"READ", "EDIT"}}},
		{"POST", nx + "/security/privileges/repository-content-selector", obj{"name": "team-a-read", "description": "", "format": "maven2", "repository": "*-maven2",
			"contentSelector": "team-a", "actions": []string{"BROWSE", "READ"}}},
		{"POST", nx + "/security/privileges/application", obj{"name": "see-blobstores", "description": "x", "domain": "blobstores", "actions": []string{"READ"}}},
		{"POST", nx + "/security/privileges/wildcard", obj{"name": "see-tasks", "description": "x", "pattern": "nexus:tasks:read"}},
		{"POST", nx + "/security/roles", obj{"id": "z-base", "name": "Base", "description": "everyone", "privileges": []string{"nx-repository-view-*-*-browse", "team-a-read"}, "roles": []string{}}},
		{"POST", nx + "/security/roles", obj{"id": "developers", "name": "Developers", "description": "deploys", "roles": []string{"z-base"},
			"privileges": []string{"deploy-releases", "nx-repository-view-maven2-maven-releases-read", "nx-repository-view-raw-raw-cloud-read", "see-tasks", "see-blobstores"}}},
		{"POST", nx + "/security/users", obj{"userId": "alice", "firstName": "Alice", "lastName": "Kim", "emailAddress": "alice@example.org", "password": "alice-password",
			"status": "active", "roles": []string{"developers", "nx-admin"}}},
		{"POST", nx + "/security/users", obj{"userId": "carol", "firstName": "Carol", "lastName": "Park", "emailAddress": "", "password": "carol-password",
			"status": "disabled", "roles": []string{"z-base"}}},
		{"PUT", nx + "/security/anonymous", obj{"enabled": true}},
		{"PUT", nx + "/email", obj{"enabled": true, "host": "smtp.example.org", "port": 465, "username": "mailer", "password": "smtp-secret", "fromAddress": "cc@example.org",
			"sslOnConnectEnabled": true, "sslServerIdentityCheckEnabled": true}},
		{"PUT", native + "/realms/ldap", obj{"url": "ldap://ldap.example.org", "userBaseDN": "ou=people,dc=example,dc=org"}},
		{"PUT", nx + "/security/realms/active", []string{"NpmToken", "NexusAuthenticatingRealm", "LdapRealm"}},
	} {
		a.must(t, step.method, step.path, step.body)
	}

	out, err := importConfig(t, a.url, b, "-config-only", "-from-user", "admin", "-from-password-file", a.pwFile)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for kindName, want := range map[string]string{
		"blobstore\tcloud":      "skipped (its secret access key is not readable from Nexus)",
		"repository\traw-cloud": "skipped (its blob store cloud is missing on the target)",
		"role\tdevelopers":      "created\twithout privileges that the target does not have: nx-repository-view-raw-raw-cloud-read",
		// What a Command Center does not answer in Nexus's shape.
		"ldap\t*": "skipped (the source does not serve /service/rest/v1/security/ldap)",
		"task\t*": "skipped (the source does not serve /service/extdirect)",
		"http\t*": "skipped (the source does not serve /service/extdirect)",
	} {
		k, n, _ := strings.Cut(kindName, "\t")
		if got := lineOf(out, k, n); !strings.HasPrefix(got, want) {
			t.Errorf("%s = %q, want %q", kindName, got, want)
		}
	}
	for _, want := range []string{
		"needs manual action: blobstore cloud: ", "needs manual action: repository maven-central: enter the password of its remote's user \"mirror\"",
		"needs manual action: repository npm-proxy: enter the bearer token", "needs manual action: repository apt-hosted: enter its apt signing key",
		"needs manual action: user alice: set a password", "needs manual action: realms: LdapRealm is on in Nexus",
		"needs manual action: email: enter the password of the SMTP user \"mailer\"",
		// Command Center's anonymous access has no user whose roles could be read.
		"needs manual action: anonymous: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	for _, secret := range []string{"remote-secret", "npm-token", "smtp-secret", "alice-password", c.SecretKey, "PRIVATE KEY"} {
		if strings.Contains(out, secret) {
			t.Errorf("the output holds the secret %q", secret)
		}
	}

	// The two instances through the Nexus API: equal but for what the source
	// does not hand out and what depends on it.
	skip := map[string]bool{"cloud": true, "raw-cloud": true}
	for _, l := range []struct{ path, key string }{
		{nx + "/blobstores", "name"}, {nx + "/cleanup-policies", "name"}, {nx + "/routing-rules", "name"}, {nx + "/security/content-selectors", "name"},
		{nx + "/repositorySettings", "name"}, {nx + "/security/privileges", "name"}, {nx + "/security/roles", "id"}, {nx + "/security/users", "userId"},
		{nx + "/blobstores/group", "name"},
	} {
		normal := func(cc *ccServer) map[string]obj {
			out := map[string]obj{}
			list, _ := cc.get(t, l.path).([]any)
			for _, x := range list {
				o := x.(obj)
				name := str(o, l.key)
				if skip[name] || strings.Contains(name, "-raw-cloud-") {
					continue
				}
				for _, k := range []string{"url", "blobCount", "totalSizeInBytes", "availableSpaceInBytes"} {
					delete(o, k)
				}
				if hc := sub(o, "httpClient"); hc != nil {
					hc["authentication"] = nil // asked for by hand, above
				}
				if name == "apt-hosted" {
					o["aptSigning"] = nil
				}
				if name == "developers" { // without the privilege of the repository that is not there
					o["privileges"] = strings.ReplaceAll(fmt.Sprint(o["privileges"]), " nx-repository-view-raw-raw-cloud-read", "")
				}
				out[name] = o
			}
			return out
		}
		got, want := normal(b), normal(a)
		if len(want) == 0 {
			t.Errorf("%s: the source lists nothing", l.path)
		}
		for name, w := range want {
			if !reflect.DeepEqual(got[name], w) {
				t.Errorf("%s %s:\n  source %v\n  target %v", l.path, name, w, got[name])
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: %d objects on the target, %d on the source", l.path, len(got), len(want))
		}
	}
	if got, want := b.get(t, nx+"/email"), a.get(t, nx+"/email"); !reflect.DeepEqual(got, want) {
		t.Errorf("e-mail:\n  source %v\n  target %v", want, got)
	}
	// The realms that are on, in order: the ldap realm waits for the LDAP
	// server, which a Command Center source does not answer.
	on := func(cc *ccServer) (ids []string) {
		for _, r := range cc.get(t, native+"/realms").(obj)["realms"].([]any) {
			if r.(obj)["enabled"] == true && r.(obj)["id"] != "ldap" {
				ids = append(ids, str(r.(obj), "id"))
			}
		}
		return ids
	}
	if got, want := on(b), on(a); !reflect.DeepEqual(got, want) || len(want) != 2 {
		t.Errorf("realms: source %v, target %v", want, got)
	}

	// The two instances as each exports its configuration (without
	// secrets): the same objects, in Command Center's own model.
	export := func(cc *ccServer) map[string]any {
		var doc map[string]any
		if err := json.Unmarshal(cc.must(t, "GET", native+"/configuration?format=json", nil), &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	da, db := export(a), export(b)
	for _, section := range []string{"blobStores", "routingRules", "contentSelectors", "cleanupPolicies", "repositories", "roles", "users"} {
		normal := func(doc map[string]any) string {
			var out []any
			list, _ := doc[section].([]any)
			for _, x := range list {
				o := x.(obj)
				if n := str(o, "name"); n == "cloud" || n == "raw-cloud" {
					continue
				}
				delete(o, "remoteAuth")
				if r, ok := o["remote"].(string); ok { // the source's Nexus API answers a remote with a "/" at its end
					o["remote"] = strings.TrimSuffix(r, "/")
				}
				if o["contentDisposition"] == "" { // what a Maven repository made without one answers as INLINE
					o["contentDisposition"] = "inline"
				}
				if privs, ok := o["privileges"].([]any); ok && section == "roles" {
					o["privileges"] = slices.DeleteFunc(privs, func(p any) bool { return p.(obj)["target"] == "repo:raw-cloud" })
				}
				out = append(out, o)
			}
			j, _ := json.MarshalIndent(out, "", " ")
			return string(j)
		}
		if got, want := normal(db), normal(da); got != want || len(want) < 10 {
			t.Errorf("exported %s differ:\nsource %s\ntarget %s", section, want, got)
		}
	}
	if s := sub(obj(db), "smtp"); !reflect.DeepEqual(s, sub(obj(da), "smtp")) || s["host"] != "smtp.example.org" {
		t.Errorf("exported smtp: source %v, target %v", da["smtp"], s)
	}

	// A second run has nothing to make.
	out, err = importConfig(t, a.url, b, "-config-only", "-from-user", "admin", "-from-password-file", a.pwFile)
	if err != nil || !strings.Contains(lastLine(out), ": 0 created, 0 updated, ") || !strings.Contains(lastLine(out), " 0 differ, ") {
		t.Errorf("second run: %v, %s", err, lastLine(out))
	}
}

// -config makes the configuration and then copies the content of the
// hosted repositories it made, in one run.
func TestImportNexusConfigThenContent(t *testing.T) {
	content := &fakeNexus{comps: map[string][]nexusComponent{}, files: map[string][]byte{}}
	hosted := func(name, store string) string {
		return `{"name":"` + name + `","format":"raw","type":"hosted","url":"x","online":true,"storage":{"blobStoreName":"` + store + `",
			"strictContentTypeValidation":false,"writePolicy":"ALLOW"},"cleanup":null,"component":{"proprietaryComponents":false},"raw":{"contentDisposition":"INLINE"}}`
	}
	nexus := &nexusConfig{fallback: content, get: map[string]string{
		nx + "/blobstores": `[{"softQuota":null,"name":"default","type":"File"},{"softQuota":null,"name":"lake","type":"S3"}]`,
		nx + "/blobstores/s3/lake": `{"name":"lake","bucketConfiguration":{"bucket":{"region":"us-east-1","name":"lake","prefix":""},
			"bucketSecurity":{"accessKeyId":"AKIAEXAMPLE","secretAccessKey":"` + placeholder + `"}}}`,
		nx + "/repositorySettings": `[` + hosted("raw-hosted", "default") + `,` + hosted("raw-lake", "lake") + `]`,
	}}
	src := httptest.NewServer(nexus)
	defer src.Close()
	content.url = src.URL
	content.repos = []nexusRepo{{"raw-hosted", "raw", "hosted"}, {"raw-lake", "raw", "hosted"}}
	content.comps["raw-hosted"] = []nexusComponent{{Name: "docs/readme.txt", Assets: []nexusAsset{content.asset("raw-hosted", "docs/readme.txt", []byte("hello raw"))}}}
	content.comps["raw-lake"] = []nexusComponent{{Name: "big.bin", Assets: []nexusAsset{content.asset("raw-lake", "big.bin", []byte("not copied"))}}}

	cc := newCC(t)
	out, err := importConfig(t, src.URL, cc, "-config", "-dry-run")
	if err != nil || lineOf(out, "repository", "raw-hosted") != "would create" || !strings.Contains(out, "raw-hosted\traw\tdocs/readme.txt\t\t1\t9\twould copy") ||
		content.downloads != 0 || cc.named(t, nx+"/repositorySettings", "name", "raw-hosted") != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	out, err = importConfig(t, src.URL, cc, "-config")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for _, want := range []string{"config\trepository\traw-hosted\tcreated\n", "config\trepository\traw-lake\tskipped (its blob store lake is missing on the target)\n",
		"raw-hosted\traw\tdocs/readme.txt\t\t1\t9\tok\n", "raw-lake: skipped (Command Center has no such repository: see its configuration line)\n",
		"  raw-hosted -> raw-hosted (raw): 1 copied, 0 skipped, 0 failed, 9 bytes\n", "needs manual action: blobstore lake: "} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if !strings.HasPrefix(lastLine(out), "configuration: 1 created, ") || content.downloads != 1 {
		t.Errorf("last line %q, %d downloads", lastLine(out), content.downloads)
	}
	if st, b := cc.do(t, "", "", "GET", "/repository/raw-hosted/docs/readme.txt", nil); st != 200 || string(b) != "hello raw" {
		t.Errorf("the file was not copied: %d %q", st, b)
	}
	// -repo with -config copies only what it names.
	out, err = importConfig(t, src.URL, cc, "-config", "-repo", "raw-hosted")
	if err != nil || strings.Contains(out, "raw-lake:") || !strings.Contains(out, "raw-hosted -> raw-hosted (raw): 0 copied, 1 skipped") {
		t.Errorf("-config -repo: %v\n%s", err, out)
	}
}
