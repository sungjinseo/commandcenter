package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/cleanup"
	"commandcenter/internal/component"
	"commandcenter/internal/format/maven"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// ---- maven.purge-snapshots and maven.purge-unused-snapshots ----

// TypeMavenPurgeUnused removes the Maven SNAPSHOT builds not used for a while
// (Nexus's repository.maven.purge-unused-snapshots).
const TypeMavenPurgeUnused = "maven.purge-unused-snapshots"

// Scopes of maven.purge-snapshots.
const (
	// ScopeBuild is Nexus's "Maven - Delete SNAPSHOT": within each X-SNAPSHOT
	// version, the newest minimumRetained builds are kept (-1: none is
	// removed this way) and older builds go when they are older than
	// snapshotRetentionDays (0: any age). With removeIfReleased, every build
	// of X-SNAPSHOT older than gracePeriodDays goes once release X is in any
	// Maven repository. Hosted and proxy repositories, groups (their
	// members) and "*" (every one) are taken; RELEASE ones are skipped.
	ScopeBuild = "build"
	// ScopeVersion is the scope of tasks created before 0.3 (the upgrade
	// stamps them with it): per GA, the newest minimumRetained SNAPSHOT
	// versions are kept, older versions go whole, and with removeIfReleased
	// X-SNAPSHOT goes once release X has been in the same repository for
	// gracePeriodDays. One Maven hosted repository.
	ScopeVersion = "version"
)

// purgeParams selects the SNAPSHOT builds or versions to remove. With
// lastDownloadedDays, a build (or version) goes by age only when it was not
// downloaded (or, never downloaded, not stored) for that long either.
// preview only logs what would go.
type purgeParams struct {
	Repository            string `json:"repository"`
	Scope                 string `json:"scope,omitempty"` // "" in a stored task: ScopeVersion
	MinimumRetained       int    `json:"minimumRetained"`
	SnapshotRetentionDays int    `json:"snapshotRetentionDays"`
	LastDownloadedDays    int    `json:"lastDownloadedDays,omitempty"`
	RemoveIfReleased      bool   `json:"removeIfReleased,omitempty"`
	GracePeriodDays       int    `json:"gracePeriodDays,omitempty"`
	Preview               bool   `json:"preview,omitempty"`
	// Unused is maven.purge-unused-snapshots: hosted repositories only.
	Unused bool `json:"unused,omitempty"`
}

// Defaults of purgeParams.
const (
	DefaultMinimumRetained = 1
	DefaultSnapshotDays    = 30
)

var purgeFields = []ParamSpec{
	{Name: "repository", Type: "repository", Required: true, Help: `a Maven hosted or proxy repository, a group (its members) or "*" (every one); not RELEASE`},
	{Name: "scope", Type: "string", Default: ScopeBuild, Help: `"build": the builds within each SNAPSHOT version (Nexus); "version": whole SNAPSHOT versions per GA, one hosted repository`},
	{Name: "minimumRetained", Type: "int", Default: DefaultMinimumRetained, Help: "newest builds kept per SNAPSHOT version (scope version: versions per GA); -1: remove none by age"},
	{Name: "snapshotRetentionDays", Type: "int", Default: DefaultSnapshotDays, Help: "remove older ones than this; 0: any age"},
	{Name: "lastDownloadedDays", Type: "int", Default: 0, Help: "only ones not downloaded for this long; 0: ignore"},
	{Name: "removeIfReleased", Type: "bool", Default: false, Help: "remove X-SNAPSHOT once release X exists (scope build: in any Maven repository)"},
	{Name: "gracePeriodDays", Type: "int", Default: 0, Help: "with removeIfReleased: days the snapshot (scope version: the release) must be old"},
	{Name: "preview", Type: "bool", Default: false, Help: "only log what would be removed"},
}

var mavenPurgeUnusedFields = []ParamSpec{
	{Name: "repository", Type: "repository", Required: true, Help: `a Maven hosted repository, a group (its hosted members) or "*"; not RELEASE`},
	{Name: "lastUsed", Type: "int", Default: 1, Help: "remove SNAPSHOT builds not downloaded (never downloaded: not stored) for this many days, at least 1"},
	{Name: "preview", Type: "bool", Default: false, Help: "only log what would be removed"},
}

func purgeParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	// Pointers tell an absent count from an explicit 0.
	var in struct {
		Repository            string `json:"repository"`
		Scope                 string `json:"scope"`
		MinimumRetained       *int   `json:"minimumRetained"`
		SnapshotRetentionDays *int   `json:"snapshotRetentionDays"`
		LastDownloadedDays    int    `json:"lastDownloadedDays"`
		RemoveIfReleased      bool   `json:"removeIfReleased"`
		GracePeriodDays       int    `json:"gracePeriodDays"`
		Preview               bool   `json:"preview"`
	}
	if err := decodeParams(raw, &in); err != nil {
		return "", []string{err.Error()}
	}
	p := purgeParams{Repository: in.Repository, Scope: in.Scope, MinimumRetained: DefaultMinimumRetained, SnapshotRetentionDays: DefaultSnapshotDays,
		LastDownloadedDays: in.LastDownloadedDays, RemoveIfReleased: in.RemoveIfReleased, GracePeriodDays: in.GracePeriodDays, Preview: in.Preview}
	if p.Scope == "" {
		p.Scope = ScopeBuild
	}
	if p.GracePeriodDays < 0 { // Nexus's default, -1, is no grace period
		p.GracePeriodDays = 0
	}
	if in.MinimumRetained != nil {
		p.MinimumRetained = *in.MinimumRetained
	}
	if in.SnapshotRetentionDays != nil {
		p.SnapshotRetentionDays = *in.SnapshotRetentionDays
	}
	var errs []string
	min := map[string]int{"minimumRetained": 0, "snapshotRetentionDays": 0, "lastDownloadedDays": 0, "gracePeriodDays": 0}
	if p.Scope == ScopeBuild {
		min["minimumRetained"] = -1
	}
	for name, v := range map[string]int{"minimumRetained": p.MinimumRetained, "snapshotRetentionDays": p.SnapshotRetentionDays,
		"lastDownloadedDays": p.LastDownloadedDays, "gracePeriodDays": p.GracePeriodDays} {
		if v < min[name] {
			errs = append(errs, fmt.Sprintf("params.%s must be at least %d", name, min[name]))
		}
	}
	sort.Strings(errs)
	if p.Scope != ScopeBuild && p.Scope != ScopeVersion {
		errs = append(errs, fmt.Sprintf("params.scope %q is not %q or %q", p.Scope, ScopeBuild, ScopeVersion))
	}
	if len(errs) == 0 {
		errs = s.checkPurgeRepo(p, check)
	}
	if len(errs) > 0 {
		return "", errs
	}
	return encodeParams(p), nil
}

// mavenPurgeUnusedParamsOf maps maven.purge-unused-snapshots (Nexus's "Maven -
// Delete unused SNAPSHOT") to purgeParams: every build, whatever its
// age, that was not used for lastUsed days.
func mavenPurgeUnusedParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	var in struct {
		Repository string `json:"repository"`
		LastUsed   *int   `json:"lastUsed"`
		Preview    bool   `json:"preview"`
	}
	if err := decodeParams(raw, &in); err != nil {
		return "", []string{err.Error()}
	}
	p := purgeParams{Repository: in.Repository, Scope: ScopeBuild, MinimumRetained: 0, LastDownloadedDays: 1, Preview: in.Preview, Unused: true}
	if in.LastUsed != nil {
		p.LastDownloadedDays = *in.LastUsed
	}
	if p.LastDownloadedDays < 1 {
		return "", []string{"params.lastUsed must be at least 1"}
	}
	if errs := s.checkPurgeRepo(p, check); len(errs) > 0 {
		return "", errs
	}
	return encodeParams(p), nil
}

// checkPurgeRepo checks the repository a purge names.
func (s *Service) checkPurgeRepo(p purgeParams, check bool) []string {
	rp := s.Repos.Get(p.Repository)
	switch {
	case p.Repository == "":
		return []string{"params.repository is required"}
	case !check:
	case p.Repository == "*" && p.Scope == ScopeBuild:
	case rp == nil:
		return []string{fmt.Sprintf("repository %q does not exist", p.Repository)}
	case p.Scope == ScopeVersion && (rp.Format != "maven" || rp.Type != "hosted"):
		return []string{fmt.Sprintf("repository %q is %s %s: scope version takes a Maven hosted repository", p.Repository, rp.Format, rp.Type)}
	case rp.Format != "maven" || p.Unused && rp.Type == "proxy":
		return []string{fmt.Sprintf("repository %q is %s %s: the task takes a Maven %s repository or group", p.Repository, rp.Format, rp.Type, unusedKinds(p.Unused))}
	case rp.VersionPolicy == "release" && p.Scope == ScopeBuild:
		return []string{fmt.Sprintf("repository %q has version policy RELEASE: it holds no snapshots", p.Repository)}
	case rp.WritePolicy == "deny":
		return []string{fmt.Sprintf("repository %q is read-only (writePolicy deny)", p.Repository)}
	}
	return nil
}

func unusedKinds(unused bool) string {
	if unused {
		return "hosted"
	}
	return "hosted or proxy"
}

// purgeTargets resolves the repository a purge names: the repository, the
// leaf members of a group, or every Maven repository for "*", keeping the
// hosted (and, unless unused, proxy) ones that may hold snapshots.
func (s *Service) purgeTargets(p purgeParams, log *RunLog) ([]*repo.Repository, error) {
	var cands []*repo.Repository
	switch rp := s.Repos.Get(p.Repository); {
	case p.Repository == "*":
		cands = s.Repos.Snapshot().Sorted
	case rp == nil:
		return nil, fmt.Errorf("repository %q no longer exists: edit or delete this task", p.Repository)
	case rp.Format != "maven":
		return nil, fmt.Errorf("repository %q is now %s, not Maven: edit or delete this task", p.Repository, rp.Format)
	case rp.Type == "group":
		cands = leafMembers(rp)
	case p.Scope == ScopeVersion && rp.Type != "hosted":
		return nil, fmt.Errorf("repository %q is now %s %s, not Maven hosted: edit or delete this task", p.Repository, rp.Format, rp.Type)
	case rp.WritePolicy == "deny":
		return nil, fmt.Errorf("repository %q is read-only (writePolicy deny): nothing removed", p.Repository)
	default:
		cands = []*repo.Repository{rp}
	}
	var out []*repo.Repository
	for _, rp := range cands {
		switch {
		case rp.Format != "maven" || rp.Type == "group" || p.Unused && rp.Type != "hosted":
		case rp.VersionPolicy == "release" && p.Scope == ScopeBuild || rp.WritePolicy == "deny":
			if p.Repository != "*" {
				log.Infof("%s: skipped: version policy RELEASE or read-only", rp.Name)
			}
		default:
			out = append(out, rp)
		}
	}
	return out, nil
}

// leafMembers flattens nested groups into their non-group members, each once.
func leafMembers(g *repo.Repository) []*repo.Repository {
	var out []*repo.Repository
	seen := map[string]bool{}
	var walk func(*repo.Repository)
	walk = func(r *repo.Repository) {
		if seen[r.Name] {
			return
		}
		seen[r.Name] = true
		if r.Type != "group" {
			out = append(out, r)
			return
		}
		for _, m := range r.MemberRepos {
			walk(m)
		}
	}
	walk(g)
	return out
}

// snapBuild is what the scan saw of one SNAPSHOT build (a component).
type snapBuild struct {
	version  string
	assets   []component.Asset
	paths    map[string]bool
	created  time.Time // newest asset
	modified time.Time
	lastDl   time.Time
	bytes    int64
}

func (b *snapBuild) add(a meta.ScanAsset) {
	b.assets = append(b.assets, component.Asset{Path: a.Path, Size: a.Size})
	b.paths[a.Path] = true
	b.bytes += a.Size
	if a.Created.After(b.created) {
		b.created = a.Created
	}
	if a.Modified.After(b.modified) {
		b.modified = a.Modified
	}
	if a.LastDownloaded.After(b.lastDl) {
		b.lastDl = a.LastDownloaded
	}
}

// lastUsed is when the build was last downloaded, or stored if never.
func (b *snapBuild) lastUsed() time.Time {
	if b.lastDl.IsZero() {
		return b.created
	}
	return b.lastDl
}

// snapVersion is one X-SNAPSHOT version of a GA: its builds.
type snapVersion struct {
	version string
	builds  map[string]*snapBuild
	created time.Time // newest asset
}

// lastUsed is when a file of the version was last downloaded, or its
// newest file stored if none ever was.
func (v *snapVersion) lastUsed() time.Time {
	var t time.Time
	for _, b := range v.builds {
		if b.lastDl.After(t) {
			t = b.lastDl
		}
	}
	if t.IsZero() {
		return v.created
	}
	return t
}

type purgeGA struct {
	group, name string
	releases    map[string]time.Time // version -> newest asset
	snaps       map[string]*snapVersion
}

// purgeCounts adds up a run.
type purgeCounts struct {
	removed, total, kept, failed, assets int
	bytes                                int64
}

func runPurge(ctx context.Context, s *Service, t meta.Task, actor string, log *RunLog) (summary, error) {
	var p purgeParams
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %w", err)
	}
	if p.Scope == "" {
		p.Scope = ScopeVersion
	}
	if s.Deleters == nil {
		return summary{msg: "component deletion is not available"}, errors.New("component deletion is not available")
	}
	repos, err := s.purgeTargets(p, log)
	if err != nil {
		return summary{}, err
	}
	if s.Metrics != nil && p.LastDownloadedDays > 0 {
		s.flushOthers(ctx)
		if err := s.Metrics.Flush(ctx); err != nil {
			return summary{}, fmt.Errorf("could not write the pending download counts: %w", err)
		}
	}
	released := s.releaseFinder(ctx)
	var c purgeCounts
	for _, rp := range repos {
		if err := purgeRepo(ctx, s, rp, p, actor, released, &c, log); err != nil {
			return purgeSummary(p, repos, c), err
		}
	}
	sum := purgeSummary(p, repos, c)
	if c.kept > 0 {
		sum.msg += fmt.Sprintf("; kept %d changed since evaluation", c.kept)
	}
	log.Infof("%s", sum.msg)
	if c.failed > 0 {
		return sum, fmt.Errorf("%d deletes failed (see the log)", c.failed)
	}
	return sum, nil
}

// releaseFinder reports whether release version v of GA g:a is in any Maven
// hosted or proxy repository (Nexus looks in every Maven repository).
func (s *Service) releaseFinder(ctx context.Context) func(g, a, v string) bool {
	cache := map[string]bool{}
	return func(g, a, v string) bool {
		key := g + ":" + a + ":" + v
		if found, ok := cache[key]; ok {
			return found
		}
		dir := strings.ReplaceAll(g, ".", "/") + "/" + a + "/" + v + "/"
		found := false
		for _, rp := range s.Repos.Snapshot().Sorted {
			if rp.Format != "maven" || rp.Type == "group" || found {
				continue
			}
			page, err := s.DB.ScanAssets(ctx, rp.Name, dir, 20)
			if err != nil {
				continue
			}
			for _, x := range page {
				if !strings.HasPrefix(x.Path, dir) {
					break
				}
				if xg, xa, xv, ok := component.DeriveMaven(x.Path); ok && xg == g && xa == a && xv == v {
					found = true
					break
				}
			}
		}
		cache[key] = found
		return found
	}
}

// purgeRepo scans one repository and removes what p selects.
func purgeRepo(ctx context.Context, s *Service, rp *repo.Repository, p purgeParams, actor string, released func(g, a, v string) bool,
	c *purgeCounts, log *RunLog) error {
	// Scan every artifact of the repository into its GA.
	limit := s.RebuildScan
	if limit <= 0 {
		limit = maven.DefaultRebuildScan
	}
	gas := map[string]*purgeGA{}
	after, scanned, truncated := "", 0, false
	for !truncated {
		page, err := s.DB.ScanAssets(ctx, rp.Name, after, 1000)
		if err != nil {
			return err
		}
		for _, a := range page {
			if scanned == limit {
				truncated = true
				break
			}
			scanned++
			g, name, v, ok := component.DeriveMaven(a.Path)
			if !ok {
				continue
			}
			ga := gas[g+":"+name]
			if ga == nil {
				ga = &purgeGA{group: g, name: name, releases: map[string]time.Time{}, snaps: map[string]*snapVersion{}}
				gas[g+":"+name] = ga
			}
			base := component.MavenBaseVersion(v)
			if !strings.HasSuffix(base, "-SNAPSHOT") {
				if a.Created.After(ga.releases[v]) {
					ga.releases[v] = a.Created
				}
				continue
			}
			sv := ga.snaps[base]
			if sv == nil {
				sv = &snapVersion{version: base, builds: map[string]*snapBuild{}}
				ga.snaps[base] = sv
			}
			b := sv.builds[v]
			if b == nil {
				b = &snapBuild{version: v, paths: map[string]bool{}}
				sv.builds[v] = b
			}
			b.add(a)
			if a.Created.After(sv.created) {
				sv.created = a.Created
			}
		}
		if len(page) < 1000 {
			break
		}
		after = page[len(page)-1].Path
	}
	if truncated {
		log.Warnf("%s: scanned %d assets and stopped at the budget: later GAs were not evaluated", rp.Name, scanned)
	}

	now := s.now()
	older := func(t time.Time, days int) bool { return days == 0 || t.Before(now.AddDate(0, 0, -days)) }
	keys := make([]string, 0, len(gas))
	for k := range gas {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	verb := "removing"
	if p.Preview {
		verb = "would remove"
	}
	// remove deletes the builds of one unit (a build, or a whole version).
	remove := func(ga *purgeGA, label, reason string, builds []*snapBuild) bool {
		if cur := s.Repos.Get(rp.Name); cur == nil || cur.Gen != rp.Gen {
			log.Warnf("%s: stopped: the repository changed during the run", rp.Name)
			return false
		}
		n, bytes := 0, int64(0)
		for _, b := range builds {
			n, bytes = n+len(b.assets), bytes+b.bytes
		}
		if p.Preview {
			c.removed++
			c.assets += n
			c.bytes += bytes
			log.Infof("%s: %s %s (%s; %d files, %d bytes)", rp.Name, verb, label, reason, n, bytes)
			return true
		}
		var got component.Result
		changed, failed := false, false
		for _, b := range builds {
			comp := component.Component{Repo: rp.Name, Format: "maven", Group: ga.group, Name: ga.name, Version: b.version,
				LastUpdated: b.created, Assets: b.assets,
				Evaluated: &component.Evaluated{Paths: b.paths, MaxCreated: b.created, MaxModified: b.modified, LastDownloaded: b.lastDl}}
			res, err := s.Deleters.Delete(ctx, rp, comp, actor)
			switch {
			case errors.Is(err, cleanup.ErrChanged):
				changed = true
				log.Infof("%s: kept %s: it changed since evaluation (%v)", rp.Name, comp.Label(), err)
			case err != nil:
				failed = true
				log.Errorf("%s: deleting %s: %v", rp.Name, comp.Label(), err)
			default:
				got.Assets += res.Assets
				got.Bytes += res.Bytes
			}
		}
		switch {
		case failed:
			c.failed++
		case changed && got.Assets == 0:
			c.kept++
			return true
		}
		if changed {
			c.kept++
		}
		if got.Assets > 0 {
			c.removed++
			c.assets += got.Assets
			c.bytes += got.Bytes
			log.Infof("%s: removed %s (%s; %d files, %d bytes)", rp.Name, label, reason, got.Assets, got.Bytes)
		}
		return true
	}
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		ga := gas[k]
		if p.Scope == ScopeVersion {
			if !purgeVersions(ga, p, now, older, c, remove) {
				return nil
			}
			continue
		}
		bases := make([]string, 0, len(ga.snaps))
		for v := range ga.snaps {
			bases = append(bases, v)
		}
		sort.Strings(bases)
		for _, base := range bases {
			sv := ga.snaps[base]
			builds := make([]*snapBuild, 0, len(sv.builds))
			for _, b := range sv.builds {
				builds = append(builds, b)
			}
			c.total += len(builds)
			sort.Slice(builds, func(i, j int) bool { return maven.CompareVersions(builds[i].version, builds[j].version) > 0 }) // newest first
			isReleased := p.RemoveIfReleased && released(ga.group, ga.name, strings.TrimSuffix(base, "-SNAPSHOT"))
			kept := 0
			for _, b := range builds {
				reason := ""
				switch {
				case isReleased && older(b.created, p.GracePeriodDays):
					reason = "released"
				case p.MinimumRetained < 0 || kept < p.MinimumRetained || !older(b.created, p.SnapshotRetentionDays) ||
					p.LastDownloadedDays > 0 && !older(b.lastUsed(), p.LastDownloadedDays):
					kept++
					continue
				case p.Unused:
					reason = fmt.Sprintf("not used for %d days", p.LastDownloadedDays)
				case p.SnapshotRetentionDays > 0:
					reason = fmt.Sprintf("older than %d days", p.SnapshotRetentionDays)
				case p.LastDownloadedDays > 0:
					reason = fmt.Sprintf("not downloaded for %d days", p.LastDownloadedDays)
				default:
					reason = fmt.Sprintf("beyond the %d newest", p.MinimumRetained)
				}
				label := component.Component{Format: "maven", Group: ga.group, Name: ga.name, Version: b.version}.Label()
				if !remove(ga, label, reason, []*snapBuild{b}) {
					return nil
				}
			}
		}
	}
	return nil
}

// purgeVersions applies scope version to one GA: whole SNAPSHOT versions,
// newest first by their newest file, the release looked up in the same
// repository and the grace period measured from it. false stops the run.
func purgeVersions(ga *purgeGA, p purgeParams, now time.Time, older func(time.Time, int) bool, c *purgeCounts,
	remove func(*purgeGA, string, string, []*snapBuild) bool) bool {
	vs := make([]*snapVersion, 0, len(ga.snaps))
	for _, sv := range ga.snaps {
		vs = append(vs, sv)
	}
	c.total += len(vs)
	sort.Slice(vs, func(i, j int) bool { // newest first
		if !vs[i].created.Equal(vs[j].created) {
			return vs[i].created.After(vs[j].created)
		}
		return vs[i].version > vs[j].version
	})
	for i, sv := range vs {
		reason := ""
		switch {
		case p.RemoveIfReleased && olderRelease(ga.releases, strings.TrimSuffix(sv.version, "-SNAPSHOT"), now, p.GracePeriodDays):
			reason = "released"
		case i < p.MinimumRetained:
			continue
		case !older(sv.created, p.SnapshotRetentionDays):
			continue
		case p.LastDownloadedDays > 0 && !older(sv.lastUsed(), p.LastDownloadedDays):
			continue
		case p.SnapshotRetentionDays > 0:
			reason = fmt.Sprintf("older than %d days", p.SnapshotRetentionDays)
		case p.LastDownloadedDays > 0:
			reason = fmt.Sprintf("not downloaded for %d days", p.LastDownloadedDays)
		default:
			reason = fmt.Sprintf("beyond the %d newest", p.MinimumRetained)
		}
		builds := make([]*snapBuild, 0, len(sv.builds))
		for _, b := range sv.builds {
			builds = append(builds, b)
		}
		sort.Slice(builds, func(i, j int) bool { return builds[i].version < builds[j].version })
		label := component.Component{Format: "maven", Group: ga.group, Name: ga.name, Version: sv.version}.Label()
		if !remove(ga, label, reason, builds) {
			return false
		}
	}
	return true
}

// olderRelease reports whether release version v exists and its newest
// file is older than days.
func olderRelease(releases map[string]time.Time, v string, now time.Time, days int) bool {
	t, ok := releases[v]
	return ok && (days == 0 || t.Before(now.AddDate(0, 0, -days)))
}

func purgeSummary(p purgeParams, repos []*repo.Repository, c purgeCounts) summary {
	unit := "SNAPSHOT builds"
	if p.Scope == ScopeVersion {
		unit = "SNAPSHOT versions"
	}
	where := fmt.Sprintf("%d repositories", len(repos))
	if len(repos) == 1 {
		where = repos[0].Name
	}
	if p.Preview {
		return summary{msg: fmt.Sprintf("%s: preview: %d of %d %s would be removed (%d files, %d bytes)", where, c.removed, c.total, unit, c.assets, c.bytes)}
	}
	return summary{msg: fmt.Sprintf("%s: removed %d of %d %s (%d files, %d bytes)", where, c.removed, c.total, unit, c.assets, c.bytes),
		removed: int64(c.removed), assets: int64(c.assets), bytes: c.bytes}
}
