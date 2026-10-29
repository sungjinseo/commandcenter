package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"commandcenter/internal/config"
	"commandcenter/internal/meta/metatest"
	"commandcenter/internal/tasks"
)

// utCall sends a request with Basic user:pass (none when user is ""),
// the UI header when ui is set, and returns the status and body.
func utCall(t *testing.T, method, url, user, pass, body string, ui bool, hdr ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	if ui {
		req.Header.Set("X-Command-Center-UI", "1")
	}
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

func utJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return m
}

// User tokens through the real server: Maven, npm and Docker accept the
// codes as Basic credentials; "required" refuses passwords there but not
// in the UI login or for API tokens; reset, admin reset, user delete and
// the Nexus API.
func TestUserTokensEndToEnd(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg, err := config.Parse([]byte(`
dataDir: ` + t.TempDir() + `
users:
  - { name: root, passwordHash: "` + string(h) + `", roles: [admin] }
  - { name: dev, passwordHash: "` + string(h) + `", roles: [deployer] }
roles:
  deployer: { write: [maven-releases, npm-hosted, docker-hosted] }
repositories:
  - { name: maven-releases, format: maven, type: hosted }
  - { name: npm-hosted, format: npm, type: hosted }
  - { name: docker-hosted, format: docker, type: hosted }
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.URL = metatest.URL(t, cfg.DataDir)
	a, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	u := ts.URL
	settings := func(body string) {
		t.Helper()
		if st, b := utCall(t, "PUT", u+"/-/api/settings", "root", "pw", `{"userTokens":`+body+`}`, true); st != 200 {
			t.Fatalf("settings %s: %d %s", body, st, b)
		}
	}
	create := func() (string, string) {
		t.Helper()
		st, b := utCall(t, "POST", u+"/-/api/me/usertoken", "dev", "pw", "", true)
		m := utJSON(t, b)
		if st != 201 || m["user"] != "dev" || len(m["nameCode"].(string)) != 8 || len(m["passCode"].(string)) != 44 {
			t.Fatalf("create: %d %s", st, b)
		}
		return m["nameCode"].(string), m["passCode"].(string)
	}
	jar := 0
	maven := func(user, pass string) int {
		t.Helper()
		jar++
		st, _ := utCall(t, "PUT", u+"/repository/maven-releases/org/x/a/1."+strconv.Itoa(jar)+"/a-1."+strconv.Itoa(jar)+".jar", user, pass, "jar", false)
		return st
	}

	if st, b := utCall(t, "POST", u+"/-/api/me/usertoken", "dev", "pw", "", true); st != 409 {
		t.Fatalf("create while disabled: %d %s", st, b)
	}
	settings(`{"enabled":true,"expirationDays":30}`)
	name, pass := create()
	if st, b := utCall(t, "GET", u+"/-/api/me/usertoken", "dev", "pw", "", false); st != 200 ||
		!strings.Contains(b, `"exists":true`) || strings.Contains(b, name) || strings.Contains(b, pass) || !strings.Contains(b, `"expired":false`) {
		t.Fatalf("me: %d %s", st, b)
	}

	// Maven, npm, Docker.
	if st := maven(name, pass); st != 201 {
		t.Fatalf("maven PUT with codes: %d", st)
	}
	if st, b := utCall(t, "GET", u+"/repository/npm-hosted/left-pad", name, pass, "", false); st != 404 {
		t.Fatalf("npm GET with codes: %d %s", st, b)
	}
	if st, _ := utCall(t, "GET", u+"/repository/npm-hosted/left-pad", name, pass[:43]+"!", "", false); st != 401 {
		t.Fatalf("npm GET with a wrong pass code: %d", st)
	}
	if st, b := utCall(t, "GET", u+"/v2/", name, pass, "", false); st != 200 {
		t.Fatalf("docker /v2/ with codes: %d %s", st, b)
	}
	st, b := utCall(t, "GET", u+"/v2/token?service=x&scope=repository:docker-hosted/app:pull,push", name, pass, "", false)
	if st != 200 {
		t.Fatalf("docker token: %d %s", st, b)
	}
	if st, b := utCall(t, "POST", u+"/v2/docker-hosted/app/blobs/uploads/", "", "", "", false, "Authorization", "Bearer "+utJSON(t, b)["token"].(string)); st != 202 {
		t.Fatalf("docker push with the codes' token: %d %s", st, b)
	}
	// A token cannot mint tokens.
	if st, _ := utCall(t, "POST", u+"/-/api/me/usertoken", name, pass, "", true); st != 403 {
		t.Fatalf("create with codes: %d", st)
	}

	// Required: passwords are refused on repository endpoints only.
	settings(`{"enabled":true,"required":true,"expirationDays":30}`)
	if st, b := utCall(t, "PUT", u+"/repository/maven-releases/org/x/a/9/a-9.jar", "dev", "pw", "jar", false); st != 401 || !strings.Contains(b, "user token") {
		t.Fatalf("maven PUT with a password: %d %s", st, b)
	}
	if st, _ := utCall(t, "GET", u+"/v2/", "root", "pw", "", false); st != 401 {
		t.Fatalf("docker login with an admin password: %d", st)
	}
	if st := maven(name, pass); st != 201 {
		t.Fatalf("maven PUT with codes while required: %d", st)
	}
	st, b = utCall(t, "POST", u+"/-/api/tokens", "dev", "pw", "", true)
	if st != 201 {
		t.Fatalf("API token: %d %s", st, b)
	}
	if st := maven("dev", utJSON(t, b)["token"].(string)); st != 201 {
		t.Fatalf("maven PUT with an API token while required: %d", st)
	}
	if st, b := utCall(t, "POST", u+"/-/api/login", "", "", `{"user":"dev","password":"pw"}`, true); st != 200 {
		t.Fatalf("UI login while required: %d %s", st, b)
	}
	npmLogin := func(user, pw string) (int, string) {
		return utCall(t, "PUT", u+"/repository/npm-hosted/-/user/org.couchdb.user:"+user, "", "", `{"name":"`+user+`","password":"`+pw+`"}`, false)
	}
	if st, b := npmLogin("dev", "pw"); st != 401 || !strings.Contains(b, "user token") {
		t.Fatalf("npm login with a password: %d %s", st, b)
	}
	if st, b := npmLogin(name, pass); st != 201 || !strings.Contains(b, `"token":"cc_`) {
		t.Fatalf("npm login with codes: %d %s", st, b)
	}

	// The admin list and the Nexus settings API.
	if st, b := utCall(t, "GET", u+"/-/api/usertokens", "root", "pw", "", false); st != 200 || !strings.Contains(b, `"user":"dev"`) || !strings.Contains(b, `"nameCode":"`+name+`"`) {
		t.Fatalf("admin list: %d %s", st, b)
	}
	if st, _ := utCall(t, "GET", u+"/-/api/usertokens", "dev", "pw", "", false); st != 403 {
		t.Fatalf("list as a user: %d", st)
	}
	nx := u + "/service/rest/v1/security/user-tokens"
	if st, b := utCall(t, "GET", nx, "root", "pw", "", false); st != 200 || b != `{"enabled":true,"protectContent":true,"expirationEnabled":true,"expirationDays":30}`+"\n" {
		t.Fatalf("nexus settings: %d %s", st, b)
	}
	if st, b := utCall(t, "PUT", nx, "root", "pw", `{"enabled":true,"protectContent":false,"expirationEnabled":false,"expirationDays":30}`, false); st != 200 {
		t.Fatalf("nexus settings PUT: %d %s", st, b)
	}
	if st, b := utCall(t, "GET", u+"/-/api/settings", "root", "pw", "", false); st != 200 || !strings.Contains(b, `"userTokens":{"enabled":true,"required":false,"expirationDays":0}`) {
		t.Fatalf("settings after nexus PUT: %d %s", st, b)
	}
	if st, b := utCall(t, "GET", nx+"/tokens?realm=NexusAuthenticatingRealm&userId=dev", "root", "pw", "", false); st != 200 || !strings.Contains(b, `"nameCode":"`+name+`"`) || !strings.Contains(b, `"realm":"NexusAuthenticatingRealm"`) {
		t.Fatalf("nexus list: %d %s", st, b)
	}
	if st, b := utCall(t, "GET", nx+"/tokens?realm=LdapRealm", "root", "pw", "", false); st != 200 || b != "[]\n" {
		t.Fatalf("nexus list ldap: %d %s", st, b)
	}
	if st, b := utCall(t, "GET", u+"/service/rest/v1/security/users/dev/NexusAuthenticatingRealm/user-token", "root", "pw", "", false); st != 200 || !strings.Contains(b, `"userId":"dev"`) || strings.Contains(b, "passCode") {
		t.Fatalf("nexus get: %d %s", st, b)
	}
	if st, _ := utCall(t, "GET", nx, "dev", "pw", "", false); st != 403 {
		t.Fatalf("nexus settings as a user: %d", st)
	}

	// A reset invalidates the old codes.
	name2, pass2 := create()
	if st := maven(name, pass); st != 401 {
		t.Fatalf("old codes after a reset: %d", st)
	}
	if st := maven(name2, pass2); st != 201 {
		t.Fatalf("new codes: %d", st)
	}
	// The admin's reset of one user, then of everyone (with confirmation).
	if st, _ := utCall(t, "DELETE", u+"/-/api/usertokens/dev", "root", "pw", "", true); st != 204 {
		t.Fatalf("admin reset: %d", st)
	}
	if st := maven(name2, pass2); st != 401 {
		t.Fatalf("codes after the admin reset: %d", st)
	}
	create()
	if st, _ := utCall(t, "DELETE", u+"/-/api/usertokens", "root", "pw", "", true); st != 400 {
		t.Fatalf("reset all without confirm: %d", st)
	}
	if st, b := utCall(t, "DELETE", u+"/-/api/usertokens?confirm=true", "root", "pw", "", true); st != 200 || !strings.Contains(b, `"deleted":1`) {
		t.Fatalf("reset all: %d %s", st, b)
	}
	// The Nexus per-user API creates one for the user; deleting the user
	// kills it.
	st, b = utCall(t, "POST", u+"/service/rest/v1/security/users/dev/NexusAuthenticatingRealm/user-token", "root", "pw", "", false)
	m := utJSON(t, b)
	if st != 200 || m["passCode"] == nil {
		t.Fatalf("nexus create: %d %s", st, b)
	}
	name3, pass3 := m["nameCode"].(string), m["passCode"].(string)
	if st := maven(name3, pass3); st != 201 {
		t.Fatalf("nexus-created codes: %d", st)
	}
	if st, b := utCall(t, "DELETE", u+"/-/api/users/dev", "root", "pw", "", true); st != 204 {
		t.Fatalf("delete user: %d %s", st, b)
	}
	if st := maven(name3, pass3); st != 401 {
		t.Fatalf("deleted user's codes: %d", st)
	}
	if st, b := utCall(t, "GET", u+"/-/api/usertokens", "root", "pw", "", false); st != 200 || !strings.Contains(b, `"userTokens":[]`) {
		t.Fatalf("list after the user delete: %d %s", st, b)
	}

	// The purge task deletes the tokens older than expirationDays.
	ctx := context.Background()
	if err := a.DB.PutUserToken(ctx, "root", "oldoldol", "h", time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.PutUserToken(ctx, "ldap:x", "newnewne", "h", time.Now()); err != nil {
		t.Fatal(err)
	}
	st, b = utCall(t, "POST", u+"/-/api/tasks", "root", "pw", `{"name":"purge","type":"`+tasks.TypePurgeUserTokens+`","schedule":"manual","params":{}}`, true)
	if st != 201 {
		t.Fatalf("create task: %d %s", st, b)
	}
	task := utJSON(t, b)["id"].(string)
	if out, err := a.Tasks.RunNow(task, "root"); err != nil || out.Result != tasks.ResultOK || out.Removed != 0 { // expirationDays 0: never
		t.Fatalf("purge without expiry: %+v %v", out, err)
	}
	settings(`{"enabled":true,"expirationDays":1}`)
	if st, b := utCall(t, "GET", u+"/-/api/usertokens", "root", "pw", "", false); !strings.Contains(b, `"expired":true`) || !strings.Contains(b, `"expired":false`) {
		t.Fatalf("expired flags: %d %s", st, b)
	}
	out, err := a.Tasks.RunNow(task, "root")
	if err != nil || out.Result != tasks.ResultOK || out.Removed != 1 {
		t.Fatalf("purge: %+v %v", out, err)
	}
	if _, err := a.DB.GetUserToken(ctx, "root"); err == nil {
		t.Fatal("expired token kept")
	}
	if _, err := a.DB.GetUserToken(ctx, "ldap:x"); err != nil {
		t.Fatalf("live token purged: %v", err)
	}

	// Every change left an event.
	var list string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		_, list = utCall(t, "GET", u+"/-/api/events?limit=200", "root", "pw", "", false)
		if strings.Contains(list, `"kind":"task.create"`) {
			break
		}
	}
	for _, want := range []string{"settings.userTokens", "usertoken.create", "usertoken.reset"} {
		if !strings.Contains(list, `"kind":"`+want+`"`) {
			t.Fatalf("no %s event: %s", want, list)
		}
	}
	if strings.Contains(list, pass) || strings.Contains(list, pass3) {
		t.Fatal("an event holds a pass code")
	}
}
