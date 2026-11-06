package main

import "fmt"

// steps is the comparison, in order. Names are unique: known.txt refers to them.
func steps() []step {
	var s []step
	add := func(name, method, path, body string) *step {
		s = append(s, step{name: name, method: method, path: path, body: body})
		return &s[len(s)-1]
	}
	statusOnly := func(name, method, path, body string) {
		add(name, method, path, body).status = true
	}
	storage := `"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW_ONCE"}`
	proxy := `"proxy":{"remoteUrl":"http://127.0.0.1:9/","contentMaxAge":1440,"metadataMaxAge":1440},"negativeCache":{"enabled":true,"timeToLive":1440},"httpClient":{"blocked":false,"autoBlock":true}`

	// Status and system.
	statusOnly("status", "GET", v1+"/status", "")
	statusOnly("status-writable", "GET", v1+"/status/writable", "")
	statusOnly("status-check", "GET", v1+"/status/check", "")
	s[len(s)-1].as = "anon"
	statusOnly("formats-specs", "GET", v1+"/formats/upload-specs", "")

	// Blob stores.
	statusOnly("blobstores-list", "GET", v1+"/blobstores", "")
	add("blobstore-file-create", "POST", v1+"/blobstores/file", `{"name":"diff-store","path":"diff-store","softQuota":{"type":"spaceRemainingQuota","limit":1048576}}`)
	add("blobstore-file-get", "GET", v1+"/blobstores/file/diff-store", "")
	add("blobstore-quota", "GET", v1+"/blobstores/diff-store/quota-status", "")
	add("blobstore-missing", "GET", v1+"/blobstores/file/diff-missing", "")

	// Repositories of each format and type.
	for _, r := range []struct{ format, typ, body string }{
		{"raw", "hosted", storage + `,"raw":{"contentDisposition":"ATTACHMENT"}`},
		{"raw", "proxy", storage + `,` + proxy + `,"raw":{"contentDisposition":"ATTACHMENT"}`},
		{"maven", "hosted", storage + `,"maven":{"versionPolicy":"RELEASE","layoutPolicy":"STRICT","contentDisposition":"INLINE"}`},
		{"maven", "proxy", storage + `,` + proxy + `,"maven":{"versionPolicy":"RELEASE","layoutPolicy":"PERMISSIVE","contentDisposition":"INLINE"}`},
		{"npm", "hosted", storage},
		{"npm", "proxy", storage + `,` + proxy},
		{"pypi", "hosted", storage},
		{"pypi", "proxy", storage + `,` + proxy},
		{"docker", "hosted", storage + `,"docker":{"v1Enabled":false,"forceBasicAuth":true}`},
		{"docker", "proxy", storage + `,` + proxy + `,"docker":{"v1Enabled":false,"forceBasicAuth":true},"dockerProxy":{"indexType":"REGISTRY"}`},
		{"helm", "hosted", storage},
		{"helm", "proxy", storage + `,` + proxy},
		{"yum", "hosted", storage + `,"yum":{"repodataDepth":0,"deployPolicy":"STRICT"}`},
		{"yum", "proxy", storage + `,` + proxy},
		{"apt", "hosted", storage + `,"apt":{"distribution":"bookworm"},"aptSigning":{"keypair":"","passphrase":""}`},
		{"apt", "proxy", storage + `,` + proxy + `,"apt":{"distribution":"bookworm","flat":false}`},
		{"nuget", "hosted", storage},
		{"nuget", "proxy", storage + `,` + proxy + `,"nugetProxy":{"queryCacheItemMaxAge":3600,"nugetVersion":"V3"}`},
		{"rubygems", "hosted", storage},
		{"rubygems", "proxy", storage + `,` + proxy},
		{"go", "proxy", storage + `,` + proxy},
		{"cargo", "hosted", storage},
		{"conda", "proxy", storage + `,` + proxy},
		{"conan", "proxy", storage + `,` + proxy},
		{"r", "hosted", storage},
		{"gitlfs", "hosted", storage},
		{"cocoapods", "proxy", storage + `,` + proxy},
		{"p2", "proxy", storage + `,` + proxy},
	} {
		name := "diff-" + r.format + "-" + r.typ
		tag := r.format + "-" + r.typ
		add("repo-create-"+tag, "POST", v1+"/repositories/"+r.format+"/"+r.typ, fmt.Sprintf(`{"name":%q,"online":true,%s}`, name, r.body))
		add("repo-get-"+tag, "GET", v1+"/repositories/"+r.format+"/"+r.typ+"/"+name, "")
		add("repo-view-"+tag, "GET", v1+"/repositories/"+name, "")
	}
	add("repo-group-maven", "POST", v1+"/repositories/maven/group", `{"name":"diff-maven-group","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true},"group":{"memberNames":["diff-maven-hosted","diff-maven-proxy"]}}`)
	add("repo-group-maven-get", "GET", v1+"/repositories/maven/group/diff-maven-group", "")
	add("repo-group-npm", "POST", v1+"/repositories/npm/group", `{"name":"diff-npm-group","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":true},"group":{"memberNames":["diff-npm-hosted","diff-npm-proxy"]}}`)
	add("repo-group-npm-get", "GET", v1+"/repositories/npm/group/diff-npm-group", "")
	add("repo-duplicate", "POST", v1+"/repositories/raw/hosted", `{"name":"diff-raw-hosted","online":true,`+storage+`}`)
	add("repo-bad-name", "POST", v1+"/repositories/raw/hosted", `{"name":"bad name!","online":true,`+storage+`}`)
	add("repo-missing-storage", "POST", v1+"/repositories/raw/hosted", `{"name":"diff-nostorage","online":true}`)
	add("repo-unknown-store", "POST", v1+"/repositories/raw/hosted", `{"name":"diff-nostore","online":true,"storage":{"blobStoreName":"nope","strictContentTypeValidation":true,"writePolicy":"ALLOW"}}`)
	add("repo-update-name-mismatch", "PUT", v1+"/repositories/raw/hosted/diff-raw-hosted", `{"name":"other","online":true,`+storage+`}`)
	add("repo-update", "PUT", v1+"/repositories/raw/hosted/diff-raw-hosted", `{"name":"diff-raw-hosted","online":true,"storage":{"blobStoreName":"default","strictContentTypeValidation":false,"writePolicy":"ALLOW"},"raw":{"contentDisposition":"INLINE"}}`)
	add("repo-after-update", "GET", v1+"/repositories/raw/hosted/diff-raw-hosted", "")
	add("repo-missing", "GET", v1+"/repositories/diff-missing", "")
	add("repo-invalidate-hosted", "POST", v1+"/repositories/diff-raw-hosted/invalidate-cache", "")
	add("repo-invalidate-proxy", "POST", v1+"/repositories/diff-raw-proxy/invalidate-cache", "")
	add("repo-rebuild-index", "POST", v1+"/repositories/diff-raw-hosted/rebuild-index", "")

	// Uploads, listings and searches.
	s = append(s, step{name: "upload-raw", method: "POST", path: v1 + "/components?repository=diff-raw-hosted",
		parts: map[string]string{"raw.directory": "/dir/sub", "raw.asset1": "file:a.txt", "raw.asset1.filename": "a.txt", "raw.asset2": "file:b.txt", "raw.asset2.filename": "b.txt"}})
	s = append(s, step{name: "upload-maven", method: "POST", path: v1 + "/components?repository=diff-maven-hosted",
		parts: map[string]string{"maven2.groupId": "org.diff", "maven2.artifactId": "lib", "maven2.version": "1.0", "maven2.generate-pom": "true", "maven2.packaging": "jar",
			"maven2.asset1": "file:lib-1.0.jar", "maven2.asset1.extension": "jar", "maven2.asset2": "file:lib-1.0-sources.jar", "maven2.asset2.extension": "jar", "maven2.asset2.classifier": "sources"}})
	s = append(s, step{name: "upload-maven-again", method: "POST", path: v1 + "/components?repository=diff-maven-hosted", status: true,
		parts: map[string]string{"maven2.groupId": "org.diff", "maven2.artifactId": "lib", "maven2.version": "1.0", "maven2.asset1": "file:lib-1.0.jar", "maven2.asset1.extension": "jar"}})
	s = append(s, step{name: "upload-no-repo", method: "POST", path: v1 + "/components", status: true, parts: map[string]string{"raw.directory": "/x", "raw.asset1": "file:x", "raw.asset1.filename": "x"}})
	s = append(s, step{name: "upload-missing-field", method: "POST", path: v1 + "/components?repository=diff-maven-hosted", parts: map[string]string{"maven2.groupId": "org.diff"}})
	add("components-raw", "GET", v1+"/components?repository=diff-raw-hosted", "")
	add("components-maven", "GET", v1+"/components?repository=diff-maven-hosted", "")
	add("components-no-repo", "GET", v1+"/components", "")
	add("components-group", "GET", v1+"/components?repository=diff-maven-group", "")
	add("assets-raw", "GET", v1+"/assets?repository=diff-raw-hosted", "")
	add("assets-maven", "GET", v1+"/assets?repository=diff-maven-hosted", "")
	add("search-name", "GET", v1+"/search?name=lib&repository=diff-maven-hosted", "")
	add("search-gav", "GET", v1+"/search?maven.groupId=org.diff&maven.artifactId=lib&maven.baseVersion=1.0", "")
	add("search-wild", "GET", v1+"/search?name=li*", "")
	add("search-format", "GET", v1+"/search?format=raw", "")
	add("search-assets", "GET", v1+"/search/assets?repository=diff-maven-hosted&maven.classifier=sources", "")
	add("search-assets-main", "GET", v1+"/search/assets?repository=diff-maven-hosted&maven.classifier=&maven.extension=jar", "")
	statusOnly("search-download-one", "GET", v1+"/search/assets/download?repository=diff-maven-hosted&maven.classifier=sources", "")
	statusOnly("search-download-many", "GET", v1+"/search/assets/download?repository=diff-maven-hosted", "")
	statusOnly("search-download-none", "GET", v1+"/search/assets/download?repository=diff-maven-hosted&name=none", "")
	statusOnly("search-bad-sort", "GET", v1+"/search?sort=nope", "")
	statusOnly("component-bad-id", "GET", v1+"/components/nope", "")
	statusOnly("asset-bad-id", "GET", v1+"/assets/nope", "")

	// Repository paths: what plain HTTP clients and Maven see.
	raw := func(name, method, path string) {
		s = append(s, step{name: name, method: method, path: path, status: true})
	}
	s = append(s, step{name: "raw-get", method: "GET", path: "/repository/diff-raw-hosted/dir/sub/a.txt", raw: true})
	raw("raw-head", "HEAD", "/repository/diff-raw-hosted/dir/sub/a.txt")
	s = append(s, step{name: "raw-range", method: "GET", path: "/repository/diff-raw-hosted/dir/sub/a.txt", raw: true, header: map[string]string{"Range": "bytes=2-5"}})
	raw("raw-missing", "GET", "/repository/diff-raw-hosted/dir/none.txt")
	raw("raw-dir", "GET", "/repository/diff-raw-hosted/dir/sub/")
	s = append(s, step{name: "raw-put", method: "PUT", path: "/repository/diff-raw-hosted/put/c.txt", body: "c", ctype: "text/plain", status: true})
	s = append(s, step{name: "raw-put-again", method: "PUT", path: "/repository/diff-raw-hosted/put/c.txt", body: "c2", ctype: "text/plain", status: true})
	raw("raw-delete", "DELETE", "/repository/diff-raw-hosted/put/c.txt")
	raw("raw-delete-again", "DELETE", "/repository/diff-raw-hosted/put/c.txt")
	raw("raw-unknown-repo", "GET", "/repository/diff-missing/a.txt")
	s = append(s, step{name: "raw-anon-put", method: "PUT", path: "/repository/diff-raw-hosted/put/anon.txt", body: "x", ctype: "text/plain", as: "anon", status: true})
	raw("maven-jar", "GET", "/repository/diff-maven-hosted/org/diff/lib/1.0/lib-1.0.jar")
	raw("maven-sha1", "GET", "/repository/diff-maven-hosted/org/diff/lib/1.0/lib-1.0.jar.sha1")
	raw("maven-sha256", "GET", "/repository/diff-maven-hosted/org/diff/lib/1.0/lib-1.0.jar.sha256")
	raw("maven-metadata", "GET", "/repository/diff-maven-hosted/org/diff/lib/maven-metadata.xml")
	raw("maven-group-jar", "GET", "/repository/diff-maven-group/org/diff/lib/1.0/lib-1.0.jar")
	raw("maven-group-put", "PUT", "/repository/diff-maven-group/org/diff/lib/2.0/lib-2.0.jar")
	s = append(s, step{name: "maven-bad-layout", method: "PUT", path: "/repository/diff-maven-hosted/not/a/layout.txt", body: "x", ctype: "text/plain", status: true})
	s = append(s, step{name: "maven-snapshot-in-release", method: "PUT", path: "/repository/diff-maven-hosted/org/diff/lib/2.0-SNAPSHOT/lib-2.0-SNAPSHOT.jar", body: "x", ctype: "application/java-archive", status: true})

	// Security.
	statusOnly("realms-available", "GET", v1+"/security/realms/available", "")
	add("realms-active", "GET", v1+"/security/realms/active", "")
	add("anonymous", "GET", v1+"/security/anonymous", "")
	add("user-sources", "GET", v1+"/security/user-sources", "")
	add("selector-create", "POST", v1+"/security/content-selectors", `{"name":"diff-sel","description":"d","expression":"format == \"raw\" and path =^ \"/dir/\""}`)
	add("selector-get", "GET", v1+"/security/content-selectors/diff-sel", "")
	add("selector-bad", "POST", v1+"/security/content-selectors", `{"name":"diff-bad","description":"d","expression":"nonsense("}`)
	add("privilege-view", "POST", v1+"/security/privileges/repository-view", `{"name":"diff-view","description":"d","actions":["READ","BROWSE"],"format":"raw","repository":"diff-raw-hosted"}`)
	add("privilege-view-get", "GET", v1+"/security/privileges/diff-view", "")
	add("privilege-selector", "POST", v1+"/security/privileges/repository-content-selector", `{"name":"diff-csel","description":"d","actions":["READ"],"format":"raw","repository":"diff-raw-hosted","contentSelector":"diff-sel"}`)
	add("privilege-selector-get", "GET", v1+"/security/privileges/diff-csel", "")
	add("privilege-app", "POST", v1+"/security/privileges/application", `{"name":"diff-app","description":"d","actions":["READ"],"domain":"users"}`)
	add("privilege-app-get", "GET", v1+"/security/privileges/diff-app", "")
	add("privilege-wild", "POST", v1+"/security/privileges/wildcard", `{"name":"diff-wild","description":"d","pattern":"nexus:repository-view:raw:diff-raw-hosted:read"}`)
	add("privilege-wild-get", "GET", v1+"/security/privileges/diff-wild", "")
	add("privilege-missing", "GET", v1+"/security/privileges/diff-missing", "")
	add("role-create", "POST", v1+"/security/roles", `{"id":"diff-role","name":"diff role","description":"d","privileges":["diff-view"],"roles":[]}`)
	add("role-get", "GET", v1+"/security/roles/diff-role", "")
	add("role-nested", "POST", v1+"/security/roles", `{"id":"diff-nested","name":"nested","description":"d","privileges":[],"roles":["diff-role"]}`)
	add("role-nested-get", "GET", v1+"/security/roles/diff-nested", "")
	add("role-missing", "GET", v1+"/security/roles/diff-missing", "")
	add("role-bad-privilege", "POST", v1+"/security/roles", `{"id":"diff-badp","name":"x","description":"d","privileges":["nope"],"roles":[]}`)
	add("user-create", "POST", v1+"/security/users", `{"userId":"diff-user","firstName":"Diff","lastName":"User","emailAddress":"diff@example.com","password":"Diff-password-1","status":"active","roles":["diff-role"]}`)
	add("user-list", "GET", v1+"/security/users?userId=diff-user", "")
	add("user-duplicate", "POST", v1+"/security/users", `{"userId":"diff-user","firstName":"Diff","lastName":"User","emailAddress":"diff@example.com","password":"Diff-password-1","status":"active","roles":["diff-role"]}`)
	add("user-bad-role", "POST", v1+"/security/users", `{"userId":"diff-user2","firstName":"D","lastName":"U","emailAddress":"d2@example.com","password":"Diff-password-1","status":"active","roles":["nope"]}`)

	// What that user may do.
	s = append(s, step{name: "as-user-read", method: "GET", path: "/repository/diff-raw-hosted/dir/sub/a.txt", as: "diff-user", status: true})
	s = append(s, step{name: "as-user-other-repo", method: "GET", path: "/repository/diff-maven-hosted/org/diff/lib/1.0/lib-1.0.jar", as: "diff-user", status: true})
	s = append(s, step{name: "as-user-write", method: "PUT", path: "/repository/diff-raw-hosted/put/u.txt", body: "x", ctype: "text/plain", as: "diff-user", status: true})
	s = append(s, step{name: "as-user-admin", method: "GET", path: v1 + "/security/users", as: "diff-user", status: true})
	s = append(s, step{name: "as-user-components", method: "GET", path: v1 + "/components?repository=diff-raw-hosted", as: "diff-user"})
	s = append(s, step{name: "as-anon-admin", method: "GET", path: v1 + "/security/users", as: "anon", status: true})
	s = append(s, step{name: "as-wrong-password", method: "GET", path: v1 + "/security/users", as: "diff-nobody", status: true})

	// Routing rules, tasks, e-mail.
	add("routing-create", "POST", v1+"/routing-rules", `{"name":"diff-rule","description":"d","mode":"BLOCK","matchers":["^/secret/.*"]}`)
	add("routing-get", "GET", v1+"/routing-rules/diff-rule", "")
	add("routing-bad", "POST", v1+"/routing-rules", `{"name":"diff-rule2","description":"d","mode":"NOPE","matchers":[]}`)
	statusOnly("tasks-list", "GET", v1+"/tasks", "")
	statusOnly("tasks-type", "GET", v1+"/tasks?type=blobstore.compact", "")
	add("email", "GET", v1+"/email", "")

	// Cleaning up what the steps made, in order.
	statusOnly("role-in-use", "DELETE", v1+"/security/roles/diff-role", "")
	statusOnly("blobstore-in-use", "DELETE", v1+"/blobstores/default", "")
	statusOnly("user-delete", "DELETE", v1+"/security/users/diff-user", "")
	statusOnly("role-delete-nested", "DELETE", v1+"/security/roles/diff-nested", "")
	statusOnly("role-delete", "DELETE", v1+"/security/roles/diff-role", "")
	statusOnly("privilege-delete", "DELETE", v1+"/security/privileges/diff-view", "")
	statusOnly("selector-delete-used", "DELETE", v1+"/security/content-selectors/diff-sel", "")
	statusOnly("repo-delete", "DELETE", v1+"/repositories/diff-raw-proxy", "")
	statusOnly("repo-delete-missing", "DELETE", v1+"/repositories/diff-raw-proxy", "")
	return s
}
