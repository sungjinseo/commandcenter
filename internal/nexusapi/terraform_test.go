package nexusapi_test

import (
	"bufio"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"commandcenter/internal/blob/s3test"
	"commandcenter/internal/ldap/ldaptest"
	"commandcenter/internal/saml/samltest"
)

// The Terraform provider for Nexus (datadrivers/nexus 3.0.1, with
// go-nexus-client 1.20.0) against this API.
// testdata/terraform-provider.jsonl holds every write the provider sent
// while test/compat/terraform.sh ran its configuration through a logging
// proxy: the first apply (create), the apply of step 2 (update), the way
// back to step 1 (back) and the destroy, one request a line, with what
// depends on the run as @NAMES@. The Azure blob store and the repository on
// it are left out, which need an Azure endpoint (the script has one).

// tfRequest is one line of the record.
type tfRequest struct {
	Step, Method, Path string
	Status             int
	Body               json.RawMessage // a JSON body, or
	Type, Text         string          // another one and its Content-Type
}

// tfRead is where the provider reads what a write changed, "" when it
// reads nothing back (a delete, a password, a certificate).
func tfRead(method, p string, body map[string]any) string {
	name := func(key string) string { s, _ := body[key].(string); return s }
	switch {
	case method == "DELETE" || strings.HasSuffix(p, "/change-password") || strings.HasSuffix(p, "/change-order") || strings.HasPrefix(p, "/security/ssl/"):
		return ""
	case method == "POST" && (strings.HasPrefix(p, "/repositories/") || strings.HasPrefix(p, "/blobstores/")):
		return p + "/" + name("name")
	case strings.HasPrefix(p, "/security/privileges/"):
		return "/security/privileges/" + name("name")
	case p == "/security/roles":
		return p + "/" + name("id")
	case strings.HasPrefix(p, "/security/users"):
		return "/security/users?userId=" + name("userId")
	case p == "/security/content-selectors" || p == "/routing-rules" || p == "/security/ldap":
		return p + "/" + name("name")
	}
	return p // a PUT of a named object or of a setting
}

// tfHeld reports where the answer does not hold what the provider sent.
// What is never returned is skipped (passwords and keys), as is what the
// provider does not read (a user's source; the name in a file blob store's
// answer, which Nexus leaves out too); an empty string is an absent value.
func tfHeld(sent, read any, at string) string {
	switch s := sent.(type) {
	case map[string]any:
		r, ok := read.(map[string]any)
		if !ok {
			return at + ": read " + jsonText(read)
		}
		for k, v := range s {
			switch k {
			case "password", "keypair", "passphrase", "secretAccessKey", "authPassword", "source", "id":
				continue
			}
			if d := tfHeld(v, r[k], at+"."+k); d != "" {
				return d
			}
		}
		return ""
	case string:
		if s == "" && read == nil {
			return ""
		}
	}
	if !reflect.DeepEqual(sent, read) {
		return at + ": sent " + jsonText(sent) + ", read " + jsonText(read)
	}
	return ""
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestTerraformProviderRequests replays the record: every request must get
// the status the provider got, and what the provider then reads must hold
// every value it sent, or its next plan is not empty.
func TestTerraformProviderRequests(t *testing.T) {
	e := start(t, false)
	s3 := s3test.New(t).Config()
	idp := samltest.New(t)
	aptKey, err := os.ReadFile("../pgp/testdata/ed25519.asc")
	if err != nil {
		t.Fatal(err)
	}
	inJSON := func(s string) string { // s as the inside of a JSON string
		b, _ := json.Marshal(s)
		return string(b[1 : len(b)-1])
	}
	fill := strings.NewReplacer(
		"@APT_KEY@", inJSON(string(aptKey)),
		"@IDP_METADATA@", inJSON(idp.Metadata()+"\n"), // as a file has it: read back as written
		"@CERTIFICATE@", inJSON(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Cert.Raw}))),
		"@S3_ENDPOINT@", s3.Endpoint, "@S3_BUCKET@", s3.Bucket, "@S3_REGION@", s3.Region,
		"@S3_ACCESS_KEY@", s3.AccessKey, "@S3_SECRET_KEY@", s3.SecretKey)

	// The provider has no resource for a cleanup policy: the one its repositories name.
	e.want("POST", v1+"/cleanup-policies", "admin", `{"name":"tf-cleanup","format":"*","criteriaLastBlobUpdated":30}`, 201)

	send := func(method, path, ctype, body string) (int, []byte) {
		req, _ := http.NewRequest(method, e.ts.URL+v1+path, strings.NewReader(body))
		req.SetBasicAuth("admin", "pw")
		req.Header.Set("Content-Type", ctype)
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	settings := func(path string) map[string]any {
		return decodeMap(t, e.want("GET", v1+path, "admin", "", 200))
	}

	f, err := os.Open("testdata/terraform-provider.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	lines.Buffer(nil, 1<<20)
	var step, certID string
	count := map[string]int{}
	for n := 1; lines.Scan(); n++ {
		var q tfRequest
		if err := json.Unmarshal([]byte(fill.Replace(lines.Text())), &q); err != nil {
			t.Fatalf("line %d: %v", n, err)
		}
		if step == "update" && q.Step == "back" {
			// Step 2 took the proxy's credentials, cleanup policy and routing
			// rule away by leaving them out of its PUT.
			p := settings("/repositories/maven/proxy/tf-maven-proxy")
			if p["cleanup"] != nil || p["httpClient"].(map[string]any)["authentication"] != nil || p["routingRule"] != "" {
				t.Errorf("after the update the proxy keeps what the provider removed: %v", p)
			}
		}
		step = q.Step
		count[step]++
		path := strings.Replace(q.Path, "@CERTIFICATE_ID@", certID, 1)
		ctype, body := "application/json", string(q.Body)
		if q.Body == nil {
			ctype, body = q.Type, q.Text
		}
		status, answer := send(q.Method, path, ctype, body)
		if status != q.Status {
			t.Fatalf("line %d (%s): %s %s: status %d, the provider got %d: %s", n, q.Step, q.Method, path, status, q.Status, answer)
		}
		if path == "/security/ssl/truststore" { // the provider deletes the certificate by the id it reads
			certID, _ = decodeMap(t, answer)["id"].(string)
		}
		var sent any
		if json.Unmarshal(q.Body, &sent) != nil {
			continue
		}
		obj, _ := sent.(map[string]any)
		from := tfRead(q.Method, path, obj)
		if from == "" {
			continue
		}
		var read any
		if err := json.Unmarshal(e.want("GET", v1+from, "admin", "", 200), &read); err != nil {
			t.Fatalf("line %d: GET %s: %v", n, from, err)
		}
		if list, ok := read.([]any); ok && strings.HasPrefix(from, "/security/users?") && len(list) == 1 {
			read = list[0]
		}
		if strings.HasPrefix(path, "/blobstores/file") {
			delete(obj, "name")
		}
		if d := tfHeld(sent, read, q.Method+" "+path); d != "" {
			t.Errorf("line %d (%s): not read back as sent: %s", n, q.Step, d)
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	if count["create"] < 60 || count["update"] < 30 || count["back"] < 30 || count["destroy"] < 55 {
		t.Fatalf("the record is short: %v", count)
	}

	// After the destroy nothing of it is left, and the saml and ldap realms
	// went off with their settings.
	for _, list := range []string{"/repositories", "/blobstores", "/security/users", "/security/roles", "/security/privileges",
		"/security/content-selectors", "/routing-rules", "/security/ssl/truststore", "/security/ldap"} {
		if b := e.want("GET", v1+list, "admin", "", 200); strings.Contains(string(b), `"tf-`) {
			t.Errorf("%s after the destroy: %s", list, b)
		}
	}
	e.want("GET", v1+"/security/saml", "admin", "", 404)
	if b := e.want("GET", v1+"/security/realms/active", "admin", "", 200); strings.Contains(string(b), "SamlRealm") || strings.Contains(string(b), "LdapRealm") {
		t.Errorf("realms after the SAML and LDAP settings were deleted: %s", b)
	}
}

// A group holds a soft quota like any store, and a PUT replaces it: the
// provider leaves the key out when the configuration has none.
func TestBlobGroupSoftQuota(t *testing.T) {
	e := start(t, false)
	e.want("POST", v1+"/blobstores/file", "admin", `{"name":"m","path":"m"}`, 204)
	quota := func() any {
		return decodeMap(t, e.want("GET", v1+"/blobstores/group/g", "admin", "", 200))["softQuota"]
	}
	body := func(q string) string {
		return `{"name":"g","fillPolicy":"roundRobin","members":["m"]` + q + `}`
	}
	want := func(limit float64) map[string]any { return map[string]any{"type": "spaceUsedQuota", "limit": limit} }
	e.want("POST", v1+"/blobstores/group", "admin", body(`,"softQuota":{"type":"spaceUsedQuota","limit":1000}`), 204)
	if got := quota(); !reflect.DeepEqual(got, want(1000)) {
		t.Fatalf("after POST: %v", got)
	}
	e.want("PUT", v1+"/blobstores/group/g", "admin", body(`,"softQuota":{"type":"spaceUsedQuota","limit":2000}`), 204)
	if got := quota(); !reflect.DeepEqual(got, want(2000)) {
		t.Fatalf("after PUT: %v", got)
	}
	e.want("PUT", v1+"/blobstores/group/g", "admin", body(""), 204)
	if got := quota(); got != nil {
		t.Fatalf("after a PUT without one: %v", got)
	}
}

// What is left out of Nexus's API on purpose answers with the reason, in
// the statuses the provider's resources turn into an error
// (nexus_privilege_script, nexus_capability, nexus_iqserver,
// nexus_security_oidc); the certificates of the trust store are listed where
// the provider looks for the one it added.
func TestTerraformLeftOut(t *testing.T) {
	e := start(t, false)
	for _, c := range []struct {
		method, path string
		status       int
		reason       string
	}{
		{"POST", "/capabilities", 404, "no capabilities"},
		{"PUT", "/capabilities/0123", 404, "no capabilities"},
		{"PUT", "/iq", 404, "no IQ Server integration"},
		{"PUT", "/security/oauth2", 400, "OpenID Connect by discovery"},
		{"GET", "/security/oauth2", 404, "OpenID Connect by discovery"},
		{"DELETE", "/security/oauth2", 404, "OpenID Connect by discovery"},
		{"POST", "/security/privileges/script", 400, "no script API"},
	} {
		body := ""
		if c.method == "POST" || c.method == "PUT" {
			body = `{"name":"x"}`
		}
		m := decodeMap(t, e.want(c.method, v1+c.path, "admin", body, c.status))
		if msg, _ := m["message"].(string); m["id"] != "*" || !strings.Contains(msg, c.reason) {
			t.Errorf("%s %s: %v", c.method, c.path, m)
		}
	}

	e.want("GET", v1+"/security/ssl/truststore", "ci", "", 403)
	if b := e.want("GET", v1+"/security/ssl/truststore", "admin", "", 200); strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("an empty trust store: %s", b)
	}
	cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: samltest.New(t).Cert.Raw}))
	id, _ := decodeMap(t, e.want("POST", v1+"/security/ssl/truststore", "admin", cert, 201))["id"].(string)
	var list []map[string]any
	e.get(v1+"/security/ssl/truststore", "admin", &list)
	if len(list) != 1 || list[0]["id"] != id || list[0]["pem"] != cert || list[0]["fingerprint"] != id {
		t.Fatalf("the trust store lists %v, added %s", list, id)
	}
	var old []map[string]any // the path earlier builds listed it at
	e.get(v1+"/security/ssl", "admin", &old)
	if !reflect.DeepEqual(old, list) {
		t.Fatalf("/security/ssl lists %v", old)
	}
}

// docker.forceBasicAuth is the repository's anonymous-read override, as
// requireAuthentication is for cargo: true asks for credentials where
// anonymous read is on, false follows the global setting again.
func TestDockerForceBasicAuth(t *testing.T) {
	e := start(t, true)
	body := func(force bool) string {
		return `{"name":"docker-h","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW","latestPolicy":false},` +
			`"docker":{"forceBasicAuth":` + jsonText(force) + `,"v1Enabled":false,"pathEnabled":true}}`
	}
	forced := func() any {
		return decodeMap(t, e.want("GET", v1+"/repositories/docker/hosted/docker-h", "admin", "", 200))["docker"].(map[string]any)["forceBasicAuth"]
	}
	e.want("POST", v1+"/repositories/docker/hosted", "admin", body(true), 201)
	e.want("GET", v1+"/repositories/docker-h", "", "", 401)
	if forced() != true {
		t.Fatal("forceBasicAuth true reads back false")
	}
	e.want("PUT", v1+"/repositories/docker/hosted/docker-h", "admin", body(false), 204)
	e.want("GET", v1+"/repositories/docker-h", "", "", 200)
	if forced() != false {
		t.Fatal("forceBasicAuth false reads back true")
	}
	// The override set in the web UI reads as forceBasicAuth, and a body
	// without the docker section leaves it.
	if st, b := e.ui("PUT", "/-/api/repositories/docker-h", `{"format":"docker","type":"hosted","writePolicy":"allow","anonymousRead":false}`); st != 200 {
		t.Fatalf("ui update: %d %s", st, b)
	}
	if forced() != true {
		t.Fatal("anonymousRead false does not read as forceBasicAuth")
	}
	e.want("PUT", v1+"/repositories/docker/hosted/docker-h", "admin",
		`{"name":"docker-h","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW"}}`, 204)
	e.want("GET", v1+"/repositories/docker-h", "", "", 401)
}

// The values Nexus keeps and Command Center does not act on read back as
// written, through an edit in the web UI too (which has no field for them),
// and a PUT without them resets them as Nexus's would.
func TestNexusKeptValues(t *testing.T) {
	e := start(t, false)
	const nuget = `{"name":"nuget-p","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true},` +
		`"proxy":{"contentMaxAge":1440,"metadataMaxAge":1440,"remoteUrl":"https://nuget.example.com/v3/index.json"},` +
		`"negativeCache":{"enabled":true,"timeToLive":1440},"httpClient":{"autoBlock":true,"blocked":false,` +
		`"authentication":{"type":"username","username":"u","password":"p","preemptive":true}},` +
		`"nugetProxy":{"queryCacheItemMaxAge":0,"nugetVersion":"V3"}}`
	e.want("POST", v1+"/repositories/nuget/proxy", "admin", nuget, 201)
	read := func() (age, preemptive, strict any) {
		s := decodeMap(t, e.want("GET", v1+"/repositories/nuget/proxy/nuget-p", "admin", "", 200))
		auth, _ := s["httpClient"].(map[string]any)["authentication"].(map[string]any)
		return s["nugetProxy"].(map[string]any)["queryCacheItemMaxAge"], auth["preemptive"], s["storage"].(map[string]any)["strictContentTypeValidation"]
	}
	if age, pre, strict := read(); age != float64(0) || pre != true || strict != true {
		t.Fatalf("read back %v %v %v", age, pre, strict)
	}
	if st, b := e.ui("PUT", "/-/api/repositories/nuget-p", `{"format":"nuget","type":"proxy","remote":"https://nuget.example.com/v3/index.json","strictContentTypeValidation":true,"metadataMaxAge":"1h"}`); st != 200 {
		t.Fatalf("ui update: %d %s", st, b)
	}
	if age, pre, strict := read(); age != float64(0) || pre != true || strict != true {
		t.Fatalf("after an edit in the web UI: %v %v %v", age, pre, strict)
	}
	_, conf := e.ui("GET", "/-/api/repositories/nuget-p", "")
	if strings.Contains(string(conf), "queryCacheItemMaxAge") {
		t.Fatalf("the UI's view shows the kept values: %s", conf)
	}
	plain := strings.NewReplacer(`,"authentication":{"type":"username","username":"u","password":"p","preemptive":true}`, ``,
		`"queryCacheItemMaxAge":0,`, ``).Replace(nuget)
	e.want("PUT", v1+"/repositories/nuget/proxy/nuget-p", "admin", plain, 204)
	s := decodeMap(t, e.want("GET", v1+"/repositories/nuget/proxy/nuget-p", "admin", "", 200))
	if s["nugetProxy"].(map[string]any)["queryCacheItemMaxAge"] != float64(3600) || s["httpClient"].(map[string]any)["authentication"] != nil {
		t.Fatalf("after a PUT without them: %v", s)
	}
	// What cannot be kept is still refused.
	e.wantField("PUT", v1+"/repositories/nuget/proxy/nuget-p", strings.Replace(plain, `"nugetVersion":"V3"`, `"nugetVersion":"V3","queryCacheItemMaxAge":-1`, 1), "*")
}

// Nexus's SAML settings: both signature checks off (what the provider sends
// when its configuration names neither) is refused; a PUT keeps what only
// Command Center has; a body the provider reads is its own, in Nexus's
// shape.
func TestSAMLCompat(t *testing.T) {
	e := start(t, false)
	idp := samltest.New(t)
	md := jsonText(idp.Metadata())
	body := func(response, assertion bool) string {
		return `{"entityId":"","idpMetadata":` + md + `,"usernameAttribute":"username","firstNameAttribute":"","lastNameAttribute":"","emailAttribute":"",` +
			`"groupsAttribute":"groups","validateResponseSignature":` + jsonText(response) + `,"validateAssertionSignature":` + jsonText(assertion) + `}`
	}
	e.want("GET", v1+"/security/saml", "admin", "", 404)
	e.want("GET", v1+"/security/saml", "ci", "", 403)
	e.want("PUT", v1+"/security/saml", "ci", body(true, true), 403)
	if b := e.want("PUT", v1+"/security/saml", "admin", body(false, false), 400); !strings.Contains(string(b), "cannot both be off") {
		t.Fatalf("both checks off: %s", b)
	}
	e.want("PUT", v1+"/security/saml", "admin", strings.Replace(body(true, true), md, `"<not-metadata/>"`, 1), 400)
	e.want("PUT", v1+"/security/saml", "admin", body(true, false), 204)
	want := decodeMap(t, []byte(body(true, false)))
	if got := decodeMap(t, e.want("GET", v1+"/security/saml", "admin", "", 200)); !reflect.DeepEqual(got, want) {
		t.Fatalf("GET:\n%v\nwant\n%v", got, want)
	}

	// The document's end is kept as written (a file's final newline); what
	// precedes it, which the parser refuses, is dropped.
	padded := jsonText("\n  " + idp.Metadata() + "\n")
	e.want("PUT", v1+"/security/saml", "admin", strings.Replace(body(true, false), md, padded, 1), 204)
	if got := decodeMap(t, e.want("GET", v1+"/security/saml", "admin", "", 200)); got["idpMetadata"] != idp.Metadata()+"\n" {
		t.Fatalf("metadata read back as %q", got["idpMetadata"])
	}

	// The group mappings and the other settings of /-/api/realms/saml stay.
	_, cur := e.ui("GET", "/-/api/realms/saml", "")
	ui := decodeMap(t, cur)
	for _, k := range []string{"enabled", "effectiveEntityId", "acsUrl", "metadataUrl", "spCertificate", "spCertificateInfo", "idp", "idpError"} {
		delete(ui, k) // the answer's own fields
	}
	ui["groupMappings"], ui["defaultRoles"], ui["clockSkewSeconds"], ui["displayName"] = []any{map[string]any{"group": "devs", "roles": []any{"deployer"}}}, []any{"deployer"}, 30, "Company SSO"
	if st, b := e.ui("PUT", "/-/api/realms/saml", jsonText(ui)); st != 200 {
		t.Fatalf("ui update: %d %s", st, b)
	}
	e.want("PUT", v1+"/security/saml", "admin", strings.Replace(body(true, true), `"usernameAttribute":"username"`, `"usernameAttribute":"uid"`, 1), 204)
	_, cur = e.ui("GET", "/-/api/realms/saml", "")
	ui = decodeMap(t, cur)
	if ui["usernameAttribute"] != "uid" || ui["clockSkewSeconds"] != float64(30) || ui["displayName"] != "Company SSO" ||
		len(ui["groupMappings"].([]any)) != 1 || len(ui["defaultRoles"].([]any)) != 1 || ui["validateAssertionSignature"] != true {
		t.Fatalf("after a Nexus PUT: %s", cur)
	}

	// DELETE turns the realm off with the settings.
	e.want("PUT", v1+"/security/realms/active", "admin", `["NexusAuthenticatingRealm","SamlRealm"]`, 204)
	e.want("DELETE", v1+"/security/saml", "ci", "", 403)
	e.want("DELETE", v1+"/security/saml", "admin", "", 204)
	e.want("GET", v1+"/security/saml", "admin", "", 404)
	if b := e.want("GET", v1+"/security/realms/active", "admin", "", 200); strings.Contains(string(b), "SamlRealm") {
		t.Fatalf("realms after the delete: %s", b)
	}
	e.want("DELETE", v1+"/security/saml", "admin", "", 204) // nothing to delete: still 204, as the provider's destroy expects
}

// Nexus's LDAP servers over the one directory: the body the provider sends
// maps onto the settings of /-/api/realms/ldap and reads back as written, and
// the directory acts on it: who is found, and which role a group gives.
func TestLDAPCompat(t *testing.T) {
	e := start(t, false)
	const svc, base = "cn=svc,dc=example,dc=org", "ou=people,dc=example,dc=org"
	dir := ldaptest.New(t, nil)
	dir.Add(ldaptest.Entry{DN: svc, Password: "svc-secret"})
	person := func(uid, under, kind, group string) {
		dir.Add(ldaptest.Entry{DN: "uid=" + uid + "," + under + base, Password: "pw", Attrs: map[string][]string{"objectClass": {"inetOrgPerson"},
			"uid": {uid}, "cn": {"Someone"}, "employeeType": {kind}, "memberOf": {"cn=" + group + ",ou=groups,dc=example,dc=org"}}})
	}
	person("carol", "", "engineer", "devs")
	person("dave", "", "sales", "others")
	person("erin", "ou=contractors,", "engineer", "devs")
	person("mallory", "", "engineer", "admin")
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(dir.URL("ldap"), "ldap://"))
	e.want("POST", v1+"/security/roles", "admin", `{"id":"devs","name":"devs","privileges":["nx-users-read"]}`, 200)

	body := `{"id":"","name":"corp","protocol":"LDAP","host":"` + host + `","port":` + port + `,"searchBase":"dc=example,dc=org",
	  "authScheme":"SIMPLE","authUsername":"` + svc + `","authPassword":"svc-secret","connectionTimeoutSeconds":300,"connectionRetryDelaySeconds":7,"maxIncidentsCount":9,
	  "userBaseDn":"ou=people","userObjectClass":"inetOrgPerson","userLdapFilter":"employeeType=engineer","userIdAttribute":"uid",
	  "userRealNameAttribute":"cn","userEmailAddressAttribute":"mail","ldapGroupsAsRoles":true,"groupType":"dynamic","userMemberOfAttribute":"memberOf",
	  "useTrustStore":true,"authRealm":"EXAMPLE"}`
	get := func() map[string]any { return decodeMap(t, e.want("GET", v1+"/security/ldap/corp", "admin", "", 200)) }
	ui := func() map[string]any {
		_, b := e.ui("GET", "/-/api/realms/ldap", "")
		return decodeMap(t, b)
	}

	if b := e.want("GET", v1+"/security/ldap", "admin", "", 200); strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("no directory: %s", b)
	}
	e.want("GET", v1+"/security/ldap", "ci", "", 403)
	e.want("GET", v1+"/security/ldap", "", "", 401)
	e.want("GET", v1+"/security/ldap/corp", "admin", "", 404)
	e.want("POST", v1+"/security/ldap", "ci", body, 403)
	e.want("POST", v1+"/security/ldap", "admin", body, 201)
	if b := e.want("POST", v1+"/security/ldap", "admin", strings.Replace(body, `"corp"`, `"second"`, 1), 400); !strings.Contains(string(b), "one LDAP directory") {
		t.Fatalf("a second server: %s", b)
	}

	// It reads back as written, without the password.
	got, written := get(), decodeMap(t, []byte(body))
	delete(written, "authPassword")
	delete(written, "id")
	if d := subset(written, got, "server"); d != "" {
		t.Errorf("read back: %s", d)
	}
	if _, has := got["authPassword"]; has || strings.Contains(jsonText(got), "svc-secret") {
		t.Errorf("the password is shown: %v", got)
	}
	var list []map[string]any
	e.get(v1+"/security/ldap", "admin", &list)
	if len(list) != 1 || !reflect.DeepEqual(list[0], got) {
		t.Errorf("the list: %v", list)
	}

	// What the directory holds; a timeout beyond the longest is the longest.
	c := ui()
	for k, want := range map[string]any{"url": "ldap://" + host + ":" + port, "bindDN": svc, "bindPasswordSet": true, "timeoutSeconds": float64(60),
		"userBaseDN": base, "userFilter": "(&(objectClass=inetOrgPerson)(uid={user})(employeeType=engineer))", "userOneLevel": true, "groupsAsRoles": true} {
		if c[k] != want {
			t.Errorf("the directory's %s = %v, want %v", k, c[k], want)
		}
	}

	// It acts: the filter, the direct children, the role of a group's name,
	// and never the administrators' role.
	e.want("PUT", v1+"/security/realms/active", "admin", `["NexusAuthenticatingRealm","LdapRealm"]`, 204)
	e.want("GET", v1+"/security/users", "carol", "", 200)
	e.want("GET", v1+"/security/users", "dave", "", 401)
	e.want("GET", v1+"/security/users", "erin", "", 401)
	e.want("GET", v1+"/security/users", "mallory", "", 403)

	// Without the filter, through the subtree; an empty password keeps the
	// stored one, and the bind still works.
	wide := strings.NewReplacer(`"userLdapFilter":"employeeType=engineer",`, ``, `"authPassword":"svc-secret",`, `"authPassword":"",`, `"useTrustStore":true`, `"useTrustStore":true,"userSubtree":true`).Replace(body)
	e.want("PUT", v1+"/security/ldap/corp", "ci", wide, 403)
	e.want("PUT", v1+"/security/ldap/other", "admin", wide, 404)
	e.want("PUT", v1+"/security/ldap/corp", "admin", wide, 204)
	e.want("GET", v1+"/security/users", "erin", "", 200)
	e.want("GET", v1+"/security/users", "dave", "", 403)
	if got = get(); got["userSubtree"] != true || got["userLdapFilter"] != "" {
		t.Errorf("after the update: %v", got)
	}
	// Another server does not get the stored password.
	if b := e.want("PUT", v1+"/security/ldap/corp", "admin", strings.Replace(wide, `"host":"`+host+`"`, `"host":"ldap.example.org"`, 1), 400); !strings.Contains(string(b), "re-enter the bind password") {
		t.Errorf("another server: %s", b)
	}

	// What cannot be honoured is refused, naming the field.
	for _, x := range []struct{ from, to, id string }{
		{`"authScheme":"SIMPLE"`, `"authScheme":"DIGEST_MD5"`, "authScheme"},
		{`"protocol":"LDAP"`, `"protocol":"HTTP"`, "protocol"},
		{`"userRealNameAttribute":"cn"`, `"userPasswordAttribute":"userPassword"`, "userPasswordAttribute"},
		{`"groupType":"dynamic"`, `"groupType":"static"`, "groupType"},
	} {
		e.wantField("PUT", v1+"/security/ldap/corp", strings.Replace(wide, x.from, x.to, 1), x.id)
	}
	// An incomplete set of static groups is fine while it gives no roles, as
	// in Nexus: it reads back and does nothing.
	partial := strings.NewReplacer(`"groupType":"dynamic"`, `"groupType":"static","groupBaseDn":"ou=groups","groupObjectClass":"groupOfNames"`, `"ldapGroupsAsRoles":true`, `"ldapGroupsAsRoles":false`).Replace(wide)
	e.want("PUT", v1+"/security/ldap/corp", "admin", partial, 204)
	if got = get(); got["groupObjectClass"] != "groupOfNames" || got["groupType"] != "static" || got["ldapGroupsAsRoles"] != false {
		t.Errorf("static groups: %v", got)
	}
	if c = ui(); c["groupType"] != "dynamic" {
		t.Errorf("an incomplete set acts: %v", c)
	}
	e.want("GET", v1+"/security/users", "carol", "", 403)
	e.want("PUT", v1+"/security/ldap/corp", "admin", wide, 204)

	// Static groups, with the roles they give: the provider's body reads back as
	// written and means the same in the directory's settings. The group entries
	// say who is in, not the attribute (carol has the group there, erin too).
	groupEntry := func(name, class, attr string, members ...string) {
		dir.Add(ldaptest.Entry{DN: "cn=" + name + ",ou=groups,dc=example,dc=org", Attrs: map[string][]string{"objectClass": {class}, "cn": {name}, attr: members}})
	}
	groupEntry("devs", "groupOfNames", "member", "uid=dave,"+base)
	groupEntry("admin", "groupOfNames", "member", "uid=mallory,"+base, "uid=dave,"+base)
	const groups = `"groupType":"static","groupBaseDn":"ou=groups","groupSubtree":true,"groupObjectClass":"groupOfNames","groupIdAttribute":"cn","groupMemberAttribute":"member","groupMemberFormat":"${dn}"`
	staticBody := strings.Replace(wide, `"groupType":"dynamic"`, groups, 1)
	e.want("PUT", v1+"/security/ldap/corp", "admin", staticBody, 204)
	got, written = get(), decodeMap(t, []byte(staticBody))
	delete(written, "authPassword")
	delete(written, "id")
	if d := subset(written, got, "static server"); d != "" {
		t.Errorf("read back: %s", d)
	}
	c = ui()
	for k, want := range map[string]any{"groupType": "static", "groupBaseDN": "ou=groups,dc=example,dc=org", "groupSubtree": true, "groupObjectClass": "groupOfNames",
		"groupIdAttr": "cn", "groupMemberAttr": "member", "groupMemberFormat": "${dn}", "groupsAsRoles": true} {
		if c[k] != want {
			t.Errorf("the directory's %s = %v, want %v", k, c[k], want)
		}
	}
	e.want("GET", v1+"/security/users", "dave", "", 200)
	e.want("GET", v1+"/security/users", "carol", "", 403)
	e.want("GET", v1+"/security/users", "mallory", "", 403) // the administrators' role takes a mapping, also for a static group
	// No relative group DN: the groups are under the search base, in its subtree here.
	e.want("PUT", v1+"/security/ldap/corp", "admin", strings.Replace(staticBody, `"groupBaseDn":"ou=groups",`, ``, 1), 204)
	if c = ui(); c["groupBaseDN"] != "dc=example,dc=org" {
		t.Errorf("the search base as the group base: %v", c)
	}
	e.want("GET", v1+"/security/users", "dave", "", 200)
	// With posixGroup and a plain name, as Nexus's template for it has it.
	groupEntry("devs", "posixGroup", "memberUid", "erin")
	posix := strings.NewReplacer(`"groupObjectClass":"groupOfNames"`, `"groupObjectClass":"posixGroup"`, `"groupMemberAttribute":"member"`, `"groupMemberAttribute":"memberUid"`, `"${dn}"`, `"${username}"`).Replace(staticBody)
	e.want("PUT", v1+"/security/ldap/corp", "admin", posix, 204)
	e.want("GET", v1+"/security/users", "erin", "", 200)
	e.want("GET", v1+"/security/users", "dave", "", 403)
	if got = get(); got["groupMemberFormat"] != "${username}" {
		t.Errorf("posix: %v", got)
	}
	// The settings a static directory reads in Nexus's shape, set up in the web
	// UI, say the same when sent back: the two forms do not disagree, with or
	// without groups as roles.
	for _, asRoles := range []bool{true, false} {
		form := ui()
		delete(form, "bindPasswordSet")
		form["groupBaseDN"], form["groupsAsRoles"] = "ou=teams,o=elsewhere", asRoles
		if st, b := e.ui("PUT", "/-/api/realms/ldap", jsonText(form)); st != 200 {
			t.Fatalf("ui update: %d %s", st, b)
		}
		before := ui()
		got = get()
		if got["groupType"] != "static" || got["ldapGroupsAsRoles"] != asRoles || got["searchBase"] != "" || got["userBaseDn"] != base || got["groupBaseDn"] != "ou=teams,o=elsewhere" ||
			got["groupMemberAttribute"] != "memberUid" || got["groupMemberFormat"] != "${username}" || got["groupObjectClass"] != "posixGroup" || got["groupIdAttribute"] != "cn" || got["groupSubtree"] != true {
			t.Errorf("derived from a static directory: %v", got)
		}
		e.want("PUT", v1+"/security/ldap/corp", "admin", jsonText(got), 204)
		if after := ui(); !reflect.DeepEqual(after, before) {
			t.Errorf("the derived answer sent back changed the directory:\n%v\n%v", before, after)
		}
		if again := get(); !reflect.DeepEqual(again, got) {
			t.Errorf("read back after it:\n%v\n%v", got, again)
		}
	}
	// A complete set acts with ldapGroupsAsRoles off as well: the mappings read it.
	if st, b := e.ui("PUT", "/-/api/realms/ldap", jsonText(func() map[string]any {
		f := ui()
		delete(f, "bindPasswordSet")
		f["groupBaseDN"], f["groupMappings"] = "ou=groups,dc=example,dc=org", []map[string]any{{"group": "cn=devs,ou=groups,dc=example,dc=org", "roles": []string{"devs"}}}
		return f
	}())); st != 200 {
		t.Fatalf("a mapping: %d %s", st, b)
	}
	off := strings.Replace(posix, `"ldapGroupsAsRoles":true`, `"ldapGroupsAsRoles":false`, 1)
	e.want("PUT", v1+"/security/ldap/corp", "admin", off, 204)
	e.want("GET", v1+"/security/users", "erin", "", 200)
	e.want("GET", v1+"/security/users", "dave", "", 403)
	e.wantField("PUT", v1+"/security/ldap/corp", strings.Replace(posix, `"groupType":"static"`, `"groupType":"other"`, 1), "groupType")
	// A format that names nobody would put everyone in the group.
	e.wantField("PUT", v1+"/security/ldap/corp", strings.Replace(posix, `"${username}"`, `"erin"`, 1), "groupType")
	// An attribute that would break the search is refused by the directory's settings.
	e.want("PUT", v1+"/security/ldap/corp", "admin", strings.Replace(posix, `"groupIdAttribute":"cn"`, `"groupIdAttribute":"c n"`, 1), 400)
	// Back to dynamic, the groups of the attribute again; the mapping stays.
	e.want("PUT", v1+"/security/ldap/corp", "admin", wide, 204)
	e.want("GET", v1+"/security/users", "carol", "", 200)
	if c = ui(); c["groupType"] != "dynamic" {
		t.Errorf("back to dynamic: %v", c)
	}
	if st, b := e.ui("PUT", "/-/api/realms/ldap", jsonText(func() map[string]any {
		f := ui()
		delete(f, "bindPasswordSet")
		f["groupMappings"] = []map[string]any{}
		return f
	}())); st != 200 {
		t.Fatalf("no mapping: %d %s", st, b)
	}

	// The web UI's form, which has no fields for the two switches, keeps
	// them; the change then reads through the API, under the same name, and
	// that answer can be sent back.
	form := ui()
	for _, k := range []string{"bindPasswordSet", "userOneLevel", "groupsAsRoles"} {
		delete(form, k)
	}
	form["userBaseDN"] = "dc=example,dc=org"
	form["userFilter"] = "(mail={user})"
	if st, b := e.ui("PUT", "/-/api/realms/ldap", jsonText(form)); st != 200 {
		t.Fatalf("ui update: %d %s", st, b)
	}
	if c = ui(); c["groupsAsRoles"] != true || c["userOneLevel"] != false {
		t.Errorf("the web UI's form changed the switches: %v", c)
	}
	got = get()
	if got["name"] != "corp" || got["searchBase"] != "dc=example,dc=org" || got["userBaseDn"] != "" || got["userLdapFilter"] != "(mail={user})" || got["authScheme"] != "SIMPLE" {
		t.Errorf("derived: %v", got)
	}
	e.want("PUT", v1+"/security/ldap/corp", "admin", jsonText(got), 204)
	if c = ui(); c["userFilter"] != "(mail={user})" || c["userBaseDN"] != "dc=example,dc=org" {
		t.Errorf("the derived answer sent back: %v", c)
	}

	// The order of the one server.
	e.want("POST", v1+"/security/ldap/change-order", "admin", `["corp"]`, 204)
	e.want("POST", v1+"/security/ldap/change-order", "admin", `["corp","other"]`, 400)

	// DELETE resets the directory and turns its realm off.
	e.want("DELETE", v1+"/security/ldap/other", "admin", "", 404)
	e.want("DELETE", v1+"/security/ldap/corp", "ci", "", 403)
	e.want("DELETE", v1+"/security/ldap/corp", "admin", "", 204)
	if b := e.want("GET", v1+"/security/ldap", "admin", "", 200); strings.TrimSpace(string(b)) != "[]" {
		t.Errorf("after the delete: %s", b)
	}
	if c = ui(); c["url"] != "" || c["bindPasswordSet"] != false || c["bindDN"] != "" || c["groupType"] != "dynamic" || c["groupBaseDN"] != "" || c["groupMemberFormat"] != "" {
		t.Errorf("after the delete: %v", c)
	}
	if b := e.want("GET", v1+"/security/realms/active", "admin", "", 200); strings.Contains(string(b), "LdapRealm") {
		t.Errorf("realms after the delete: %s", b)
	}
	e.want("POST", v1+"/security/ldap/change-order", "admin", `["corp"]`, 400)

	// A directory set up in the web UI is the server "ldap" (a provider can
	// import it), and an anonymous search reads as NONE.
	if st, b := e.ui("PUT", "/-/api/realms/ldap", `{"url":"ldaps://ldap.example.org","userBaseDN":"ou=people,dc=example,dc=org"}`); st != 200 {
		t.Fatalf("ui create: %d %s", st, b)
	}
	got = decodeMap(t, e.want("GET", v1+"/security/ldap/ldap", "admin", "", 200))
	want := map[string]any{"name": "ldap", "protocol": "ldaps", "host": "ldap.example.org", "port": float64(636), "authScheme": "NONE",
		"searchBase": "ou=people,dc=example,dc=org", "userSubtree": true, "userLdapFilter": "", "userIdAttribute": "uid", "userMemberOfAttribute": "memberOf", "groupType": "dynamic"}
	if d := subset(want, got, "derived"); d != "" {
		t.Error(d)
	}
}

// The built-in privileges a Nexus role commonly lists: the privileges domain
// is the roles domain here (privileges live in roles), and a user's own token
// is open to every signed-in user, so holding it grants nothing more. What has
// no equivalent is refused, by name.
func TestRoleBuiltinPrivileges(t *testing.T) {
	e := start(t, false)
	body := `{"id":"sec","name":"sec","privileges":["nx-privileges-read","nx-privileges-update","nx-usertoken-current"]}`
	e.want("POST", v1+"/security/roles", "admin", body, 200)
	got := decodeMap(t, e.want("GET", v1+"/security/roles/sec", "admin", "", 200))
	if d := subset(decodeMap(t, []byte(body)), got, "role"); d != "" {
		t.Error(d)
	}
	for _, name := range []string{"nx-privileges-read", "nx-privileges-all", "nx-usertoken-current"} {
		if p := decodeMap(t, e.want("GET", v1+"/security/privileges/"+name, "admin", "", 200)); p["readOnly"] != true || p["type"] != "application" {
			t.Errorf("%s: %v", name, p)
		}
	}
	if b := e.want("POST", v1+"/security/roles", "admin", `{"id":"log","name":"log","privileges":["nx-logging-read"]}`, 400); !strings.Contains(string(b), "nx-logging-read") {
		t.Errorf("a privilege without an equivalent: %s", b)
	}
}

// The provider destroys what does not depend on each other at once: two
// settings going at the same time each turn their realm off, and neither
// brings the other's back.
func TestRealmsOffTogether(t *testing.T) {
	e := start(t, false)
	idp := jsonText(samltest.New(t).Metadata())
	for i := 0; i < 10; i++ {
		e.want("PUT", v1+"/security/saml", "admin", `{"idpMetadata":`+idp+`,"usernameAttribute":"uid","validateResponseSignature":true,"validateAssertionSignature":true}`, 204)
		e.want("POST", v1+"/security/ldap", "admin", `{"name":"corp","protocol":"ldap","host":"ldap.example.org","port":389,"searchBase":"dc=example,dc=org","authScheme":"NONE","groupType":"dynamic"}`, 201)
		e.want("PUT", v1+"/security/realms/active", "admin", `["NexusAuthenticatingRealm","LdapRealm","SamlRealm"]`, 204)
		var wg sync.WaitGroup
		for _, path := range []string{"/security/saml", "/security/ldap/corp"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if st, b, _ := e.do("DELETE", v1+path, "admin", ""); st != 204 {
					t.Errorf("round %d: DELETE %s: %d %s", i, path, st, b)
				}
			}()
		}
		wg.Wait()
		if b := e.want("GET", v1+"/security/realms/active", "admin", "", 200); strings.Contains(string(b), "SamlRealm") || strings.Contains(string(b), "LdapRealm") {
			t.Fatalf("round %d: realms %s", i, b)
		}
	}
}
