package ui

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"commandcenter/internal/auth"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// Configuration as code. GET /-/api/configuration exports every
// admin-configurable setting and object as one YAML or JSON document; POST
// /-/api/configuration applies one. An apply checks the whole document
// first, then changes the objects in dependency order (blob stores before
// repositories, roles before users, ...) through the same /-/api/ handlers
// an admin uses, called in-process as the caller (as the Nexus API does), so
// the validation, locks, lockout checks, sealing, events and the
// notifications to the other nodes are theirs. Every handler commits on its
// own, so one database transaction cannot hold them all: when a step fails,
// the steps already done are undone in reverse order with the same handlers
// (the stored values, secrets included, were read before) and the answer
// says what happened to each.
//
// A document's sections are the keys of confSections. A section left out
// is not touched; prune deletes the objects a present section does not
// list. Secrets are left out unless exported encrypted (confcode_crypt.go);
// a document without them keeps the stored ones.

const (
	confVersion = 1
	// confMaxBody bounds an applied document (thousands of repositories fit).
	confMaxBody = 8 << 20
	// confLock serialises applies across the cluster (meta.DB.Lock). It is
	// taken before the handlers take the access and registry locks.
	confLock = "configuration"
	// ConfigPassphraseHeader carries the passphrase of the secrets block.
	ConfigPassphraseHeader = "X-Config-Passphrase"
	// confActor is the principal of the CLI's export and apply: not a valid
	// user id, so it never names a stored user.
	confActor = "cli:config"
)

// Step actions and statuses.
const (
	confCreate    = "create"
	confUpdate    = "update"
	confDelete    = "delete"
	confUnchanged = "unchanged"
)

type confObj = map[string]any

// confSecret is a secret field of a section's request body (dotted path).
// Export moves it out of the object; apply puts it back when the document
// carries it, and clears it (clear, or path = "") when the document's
// secrets block is authoritative and lacks it.
type confSecret struct {
	path    string
	clear   string
	noClear bool                // a missing value always keeps the stored one
	own     bool                // set by the section's write, not part of the request body
	when    func(v string) bool // nil: every non-empty value is the secret
	show    func(v string) string
}

// confSection describes one section of the document.
type confSection struct {
	name    string
	single  bool   // one object; otherwise a list of objects with a key
	key     string // list: the field holding the object's key
	base    string // the API path: single PUT; list POST base, PUT/DELETE base/{id}
	proto   func() any
	extra   []string // document fields that are not the request body's
	secrets []confSecret
	// keep lists fields whose null on update keeps the stored value; with
	// partial (single only) every field does, and PUT sends only the
	// changed ones.
	keep        []string
	partial     bool
	noUpdateKey bool // the PUT body has no key field
	// canon is the request the handler would store for o (its defaults
	// applied), so a document that leaves a field out compares as the
	// handler reads it; nil when o does not convert.
	canon   func(o confObj) any
	read    func(c *confRun) ([]any, error)
	check   func(c *confRun, objs []confObj)
	write   func(c *confRun, st *ConfigStep) error // nil: the generic POST/PUT/DELETE
	keyOf   func(o confObj) string                 // nil: o[key]
	order   func(objs []confObj) []confObj         // dependency order; nil: by key
	idOf    func(c *confRun, key string) (string, error)
	keepDel func(key string) bool // never deleted by prune
	avail   func(u *UI) bool      // nil: always
}

// ConfigOptions select what ExportConfig and ApplyConfig do.
type ConfigOptions struct {
	Format           string // export: "yaml" (default) or "json"
	Include, Exclude []string
	Secrets          bool   // export: add the secrets, encrypted with Passphrase
	Passphrase       string // the secrets block's passphrase (export and apply)
	Passwords        bool   // export: the local users' password hashes
	DryRun           bool   // apply: plan only
	Prune            bool   // apply: delete what a present section does not list
	CreateOnly       bool   // apply: create missing objects only (Nexus's upload)
	RequestID        string

	noSecrets bool // the caller may not compare secrets (an admin token's dry run)
}

// ConfigStep is one object of the plan.
type ConfigStep struct {
	Section  string         `json:"section"`
	Key      string         `json:"key,omitempty"`
	Action   string         `json:"action"`
	Changes  []ConfigChange `json:"changes,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
	// Status is set by an apply: applied, failed, rolledBack,
	// rollbackFailed, notAttempted or skipped (createOnly).
	Status     string   `json:"status,omitempty"`
	Error      string   `json:"error,omitempty"`
	Errors     []string `json:"errors,omitempty"`
	HTTPStatus int      `json:"httpStatus,omitempty"`

	s               *confSection
	cur, want       confObj
	curSec, wantSec map[string]string
	authoritative   bool // wantSec is complete: clear the secrets it lacks
	undo            bool // a rollback: restore cur exactly
}

// ConfigChange is one field of an update. A secret's values are never
// shown: Secret is set instead.
type ConfigChange struct {
	Field  string          `json:"field"`
	From   json.RawMessage `json:"from,omitempty"`
	To     json.RawMessage `json:"to,omitempty"`
	Secret bool            `json:"secret,omitempty"`
}

// ConfigResult is the answer of an apply or dry run.
type ConfigResult struct {
	DryRun   bool           `json:"dryRun"`
	Prune    bool           `json:"prune"`
	OK       bool           `json:"ok"`
	Summary  map[string]int `json:"summary"`
	Steps    []*ConfigStep  `json:"steps"`
	Warnings []string       `json:"warnings,omitempty"`
	// Error names the step that failed; RolledBack whether every step done
	// before it was undone.
	Error      string `json:"error,omitempty"`
	RolledBack bool   `json:"rolledBack,omitempty"`
}

// confState is a section's objects by key, with their secrets.
type confState struct {
	objs map[string]confObj
	sec  map[string]map[string]string
	keys []string  // in read or document order
	dups []confObj // stored objects whose key another one has (task and webhook names)
}

func newConfState() *confState {
	return &confState{objs: map[string]confObj{}, sec: map[string]map[string]string{}}
}

// confRun is one export or apply.
type confRun struct {
	u     *UI
	ctx   context.Context
	opts  ConfigOptions
	cur   map[string]*confState
	want  map[string]*confState // the document's selected sections
	given bool                  // the document had a secrets block
	errs  []string
	warns []string
	set   *meta.Settings // the stored settings, read once
	memo  map[string]map[string]confObj
}

func (c *confRun) bad(format string, args ...any) {
	c.errs = append(c.errs, fmt.Sprintf(format, args...))
}

// ---- routes ----

func (u *UI) configRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/configuration", u.getConfiguration)
	mux.HandleFunc("POST /-/api/configuration", u.postConfiguration)
}

func confList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// GET /-/api/configuration?format=yaml|json&include=&exclude=&secrets=omit|encrypted&passwords=true
func (u *UI) getConfiguration(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := ConfigOptions{Format: q.Get("format"), Include: confList(q.Get("include")), Exclude: confList(q.Get("exclude")),
		Passphrase: r.Header.Get(ConfigPassphraseHeader), Passwords: q.Get("passwords") == "true", RequestID: r.Header.Get("X-Request-Id")}
	switch q.Get("secrets") {
	case "", "omit":
	case "encrypted":
		opts.Secrets = true
	default:
		writeError(w, http.StatusBadRequest, `secrets must be "omit" or "encrypted"`)
		return
	}
	if opts.Secrets || opts.Passwords {
		if !requireAdminInteractive(w, r) {
			return
		}
	} else if _, ok := requireAdmin(w, r); !ok {
		return
	}
	b, err := u.ExportConfig(r.Context(), opts)
	var ve *repo.ValidationError
	if errors.As(err, &ve) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": ve.Errors[0], "errors": ve.Errors})
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	if opts.Secrets || opts.Passwords {
		u.emit(r, "configuration.export", "", "configuration", fmt.Sprintf("secrets %t, passwords %t", opts.Secrets, opts.Passwords))
	}
	name, ctype := "commandcenter-config.yaml", "application/yaml; charset=utf-8"
	if opts.Format == "json" {
		name, ctype = "commandcenter-config.json", "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(b)
}

// POST /-/api/configuration?dryRun=true&prune=true&createOnly=true&include=&exclude=
// takes the document (YAML or JSON) as the body.
func (u *UI) postConfiguration(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := ConfigOptions{Include: confList(q.Get("include")), Exclude: confList(q.Get("exclude")),
		Passphrase: r.Header.Get(ConfigPassphraseHeader), DryRun: q.Get("dryRun") == "true", Prune: q.Get("prune") == "true",
		CreateOnly: q.Get("createOnly") == "true", RequestID: r.Header.Get("X-Request-Id")}
	// A dry run changes nothing: an admin token may run one (drift checks),
	// of a document without secrets (comparing them is the interactive
	// admin's). An apply is an interactive admin's change.
	if opts.DryRun {
		p, ok := requireAdmin(w, r)
		if !ok {
			return
		}
		opts.noSecrets = p.Via != auth.ViaPassword && p.Via != auth.ViaSession && p.Via != auth.ViaRUT
	} else if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, confMaxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "the document is larger than "+strconv.Itoa(confMaxBody>>20)+" MiB")
		return
	}
	res, err := u.ApplyConfig(r.Context(), body, opts)
	var ve *repo.ValidationError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": ve.Errors[0], "errors": ve.Errors})
	case err != nil:
		u.fail(w, r, err)
	case !res.OK:
		writeJSON(w, http.StatusConflict, res)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

// ---- export ----

// ExportConfig renders the configuration as the principal in ctx sees it.
// The CLI calls it with an admin principal (WithConfigActor).
func (u *UI) ExportConfig(ctx context.Context, opts ConfigOptions) ([]byte, error) {
	if opts.Format != "" && opts.Format != "yaml" && opts.Format != "json" {
		return nil, &repo.ValidationError{Errors: []string{`format must be "yaml" or "json"`}}
	}
	if opts.Secrets {
		if err := checkPassphrase(opts.Passphrase); err != nil {
			return nil, &repo.ValidationError{Errors: []string{err.Error()}}
		}
	}
	sel, err := confSelect(opts)
	if err != nil {
		return nil, err
	}
	c := &confRun{u: u, ctx: ctx, opts: opts}
	doc := []confKV{{"version", confVersion}}
	payload := map[string]map[string]map[string]string{}
	for _, s := range confSections() {
		if !sel[s.name] || !s.available(u) {
			continue
		}
		st, err := c.read(s, false)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.name, err)
		}
		var objs []any
		for _, k := range st.keys {
			o := st.objs[k]
			if s.name == "users" && opts.Passwords && st.sec[k]["passwordHash"] != "" {
				o["passwordHash"] = st.sec[k]["passwordHash"]
			}
			objs = append(objs, o)
			for p, v := range st.sec[k] {
				if !opts.Secrets || p == "passwordHash" {
					continue
				}
				if payload[s.name] == nil {
					payload[s.name] = map[string]map[string]string{}
				}
				if payload[s.name][k] == nil {
					payload[s.name][k] = map[string]string{}
				}
				payload[s.name][k][p] = v
			}
		}
		for _, o := range st.dups {
			objs = append(objs, o)
		}
		if s.single {
			if len(objs) == 1 {
				doc = append(doc, confKV{s.name, objs[0]})
			}
			continue
		}
		if objs == nil {
			objs = []any{}
		}
		doc = append(doc, confKV{s.name, objs})
	}
	if opts.Secrets {
		box, err := sealConfigSecrets(payload, opts.Passphrase)
		if err != nil {
			return nil, err
		}
		doc = append(doc, confKV{"secrets", box})
	}
	return renderConf(doc, opts.Format)
}

// confKV keeps the document's top-level order.
type confKV struct {
	k string
	v any
}

// renderConf writes doc as indented JSON, or as YAML in the same order.
func renderConf(doc []confKV, format string) ([]byte, error) {
	var raw bytes.Buffer
	raw.WriteByte('{')
	for i, kv := range doc {
		if i > 0 {
			raw.WriteByte(',')
		}
		k, _ := json.Marshal(kv.k)
		v, err := json.Marshal(kv.v)
		if err != nil {
			return nil, err
		}
		raw.Write(k)
		raw.WriteByte(':')
		raw.Write(v)
	}
	raw.WriteByte('}')
	if format == "json" {
		var out bytes.Buffer
		if err := json.Indent(&out, raw.Bytes(), "", "  "); err != nil {
			return nil, err
		}
		out.WriteByte('\n')
		return out.Bytes(), nil
	}
	// JSON is YAML: parsed into a node it keeps the key order; plain styles
	// then make it block YAML (the encoder quotes what would read back as
	// another type, and writes multi-line strings as literal blocks).
	var n yaml.Node
	if err := yaml.Unmarshal(raw.Bytes(), &n); err != nil {
		return nil, err
	}
	var plain func(*yaml.Node)
	plain = func(n *yaml.Node) {
		n.Style = 0
		for _, c := range n.Content {
			plain(c)
		}
	}
	plain(&n)
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return nil, err
	}
	enc.Close()
	return out.Bytes(), nil
}

// confSelect returns the selected section names.
func confSelect(opts ConfigOptions) (map[string]bool, error) {
	known := map[string]bool{}
	for _, s := range confSections() {
		known[s.name] = true
	}
	var errs []string
	for _, n := range append(slices.Clone(opts.Include), opts.Exclude...) {
		if !known[n] {
			errs = append(errs, fmt.Sprintf("unknown section %q", n))
		}
	}
	if len(errs) > 0 {
		return nil, &repo.ValidationError{Errors: errs}
	}
	sel := map[string]bool{}
	for n := range known {
		sel[n] = len(opts.Include) == 0 || slices.Contains(opts.Include, n)
	}
	for _, n := range opts.Exclude {
		sel[n] = false
	}
	return sel, nil
}

func (s *confSection) available(u *UI) bool { return s.avail == nil || s.avail(u) }

// read returns the section's stored objects in document form, their
// secrets split off. Duplicate keys are an error for an apply (strict).
func (c *confRun) read(s *confSection, strict bool) (*confState, error) {
	raw, err := s.read(c)
	if err != nil {
		return nil, err
	}
	st := newConfState()
	for _, v := range raw {
		o, err := toConfObj(v)
		if err != nil {
			return nil, err
		}
		sec := s.split(o)
		if o, err = s.reshape(o, false); err != nil {
			return nil, err
		}
		k := s.keyFor(o)
		if _, dup := st.objs[k]; dup {
			if strict {
				c.bad("%s: two stored objects are named %q: rename one before applying a document with this section", s.name, k)
			}
			st.dups = append(st.dups, o) // exported as they are; an apply refuses them
			continue
		}
		st.objs[k], st.sec[k] = o, sec
		st.keys = append(st.keys, k)
	}
	if !s.single {
		sort.Strings(st.keys)
		objs := make([]confObj, 0, len(st.keys))
		for _, k := range st.keys {
			objs = append(objs, st.objs[k])
		}
		st.keys = st.keys[:0]
		for _, o := range s.ordered(objs) {
			st.keys = append(st.keys, s.keyFor(o))
		}
	}
	return st, nil
}

func (s *confSection) keyFor(o confObj) string {
	if s.single {
		return ""
	}
	if s.keyOf != nil {
		return s.keyOf(o)
	}
	k, _ := o[s.key].(string)
	return k
}

func (s *confSection) ordered(objs []confObj) []confObj {
	if s.order != nil {
		return s.order(objs)
	}
	return objs
}

// split moves the secrets out of o and returns them by path.
func (s *confSection) split(o confObj) map[string]string {
	sec := map[string]string{}
	for _, x := range s.secrets {
		v, _ := getPath(o, x.path).(string)
		secret := v != "" && (x.when == nil || x.when(v))
		switch {
		case secret && x.show != nil:
			setPath(o, x.path, x.show(v))
		case x.show == nil:
			delPath(o, x.path)
		}
		if secret {
			sec[x.path] = v
		}
		if x.clear != "" {
			delPath(o, x.clear)
		}
	}
	return sec
}

// reshape puts o through the section's request type: strictly (a
// document's object: unknown fields and wrong types are errors) or not (a
// stored one), so both sides of a diff have the same fields.
func (s *confSection) reshape(o confObj, strict bool) (confObj, error) {
	extra := map[string]any{}
	for _, f := range s.extra {
		if v, ok := o[f]; ok {
			extra[f] = v
			delete(o, f)
		}
	}
	b, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	p := s.proto()
	dec := json.NewDecoder(bytes.NewReader(b))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(p); err != nil {
		return nil, err
	}
	out, err := toConfObj(p)
	if err != nil {
		return nil, err
	}
	s.split(out) // drops the empty secret and clear fields the type has
	for f, v := range extra {
		out[f] = v
	}
	return out, nil
}

// ---- apply ----

// ApplyConfig validates doc and, unless opts.DryRun, applies it as the
// principal in ctx. A *repo.ValidationError means nothing was changed.
func (u *UI) ApplyConfig(ctx context.Context, doc []byte, opts ConfigOptions) (*ConfigResult, error) {
	if opts.CreateOnly && opts.Prune {
		return nil, &repo.ValidationError{Errors: []string{"createOnly and prune exclude each other"}}
	}
	sel, err := confSelect(opts)
	if err != nil {
		return nil, err
	}
	top, err := parseConf(doc)
	if err != nil {
		return nil, &repo.ValidationError{Errors: []string{err.Error()}}
	}
	if !opts.DryRun {
		unlock, err := u.DB.Lock(ctx, confLock)
		if err != nil {
			return nil, err
		}
		defer unlock()
		// A caller that leaves midway must not stop the steps or their
		// rollback halfway.
		ctx = context.WithoutCancel(ctx)
	}
	c := &confRun{u: u, ctx: ctx, opts: opts, cur: map[string]*confState{}, want: map[string]*confState{}}
	payload, err := c.load(top, sel)
	if err != nil {
		return nil, err
	}
	// Every problem at once: an object that does not parse is left out
	// of the checks of the others.
	c.mergeSecrets(payload)
	c.validate()
	if c.opts.noSecrets {
		for name, w := range c.want {
			for k, sec := range w.sec {
				if len(sec) > 0 {
					c.bad("%s %q: a document with secrets (or password hashes) needs a password or session login", name, k)
				}
			}
		}
	}
	if len(c.errs) > 0 {
		return nil, &repo.ValidationError{Errors: c.errs}
	}
	steps := c.plan()
	res := &ConfigResult{DryRun: opts.DryRun, Prune: opts.Prune, OK: true, Steps: steps, Warnings: c.warns,
		Summary: map[string]int{confCreate: 0, confUpdate: 0, confDelete: 0, confUnchanged: 0}}
	for _, st := range steps {
		res.Summary[st.Action]++
	}
	if opts.DryRun {
		return res, nil
	}
	c.run(res)
	done := map[string]int{}
	for _, st := range res.Steps {
		if st.Status != "" && st.Status != "skipped" && st.Status != "notAttempted" {
			done[st.Action]++
		}
	}
	if len(done) > 0 {
		r := c.request(http.MethodPost, "/-/api/configuration")
		detail := fmt.Sprintf("created %d, updated %d, deleted %d", done[confCreate], done[confUpdate], done[confDelete])
		if !res.OK {
			detail = "failed: " + res.Error + "; rolled back: " + strconv.FormatBool(res.RolledBack)
		}
		u.emit(r, "configuration.apply", "", "configuration", detail)
	}
	return res, nil
}

// parseConf reads a YAML or JSON document into its top-level keys.
func parseConf(doc []byte) (map[string]any, error) {
	var v any
	if err := yaml.Unmarshal(doc, &v); err != nil {
		return nil, fmt.Errorf("the document is not valid YAML or JSON: %v", err)
	}
	top, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("the document must be a mapping of sections")
	}
	n, err := toConfAny(top)
	if err != nil {
		return nil, fmt.Errorf("the document does not convert to JSON: %v", err)
	}
	top = n.(map[string]any)
	if ver, _ := top["version"].(json.Number); ver.String() != strconv.Itoa(confVersion) {
		return nil, fmt.Errorf("version must be %d", confVersion)
	}
	return top, nil
}

// load reads the stored state of every available section and the
// document's selected sections into c.want. It returns the decrypted
// secrets block, if any.
func (c *confRun) load(top map[string]any, sel map[string]bool) (map[string]map[string]map[string]string, error) {
	known := map[string]bool{"version": true, "secrets": true}
	for _, s := range confSections() {
		known[s.name] = true
		if !s.available(c.u) {
			if _, ok := top[s.name]; ok && sel[s.name] {
				c.warns = append(c.warns, fmt.Sprintf("section %s is skipped: it is not available in this process", s.name))
			}
			continue
		}
		v, present := top[s.name]
		managed := present && sel[s.name]
		st, err := c.read(s, managed)
		if err != nil {
			return nil, fmt.Errorf("read the stored %s: %w", s.name, err)
		}
		c.cur[s.name] = st
		if managed {
			c.want[s.name] = c.doc(s, v)
		}
	}
	for k := range top {
		if !known[k] {
			c.bad("unknown section %q", k)
		}
	}
	box, ok := top["secrets"]
	if !ok {
		return nil, nil
	}
	if c.opts.Passphrase == "" {
		c.bad("the document holds encrypted secrets: send their passphrase (%s), or remove the secrets block to keep the stored ones", ConfigPassphraseHeader)
		return nil, nil
	}
	payload, err := openConfigSecrets(box, c.opts.Passphrase)
	if err != nil {
		c.bad("secrets: %v", err)
		return nil, nil
	}
	c.given = true
	return payload, nil
}

// doc reads a section of the document.
func (c *confRun) doc(s *confSection, v any) *confState {
	st := newConfState()
	var items []any
	if s.single {
		items = []any{v}
	} else if list, ok := v.([]any); ok {
		items = list
	} else if v != nil {
		c.bad("%s must be a list", s.name)
		return st
	}
	for i, it := range items {
		at := s.name
		if !s.single {
			at = fmt.Sprintf("%s[%d]", s.name, i)
		}
		o, ok := it.(map[string]any)
		if !ok {
			c.bad("%s must be an object", at)
			continue
		}
		sec := s.split(o)
		r, err := s.reshape(o, true)
		if err != nil {
			c.bad("%s: %s", at, strings.TrimPrefix(err.Error(), "json: "))
			continue
		}
		k := s.keyFor(r)
		if !s.single && k == "" {
			c.bad("%s: no valid %s", at, s.key)
			continue
		}
		if _, dup := st.objs[k]; dup {
			c.bad("%s: %q is listed twice", at, k)
			continue
		}
		st.objs[k], st.sec[k] = r, sec
		st.keys = append(st.keys, k)
	}
	return st
}

// mergeSecrets adds the secrets block to the document's objects. A secret
// for an object or field the document does not have is an error.
func (c *confRun) mergeSecrets(payload map[string]map[string]map[string]string) {
	for name, objs := range payload {
		s := confSectionByName(name)
		if s == nil {
			c.bad("secrets: unknown section %q", name)
			continue
		}
		w := c.want[name]
		if w == nil {
			continue // a section the apply does not manage
		}
		for k, fields := range objs {
			if _, ok := w.objs[k]; !ok {
				c.bad("secrets: %s %q is not in the document", name, k)
				continue
			}
			for p, v := range fields {
				if !slices.ContainsFunc(s.secrets, func(x confSecret) bool { return x.path == p }) {
					c.bad("secrets: %s %q: %q is not a secret field", name, k, p)
					continue
				}
				w.sec[k][p] = v
			}
		}
	}
}

// desired is a section's objects after the apply: the document's, plus
// the stored ones it does not list unless they are pruned.
func (c *confRun) desired(name string) map[string]confObj {
	if d, ok := c.memo[name]; ok {
		return d
	}
	if c.memo == nil {
		c.memo = map[string]map[string]confObj{}
	}
	c.memo[name] = c.desiredOf(name)
	return c.memo[name]
}

func (c *confRun) desiredOf(name string) map[string]confObj {
	cur := c.cur[name]
	if cur == nil {
		return map[string]confObj{}
	}
	w := c.want[name]
	if w == nil {
		return cur.objs
	}
	out := map[string]confObj{}
	if !c.opts.Prune {
		for k, o := range cur.objs {
			out[k] = o
		}
	} else if s := confSectionByName(name); s.keepDel != nil {
		for k, o := range cur.objs {
			if s.keepDel(k) {
				out[k] = o
			}
		}
	}
	for k, o := range w.objs {
		out[k] = o
	}
	return out
}

func (c *confRun) has(section, key string) bool { _, ok := c.desired(section)[key]; return ok }

// validate runs every managed section's check, then the lockout check.
func (c *confRun) validate() {
	for _, s := range confSections() {
		w := c.want[s.name]
		if w == nil || s.check == nil {
			continue
		}
		objs := make([]confObj, 0, len(w.keys))
		for _, k := range w.keys {
			objs = append(objs, w.objs[k])
		}
		s.check(c, objs)
	}
	c.checkRefs()
	c.checkLockout()
}

// plan lists the steps: creates and updates section by section in
// dependency order, then the deletes in the reverse order.
func (c *confRun) plan() []*ConfigStep {
	var steps, deletes []*ConfigStep
	secs := confSections()
	for _, s := range secs {
		w := c.want[s.name]
		if w == nil {
			continue
		}
		cur := c.cur[s.name]
		objs := make([]confObj, 0, len(w.keys))
		for _, k := range w.keys {
			objs = append(objs, w.objs[k])
		}
		for _, o := range s.ordered(objs) {
			k := s.keyFor(o)
			st := &ConfigStep{Section: s.name, Key: k, s: s, want: o, wantSec: w.sec[k], authoritative: c.given}
			if co, ok := cur.objs[k]; ok {
				st.cur, st.curSec = co, cur.sec[k]
				st.Action, st.Changes = confUnchanged, c.diff(s, st)
				if len(st.Changes) > 0 {
					st.Action = confUpdate
				}
			} else {
				st.Action = confCreate
				if s.name == "users" && o["source"] == meta.SourceLocal && w.sec[k]["passwordHash"] == "" {
					st.Warnings = append(st.Warnings, "no passwordHash: the user cannot sign in until an admin sets a password")
				}
			}
			steps = append(steps, st)
		}
	}
	if c.opts.Prune {
		for i := len(secs) - 1; i >= 0; i-- {
			s := secs[i]
			w := c.want[s.name]
			if w == nil || s.single {
				continue
			}
			cur := c.cur[s.name]
			for j := len(cur.keys) - 1; j >= 0; j-- {
				k := cur.keys[j]
				if _, ok := w.objs[k]; ok || s.keepDel != nil && s.keepDel(k) {
					continue
				}
				deletes = append(deletes, &ConfigStep{Section: s.name, Key: k, Action: confDelete, s: s, cur: cur.objs[k], curSec: cur.sec[k]})
			}
		}
	}
	return append(steps, deletes...)
}

// diff compares the stored and wanted object field by field; fields in
// keep (all fields of a partial section) that the document leaves null
// take the stored value first.
func (c *confRun) diff(s *confSection, st *ConfigStep) []ConfigChange {
	want := deepCopy(st.want).(map[string]any)
	keep := s.keep
	if s.partial {
		keep = nil
		for k := range want {
			keep = append(keep, k)
		}
	}
	for _, f := range keep {
		if getPath(want, f) == nil {
			setPath(want, f, getPath(st.cur, f))
		}
	}
	if s.canon != nil {
		if v := s.canon(want); v != nil {
			if o, err := toConfObj(v); err == nil {
				s.split(o)
				if o, err = s.reshape(o, false); err == nil {
					want = o
				}
			}
		}
	}
	st.want = want
	cur := deepCopy(st.cur).(map[string]any)
	for _, f := range s.extra {
		delete(want, f)
		delete(cur, f)
	}
	a, b := map[string]any{}, map[string]any{}
	flatten("", cur, a)
	flatten("", want, b)
	fields := map[string]bool{}
	for k := range a {
		fields[k] = true
	}
	for k := range b {
		fields[k] = true
	}
	var out []ConfigChange
	for f := range fields {
		if confSame(a[f], b[f]) {
			continue
		}
		from, _ := json.Marshal(a[f])
		to, _ := json.Marshal(b[f])
		out = append(out, ConfigChange{Field: f, From: from, To: to})
	}
	for _, x := range s.secrets {
		cv, wv := st.curSec[x.path], st.wantSec[x.path]
		if cv == wv || wv == "" && (!st.authoritative || x.noClear) {
			continue
		}
		out = append(out, ConfigChange{Field: x.path, Secret: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

// run applies the planned steps in order; on a failure it undoes the
// applied ones in reverse order.
func (c *confRun) run(res *ConfigResult) {
	var done []*ConfigStep
	for i, st := range res.Steps {
		if st.Action == confUnchanged {
			continue
		}
		if c.opts.CreateOnly && st.Action != confCreate {
			st.Status = "skipped"
			continue
		}
		if err := c.write(st); err != nil {
			setStepError(st, err)
			st.Status = "failed"
			res.OK = false
			res.Error = fmt.Sprintf("%s %s %q: %s", st.Action, st.Section, st.Key, st.Error)
			for _, rest := range res.Steps[i+1:] {
				if rest.Action != confUnchanged {
					rest.Status = "notAttempted"
				}
			}
			res.RolledBack = true
			for j := len(done) - 1; j >= 0; j-- {
				d := done[j]
				if err := c.write(d.inverse()); err != nil {
					d.Status = "rollbackFailed"
					setStepError(d, err)
					res.RolledBack = false
					continue
				}
				d.Status = "rolledBack"
			}
			return
		}
		st.Status = "applied"
		done = append(done, st)
	}
}

// inverse is the step that undoes st, with the values read before.
func (st *ConfigStep) inverse() *ConfigStep {
	inv := *st
	inv.authoritative, inv.undo = true, true // the stored secrets are known exactly
	switch st.Action {
	case confCreate:
		inv.Action, inv.cur, inv.curSec = confDelete, st.want, st.wantSec
	case confDelete:
		inv.Action, inv.want, inv.wantSec, inv.cur, inv.curSec = confCreate, st.cur, st.curSec, nil, nil
	case confUpdate:
		inv.want, inv.wantSec, inv.cur, inv.curSec = st.cur, st.curSec, st.want, st.wantSec
	}
	return &inv
}

// confCallError is a handler's refusal.
type confCallError struct {
	status int
	msg    string
	errs   []string
}

func (e *confCallError) Error() string { return fmt.Sprintf("%s (HTTP %d)", e.msg, e.status) }

func setStepError(st *ConfigStep, err error) {
	var ce *confCallError
	if errors.As(err, &ce) {
		st.Error, st.Errors, st.HTTPStatus = ce.msg, ce.errs, ce.status
		return
	}
	st.Error = err.Error()
}

// write runs one step through the section's API.
func (c *confRun) write(st *ConfigStep) error {
	if st.s.write != nil {
		return st.s.write(c, st)
	}
	return c.writeAPI(st)
}

// writeAPI is the generic write: POST base, PUT base[/{id}], DELETE base/{id}.
func (c *confRun) writeAPI(st *ConfigStep) error {
	s := st.s
	switch st.Action {
	case confCreate:
		return c.call(http.MethodPost, s.base, c.body(st), http.StatusCreated, http.StatusOK)
	case confUpdate:
		path := s.base
		if !s.single {
			id, err := c.id(s, st.Key)
			if err != nil {
				return err
			}
			path += "/" + url.PathEscape(id)
		}
		b := c.body(st)
		if s.noUpdateKey {
			delete(b, s.key)
		}
		if s.partial {
			b = partialBody(st, b)
		}
		return c.call(http.MethodPut, path, b, http.StatusOK)
	case confDelete:
		id, err := c.id(s, st.Key)
		if err != nil {
			return err
		}
		return c.call(http.MethodDelete, s.base+"/"+url.PathEscape(id), nil, http.StatusNoContent, http.StatusOK)
	}
	return fmt.Errorf("unknown action %q", st.Action)
}

// partialBody keeps the top-level fields st changes.
func partialBody(st *ConfigStep, b confObj) confObj {
	out := confObj{}
	for _, ch := range st.Changes {
		top, _, _ := strings.Cut(ch.Field, ".")
		if v, ok := b[top]; ok {
			out[top] = v
		}
	}
	return out
}

func (c *confRun) id(s *confSection, key string) (string, error) {
	if s.idOf == nil {
		return key, nil
	}
	return s.idOf(c, key)
}

// body is the request body of st's want: the object, the secrets the
// document gives, and the clear flags of the stored secrets it drops.
func (c *confRun) body(st *ConfigStep) confObj {
	b := deepCopy(st.want).(map[string]any)
	for _, f := range st.s.extra {
		delete(b, f)
	}
	for _, x := range st.s.secrets {
		if x.own {
			continue
		}
		if v := st.wantSec[x.path]; v != "" {
			setPath(b, x.path, v)
			continue
		}
		if !st.authoritative || x.noClear || st.curSec[x.path] == "" {
			continue
		}
		if x.clear != "" {
			setPath(b, x.clear, true)
		} else {
			setPath(b, x.path, "")
		}
	}
	return b
}

// request is a request carrying c's context (the principal) and request id.
func (c *confRun) request(method, path string) *http.Request {
	r, _ := http.NewRequestWithContext(c.ctx, method, path, nil)
	r.Header.Set("X-Request-Id", c.opts.RequestID)
	return r
}

// call serves method path with body through the UI's own API, as the
// caller, and returns an error unless the status is one of ok.
func (c *confRun) call(method, path string, body any, ok ...int) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(c.ctx, method, path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set(auth.UIHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", c.opts.RequestID)
	c.u.internalOnce.Do(func() { c.u.internal = c.u.apiHandler() })
	rec := &bodyRecorder{h: http.Header{}}
	c.u.internal.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	if slices.Contains(ok, rec.status) {
		return nil
	}
	var e struct {
		Error  string   `json:"error"`
		Errors []string `json:"errors"`
	}
	json.Unmarshal(rec.body.Bytes(), &e)
	if e.Error == "" {
		e.Error = http.StatusText(rec.status)
	}
	return &confCallError{status: rec.status, msg: e.Error, errs: e.Errors}
}

// ---- lockout ----

// checkLockout refuses a document whose end state would leave no admin
// who can sign in, by the rules of the access API's checkLockout: the last
// restorable admin (an active local user with a password and the admin
// role) must stay, and a system where an admin can sign in now must keep
// one.
func (c *confRun) checkLockout() {
	if c.want["users"] == nil && c.want["roles"] == nil && c.want["realms"] == nil && c.want["ldap"] == nil &&
		c.want["crowd"] == nil {
		return
	}
	data, err := c.u.DB.LoadAccess(c.ctx)
	if err != nil {
		c.bad("lockout check: %v", err)
		return
	}
	stored := map[string]meta.User{}
	for _, x := range data.Users {
		stored[x.ID] = x
	}
	var users []meta.User
	for k, o := range c.desired("users") {
		x := asConf[confUser](o)
		nu := meta.User{ID: x.ID, Name: x.Name, Email: x.Email, Source: x.Source, Status: x.Status, Roles: x.Roles}
		if nu.Source == "" {
			nu.Source = meta.SourceLocal
		}
		if nu.Status == "" {
			nu.Status = meta.StatusActive
		}
		nu.PasswordHash = stored[k].PasswordHash
		if w := c.want["users"]; w != nil && w.sec[k]["passwordHash"] != "" {
			nu.PasswordHash = w.sec[k]["passwordHash"]
		}
		if _, ok := stored[k]; !ok && (c.want["users"] == nil || c.want["users"].sec[k]["passwordHash"] == "") {
			nu.PasswordHash = "" // created without a password
		}
		users = append(users, nu)
	}
	var roles []meta.Role
	for _, x := range data.Roles {
		if x.ID == meta.AdminRole {
			roles = append(roles, x)
		}
	}
	for _, o := range c.desired("roles") {
		x := asConf[confRole](o)
		roles = append(roles, meta.Role{ID: x.ID, Description: x.Description, Privileges: toPrivileges(x.Privileges), Roles: x.Roles})
	}
	set := data.Settings
	if o := c.desired("realms")[""]; o != nil && c.want["realms"] != nil {
		set.Realms = asConf[confRealms](o).Realms
	}
	if o := c.desired("ldap")[""]; o != nil && c.want["ldap"] != nil {
		set.LDAP = ldapMerged(asConf[ldapRequest](o))
	}
	if o := c.desired("crowd")[""]; o != nil && c.want["crowd"] != nil {
		set.Crowd = crowdMerged(asConf[crowdRequest](o))
	}
	before := auth.NewSnapshot(data.Users, data.Roles, data.Settings)
	after := auth.NewSnapshot(users, roles, set)
	if auth.RestorableAdmins(before) > 0 && auth.RestorableAdmins(after) == 0 || auth.CheckLockout(before) == nil && auth.CheckLockout(after) != nil {
		c.bad("%s: the document's users, roles and realms leave no admin who can sign in (keep an active local user with a password "+
			"and the admin role; with prune, list it in users)", lockoutMessage)
	}
}

// ---- helpers ----

// toConfObj turns v into its JSON object form, numbers kept exact.
func toConfObj(v any) (confObj, error) {
	n, err := toConfAny(v)
	if err != nil {
		return nil, err
	}
	o, ok := n.(map[string]any)
	if !ok {
		return nil, errors.New("not an object")
	}
	return o, nil
}

func toConfAny(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	err = dec.Decode(&out)
	return out, err
}

// asConf decodes an object into T (fields the object lacks stay zero).
func asConf[T any](o confObj) T {
	var t T
	b, _ := json.Marshal(o)
	json.Unmarshal(b, &t)
	return t
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopy(e)
		}
		return out
	}
	return v
}

func getPath(o map[string]any, path string) any {
	var cur any = o
	for _, k := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func setPath(o map[string]any, path string, v any) {
	keys := strings.Split(path, ".")
	m := o
	for _, k := range keys[:len(keys)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[keys[len(keys)-1]] = v
}

func delPath(o map[string]any, path string) {
	keys := strings.Split(path, ".")
	m := o
	for _, k := range keys[:len(keys)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			return
		}
		m = next
	}
	delete(m, keys[len(keys)-1])
}

// flatten maps every leaf of v (a non-object, or an empty object) to its
// dotted path; lists are leaves.
func flatten(prefix string, v any, out map[string]any) {
	if m, ok := v.(map[string]any); ok && len(m) > 0 {
		for k, x := range m {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flatten(p, x, out)
		}
		return
	}
	if prefix != "" {
		out[prefix] = v
	}
}

// confSame compares two leaves; null, "", [] and {} are the same (the
// handlers read them alike).
func confSame(a, b any) bool {
	empty := func(v any) bool {
		switch x := v.(type) {
		case nil:
			return true
		case string:
			return x == ""
		case []any:
			return len(x) == 0
		case map[string]any:
			return len(x) == 0
		}
		return false
	}
	if empty(a) && empty(b) {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// randomPassword is the password a local user is created with before its
// hash is set: nobody knows it.
func randomPassword() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// WithConfigActor is the context the CLI's export and apply run in: an
// admin who signed in with a password, named cli:config in the events.
func WithConfigActor(ctx context.Context) context.Context {
	return auth.WithPrincipal(ctx, auth.Principal{User: confActor, Admin: true, Via: auth.ViaPassword})
}
