package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/nexusapi"
	"commandcenter/internal/pgp"
	"commandcenter/internal/ui"
)

// import-nexus -config: the configuration of the Nexus server, read through
// its REST API as an administrator and made on Command Center in dependency
// order. Each object is written through Command Center's Nexus API
// (/service/rest/v1), which takes Nexus's bodies and validates them as the
// web UI does; the LDAP server and the outbound HTTP settings, which that
// API does not write, go to /-/api. What Nexus does not hand out (passwords,
// tokens, keys) is never made up: the object is made without it where that
// leaves nothing open, and a "needs manual action" line says what to enter.
//
// Where Nexus has no REST resource the web UI's own endpoints are read:
// cleanup policies at /service/rest/internal/cleanup-policies, and the
// tasks' schedules and the HTTP settings through /service/extdirect.

// obj is a JSON object as the APIs answer it.
type obj = map[string]any

func str(o obj, k string) string { s, _ := o[k].(string); return s }
func sub(o obj, k string) obj    { m, _ := o[k].(obj); return m }

// list is the strings of the array o[k].
func list(o obj, k string) []string {
	var out []string
	l, _ := o[k].([]any)
	for _, v := range l {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// pick is o with only the named fields: the ones Command Center stores, so
// that a later run compares equal.
func pick(o obj, keys ...string) obj {
	out := obj{}
	for _, k := range keys {
		if v, ok := o[k]; ok {
			out[k] = v
		}
	}
	return out
}

// request is one write to Command Center.
type request struct {
	method, path string
	body         any
}

// item is one object of the migration: what the target should hold (want),
// what it holds (cur; nil for none), and the requests that make and change it.
type item struct {
	kind, name     string
	want, cur      obj
	create, update *request // update nil: the object cannot be changed in place
	// unset marks a setting of the target that nobody configured (cur is
	// its default): it is set like a new object, without -update.
	unset  bool
	skip   string   // why the object is not migrated
	notes  []string // what is not carried over as it was
	manual []string // what the administrator has to enter once the target has the object
}

func (it *item) note(format string, a ...any) { it.notes = append(it.notes, fmt.Sprintf(format, a...)) }
func (it *item) todo(format string, a ...any) {
	it.manual = append(it.manual, fmt.Sprintf(format, a...))
}

// migration is one run of the configuration migration.
type migration struct {
	im     *importer
	ctx    context.Context
	update bool
	pw     *os.File // receives the generated passwords; nil: nobody learns them
	// have is what the target holds when this run is done (in a dry run:
	// would hold), by kind and name: what later objects may refer to.
	have       map[string]map[string]bool
	repoFormat map[string]string // of the repositories in have, in Nexus's spelling
	formats    map[string]bool   // the formats Command Center has, in Nexus's spelling
	roles      []obj             // the source's roles and local users, for anonymous access
	users      []obj
	// fresh says the target had no local user besides the one running the
	// migration when it started: nobody whose reads a change of
	// authenticatedRead could take away.
	fresh     bool
	ldapReady bool // the target has an LDAP server to turn the realm on for
	// seals are the hosted repositories the target has read-only as Nexus
	// has them, by name; sourceOffline the hosted ones Nexus serves no file of.
	seals         map[string]*seal
	sourceOffline map[string]bool
	dockerRealm   *bool // dockerRealmOn, once it was asked
	dockerOff     int   // docker repositories without anonymous pull, as in Nexus
	manual        []string
	n             struct{ created, updated, unchanged, differ, skipped, failed int }
}

// seal is how a read-only repository is closed again after its copy opened
// it (importer.copySealed).
type seal struct {
	update *request // the repository as Nexus has it
	open   bool     // an earlier run left it open
}

// openMarker is the tag on the target that says a run opened the repository
// for its copy: there before it opens, gone once it is closed, whatever
// happens to the run in between. A repository has no field of its own for it.
func openMarker(name string) string { return nx + "/tags/import-nexus-open." + name }

// Nexus's API prefix on both servers, and Command Center's own.
const (
	nx     = "/service/rest/v1"
	native = "/-/api"
)

// migrateConfig runs the migration and prints a line per object. The error
// is one that stops it before anything is read; finish prints the rest.
func (im *importer) migrateConfig(ctx context.Context, update bool, passwordsOut string) (*migration, error) {
	m := &migration{im: im, ctx: ctx, update: update, have: map[string]map[string]bool{}, repoFormat: map[string]string{}, formats: map[string]bool{},
		seals: map[string]*seal{}, sourceOffline: map[string]bool{}}
	for _, f := range ui.AccessFormats() {
		m.formats[auth.NexusFormat(f)] = true
	}
	var me struct {
		Admin bool `json:"admin"`
	}
	if err := m.dst(http.MethodGet, native+"/me", nil, &me); err != nil {
		return nil, fmt.Errorf("sign in to Command Center: %w (check -to-user and its password)", err)
	}
	if !me.Admin {
		return nil, errors.New("-config needs an administrator of Command Center as -to-user")
	}
	if passwordsOut != "" && !im.dryRun {
		f, err := os.OpenFile(passwordsOut, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err == nil {
			err = f.Chmod(0o600) // a file that was there before, too
		}
		if err != nil {
			return nil, fmt.Errorf("-passwords-out: %w", err)
		}
		m.pw = f
	}
	for _, step := range []func(){m.blobStores, m.cleanupPolicies, m.routingRules, m.selectors, m.repositories, m.privileges,
		m.rolesOf, m.usersOf, m.anonymous, m.ldap, m.realms, m.email, m.tasks, m.httpSettings} {
		step()
	}
	return m, nil
}

// finish prints the manual actions and the summary, and returns how many
// objects failed.
func (m *migration) finish() int {
	for _, name := range sortedKeys(m.seals) { // what no copy reached: -config-only, another -repo
		if s := m.seals[name]; s.open && !m.im.dryRun {
			m.close(name, s)
		}
	}
	if m.pw != nil {
		if err := m.pw.Close(); err != nil {
			m.n.failed++
			m.im.printf("config\tuser\t*\terror: %v\n", err)
		}
	}
	for _, a := range m.manual {
		m.im.printf("needs manual action: %s\n", a)
	}
	if m.dockerOff > 0 {
		verb := "is"
		if m.im.dryRun {
			verb = "would be"
		}
		m.im.printf("docker: anonymous pull %s off on %d repositories, because Nexus does not allow it on them\n", verb, m.dockerOff)
	}
	format := "configuration: %d created, %d updated, %d unchanged, %d differ, %d skipped, %d failed, %d need manual action\n"
	if m.im.dryRun {
		format = "configuration (dry run, nothing was written): %d to create, %d to update, %d unchanged, %d differ, %d skipped, %d failed, %d need manual action\n"
	}
	m.im.printf(format, m.n.created, m.n.updated, m.n.unchanged, m.n.differ, m.n.skipped, m.n.failed, len(m.manual))
	return m.n.failed
}

// todo records what the administrator has to do by hand, once.
func (m *migration) todo(format string, a ...any) {
	if s := fmt.Sprintf(format, a...); !slices.Contains(m.manual, s) {
		m.manual = append(m.manual, s)
	}
}

func (m *migration) line(kind, name, status string, notes []string) {
	if len(notes) > 0 {
		status += "\t" + strings.Join(notes, "; ")
	}
	m.im.printf("config\t%s\t%s\t%s\n", kind, name, status)
}

// ---- HTTP ----

var uiHeader = http.Header{auth.UIHeader: {"1"}} // /-/api takes changes with it only

func (m *migration) src(method, p string, body, out any) error {
	return m.im.call(m.ctx, m.im.src, m.im.fromAuth, method, m.im.from.String()+p, nil, body, out)
}

func (m *migration) dst(method, p string, body, out any) error {
	return m.im.call(m.ctx, m.im.dst, m.im.toAuth, method, m.im.to.String()+p, uiHeader, body, out)
}

// call makes a JSON request and decodes a 2xx answer into out (nil: none
// is expected). The error of another status carries the server's message.
func (im *importer) call(ctx context.Context, c *http.Client, authz, method, u string, hdr http.Header, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return apiError(resp)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}

// apiError is the error of a refused request with the message of the body:
// {"message"} and [{"id","message"}] of the Nexus API, {"error","errors"} of
// /-/api, anything else as it is.
func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	var one struct {
		Message string   `json:"message"`
		Error   string   `json:"error"`
		Errors  []string `json:"errors"`
	}
	var many []struct {
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(b, &many) == nil {
		for _, x := range many {
			one.Errors = append(one.Errors, x.Message)
		}
	} else if json.Unmarshal(b, &one) != nil {
		msg = strings.Join(strings.Fields(string(b)), " ")
	}
	if msg = cmp.Or(strings.Join(one.Errors, "; "), one.Message, one.Error, msg); len(msg) > 400 {
		msg = msg[:400] + "..."
	}
	return &statusError{resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, msg}
}

// direct calls a method of the API Nexus's own web UI uses (ExtDirect, a
// JSON-RPC at /service/extdirect) and decodes its data into out.
func (m *migration) direct(action, method string, out any) error {
	var resp struct {
		Type    string `json:"type"` // "exception" for a call that threw
		Message string `json:"message"`
		Result  struct {
			Success bool            `json:"success"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"result"`
	}
	const p = "/service/extdirect"
	if err := m.src(http.MethodPost, p, obj{"action": action, "method": method, "data": nil, "type": "rpc", "tid": 1}, &resp); err != nil {
		return err
	}
	if !resp.Result.Success {
		return fmt.Errorf("POST %s %s.%s: %s", p, action, method, cmp.Or(resp.Result.Message, resp.Message, "no result"))
	}
	return json.Unmarshal(resp.Result.Data, out)
}

// readable reports whether a kind's objects could be read from both
// servers and prints the kind's line when they could not: a source without
// that API (404) is skipped, any other error is a failure.
func (m *migration) readable(kind string, srcErr, dstErr error) bool {
	var se *statusError
	switch {
	case srcErr == nil && dstErr == nil:
		return true
	case errors.As(srcErr, &se) && se.status == http.StatusNotFound:
		m.n.skipped++
		m.line(kind, "*", "skipped (the source does not serve "+se.path+")", nil)
	default:
		m.n.failed++
		m.line(kind, "*", "error: "+cmp.Or(srcErr, dstErr).Error(), nil)
	}
	return false
}

// lists reads a kind's objects: the source's in the order of their key
// field, the target's by it.
func (m *migration) lists(kind, srcPath, dstPath, key string) ([]obj, map[string]obj, bool) {
	var src, dst []obj
	if !m.readable(kind, m.src(http.MethodGet, srcPath, nil, &src), m.dst(http.MethodGet, dstPath, nil, &dst)) {
		return nil, nil, false
	}
	sort.SliceStable(src, func(i, j int) bool { return str(src[i], key) < str(src[j], key) })
	cur := map[string]obj{}
	for _, o := range dst {
		cur[str(o, key)] = o
	}
	m.have[kind] = map[string]bool{}
	return src, cur, true
}

// ---- the plan of one object ----

// apply brings one object of the target in line and prints its line. It
// reports whether the target holds the object afterwards (in a dry run:
// would hold it), and what this run did to it: "created", "updated" or "".
func (m *migration) apply(it item) (have bool, did string) {
	status, notes := "", it.notes
	switch diff := differs(it.want, it.cur); {
	case it.skip != "":
		m.n.skipped++
		status = "skipped (" + it.skip + ")"
	case it.cur == nil:
		if status, have = m.write(cmp.Or(it.create, it.update), "create", &m.n.created); have { // a setting has the one request
			did = "created"
		}
	case len(diff) == 0:
		m.n.unchanged++
		status, have, notes = "unchanged", true, nil
	case it.update != nil && (m.update || it.unset):
		var ok bool
		if status, ok = m.write(it.update, "update", &m.n.updated); ok {
			did = "updated"
		}
		have = true
	default:
		m.n.differ++
		status, have = "differs: "+strings.Join(diff, ", ")+" (left as it is", true
		if it.update != nil {
			status += "; -update changes it"
		}
		status += ")"
	}
	if have {
		for _, s := range it.manual {
			m.todo("%s", s)
		}
	}
	m.line(it.kind, it.name, status, notes)
	return have, did
}

// write sends r, or in a dry run says that it would.
func (m *migration) write(r *request, verb string, count *int) (string, bool) {
	switch {
	case r == nil:
		m.n.failed++
		return "error: Command Center has no such object to " + verb, false
	case m.im.dryRun:
		*count++
		return "would " + verb, true
	}
	if err := m.dst(r.method, r.path, r.body, nil); err != nil {
		m.n.failed++
		return "error: " + err.Error(), false
	}
	*count++
	return verb + "d", true
}

// ordered are the list fields whose order means something; the others
// compare as sets (Nexus answers its sets in no order).
var ordered = map[string]bool{"memberNames": true, "members": true, "active": true}

// differs lists the fields of want that cur holds another value for, as
// dotted paths (never the values: none is a secret, but none needs
// printing either). A field that is empty on both sides (absent, null, "",
// false, 0, an empty list or object) is the same.
func differs(want, cur obj) []string {
	var out []string
	var walk func(path string, w, c any)
	walk = func(path string, w, c any) {
		if isEmpty(w) && isEmpty(c) {
			return
		}
		if wm, ok := w.(obj); ok {
			cm, _ := c.(obj)
			for _, k := range sortedKeys(wm) {
				walk(strings.TrimPrefix(path+"."+k, "."), wm[k], cm[k])
			}
			return
		}
		if inOrder := ordered[path[strings.LastIndexByte(path, '.')+1:]]; canon(w, inOrder) != canon(c, inOrder) {
			out = append(out, path)
		}
	}
	walk("", want, cur)
	return out
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case float64:
		return x == 0
	case int:
		return x == 0
	case []any:
		return len(x) == 0
	case []string:
		return len(x) == 0
	case obj:
		for _, e := range x {
			if !isEmpty(e) {
				return false
			}
		}
		return true
	}
	return false
}

// canon is v as comparable text: its JSON, a list's elements sorted unless
// the list is in order.
func canon(v any, inOrder bool) string {
	b, _ := json.Marshal(v)
	var l []json.RawMessage
	if inOrder || json.Unmarshal(b, &l) != nil {
		return string(b)
	}
	parts := make([]string, len(l))
	for i, e := range l {
		parts[i] = string(e)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// dependFirst orders objs so that each comes after the ones deps names
// (a group after its member groups, a role after the roles it contains).
func dependFirst(objs []obj, key string, deps func(obj) []string) []obj {
	byKey, seen := map[string]obj{}, map[string]bool{}
	for _, o := range objs {
		byKey[str(o, key)] = o
	}
	out := make([]obj, 0, len(objs))
	var visit func(o obj)
	visit = func(o obj) {
		if seen[str(o, key)] {
			return
		}
		seen[str(o, key)] = true
		for _, d := range deps(o) {
			if x, ok := byKey[d]; ok {
				visit(x)
			}
		}
		out = append(out, o)
	}
	for _, o := range objs {
		visit(o)
	}
	return out
}

// ---- blob stores ----

// blobStores maps the stores by name. A file store is made under Command
// Center's data directory, whatever its path on the Nexus host was (make
// the store first to put it elsewhere), and its soft quota follows Nexus's.
// An S3 store needs its secret key, which Nexus does not hand out: one with
// an access key is left to the administrator, one without (the server's
// own role) is made on the same bucket. Groups follow their members.
func (m *migration) blobStores() {
	const kind = "blobstore"
	src, cur, ok := m.lists(kind, nx+"/blobstores", nx+"/blobstores", "name")
	if !ok {
		return
	}
	sort.SliceStable(src, func(i, j int) bool { return str(src[i], "type") != "Group" && str(src[j], "type") == "Group" })
	for _, s := range src {
		name, typ := str(s, "name"), str(s, "type")
		it := item{kind: kind, name: name, cur: cur[name], want: obj{"type": typ, "softQuota": s["softQuota"]}}
		one := nx + "/blobstores/" + strings.ToLower(typ) + "/" + url.PathEscape(name)
		switch {
		case it.cur != nil && (str(it.cur, "type") != typ || typ != "File" && typ != "Group"):
			// There under another type, which cannot change, or a bucket
			// the administrator set up: its settings are theirs.
			it.want = obj{"type": typ}
		case typ == "File":
			it.create = &request{http.MethodPost, nx + "/blobstores/file", obj{"name": name, "path": name, "softQuota": s["softQuota"]}}
			it.update = &request{http.MethodPut, one, it.want}
		case typ == "Group":
			var g obj
			err := m.src(http.MethodGet, one, nil, &g)
			if err == nil && it.cur != nil {
				err = m.dst(http.MethodGet, one, nil, &it.cur)
			}
			if err != nil {
				it.skip = "cannot be read: " + err.Error()
				break
			}
			for _, member := range list(g, "members") {
				if !m.have[kind][member] {
					it.skip = "its member " + member + " is missing on the target"
				}
			}
			it.want = obj{"name": name, "members": g["members"], "fillPolicy": g["fillPolicy"], "softQuota": g["softQuota"]}
			it.create, it.update = &request{http.MethodPost, nx + "/blobstores/group", it.want}, &request{http.MethodPut, one, it.want}
		case typ == "S3":
			var c obj
			if err := m.src(http.MethodGet, one, nil, &c); err != nil {
				it.skip = "cannot be read: " + err.Error()
				break
			}
			bc := sub(c, "bucketConfiguration")
			b := sub(bc, "bucket")
			if str(sub(bc, "bucketSecurity"), "accessKeyId") != "" {
				it.skip = "its secret access key is not readable from Nexus"
				m.todo("blobstore %s: make this S3 blob store on Command Center (bucket %s, region %s, prefix %q) with its access key, then run this again: "+
					"the repositories that use it are not made before", name, str(b, "name"), str(b, "region"), str(b, "prefix"))
				break
			}
			it.create = &request{http.MethodPost, nx + "/blobstores/s3", obj{"name": name, "softQuota": s["softQuota"], "bucketConfiguration": obj{
				"bucket": pick(b, "region", "name", "prefix"), "encryption": bc["encryption"], "failoverBuckets": bc["failoverBuckets"],
				"advancedBucketConnection": pick(sub(bc, "advancedBucketConnection"), "endpoint", "forcePathStyle", "maxConnectionPoolSize")}}}
		default:
			it.skip = "the credentials of a " + typ + " blob store are not readable from Nexus"
			m.todo("blobstore %s: make this %s blob store on Command Center, then run this again: the repositories that use it are not made before", name, typ)
		}
		have, _ := m.apply(it)
		m.have[kind][name] = have || cur[name] != nil
	}
}

// ---- cleanup policies, routing rules, content selectors ----

// named migrates a kind whose objects both servers list at a path and name
// in a field: body is the object as Command Center takes it (nil: skipped
// for the reason).
func (m *migration) named(kind, srcPath, dstPath, key string, body func(o obj, it *item) obj) {
	src, cur, ok := m.lists(kind, srcPath, dstPath, key)
	if !ok {
		return
	}
	for _, o := range src {
		name := str(o, key)
		it := item{kind: kind, name: name, cur: cur[name]}
		if it.want = body(o, &it); it.want != nil {
			it.create = &request{http.MethodPost, dstPath, it.want}
			it.update = &request{http.MethodPut, dstPath + "/" + url.PathEscape(name), it.want}
		}
		if have, _ := m.apply(it); have {
			m.have[kind][name] = true
		}
	}
}

func (m *migration) cleanupPolicies() {
	const kind = "cleanup-policy"
	// Nexus's public source has the web UI's resource only; /v1 is Nexus
	// Pro's, and what a Command Center source serves.
	srcPath := "/service/rest/internal/cleanup-policies"
	var se *statusError
	if err := m.src(http.MethodGet, srcPath, nil, new([]obj)); errors.As(err, &se) && se.status == http.StatusNotFound {
		srcPath = nx + "/cleanup-policies"
	}
	m.named(kind, srcPath, nx+"/cleanup-policies", "name", func(o obj, it *item) obj {
		if f := str(o, "format"); f != "*" && !m.formats[f] {
			it.skip = "format " + f + " is not supported"
			return nil
		}
		return pick(o, "name", "notes", "format", "criteriaLastBlobUpdated", "criteriaLastDownloaded", "criteriaReleaseType",
			"criteriaAssetRegex", "retain", "sortBy")
	})
}

func (m *migration) routingRules() {
	m.named("routing-rule", nx+"/routing-rules", nx+"/routing-rules", "name", func(o obj, _ *item) obj {
		return pick(o, "name", "description", "mode", "matchers")
	})
}

func (m *migration) selectors() {
	const p = nx + "/security/content-selectors"
	m.named("content-selector", p, p, "name", func(o obj, it *item) obj {
		if t := str(o, "type"); t != "" && t != "csel" {
			it.skip = "a " + t + " selector: Command Center evaluates CSEL only"
			return nil
		}
		if strings.Contains(str(o, "expression"), "/v2/") {
			it.note("it names docker paths under /v2/: Command Center's docker path is the image name, so rewrite the expression")
		}
		return pick(o, "name", "description", "expression")
	})
}

// ---- repositories ----

func (m *migration) repositories() {
	const kind = "repository"
	src, cur, ok := m.lists(kind, nx+"/repositorySettings", nx+"/repositorySettings", "name")
	if !ok {
		return
	}
	rank := map[string]int{"hosted": 0, "proxy": 1, "group": 2}
	sort.SliceStable(src, func(i, j int) bool { return rank[str(src[i], "type")] < rank[str(src[j], "type")] })
	members := func(r obj) []string { return list(sub(r, "group"), "memberNames") }
	for _, r := range dependFirst(src, "name", members) {
		it := m.repository(r, cur[str(r, "name")])
		hosted := str(r, "type") == "hosted" && it.want != nil
		if hosted && r["online"] == false {
			m.sourceOffline[it.name] = true
		}
		// A read-only repository the target has open, with a marker: a run
		// that was killed left it so, and this one closes it. The target
		// counts as what this run leaves.
		sealed := hosted && readOnly(it.want)
		interrupted := false
		if sealed && it.cur != nil && m.dst(http.MethodGet, openMarker(it.name), nil, nil) == nil {
			if interrupted = !readOnly(it.cur); interrupted {
				sub(it.cur, "storage")["writePolicy"] = sub(it.want, "storage")["writePolicy"]
			} else if !m.im.dryRun {
				m.dst(http.MethodDelete, openMarker(it.name), nil, nil) // closed, and the marker outlived that
			}
		}
		have, did := m.apply(it)
		if !have {
			continue
		}
		m.have[kind][it.name], m.repoFormat[it.name] = true, str(r, "format")
		if str(r, "format") == "docker" {
			m.countDockerOff(it, did)
		}
		switch {
		case !sealed:
		case interrupted && did == "":
			m.seals[it.name] = &seal{update: it.update, open: true}
			m.line(kind, it.name, "an earlier run left it open: it is closed after its copy", nil)
		case it.cur == nil || did != "" || readOnly(it.cur):
			m.seals[it.name] = &seal{update: it.update}
		default:
			m.todo("repository %s: it takes uploads here and is read-only on Nexus: set its write policy to DENY, or run this with -update, if that is not your choice", it.name)
		}
	}
}

// mergeFormats are the formats whose groups merge in Nexus and take
// mergeMembers here.
var mergeFormats = map[string]bool{"npm": true, "pypi": true, "r": true, "conda": true, "yum": true}

// merges reports whether the group the target has already (cur) merges.
func (m *migration) merges(name string, cur obj) bool {
	var cfg struct {
		MergeMembers bool `json:"mergeMembers"`
	}
	return cur != nil && m.dst(http.MethodGet, native+"/repositories/"+url.PathEscape(name)+"/config", nil, &cfg) == nil && cfg.MergeMembers
}

// readOnly reports whether a repository's settings refuse every upload.
func readOnly(r obj) bool { return strings.EqualFold(str(sub(r, "storage"), "writePolicy"), "DENY") }

// always makes a request the end of a run needs, interrupted or not.
func (m *migration) always(method, p string, body any) error {
	return m.im.call(context.WithoutCancel(m.ctx), m.im.dst, m.im.toAuth, method, m.im.to.String()+p, uiHeader, body, nil)
}

// open lets a sealed repository take uploads for its copy, its marker first.
func (m *migration) open(name string, s *seal) error {
	if m.dst(http.MethodGet, openMarker(name), nil, nil) != nil {
		attrs := obj{"repository": name, "note": "import-nexus opened this repository for its copy; the next run closes it if this one could not"}
		if err := m.dst(http.MethodPost, nx+"/tags", obj{"name": path.Base(openMarker(name)), "attributes": attrs}, nil); err != nil {
			return err
		}
	}
	m.line("repository", name, "opening for its copy: writePolicy ALLOW_ONCE until it is closed again, read-only as Nexus has it", nil)
	body := obj{}
	for k, v := range s.update.body.(obj) {
		body[k] = v
	}
	storage := pick(sub(body, "storage"), "blobStoreName", "strictContentTypeValidation")
	storage["writePolicy"] = "ALLOW_ONCE"
	body["storage"] = storage
	if err := m.dst(s.update.method, s.update.path, body, nil); err != nil {
		m.always(http.MethodDelete, openMarker(name), nil)
		return err
	}
	return nil
}

// close puts a sealed repository back as Nexus has it. What it cannot close
// keeps its marker and is reported, and the next run closes it.
func (m *migration) close(name string, s *seal) {
	err := m.always(s.update.method, s.update.path, s.update.body)
	for try := 0; err != nil && try < 2; try++ {
		time.Sleep(2 * time.Second)
		err = m.always(s.update.method, s.update.path, s.update.body)
	}
	if err != nil {
		m.n.failed++
		m.line("repository", name, "error: it STILL TAKES UPLOADS, not closed: "+err.Error(), nil)
		m.todo("repository %s: it still takes uploads and is read-only on Nexus: run this again, or set its write policy to DENY", name)
		return
	}
	s.open = false
	m.always(http.MethodDelete, openMarker(name), nil) // what stays is removed by the next run
	m.line("repository", name, "closed: read-only, as Nexus has it", nil)
}

// dockerRealmOn reports whether Nexus's Docker Bearer Token realm is active:
// without it anonymous pull does not work, whatever forceBasicAuth says.
// What cannot be read counts as no: a move does not widen access.
func (m *migration) dockerRealmOn() bool {
	if m.dockerRealm == nil {
		var realms []string
		on := m.src(http.MethodGet, nx+"/security/realms/active", nil, &realms) == nil && slices.Contains(realms, "DockerToken")
		m.dockerRealm = &on
	}
	return *m.dockerRealm
}

// countDockerOff counts a docker repository whose anonymous pull is off now,
// or says that it is not though Nexus has it off.
func (m *migration) countDockerOff(it item, did string) {
	if sub(it.want, "docker")["forceBasicAuth"] != true {
		return
	}
	if it.cur == nil || did == "updated" || sub(it.cur, "docker")["forceBasicAuth"] == true {
		m.dockerOff++
		return
	}
	m.todo("repository %s: it allows anonymous pull here and Nexus does not: run this with -update, or turn its anonymous read off", it.name)
}

// repository is the plan of one repository: the settings Command Center
// keeps, from Nexus's settings r. What it has no equivalent for becomes a
// note, a credential Nexus does not hand out a manual action.
func (m *migration) repository(r, cur obj) item {
	name, format, typ := str(r, "name"), str(r, "format"), str(r, "type")
	it := item{kind: "repository", name: name, cur: cur}
	if !m.formats[format] {
		it.skip = "format " + format + " is not supported"
		return it
	}
	want := obj{"name": name, "format": format, "type": typ, "online": r["online"]}
	st := sub(r, "storage")
	if typ != "group" { // a Command Center group has no blob store and no cleanup
		store := str(st, "blobStoreName")
		if !m.have["blobstore"][store] {
			it.skip = "its blob store " + store + " is missing on the target"
			return it
		}
		storage := obj{"blobStoreName": store, "strictContentTypeValidation": st["strictContentTypeValidation"]}
		if typ == "hosted" {
			storage["writePolicy"] = st["writePolicy"]
		}
		policies := []string{}
		for _, p := range list(sub(r, "cleanup"), "policyNames") {
			if m.have["cleanup-policy"][p] {
				policies = append(policies, p)
			} else {
				it.note("without the cleanup policy %s, which is not on the target", p)
			}
		}
		want["storage"], want["cleanup"] = storage, obj{"policyNames": policies}
	}
	create := obj{} // what only a new repository is given: a signing key
	switch typ {
	case "proxy":
		remote := pick(sub(r, "proxy"), "remoteUrl", "contentMaxAge", "metadataMaxAge")
		// The same remote, but for the "/" its path ends with there.
		if cp := sub(cur, "proxy"); str(cp, "remoteUrl") == str(remote, "remoteUrl")+"/" {
			cp["remoteUrl"] = remote["remoteUrl"]
		}
		want["proxy"] = remote
		want["negativeCache"] = pick(sub(r, "negativeCache"), "enabled", "timeToLive")
		hc := sub(r, "httpClient")
		client := pick(hc, "blocked", "autoBlock")
		if c := sub(hc, "connection"); c != nil {
			client["connection"] = pick(c, "retries", "userAgentSuffix", "timeout", "enableCircularRedirects", "enableCookies", "useTrustStore")
		}
		// A PUT without credentials takes the proxy's away, so what the
		// target holds is sent back: its secret stays, being left out.
		if ca := sub(sub(cur, "httpClient"), "authentication"); ca != nil {
			client["authentication"] = ca
		}
		want["httpClient"] = client
		// The remote's credentials are not readable. The proxy is made
		// without any: a user name alone would be sent with an empty password.
		if a := sub(hc, "authentication"); a != nil {
			kindOf, ca := str(a, "type"), sub(sub(cur, "httpClient"), "authentication")
			switch {
			case kindOf == "ntlm":
				it.note("NTLM authentication to the remote (user %s) has no equivalent: put a proxy that speaks NTLM in front of it", str(a, "username"))
			case str(ca, "type") == kindOf && str(ca, "username") == str(a, "username"): // entered since
			case kindOf == "bearerToken":
				it.todo("repository %s: enter the bearer token of its remote (it is not readable from Nexus; the proxy has none)", name)
			default:
				it.todo("repository %s: enter the password of its remote's user %q (it is not readable from Nexus; the proxy sends no credentials)", name, str(a, "username"))
			}
		}
		rule := cmp.Or(str(r, "routingRuleName"), str(r, "routingRule"))
		if rule != "" && !m.have["routing-rule"][rule] {
			it.note("without the routing rule %s, which is not on the target", rule)
			rule = ""
		}
		want["routingRule"] = rule
		if rep := sub(r, "replication"); rep["preemptivePullEnabled"] == true {
			want["replication"] = pick(rep, "preemptivePullEnabled", "assetPathRegex")
		}
	case "group":
		g := sub(r, "group")
		for _, member := range list(g, "memberNames") {
			if !m.have["repository"][member] {
				it.skip = "its member " + member + " is missing on the target"
				return it
			}
		}
		want["group"] = pick(g, "memberNames")
		if w := str(g, "writableMember"); w != "" {
			want["group"].(obj)["writableMember"] = w
		}
		// Nexus merges these groups, and this does not by itself, for a public
		// package must not reach an internal name unasked: the operator's call.
		if mergeFormats[format] && !m.merges(name, cur) {
			what := "a package"
			if format == "yum" {
				what = "a repodata/ path"
			}
			it.todo("repository %s: Nexus merges this group's members; here the first member that has %s answers alone. "+
				"mergeMembers merges them as Nexus does, and lets a public package add versions to an internal one of the same name: turn it on in its settings if clients need that", name, what)
		}
	}
	switch format {
	case "maven2":
		if typ != "group" {
			want["maven"] = pick(sub(r, "maven"), "versionPolicy", "layoutPolicy", "contentDisposition")
		}
	case "raw":
		want["raw"] = pick(sub(r, "raw"), "contentDisposition")
	case "cargo", "terraform", "swift":
		want[format] = pick(sub(r, format), "requireAuthentication")
	case "nuget":
		if typ == "proxy" {
			want["nugetProxy"] = pick(sub(r, "nugetProxy"), "nugetVersion")
		}
	case "apt":
		if typ == "hosted" {
			want["apt"] = pick(sub(r, "apt"), "distribution")
		} else if typ == "proxy" {
			want["apt"] = pick(sub(r, "apt"), "distribution", "flat", "enforceDistribution")
		}
		// Nexus answers a hosted repository's armored private key, without
		// its passphrase. A key that needs none is given to a new
		// repository; any other is the administrator's to enter.
		if sg := sub(r, "aptSigning"); sg != nil && typ == "hosted" && sub(cur, "aptSigning") == nil {
			if _, err := pgp.ParsePrivateKey(str(sg, "keypair")); err == nil && cur == nil {
				create["aptSigning"] = obj{"keypair": sg["keypair"]}
				it.note("with its apt signing key, which has no passphrase")
			} else {
				it.todo("repository %s: enter its apt signing key and passphrase (they are not readable from Nexus); until then its metadata is not signed", name)
			}
		} else if sg != nil && typ == "proxy" {
			it.note("a proxy's metadata is served as the remote signed it: the signing key is not used")
		}
	case "yum":
		y := sub(r, "yum")
		if typ == "hosted" {
			want["yum"] = pick(y, "repodataDepth")
		}
		if depth, _ := y["repodataDepth"].(float64); depth > 0 && str(y, "deployPolicy") == "PERMISSIVE" {
			it.note("deployPolicy PERMISSIVE has no equivalent: at a repodata depth every RPM must be in a directory")
		}
		if str(sub(r, "yumSigning"), "keypair") != "" {
			it.note("yum metadata is not signed here: clients that set repo_gpgcheck=1 must turn it off")
		}
	case "docker":
		d := sub(r, "docker")
		for _, k := range []string{"httpPort", "httpsPort"} {
			if port, ok := d[k].(float64); ok {
				it.note("its connector (%s %.0f) is not carried over: docker is served at <Command Center host>/%s/<image>", k, port, name)
				if typ == "hosted" {
					it.note("its images are copied with -repo %s -docker-from %s://%s:%.0f", name, strings.TrimSuffix(k, "Port"), m.im.from.Hostname(), port)
				}
			}
		}
		if str(d, "subdomain") != "" {
			it.note("its subdomain connector is not carried over")
		}
		if d["v1Enabled"] == true {
			it.note("the Docker V1 API is not served")
		}
		// A repository without anonymous pull in Nexus has none after the
		// move: its anonymous read is off whatever the target's global says.
		force := d["forceBasicAuth"] != false || !m.dockerRealmOn()
		want["docker"] = obj{"forceBasicAuth": force}
		switch {
		case !force:
			it.note("anonymous pull follows the anonymous read setting, not a bearer token realm")
		case d["forceBasicAuth"] == false:
			it.note("anonymous pull is off: forceBasicAuth is off in Nexus, but so is its Docker Bearer Token realm")
		}
		if st["latestPolicy"] == true {
			it.note("redeploying only the latest tag has no equivalent: the write policy applies to every tag")
		}
		if p := sub(r, "dockerProxy"); p["cacheForeignLayers"] == true {
			it.note("foreign layers are not cached: clients fetch them from their own URLs")
		}
	}
	it.want = want
	path := nx + "/repositories/" + format + "/" + typ
	if format == "maven2" {
		path = nx + "/repositories/maven/" + typ
	}
	body := obj{}
	for _, part := range []obj{want, create} {
		for k, v := range part {
			body[k] = v
		}
	}
	it.create, it.update = &request{http.MethodPost, path, body}, &request{http.MethodPut, path + "/" + url.PathEscape(name), want}
	return it
}

// ---- privileges, roles, users ----

// privileges migrates the privileges made in Nexus (its built-in ones are
// Command Center's by the same names).
func (m *migration) privileges() {
	const kind = "privilege"
	const p = nx + "/security/privileges"
	src, cur, ok := m.lists(kind, p, p, "name")
	if !ok {
		return
	}
	for _, o := range src {
		if o["readOnly"] == true {
			continue
		}
		name, typ := str(o, "name"), str(o, "type")
		it := item{kind: kind, name: name, cur: cur[name], want: pick(o, "type", "name", "description")}
		fields := map[string][]string{"application": {"domain", "actions"}, "wildcard": {"pattern"}, "repository-view": {"format", "repository", "actions"},
			"repository-admin": {"format", "repository", "actions"}, "repository-content-selector": {"format", "repository", "contentSelector", "actions"}}[typ]
		for _, f := range fields {
			it.want[f] = o[f]
		}
		format, repo, sel := str(o, "format"), str(o, "repository"), str(o, "contentSelector")
		switch {
		case fields == nil:
			it.skip = "a " + typ + " privilege has no equivalent"
		case format != "" && format != "*" && !m.formats[format]:
			it.skip = "format " + format + " is not supported"
		case repo != "" && !strings.HasPrefix(repo, "*") && !m.have["repository"][repo]:
			it.skip = "its repository " + repo + " is missing on the target"
		case sel != "" && !m.have["content-selector"][sel]:
			it.skip = "its content selector " + sel + " is missing on the target"
		}
		it.create = &request{http.MethodPost, p + "/" + typ, it.want}
		it.update = &request{http.MethodPut, p + "/" + typ + "/" + url.PathEscape(name), it.want}
		if have, _ := m.apply(it); have {
			m.have[kind][name] = true
		}
	}
}

// builtinAdmin is Nexus's administrator role, which Command Center has
// built in under the same name in this API.
const builtinAdmin = "nx-admin"

// rolesOf migrates the roles, a role after the ones it contains. A
// privilege Command Center has no equivalent for is left out of the role
// (a role then grants less than in Nexus, never more).
func (m *migration) rolesOf() {
	const kind = "role"
	const p = nx + "/security/roles"
	src, cur, ok := m.lists(kind, p+"?source=default", p, "id")
	if !ok {
		return
	}
	m.roles = src
	var privs []obj
	if !m.readable(kind, nil, m.dst(http.MethodGet, nx+"/security/privileges", nil, &privs)) {
		return
	}
	known := map[string]bool{}
	for _, x := range privs {
		known[str(x, "name")] = true
	}
	for name := range m.have["privilege"] {
		known[name] = true
	}
	for repo, format := range m.repoFormat { // the built-in ones of a repository made by this run
		for _, a := range []string{"browse", "read", "edit", "add", "delete", "*"} {
			known["nx-repository-view-"+format+"-"+repo+"-"+a], known["nx-repository-admin-"+format+"-"+repo+"-"+a] = true, a != "add"
		}
	}
	m.have[kind][builtinAdmin] = true
	for _, o := range dependFirst(src, "id", func(o obj) []string { return list(o, "roles") }) {
		id := str(o, "id")
		if id == builtinAdmin {
			continue
		}
		it := item{kind: kind, name: id, cur: cur[id], want: pick(o, "id", "name", "description")}
		keep := func(field string, ok map[string]bool, what string) {
			kept, dropped := []string{}, []string{}
			for _, x := range list(o, field) {
				if ok[x] {
					kept = append(kept, x)
				} else {
					dropped = append(dropped, x)
				}
			}
			if it.want[field] = kept; len(dropped) > 0 {
				sort.Strings(dropped)
				it.note("without %s that the target does not have: %s", what, strings.Join(dropped, ", "))
			}
		}
		keep("privileges", known, "privileges")
		keep("roles", m.have[kind], "roles")
		it.create = &request{http.MethodPost, p, it.want}
		it.update = &request{http.MethodPut, p + "/" + url.PathEscape(id), it.want}
		if have, _ := m.apply(it); have {
			m.have[kind][id] = true
		}
	}
}

// newPassword is a password nobody has seen: 144 random bits, and a small
// letter, a capital, a digit and a sign for the policies that ask for them.
func newPassword() string {
	b := make([]byte, 18)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b) + "-aZ9"
}

// usersOf migrates the local users. Nexus does not hand out password
// hashes, so a new user gets a random password, which goes to the
// -passwords-out file or nowhere.
func (m *migration) usersOf() {
	const kind = "user"
	const p = nx + "/security/users"
	src, cur, ok := m.lists(kind, p+"?source=default", p+"?source=default", "userId")
	if !ok {
		return
	}
	m.users = src
	m.fresh = len(cur) <= 1 // nobody but the administrator who runs this
	for _, o := range src {
		id := str(o, "userId")
		it := item{kind: kind, name: id, cur: cur[id], want: pick(o, "userId", "firstName", "lastName", "emailAddress")}
		if id == "anonymous" {
			it.skip = "anonymous access is a setting of Command Center, not a user"
		}
		switch s := str(o, "status"); s { // as Command Center keeps them
		case "locked":
			it.want["status"] = "disabled"
		case "changepassword":
			it.want["status"] = "active"
		default:
			it.want["status"] = s
		}
		roles := []string{}
		for _, r := range list(o, "roles") {
			if m.have["role"][r] {
				roles = append(roles, r)
			} else {
				it.note("without the role %s, which is not on the target", r)
			}
		}
		it.want["roles"] = roles
		pw := newPassword()
		body := obj{"password": pw}
		for k, v := range it.want {
			body[k] = v
		}
		it.create = &request{http.MethodPost, p, body}
		it.update = &request{http.MethodPut, p + "/" + url.PathEscape(id), it.want}
		have, did := m.apply(it)
		if have {
			m.have[kind][id] = true
		}
		switch {
		case did != "created":
		case m.im.dryRun:
			m.todo("users: every user this makes gets a random password; give -passwords-out FILE to keep them, or set each one afterwards")
		case m.pw == nil:
			m.todo("user %s: set a password (Nexus does not hand out password hashes; the random one it was made with is not kept)", id)
		default:
			if _, err := fmt.Fprintf(m.pw, "%s\t%s\n", id, pw); err != nil {
				m.todo("user %s: set a password (the generated one could not be written to %s: %v)", id, m.pw.Name(), err)
				break
			}
			m.todo("user %s: hand over the generated password from %s and have it changed", id, m.pw.Name())
		}
	}
}

// ---- anonymous access, LDAP, realms, e-mail ----

// anonymous turns the global anonymous read on only when Nexus's anonymous
// user reads every repository: a narrower grant would become a wider one.
func (m *migration) anonymous() {
	const kind = "anonymous"
	const p = nx + "/security/anonymous"
	var src, cur obj
	if !m.readable(kind, m.src(http.MethodGet, p, nil, &src), m.dst(http.MethodGet, p, nil, &cur)) {
		return
	}
	enabled := src["enabled"] == true
	if id := str(src, "userId"); enabled && !m.readsEverything(id) {
		enabled = false
		m.todo("anonymous: Nexus lets anonymous callers read what the roles of its user %q allow, not every repository: "+
			"turn anonymous read on for those repositories (the global switch is left off)", id)
	}
	it := item{kind: kind, name: "access", cur: cur, unset: cur["enabled"] != true, want: obj{"enabled": enabled}}
	it.update = &request{http.MethodPut, p, obj{"enabled": enabled, "userId": "anonymous", "realmName": "NexusAuthorizingRealm"}}
	m.apply(it)
	// The other way reads could become wider than in Nexus: Command Center's
	// authenticatedRead, which Nexus has no object for. A target nobody else
	// uses yet gets Nexus's rule, so that the migrated roles decide who
	// reads; one with users of its own keeps its setting, which is the
	// administrator's to change.
	var settings obj
	if m.dst(http.MethodGet, native+"/settings", nil, &settings) != nil || settings["authenticatedRead"] != true {
		return
	}
	if !m.fresh {
		m.todo("settings: Command Center lets every signed-in user read every repository (authenticatedRead), where Nexus asks for a privilege: " +
			"turn it off in the general settings if the migrated roles are to decide who reads")
		return
	}
	m.apply(item{kind: "setting", name: "authenticatedRead", cur: obj{"authenticatedRead": true}, unset: true, want: obj{"authenticatedRead": false},
		update: &request{http.MethodPut, native + "/settings", obj{"authenticatedRead": false}},
		notes:  []string{"signed-in users read what their roles allow, as in Nexus (the target has no other users yet)"}})
}

// readsEverything reports whether the source's user id holds, through its
// roles, a built-in privilege that reads every repository.
func (m *migration) readsEverything(id string) bool {
	byID := map[string]obj{}
	for _, r := range m.roles {
		byID[str(r, "id")] = r
	}
	seen := map[string]bool{}
	var reads func(role string) bool
	reads = func(role string) bool {
		r, ok := byID[role]
		if !ok || seen[role] {
			return false
		}
		seen[role] = true
		for _, p := range list(r, "privileges") {
			if p == "nx-all" || p == "nx-repository-view-*-*-*" || p == "nx-repository-view-*-*-read" {
				return true
			}
		}
		return slices.ContainsFunc(list(r, "roles"), reads)
	}
	for _, u := range m.users {
		if str(u, "userId") == id {
			return slices.ContainsFunc(list(u, "roles"), reads)
		}
	}
	return false
}

// ldap migrates Nexus's first LDAP server onto Command Center's one. The
// bind password is not readable and Command Center takes no bind DN without
// one, so the server is made without both: the directory is searched
// anonymously until the administrator enters them. LDAP groups become
// roles as in Nexus, by name: one mapping per role, never one to the
// administrator role, and what the target maps already is kept.
func (m *migration) ldap() {
	const kind = "ldap"
	var servers []obj
	var cur obj
	if !m.readable(kind, m.src(http.MethodGet, nx+"/security/ldap", nil, &servers), m.dst(http.MethodGet, native+"/realms/ldap", nil, &cur)) {
		return
	}
	m.ldapReady = str(cur, "url") != ""
	if len(servers) == 0 {
		return
	}
	sort.SliceStable(servers, func(i, j int) bool {
		oi, _ := servers[i]["order"].(float64)
		oj, _ := servers[j]["order"].(float64)
		return oi < oj
	})
	s := servers[0]
	it := item{kind: kind, name: str(s, "name"), cur: cur, unset: !m.ldapReady}
	idAttr := cmp.Or(str(s, "userIdAttribute"), "uid")
	filter := []string{"(" + idAttr + "={user})"}
	if class := str(s, "userObjectClass"); class != "" {
		filter = append([]string{"(objectClass=" + class + ")"}, filter...)
	}
	if f := strings.TrimSpace(str(s, "userLdapFilter")); f != "" {
		if !strings.HasPrefix(f, "(") {
			f = "(" + f + ")"
		}
		filter = append(filter, f)
	}
	userFilter := filter[0]
	if len(filter) > 1 {
		userFilter = "(&" + strings.Join(filter, "") + ")"
	}
	base := strings.Trim(str(s, "userBaseDn")+","+str(s, "searchBase"), ",")
	timeout, _ := s["connectionTimeoutSeconds"].(float64)
	want := obj{"url": fmt.Sprintf("%s://%s:%v", strings.ToLower(str(s, "protocol")), str(s, "host"), s["port"]),
		"userBaseDN": base, "userFilter": userFilter, "userIdAttr": idAttr,
		"nameAttr":  cmp.Or(str(s, "userRealNameAttribute"), "cn"),
		"emailAttr": cmp.Or(str(s, "userEmailAddressAttribute"), "mail"),
		"groupAttr": cmp.Or(str(s, "userMemberOfAttribute"), "memberOf"),

		"userOneLevel": s["userSubtree"] == false}
	if timeout > 0 {
		want["timeoutSeconds"] = min(max(timeout, meta.MinLDAPTimeoutSeconds), meta.MaxLDAPTimeoutSeconds)
	}
	if s["ldapGroupsAsRoles"] == true {
		mappings, _ := cur["groupMappings"].([]any)
		mapped := map[string]bool{}
		for _, x := range mappings {
			g, _ := x.(obj)
			mapped[str(g, "group")] = true
		}
		for _, role := range sortedKeys(m.have["role"]) {
			if role != builtinAdmin && !mapped[role] {
				mappings = append(mappings, obj{"group": role, "roles": []string{role}})
			}
		}
		want["groupMappings"] = mappings
		it.note("an LDAP group gives the role of its name, as in Nexus, but for %s: map the administrators' group by its DN yourself", builtinAdmin)
		if strings.EqualFold(str(s, "groupType"), "static") {
			want["groupType"] = "static"
			want["groupBaseDN"] = strings.Trim(str(s, "groupBaseDn")+","+str(s, "searchBase"), ",")
			want["groupSubtree"] = s["groupSubtree"] == true
			want["groupObjectClass"], want["groupIdAttr"] = str(s, "groupObjectClass"), str(s, "groupIdAttribute")
			want["groupMemberAttr"], want["groupMemberFormat"] = str(s, "groupMemberAttribute"), str(s, "groupMemberFormat")
		}
	} else if strings.EqualFold(str(s, "groupType"), "static") {
		// Nexus reads no groups then, and without a mapping here they would
		// give nothing but a search at every sign-in.
		it.note("its static group settings are not carried: ldapGroupsAsRoles is off, so Nexus does not read them")
	}
	if scheme := strings.ToUpper(str(s, "authScheme")); scheme != "" && scheme != "NONE" {
		if dn := str(s, "authUsername"); str(cur, "bindDN") == dn && cur["bindPasswordSet"] == true {
			want["bindDN"] = dn
		} else {
			it.todo("ldap %s: enter the bind DN %q and its password (the password is not readable from Nexus); until then the directory is searched anonymously", it.name, dn)
		}
		if scheme != "SIMPLE" {
			it.note("the %s bind has no equivalent: Command Center binds with a DN and a password", scheme)
		}
	}
	it.want = want
	body := obj{}
	for _, part := range []obj{cur, want} { // the whole configuration is put: what the target has beyond want stays
		for k, v := range part {
			body[k] = v
		}
	}
	delete(body, "bindPasswordSet")
	it.update = &request{http.MethodPut, native + "/realms/ldap", body}
	if _, did := m.apply(it); did != "" {
		m.ldapReady = true
	}
	for _, o := range servers[1:] {
		m.n.skipped++
		m.line(kind, str(o, "name"), "skipped (Command Center has one LDAP server: "+it.name+", Nexus's first, is the one migrated)", nil)
	}
}

// setupRealms are the realms that need a configuration of their own on
// Command Center, which this does not migrate.
var setupRealms = map[string]bool{"Crowd": true, "SamlRealm": true, "OAuth2Realm": true, "rutauth-realm": true}

// realms puts Nexus's active realms first, in its order, and keeps after
// them what the target has on: a migration turns no realm off (Command
// Center's token realm, which API tokens need, has no single Nexus twin).
func (m *migration) realms() {
	const kind = "realms"
	const p = nx + "/security/realms/active"
	var src, cur []string
	var avail []obj
	err := m.dst(http.MethodGet, p, nil, &cur)
	if err == nil {
		err = m.dst(http.MethodGet, nx+"/security/realms/available", nil, &avail)
	}
	if !m.readable(kind, m.src(http.MethodGet, p, nil, &src), err) {
		return
	}
	it := item{kind: kind, name: "active", cur: obj{"active": cur}}
	it.unset = !slices.ContainsFunc(cur, func(id string) bool { return id == "LdapRealm" || setupRealms[id] })
	want := []string{}
	for _, id := range src {
		switch {
		case !slices.ContainsFunc(avail, func(a obj) bool { return str(a, "id") == id }):
			it.note("%s has no equivalent", id)
		case slices.Contains(cur, id) || id != "LdapRealm" && !setupRealms[id] || id == "LdapRealm" && m.ldapReady:
			if want = append(want, id); id == "DefaultRole" && !slices.Contains(cur, id) {
				m.todo("realms: Nexus's Default Role realm is on: name its role under defaultRoles in the general settings (the role is a capability's, which this does not read)")
			}
		default:
			m.todo("realms: %s is on in Nexus: configure it on Command Center (Realms), then turn it on", id)
		}
	}
	for _, id := range cur {
		if !slices.Contains(want, id) {
			want = append(want, id)
		}
	}
	it.want = obj{"active": want}
	it.update = &request{http.MethodPut, p, want}
	m.apply(it)
}

// email migrates the SMTP settings without the password. Command Center
// always requires STARTTLS once it is on and always checks the server's
// identity over TLS, so the looser Nexus settings become the strict ones.
func (m *migration) email() {
	const kind = "email"
	var src, cur, smtp obj
	err := m.dst(http.MethodGet, nx+"/email", nil, &cur)
	if err == nil {
		err = m.dst(http.MethodGet, native+"/settings/smtp", nil, &smtp)
	}
	if !m.readable(kind, m.src(http.MethodGet, nx+"/email", nil, &src), err) {
		return
	}
	if src["enabled"] != true {
		return // Nexus sends no mail: nothing to carry over
	}
	it := item{kind: kind, name: "settings", cur: cur, unset: str(cur, "host") == "", want: pick(src, "enabled", "host", "port", "username", "fromAddress", "subjectPrefix", "nexusTrustStoreEnabled")}
	tls := src["sslOnConnectEnabled"] == true
	startTLS := !tls && (src["startTlsEnabled"] == true || src["startTlsRequired"] == true)
	it.want["sslOnConnectEnabled"], it.want["startTlsEnabled"], it.want["startTlsRequired"] = tls, startTLS, startTLS
	it.want["sslServerIdentityCheckEnabled"] = tls || startTLS
	if startTLS && src["startTlsRequired"] != true {
		it.note("STARTTLS is required once it is on: mail is not sent to a server that does not offer it")
	}
	if (tls || startTLS) && src["sslServerIdentityCheckEnabled"] != true {
		it.note("the server's identity is always checked over TLS: its certificate must be valid for its name")
	}
	if u := str(src, "username"); u != "" && smtp["passwordSet"] != true {
		it.todo("email: enter the password of the SMTP user %q (it is not readable from Nexus); until then mail is not accepted by a server that asks for it", u)
	}
	it.update = &request{http.MethodPut, nx + "/email", it.want}
	m.apply(it)
}

// ---- tasks and the HTTP settings (ExtDirect) ----

// tasks makes the scheduled tasks Command Center has a type for. A task is
// found again by its type and name; its schedule and properties are not
// compared (the Nexus API does not answer them), so one that is there is
// left as it is.
func (m *migration) tasks() {
	const kind = "task"
	var src []obj
	var cur struct {
		Items []obj `json:"items"`
	}
	if !m.readable(kind, m.direct("coreui_Task", "read", &src), m.dst(http.MethodGet, nx+"/tasks", nil, &cur)) {
		return
	}
	sort.SliceStable(src, func(i, j int) bool { return str(src[i], "name") < str(src[j], "name") })
	for _, t := range src {
		typ, name := str(t, "typeId"), str(t, "name")
		it := item{kind: kind, name: name, want: obj{"name": name, "type": typ}}
		for _, c := range cur.Items {
			if str(c, "name") == name && str(c, "type") == typ {
				it.cur = c
			}
		}
		frequency := obj{"schedule": str(t, "schedule")}
		switch str(t, "schedule") {
		case "manual":
		case "advanced":
			frequency["schedule"], frequency["cronExpression"] = "cron", t["cronExpression"]
		case "once", "hourly", "daily", "weekly", "monthly":
			// The start is an instant in the Nexus server's zone, which the
			// days of a weekly or monthly schedule are counted in.
			start, err := time.Parse(time.RFC3339, str(t, "startDate"))
			if err != nil {
				it.skip = "its start date cannot be read"
			}
			frequency["startDate"], frequency["timeZoneOffset"], frequency["recurringDays"] = start.UnixMilli(), start.Format("-07:00"), t["recurringDays"]
		default:
			it.skip = "a task Nexus runs by itself (schedule " + str(t, "schedule") + ")"
		}
		if why := nexusapi.TaskTypeRefusal(typ); why != "" {
			it.skip = "no equivalent of " + typ + ": " + why
		}
		properties := obj{}
		for k, v := range sub(t, "properties") { // without Nexus's own bookkeeping
			if !strings.HasPrefix(k, ".") && !strings.HasPrefix(k, "lastRunState.") {
				properties[k] = v
			}
		}
		body := obj{"type": typ, "name": name, "enabled": t["enabled"], "frequency": frequency, "properties": properties}
		if to := str(t, "alertEmail"); to != "" {
			body["alertEmail"], body["notificationCondition"] = to, t["notificationCondition"]
		}
		it.create = &request{http.MethodPost, nx + "/tasks", body}
		m.apply(it)
	}
}

// httpSettings migrates the outbound HTTP settings: the user agent suffix,
// the timeout, the retries and the proxy. Command Center has one proxy for
// HTTP and HTTPS, so Nexus's HTTP proxy is taken (its HTTPS proxy when
// there is no other). The proxy's password is not readable, and a user
// name alone would be sent with an empty password, so it is set without.
func (m *migration) httpSettings() {
	const kind = "http"
	var s, settings obj
	if !m.readable(kind, m.direct("coreui_HttpSettings", "read", &s), m.dst(http.MethodGet, native+"/settings", nil, &settings)) {
		return
	}
	cur, def := sub(settings, "http"), meta.DefaultHTTP()
	cp := sub(cur, "proxy")
	it := item{kind: kind, name: "settings", cur: cur}
	it.unset = str(cp, "host") == "" && len(differs(obj{"userAgentSuffix": def.UserAgentSuffix, "connectTimeoutSeconds": def.ConnectTimeoutSeconds,
		"timeoutSeconds": def.TimeoutSeconds, "retries": def.Retries}, cur)) == 0
	want := obj{}
	if v := str(s, "userAgentSuffix"); v != "" {
		want["userAgentSuffix"] = v
	}
	if v, ok := s["timeout"].(float64); ok {
		want["connectTimeoutSeconds"], want["timeoutSeconds"] = v, v
	}
	if v, ok := s["retries"].(float64); ok {
		want["retries"] = v
	}
	// The whole http value is put: what the target has beyond want stays.
	body := pick(cur, "userAgentSuffix", "connectTimeoutSeconds", "timeoutSeconds", "retries")
	for k, v := range want {
		body[k] = v
	}
	send := pick(cp, "host", "port", "username", "nonProxyHosts")
	px := "http"
	if s["httpEnabled"] != true {
		px = "https"
	}
	if s[px+"Enabled"] == true {
		proxy := obj{"host": s[px+"Host"], "port": s[px+"Port"], "nonProxyHosts": s["nonProxyHosts"]}
		if canon(pick(cp, "host", "port"), true) != canon(pick(proxy, "host", "port"), true) {
			send["username"], send["clearPassword"] = "", true // the stored credentials are another proxy's
		}
		for k, v := range proxy {
			send[k] = v
		}
		if send["nonProxyHosts"] == nil {
			send["nonProxyHosts"] = []string{}
		}
		if u := str(s, px+"AuthUsername"); s[px+"AuthEnabled"] == true {
			switch {
			case str(s, px+"AuthNtlmHost") != "" || str(s, px+"AuthNtlmDomain") != "":
				it.note("NTLM authentication to the proxy (user %s) has no equivalent", u)
			case send["username"] == u && cp["passwordSet"] == true:
				proxy["username"] = u
			default:
				it.todo("http: enter the proxy's user %q and its password (the password is not readable from Nexus); until then requests go through the proxy without credentials", u)
			}
		}
		if px == "http" && s["httpsEnabled"] == true && (s["httpsHost"] != s["httpHost"] || s["httpsPort"] != s["httpPort"]) {
			it.note("Nexus's HTTPS proxy %s:%v is not carried over: one proxy serves HTTP and HTTPS", str(s, "httpsHost"), s["httpsPort"])
		}
		want["proxy"] = proxy
	}
	if len(want) == 0 {
		return // Nexus runs on its defaults
	}
	body["proxy"] = send
	it.want = want
	it.update = &request{http.MethodPut, native + "/settings", obj{"http": body}}
	m.apply(it)
}
