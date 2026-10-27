package importer

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/config"
	"commandcenter/internal/importer/nexus"
	"commandcenter/internal/logsafe"
)

// mapFormat maps a source format to Command Center's; "" when unsupported.
func mapFormat(f string) string {
	switch f {
	case "maven2":
		return "maven"
	case "npm", "pypi", "cargo", "go":
		return f
	}
	return ""
}

// notImported explains formats Command Center has but the import leaves out.
var notImported = map[string]string{
	"docker": "docker repositories are not imported (neither definitions nor images): create docker repositories in Command Center and copy the images with docker pull, docker tag and docker push",
}

// contentFormats are the formats whose hosted content is imported (go has no
// hosted repositories on the source).
var contentFormats = map[string]bool{"maven": true, "npm": true, "pypi": true, "cargo": true}

func knownType(t string) bool { return t == "hosted" || t == "proxy" || t == "group" }

// MapWritePolicy maps the source's writePolicy (ALLOW, ALLOW_ONCE, DENY) to
// Command Center's; ok is false for an unknown value.
func MapWritePolicy(s string) (string, bool) {
	switch strings.ToUpper(s) {
	case "ALLOW":
		return "allow", true
	case "ALLOW_ONCE":
		return "allow_once", true
	case "DENY":
		return "deny", true
	}
	return "", false
}

// MapVersionPolicy maps the source's Maven versionPolicy (RELEASE, SNAPSHOT,
// MIXED); ok is false for an unknown value.
func MapVersionPolicy(s string) (string, bool) {
	switch strings.ToUpper(s) {
	case "RELEASE":
		return "release", true
	case "SNAPSHOT":
		return "snapshot", true
	case "MIXED":
		return "mixed", true
	}
	return "", false
}

// maxMinutes caps a duration at about 100 years, far below the overflow of
// time.Duration.
const maxMinutes = 100 * 365 * 24 * 60

// MinutesToDuration maps a source max age or time to live in minutes to a
// Command Center duration: -1 (or any negative value) is never, 0 is one
// second (the source revalidates every time; 0 means "default" here), and
// values past maxMinutes are capped.
func MinutesToDuration(m int) config.Duration {
	switch {
	case m < 0:
		return config.Never
	case m == 0:
		return config.Duration(time.Second)
	case m > maxMinutes:
		m = maxMinutes
	}
	return config.Duration(time.Duration(m) * time.Minute)
}

// plan is one selected source repository.
type plan struct {
	src    nexus.RepoSettings
	format string // Command Center format
	target string
	rr     *RepoReport
	ok     bool // the target exists (or is created, or would be) with the right format and type
	// needsCredentials says why a proxy must have its remote credentials
	// entered again; listed in the report once the proxy is created.
	needsCredentials string
}

func (pl *plan) contentFormat() bool { return pl.src.Type == "hosted" && contentFormats[pl.format] }

// plan selects the source repositories (the filter, supported formats) and
// reports the rest.
func (e *Engine) plan(settings []nexus.RepoSettings, p Params, rep *Report, log Logger) []*plan {
	want := map[string]bool{}
	for _, r := range p.Repositories {
		want[r] = true
	}
	found := map[string]bool{}
	unsupported := map[string]*Unsupported{}
	var out []*plan
	sort.Slice(settings, func(i, j int) bool { return settings[i].Name < settings[j].Name })
	for _, s := range settings {
		found[s.Name] = true
		if len(want) > 0 && !want[s.Name] {
			continue
		}
		f := mapFormat(s.Format)
		if f == "" || !knownType(s.Type) {
			u := unsupported[s.Format]
			if u == nil {
				u = &Unsupported{Format: s.Format, Repositories: []string{}, Note: notImported[s.Format]}
				unsupported[s.Format] = u
			}
			u.Count++
			u.Repositories = append(u.Repositories, s.Name)
			continue
		}
		pl := &plan{src: s, format: f, target: p.RenamePrefix + s.Name,
			rr: &RepoReport{Source: s.Name, Target: p.RenamePrefix + s.Name, Format: s.Format, Type: s.Type}}
		out = append(out, pl)
		rep.Repositories = append(rep.Repositories, pl.rr)
	}
	for _, r := range p.Repositories {
		if !found[r] {
			rep.warn(fmt.Sprintf("repository %q was asked for but does not exist on the source", r))
		}
	}
	formats := make([]string, 0, len(unsupported))
	for f := range unsupported {
		formats = append(formats, f)
	}
	sort.Strings(formats)
	for _, f := range formats {
		u := unsupported[f]
		rep.Unsupported = append(rep.Unsupported, *u)
		if u.Note != "" {
			log.Warnf("%d %s repositories are not imported (%s): %s", u.Count, f, joinCapped(u.Repositories, 10), u.Note)
			continue
		}
		log.Warnf("%d %s repositories are not imported: the format is unsupported in this version (%s)", u.Count, f, joinCapped(u.Repositories, 10))
	}
	return out
}

// checkExisting decides a plan against an existing repository: reused when
// the format and type match, a conflict otherwise. It reports whether a
// repository of that name exists.
func (e *Engine) checkExisting(pl *plan, rep *Report) bool {
	cur := e.Repos.Get(pl.target)
	if cur == nil {
		if pl.rr.Definition == "" {
			pl.rr.Definition = DefMissing
			pl.rr.Notes = append(pl.rr.Notes, "no repository "+pl.target+" exists and importDefinitions is off")
		}
		return false
	}
	if cur.Format == pl.format && cur.Type == pl.src.Type {
		pl.rr.Definition, pl.ok = DefExists, true
		return true
	}
	pl.rr.Definition = DefConflict
	rep.conflict(fmt.Sprintf("repository %q exists as %s %s; the source %q is %s %s: skipped (use renamePrefix to import it under another name)",
		pl.target, cur.Format, cur.Type, pl.src.Name, pl.format, pl.src.Type))
	return true
}

// definitions creates the selected repositories: hosted and proxy first,
// then groups in member order (Decision 2).
func (e *Engine) definitions(ctx context.Context, plans []*plan, p Params, actor string, rep *Report, log Logger) {
	byName := map[string]*plan{}
	for _, pl := range plans {
		byName[pl.src.Name] = pl
	}
	if p.RenamePrefix != "" {
		rep.warn(fmt.Sprintf("renamePrefix %q: repositories are created as %s<name>; a group member is the imported member, "+
			"or, when the member was not selected or is of an unsupported format, an existing repository named %s<member> of the group's format", p.RenamePrefix, p.RenamePrefix, p.RenamePrefix))
	}
	var groups []*plan
	for _, pl := range plans {
		if ctx.Err() != nil {
			return
		}
		if pl.src.Type == "proxy" && pl.src.HTTPClient != nil && pl.src.HTTPClient.Authentication != nil {
			pl.needsCredentials = "the source proxy authenticates to its remote; the API does not return the credentials"
		}
		if e.checkExisting(pl, rep) {
			continue
		}
		pl.rr.Definition, pl.rr.Notes = "", nil
		if pl.src.Type == "group" {
			groups = append(groups, pl)
			continue
		}
		def, notes, err := e.leafDef(pl, p)
		pl.rr.Notes = append(pl.rr.Notes, notes...)
		if err != nil {
			pl.rr.Definition = DefFailed
			pl.rr.addError(err.Error())
			continue
		}
		e.create(ctx, pl, def, p, actor, rep, log)
	}
	// Groups whose members are groups wait for them: a few passes.
	pending := groups
	for pass := 0; len(pending) > 0 && pass <= len(groups); pass++ {
		var next []*plan
		for _, g := range pending {
			if ctx.Err() != nil {
				return
			}
			if !membersSettled(g, byName) {
				next = append(next, g)
				continue
			}
			e.createGroup(ctx, g, byName, p, actor, rep, log)
		}
		if len(next) == len(pending) {
			for _, g := range next {
				g.rr.Definition = DefFailed
				g.rr.addError("group membership forms a cycle on the source")
			}
			break
		}
		pending = next
	}
}

func membersSettled(g *plan, byName map[string]*plan) bool {
	if g.src.Group == nil {
		return true
	}
	for _, m := range g.src.Group.MemberNames {
		if mp := byName[m]; mp != nil && mp.src.Type == "group" && mp.rr.Definition == "" {
			return false
		}
	}
	return true
}

// leafDef builds a hosted or proxy definition.
func (e *Engine) leafDef(pl *plan, p Params) (config.Repository, []string, error) {
	s := pl.src
	def := config.Repository{Name: pl.target, Format: pl.format, Type: s.Type, BlobStore: p.BlobStore}
	var notes []string
	switch s.Type {
	case "hosted":
		if w, ok := MapWritePolicy(s.Storage.WritePolicy); ok {
			def.WritePolicy = w
		} else if s.Storage.WritePolicy != "" {
			notes = append(notes, fmt.Sprintf("unknown writePolicy %q: the default applies", s.Storage.WritePolicy))
		}
		if pl.format == "maven" && s.Maven != nil {
			if v, ok := MapVersionPolicy(s.Maven.VersionPolicy); ok {
				def.VersionPolicy = v
			} else if s.Maven.VersionPolicy != "" {
				notes = append(notes, fmt.Sprintf("unknown versionPolicy %q: the default applies", s.Maven.VersionPolicy))
			}
		}
	case "proxy":
		if s.Proxy == nil || s.Proxy.RemoteURL == "" {
			return def, notes, fmt.Errorf("the source does not report the remote URL of proxy %q", s.Name)
		}
		def.Remote = s.Proxy.RemoteURL
		// Credentials embedded in the remote URL are never copied.
		if u, err := url.Parse(def.Remote); err == nil && u.User != nil {
			u.User = nil
			def.Remote = u.String()
			pl.needsCredentials = "credentials were embedded in the source URL and were not copied"
			notes = append(notes, pl.needsCredentials)
		}
		def.ContentMaxAge = MinutesToDuration(s.Proxy.ContentMaxAge)
		def.MetadataMaxAge = MinutesToDuration(s.Proxy.MetadataMaxAge)
		if nc := s.NegativeCache; nc != nil {
			if nc.Enabled {
				def.NegativeCacheTTL = MinutesToDuration(nc.TimeToLive)
			} else {
				notes = append(notes, "the negative cache is disabled on the source; Command Center always has one, with the default time to live")
			}
		}
	}
	return def, notes, nil
}

func (e *Engine) createGroup(ctx context.Context, g *plan, byName map[string]*plan, p Params, actor string, rep *Report, log Logger) {
	def := config.Repository{Name: g.target, Format: g.format, Type: "group"}
	var members []string
	if g.src.Group != nil {
		members = g.src.Group.MemberNames
	}
	for _, m := range members {
		mp := byName[m]
		switch {
		case mp != nil && mp.ok:
			def.Members = append(def.Members, mp.target)
		case mp != nil && mp.format != g.format:
			g.rr.Notes = append(g.rr.Notes, fmt.Sprintf("member %q dropped: format %s", m, mp.src.Format))
		case mp != nil:
			g.rr.Notes = append(g.rr.Notes, fmt.Sprintf("member %q dropped: it was not imported (%s)", m, mp.rr.Definition))
		default:
			// The renamePrefix member rule: a member that was not selected,
			// or is of an unsupported format, is kept when a repository named
			// renamePrefix+member of the group's format exists here already.
			if cur := e.Repos.Get(p.RenamePrefix + m); cur != nil && cur.Format == g.format {
				def.Members = append(def.Members, cur.Name)
				continue
			}
			g.rr.Notes = append(g.rr.Notes, fmt.Sprintf("member %q dropped: not imported (unsupported format or not selected)", m))
		}
	}
	for _, n := range g.rr.Notes {
		rep.warn(fmt.Sprintf("group %s: %s", g.target, n))
	}
	if len(def.Members) == 0 {
		g.rr.Definition = DefSkipped
		g.rr.addError("no member of the group could be imported")
		log.Warnf("%s: group skipped: no member could be imported", g.src.Name)
		return
	}
	e.create(ctx, g, def, p, actor, rep, log)
}

func (e *Engine) create(ctx context.Context, pl *plan, def config.Repository, p Params, actor string, rep *Report, log Logger) {
	if p.DryRun {
		pl.rr.Definition, pl.ok = DefWouldCreate, true
		rep.Created++
		e.noteCredentials(pl, rep)
		log.Infof("%s: would create %s %s %s", pl.src.Name, def.Format, def.Type, def.Name)
		return
	}
	stored, err := e.Repos.CreateDef(ctx, def)
	if err != nil {
		pl.rr.Definition = DefFailed
		pl.rr.addError(logsafe.Err(err))
		log.Errorf("%s: could not create %s: %v", pl.src.Name, def.Name, err)
		return
	}
	pl.rr.Definition, pl.ok = DefCreated, true
	rep.Created++
	e.noteCredentials(pl, rep)
	detail := stored.Format + "/" + stored.Type + " (imported from " + pl.src.Name + ")"
	if e.Events != nil {
		e.Events.Emit("repo.create", actor, stored.Name, stored.Name, detail)
	}
	log.Infof("%s: created %s %s %s", pl.src.Name, stored.Format, stored.Type, stored.Name)
}

// noteCredentials lists a created (or, in a dry run, to be created) proxy
// whose remote credentials must be entered again.
func (e *Engine) noteCredentials(pl *plan, rep *Report) {
	if pl.needsCredentials == "" {
		return
	}
	rep.ProxiesNeedingCredentials = append(rep.ProxiesNeedingCredentials, pl.target)
	if !slices.Contains(pl.rr.Notes, pl.needsCredentials) {
		pl.rr.Notes = append(pl.rr.Notes, pl.needsCredentials)
	}
}
