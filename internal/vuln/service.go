package vuln

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"commandcenter/internal/auth"
	"commandcenter/internal/component"
	"commandcenter/internal/config"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
	"commandcenter/internal/proxy"
	"commandcenter/internal/repo"
)

// Settings keys: the vulnerability settings, and the record of the last
// import, whose time tells every instance to rebuild its index.
const (
	SettingKey  = "vulnerability"
	databaseKey = "vulnerability.database"
)

// Settings are the vulnerability data settings. Mode "online" asks the OSV
// API at APIURL for what the local database does not answer, caching the
// answers for CacheHours; "offline" uses only the local database, filled
// from the OSV dumps (vulnerability.sync downloads <DumpURL>/<ecosystem>/
// all.zip for Ecosystems, empty meaning every covered one, or an admin
// uploads them). MaxImportMiB bounds one dump.
type Settings struct {
	Enabled      bool     `json:"enabled"`
	Mode         string   `json:"mode"`
	APIURL       string   `json:"apiURL"`
	DumpURL      string   `json:"dumpURL"`
	CacheHours   int      `json:"cacheHours"`
	Ecosystems   []string `json:"ecosystems"`
	MaxImportMiB int      `json:"maxImportMiB"`
}

// DefaultSettings are the values of an unset key: disabled.
func DefaultSettings() Settings {
	return Settings{Mode: "online", APIURL: "https://api.osv.dev", DumpURL: "https://osv-vulnerabilities.storage.googleapis.com",
		CacheHours: 24, Ecosystems: []string{}, MaxImportMiB: 1024}
}

// Limits of the settings.
const (
	MaxCacheHours   = 720
	MaxImportMiBMax = 16384
)

// Validate checks s.
func (s Settings) Validate() error {
	if s.Mode != "online" && s.Mode != "offline" {
		return errors.New(`vulnerability.mode must be "online" or "offline"`)
	}
	for _, f := range []struct{ name, v string }{{"apiURL", s.APIURL}, {"dumpURL", s.DumpURL}} {
		u, err := url.Parse(f.v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || strings.ContainsAny(f.v, "?#") {
			return fmt.Errorf("vulnerability.%s must be an absolute http(s) URL without user info, query or fragment", f.name)
		}
	}
	if s.CacheHours < 1 || s.CacheHours > MaxCacheHours {
		return fmt.Errorf("vulnerability.cacheHours must be between 1 and %d", MaxCacheHours)
	}
	if s.MaxImportMiB < 1 || s.MaxImportMiB > MaxImportMiBMax {
		return fmt.Errorf("vulnerability.maxImportMiB must be between 1 and %d", MaxImportMiBMax)
	}
	seen := map[string]bool{}
	for _, e := range s.Ecosystems {
		if !covered[e] || seen[e] {
			return fmt.Errorf("vulnerability.ecosystems: %q is not one of %s, or is listed twice", e, strings.Join(CoveredEcosystems(), ", "))
		}
		seen[e] = true
	}
	return nil
}

// Database records the last import into the local database.
type Database struct {
	ImportedAt time.Time `json:"importedAt"`
	Source     string    `json:"source"` // "upload" or "sync:<ecosystem>"
	Advisories int       `json:"advisories"`
}

// Failure is the last failed online lookup.
type Failure struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
}

// settingsTTL is how long the settings are reused before they are read
// again, so a change another instance stored applies within it.
const settingsTTL = 5 * time.Second

// lookupTimeout bounds the online lookup a download or the UI waits for.
var lookupTimeout = 15 * time.Second

// Service answers vulnerability lookups, imports OSV data and runs the
// firewall. DB and Repos must be set.
type Service struct {
	DB      *meta.DB
	Repos   *repo.Registry
	Events  repo.EventSink // firewall.blocked, firewall.release, vulnerability.import; nil records none
	TempDir string         // where uploaded and downloaded dumps are spooled; "" is the system's
	Log     *slog.Logger

	mu       sync.Mutex
	settings Settings
	loaded   time.Time
	dbGen    time.Time // the import the index was built after
	clients  map[string]*http.Client

	busy      sync.Mutex  // one import or sync at a time
	importing atomic.Bool // while set, the index answers "maybe" for every package
	ixMu      sync.Mutex
	ixGen     uint64
	index     atomic.Pointer[[]uint64] // sorted hashes of the packages with advisories; nil: not built
	building  atomic.Bool
	builds    sync.WaitGroup // the index build in the background; Close waits for it
	closed    bool           // under ixMu: no more builds
	extra     sync.Map       // hash -> struct{}: packages stored by online lookups since

	failMu   sync.Mutex
	failure  *Failure
	failures int64
	loggedAt time.Time
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (s *Service) emit(kind, actor, repoName, subject, detail string) {
	if s.Events != nil {
		s.Events.Emit(kind, actor, repoName, subject, detail)
	}
}

// Settings returns the stored settings, read again after settingsTTL.
func (s *Service) Settings(ctx context.Context) Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded.IsZero() && time.Since(s.loaded) < settingsTTL {
		return s.copySettings()
	}
	set := DefaultSettings()
	if _, err := s.DB.GetSettingJSON(ctx, SettingKey, &set); err != nil {
		s.log().Warn("could not read the vulnerability settings; the previous ones stay in force", "err", logsafe.Err(err))
		if !s.loaded.IsZero() {
			return s.copySettings()
		}
		set = DefaultSettings()
	}
	var db Database
	if _, err := s.DB.GetSettingJSON(ctx, databaseKey, &db); err == nil && !db.ImportedAt.Equal(s.dbGen) {
		s.dbGen = db.ImportedAt
		s.invalidate() // another instance (or this one) imported: rebuild
	}
	s.settings, s.loaded = set, time.Now()
	return s.copySettings()
}

func (s *Service) copySettings() Settings {
	c := s.settings
	c.Ecosystems = append([]string{}, c.Ecosystems...)
	return c
}

// SaveSettings validates and stores the settings.
func (s *Service) SaveSettings(ctx context.Context, set Settings) error {
	if set.Ecosystems == nil {
		set.Ecosystems = []string{}
	}
	if err := set.Validate(); err != nil {
		return err
	}
	if err := s.DB.SetSettingJSON(ctx, SettingKey, set); err != nil {
		return err
	}
	s.mu.Lock()
	s.settings, s.loaded = set, time.Now()
	s.mu.Unlock()
	return nil
}

// client is the outbound client for the service at base, policed as a
// proxy's remote fetches are (http proxy setting, SSL trust store).
func (s *Service) client(base string) (*http.Client, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients == nil {
		s.clients = map[string]*http.Client{}
	}
	c := s.clients[base]
	if c == nil {
		c = proxy.NewClientFor(u, nil)
		s.clients[base] = c
	}
	return c, nil
}

// ---- the in-memory index ----

func hashKey(eco, key string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(eco))
	h.Write([]byte{0})
	h.Write([]byte(key))
	return h.Sum64()
}

// invalidate drops the index; the next lookup rebuilds it in the background.
func (s *Service) invalidate() {
	s.ixMu.Lock()
	s.ixGen++
	s.index.Store(nil)
	s.ixMu.Unlock()
}

// maybe reports whether the package may have stored advisories: always
// while the index is built or an import runs, else when its hash is in
// the index. ponytail: 8 bytes per package with advisories (a sorted hash
// set, ~3 MiB for all of OSV's covered ecosystems); a hash collision only
// costs a database lookup.
func (s *Service) maybe(eco, key string) bool {
	if s.importing.Load() {
		return true
	}
	ix := s.index.Load()
	if ix == nil {
		s.buildIndex()
		return true
	}
	h := hashKey(eco, key)
	if _, ok := slices.BinarySearch(*ix, h); ok {
		return true
	}
	_, ok := s.extra.Load(h)
	return ok
}

func (s *Service) buildIndex() {
	if !s.building.CompareAndSwap(false, true) {
		return
	}
	s.ixMu.Lock()
	gen, closed := s.ixGen, s.closed
	if !closed {
		s.builds.Add(1)
	}
	s.ixMu.Unlock()
	if closed {
		s.building.Store(false)
		return
	}
	go func() {
		defer s.builds.Done()
		defer s.building.Store(false)
		var hs []uint64
		if err := s.DB.WalkVulnPackages(context.Background(), func(e, p string) { hs = append(hs, hashKey(e, p)) }); err != nil {
			s.log().Warn("could not build the vulnerability index; lookups read the database", "err", logsafe.Err(err))
			return
		}
		slices.Sort(hs)
		s.ixMu.Lock()
		if s.ixGen == gen {
			s.index.Store(&hs)
		}
		s.ixMu.Unlock()
	}()
}

// Close waits for an index build in progress; call it before the
// database closes. Lookups afterwards read the database.
func (s *Service) Close() {
	s.ixMu.Lock()
	s.closed = true
	s.ixMu.Unlock()
	s.builds.Wait()
}

// ---- lookups ----

// Vulnerability is one advisory affecting a component.
type Vulnerability struct {
	ID        string   `json:"id"`
	Aliases   []string `json:"aliases"`
	Summary   string   `json:"summary"`
	Severity  string   `json:"severity"` // critical, high, medium, low, none, unknown
	Score     *float64 `json:"score"`    // CVSS base score; null when unknown
	Vector    string   `json:"vector,omitempty"`
	FixedIn   []string `json:"fixedIn"`
	Published string   `json:"published"`
	Modified  string   `json:"modified"`
	URL       string   `json:"url"`
	Malicious bool     `json:"malicious"`
}

// Result is what a lookup found for one package. Status is "ok", "unknown"
// (the online lookup failed: local data only) or "disabled".
type Result struct {
	Covered         bool            `json:"covered"`
	Ecosystem       string          `json:"ecosystem"`
	Status          string          `json:"status"`
	Vulnerabilities []Vulnerability `json:"vulnerabilities"`
}

// MaxSeverity is the highest severity of r's vulnerabilities ("" when none).
func (r Result) MaxSeverity() string {
	best := ""
	for _, v := range r.Vulnerabilities {
		if best == "" || Rank(v.Severity) > Rank(best) {
			best = v.Severity
		}
	}
	return best
}

// Malicious reports a malicious-package advisory among r's.
func (r Result) Malicious() bool {
	return slices.ContainsFunc(r.Vulnerabilities, func(v Vulnerability) bool { return v.Malicious })
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func osvURL(id string) string { return "https://osv.dev/vulnerability/" + url.PathEscape(id) }

func vulnOf(a meta.VulnAdvisory, fixed []string) Vulnerability {
	v := Vulnerability{ID: a.ID, Aliases: a.Aliases, Summary: a.Summary, Severity: a.Severity, Vector: a.Vector,
		FixedIn: fixed, Published: rfc3339(a.Published), Modified: rfc3339(a.Modified), URL: osvURL(a.ID), Malicious: a.Malicious}
	if v.Aliases == nil {
		v.Aliases = []string{}
	}
	if v.FixedIn == nil {
		v.FixedIn = []string{}
	}
	if v.Severity == "" {
		v.Severity = "unknown"
	}
	if a.Score10 >= 0 {
		f := float64(a.Score10) / 10
		v.Score = &f
	}
	return v
}

// bare is an advisory the online API named but whose details could not be read.
func bare(id string) Vulnerability {
	return Vulnerability{ID: id, Aliases: []string{}, Severity: "unknown", FixedIn: []string{}, URL: osvURL(id),
		Malicious: strings.HasPrefix(id, "MAL-")}
}

// Check looks up pkgs: the stored advisories whose ranges hold each
// version, and in online mode (when network is set) the API's answer,
// from the cache while it is younger than cacheHours. An online failure is
// no error: the results are the local ones, with status "unknown".
func (s *Service) Check(ctx context.Context, pkgs []Package, network bool) ([]Result, error) {
	set := s.Settings(ctx)
	out := make([]Result, len(pkgs))
	if !set.Enabled {
		for i, p := range pkgs {
			out[i] = Result{Covered: true, Ecosystem: p.Ecosystem, Status: "disabled", Vulnerabilities: []Vulnerability{}}
		}
		return out, nil
	}
	online := network && set.Mode == "online"
	ids := make([][]string, len(pkgs))
	var ask []int
	for i, p := range pkgs {
		local, err := s.local(ctx, p)
		if err != nil {
			return nil, err
		}
		out[i] = Result{Covered: true, Ecosystem: p.Ecosystem, Status: "ok", Vulnerabilities: local}
		if set.Mode != "online" {
			continue
		}
		q, found, err := s.DB.GetVulnQuery(ctx, p.Ecosystem, p.key(), p.Version)
		if err != nil {
			return nil, err
		}
		switch {
		case found && time.Since(q.Checked) < time.Duration(set.CacheHours)*time.Hour:
			ids[i] = q.IDs
		case found: // stale: used when it cannot be refreshed
			ids[i] = q.IDs
			if online {
				ask = append(ask, i)
			}
		case online:
			ask = append(ask, i)
		}
	}
	if len(ask) > 0 {
		sub := make([]Package, len(ask))
		for j, i := range ask {
			sub[j] = pkgs[i]
		}
		got, err := s.queryOnline(ctx, set, sub)
		if err != nil {
			s.noteFailure(err)
			for _, i := range ask {
				out[i].Status = "unknown"
			}
		} else {
			for j, i := range ask {
				ids[i] = got[j]
			}
		}
	}
	for i, p := range pkgs {
		for _, id := range ids[i] {
			if slices.ContainsFunc(out[i].Vulnerabilities, func(v Vulnerability) bool { return v.ID == id }) {
				continue
			}
			v, err := s.stored(ctx, id, p)
			if err != nil {
				return nil, err
			}
			if v != nil {
				out[i].Vulnerabilities = append(out[i].Vulnerabilities, *v)
			}
		}
		sortVulns(out[i].Vulnerabilities)
	}
	return out, nil
}

// Lookup is Check of one package, online within lookupTimeout: what a
// download or the UI waits for.
func (s *Service) Lookup(ctx context.Context, p Package) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	res, err := s.Check(ctx, []Package{p}, true)
	if err != nil {
		return Result{}, err
	}
	return res[0], nil
}

func sortVulns(vs []Vulnerability) {
	slices.SortFunc(vs, func(a, b Vulnerability) int {
		switch {
		case a.Malicious != b.Malicious:
			if a.Malicious {
				return -1
			}
			return 1
		case Rank(a.Severity) != Rank(b.Severity):
			return Rank(b.Severity) - Rank(a.Severity)
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// local evaluates the stored advisories of p.
func (s *Service) local(ctx context.Context, p Package) ([]Vulnerability, error) {
	out := []Vulnerability{}
	if !s.maybe(p.Ecosystem, p.key()) {
		return out, nil
	}
	rows, err := s.DB.VulnAdvisoriesFor(ctx, p.Ecosystem, p.key())
	if err != nil {
		return nil, err
	}
	for _, a := range rows {
		if !a.Withdrawn.IsZero() {
			continue
		}
		hit, fixed, err := Affects(p.Ecosystem, p.Version, a.Affected)
		if err != nil {
			s.log().Warn("stored advisory does not decode; it is ignored", "id", a.ID, "err", err)
			continue
		}
		if hit {
			out = append(out, vulnOf(a, fixed))
		}
	}
	return out, nil
}

// stored is advisory id, which the API says affects p, from the database:
// nil when it is withdrawn, bare when it is not stored.
func (s *Service) stored(ctx context.Context, id string, p Package) (*Vulnerability, error) {
	a, err := s.DB.VulnAdvisory(ctx, id, p.Ecosystem, p.key())
	if errors.Is(err, meta.ErrNotFound) {
		v := bare(id)
		return &v, nil
	}
	if err != nil {
		return nil, err
	}
	if !a.Withdrawn.IsZero() {
		return nil, nil
	}
	var fixed []string
	if a.Ecosystem == p.Ecosystem && a.Package == p.key() {
		_, fixed, _ = Affects(p.Ecosystem, p.Version, a.Affected)
	}
	v := vulnOf(a, fixed)
	return &v, nil
}

func (s *Service) noteFailure(err error) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	s.failures++
	s.failure = &Failure{At: time.Now().UTC(), Message: err.Error()}
	if time.Since(s.loggedAt) > time.Minute { // one line a minute while the API is down
		s.loggedAt = time.Now()
		s.log().Warn("OSV lookup failed; components are served and reported with the local data only (status unknown)",
			"err", logsafe.Err(err), "failures", s.failures)
	}
}

// Limits of the online API's answers.
const (
	batchSize        = 1000 // queries per querybatch, the API's limit
	maxPages         = 10   // next_page_token rounds per query
	maxBatchResponse = 64 << 20
	maxEntryBytes    = 16 << 20 // one advisory, online or in a dump
	detailFetches    = 8        // GET /v1/vulns at once
)

type osvQuery struct {
	Package struct {
		Name      string `json:"name"`
		Ecosystem string `json:"ecosystem"`
	} `json:"package"`
	Version   string `json:"version"`
	PageToken string `json:"page_token,omitempty"`
}

type osvBatchAnswer struct {
	Results []struct {
		Vulns []struct {
			ID       string `json:"id"`
			Modified string `json:"modified"`
		} `json:"vulns"`
		NextPageToken string `json:"next_page_token"`
	} `json:"results"`
}

var errNotFound = errors.New("not found")

// call sends one API request and decodes its JSON answer into out.
func (s *Service) call(ctx context.Context, c *http.Client, method, target string, body any, out any, limit int64) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", proxy.UserAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%s %s: status %d", method, req.URL.Path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(out); err != nil {
		return fmt.Errorf("%s %s: %w", method, req.URL.Path, err)
	}
	return nil
}

// queryOnline asks the API which advisories affect pkgs, reads the
// details of those not stored (or changed since), stores them and the
// answers, and returns the ids per package.
func (s *Service) queryOnline(ctx context.Context, set Settings, pkgs []Package) ([][]string, error) {
	c, err := s.client(set.APIURL)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(set.APIURL, "/")
	ids := make([][]string, len(pkgs))
	modified := map[string]time.Time{}
	tokens := make([]string, len(pkgs))
	pending := make([]int, len(pkgs))
	for i := range pending {
		pending[i] = i
	}
	for page := 0; len(pending) > 0 && page < maxPages; page++ {
		var next []int
		for start := 0; start < len(pending); start += batchSize {
			chunk := pending[start:min(start+batchSize, len(pending))]
			qs := make([]osvQuery, len(chunk))
			for j, i := range chunk {
				qs[j].Package.Name, qs[j].Package.Ecosystem, qs[j].Version, qs[j].PageToken = pkgs[i].Name, pkgs[i].Ecosystem, pkgs[i].Version, tokens[i]
			}
			var ans osvBatchAnswer
			if err := s.call(ctx, c, http.MethodPost, base+"/v1/querybatch", map[string]any{"queries": qs}, &ans, maxBatchResponse); err != nil {
				return nil, err
			}
			if len(ans.Results) != len(chunk) {
				return nil, fmt.Errorf("querybatch answered %d results for %d queries", len(ans.Results), len(chunk))
			}
			for j, i := range chunk {
				for _, v := range ans.Results[j].Vulns {
					if idRE.MatchString(v.ID) && !slices.Contains(ids[i], v.ID) {
						ids[i] = append(ids[i], v.ID)
						modified[v.ID] = parseTime(v.Modified)
					}
				}
				if tokens[i] = ans.Results[j].NextPageToken; tokens[i] != "" {
					next = append(next, i)
				}
			}
		}
		pending = next
	}
	if err := s.fetchDetails(ctx, c, base, modified); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	qs := make([]meta.VulnQuery, len(pkgs))
	for i, p := range pkgs {
		qs[i] = meta.VulnQuery{Ecosystem: p.Ecosystem, Package: p.key(), Version: p.Version, IDs: ids[i], Checked: now}
	}
	return ids, s.DB.PutVulnQueries(ctx, qs)
}

// fetchDetails reads and stores the advisories in modified (id -> the
// modification time the API gave) that are not stored or older there. An
// advisory that cannot be read is left out: it is reported by id alone.
func (s *Service) fetchDetails(ctx context.Context, c *http.Client, base string, modified map[string]time.Time) error {
	var need []string
	for id, mod := range modified {
		a, err := s.DB.VulnAdvisory(ctx, id, "", "")
		switch {
		case errors.Is(err, meta.ErrNotFound):
			need = append(need, id)
		case err != nil:
			return err
		case !mod.IsZero() && a.Modified.Before(mod):
			need = append(need, id)
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(detailFetches)
	for _, id := range need {
		g.Go(func() error {
			var e Entry
			if err := s.call(gctx, c, http.MethodGet, base+"/v1/vulns/"+url.PathEscape(id), nil, &e, maxEntryBytes); err != nil {
				s.log().Warn("could not read an OSV advisory; it is reported by id only", "id", id, "err", logsafe.Err(err))
				return nil
			}
			if e.ID != id {
				return nil
			}
			rows := e.Rows()
			if err := s.DB.ReplaceVulnAdvisories(gctx, []string{id}, rows); err != nil {
				return err
			}
			for _, r := range rows {
				s.extra.Store(hashKey(r.Ecosystem, r.Package), struct{}{})
			}
			return nil
		})
	}
	return g.Wait()
}

// ---- the firewall ----

var allowREs sync.Map // pattern -> *regexp.Regexp

// allowListed reports whether coordinate (name@version) matches an entry
// of list. Entries are validated on write; one that does not compile is
// skipped.
func allowListed(list []string, coordinate string) bool {
	for _, p := range list {
		re, ok := allowREs.Load(p)
		if !ok {
			c, err := regexp.Compile(p)
			if err != nil {
				continue
			}
			re, _ = allowREs.LoadOrStore(p, c)
		}
		if re.(*regexp.Regexp).MatchString(coordinate) {
			return true
		}
	}
	return false
}

// Blocking returns the vulnerabilities of r that policy fw refuses: the
// malicious ones unless blockMalicious is off, and those rated at least
// blockSeverityAtLeast ("none" or unset refuses none by severity; an
// unknown severity never reaches a threshold).
func Blocking(fw *config.Firewall, r Result) []Vulnerability {
	var out []Vulnerability
	threshold := Rank(fw.BlockSeverityAtLeast)
	for _, v := range r.Vulnerabilities {
		if (v.Malicious && fw.MalwareBlocked()) || (threshold > 0 && Rank(v.Severity) >= threshold) {
			out = append(out, v)
		}
	}
	return out
}

// Firewall decides whether the file at path of repository repoName may be
// served (repo.Store.Firewall): a proxy with an enabled firewall refuses,
// with 403, a file of a component its policy quarantines, whether the
// file was just downloaded or cached long ago. Everything else is served:
// hosted and group repositories, files that are no component, components
// on the allow list, and any lookup that fails (fail open; the online
// lookup's failure leaves the local data, which still blocks).
func (s *Service) Firewall(ctx context.Context, repoName, path string) error {
	rp := s.Repos.Get(repoName)
	if rp == nil || rp.Type != "proxy" || rp.Firewall == nil || !rp.Firewall.Enabled {
		return nil
	}
	group, name, version, ok := component.DeriveAsset(rp.Format, path)
	if !ok {
		return nil
	}
	p, ok := FromComponent(rp.Format, group, name, version)
	if !ok || allowListed(rp.Firewall.AllowList, p.Coordinate()) {
		return nil
	}
	res, err := s.Lookup(ctx, p)
	if err != nil {
		s.log().Warn("firewall lookup failed; the file is served", "repo", repoName, "component", p.Coordinate(), "err", logsafe.Err(err))
		return nil
	}
	block := Blocking(rp.Firewall, res)
	if len(block) == 0 {
		return nil
	}
	ids := make([]string, len(block))
	for i, v := range block {
		ids[i] = v.ID
	}
	sev := Result{Vulnerabilities: block}.MaxSeverity()
	created, err := s.DB.Quarantine(context.WithoutCancel(ctx), meta.QuarantineItem{Repo: rp.Name, Name: p.Name, Version: p.Version,
		Ecosystem: p.Ecosystem, Path: path, IDs: ids, Severity: sev, Malicious: res.Malicious(), Last: time.Now().UTC()})
	if err != nil {
		s.log().Warn("could not record a quarantined component", "repo", repoName, "component", p.Coordinate(), "err", logsafe.Err(err))
	}
	if created {
		s.emit("firewall.blocked", auth.FromContext(ctx).Owner(), rp.Name, p.Coordinate(), truncate(strings.Join(ids, ","), 500))
	}
	return &repo.Error{Status: http.StatusForbidden, Msg: refusal(p, rp.Name, block)}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// refusal is the 403 body: the component, the repository and the
// advisories, which clients print.
func refusal(p Package, repoName string, vs []Vulnerability) string {
	var parts []string
	for i, v := range vs {
		if i == 10 {
			parts = append(parts, fmt.Sprintf("and %d more", len(vs)-10))
			break
		}
		why := v.Severity
		if v.Score != nil {
			why += fmt.Sprintf(" %.1f", *v.Score)
		}
		if v.Malicious {
			why = "malicious package"
		}
		parts = append(parts, v.ID+" ("+why+")")
	}
	return fmt.Sprintf("%s %s is quarantined by the firewall of repository %q: %s; see %s",
		p.Ecosystem, p.Coordinate(), repoName, strings.Join(parts, ", "), osvURL(vs[0].ID))
}

// AllowEntry is the allow-list entry that releases one component.
func AllowEntry(name, version string) string { return "^" + regexp.QuoteMeta(name+"@"+version) + "$" }

// Release lets proxy repoName serve name@version again: its allow list
// gets AllowEntry (once) and the quarantine row goes.
func (s *Service) Release(ctx context.Context, repoName, name, version string) (string, error) {
	rp := s.Repos.Get(repoName)
	if rp == nil {
		return "", repo.Errorf(http.StatusNotFound, "repository %q not found", repoName)
	}
	if rp.Type != "proxy" || rp.Firewall == nil {
		return "", repo.Errorf(http.StatusBadRequest, "repository %q has no firewall", repoName)
	}
	entry := AllowEntry(name, version)
	if !slices.Contains(rp.Firewall.AllowList, entry) {
		def := rp.Repository
		fw := *def.Firewall
		fw.AllowList = append(slices.Clone(fw.AllowList), entry)
		def.Firewall = &fw
		if _, err := s.Repos.UpdateDef(ctx, repoName, def, repo.ClearSecrets{KeepCleanupPolicy: true}); err != nil {
			return "", err
		}
	}
	if err := s.DB.DeleteQuarantine(ctx, repoName, name, version); err != nil && !errors.Is(err, meta.ErrNotFound) {
		return "", err
	}
	return entry, nil
}

// ---- status ----

// Status describes the local database and the online lookups.
type Status struct {
	Enabled        bool             `json:"enabled"`
	Mode           string           `json:"mode"`
	Advisories     int64            `json:"advisories"`
	Malicious      int64            `json:"malicious"`
	ByEcosystem    map[string]int64 `json:"byEcosystem"`
	LastImport     *Database        `json:"lastImport"`
	Running        bool             `json:"running"` // an import or sync
	LookupFailures int64            `json:"lookupFailures"`
	LastFailure    *Failure         `json:"lastFailure"`
}

// Status reports the stored data and the lookups since start.
func (s *Service) Status(ctx context.Context) (Status, error) {
	set := s.Settings(ctx)
	by, mal, err := s.DB.VulnStats(ctx)
	if err != nil {
		return Status{}, err
	}
	st := Status{Enabled: set.Enabled, Mode: set.Mode, ByEcosystem: by, Malicious: mal, Running: s.importing.Load()}
	for _, n := range by {
		st.Advisories += n
	}
	var db Database
	if found, err := s.DB.GetSettingJSON(ctx, databaseKey, &db); err != nil {
		return Status{}, err
	} else if found {
		st.LastImport = &db
	}
	s.failMu.Lock()
	st.LookupFailures, st.LastFailure = s.failures, s.failure
	s.failMu.Unlock()
	return st, nil
}

// ---- the scan ----

// ScanTotals counts what a scan did.
type ScanTotals struct {
	Repositories, Components, Vulnerable, Unchecked int
}

const (
	scanPage  = 1000 // assets read at once
	scanBatch = 500  // components looked up at once
	scanSeen  = 50000
)

// Scan looks up every component of the hosted and proxy repositories of a
// covered format (only the one named, unless only is "") and stores the
// findings; logf reports progress.
func (s *Service) Scan(ctx context.Context, only string, logf func(format string, args ...any)) (ScanTotals, error) {
	var t ScanTotals
	if !s.Settings(ctx).Enabled {
		return t, errors.New("vulnerability data is disabled in the settings")
	}
	for _, rp := range s.Repos.Snapshot().Sorted {
		if (only != "" && rp.Name != only) || (rp.Type != "hosted" && rp.Type != "proxy") || Ecosystems[rp.Format] == "" {
			continue
		}
		n, vuln, unchecked, err := s.scanRepo(ctx, rp)
		if err != nil {
			return t, fmt.Errorf("repository %q: %w", rp.Name, err)
		}
		logf("%s: %d components, %d vulnerable, %d not checked online", rp.Name, n, vuln, unchecked)
		t.Repositories++
		t.Components, t.Vulnerable, t.Unchecked = t.Components+n, t.Vulnerable+vuln, t.Unchecked+unchecked
	}
	return t, nil
}

func (s *Service) scanRepo(ctx context.Context, rp *repo.Repository) (n, vulnerable, unchecked int, err error) {
	// Rows this scan writes are stamped at or after start, which is past the
	// millisecond any earlier scan stamped: the prune at the end keeps them only.
	start := time.Now().UTC().Truncate(time.Millisecond).Add(time.Millisecond)
	// ponytail: duplicates are dropped within scanSeen components; past it
	// one may be looked up twice (the second write replaces the first).
	seen := map[Package]bool{}
	var batch []Package
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		res, err := s.Check(ctx, batch, true)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if now.Before(start) {
			now = start
		}
		fs := make([]meta.VulnFinding, len(res))
		for i, r := range res {
			ids := make([]string, len(r.Vulnerabilities))
			for j, v := range r.Vulnerabilities {
				ids[j] = v.ID
			}
			sev := r.MaxSeverity()
			fs[i] = meta.VulnFinding{Repo: rp.Name, Ecosystem: batch[i].Ecosystem, Package: batch[i].key(), Version: batch[i].Version,
				Name: batch[i].Coordinate(), IDs: ids, Severity: sev, Rank: Rank(sev), Malicious: r.Malicious(), Unchecked: r.Status == "unknown", Scanned: now}
			n++
			if len(ids) > 0 {
				vulnerable++
			}
			if r.Status == "unknown" {
				unchecked++
			}
		}
		batch = batch[:0]
		return s.DB.PutVulnFindings(ctx, fs)
	}
	after := ""
	for {
		page, err := s.DB.ScanAssets(ctx, rp.Name, after, scanPage)
		if err != nil {
			return n, vulnerable, unchecked, err
		}
		for _, a := range page {
			g, name, v, ok := component.DeriveAsset(rp.Format, a.Path)
			if !ok {
				continue
			}
			p, ok := FromComponent(rp.Format, g, name, v)
			if !ok || seen[p] {
				continue
			}
			if len(seen) >= scanSeen {
				clear(seen)
			}
			seen[p] = true
			if batch = append(batch, p); len(batch) >= scanBatch {
				if err := flush(); err != nil {
					return n, vulnerable, unchecked, err
				}
			}
		}
		if len(page) < scanPage {
			break
		}
		after = page[len(page)-1].Path
	}
	if err := flush(); err != nil {
		return n, vulnerable, unchecked, err
	}
	return n, vulnerable, unchecked, s.DB.PruneVulnFindings(ctx, rp.Name, start)
}
