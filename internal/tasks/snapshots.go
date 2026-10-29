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
)

// ---- maven.purge-snapshots ----

// purgeParams selects the SNAPSHOT versions of a Maven hosted repository to
// remove (Nexus "Remove snapshots from Maven repository"). Per GA, the
// newest minimumRetained snapshot versions are kept; of the rest, a
// version goes when it is older than snapshotRetentionDays (0: any age)
// and, with lastDownloadedDays, was not downloaded (or, never downloaded,
// not stored) for that long. With removeIfReleased, X-SNAPSHOT goes,
// retained or not, once release X has been there for gracePeriodDays.
// preview only logs what would go.
type purgeParams struct {
	Repository            string `json:"repository"`
	MinimumRetained       int    `json:"minimumRetained"`
	SnapshotRetentionDays int    `json:"snapshotRetentionDays"`
	LastDownloadedDays    int    `json:"lastDownloadedDays,omitempty"`
	RemoveIfReleased      bool   `json:"removeIfReleased,omitempty"`
	GracePeriodDays       int    `json:"gracePeriodDays,omitempty"`
	Preview               bool   `json:"preview,omitempty"`
}

// Defaults of purgeParams.
const (
	DefaultMinimumRetained = 1
	DefaultSnapshotDays    = 30
)

var purgeFields = []ParamSpec{
	{Name: "repository", Type: "repository", Required: true, Help: "a Maven hosted repository"},
	{Name: "minimumRetained", Type: "int", Default: DefaultMinimumRetained, Help: "newest snapshot versions kept per GA"},
	{Name: "snapshotRetentionDays", Type: "int", Default: DefaultSnapshotDays, Help: "remove versions older than this; 0: any age"},
	{Name: "lastDownloadedDays", Type: "int", Default: 0, Help: "only versions not downloaded for this long; 0: ignore"},
	{Name: "removeIfReleased", Type: "bool", Default: false, Help: "remove X-SNAPSHOT once release X exists"},
	{Name: "gracePeriodDays", Type: "int", Default: 0, Help: "with removeIfReleased: days after the release"},
	{Name: "preview", Type: "bool", Default: false, Help: "only log what would be removed"},
}

func purgeParamsOf(s *Service, raw json.RawMessage, check bool) (string, []string) {
	// Pointers tell an absent count from an explicit 0.
	var in struct {
		Repository            string `json:"repository"`
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
	p := purgeParams{Repository: in.Repository, MinimumRetained: DefaultMinimumRetained, SnapshotRetentionDays: DefaultSnapshotDays,
		LastDownloadedDays: in.LastDownloadedDays, RemoveIfReleased: in.RemoveIfReleased, GracePeriodDays: in.GracePeriodDays, Preview: in.Preview}
	if in.MinimumRetained != nil {
		p.MinimumRetained = *in.MinimumRetained
	}
	if in.SnapshotRetentionDays != nil {
		p.SnapshotRetentionDays = *in.SnapshotRetentionDays
	}
	var errs []string
	for name, v := range map[string]int{"minimumRetained": p.MinimumRetained, "snapshotRetentionDays": p.SnapshotRetentionDays,
		"lastDownloadedDays": p.LastDownloadedDays, "gracePeriodDays": p.GracePeriodDays} {
		if v < 0 {
			errs = append(errs, "params."+name+" must not be negative")
		}
	}
	sort.Strings(errs)
	rp := s.Repos.Get(p.Repository)
	switch {
	case p.Repository == "":
		errs = append(errs, "params.repository is required")
	case !check:
	case rp == nil:
		errs = append(errs, fmt.Sprintf("repository %q does not exist", p.Repository))
	case rp.Format != "maven" || rp.Type != "hosted":
		errs = append(errs, fmt.Sprintf("repository %q is %s %s: the task takes a Maven hosted repository", p.Repository, rp.Format, rp.Type))
	case rp.WritePolicy == "deny":
		errs = append(errs, fmt.Sprintf("repository %q is read-only (writePolicy deny)", p.Repository))
	}
	if len(errs) > 0 {
		return "", errs
	}
	return encodeParams(p), nil
}

// snapVersion is what the scan saw of one SNAPSHOT version.
type snapVersion struct {
	version  string
	assets   []component.Asset
	paths    map[string]bool
	created  time.Time // newest asset
	modified time.Time
	lastDl   time.Time
	bytes    int64
}

type purgeGA struct {
	group, name string
	releases    map[string]time.Time // version -> newest asset
	snaps       map[string]*snapVersion
}

func runPurge(ctx context.Context, s *Service, t meta.Task, actor string, log *RunLog) (summary, error) {
	var p purgeParams
	if err := json.Unmarshal([]byte(t.Params), &p); err != nil {
		return summary{msg: "invalid params"}, fmt.Errorf("decode params: %w", err)
	}
	if s.Deleters == nil {
		return summary{msg: "component deletion is not available"}, errors.New("component deletion is not available")
	}
	rp := s.Repos.Get(p.Repository)
	switch {
	case rp == nil:
		return summary{}, fmt.Errorf("repository %q no longer exists: edit or delete this task", p.Repository)
	case rp.Format != "maven" || rp.Type != "hosted":
		return summary{}, fmt.Errorf("repository %q is now %s %s, not Maven hosted: edit or delete this task", p.Repository, rp.Format, rp.Type)
	case rp.WritePolicy == "deny":
		return summary{}, fmt.Errorf("repository %q is read-only (writePolicy deny): nothing removed", p.Repository)
	}
	if s.Metrics != nil && p.LastDownloadedDays > 0 {
		s.flushOthers(ctx)
		if err := s.Metrics.Flush(ctx); err != nil {
			return summary{}, fmt.Errorf("could not write the pending download counts: %w", err)
		}
	}

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
			return summary{}, err
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
			if !strings.HasSuffix(v, "-SNAPSHOT") {
				if a.Created.After(ga.releases[v]) {
					ga.releases[v] = a.Created
				}
				continue
			}
			sv := ga.snaps[v]
			if sv == nil {
				sv = &snapVersion{version: v, paths: map[string]bool{}}
				ga.snaps[v] = sv
			}
			sv.assets = append(sv.assets, component.Asset{Path: a.Path, Size: a.Size})
			sv.paths[a.Path] = true
			sv.bytes += a.Size
			if a.Created.After(sv.created) {
				sv.created = a.Created
			}
			if a.Modified.After(sv.modified) {
				sv.modified = a.Modified
			}
			if a.LastDownloaded.After(sv.lastDl) {
				sv.lastDl = a.LastDownloaded
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
	var total, removed, kept, failed, assets int
	var bytes int64
	verb := "removing"
	if p.Preview {
		verb = "would remove"
	}
	for _, k := range keys {
		ga := gas[k]
		vs := make([]*snapVersion, 0, len(ga.snaps))
		for _, sv := range ga.snaps {
			vs = append(vs, sv)
		}
		total += len(vs)
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
			case p.LastDownloadedDays > 0 && !older(lastUsed(sv), p.LastDownloadedDays):
				continue
			case p.SnapshotRetentionDays > 0:
				reason = fmt.Sprintf("older than %d days", p.SnapshotRetentionDays)
			case p.LastDownloadedDays > 0:
				reason = fmt.Sprintf("not downloaded for %d days", p.LastDownloadedDays)
			default:
				reason = fmt.Sprintf("beyond the %d newest", p.MinimumRetained)
			}
			if err := ctx.Err(); err != nil {
				return purgeSummary(rp.Name, p.Preview, removed, total, assets, bytes), err
			}
			if cur := s.Repos.Get(rp.Name); cur == nil || cur.Gen != rp.Gen {
				log.Warnf("%s: stopped: the repository changed during the run", rp.Name)
				break
			}
			c := component.Component{Repo: rp.Name, Format: "maven", Group: ga.group, Name: ga.name, Version: sv.version,
				LastUpdated: sv.created, Assets: sv.assets,
				Evaluated: &component.Evaluated{Paths: sv.paths, MaxCreated: sv.created, MaxModified: sv.modified, LastDownloaded: sv.lastDl}}
			if p.Preview {
				removed++
				assets += len(sv.assets)
				bytes += sv.bytes
				log.Infof("%s: %s %s (%s; %d files, %d bytes)", rp.Name, verb, c.Label(), reason, len(sv.assets), sv.bytes)
				continue
			}
			res, err := s.Deleters.Delete(ctx, rp, c, actor)
			switch {
			case errors.Is(err, cleanup.ErrChanged):
				kept++
				log.Infof("%s: kept %s: it changed since evaluation (%v)", rp.Name, c.Label(), err)
			case err != nil:
				failed++
				log.Errorf("%s: deleting %s: %v", rp.Name, c.Label(), err)
			default:
				removed++
				assets += res.Assets
				bytes += res.Bytes
				log.Infof("%s: removed %s (%s; %d files, %d bytes)", rp.Name, c.Label(), reason, res.Assets, res.Bytes)
			}
		}
	}
	sum := purgeSummary(rp.Name, p.Preview, removed, total, assets, bytes)
	if kept > 0 {
		sum.msg += fmt.Sprintf("; kept %d changed since evaluation", kept)
	}
	log.Infof("%s", sum.msg)
	if failed > 0 {
		return sum, fmt.Errorf("%d deletes failed (see the log)", failed)
	}
	return sum, nil
}

// olderRelease reports whether release version v exists and its newest
// file is older than days.
func olderRelease(releases map[string]time.Time, v string, now time.Time, days int) bool {
	t, ok := releases[v]
	return ok && (days == 0 || t.Before(now.AddDate(0, 0, -days)))
}

// lastUsed is when the version was last downloaded, or stored if never.
func lastUsed(sv *snapVersion) time.Time {
	if sv.lastDl.IsZero() {
		return sv.created
	}
	return sv.lastDl
}

func purgeSummary(repoName string, preview bool, removed, total, assets int, bytes int64) summary {
	msg := fmt.Sprintf("%s: removed %d of %d SNAPSHOT versions (%d files, %d bytes)", repoName, removed, total, assets, bytes)
	if preview {
		msg = fmt.Sprintf("%s: preview: %d of %d SNAPSHOT versions would be removed (%d files, %d bytes)", repoName, removed, total, assets, bytes)
		return summary{msg: msg}
	}
	return summary{msg: msg, removed: int64(removed), assets: int64(assets), bytes: bytes}
}
