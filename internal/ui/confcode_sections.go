package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/cleanup"
	"commandcenter/internal/config"
	"commandcenter/internal/ldap"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/saml"
	"commandcenter/internal/tasks"
	"commandcenter/internal/vuln"
)

// The sections of the configuration document, in the order an apply
// creates and updates them (deletes run in the reverse order). Each one's
// objects have the shape of its API's request body.

// Document shapes the request bodies of which are declared inside their handlers.
type (
	confCert struct {
		PEM string `json:"pem"`
	}
	confRole struct {
		ID          string         `json:"id"`
		Description string         `json:"description"`
		Privileges  []privilegeDTO `json:"privileges"`
	}
	confUser struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		Email  string   `json:"email"`
		Source string   `json:"source"`
		Status string   `json:"status"`
		Roles  []string `json:"roles"`
	}
	confUserRead struct {
		confUser
		PasswordHash string `json:"passwordHash,omitempty"`
	}
	confRealms struct {
		Realms []meta.Realm `json:"realms"`
	}
	confLogging struct {
		Level string `json:"level"`
	}
)

// confSectionList is set in init: the sections' functions refer to it.
var confSectionList []*confSection

func confSections() []*confSection { return confSectionList }

func init() {
	confSectionList = []*confSection{
		{name: "trustStore", key: "pem", base: "/-/api/ssl/certificates", proto: func() any { return new(confCert) },
			keyOf: certID, read: readTrustStore, check: checkTrustStore},
		{name: "blobStores", key: "name", base: "/-/api/blobstores", proto: func() any { return new(blobStoreRequest) },
			secrets: []confSecret{{path: "s3.secretAccessKey", noClear: true}, {path: "azure.accountKey", noClear: true},
				{path: "azure.sasToken", noClear: true}, {path: "azure.clientSecret", noClear: true}, {path: "google.accountKey", noClear: true}},
			keepDel: func(k string) bool { return k == repo.DefaultStore },
			order:   blobStoreOrder, read: readBlobStores, check: checkBlobStores, write: writeBlobStore},
		{name: "routingRules", key: "name", base: "/-/api/routing-rules", proto: func() any { return new(ruleBody) },
			read: readRoutingRules, check: checkRoutingRules},
		{name: "contentSelectors", key: "name", base: "/-/api/content-selectors", proto: func() any { return new(selectorBody) },
			read: readSelectors, check: checkSelectors},
		{name: "cleanupPolicies", key: "name", base: "/-/api/cleanup/policies", proto: func() any { return new(policyRequest) },
			canon: func(o confObj) any {
				p := asConf[policyRequest](o).policy()
				if p.Validate() != nil {
					return nil
				}
				return policyRequestOf(p)
			},
			read: readPolicies, check: checkPolicies},
		{name: "repositories", key: "name", base: "/-/api/repositories", proto: func() any { return new(repoRequest) },
			secrets: []confSecret{
				{path: "remoteAuth.password", clear: "remoteAuth.clearPassword"},
				{path: "remoteAuth.bearerToken", clear: "remoteAuth.clearBearerToken"},
				{path: "signingKey", clear: "clearSigningKey"},
				// A remote with user info or a query holds credentials: the
				// document keeps it without them.
				{path: "remote", clear: "clearRemoteHiddenParts", when: remoteHidden, show: remotePublic},
			},
			keep: []string{"online", "autoBlock", "cleanupPolicy", "firewall"},
			canon: func(o confObj) any {
				r := asConf[repoRequest](o)
				d, _, err := r.definition()
				if err != nil {
					return nil
				}
				return repoRequestOf(d)
			},
			order: repoOrder, read: readRepositories, check: checkRepositories},
		{name: "roles", key: "id", base: "/-/api/roles", proto: func() any { return new(confRole) }, noUpdateKey: true,
			read: readRoles, check: checkRoles},
		{name: "users", key: "id", base: "/-/api/users", proto: func() any { return new(confUser) },
			secrets: []confSecret{{path: "passwordHash", noClear: true, own: true}},
			read:    readUsers, check: checkUsers, write: writeUser},
		{name: "settings", single: true, base: "/-/api/settings", proto: func() any { return new(settingsRequest) }, partial: true,
			secrets: []confSecret{{path: "http.proxy.password", clear: "http.proxy.clearPassword"}},
			read:    readSettings, check: checkSettings},
		{name: "smtp", single: true, base: "/-/api/settings/smtp", proto: func() any { return new(smtpRequest) },
			secrets: []confSecret{{path: "password", clear: "clearPassword"}},
			read:    func(c *confRun) ([]any, error) { return c.setting(func(s meta.Settings) any { return s.SMTP }) },
			check:   checkSMTP},
		{name: "ldap", single: true, base: "/-/api/realms/ldap", proto: func() any { return new(ldapRequest) },
			secrets: []confSecret{{path: "bindPassword", clear: "clearBindPassword"}},
			canon:   func(o confObj) any { return ldapMerged(asConf[ldapRequest](o)) },
			read:    func(c *confRun) ([]any, error) { return c.setting(func(s meta.Settings) any { return s.LDAP }) },
			check:   checkLDAP},
		{name: "oidc", single: true, base: "/-/api/realms/oidc", proto: func() any { return new(oidcRequest) },
			secrets: []confSecret{{path: "clientSecret", clear: "clearClientSecret"}},
			canon:   func(o confObj) any { return asConf[oidcRequest](o).merged(meta.OIDC{}) },
			read:    func(c *confRun) ([]any, error) { return c.setting(func(s meta.Settings) any { return s.OIDC }) },
			check:   checkOIDC},
		{name: "saml", single: true, base: "/-/api/realms/saml", proto: func() any { return new(samlRequest) },
			// The service provider's key pair: generated by the first save;
			// it moves with the secrets only, so the IdP keeps trusting it.
			secrets: []confSecret{{path: "spPrivateKey", noClear: true, own: true}, {path: "spCertificate", noClear: true, own: true}},
			canon:   func(o confObj) any { return asConf[samlRequest](o).merged(meta.SAML{}) },
			read:    func(c *confRun) ([]any, error) { return c.setting(func(s meta.Settings) any { return s.SAML }) },
			check:   checkSAML, write: writeSAML},
		{name: "crowd", single: true, base: "/-/api/realms/crowd", proto: func() any { return new(crowdRequest) },
			secrets: []confSecret{{path: "applicationPassword", clear: "clearApplicationPassword"}},
			canon:   func(o confObj) any { return crowdMerged(asConf[crowdRequest](o)) },
			read:    func(c *confRun) ([]any, error) { return c.setting(func(s meta.Settings) any { return s.Crowd }) },
			check:   checkCrowd},
		{name: "rutauth", single: true, base: "/-/api/realms/rutauth", proto: func() any { return new(meta.RUT) },
			read:  func(c *confRun) ([]any, error) { return c.setting(func(s meta.Settings) any { return s.RUT }) },
			check: checkRUT},
		{name: "realms", single: true, base: "/-/api/realms", proto: func() any { return new(confRealms) },
			read: func(c *confRun) ([]any, error) {
				return c.setting(func(s meta.Settings) any { return confRealms{Realms: s.Realms} })
			},
			check: checkRealms},
		{name: "tasks", key: "name", base: "/-/api/tasks", proto: func() any { return new(tasks.Def) }, extra: []string{"builtin"},
			keep: []string{"enabled", "notifyEmail"}, keyOf: taskKey, idOf: taskID,
			keepDel: func(k string) bool { return k == tasks.SeedID },
			avail:   func(u *UI) bool { return u.Tasks != nil }, read: readTasks, check: checkTasks},
		{name: "webhooks", key: "name", base: "/-/api/webhooks", proto: func() any { return new(webhookRequest) },
			secrets: []confSecret{{path: "secret"}}, idOf: webhookID,
			avail: func(u *UI) bool { return u.Webhooks != nil }, read: readWebhooks, check: checkWebhooks},
		{name: "vulnerability", single: true, base: "/-/api/vulnerability/settings", proto: func() any { return new(vuln.Settings) },
			avail: func(u *UI) bool { return u.Vuln != nil },
			read:  func(c *confRun) ([]any, error) { return []any{c.u.Vuln.Settings(c.ctx)}, nil },
			check: func(c *confRun, objs []confObj) {
				if err := asConf[vuln.Settings](objs[0]).Validate(); err != nil {
					c.bad("vulnerability: %v", err)
				}
			}},
		// The log level of the running nodes, until their next start: a CLI
		// process has none.
		{name: "logging", single: true, base: "/-/api/logging", proto: func() any { return new(confLogging) },
			avail: func(u *UI) bool { return u.LogLevel != nil },
			read: func(c *confRun) ([]any, error) {
				return []any{confLogging{Level: levelName(c.u.LogLevel.Level())}}, nil
			},
			check: func(c *confRun, objs []confObj) {
				if _, ok := parseLevel(asConf[confLogging](objs[0]).Level); !ok {
					c.bad("logging: level must be debug, info, warn or error")
				}
			}},
	}
}

func confSectionByName(name string) *confSection {
	for _, s := range confSections() {
		if s.name == name {
			return s
		}
	}
	return nil
}

// setting reads the stored settings once per run.
func (c *confRun) setting(f func(meta.Settings) any) ([]any, error) {
	if c.set == nil {
		s, err := c.u.DB.LoadSettings(c.ctx)
		if err != nil {
			return nil, err
		}
		c.set = &s
	}
	return []any{f(*c.set)}, nil
}

// desiredOne is a single section's object after the apply, decoded.
func desiredOne[T any](c *confRun, name string) (T, bool) {
	o, ok := c.desired(name)[""]
	if !ok {
		var t T
		return t, false
	}
	return asConf[T](o), true
}

// ---- trust store ----

func certID(o confObj) string {
	p, _ := o["pem"].(string)
	certs, err := meta.ParseCertificates(p)
	if err != nil || len(certs) != 1 {
		return ""
	}
	return meta.Fingerprint(certs[0])
}

func readTrustStore(c *confRun) ([]any, error) {
	list, err := c.setting(func(s meta.Settings) any { return s.TrustedCerts })
	if err != nil {
		return nil, err
	}
	var out []any
	for _, p := range list[0].([]string) {
		out = append(out, confCert{PEM: p})
	}
	return out, nil
}

func checkTrustStore(c *confRun, objs []confObj) {
	var pems []string
	for _, o := range c.desired("trustStore") {
		pems = append(pems, o["pem"].(string))
	}
	for _, o := range objs { // the stored form, so the same certificate compares equal
		certs, _ := meta.ParseCertificates(o["pem"].(string))
		o["pem"] = certPEM(certs[0])
	}
	if err := meta.ValidateTrustedCertificates(pems); err != nil {
		c.bad("trustStore: %v", err)
	}
}

// ---- blob stores ----

func blobStoreOrder(objs []confObj) []confObj {
	out := slices.Clone(objs)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i]["type"] != meta.BlobStoreGroup && out[j]["type"] == meta.BlobStoreGroup
	})
	return out
}

func readBlobStores(c *confRun) ([]any, error) {
	rows, err := c.u.DB.ListBlobStores(c.ctx)
	if err != nil {
		return nil, err
	}
	var out []any
	for _, bs := range rows {
		b := blobStoreRequest{Name: bs.Name, Type: bs.Type, Path: bs.Path, Quota: bs.Quota}
		if b.Type == "" {
			b.Type = meta.BlobStoreFile
		}
		// The default place is the data directory's, whichever it is.
		if bs.Name == repo.DefaultStore || bs.Path == filepath.Join("blobstores", bs.Name) {
			b.Path = ""
		}
		if bs.S3 != nil {
			s3 := *bs.S3
			b.S3 = &s3
		}
		if bs.Azure != nil {
			az := *bs.Azure
			b.Azure = &az
		}
		if bs.Google != nil {
			g := *bs.Google
			b.Google = &g
		}
		if g := bs.Group; g != nil {
			b.Members, b.FillPolicy = append([]blob.GroupMember{}, g.Members...), g.FillPolicy
		}
		out = append(out, b)
	}
	return out, nil
}

func checkBlobStores(c *confRun, objs []confObj) {
	desired := c.desired("blobStores")
	for _, o := range objs {
		b := asConf[blobStoreRequest](o)
		at := "blobStores " + b.Name
		if err := repo.CheckName("blob store", b.Name); err != nil {
			c.bad("%s: %v", at, err)
		}
		if err := meta.ValidateQuota(b.Quota); err != nil {
			c.bad("%s: %v", at, err)
		}
		if b.Path != "" && !filepath.IsAbs(b.Path) {
			c.bad("%s: path must be absolute, or empty for <dataDir>/blobstores/%s", at, b.Name)
		}
		if b.Type == "" {
			b.Type, o["type"] = meta.BlobStoreFile, meta.BlobStoreFile
		}
		cur, exists := c.cur["blobStores"].objs[b.Name]
		switch b.Type {
		case meta.BlobStoreS3:
			if b.S3 == nil {
				c.bad("%s: an s3 store needs s3 with at least its bucket", at)
			} else if !exists {
				s3 := *b.S3
				s3.SecretKey = c.want["blobStores"].sec[b.Name]["s3.secretAccessKey"]
				if err := s3.Validate(); err != nil {
					c.bad("%s: s3: %v (an exported document needs secrets=encrypted to create an s3 store with keys)", at, err)
				}
			}
		case meta.BlobStoreGroup:
			g := blob.GroupConfig{Members: b.Members, FillPolicy: b.FillPolicy}
			if err := g.Validate(); err != nil {
				c.bad("%s: %v", at, err)
			}
			o["fillPolicy"] = g.FillPolicy
			for _, m := range b.Members {
				if mo := desired[m.Name]; mo != nil && mo["type"] == meta.BlobStoreGroup {
					c.bad("%s: member %q is a group", at, m.Name)
				}
			}
		}
		if !exists {
			continue
		}
		for _, f := range []string{"type", "path", "s3", "google"} {
			if !confSame(normJSON(cur[f]), normJSON(o[f])) {
				c.bad("%s: %s cannot be changed (create a new store and move the repositories to it)", at, f)
			}
		}
		// An azure store's credentials can change (writeBlobStore), its location cannot.
		ca, _ := normJSON(cur["azure"]).(map[string]any)
		oa, _ := normJSON(o["azure"]).(map[string]any)
		for _, f := range []string{"accountName", "containerName", "prefix", "endpoint"} {
			if !confSame(ca[f], oa[f]) {
				c.bad("%s: azure.%s cannot be changed (create a new store and move the repositories to it)", at, f)
			}
		}
	}
}

// normJSON is v as its JSON would decode, so maps from both sides compare.
func normJSON(v any) any {
	out, _ := toConfAny(v)
	return out
}

// writeBlobStore updates what PUT changes: the quota, a group's members and
// fill policy (the rest is immutable, checked before).
func writeBlobStore(c *confRun, st *ConfigStep) error {
	if st.Action != confUpdate {
		return c.writeAPI(st)
	}
	b := confObj{}
	for _, ch := range st.Changes {
		switch top, _, _ := strings.Cut(ch.Field, "."); top {
		case "quota":
			b["quota"] = st.want["quota"]
		case "members", "fillPolicy":
			b["members"], b["fillPolicy"] = st.want["members"], st.want["fillPolicy"]
		case "azure": // credentials: the PUT keeps a secret left empty under the same method
			b["azure"] = c.body(st)["azure"]
		}
	}
	if len(b) == 0 {
		return nil
	}
	return c.call(http.MethodPut, st.s.base+"/"+url.PathEscape(st.Key), b, http.StatusOK)
}

// ---- routing rules, content selectors, cleanup policies ----

func readRoutingRules(c *confRun) ([]any, error) {
	list, err := c.u.DB.ListRoutingRules(c.ctx)
	var out []any
	for _, r := range list {
		out = append(out, ruleBody{Name: r.Name, Description: r.Description, Mode: r.Mode, Matchers: r.Matchers})
	}
	return out, err
}

func checkRoutingRules(c *confRun, objs []confObj) {
	for _, o := range objs {
		b := asConf[ruleBody](o)
		o["mode"] = strings.ToUpper(b.Mode)
		if _, err := b.rule(); err != nil {
			c.bad("routingRules %s: %v", b.Name, strings.Join(err.(*repo.ValidationError).Errors, "; "))
		}
	}
}

func readSelectors(c *confRun) ([]any, error) {
	list, err := c.u.DB.ListContentSelectors(c.ctx)
	var out []any
	for _, s := range list {
		out = append(out, selectorBody{Name: s.Name, Description: s.Description, Expression: s.Expression})
	}
	return out, err
}

func checkSelectors(c *confRun, objs []confObj) {
	for _, o := range objs {
		b := asConf[selectorBody](o)
		o["expression"] = strings.TrimSpace(b.Expression)
		if _, err := b.selector(); err != nil {
			c.bad("contentSelectors %s: %v", b.Name, strings.Join(err.(*repo.ValidationError).Errors, "; "))
		}
	}
}

func policyRequestOf(p cleanup.Policy) policyRequest {
	return policyRequest{Name: p.Name, Format: p.Format, ComponentAgeDays: p.ComponentAgeDays, LastDownloadedDays: p.LastDownloadedDays,
		ReleaseType: p.ReleaseType, NamePattern: p.NamePattern, PathPattern: p.PathPattern, Retain: p.Retain, RetainSortBy: p.RetainSortBy,
		Exclude: exclusionJSON{NamePattern: p.Exclude.NamePattern, VersionPattern: p.Exclude.VersionPattern}, Notes: p.Notes}
}

func readPolicies(c *confRun) ([]any, error) {
	list, err := cleanup.ListPolicies(c.ctx, c.u.DB)
	var out []any
	for _, p := range list {
		out = append(out, policyRequestOf(p))
	}
	return out, err
}

func checkPolicies(c *confRun, objs []confObj) {
	for _, o := range objs {
		p := asConf[policyRequest](o).policy()
		if err := p.Validate(); err != nil {
			c.bad("cleanupPolicies %s: %v", p.Name, err)
		}
	}
}

// ---- repositories ----

func remoteHidden(v string) bool { _, hidden, _ := publicRemote(v); return hidden }
func remotePublic(v string) string {
	pub, _, _ := publicRemote(v)
	return pub
}

// repoRequestOf is the request that stores d as it is.
func repoRequestOf(d config.Repository) repoRequest {
	config.ApplyRepositoryDefaults(&d)
	b := repoRequest{Name: d.Name, Format: d.Format, Type: d.Type, Remote: d.Remote,
		MetadataMaxAge: d.MetadataMaxAge.Text(), ContentMaxAge: d.ContentMaxAge.Text(), NegativeCacheTTL: d.NegativeCacheTTL.Text(),
		Members: slices.Clone(d.Members), WriteMember: d.WriteMember, RoutingRule: d.RoutingRule,
		VersionPolicy: d.VersionPolicy, WritePolicy: d.WritePolicy, Depth: d.Depth, BlobStore: d.BlobStore,
		Distribution: d.Distribution, Component: d.Component, SigningKey: d.SigningKey, GPGPublicKey: d.GPGPublicKey,
		StrictContentTypeValidation: d.StrictContentTypeValidation, ContentDisposition: d.ContentDisposition,
		Blocked: d.Blocked, AutoBlock: d.AutoBlock, Firewall: d.Firewall}
	if d.AnonymousRead != nil {
		v := *d.AnonymousRead
		b.AnonymousRead = &v
	}
	cp, on := d.CleanupPolicy, d.IsOnline()
	b.CleanupPolicy, b.Online = &cp, &on
	if a := d.RemoteAuth; a != nil {
		b.RemoteAuth = &repoAuthRequest{Username: a.Username, Password: a.Password, BearerToken: a.BearerToken}
	}
	if r := d.Replication; r != nil {
		v := *r
		b.Replication = &v
	}
	ev := d.Evaluation // always given, so an apply also resets an override to inherit
	if ev == "" {
		ev = "inherit"
	}
	b.Evaluation = &ev
	if d.Type == "proxy" { // always given, so an apply also clears them ({} is none)
		on := d.NegativeCacheEnabled == nil || *d.NegativeCacheEnabled
		b.NegativeCacheEnabled = &on
		b.Connection = &config.Connection{}
		if c := config.CleanConnection(d.Connection); c != nil {
			b.Connection = c
		}
	}
	return b
}

func readRepositories(c *confRun) ([]any, error) {
	if err := c.u.Repos.Resync(c.ctx); err != nil { // what every node left, not a few seconds old
		return nil, err
	}
	var out []any
	for _, rp := range c.u.Repos.Snapshot().Sorted {
		out = append(out, repoRequestOf(rp.Repository))
	}
	return out, nil
}

// repoOrder puts every group after its members.
func repoOrder(objs []confObj) []confObj {
	by := map[string]confObj{}
	for _, o := range objs {
		by[o["name"].(string)] = o
	}
	done := map[string]bool{}
	var out []confObj
	var visit func(o confObj)
	visit = func(o confObj) {
		name := o["name"].(string)
		if done[name] {
			return
		}
		done[name] = true
		r := asConf[repoRequest](o)
		for _, m := range append(slices.Clone(r.Members), r.WriteMember) {
			if mo := by[m]; mo != nil {
				visit(mo)
			}
		}
		out = append(out, o)
	}
	for _, o := range objs {
		visit(o)
	}
	return out
}

func checkRepositories(c *confRun, objs []confObj) {
	var defs []config.Repository
	for _, o := range c.desired("repositories") {
		r := asConf[repoRequest](o)
		d, _, err := r.definition()
		if err != nil {
			c.bad("repositories %s: %v", r.Name, strings.Join(err.(*repo.ValidationError).Errors, "; "))
			continue
		}
		config.ApplyRepositoryDefaults(&d)
		defs = append(defs, d)
	}
	for _, o := range objs {
		r := asConf[repoRequest](o)
		if err := repo.CheckName("repository", r.Name); err != nil {
			c.bad("repositories %s: %v", r.Name, err)
		}
		if d, _, err := r.definition(); err == nil {
			config.ApplyRepositoryDefaults(&d)
			if err := config.ValidateDefinition(d); err != nil {
				c.bad("repositories %s: %v", r.Name, err)
			}
		}
		if cur, ok := c.cur["repositories"].objs[r.Name]; ok {
			cr := asConf[repoRequest](cur)
			if r.Format != cr.Format || r.Type != cr.Type {
				c.bad("repositories %s: format and type cannot be changed", r.Name)
			}
			if r.BlobStore != "" && r.Type != "group" && repo.StoreOf(config.Repository{BlobStore: r.BlobStore}) != repo.StoreOf(config.Repository{BlobStore: cr.BlobStore}) {
				c.bad("repositories %s: blobStore cannot be changed here (run the repository's change-blobstore move)", r.Name)
			}
		}
	}
	if err := config.ValidateRepositories(defs); err != nil {
		c.bad("repositories: %v", err)
	}
}

// ---- roles and users ----

func readRoles(c *confRun) ([]any, error) {
	list, err := c.u.DB.ListRoles(c.ctx)
	var out []any
	for _, x := range list {
		if x.ID == meta.AdminRole { // built in: not editable
			continue
		}
		d := roleJSON(x)
		out = append(out, confRole{ID: d.ID, Description: d.Description, Privileges: d.Privileges})
	}
	return out, err
}

func checkRoles(c *confRun, objs []confObj) {
	valid := map[string]bool{}
	for _, a := range meta.Actions {
		valid[a] = true
	}
	for _, o := range objs {
		x := asConf[confRole](o)
		at := "roles " + x.ID
		var v validation
		if err := checkRoleID(x.ID); err != nil {
			c.bad("%s: %v", at, err)
		} else if strings.EqualFold(x.ID, meta.AdminRole) {
			c.bad("%s: the built-in admin role is not part of the document", at)
		}
		checkText(&v, "description", x.Description)
		for i, p := range x.Privileges {
			pat := fmt.Sprintf("%s privilege %d", at, i+1)
			switch {
			case p.Target == "*":
			case strings.HasPrefix(p.Target, "format:"):
				if !contains(accessFormats, strings.TrimPrefix(p.Target, "format:")) {
					v.add("%s: unknown format in %q", pat, p.Target)
				}
			case strings.HasPrefix(p.Target, "repo:"), strings.HasPrefix(p.Target, meta.SelectorPrefix):
			case p.Target == auth.MetricsTarget: // as the role API checks it (access.go)
				if len(p.Actions) != 1 || p.Actions[0] != meta.ActionRead {
					v.add(`%s: the %q target takes the action "read" only`, pat, auth.MetricsTarget)
				}
			default:
				v.add(`%s: target %q must be "*", "format:<format>", "repo:<name>", "selector:<name>" or %q`, pat, p.Target, auth.MetricsTarget)
			}
			for _, a := range p.Actions {
				if !valid[a] {
					v.add("%s: unknown action %q", pat, a)
				}
			}
		}
		c.errs = append(c.errs, v...)
	}
}

func readUsers(c *confRun) ([]any, error) {
	list, err := c.u.DB.ListUsers(c.ctx)
	var out []any
	for _, x := range list {
		u := confUserRead{confUser: confUser{ID: x.ID, Name: x.Name, Email: x.Email, Source: x.Source, Status: x.Status, Roles: nonNil(x.Roles)}}
		if x.Source == meta.SourceLocal {
			u.PasswordHash = x.PasswordHash
		}
		out = append(out, u)
	}
	return out, err
}

func checkUsers(c *confRun, objs []confObj) {
	for _, o := range objs {
		x := asConf[confUser](o)
		at := "users " + x.ID
		var v validation
		if err := checkUserID(x.ID); err != nil {
			v = append(v, err.(*repo.ValidationError).Errors...)
		}
		if x.Source == "" {
			x.Source, o["source"] = meta.SourceLocal, meta.SourceLocal
		}
		if x.Status == "" {
			x.Status, o["status"] = meta.StatusActive, meta.StatusActive
		}
		switch x.Source {
		case meta.SourceLocal, meta.SourceLDAP, meta.SourceOIDC, meta.SourceCrowd, meta.SourceRUT, meta.SourceSAML:
		default:
			v.add("unknown source %q", x.Source)
		}
		if x.Status != meta.StatusActive && x.Status != meta.StatusDisabled {
			v.add("status must be %q or %q", meta.StatusActive, meta.StatusDisabled)
		}
		checkText(&v, "name", x.Name)
		checkText(&v, "email", x.Email)
		checkEmail(&v, x.Email)
		if h := c.want["users"].sec[x.ID]["passwordHash"]; h != "" && (x.Source != meta.SourceLocal || !auth.HashUsable(h)) {
			v.add("passwordHash must be a bcrypt hash, of a local user")
		}
		if cur, ok := c.cur["users"].objs[x.ID]; ok && cur["source"] != x.Source {
			v.add("source cannot be changed")
		}
		for _, msg := range v {
			c.bad("%s: %s", at, msg)
		}
	}
}

// writeUser goes through the users API; a local user is created with a
// random password, then gets the document's hash (or none).
func writeUser(c *confRun, st *ConfigStep) error {
	base := "/-/api/users"
	o := asConf[confUser](st.want)
	local := o.Source == meta.SourceLocal
	switch st.Action {
	case confCreate:
		b := deepCopy(st.want).(map[string]any)
		if local {
			b["password"] = randomPassword()
		}
		if err := c.call(http.MethodPost, base, b, http.StatusCreated); err != nil {
			return err
		}
		if local {
			if err := c.setPasswordHash(st.Key, st.wantSec["passwordHash"]); err != nil {
				c.call(http.MethodDelete, base+"/"+url.PathEscape(st.Key), nil, http.StatusNoContent) // the step fails whole
				return err
			}
		}
		return nil
	case confUpdate:
		if slices.ContainsFunc(st.Changes, func(ch ConfigChange) bool { return !ch.Secret }) {
			b := confObj{"name": o.Name, "email": o.Email, "status": o.Status, "roles": nonNil(o.Roles)}
			if err := c.call(http.MethodPut, base+"/"+url.PathEscape(st.Key), b, http.StatusOK); err != nil {
				return err
			}
		}
		if h := st.wantSec["passwordHash"]; local && h != st.curSec["passwordHash"] && (h != "" || st.undo) {
			return c.setPasswordHash(st.Key, h)
		}
		return nil
	}
	return c.call(http.MethodDelete, base+"/"+url.PathEscape(st.Key), nil, http.StatusNoContent)
}

// setPasswordHash stores a local user's password hash ("" for none), as
// the password reset does: under the access lock, refused when it would
// lock every admin out, then reloaded, the user's sessions ended and the
// event recorded.
func (c *confRun) setPasswordHash(id, hash string) error {
	u := c.u
	unlock, err := u.DB.Lock(c.ctx, accessLock)
	if err != nil {
		return err
	}
	defer unlock()
	cur, err := u.DB.GetUser(c.ctx, id)
	if err != nil {
		return err
	}
	if cur.Source != meta.SourceLocal {
		return &confCallError{status: http.StatusBadRequest, msg: "only a local user has a password"}
	}
	cur.PasswordHash = hash
	err = u.checkLockout(c.ctx, func(users []meta.User) []meta.User {
		for i := range users {
			if users[i].ID == id {
				users[i] = cur
			}
		}
		return users
	})
	if err == nil {
		err = u.DB.UpdateUser(c.ctx, cur)
	}
	if err != nil {
		if errors.Is(err, auth.ErrLockout) {
			return &confCallError{status: http.StatusConflict, msg: lockoutMessage}
		}
		return err
	}
	if err := u.reloadAccess(c.ctx); err != nil {
		return err
	}
	if err := u.Auth.OnUserChanged(id); err != nil {
		return err
	}
	u.emit(c.request(http.MethodPut, "/-/api/users/"+id+"/password"), "user.password", "", id, "configuration")
	return nil
}

// ---- general settings and realms ----

func readSettings(c *confRun) ([]any, error) {
	return c.setting(func(s meta.Settings) any {
		h := s.HTTP
		return confObj{"baseURL": s.BaseURL, "anonymousRead": s.AnonymousRead, "authenticatedRead": s.AuthenticatedRead,
			"sessionIdleMinutes": s.SessionIdleMinutes, "maxUploadMiB": s.MaxUploadMiB, "defaultLanguage": s.DefaultLanguage,
			"metricsAnonymous": s.MetricsAnonymous, "terraformDefaultRepository": s.TerraformDefaultRepository,
			"userTokens": s.UserTokens, "defaultRoles": nonNil(s.DefaultRoles),
			"http": confObj{"userAgentSuffix": h.UserAgentSuffix, "connectTimeoutSeconds": h.ConnectTimeoutSeconds,
				"timeoutSeconds": h.TimeoutSeconds, "retries": h.Retries,
				"proxy": confObj{"host": h.Proxy.Host, "port": h.Proxy.Port, "username": h.Proxy.Username, "password": h.Proxy.Password,
					"nonProxyHosts": nonNil(h.Proxy.NonProxyHosts)}}}
	})
}

func checkSettings(c *confRun, objs []confObj) {
	b := asConf[settingsRequest](objs[0])
	try := func(err error) {
		if err != nil {
			c.bad("settings: %v", err)
		}
	}
	if b.BaseURL != nil {
		try(meta.ValidateBaseURL(strings.TrimRight(strings.TrimSpace(*b.BaseURL), "/")))
	}
	if b.SessionIdleMinutes != nil {
		try(meta.ValidateSessionIdleMinutes(*b.SessionIdleMinutes))
	}
	if b.MaxUploadMiB != nil {
		try(meta.ValidateMaxUploadMiB(*b.MaxUploadMiB))
	}
	if b.DefaultLanguage != nil {
		try(meta.ValidateDefaultLanguage(*b.DefaultLanguage))
	}
	if h := b.HTTP; h != nil {
		try(meta.ValidateHTTP(meta.HTTP{UserAgentSuffix: strings.TrimSpace(h.UserAgentSuffix), ConnectTimeoutSeconds: h.ConnectTimeoutSeconds,
			TimeoutSeconds: h.TimeoutSeconds, Retries: h.Retries, Proxy: meta.HTTPProxy{Host: strings.TrimSpace(h.Proxy.Host),
				Port: h.Proxy.Port, Username: h.Proxy.Username, NonProxyHosts: h.Proxy.NonProxyHosts}}))
	}
	if b.UserTokens != nil {
		try(meta.ValidateUserTokens(*b.UserTokens))
	}
	if b.DefaultRoles != nil && contains(*b.DefaultRoles, meta.AdminRole) {
		c.bad("settings defaultRoles: the admin role cannot be a default role")
	}
}

func checkSMTP(c *confRun, objs []confObj) {
	if err := meta.ValidateSMTP(asConf[meta.SMTP](objs[0])); err != nil {
		c.bad("smtp: %v", err)
	}
}

// ldapMerged is the configuration putLDAP stores for body (without the bind password).
func ldapMerged(body ldapRequest) meta.LDAP {
	def := meta.DefaultLDAP()
	next := meta.LDAP{URL: strings.TrimSpace(body.URL), StartTLS: body.StartTLS, InsecureSkipVerify: body.InsecureSkipVerify,
		BindDN: strings.TrimSpace(body.BindDN), TimeoutSeconds: body.TimeoutSeconds, UserBaseDN: strings.TrimSpace(body.UserBaseDN),
		UserFilter: orDefault(body.UserFilter, def.UserFilter), UserIDAttr: orDefault(body.UserIDAttr, def.UserIDAttr),
		NameAttr: orDefault(body.NameAttr, def.NameAttr), EmailAttr: orDefault(body.EmailAttr, def.EmailAttr),
		GroupAttr: orDefault(body.GroupAttr, def.GroupAttr), GroupMappings: body.GroupMappings, DefaultRoles: body.DefaultRoles}
	if next.TimeoutSeconds == 0 {
		next.TimeoutSeconds = def.TimeoutSeconds
	}
	return next
}

func checkLDAP(c *confRun, objs []confObj) {
	next := ldapMerged(asConf[ldapRequest](objs[0]))
	next.BindPassword = "kept" // the stored one, or the document's: putLDAP checks it
	if err := meta.ValidateLDAP(next); err != nil {
		c.bad("ldap: %v", err)
	}
	if _, err := ldap.CompileFilter(ldap.UserFilter(next.UserFilter, "x")); err != nil {
		c.bad("ldap userFilter: %s", strings.TrimPrefix(err.Error(), "ldap filter: "))
	}
}

func checkOIDC(c *confRun, objs []confObj) {
	next := asConf[oidcRequest](objs[0]).merged(meta.OIDC{})
	if err := meta.ValidateOIDC(next); err != nil {
		c.bad("oidc: %v", err)
	}
}

func checkSAML(c *confRun, objs []confObj) {
	next := asConf[samlRequest](objs[0]).merged(meta.SAML{})
	if err := meta.ValidateSAML(next); err != nil {
		c.bad("saml: %v", err)
	}
	if next.IdPMetadataXML != "" {
		if _, err := saml.ParseMetadata([]byte(next.IdPMetadataXML)); err != nil {
			c.bad("saml idpMetadataXML: %v", err)
		}
	}
	sec := c.want["saml"].sec[""]
	if sec["spPrivateKey"] != "" || sec["spCertificate"] != "" {
		if _, _, err := saml.ParseKeyPair(sec["spPrivateKey"], sec["spCertificate"]); err != nil {
			c.bad("saml service provider key pair: %v", err)
		}
	}
}

// writeSAML saves the configuration, then the document's key pair, if it
// differs from the stored one.
func writeSAML(c *confRun, st *ConfigStep) error {
	if slices.ContainsFunc(st.Changes, func(ch ConfigChange) bool { return !ch.Secret }) || st.cur == nil {
		if err := c.call(http.MethodPut, st.s.base, c.body(st), http.StatusOK); err != nil {
			return err
		}
	}
	key, cert := st.wantSec["spPrivateKey"], st.wantSec["spCertificate"]
	if key == "" || key == st.curSec["spPrivateKey"] && cert == st.curSec["spCertificate"] {
		return nil
	}
	u := c.u
	unlock, err := u.DB.Lock(c.ctx, accessLock)
	if err != nil {
		return err
	}
	defer unlock()
	set, err := u.DB.LoadSettings(c.ctx)
	if err != nil {
		return err
	}
	set.SAML.SPPrivateKey, set.SAML.SPCertificate = key, cert
	if err := u.DB.SetSettingJSON(c.ctx, meta.SettingSAML, set.SAML); err != nil {
		return err
	}
	if err := u.reloadAccess(c.ctx); err != nil {
		return err
	}
	u.saml.reset()
	u.emit(c.request(http.MethodPut, st.s.base), "realms.saml.update", "", "", "spKey")
	return nil
}

// crowdMerged is the configuration putCrowd stores for body (without the password).
func crowdMerged(body crowdRequest) meta.Crowd {
	def := meta.DefaultCrowd()
	next := meta.Crowd{URL: strings.TrimRight(strings.TrimSpace(body.URL), "/"), ApplicationName: strings.TrimSpace(body.ApplicationName),
		TimeoutSeconds: body.TimeoutSeconds, CacheMinutes: body.CacheMinutes, GroupMappings: body.GroupMappings, DefaultRoles: body.DefaultRoles}
	if next.TimeoutSeconds == 0 {
		next.TimeoutSeconds = def.TimeoutSeconds
	}
	if next.CacheMinutes == 0 {
		next.CacheMinutes = def.CacheMinutes
	}
	return next
}

func checkCrowd(c *confRun, objs []confObj) {
	next := crowdMerged(asConf[crowdRequest](objs[0]))
	next.ApplicationPassword = "kept"
	if err := meta.ValidateCrowd(next); err != nil {
		c.bad("crowd: %v", err)
	}
}

func checkRUT(c *confRun, objs []confObj) {
	b := asConf[meta.RUT](objs[0])
	b.Header = orDefault(b.Header, meta.DefaultRUT().Header)
	if err := meta.ValidateRUT(b); err != nil {
		c.bad("rutauth: %v", err)
	}
}

func checkRealms(c *confRun, objs []confObj) {
	if err := meta.ValidateRealms(asConf[confRealms](objs[0]).Realms); err != nil {
		c.bad("realms: %v", err)
	}
}

// checkRealmConfigs requires every realm enabled after the apply to have a
// complete configuration, as PUT /-/api/realms and the realm PUTs do.
func (c *confRun) checkRealmConfigs() {
	list, _ := desiredOne[confRealms](c, "realms")
	for _, r := range list.Realms {
		if !r.Enabled {
			continue
		}
		complete := true
		switch r.ID {
		case meta.RealmLDAP:
			b, _ := desiredOne[ldapRequest](c, "ldap")
			complete = ldapMerged(b).Complete()
		case meta.RealmOIDC:
			b, _ := desiredOne[oidcRequest](c, "oidc")
			complete = b.merged(meta.OIDC{}).Complete()
		case meta.RealmCrowd:
			b, _ := desiredOne[crowdRequest](c, "crowd")
			complete = crowdMerged(b).Complete()
		case meta.RealmRUT:
			b, _ := desiredOne[meta.RUT](c, "rutauth")
			b.Header = orDefault(b.Header, meta.DefaultRUT().Header)
			complete = b.Complete()
		case meta.RealmSAML:
			b, _ := desiredOne[samlRequest](c, "saml")
			complete = b.merged(meta.SAML{}).Complete()
		}
		if !complete {
			c.bad("realms: the %s realm is enabled but its configuration would be incomplete", r.ID)
		}
	}
}

// ---- tasks and webhooks ----

// taskKey is the built-in compaction task's id, any other task's name.
func taskKey(o confObj) string {
	if o["builtin"] == true {
		return tasks.SeedID
	}
	k, _ := o["name"].(string)
	return k
}

func readTasks(c *confRun) ([]any, error) {
	list, err := c.u.Tasks.List(c.ctx)
	var out []any
	for _, t := range list {
		o := confObj{"name": t.Name, "type": t.Type, "schedule": t.Schedule, "params": json.RawMessage(t.Params),
			"enabled": t.Enabled, "notifyEmail": nonNil(t.NotifyEmail)}
		if !json.Valid([]byte(t.Params)) {
			o["params"] = json.RawMessage("{}")
		}
		if t.ID == tasks.SeedID {
			o["builtin"] = true
		}
		out = append(out, o)
	}
	return out, err
}

func taskID(c *confRun, key string) (string, error) {
	if key == tasks.SeedID {
		return key, nil
	}
	list, err := c.u.Tasks.List(c.ctx)
	if err != nil {
		return "", err
	}
	for _, t := range list {
		if t.Name == key && t.ID != tasks.SeedID {
			return t.ID, nil
		}
	}
	return "", &confCallError{status: http.StatusNotFound, msg: fmt.Sprintf("no task is named %q", key)}
}

func checkTasks(c *confRun, objs []confObj) {
	for _, o := range objs {
		d := asConf[tasks.Def](o)
		at := "tasks " + d.Name
		builtin := o["builtin"] == true
		switch {
		case builtin && d.Type != tasks.TypeCompact:
			c.bad("%s: the built-in task is of type %s", at, tasks.TypeCompact)
		case !builtin && d.Name == tasks.SeedID:
			c.bad("%s: the name is the built-in task's id", at)
		case !slices.Contains(tasks.Types(), d.Type):
			c.bad("%s: unknown type %q", at, d.Type)
		}
		if cur, ok := c.cur["tasks"].objs[taskKey(o)]; ok && cur["type"] != d.Type {
			c.bad("%s: the type of a task cannot change", at)
		}
	}
}

func readWebhooks(c *confRun) ([]any, error) {
	list, err := c.u.DB.ListWebhooks(c.ctx)
	var out []any
	for _, h := range list {
		secret, enabled := h.Secret, h.Enabled
		out = append(out, webhookRequest{Name: h.Name, URL: h.URL, Secret: &secret, Events: nonNil(h.Events), Repo: h.Repo, Enabled: &enabled})
	}
	return out, err
}

func webhookID(c *confRun, key string) (string, error) {
	list, err := c.u.DB.ListWebhooks(c.ctx)
	if err != nil {
		return "", err
	}
	for _, h := range list {
		if h.Name == key {
			return h.ID, nil
		}
	}
	return "", &confCallError{status: http.StatusNotFound, msg: fmt.Sprintf("no webhook is named %q", key)}
}

func checkWebhooks(c *confRun, objs []confObj) {
	for _, o := range objs {
		b := asConf[webhookRequest](o)
		b.Repo = "" // checkRefs
		_, errs := c.u.validateWebhook(b)
		for _, e := range errs {
			c.bad("webhooks %s: %s", b.Name, e)
		}
	}
}

// ---- references ----

// checkRefs checks the references between the sections in their state
// after the apply: an object the document lists must name objects that will
// exist, and the apply must not delete an object another one still names.
// A reference both sides already had dangling (an older version's) is
// left alone.
func (c *confRun) checkRefs() {
	ref := func(at, from, fromKey, to, toKey string) {
		if toKey == "" || to == "roles" && toKey == meta.AdminRole || c.has(to, toKey) {
			return
		}
		_, listed := c.wantObj(from, fromKey)
		_, stored := c.curObj(to, toKey)
		if listed || stored {
			c.bad("%s: %s %q would not exist", at, to, toKey)
		}
	}
	roleList := func(at, from, fromKey string, ids []string) {
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				c.bad("%s: role %q is listed twice", at, id)
			}
			seen[id] = true
			ref(at, from, fromKey, "roles", id)
		}
	}
	for k, o := range c.desired("repositories") {
		r := asConf[repoRequest](o)
		at := "repositories " + k
		if r.Type != "group" {
			ref(at, "repositories", k, "blobStores", repo.StoreOf(config.Repository{BlobStore: r.BlobStore}))
		}
		ref(at, "repositories", k, "routingRules", r.RoutingRule)
		if r.CleanupPolicy != nil {
			ref(at, "repositories", k, "cleanupPolicies", *r.CleanupPolicy)
		}
		for _, m := range append(slices.Clone(r.Members), r.WriteMember) {
			ref(at, "repositories", k, "repositories", m)
		}
	}
	for k, o := range c.desired("blobStores") {
		for _, m := range asConf[blobStoreRequest](o).Members {
			ref("blobStores "+k, "blobStores", k, "blobStores", m.Name)
		}
	}
	for k, o := range c.desired("roles") {
		for _, p := range asConf[confRole](o).Privileges {
			if name, ok := strings.CutPrefix(p.Target, "repo:"); ok {
				ref("roles "+k, "roles", k, "repositories", name)
			}
			if name, ok := strings.CutPrefix(p.Target, meta.SelectorPrefix); ok {
				ref("roles "+k, "roles", k, "contentSelectors", name)
				for _, rn := range p.Repos {
					if rn != "*" {
						ref("roles "+k, "roles", k, "repositories", rn)
					}
				}
			}
		}
	}
	for k, o := range c.desired("users") {
		roleList("users "+k, "users", k, asConf[confUser](o).Roles)
	}
	if b, ok := desiredOne[settingsRequest](c, "settings"); ok {
		if b.DefaultRoles != nil {
			roleList("settings defaultRoles", "settings", "", *b.DefaultRoles)
		}
		if b.TerraformDefaultRepository != nil {
			name := strings.TrimSpace(*b.TerraformDefaultRepository)
			ref("settings terraformDefaultRepository", "settings", "", "repositories", name)
			if o := c.desired("repositories")[name]; o != nil && o["format"] != "terraform" {
				c.bad("settings terraformDefaultRepository: %q is not a terraform repository", name)
			}
		}
	}
	mappings := func(name string, defaults []string, maps []meta.GroupMapping) {
		roleList(name+" defaultRoles", name, "", defaults)
		for i, m := range maps {
			roleList(fmt.Sprintf("%s group mapping %d", name, i+1), name, "", m.Roles)
		}
	}
	if b, ok := desiredOne[ldapRequest](c, "ldap"); ok {
		mappings("ldap", b.DefaultRoles, b.GroupMappings)
	}
	if b, ok := desiredOne[oidcRequest](c, "oidc"); ok {
		mappings("oidc", b.DefaultRoles, b.GroupMappings)
	}
	if b, ok := desiredOne[samlRequest](c, "saml"); ok {
		mappings("saml", b.DefaultRoles, b.GroupMappings)
	}
	if b, ok := desiredOne[crowdRequest](c, "crowd"); ok {
		mappings("crowd", b.DefaultRoles, b.GroupMappings)
	}
	if b, ok := desiredOne[meta.RUT](c, "rutauth"); ok {
		mappings("rutauth", b.DefaultRoles, b.GroupMappings)
	}
	for k, o := range c.desired("webhooks") {
		if name, _ := o["repo"].(string); name != "" {
			ref("webhooks "+k, "webhooks", k, "repositories", name)
		}
	}
	for _, n := range []string{"realms", "ldap", "oidc", "saml", "crowd", "rutauth"} {
		if c.want[n] != nil {
			c.checkRealmConfigs()
			break
		}
	}
}

func (c *confRun) wantObj(section, key string) (confObj, bool) {
	if w := c.want[section]; w != nil {
		o, ok := w.objs[key]
		return o, ok
	}
	return nil, false
}

func (c *confRun) curObj(section, key string) (confObj, bool) {
	if s := c.cur[section]; s != nil {
		o, ok := s.objs[key]
		return o, ok
	}
	return nil, false
}
