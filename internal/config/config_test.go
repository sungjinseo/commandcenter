package config

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const validYAML = `
anonymousRead: true
users:
  - name: admin
    passwordHash: "$2a$04$abcdefghijklmnopqrstuuJ8m5mQ0xkqmEoU5I2xP0x1Q3pYJ5K2e"
    roles: [admin]
  - name: ci
    passwordHash: "$2a$04$abcdefghijklmnopqrstuuJ8m5mQ0xkqmEoU5I2xP0x1Q3pYJ5K2e"
    roles: [deployer]
roles:
  deployer: { write: ["maven-releases"] }
repositories:
  - { name: maven-central,  format: maven, type: proxy, remote: "https://repo1.maven.org/maven2" }
  - { name: maven-releases, format: maven, type: hosted }
  - { name: maven-public,   format: maven, type: group, members: [maven-releases, maven-central], writeMember: maven-releases }
`

func TestParseAppliesDefaults(t *testing.T) {
	c, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8081" || c.BaseURL != "http://localhost:8081" || c.DataDir != "/data" || c.MemoryLimitMiB != 80 {
		t.Fatalf("server defaults not applied: %+v", c)
	}
	proxy := c.Repositories[0]
	if time.Duration(proxy.MetadataMaxAge) != 24*time.Hour || proxy.ContentMaxAge != Never || time.Duration(proxy.NegativeCacheTTL) != 10*time.Minute {
		t.Fatalf("proxy defaults not applied: %+v", proxy)
	}
	hosted := c.Repositories[1]
	if hosted.VersionPolicy != "release" || hosted.WritePolicy != "allow_once" {
		t.Fatalf("maven hosted defaults not applied: %+v", hosted)
	}
}

func TestParseDurations(t *testing.T) {
	c, err := Parse([]byte(`
repositories:
  - { name: p, format: maven, type: proxy, remote: "https://x.example/", metadataMaxAge: "30m", contentMaxAge: "-1", negativeCacheTTL: "1m" }
`))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Repositories[0]
	if time.Duration(r.MetadataMaxAge) != 30*time.Minute || r.ContentMaxAge != Never || time.Duration(r.NegativeCacheTTL) != time.Minute {
		t.Fatalf("durations: %+v", r)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"unknown field":   {"bogus: 1\n", "field bogus not found"},
		"unknown format":  {"repositories: [{name: a, format: nuget, type: hosted}]", `repository "a": unknown format "nuget"`},
		"unknown type":    {"repositories: [{name: a, format: maven, type: virtual}]", `repository "a": unknown type "virtual"`},
		"duplicate name":  {"repositories: [{name: a, format: maven, type: hosted}, {name: a, format: maven, type: hosted}]", `duplicate repository name "a"`},
		"proxy no remote": {"repositories: [{name: a, format: maven, type: proxy}]", `repository "a": proxy needs an http(s) remote`},
		"missing member":  {"repositories: [{name: g, format: maven, type: group, members: [x]}]", `repository "g": unknown member "x"`},
		"cross format": {`repositories:
  - {name: n, format: npm, type: hosted}
  - {name: g, format: maven, type: group, members: [n]}`, `repository "g": member "n" has format npm`},
		"cycle": {`repositories:
  - {name: a, format: maven, type: group, members: [b]}
  - {name: b, format: maven, type: group, members: [a]}`, "group cycle"},
		"bad writeMember": {`repositories:
  - {name: h, format: maven, type: hosted}
  - {name: g, format: maven, type: group, members: [h], writeMember: x}`, `repository "g": writeMember "x" must be a hosted member`},
		"versionPolicy on npm": {"repositories: [{name: a, format: npm, type: hosted, versionPolicy: release}]", `repository "a": versionPolicy is only valid for maven`},
		"unknown role":         {"users: [{name: u, passwordHash: x, roles: [nope]}]", `user "u": unknown role "nope"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Problems in the repositories section are reported in
			// RepositoriesErr, and problems with users and roles in AccessErr,
			// not by Parse (plan 9 fix round 1, plan 12 fix round 1).
			c, err := Parse([]byte(tc.yaml))
			if err == nil {
				err = errors.Join(c.RepositoriesErr, c.AccessErr, c.SettingsErr)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestInvalidRepositoriesSectionDoesNotFailParse(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"invalid":       {"repositories: [{name: a, format: nuget, type: hosted}]", `repository "a": unknown format "nuget"`},
		"unknown field": {"repositories: [{name: a, format: maven, type: hosted, bogus: 1}]", "field bogus not found"},
		"bad duration":  {"repositories: [{name: a, format: maven, type: proxy, remote: 'https://x/', contentMaxAge: soon}]", `invalid duration "soon"`},
		"not a list":    {"repositories: 5", "repositories"},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Parse([]byte("listen: ':9000'\n" + tc.yaml + "\nusers: [{name: u, passwordHash: x}]\n"))
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}
			if c.RepositoriesErr == nil || !strings.Contains(c.RepositoriesErr.Error(), tc.want) {
				t.Fatalf("RepositoriesErr = %v, want %q", c.RepositoriesErr, tc.want)
			}
			if c.Listen != ":9000" || len(c.Users) != 1 {
				t.Fatalf("the rest of the file was not parsed: %+v", c)
			}
		})
	}
	// The rest of the file is still checked strictly.
	if _, err := Parse([]byte("repositories: [{name: a, format: nuget, type: hosted}]\nbogus: 1\n")); err == nil ||
		!strings.Contains(err.Error(), "field bogus not found") {
		t.Fatalf("top-level unknown field: %v", err)
	}
	// Users and roles are validated too, but reported in AccessErr: they only
	// matter until the first import (plan 12 fix round 1).
	if c, err := Parse([]byte("users: [{name: u, passwordHash: x, roles: [nope]}]\n")); err != nil || c.AccessErr == nil ||
		!strings.Contains(c.AccessErr.Error(), `unknown role "nope"`) {
		t.Fatalf("users section: %v %v", err, c)
	}
	c, err := Parse([]byte(validYAML))
	if err != nil || c.RepositoriesErr != nil || len(c.Repositories) != 3 {
		t.Fatalf("valid: %v %v %d", err, c.RepositoriesErr, len(c.Repositories))
	}
}

func TestMaxUploadDefault(t *testing.T) {
	c, err := Parse([]byte("{}"))
	if err != nil || c.MaxUploadMiB != 1024 {
		t.Fatalf("MaxUploadMiB = %d err=%v", c.MaxUploadMiB, err)
	}
	// maxUploadMiB and baseURL are import-only: problems are reported in
	// SettingsErr, not by Parse, so a stale value cannot block a later start.
	for _, y := range []string{"maxUploadMiB: -1\n", "maxUploadMiB: 1048577\n"} {
		c, err := Parse([]byte(y))
		if err != nil || c.SettingsErr == nil {
			t.Fatalf("%q: err=%v SettingsErr=%v", y, err, c.SettingsErr)
		}
	}
	if c, err := Parse([]byte("maxUploadMiB: 1048576\n")); err != nil || c.SettingsErr != nil {
		t.Fatalf("1 TiB must be accepted: %v %v", err, c.SettingsErr)
	}
	if c, err := Parse([]byte("baseURL: ftp://x\n")); err != nil || c.SettingsErr == nil {
		t.Fatalf("bad baseURL: err=%v SettingsErr=%v", err, c.SettingsErr)
	}
}

// QA N-24: user info in baseURL would reach every client in the URLs built
// on it, and a '?' or '#' would cut them; the error never repeats a password.
func TestBaseURLNoUserInfoQueryOrFragment(t *testing.T) {
	for _, v := range []string{"http://ci:s3cret@repo.example", "http://repo.example/path?q=1", "http://repo.example/path#f", "http://repo.example?", "http://repo.example/#"} {
		c, err := Parse([]byte("baseURL: \"" + v + "\"\n"))
		if err != nil || c.SettingsErr == nil || strings.Contains(c.SettingsErr.Error(), "s3cret") {
			t.Fatalf("%q: err=%v SettingsErr=%v", v, err, c.SettingsErr)
		}
	}
	for _, v := range []string{"https://repo.example", "http://repo.example:8081/nexus", "http://[::1]:8081"} {
		if c, err := Parse([]byte("baseURL: \"" + v + "\"\n")); err != nil || c.SettingsErr != nil {
			t.Fatalf("%q: err=%v SettingsErr=%v", v, err, c.SettingsErr)
		}
	}
}

func TestRepositoryOnlineDefault(t *testing.T) {
	var r Repository
	if !r.IsOnline() {
		t.Fatal("unset online must read as online")
	}
	ApplyRepositoryDefaults(&r)
	if r.Online == nil || !*r.Online {
		t.Fatalf("defaults: %v", r.Online)
	}
	off := false
	r.Online = &off
	ApplyRepositoryDefaults(&r)
	if r.IsOnline() {
		t.Fatal("explicit offline was overwritten")
	}
	// A stored row from before plan 12 has no online key.
	var old Repository
	if err := json.Unmarshal([]byte(`{"name":"a","format":"npm","type":"hosted"}`), &old); err != nil || !old.IsOnline() {
		t.Fatalf("old row: %v online=%v", err, old.IsOnline())
	}
	b, _ := json.Marshal(r)
	var back Repository
	if err := json.Unmarshal(b, &back); err != nil || back.IsOnline() {
		t.Fatalf("offline did not round-trip: %s", b)
	}
}

func TestRepositoryBlobStoreDefault(t *testing.T) {
	// Non-group repositories default to the "default" store.
	for _, typ := range []string{"hosted", "proxy"} {
		r := Repository{Name: "r", Format: "maven", Type: typ, Remote: "https://x.example/"}
		ApplyRepositoryDefaults(&r)
		if r.BlobStore != "default" {
			t.Fatalf("%s: blobStore = %q, want default", typ, r.BlobStore)
		}
	}
	// An explicit store is kept.
	r := Repository{Name: "r", Format: "maven", Type: "hosted", BlobStore: "fast"}
	ApplyRepositoryDefaults(&r)
	if r.BlobStore != "fast" {
		t.Fatalf("explicit blobStore overwritten: %q", r.BlobStore)
	}
	// Groups get no store.
	g := Repository{Name: "g", Format: "maven", Type: "group", Members: []string{"r"}}
	ApplyRepositoryDefaults(&g)
	if g.BlobStore != "" {
		t.Fatalf("group blobStore = %q, want empty", g.BlobStore)
	}
	// A stored row from before plan 12 has no blobStore key; it resolves to
	// default at routing time (empty here).
	var old Repository
	if err := json.Unmarshal([]byte(`{"name":"a","format":"npm","type":"hosted"}`), &old); err != nil || old.BlobStore != "" {
		t.Fatalf("old row: %v blobStore=%q", err, old.BlobStore)
	}
}

func TestValidateRepositoriesRejectsGroupBlobStore(t *testing.T) {
	list := []Repository{
		{Name: "h", Format: "maven", Type: "hosted"},
		{Name: "g", Format: "maven", Type: "group", Members: []string{"h"}, BlobStore: "fast"},
	}
	for i := range list {
		ApplyRepositoryDefaults(&list[i])
	}
	err := ValidateRepositories(list)
	if err == nil || !strings.Contains(err.Error(), "groups have no blob store") {
		t.Fatalf("expected group blob-store rejection, got %v", err)
	}
}

func TestRoleNamingUnknownRepoIsAccepted(t *testing.T) {
	// The repository may live in the metadata database (plan 9 Decision 2).
	if _, err := Parse([]byte("roles: {r: {write: [nope]}}")); err != nil {
		t.Fatalf("role naming an unknown repository: %v", err)
	}
}

func TestValidateRepositoriesCatchesEveryRule(t *testing.T) {
	cases := map[string]struct {
		repos []Repository
		want  string
	}{
		"invalid name":    {[]Repository{{Name: "a/b", Format: "maven", Type: "hosted"}}, `invalid repository name "a/b"`},
		"empty name":      {[]Repository{{Format: "maven", Type: "hosted"}}, `invalid repository name ""`},
		"duplicate":       {[]Repository{{Name: "a", Format: "maven", Type: "hosted"}, {Name: "a", Format: "maven", Type: "hosted"}}, `duplicate repository name "a"`},
		"unknown format":  {[]Repository{{Name: "a", Format: "nuget", Type: "hosted"}}, `repository "a": unknown format "nuget"`},
		"unknown type":    {[]Repository{{Name: "a", Format: "maven", Type: "virtual"}}, `repository "a": unknown type "virtual"`},
		"proxy no remote": {[]Repository{{Name: "a", Format: "maven", Type: "proxy", Remote: "ftp://x"}}, `repository "a": proxy needs an http(s) remote`},
		"group no member": {[]Repository{{Name: "g", Format: "maven", Type: "group"}}, `repository "g": group needs members`},
		"members on hosted": {[]Repository{{Name: "a", Format: "maven", Type: "hosted", WriteMember: "x"}},
			`repository "a": members/writeMember are only valid for groups`},
		"versionPolicy npm":   {[]Repository{{Name: "a", Format: "npm", Type: "hosted", VersionPolicy: "release"}}, `repository "a": versionPolicy is only valid for maven`},
		"bad versionPolicy":   {[]Repository{{Name: "a", Format: "maven", Type: "hosted", VersionPolicy: "odd"}}, `repository "a": unknown versionPolicy "odd"`},
		"bad writePolicy":     {[]Repository{{Name: "a", Format: "maven", Type: "hosted", WritePolicy: "maybe"}}, `repository "a": unknown writePolicy "maybe"`},
		"missing member":      {[]Repository{{Name: "g", Format: "maven", Type: "group", Members: []string{"x"}}}, `repository "g": unknown member "x"`},
		"cross format member": {[]Repository{{Name: "n", Format: "npm", Type: "hosted"}, {Name: "g", Format: "maven", Type: "group", Members: []string{"n"}}}, `repository "g": member "n" has format npm`},
		"bad writeMember": {[]Repository{{Name: "p", Format: "maven", Type: "proxy", Remote: "https://x.example/"},
			{Name: "g", Format: "maven", Type: "group", Members: []string{"p"}, WriteMember: "p"}}, `repository "g": writeMember "p" must be a hosted member`},
		"cleanupPolicy on group": {[]Repository{{Name: "h", Format: "maven", Type: "hosted"},
			{Name: "g", Format: "maven", Type: "group", Members: []string{"h"}, CleanupPolicy: "p"}},
			`repository "g": cleanupPolicy is only valid for hosted and proxy repositories`},
		"cleanupPolicy on cargo hosted": {[]Repository{{Name: "c", Format: "cargo", Type: "hosted", CleanupPolicy: "p"}},
			`repository "c": cleanup is not supported for cargo hosted repositories`},
		"cycle": {[]Repository{{Name: "a", Format: "maven", Type: "group", Members: []string{"b"}},
			{Name: "b", Format: "maven", Type: "group", Members: []string{"a"}}}, "group cycle involving repository"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateRepositories(tc.repos)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
	ok := []Repository{
		{Name: "h", Format: "maven", Type: "hosted", WritePolicy: "allow", VersionPolicy: "mixed"},
		{Name: "p", Format: "maven", Type: "proxy", Remote: "https://x.example/", CleanupPolicy: "pol"},
		{Name: "g", Format: "maven", Type: "group", Members: []string{"h", "p"}, WriteMember: "h"},
		{Name: "cp", Format: "cargo", Type: "proxy", Remote: "https://x.example/", CleanupPolicy: "pol"},
		{Name: "dh", Format: "docker", Type: "hosted"},
		{Name: "dp", Format: "docker", Type: "proxy", Remote: "https://registry-1.docker.io"},
		{Name: "dg", Format: "docker", Type: "group", Members: []string{"dh", "dp"}},
	}
	if err := ValidateRepositories(ok); err != nil {
		t.Fatalf("valid list: %v", err)
	}
}

func TestValidateRepositoriesReturnsEveryError(t *testing.T) {
	err := ValidateRepositories([]Repository{
		{Name: "a", Format: "nuget", Type: "hosted"},
		{Name: "b", Format: "maven", Type: "virtual"},
	})
	u, ok := err.(interface{ Unwrap() []error })
	if !ok || len(u.Unwrap()) != 2 {
		t.Fatalf("want 2 joined errors, got %v", err)
	}
}

func TestApplyRepositoryDefaults(t *testing.T) {
	p := Repository{Name: "p", Format: "npm", Type: "proxy"}
	ApplyRepositoryDefaults(&p)
	if time.Duration(p.MetadataMaxAge) != 24*time.Hour || p.ContentMaxAge != Never || time.Duration(p.NegativeCacheTTL) != 10*time.Minute {
		t.Fatalf("proxy defaults: %+v", p)
	}
	h := Repository{Name: "h", Format: "maven", Type: "hosted"}
	ApplyRepositoryDefaults(&h)
	if h.WritePolicy != "allow_once" || h.VersionPolicy != "release" {
		t.Fatalf("hosted defaults: %+v", h)
	}
}

func TestDurationJSON(t *testing.T) {
	type wrap struct {
		D Duration `json:"d"`
	}
	for _, tc := range []struct {
		d    Duration
		json string
	}{
		{Duration(24 * time.Hour), `{"d":"24h"}`},
		{Duration(10 * time.Minute), `{"d":"10m"}`},
		{Duration(90 * time.Second), `{"d":"90s"}`},
		{Never, `{"d":"-1"}`},
		{0, `{"d":""}`},
	} {
		b, err := json.Marshal(wrap{tc.d})
		if err != nil || string(b) != tc.json {
			t.Fatalf("marshal %d = %s, %v; want %s", tc.d, b, err, tc.json)
		}
		var back wrap
		if err := json.Unmarshal(b, &back); err != nil || back.D != tc.d {
			t.Fatalf("unmarshal %s = %d, %v", b, back.D, err)
		}
	}
	var w wrap
	if err := json.Unmarshal([]byte(`{"d":"1h30m"}`), &w); err != nil || time.Duration(w.D) != 90*time.Minute {
		t.Fatalf("1h30m = %d, %v", w.D, err)
	}
	for _, bad := range []string{`{"d":"soon"}`, `{"d":5}`} {
		if err := json.Unmarshal([]byte(bad), &w); err == nil {
			t.Fatalf("%s must fail", bad)
		}
	}
}

func TestRepositoryJSONRoundTrip(t *testing.T) {
	yes := true
	r := Repository{Name: "p", Format: "maven", Type: "proxy", Remote: "https://x.example/",
		RemoteAuth:     &RemoteAuth{Username: "u", Password: "pw", BearerToken: "tok"},
		MetadataMaxAge: Duration(time.Hour), ContentMaxAge: Never, NegativeCacheTTL: Duration(time.Minute),
		AnonymousRead: &yes}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"contentMaxAge":"-1"`) || !strings.Contains(string(b), `"name":"p"`) {
		t.Fatalf("json = %s", b)
	}
	var back Repository
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, r) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", back, r)
	}
}

func TestValidateRepositoriesAcceptsNameCreate(t *testing.T) {
	// "create" is only refused by the UI's create endpoint; a stored one must not block startup.
	if err := ValidateRepositories([]Repository{{Name: "create", Format: "maven", Type: "hosted"}}); err != nil {
		t.Fatal(err)
	}
}

// QA N-29: the key file for stored secrets is a regular, always-read key.
func TestSecretKeyFile(t *testing.T) {
	if c, err := Parse([]byte("secretKeyFile: /run/secrets/key\n")); err != nil || c.SecretKeyFile != "/run/secrets/key" {
		t.Fatalf("secretKeyFile = %q, %v", c.SecretKeyFile, err)
	}
}

func TestDatabase(t *testing.T) {
	c, err := Parse([]byte("database:\n  url: postgres://cc@db:5432/cc\n  maxConns: 20\n"))
	if err != nil || c.Database.URL != "postgres://cc@db:5432/cc" || c.Database.MaxConns != 20 {
		t.Fatalf("database = %+v, %v", c.Database, err)
	}
	if c, err := Parse([]byte("{}")); err != nil || c.Database != (Database{}) {
		t.Fatalf("default database = %+v, %v; want SQLite (no URL)", c.Database, err)
	}
	for _, bad := range []string{"-1", "1001"} {
		if _, err := Parse([]byte("database: {maxConns: " + bad + "}\n")); err == nil || !strings.Contains(err.Error(), "database.maxConns") {
			t.Fatalf("maxOpenConns %s: %v", bad, err)
		}
	}
}

func TestMetricsFlushSeconds(t *testing.T) {
	c, err := Parse([]byte("{}"))
	if err != nil || c.MetricsFlushSeconds != 30 {
		t.Fatalf("default = %d err=%v", c.MetricsFlushSeconds, err)
	}
	if c, err = Parse([]byte("metricsFlushSeconds: 5\n")); err != nil || c.MetricsFlushSeconds != 5 {
		t.Fatalf("5: %d %v", c.MetricsFlushSeconds, err)
	}
	for _, bad := range []string{"-1", "3601"} {
		if _, err := Parse([]byte("metricsFlushSeconds: " + bad + "\n")); err == nil {
			t.Fatalf("%s must fail", bad)
		}
	}
}

func TestPresentKeys(t *testing.T) {
	c, err := Parse([]byte("listen: ':9000'\nanonymousRead: false\nrepositories: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Present["listen"] || !c.Present["anonymousRead"] || !c.Present["repositories"] || c.Present["baseURL"] || len(c.Present) != 3 {
		t.Fatalf("Present = %v", c.Present)
	}
}

// QA L-9 + final review C1: duplicate group members and negative durations
// are rejected for a definition being created, updated or imported
// (ValidateDefinition), but stored rows decode and load (NormalizeStored).
func TestValidateDefinitionRejectsDuplicateMemberAndNegative(t *testing.T) {
	err := ValidateDefinition(Repository{Name: "g", Format: "maven", Type: "group", Members: []string{"h", "h"}})
	if err == nil || !strings.Contains(err.Error(), `duplicate member "h"`) {
		t.Fatalf("err = %v", err)
	}
	err = ValidateDefinition(Repository{Name: "p", NegativeCacheTTL: Duration(-5 * time.Minute)})
	if err == nil || !strings.Contains(err.Error(), "negativeCacheTTL") {
		t.Fatalf("err = %v", err)
	}
	if err := ValidateDefinition(Repository{Name: "p", ContentMaxAge: Never}); err != nil {
		t.Fatalf("-1: %v", err)
	}
	// The whole-list check does not hold other rows to these rules.
	if err := ValidateRepositories([]Repository{
		{Name: "h", Format: "maven", Type: "hosted", NegativeCacheTTL: Duration(-time.Second)},
		{Name: "g", Format: "maven", Type: "group", Members: []string{"h", "h"}},
	}); err != nil {
		t.Fatalf("ValidateRepositories: %v", err)
	}
}

func TestImportSectionRejectsDuplicateMemberAndNegative(t *testing.T) {
	c, err := Parse([]byte("repositories:\n" +
		"  - { name: h, format: maven, type: hosted, negativeCacheTTL: -5m }\n" +
		"  - { name: g, format: maven, type: group, members: [h, h] }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.RepositoriesErr == nil || !strings.Contains(c.RepositoriesErr.Error(), "negativeCacheTTL") ||
		!strings.Contains(c.RepositoriesErr.Error(), `duplicate member "h"`) {
		t.Fatalf("RepositoriesErr = %v", c.RepositoriesErr)
	}
}

func TestStoredLegacyDefinitionDecodesAndNormalizes(t *testing.T) {
	var r Repository
	if err := json.Unmarshal([]byte(`{"name":"p","negativeCacheTTL":"-5m","metadataMaxAge":"-1","members":["a","b","a"]}`), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.NegativeCacheTTL != Duration(-5*time.Minute) {
		t.Fatalf("decoded %v", r.NegativeCacheTTL)
	}
	notes := NormalizeStored(&r)
	if len(notes) != 2 || r.NegativeCacheTTL != 0 || r.MetadataMaxAge != Never || !slices.Equal(r.Members, []string{"a", "b"}) {
		t.Fatalf("notes=%v r=%+v", notes, r)
	}
	if notes := NormalizeStored(&r); notes != nil {
		t.Fatalf("second pass: %v", notes)
	}
}

// Remote fetches refuse a host that is not ASCII (net/http would map
// fullwidth digits and dots to an address the egress checks never saw), so a
// new definition must use the punycode form; a stored row still loads.
func TestValidateDefinitionRejectsNonASCIIRemoteHost(t *testing.T) {
	for remote, ok := range map[string]bool{
		"https://bücher.example/":             false,
		"https://１０．１/":                       false,
		"https://%EF%BC%91%EF%BC%90.example/": false, // the host is decoded
		"https://xn--bcher-kva.example/":      true,
		"https://repo.example/bücher/":        true, // only the host counts
	} {
		r := Repository{Name: "p", Format: "maven", Type: "proxy", Remote: remote}
		if err := ValidateDefinition(r); ok && err != nil || !ok && (err == nil || !strings.Contains(err.Error(), "punycode")) {
			t.Errorf("%s: err = %v", remote, err)
		}
		if err := ValidateRepositories([]Repository{r}); err != nil {
			t.Errorf("%s: stored row refused: %v", remote, err)
		}
	}
}

// QA L-9: a remote belongs to a proxy and a versionPolicy never to a group.
// New definitions are refused; stored ones load with the fields dropped.
func TestValidateDefinitionTypeFields(t *testing.T) {
	for _, c := range []struct {
		r    Repository
		want string
	}{
		{Repository{Name: "h", Format: "maven", Type: "hosted", Remote: "https://repo1.example.org/"}, "only valid for proxy"},
		{Repository{Name: "h", Format: "npm", Type: "hosted", RemoteAuth: &RemoteAuth{Username: "u"}}, "only valid for proxy"},
		{Repository{Name: "g", Format: "maven", Type: "group", Members: []string{"h"}, Remote: "https://x.example/"}, "only valid for proxy"},
		{Repository{Name: "g", Format: "maven", Type: "group", Members: []string{"h"}, VersionPolicy: "release"}, "versionPolicy is not valid for group"},
		{Repository{Name: "p", Format: "maven", Type: "proxy", Remote: "https://x.example/", VersionPolicy: "release"}, ""},
		{Repository{Name: "h", Format: "maven", Type: "hosted", VersionPolicy: "snapshot"}, ""},
	} {
		err := ValidateDefinition(c.r)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%+v: err = %v, want %q", c.r, err, c.want)
		}
	}
	r := Repository{Name: "g", Format: "maven", Type: "group", Members: []string{"h"}, VersionPolicy: "mixed",
		Remote: "https://x.example/", RemoteAuth: &RemoteAuth{Username: "u"}}
	if notes := NormalizeStored(&r); len(notes) != 2 || r.Remote != "" || r.RemoteAuth != nil || r.VersionPolicy != "" {
		t.Fatalf("notes=%v r=%+v", notes, r)
	}
	if err := ValidateDefinition(r); err != nil {
		t.Fatalf("normalized row: %v", err)
	}
	p := Repository{Name: "p", Format: "maven", Type: "proxy", Remote: "https://x.example/", VersionPolicy: "release"}
	if notes := NormalizeStored(&p); notes != nil || p.Remote == "" || p.VersionPolicy != "release" {
		t.Fatalf("proxy changed: %v %+v", notes, p)
	}
}

func TestBackupDir(t *testing.T) {
	c, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if c.BackupDir != "/backup" {
		t.Fatalf("default backupDir %q", c.BackupDir)
	}
	c, err = Parse([]byte(validYAML + "backupDir: /srv/backups/\n"))
	if err != nil || c.BackupDir != "/srv/backups" {
		t.Fatalf("backupDir %q %v", c.BackupDir, err)
	}
	if _, err := Parse([]byte(validYAML + "dataDir: /data\nbackupDir: /data\n")); err == nil || !strings.Contains(err.Error(), "backupDir") {
		t.Fatalf("backupDir equal to dataDir: %v", err)
	}
}

func TestBackupDirNotNestedWithDataDir(t *testing.T) {
	for _, bad := range []string{"dataDir: /data\nbackupDir: /data/backup\n", "dataDir: /srv/cc/data\nbackupDir: /srv/cc\n"} {
		if _, err := Parse([]byte(validYAML + bad)); err == nil || !strings.Contains(err.Error(), "backupDir") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := Parse([]byte(validYAML + "dataDir: /data\nbackupDir: /data-backup\n")); err != nil {
		t.Fatalf("a sibling with a common prefix: %v", err)
	}
}
