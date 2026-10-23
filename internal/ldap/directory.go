package ldap

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"

	"commandcenter/internal/meta"
)

// ErrNoSuchUser reports a search without a match.
var ErrNoSuchUser = errors.New("ldap: no such user")

// ErrAmbiguous reports a search with more than one match.
var ErrAmbiguous = errors.New("ldap: ambiguous user")

// ErrBadEntry reports a found entry that cannot be used (no DN or no id).
var ErrBadEntry = errors.New("ldap: unusable entry")

// User is a directory user as the configuration maps it.
type User struct {
	DN     string
	ID     string // the userIdAttr value
	Name   string
	Email  string
	Groups []string // the groupAttr values
}

// Step is one stage of a diagnostic run (Diagnose).
type Step struct {
	Step   string `json:"step"` // connect, starttls, bind, search, userBind, groups
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Client runs logins and lookups against a configured directory. The zero
// value uses the system roots.
type Client struct {
	rootCAs *x509.CertPool // tests trust their own CA
}

func (cl *Client) options(cfg meta.LDAP) DialOptions {
	t := time.Duration(cfg.TimeoutSeconds) * time.Second
	if t <= 0 {
		t = defaultTimeout
	}
	return DialOptions{URL: cfg.URL, StartTLS: cfg.StartTLS, InsecureSkipVerify: cfg.InsecureSkipVerify,
		Timeout: t, RootCAs: cl.rootCAs}
}

// trace records steps when non-nil.
type trace struct{ steps []Step }

func (t *trace) add(step string, err error, okDetail string) {
	if t == nil {
		return
	}
	if err != nil {
		t.steps = append(t.steps, Step{Step: step, Detail: err.Error()})
		return
	}
	t.steps = append(t.steps, Step{Step: step, OK: true, Detail: okDetail})
}

// open connects, upgrades with StartTLS when configured and performs the
// service bind (skipped when no bind DN is set: an anonymous search).
func (cl *Client) open(ctx context.Context, cfg meta.LDAP, tr *trace) (*Conn, error) {
	if !cfg.Complete() {
		return nil, errors.New("ldap: the configuration is incomplete")
	}
	o := cl.options(cfg)
	c, err := Dial(ctx, o)
	tr.add("connect", err, "connected to "+hostOf(cfg.URL))
	if err != nil {
		return nil, &DialError{Phase: "connect", Err: err}
	}
	if cfg.StartTLS {
		err = c.StartTLS(ctx, o)
		tr.add("starttls", err, "TLS established")
		if err != nil {
			c.Close()
			return nil, &DialError{Phase: "starttls", Err: err}
		}
	}
	if cfg.BindDN != "" {
		err = c.Bind(cfg.BindDN, cfg.BindPassword)
		tr.add("bind", err, "bound as "+cfg.BindDN)
		if err != nil {
			c.Close()
			err = fmt.Errorf("service bind: %w", err)
			var re *ResultError
			if !errors.As(err, &re) && !errors.Is(err, ErrInvalidCredentials) {
				return nil, &DialError{Phase: "bind", Err: err} // network I/O, not an answer
			}
			return nil, err
		}
	} else {
		tr.add("bind", nil, "anonymous search (no bind DN)")
	}
	return c, nil
}

func hostOf(raw string) string {
	if _, _, addr, err := parseURL(raw); err == nil {
		return addr
	}
	return "?"
}

// find searches for the one entry of user name.
func find(c *Conn, cfg meta.LDAP, name string, tr *trace) (User, error) {
	entries, err := c.Search(SearchRequest{
		BaseDN:     cfg.UserBaseDN,
		Filter:     UserFilter(cfg.UserFilter, name),
		Attributes: []string{cfg.UserIDAttr, cfg.NameAttr, cfg.EmailAttr, cfg.GroupAttr},
		SizeLimit:  maxEntries,
	})
	var re *ResultError
	switch {
	case len(entries) > 1:
		err = ErrAmbiguous
	case err != nil && errors.As(err, &re) && re.Code == ResultSizeLimitExceeded:
		err = ErrAmbiguous
	case err == nil && len(entries) == 0:
		err = ErrNoSuchUser
	}
	if err != nil {
		tr.add("search", err, "")
		return User{}, err
	}
	e := entries[0]
	u := User{DN: e.DN, ID: e.First(cfg.UserIDAttr), Name: e.First(cfg.NameAttr), Email: e.First(cfg.EmailAttr),
		Groups: append([]string{}, e.Attrs[strings.ToLower(cfg.GroupAttr)]...)}
	if u.DN == "" {
		err = fmt.Errorf("%w: empty DN", ErrBadEntry)
	} else if u.ID == "" {
		err = fmt.Errorf("%w: no %s attribute", ErrBadEntry, cfg.UserIDAttr)
	}
	tr.add("search", err, "found "+e.DN)
	return u, err
}

// Authenticate verifies name and password: service bind, search for the one
// entry matching userFilter (name escaped), then a bind as that entry. An
// empty name or password is refused without contacting the server.
func (cl *Client) Authenticate(ctx context.Context, cfg meta.LDAP, name, password string) (User, error) {
	return cl.authenticate(ctx, cfg, name, password, nil)
}

func (cl *Client) authenticate(ctx context.Context, cfg meta.LDAP, name, password string, tr *trace) (User, error) {
	if name == "" || password == "" {
		return User{}, ErrInvalidCredentials
	}
	c, err := cl.open(ctx, cfg, tr)
	if err != nil {
		return User{}, err
	}
	defer c.Close()
	u, err := find(c, cfg, name, tr)
	if err != nil {
		return User{}, err
	}
	err = c.Bind(u.DN, password)
	tr.add("userBind", err, "bound as "+u.DN)
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// Find looks up the user with the given id through the service bind (no
// user bind): the periodic re-check of a signed-in LDAP user. The found
// entry's id must equal id.
func (cl *Client) Find(ctx context.Context, cfg meta.LDAP, id string) (User, error) {
	if id == "" {
		return User{}, ErrNoSuchUser
	}
	c, err := cl.open(ctx, cfg, nil)
	if err != nil {
		return User{}, err
	}
	defer c.Close()
	u, err := find(c, cfg, id, nil)
	if err != nil {
		return User{}, err
	}
	if u.ID != id {
		return User{}, ErrNoSuchUser
	}
	return u, nil
}

// Diagnose runs the configured steps and reports each: connect, starttls,
// bind and, with a user name, search; with a password too, userBind and
// groups. Details never contain the bind password or the user's password.
func (cl *Client) Diagnose(ctx context.Context, cfg meta.LDAP, name, password string) []Step {
	tr := &trace{}
	var u User
	var err error
	switch {
	case name != "" && password != "":
		u, err = cl.authenticate(ctx, cfg, name, password, tr)
	default:
		var c *Conn
		c, err = cl.open(ctx, cfg, tr)
		if err == nil {
			if name != "" {
				u, err = find(c, cfg, name, tr)
			}
			c.Close()
		}
	}
	if err == nil && name != "" {
		roles, matched := MapRoles(cfg, u.Groups)
		tr.add("groups", nil, fmt.Sprintf("user %q; groups matched: [%s]; roles: [%s]",
			u.ID, strings.Join(matched, ", "), strings.Join(roles, ", ")))
	}
	if len(tr.steps) == 0 && err != nil {
		tr.add("connect", err, "")
	}
	for i := range tr.steps {
		tr.steps[i].Detail = redact(tr.steps[i].Detail, cfg.BindPassword, password)
	}
	return tr.steps
}

func redact(s string, secrets ...string) string {
	for _, x := range secrets {
		if x != "" {
			s = strings.ReplaceAll(s, x, "***")
		}
	}
	return s
}

// firstRDNValue returns the value of a DN's first RDN ("cn=x,ou=y" -> "x"),
// or "" when dn has no '='.
func firstRDNValue(dn string) string {
	rdn := dn
	for i := 0; i < len(dn); i++ {
		if dn[i] == '\\' {
			i++
			continue
		}
		if dn[i] == ',' || dn[i] == '+' {
			rdn = dn[:i]
			break
		}
	}
	_, v, ok := strings.Cut(rdn, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// GroupMatches reports whether a group attribute value matches a mapping's
// group: the full DN, or the first RDN's value (both case-insensitive).
func GroupMatches(value, group string) bool {
	value, group = strings.TrimSpace(value), strings.TrimSpace(group)
	if group == "" {
		return false
	}
	if strings.EqualFold(value, group) {
		return true
	}
	cn := firstRDNValue(value)
	return cn != "" && strings.EqualFold(cn, group)
}

// MapRoles returns the roles granted by groups under cfg (the union of the
// matching mappings' roles and the default roles, in first-seen order) and
// the group values that matched a mapping.
func MapRoles(cfg meta.LDAP, groups []string) (roles, matched []string) {
	seen := map[string]bool{}
	add := func(r string) {
		if !seen[r] {
			seen[r] = true
			roles = append(roles, r)
		}
	}
	hit := map[string]bool{}
	for _, m := range cfg.GroupMappings {
		for _, g := range groups {
			if GroupMatches(g, m.Group) {
				for _, r := range m.Roles {
					add(r)
				}
				if !hit[g] {
					hit[g] = true
					matched = append(matched, g)
				}
			}
		}
	}
	for _, r := range cfg.DefaultRoles {
		add(r)
	}
	if roles == nil {
		roles = []string{}
	}
	return roles, matched
}

// DialError is a failure to reach the directory: the TCP connect, the TLS
// handshake (ldaps:// or StartTLS) or network I/O during the service bind.
// Anything after a successful service bind is not a DialError.
type DialError struct {
	Phase string // connect, starttls or bind
	Err   error
}

func (e *DialError) Error() string { return e.Err.Error() }
func (e *DialError) Unwrap() error { return e.Err }
