package nexusapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// as sends method path as user/pass, JSON body, without the UI header.
func (e *env) as(method, path, user, pass, body string) (int, []byte) {
	return send(e, method, path, body, user, pass, false)
}

func (e *env) asWant(method, path, user, pass, body string, status int) []byte {
	e.t.Helper()
	st, b := e.as(method, path, user, pass, body)
	if st != status {
		e.t.Fatalf("%s %s as %q: got %d want %d: %s", method, path, user, st, status, b)
	}
	return b
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return v
}

type nxRoleT struct {
	ID, Source, Name, Description string
	ReadOnly                      bool
	Privileges, Roles             []string
}

type nxUserT struct {
	UserID, FirstName, LastName, EmailAddress, Source, Status string
	ReadOnly                                                  bool
	Roles, ExternalRoles                                      []string
}

// What datadrivers/terraform-provider-nexus does with nexus_privilege_*,
// nexus_security_role (with a nested role) and nexus_security_user: create,
// read back what it wrote, update, refuse bad input as Nexus does, delete
// (security-01, -02, -03, -09, -10).
func TestTerraformLikeSecuritySequence(t *testing.T) {
	e := start(t, false)
	// A Nexus CI privilege: read and edit (Nexus: every PUT). It reads back
	// as written, although edit means add and edit here.
	e.want("POST", v1+"/security/privileges/repository-view", "admin",
		`{"name":"mr-deploy","description":"deploy","actions":["READ","EDIT"],"format":"maven2","repository":"maven-releases"}`, 201)
	p := decode[map[string]any](t, e.want("GET", v1+"/security/privileges/mr-deploy", "admin", "", 200))
	if p["type"] != "repository-view" || fmt.Sprint(p["actions"]) != "[READ EDIT]" || p["repository"] != "maven-releases" ||
		p["format"] != "maven2" || p["description"] != "deploy" || p["readOnly"] != false {
		t.Fatalf("privilege = %v", p)
	}
	// A base role of built-in privilege names, and a role containing it.
	r := decode[nxRoleT](t, e.want("POST", v1+"/security/roles", "admin",
		`{"id":"base","name":"Base","description":"read","privileges":["nx-repository-view-maven2-maven-releases-read"],"roles":[]}`, 200))
	if r.ID != "base" || r.Name != "Base" || r.Source != "default" || strings.Join(r.Privileges, ",") != "nx-repository-view-maven2-maven-releases-read" {
		t.Fatalf("created role = %+v", r)
	}
	e.want("POST", v1+"/security/roles", "admin", `{"id":"base","name":"again","privileges":[]}`, 400) // taken: 400 as Nexus
	e.want("POST", v1+"/security/roles", "admin", `{"id":"bad","name":"x","privileges":["no-such-priv"]}`, 400)
	e.want("POST", v1+"/security/roles", "admin", `{"id":"bad","name":"x","roles":["no-such-role"]}`, 400)
	e.want("POST", v1+"/security/roles", "admin",
		`{"id":"deploy","name":"Deployer","description":"CI","privileges":["mr-deploy","nx-users-read"],"roles":["base"]}`, 200)
	var got nxRoleT
	e.get(v1+"/security/roles/deploy", "admin", &got)
	if got.Name != "Deployer" || got.Description != "CI" || strings.Join(got.Privileges, ",") != "mr-deploy,nx-users-read" || strings.Join(got.Roles, ",") != "base" || got.ReadOnly {
		t.Fatalf("role read back = %+v", got)
	}
	// The user, with first and last names that read back exactly.
	u := decode[nxUserT](t, e.want("POST", v1+"/security/users", "admin",
		`{"userId":"bob","firstName":"Mary Ann","lastName":"Van Damme","emailAddress":"bob@example.com","password":"longenough1","status":"active","roles":["deploy"]}`, 200))
	if u.UserID != "bob" || u.FirstName != "Mary Ann" || u.LastName != "Van Damme" || u.Source != "default" || u.ReadOnly || strings.Join(u.Roles, ",") != "deploy" {
		t.Fatalf("created user = %+v", u)
	}
	var users []nxUserT
	e.get(v1+"/security/users?userId=BO", "admin", &users) // a case-insensitive prefix
	if len(users) != 1 || users[0].FirstName != "Mary Ann" || users[0].LastName != "Van Damme" || users[0].EmailAddress != "bob@example.com" {
		t.Fatalf("users = %+v", users)
	}

	// The grants work: with authenticated read off, bob deploys a new
	// artifact (edit translated to add and edit) and reads through base;
	// nx-users-read lets him list users but not create one.
	if st, b := uiCall(e, "PUT", "/-/api/settings", `{"authenticatedRead":false}`); st != 200 {
		t.Fatalf("settings: %d %s", st, b)
	}
	e.asWant("PUT", "/repository/maven-releases/org/x/lib/1.0/lib-1.0.jar", "bob", "longenough1", "jar", 201)
	e.asWant("GET", "/repository/maven-releases/org/x/lib/1.0/lib-1.0.jar", "bob", "longenough1", "", 200)
	e.asWant("PUT", "/repository/raw-files/x.txt", "bob", "longenough1", "x", 403)
	e.asWant("GET", v1+"/security/users", "bob", "longenough1", "", 200)
	e.asWant("POST", v1+"/security/users", "bob", "longenough1", `{"userId":"eve","firstName":"E","lastName":"V","password":"longenough1","status":"active"}`, 403)
	e.asWant("GET", v1+"/security/roles", "bob", "longenough1", "", 403)

	// Update, and Nexus's refusals.
	e.want("PUT", v1+"/security/roles/deploy", "admin", `{"id":"deploy","name":"Deployer 2","description":"CI","privileges":["mr-deploy"],"roles":["base"]}`, 204)
	e.get(v1+"/security/roles/deploy", "admin", &got)
	if got.Name != "Deployer 2" || strings.Join(got.Privileges, ",") != "mr-deploy" {
		t.Fatalf("updated role = %+v", got)
	}
	e.asWant("GET", v1+"/security/users", "bob", "longenough1", "", 403) // nx-users-read is gone
	e.want("PUT", v1+"/security/roles/deploy", "admin", `{"id":"other","name":"x"}`, 409)
	e.want("PUT", v1+"/security/roles/base", "admin", `{"id":"base","name":"Base","roles":["deploy"]}`, 400) // a cycle
	e.want("PUT", v1+"/security/roles/nx-admin", "admin", `{"id":"nx-admin","name":"x"}`, 400)
	e.want("DELETE", v1+"/security/roles/nx-admin", "admin", "", 400)
	e.want("PUT", v1+"/security/users/bob", "admin", `{"userId":"bob","firstName":"Bob","lastName":"Builder","emailAddress":"b@example.com","status":"active","roles":["deploy"]}`, 204)
	e.get(v1+"/security/users?userId=bob", "admin", &users)
	if users[0].FirstName != "Bob" || users[0].LastName != "Builder" {
		t.Fatalf("updated user = %+v", users[0])
	}
	e.want("PUT", v1+"/security/users/bob", "admin", `{"userId":"robert","status":"active"}`, 400)
	e.want("PUT", v1+"/security/users/nobody", "admin", `{"userId":"nobody","status":"active"}`, 404)
	e.want("DELETE", v1+"/security/roles/base", "admin", "", 409)           // contained in deploy
	e.want("DELETE", v1+"/security/privileges/mr-deploy", "admin", "", 409) // contained in deploy

	// The built-in privileges are listed read-only and cannot be deleted.
	var privs []map[string]any
	e.get(v1+"/security/privileges", "admin", &privs)
	i := slices.IndexFunc(privs, func(p map[string]any) bool { return p["name"] == "nx-repository-view-maven2-maven-releases-read" })
	if i < 0 || privs[i]["readOnly"] != true || privs[i]["type"] != "repository-view" ||
		!slices.ContainsFunc(privs, func(p map[string]any) bool { return p["name"] == "mr-deploy" && p["readOnly"] == false }) ||
		!slices.ContainsFunc(privs, func(p map[string]any) bool { return p["name"] == "nx-all" && p["pattern"] == "nexus:*" }) {
		t.Fatalf("privileges: %d, %v", len(privs), i)
	}
	e.want("DELETE", v1+"/security/privileges/nx-all", "admin", "", 400)

	// Destroy, dependents first.
	e.want("DELETE", v1+"/security/users/bob", "admin", "", 204)
	e.want("DELETE", v1+"/security/roles/deploy", "admin", "", 204)
	e.want("DELETE", v1+"/security/roles/base", "admin", "", 204)
	e.want("DELETE", v1+"/security/privileges/mr-deploy", "admin", "", 204)
	e.want("GET", v1+"/security/privileges/mr-deploy", "admin", "", 404)
	e.want("DELETE", v1+"/security/privileges/mr-deploy", "admin", "", 404)
	e.want("GET", v1+"/security/roles/deploy", "admin", "", 404)
}

// Every privilege type Command Center can express, and clear 400s for the
// rest (security-03, -07, -08, -14).
func TestPrivilegeTypes(t *testing.T) {
	e := start(t, false)
	uiRole := func(id string) string {
		t.Helper()
		st, b := uiGet(e, "/-/api/roles/"+id)
		if st != 200 {
			t.Fatalf("role %s: %d %s", id, st, b)
		}
		return string(b)
	}
	// application: tasks read and run (start and stop are run here).
	e.want("POST", v1+"/security/privileges/application", "admin", `{"name":"ops","domain":"tasks","actions":["READ","START","STOP"]}`, 201)
	if b := uiRole("priv:ops"); !strings.Contains(b, `"target":"app:tasks","actions":["read","run"]`) {
		t.Fatalf("priv:ops = %s", b)
	}
	p := decode[map[string]any](t, e.want("GET", v1+"/security/privileges/ops", "admin", "", 200))
	if p["type"] != "application" || p["domain"] != "tasks" || fmt.Sprint(p["actions"]) != "[READ START STOP]" {
		t.Fatalf("ops = %v", p)
	}
	for _, body := range []string{
		`{"name":"x","domain":"tasks","actions":["ADD"]}`,         // task create stays with admins
		`{"name":"x","domain":"capabilities","actions":["READ"]}`, // no such domain here
		`{"name":"x","domain":"users","actions":["ASSOCIATE"]}`,   // no equivalent
		`{"name":"x","domain":"settings","actions":["UPDATE"]}`,   // settings changes stay with admins
	} {
		e.want("POST", v1+"/security/privileges/application", "admin", body, 400)
	}
	// wildcard: the shapes that map cleanly.
	e.want("POST", v1+"/security/privileges/wildcard", "admin", `{"name":"all-read","pattern":"nexus:repository-view:*:*:read"}`, 201)
	if b := uiRole("priv:all-read"); !strings.Contains(b, `"target":"*","actions":["read"]`) {
		t.Fatalf("priv:all-read = %s", b)
	}
	e.want("POST", v1+"/security/privileges/wildcard", "admin", `{"name":"two","pattern":"nexus:repository-view:maven2:maven-releases,maven-central:browse,read"}`, 201)
	if b := uiRole("priv:two"); !strings.Contains(b, `"target":"repo:maven-central","actions":["browse","read"]`) || !strings.Contains(b, `"target":"repo:maven-releases"`) {
		t.Fatalf("priv:two = %s", b)
	}
	e.want("POST", v1+"/security/privileges/wildcard", "admin", `{"name":"root","pattern":"nexus:*"}`, 201)
	if b := uiRole("priv:root"); !strings.Contains(b, `"roles":["admin"]`) {
		t.Fatalf("priv:root = %s", b)
	}
	e.want("POST", v1+"/security/privileges/wildcard", "admin", `{"name":"w","pattern":"nexus:users:read"}`, 201)
	for _, pat := range []string{"nexus:foo:bar:baz", "other:*", "nexus:repository-view:maven2:nope:read"} {
		e.want("POST", v1+"/security/privileges/wildcard", "admin", `{"name":"x","pattern":"`+pat+`"}`, 400)
	}
	// repository-admin: a repository's settings; add only on a format.
	e.want("POST", v1+"/security/privileges/repository-admin", "admin", `{"name":"mr-admin","format":"maven2","repository":"maven-releases","actions":["READ","EDIT"]}`, 201)
	if b := uiRole("priv:mr-admin"); !strings.Contains(b, `"target":"admin:repo:maven-releases","actions":["read","edit"]`) {
		t.Fatalf("priv:mr-admin = %s", b)
	}
	e.want("POST", v1+"/security/privileges/repository-admin", "admin", `{"name":"x","format":"maven2","repository":"maven-releases","actions":["ADD"]}`, 400)
	e.want("POST", v1+"/security/privileges/repository-admin", "admin", `{"name":"mvn-create","format":"maven2","repository":"*","actions":["ADD"]}`, 201)
	e.want("POST", v1+"/security/privileges/repository-admin", "admin", `{"name":"mr-all","format":"maven2","repository":"maven-releases","actions":["ALL"]}`, 201)
	if b := uiRole("priv:mr-all"); !strings.Contains(b, `"target":"admin:repo:maven-releases","actions":["browse","read","edit","delete"]`) {
		t.Fatalf("priv:mr-all = %s", b) // ALL on one repository: everything but add
	}
	e.want("GET", v1+"/security/privileges/nx-repository-admin-maven2-maven-releases-*", "admin", "", 200)
	e.want("GET", v1+"/security/privileges/nx-repository-admin-maven2-maven-releases-add", "admin", "", 404)
	e.want("GET", v1+"/security/privileges/nx-repository-admin-maven2-*-add", "admin", "", 200)
	e.want("POST", v1+"/security/privileges/script", "admin", `{"name":"x","scriptName":"s","actions":["RUN"]}`, 400)
	e.want("POST", v1+"/security/privileges/repository-view", "admin", `{"name":"nx-repository-view-*-*-read","actions":["READ"]}`, 400) // a built-in name
	// repository-content-selector: ALL is every action; a PUT naming
	// another privilege is 409; the format must fit the repository.
	if st, b := uiCall(e, "POST", "/-/api/content-selectors", `{"name":"acme","expression":"format == \"maven2\" && path =^ \"/org/acme/\""}`); st != 201 {
		t.Fatalf("selector with &&: %d %s", st, b)
	}
	e.want("POST", v1+"/security/privileges/repository-content-selector", "admin", `{"name":"acme-all","actions":["ALL"],"format":"maven2","repository":"maven-releases","contentSelector":"acme"}`, 201)
	if b := uiRole("priv:acme-all"); !strings.Contains(b, `"actions":["browse","read","add","edit","delete"]`) {
		t.Fatalf("priv:acme-all = %s", b)
	}
	e.want("PUT", v1+"/security/privileges/repository-content-selector/acme-all", "admin", `{"name":"other","actions":["READ"],"repository":"*","contentSelector":"acme"}`, 409)
	e.want("POST", v1+"/security/privileges/repository-content-selector", "admin", `{"name":"x","actions":["READ"],"format":"npm","repository":"maven-releases","contentSelector":"acme"}`, 400)
	e.want("POST", v1+"/security/privileges/repository-content-selector", "admin", `{"name":"every-maven","actions":["READ"],"format":"maven2","repository":"*-maven2","contentSelector":"acme"}`, 201)
	// PUT changes the type too; the read-back follows.
	e.want("PUT", v1+"/security/privileges/application/w", "admin", `{"name":"w","domain":"roles","actions":["READ"]}`, 204)
	p = decode[map[string]any](t, e.want("GET", v1+"/security/privileges/w", "admin", "", 200))
	if p["type"] != "application" || p["domain"] != "roles" {
		t.Fatalf("w = %v", p)
	}
	e.want("PUT", v1+"/security/privileges/application/nope", "admin", `{"name":"nope","domain":"roles","actions":["READ"]}`, 404)
	e.want("GET", v1+"/security/privileges/nx-repository-view-maven2-maven-releases-edit", "admin", "", 200)
	e.want("GET", v1+"/security/privileges/nx-repository-view-npm-maven-releases-edit", "admin", "", 404) // wrong format
	// A role made in the UI reads in Nexus terms.
	if st, b := uiPost(e, "/-/api/roles", `{"id":"ui-made","privileges":[{"target":"format:maven","actions":["browse","read","add","edit","delete"]},{"target":"app:selectors","actions":["read"]}],"roles":["priv:ops"]}`); st != 201 {
		t.Fatalf("ui role: %d %s", st, b)
	}
	var r nxRoleT
	e.get(v1+"/security/roles/ui-made", "admin", &r)
	if strings.Join(r.Privileges, ",") != "nx-repository-view-maven2-*-*,nx-selectors-read,ops" || len(r.Roles) != 0 {
		t.Fatalf("ui-made = %+v", r)
	}
}

// Delegated administration through application and repository-admin
// privileges, enforced in the UI's API and the Nexus API alike; the admin
// role stays with admins, and a token never administers (security-06, -07).
func TestDelegatedAdministration(t *testing.T) {
	e := start(t, false)
	mk := func(path, body string) {
		t.Helper()
		if st, b := uiPost(e, path, body); st != 201 {
			t.Fatalf("POST %s: %d %s", path, st, b)
		}
	}
	mk("/-/api/roles", `{"id":"user-admin","privileges":[{"target":"app:users","actions":["read","add","edit","delete"]}]}`)
	mk("/-/api/roles", `{"id":"maven-admin","privileges":[{"target":"admin:format:maven","actions":["browse","read","add","edit","delete"]}]}`)
	mk("/-/api/roles", `{"id":"task-op","privileges":[{"target":"app:tasks","actions":["read","run"]}]}`)
	mk("/-/api/users", `{"id":"ua","password":"longenough1","roles":["user-admin"]}`)
	mk("/-/api/users", `{"id":"ma","password":"longenough1","roles":["maven-admin"]}`)
	mk("/-/api/users", `{"id":"op","password":"longenough1","roles":["task-op"]}`)
	ui := func(method, path, user, body string) int {
		t.Helper()
		st, _ := send(e, method, path, body, user, "longenough1", true)
		return st
	}
	// The user administrator: users, but never the admin role or an admin.
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/-/api/users", "", 200},
		{"POST", "/-/api/users", `{"id":"new1","password":"longenough1","roles":["task-op"]}`, 201},
		{"PUT", "/-/api/users/new1", `{"roles":[]}`, 200},
		{"POST", "/-/api/users", `{"id":"new2","password":"longenough1","roles":["admin"]}`, 403},
		{"PUT", "/-/api/users/new1", `{"roles":["admin"]}`, 403},
		{"PUT", "/-/api/users/admin", `{"roles":["admin"]}`, 403},
		{"PUT", "/-/api/users/admin/password", `{"password":"longenough2"}`, 403},
		{"DELETE", "/-/api/users/admin", "", 403},
		{"POST", "/-/api/users", `{"id":"dirx","source":"ldap","roles":[]}`, 403}, // directory records stay with admins
		{"DELETE", "/-/api/users/new1", "", 204},
		{"GET", "/-/api/roles", "", 403},
		{"GET", "/-/api/settings", "", 403},
	} {
		if got := ui(c.method, c.path, "ua", c.body); got != c.want {
			t.Errorf("ua %s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
	// A token of the user administrator administers nothing.
	st, b := send(e, "POST", "/-/api/tokens", `{}`, "ua", "longenough1", true)
	if st != 201 {
		t.Fatalf("token: %d %s", st, b)
	}
	tok := decode[map[string]string](t, b)["token"]
	req, _ := http.NewRequest("POST", e.ts.URL+v1+"/security/users", strings.NewReader(`{"userId":"t1","firstName":"a","lastName":"b","password":"longenough1","status":"active"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.ts.Client().Do(req)
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("token user create: %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// The maven administrator: maven repositories only, both APIs.
	if got := ui("POST", "/-/api/repositories", "ma", `{"name":"mvn-new","format":"maven","type":"hosted"}`); got != 201 {
		t.Fatalf("ma create maven: %d", got)
	}
	if got := ui("POST", "/-/api/repositories", "ma", `{"name":"raw-new","format":"raw","type":"hosted"}`); got != 403 {
		t.Fatalf("ma create raw: %d", got)
	}
	if got := ui("GET", "/-/api/repositories/raw-files/config", "ma", ""); got != 403 {
		t.Fatalf("ma raw config: %d", got)
	}
	if got := ui("GET", "/-/api/repositories/maven-releases/config", "ma", ""); got != 200 {
		t.Fatalf("ma maven config: %d", got)
	}
	var settings []map[string]any
	if err := json.Unmarshal(e.asWant("GET", v1+"/repositorySettings", "ma", "longenough1", "", 200), &settings); err != nil {
		t.Fatal(err)
	}
	for _, s := range settings {
		if s["format"] != "maven2" {
			t.Fatalf("ma sees %v", s["name"])
		}
	}
	e.asWant("GET", v1+"/repositories/raw/hosted/raw-files", "ma", "longenough1", "", 403)
	e.asWant("DELETE", v1+"/repositories/raw-files", "ma", "longenough1", "", 403)
	e.asWant("DELETE", v1+"/repositories/mvn-new", "ma", "longenough1", "", 204)
	e.asWant("GET", v1+"/repositorySettings", "op", "longenough1", "", 403)

	// The task operator: tasks read and run.
	e.asWant("GET", v1+"/tasks", "op", "longenough1", "", 200)
	e.asWant("GET", v1+"/security/users", "op", "longenough1", "", 403)
	if got := ui("POST", "/-/api/tasks", "op", `{"name":"x","type":"blobstore.compact","schedule":"manual"}`); got != 403 {
		t.Fatalf("op task create: %d", got)
	}
	var me map[string]any
	_, b = send(e, "GET", "/-/api/me", "", "op", "longenough1", true)
	if json.Unmarshal(b, &me) != nil || fmt.Sprint(me["permissions"]) != "map[tasks:[read run]]" || me["repositoryAdmin"] != false {
		t.Fatalf("me = %s", b)
	}
}

// Realms and anonymous access in Nexus terms (security-04, -05).
func TestRealmsAndAnonymousCompat(t *testing.T) {
	e := start(t, false)
	var avail []map[string]string
	e.get(v1+"/security/realms/available", "admin", &avail)
	if !slices.ContainsFunc(avail, func(m map[string]string) bool { return m["id"] == "NexusAuthenticatingRealm" }) ||
		!slices.ContainsFunc(avail, func(m map[string]string) bool { return m["id"] == "LdapRealm" }) {
		t.Fatalf("available = %v", avail)
	}
	var active []string
	e.get(v1+"/security/realms/active", "admin", &active)
	if len(active) == 0 || active[0] != "NexusAuthenticatingRealm" {
		t.Fatalf("active = %v", active)
	}
	// The list reads back as written; the token aliases are one realm.
	e.want("PUT", v1+"/security/realms/active", "admin", `["NexusAuthenticatingRealm","DockerToken","DefaultRole"]`, 204)
	e.get(v1+"/security/realms/active", "admin", &active)
	if strings.Join(active, ",") != "NexusAuthenticatingRealm,DockerToken,DefaultRole" {
		t.Fatalf("active = %v", active)
	}
	st, b := uiGet(e, "/-/api/realms")
	if st != 200 || !strings.Contains(string(b), `"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true}]`) {
		t.Fatalf("realms: %d %s", st, b)
	}
	b = e.want("PUT", v1+"/security/realms/active", "admin", `["NexusAuthenticatingRealm","Bogus"]`, 400)
	if !strings.Contains(string(b), "Unknown realmIds: [Bogus]") {
		t.Fatalf("unknown: %s", b)
	}
	e.want("PUT", v1+"/security/realms/active", "admin", `["DockerToken"]`, 409)                          // no admin could sign in
	e.want("PUT", v1+"/security/realms/active", "admin", `["LdapRealm","NexusAuthenticatingRealm"]`, 400) // LDAP not configured
	e.want("PUT", v1+"/security/realms/active", "ci", `["NexusAuthenticatingRealm"]`, 403)
	e.want("GET", v1+"/security/realms/active", "ci", "", 403)

	// Anonymous: the global anonymous read, a fixed identity.
	an := decode[map[string]any](t, e.want("GET", v1+"/security/anonymous", "admin", "", 200))
	if an["enabled"] != false || an["userId"] != "anonymous" || an["realmName"] != "NexusAuthorizingRealm" {
		t.Fatalf("anonymous = %v", an)
	}
	e.upload("maven-releases/org/a/1.0/a-1.0.jar")
	e.want("GET", "/repository/maven-releases/org/a/1.0/a-1.0.jar", "", "", 401)
	an = decode[map[string]any](t, e.want("PUT", v1+"/security/anonymous", "admin", `{"enabled":true,"userId":"anonymous","realmName":"NexusAuthorizingRealm"}`, 200))
	if an["enabled"] != true {
		t.Fatalf("anonymous = %v", an)
	}
	e.want("GET", "/repository/maven-releases/org/a/1.0/a-1.0.jar", "", "", 200)
	e.want("PUT", v1+"/security/anonymous", "admin", `{"enabled":true,"userId":"bob"}`, 400)
	e.want("PUT", v1+"/security/anonymous", "admin", `{"enabled":true,"realmName":"Other"}`, 400)
	e.want("PUT", "/repository/maven-releases/org/a/2.0/a-2.0.jar", "", "x", 401) // still no anonymous writes
}

// The users API in detail: statuses, ids, change-password, sources and
// read-only directory records, the self-delete guard, user sources
// (security-01, -15, -16, -17, -21, -22).
func TestNexusUsersDetails(t *testing.T) {
	e := start(t, false)
	u := decode[nxUserT](t, e.want("POST", v1+"/security/users", "admin",
		`{"userId":"lockd","firstName":"L","lastName":"D","emailAddress":"l@example.com","password":"longenough1","status":"locked","roles":[]}`, 200))
	if u.Status != "disabled" || u.FirstName != "L" || u.LastName != "D" {
		t.Fatalf("locked = %+v", u)
	}
	e.want("POST", v1+"/security/users", "admin", `{"userId":"cp","firstName":"C","lastName":"P","password":"longenough1","status":"changepassword"}`, 200)
	b := e.want("POST", v1+"/security/users", "admin", `{"userId":"a b","firstName":"A","lastName":"B","password":"longenough1","status":"active"}`, 400)
	if !strings.Contains(string(b), "invalid user id") {
		t.Fatalf("bad id: %s", b)
	}
	e.want("POST", v1+"/security/users", "admin", `{"userId":"nopw","firstName":"A","lastName":"B","status":"active"}`, 400)
	e.want("POST", v1+"/security/users", "admin", `{"userId":"adm2","firstName":"A","lastName":"Two","password":"longenough1","status":"active","roles":["nx-admin"]}`, 200)
	var users []nxUserT
	e.get(v1+"/security/users?userId=adm2", "admin", &users)
	if len(users) != 1 || strings.Join(users[0].Roles, ",") != "nx-admin" {
		t.Fatalf("adm2 = %+v", users)
	}
	// change-password: a text/plain body, then Basic with the new one.
	req, _ := http.NewRequest("PUT", e.ts.URL+v1+"/security/users/cp/change-password", strings.NewReader("brand-new-pw1"))
	req.SetBasicAuth("admin", "pw")
	req.Header.Set("Content-Type", "text/plain")
	resp, err := e.ts.Client().Do(req)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("change-password: %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	e.asWant("GET", v1+"/repositories", "cp", "brand-new-pw1", "", 200)
	e.asWant("GET", v1+"/repositories", "cp", "longenough1", "", 401)
	// A directory user's record: LDAP, read-only, needs its realm to go.
	if st, b := uiPost(e, "/-/api/users", `{"id":"dir","source":"ldap","roles":[]}`); st != 201 {
		t.Fatalf("ldap row: %d %s", st, b)
	}
	e.get(v1+"/security/users?source=LDAP", "admin", &users)
	if len(users) != 1 || users[0].UserID != "dir" || users[0].Source != "LDAP" || !users[0].ReadOnly {
		t.Fatalf("LDAP users = %+v", users)
	}
	e.get(v1+"/security/users?source=default", "admin", &users)
	if slices.ContainsFunc(users, func(u nxUserT) bool { return u.Source != "default" }) || len(users) != 5 {
		t.Fatalf("default users = %+v", users)
	}
	e.want("PUT", v1+"/security/users/dir", "admin", `{"userId":"dir","source":"LDAP","status":"active","roles":["deployer"]}`, 204)
	e.get(v1+"/security/users?userId=dir", "admin", &users)
	if strings.Join(users[0].Roles, ",") != "deployer" {
		t.Fatalf("dir roles = %+v", users[0])
	}
	e.want("DELETE", v1+"/security/users/dir", "admin", "", 400)
	e.want("DELETE", v1+"/security/users/dir?realm=Crowd", "admin", "", 404)
	e.want("DELETE", v1+"/security/users/dir?realm=LdapRealm", "admin", "", 204)
	// A role mapping for a directory user with no record yet creates one.
	e.want("PUT", v1+"/security/users/dir2", "admin", `{"userId":"dir2","source":"LDAP","status":"active","roles":["deployer"]}`, 204)
	if st, b := uiGet(e, "/-/api/users/dir2"); st != 200 || !strings.Contains(string(b), `"source":"ldap"`) {
		t.Fatalf("dir2: %d %s", st, b)
	}
	// Nobody deletes the account they are signed in with.
	b = e.want("DELETE", v1+"/security/users/admin", "admin", "", 409)
	if !strings.Contains(string(b), "signed in as") {
		t.Fatalf("self delete: %s", b)
	}
	var sources []map[string]string
	e.get(v1+"/security/user-sources", "admin", &sources)
	if len(sources) != 1 || sources[0]["id"] != "default" {
		t.Fatalf("sources = %v", sources)
	}
	e.want("GET", v1+"/security/roles?source=bogus", "admin", "", 400)
	var roles []nxRoleT
	e.get(v1+"/security/roles?source=LDAP", "admin", &roles)
	if len(roles) != 0 {
		t.Fatalf("LDAP roles = %+v", roles)
	}
	e.asWant("DELETE", v1+"/security/users/admin", "adm2", "longenough1", "", 204) // by another admin
}
