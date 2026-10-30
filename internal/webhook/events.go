package webhook

import (
	"context"
	"slices"
	"strings"
	"time"

	"commandcenter/internal/meta"
)

// Kind describes one event kind a webhook can subscribe to.
type Kind struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// Kinds lists the event kinds, in the order the UI shows them. "*"
// subscribes to all of them, future ones included.
var Kinds = []Kind{
	{"component.created", "A component (package version) was uploaded to a hosted repository"},
	{"component.deleted", "A component was deleted (by a user, the UI or a cleanup task)"},
	{"asset.created", "A file was uploaded to a raw or Maven repository"},
	{"asset.deleted", "A file was deleted from a raw or Maven repository"},
	{"repository.created", "A repository was created"},
	{"repository.updated", "A repository's settings changed"},
	{"repository.deleted", "A repository was deleted"},
	{"blobstore.created", "A blob store was created"},
	{"blobstore.deleted", "A blob store was deleted"},
	{"cleanup-policy.created", "A cleanup policy was created"},
	{"cleanup-policy.updated", "A cleanup policy changed"},
	{"cleanup-policy.deleted", "A cleanup policy was deleted"},
	{"user.created", "A user was created"},
	{"user.updated", "A user changed"},
	{"user.deleted", "A user was deleted"},
	{"user.password", "A user's password was changed"},
	{"role.created", "A role was created"},
	{"role.updated", "A role changed"},
	{"role.deleted", "A role was deleted"},
	{"token.created", "An API token was created"},
	{"token.revoked", "An API token was revoked"},
	{"realm.updated", "An authentication realm (local, LDAP, OIDC, SAML) changed"},
	{"realm.tested", "An LDAP or OIDC connection test ran"},
	{"settings.updated", "Server or e-mail settings changed"},
	{"settings.tested", "A test e-mail was sent"},
	{"task.created", "A scheduled task was created"},
	{"task.updated", "A scheduled task changed"},
	{"task.deleted", "A scheduled task was deleted"},
	{"task.finished", "A task run finished (data.result is OK or Failed)"},
	{"cleanup.finished", "A cleanup task run finished, with what it removed"},
	{"email.failed", "An e-mail could not be sent"},
	{"webhook.created", "A webhook was created"},
	{"webhook.updated", "A webhook changed"},
	{"webhook.deleted", "A webhook was deleted"},
	{"ping", "The test event sent from the webhook page"},
	{"blobstore.updated", "A blob store group's members or fill policy changed, or a store was converted to a group"},
	{"staging.move", "Components were moved to another hosted repository (staging); data.detail names the source and counts"},
	{"staging.delete", "Components were deleted by a staging delete; data.detail has the counts"},
	{"firewall.blocked", "A proxy's firewall quarantined a component (data.ids: the advisories)"},
	{"firewall.released", "A quarantined component was released to the proxy's allow list"},
	{"vulnerability.updated", "Vulnerability data (OSV) was imported or downloaded, or its settings changed"},
	{"component.vulnerable", "repository.evaluate found advisories a monitored repository's component did not have before (data.ids: the new ones, data.severity: the component's highest)"},
	{"auth.login", "A user signed in to the UI with a password"},
	{"auth.logout", "A user signed out of the UI"},
	{"auth.failed", "A UI sign-in failed: wrong user name or password (data.subject: the name given)"},
}

// NexusKinds are the event names of Nexus Repository's webhooks, which a
// webhook of payload format nexus subscribes to (as Nexus's webhook.global
// and webhook.repository capabilities do). A commandcenter webhook may list
// them too, as aliases of the kinds they cover.
var NexusKinds = []Kind{
	{"audit", "Nexus global audit (rm:global:audit): every recorded change, each component and asset change included; as an alias, every kind but ping"},
	{"repository", "Nexus global repository (rm:global:repository): a repository was created, updated or deleted; as an alias, repository.*"},
	{"component", "Nexus repository component (rm:repository:component): a component was created, updated or deleted, by an upload, a proxy fetch or a delete; as an alias, component.*"},
	{"asset", "Nexus repository asset (rm:repository:asset): a file was created, replaced or deleted, by an upload, a proxy fetch or a delete; as an alias, asset.*"},
}

// nexusID is the X-Nexus-Webhook-ID of each Nexus event name: rm:<scope>:<name>.
var nexusID = map[string]string{
	"audit": "rm:global:audit", "repository": "rm:global:repository",
	"component": "rm:repository:component", "asset": "rm:repository:asset",
}

var known = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range Kinds {
		m[k.Kind] = true
	}
	return m
}()

// KnownKind reports whether kind is one of Kinds.
func KnownKind(kind string) bool { return known[kind] }

// NexusKind reports whether name is one of NexusKinds.
func NexusKind(name string) bool { return nexusID[name] != "" }

// renamed maps activity event kinds to webhook event kinds; upload, delete
// and task.run are mapped by mapEvent.
var renamed = map[string]string{
	"repo.create": "repository.created", "repo.update": "repository.updated", "repo.delete": "repository.deleted",
	"blobstore.create": "blobstore.created", "blobstore.delete": "blobstore.deleted",
	"policy.create": "cleanup-policy.created", "policy.update": "cleanup-policy.updated", "policy.delete": "cleanup-policy.deleted",
	"user.create": "user.created", "user.update": "user.updated", "user.delete": "user.deleted", "user.password": "user.password",
	"role.create": "role.created", "role.update": "role.updated", "role.delete": "role.deleted",
	"token.create": "token.created", "token.revoke": "token.revoked",
	"realms.update": "realm.updated", "realms.ldap.update": "realm.updated", "realms.oidc.update": "realm.updated",
	"realms.ldap.test": "realm.tested", "realms.oidc.test": "realm.tested",
	"settings.update": "settings.updated", "settings.smtp": "settings.updated", "settings.smtp.test": "settings.tested",
	"task.create": "task.created", "task.update": "task.updated", "task.delete": "task.deleted",
	"task.run": "task.finished", "cleanup": "cleanup.finished",
	"webhook.create": "webhook.created", "webhook.update": "webhook.updated", "webhook.delete": "webhook.deleted",
	"settings.defaultRoles": "settings.updated", "realms.rutauth.update": "realm.updated",
	"blobstore.update": "blobstore.updated", "blobstore.convert": "blobstore.updated",
	"firewall.release": "firewall.released", "vulnerability.import": "vulnerability.updated", "vulnerability.settings": "vulnerability.updated",
	"realms.saml.update": "realm.updated",
}

// event is one activity event as the dispatcher received it, with the
// webhook kind it maps to.
type event struct {
	kind                                 string // webhook kind
	source, actor, repo, subject, detail string // the activity event
	format                               string // the repository's format, "" if unknown
	ip                                   string // the client's address, "" if unknown

	// A commit's component and asset events (Changes) instead of an
	// activity event; actor and ip are then the committing request's, at
	// the commit's time.
	contents []content
	at       time.Time
}

// mapKind returns the webhook kind of an activity event. An upload or delete
// is an asset event in a raw repository and for a Maven path, a component
// event otherwise. Kinds this version does not know pass through unchanged.
func mapKind(source, format, subject string) string {
	switch source {
	case "upload", "delete":
		asset := format == "raw" || format == "maven" && strings.Contains(subject, "/")
		switch {
		case asset && source == "upload":
			return "asset.created"
		case asset:
			return "asset.deleted"
		case source == "upload":
			return "component.created"
		}
		return "component.deleted"
	}
	if k, ok := renamed[source]; ok {
		return k
	}
	return source
}

// component splits a component label as the formats emit it (group/name@version,
// name@version, Maven's group:name:version, Docker's image:tag) into its parts.
// Best effort: a label another shape leaves name as the whole subject.
func component(format, label string) map[string]any {
	out := map[string]any{"format": format, "label": label, "group": "", "name": label, "version": ""}
	switch {
	case format == "maven" && strings.Count(label, ":") == 2:
		p := strings.Split(label, ":")
		out["group"], out["name"], out["version"] = p[0], p[1], p[2]
	case strings.Contains(label, "@"):
		i := strings.LastIndexByte(label, '@')
		name, version := label[:i], label[i+1:]
		if j := strings.LastIndexByte(name, '/'); j > 0 {
			out["group"], name = name[:j], name[j+1:]
		}
		out["name"], out["version"] = name, version
	case format == "docker" && strings.Contains(label, ":"):
		i := strings.LastIndexByte(label, ':')
		out["name"], out["version"] = label[:i], label[i+1:]
	}
	return out
}

// data builds the kind-specific payload data of e, reading the database
// for what the activity event does not carry (an asset's size and digest, a
// task's name and type).
func (d *Dispatcher) data(ctx context.Context, e event) map[string]any {
	switch e.kind {
	case "component.created", "component.deleted":
		out := component(e.format, e.subject)
		if e.detail != "" {
			out["detail"] = e.detail
		}
		return out
	case "asset.created", "asset.deleted":
		out := map[string]any{"format": e.format, "path": e.subject}
		if e.kind == "asset.created" && d.DB != nil {
			if a, err := d.DB.GetAsset(ctx, e.repo, e.subject); err == nil {
				out["size"], out["sha256"], out["contentType"] = a.Size, a.BlobSHA256, a.ContentType
			}
		}
		return out
	case "repository.created", "repository.updated", "repository.deleted":
		out := map[string]any{"name": e.subject, "format": e.format, "type": ""}
		if f, t, ok := strings.Cut(e.detail, "/"); ok { // the UI's detail is format/type
			out["format"], out["type"] = f, t
		}
		if d.Repos != nil { // gone already after a delete
			if rp := d.Repos.Get(e.subject); rp != nil {
				out["format"], out["type"] = rp.Format, rp.Type
			}
		}
		return out
	case "task.finished", "cleanup.finished":
		out := map[string]any{"id": e.subject, "name": "", "type": "", "result": "", "message": e.detail}
		if d.DB != nil {
			if t, err := d.DB.GetTask(ctx, e.subject); err == nil {
				out["name"], out["type"], out["result"] = t.Name, t.Type, t.LastResult
			}
		}
		if res, _, ok := strings.Cut(e.detail, ": "); ok && out["result"] == "" {
			out["result"] = res
		}
		return out
	case "user.created", "user.updated", "user.deleted", "user.password":
		return map[string]any{"user": e.subject, "detail": e.detail}
	case "role.created", "role.updated", "role.deleted":
		return map[string]any{"role": e.subject, "detail": e.detail}
	case "ping":
		return map[string]any{"webhook": e.subject}
	case "firewall.blocked", "firewall.released": // subject name@version, detail the advisory ids or the entry
		out := component(e.format, e.subject)
		out["detail"] = e.detail
		if e.kind == "firewall.blocked" {
			out["ids"] = strings.Split(e.detail, ",")
		}
		return out
	case "component.vulnerable": // subject the component label, detail <severity>:<id>,<id>,...
		out := component(e.format, e.subject)
		sev, ids, _ := strings.Cut(e.detail, ":")
		out["severity"], out["ids"], out["detail"] = sev, strings.Split(ids, ","), e.detail
		return out
	}
	return map[string]any{"subject": e.subject, "detail": e.detail}
}

// matches reports whether the commandcenter webhook w subscribes to kind in
// repo: by the kind itself, "*", or a Nexus name as an alias.
func matches(w meta.Webhook, kind, repo string) bool {
	if !w.Enabled || w.Repo != "" && w.Repo != repo || w.Format == meta.PayloadNexus {
		return false
	}
	for _, k := range w.Events {
		switch k {
		case "*", kind:
			return true
		case "audit":
			if kind != "ping" {
				return true
			}
		case "repository", "component", "asset":
			if strings.HasPrefix(kind, k+".") {
				return true
			}
		}
	}
	return false
}

// nexusWants returns the names among names the nexus webhook w lists, when
// repo is in its scope.
func nexusWants(w meta.Webhook, repo string, names ...string) []string {
	if !w.Enabled || w.Format != meta.PayloadNexus || w.Repo != "" && w.Repo != repo {
		return nil
	}
	var out []string
	for _, n := range names {
		if slices.Contains(w.Events, n) {
			out = append(out, n)
		}
	}
	return out
}

// activityNames are the Nexus event names an activity event reaches:
// repository for a repository change, audit for every recorded change.
// Uploads and deletes are none: a nexus webhook gets them from the content
// rows of their commits (Dispatcher.Changes), proxy caching included.
func activityNames(e event) []string {
	switch {
	case e.kind == "ping" || e.source == "upload" || e.source == "delete":
		return nil
	case strings.HasPrefix(e.kind, "repository."):
		return []string{"repository", "audit"}
	}
	return []string{"audit"}
}
