package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"commandcenter/internal/ldap"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/truststore"
)

// Atlassian Crowd login. A password is checked with Crowd's REST API
// (POST /rest/usermanagement/1/authentication, as the configured
// application) and the user's nested groups are read; a success is cached
// for cacheMinutes under the user name and a hash of the password (never the
// password), and a later success with another password drops the older
// entries. Sessions and tokens of the user ("crowd:<id>") resolve through an
// in-memory entry that is re-read from Crowd once it is older than
// cacheMinutes; while Crowd cannot be reached the user is denied (fail
// closed; local admins are unaffected). Roles are computed on every use
// from the live snapshot: the group mappings, the default roles and an
// optional override row (source=crowd, same id) whose roles are added and
// which denies the user when disabled.

const (
	crowdOwnerPrefix = "crowd:"
	sourceCrowd      = "crowd"
	crowdMax         = 1000             // cached users and cached password successes, each
	crowdRetry       = 10 * time.Second // after a failed re-read, deny without asking Crowd for this long
	crowdMaxBody     = 1 << 20
	crowdAPI         = "/rest/usermanagement/1"
)

// CrowdUser is a Crowd user with its groups and, from CrowdUsers, the
// roles they map to under the live snapshot.
type CrowdUser struct {
	ID, Name, Email string
	Groups          []string
	Roles           []string
}

type crowdEntry struct {
	user      CrowdUser
	checkedAt time.Time
	failedAt  time.Time
}

type crowdPassHit struct {
	id  string
	exp time.Time
}

// CrowdError is an answer of Crowd other than a success: its HTTP status
// and the reason of its error body (INVALID_USER_AUTHENTICATION,
// USER_NOT_FOUND, INACTIVE_ACCOUNT, ...).
type CrowdError struct {
	Status int
	Reason string
}

func (e *CrowdError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("crowd answered %d (%s)", e.Status, e.Reason)
	}
	return fmt.Sprintf("crowd answered %d", e.Status)
}

// refusal reports whether e says the user may not sign in (a wrong
// password, an unknown or inactive user): an answer about the caller's
// input, not an outage.
func (e *CrowdError) refusal() bool {
	return e.Status == http.StatusBadRequest || e.Status == http.StatusNotFound
}

// errCrowdInactive is a user Crowd lists as inactive.
var errCrowdInactive = &CrowdError{Status: http.StatusBadRequest, Reason: "INACTIVE_ACCOUNT"}

// IsCrowd reports whether p signed in through the crowd realm.
func (p Principal) IsCrowd() bool { return p.source == sourceCrowd }

func crowdUsable(s *Snapshot) bool { return s.realms[meta.RealmCrowd] && s.settings.Crowd.Complete() }

func crowdChanged(old, cur *Snapshot) bool {
	return !reflect.DeepEqual(old.settings.Crowd, cur.settings.Crowd)
}

func crowdTTL(c meta.Crowd) time.Duration { return time.Duration(max(c.CacheMinutes, 1)) * time.Minute }

func (a *Authenticator) clearCrowd() {
	a.crowdMu.Lock()
	a.crowdUsers, a.crowdPass = map[string]crowdEntry{}, map[string]crowdPassHit{}
	if a.crowdHTTP != nil {
		a.crowdHTTP.CloseIdleConnections()
	}
	a.crowdHTTP, a.crowdHTTPURL = nil, ""
	a.crowdMu.Unlock()
}

func (a *Authenticator) dropCrowd(id string) {
	a.crowdMu.Lock()
	delete(a.crowdUsers, id)
	for k, v := range a.crowdPass {
		if v.id == id {
			delete(a.crowdPass, k)
		}
	}
	a.crowdMu.Unlock()
}

// crowdPassKey is the cache key of a password success: never the plaintext.
func crowdPassKey(name, pass string) string {
	sum := sha256.Sum256([]byte("crowd\x00" + name + "\x00" + pass))
	return hex.EncodeToString(sum[:])
}

// crowdClient returns the HTTP client for Crowd at raw, built once per URL:
// CrowdHTTP's, or one trusting the SSL trust store.
func (a *Authenticator) crowdClient(raw string) (*http.Client, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	a.crowdMu.Lock()
	defer a.crowdMu.Unlock()
	if a.crowdHTTP != nil && a.crowdHTTPURL == raw {
		return a.crowdHTTP, nil
	}
	if a.crowdHTTP != nil {
		a.crowdHTTP.CloseIdleConnections()
	}
	if a.CrowdHTTP != nil {
		a.crowdHTTP = a.CrowdHTTP(u)
	} else {
		a.crowdHTTP = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{RootCAs: truststore.Pool()}}}
	}
	a.crowdHTTPURL = raw
	return a.crowdHTTP, nil
}

// crowdCall runs one Crowd REST call as the application and decodes a 2xx
// JSON answer into out; any other answer is a *CrowdError.
func (a *Authenticator) crowdCall(ctx context.Context, cfg meta.Crowd, method, path string, q url.Values, body, out any) error {
	c, err := a.crowdClient(cfg.URL)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(max(cfg.TimeoutSeconds, 1))*time.Second)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	target := strings.TrimRight(cfg.URL, "/") + crowdAPI + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(cfg.ApplicationName, cfg.ApplicationPassword)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, crowdMaxBody+1))
	if err != nil {
		return err
	}
	if len(b) > crowdMaxBody {
		return errors.New("the crowd response is larger than 1 MiB")
	}
	if res.StatusCode/100 != 2 {
		var e struct {
			Reason string `json:"reason"`
		}
		json.Unmarshal(b, &e)
		if len(e.Reason) > 64 || strings.ContainsFunc(e.Reason, func(r rune) bool { return r < ' ' || r > '~' }) {
			e.Reason = ""
		}
		return &CrowdError{Status: res.StatusCode, Reason: e.Reason}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("the crowd response is not JSON: %w", err)
	}
	return nil
}

type crowdUserJSON struct {
	Name        string `json:"name"`
	Email       string `json:"email"`
	DisplayName string `json:"display-name"`
	Active      bool   `json:"active"`
}

// crowdUser completes u with its nested groups; an inactive user is refused.
func (a *Authenticator) crowdUser(ctx context.Context, cfg meta.Crowd, u crowdUserJSON) (CrowdUser, error) {
	if !u.Active {
		return CrowdUser{}, errCrowdInactive
	}
	var g struct {
		Groups []struct {
			Name string `json:"name"`
		} `json:"groups"`
	}
	q := url.Values{"username": {u.Name}, "max-results": {"1000"}}
	if err := a.crowdCall(ctx, cfg, http.MethodGet, "/user/group/nested", q, nil, &g); err != nil {
		return CrowdUser{}, err
	}
	out := CrowdUser{ID: u.Name, Name: u.DisplayName, Email: u.Email, Groups: []string{}}
	for _, x := range g.Groups {
		out.Groups = append(out.Groups, x.Name)
	}
	return out, nil
}

// CrowdAuthenticate checks name's password with Crowd under cfg and reads
// the user's groups, without the cache or the throttle (the connection
// test; sign-in goes through CheckPassword).
func (a *Authenticator) CrowdAuthenticate(ctx context.Context, cfg meta.Crowd, name, pass string) (CrowdUser, error) {
	var u crowdUserJSON
	err := a.crowdCall(ctx, cfg, http.MethodPost, "/authentication", url.Values{"username": {name}},
		map[string]string{"value": pass}, &u)
	if err != nil {
		return CrowdUser{}, err
	}
	return a.crowdUser(ctx, cfg, u)
}

// CrowdFind reads user id and its groups from Crowd under cfg (no cache).
func (a *Authenticator) CrowdFind(ctx context.Context, cfg meta.Crowd, id string) (CrowdUser, error) {
	var u crowdUserJSON
	if err := a.crowdCall(ctx, cfg, http.MethodGet, "/user", url.Values{"username": {id}}, nil, &u); err != nil {
		return CrowdUser{}, err
	}
	return a.crowdUser(ctx, cfg, u)
}

// CrowdPing checks that Crowd under cfg answers and accepts the
// application's credentials.
func (a *Authenticator) CrowdPing(ctx context.Context, cfg meta.Crowd) error {
	return a.crowdCall(ctx, cfg, http.MethodGet, "/config/cookie", nil, nil, nil)
}

// crowdPassword is the crowd realm's password check; with cachedOnly it
// only answers a live cached success.
func (a *Authenticator) crowdPassword(ctx context.Context, s *Snapshot, name, pass string, cachedOnly bool) (Principal, bool) {
	if name == "" || pass == "" || !crowdUsable(s) {
		return Principal{}, false
	}
	key, now := crowdPassKey(name, pass), a.clock()
	a.crowdMu.Lock()
	hit, ok := a.crowdPass[key]
	a.crowdMu.Unlock()
	if ok && now.Before(hit.exp) {
		p, ok := a.crowdLookup(ctx, hit.id)
		p.Via = ViaPassword
		return p, ok
	}
	if cachedOnly {
		return Principal{}, false
	}
	u, err := a.CrowdAuthenticate(ctx, s.settings.Crowd, name, pass)
	if err != nil {
		var ce *CrowdError
		if !errors.As(err, &ce) || !ce.refusal() {
			a.logger().Warn("crowd sign-in failed: denying", "user", name, "err", logsafe.Err(err))
		}
		return Principal{}, false
	}
	if !meta.ValidUserID(u.ID) {
		a.logger().Warn("crowd sign-in refused: the user name is not a valid user id", "user", name)
		return Principal{}, false
	}
	p, ok := a.crowdPrincipal(s, u)
	if !ok {
		return Principal{}, false
	}
	a.crowdMu.Lock()
	if a.crowdUsers == nil || len(a.crowdUsers) >= crowdMax {
		a.crowdUsers = map[string]crowdEntry{}
	}
	if a.crowdPass == nil || len(a.crowdPass) >= crowdMax {
		a.crowdPass = map[string]crowdPassHit{}
	}
	for k, v := range a.crowdPass {
		if v.id == u.ID { // an older password of the user
			delete(a.crowdPass, k)
		}
	}
	a.crowdUsers[u.ID] = crowdEntry{user: u, checkedAt: now}
	a.crowdPass[key] = crowdPassHit{id: u.ID, exp: now.Add(crowdTTL(s.settings.Crowd))}
	a.crowdMu.Unlock()
	p.Via = ViaPassword
	return p, true
}

// crowdPrincipal builds the principal of u under s. A disabled override row
// denies it.
func (a *Authenticator) crowdPrincipal(s *Snapshot, u CrowdUser) (Principal, bool) {
	roles, _ := ldap.MapRoles(s.settings.Crowd.Mapping(), u.Groups)
	if row, ok := s.users[u.ID]; ok && row.Source == meta.SourceCrowd {
		if row.Status != meta.StatusActive {
			return Principal{}, false
		}
		for _, r := range row.Roles {
			if !hasRole(roles, r) {
				roles = append(roles, r)
			}
		}
	}
	roles = s.grant(roles)
	return Principal{User: u.ID, Admin: hasRole(roles, meta.AdminRole), roles: roles, source: sourceCrowd}, true
}

// crowdLookup returns the principal of Crowd user id, re-reading it from
// Crowd when its entry is older than cacheMinutes (or missing, as after a
// restart). Concurrent re-reads of one id share one call. It fails closed.
func (a *Authenticator) crowdLookup(ctx context.Context, id string) (Principal, bool) {
	s := a.snap.Load()
	if !crowdUsable(s) || !meta.ValidUserID(id) {
		return Principal{}, false
	}
	now := a.clock()
	a.crowdMu.Lock()
	e, ok := a.crowdUsers[id]
	a.crowdMu.Unlock()
	switch {
	case ok && !e.checkedAt.IsZero() && now.Sub(e.checkedAt) < crowdTTL(s.settings.Crowd):
		return a.crowdPrincipal(s, e.user)
	case ok && !e.failedAt.IsZero() && now.Sub(e.failedAt) < crowdRetry:
		return Principal{}, false
	}
	v, err, _ := a.crowdFlight.Do(id, func() (any, error) {
		// Not the caller's context: one caller leaving must not fail the
		// others; the configured timeout bounds the call.
		return a.CrowdFind(context.WithoutCancel(ctx), s.settings.Crowd, id)
	})
	var ce *CrowdError
	switch {
	case errors.As(err, &ce) && ce.refusal():
		a.dropCrowd(id)
		return Principal{}, false
	case err != nil:
		a.logger().Warn("crowd re-check failed: denying until Crowd answers", "user", id, "err", logsafe.Err(err))
		a.crowdMu.Lock()
		if a.crowdUsers == nil || len(a.crowdUsers) >= crowdMax {
			a.crowdUsers = map[string]crowdEntry{}
		}
		e.failedAt = a.clock()
		a.crowdUsers[id] = e
		a.crowdMu.Unlock()
		return Principal{}, false
	}
	u := v.(CrowdUser)
	a.crowdMu.Lock()
	if a.crowdUsers == nil || len(a.crowdUsers) >= crowdMax {
		a.crowdUsers = map[string]crowdEntry{}
	}
	a.crowdUsers[id] = crowdEntry{user: u, checkedAt: a.clock()}
	a.crowdMu.Unlock()
	return a.crowdPrincipal(s, u)
}

// CrowdUsers returns the cached Crowd users that are currently allowed,
// with their roles under the live snapshot, ordered by id. It never
// contacts Crowd.
func (a *Authenticator) CrowdUsers() []CrowdUser {
	s := a.snap.Load()
	if !crowdUsable(s) {
		return nil
	}
	a.crowdMu.Lock()
	var users []CrowdUser
	for _, e := range a.crowdUsers {
		if !e.checkedAt.IsZero() {
			users = append(users, e.user)
		}
	}
	a.crowdMu.Unlock()
	var out []CrowdUser
	for _, u := range users {
		if p, ok := a.crowdPrincipal(s, u); ok {
			u.Roles = p.roles
			out = append(out, u)
		}
	}
	slices.SortFunc(out, func(x, y CrowdUser) int { return strings.Compare(x.ID, y.ID) })
	return out
}
