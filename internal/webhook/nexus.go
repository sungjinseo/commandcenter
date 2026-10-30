package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"path"
	"strings"
	"time"

	"commandcenter/internal/auth"
	comp "commandcenter/internal/component"
	"commandcenter/internal/events"
	"commandcenter/internal/meta"
)

// The payload format "nexus": the bodies Nexus Repository's webhooks send,
// so that receivers written for Nexus work unchanged. Repository and audit
// events come from the activity events (Emit); component and asset events
// from the content rows each commit writes (Changes), so uploads, proxy
// caching, overwrites and deletes of every format reach them.

// Nexus actions.
const (
	actionCreated = "CREATED"
	actionUpdated = "UPDATED"
	actionDeleted = "DELETED"
)

// nexusHead is what every Nexus payload starts with; empty fields are left
// out, as Nexus leaves out nulls.
type nexusHead struct {
	Timestamp string `json:"timestamp,omitempty"`
	NodeID    string `json:"nodeId,omitempty"`
	Initiator string `json:"initiator,omitempty"`
}

type nexusRepositoryBody struct {
	nexusHead
	Action     string `json:"action"`
	Repository struct {
		Format string `json:"format"`
		Name   string `json:"name"`
		Type   string `json:"type"`
	} `json:"repository"`
}

type nexusComponentBody struct {
	nexusHead
	RepositoryName string `json:"repositoryName"`
	Action         string `json:"action"`
	Component      struct {
		ID          string `json:"id"`
		ComponentID string `json:"componentId"`
		Format      string `json:"format"`
		Name        string `json:"name"`
		Group       string `json:"group"`
		Version     string `json:"version"`
	} `json:"component"`
}

type nexusAssetBody struct {
	nexusHead
	RepositoryName string `json:"repositoryName"`
	Action         string `json:"action"`
	Asset          struct {
		ID      string `json:"id"`
		AssetID string `json:"assetId"`
		Format  string `json:"format"`
		Name    string `json:"name"`
	} `json:"asset"`
}

// nexusAuditBody has no timestamp, as Nexus's audit payload has none.
type nexusAuditBody struct {
	NodeID    string `json:"nodeId,omitempty"`
	Initiator string `json:"initiator,omitempty"`
	Audit     struct {
		Domain     string         `json:"domain"`
		Type       string         `json:"type"`
		Context    string         `json:"context"`
		Attributes map[string]any `json:"attributes,omitempty"`
	} `json:"audit"`
}

// nexusTime is Nexus's timestamp: UTC with milliseconds and the offset.
func nexusTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000-07:00") }

// initiator is the Nexus initiator: user/IP, the user alone when the
// address is unknown, *SYSTEM for the server's own work.
func initiator(actor, ip string) string {
	switch actor {
	case events.System:
		return "*SYSTEM"
	case "":
		if ip == "" {
			return "*SYSTEM"
		}
		actor = "anonymous"
	}
	if ip == "" {
		return actor
	}
	return actor + "/" + ip
}

// ctxInitiator is the initiator of work done for ctx: the request's
// principal and address, or *SYSTEM outside a request.
func ctxInitiator(ctx context.Context) string {
	return initiator(auth.FromContext(ctx).Owner(), auth.ClientIP(ctx))
}

func nexusFormat(f string) string {
	if f == "maven" {
		return "maven2"
	}
	return f
}

// shortID is a stable 32-hex-digit id, the shape of Nexus's external ids.
func shortID(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:16])
}

// opaque is the REST id of an asset or component (nexusapi's assetID and
// compID): base64url of key.
func opaque(key string) string { return base64.RawURLEncoding.EncodeToString([]byte(key)) }

// content is one asset or component change of a commit.
type content struct {
	typ, action, repo, format string
	path                      string // an asset's
	group, name, version      string // a component's, as derived (comp.DeriveAsset)
}

// compKey is the component's key as nexusapi keys it: repo:group:name:version.
func (c content) compKey() string { return c.repo + ":" + c.group + ":" + c.name + ":" + c.version }

// body renders c for the Nexus event name (asset, component or audit).
func (c content) body(name string, head nexusHead) any {
	switch name {
	case "asset":
		b := nexusAssetBody{nexusHead: head, RepositoryName: c.repo, Action: c.action}
		b.Asset.ID, b.Asset.AssetID = shortID(c.repo+":"+c.path), opaque(c.repo+":"+c.path)
		b.Asset.Format, b.Asset.Name = nexusFormat(c.format), c.assetName()
		return b
	case "component":
		b := nexusComponentBody{nexusHead: head, RepositoryName: c.repo, Action: c.action}
		b.Component.ID, b.Component.ComponentID = shortID(c.compKey()), opaque(c.compKey())
		b.Component.Format, b.Component.Version = nexusFormat(c.format), c.version
		b.Component.Name, b.Component.Group = c.nexusName()
		return b
	}
	b := nexusAuditBody{NodeID: head.NodeID, Initiator: head.Initiator}
	b.Audit.Domain, b.Audit.Type = "repository."+c.typ, strings.ToLower(c.action)
	if c.typ == "asset" {
		b.Audit.Context = c.assetName()
		b.Audit.Attributes = map[string]any{"repository.name": c.repo, "path": c.assetName(), "format": nexusFormat(c.format)}
	} else {
		name, group := c.nexusName()
		b.Audit.Context = name
		b.Audit.Attributes = map[string]any{"repository.name": c.repo, "name": name, "namespace": group, "version": c.version,
			"format": nexusFormat(c.format)}
	}
	return b
}

// assetName is the asset's path as Nexus names it: with a leading slash,
// under /v2/ for docker.
func (c content) assetName() string {
	if c.format == "docker" {
		return "/v2/" + c.path
	}
	return "/" + c.path
}

// nexusName is the component's name and group as Nexus names them: a raw
// file is named by its path with a leading slash, its group the directory;
// an npm scope has no "@".
func (c content) nexusName() (name, group string) {
	switch c.format {
	case "raw":
		return "/" + c.name, "/" + strings.TrimSuffix(path.Dir(c.name), ".")
	case "npm":
		return c.name, strings.TrimPrefix(c.group, "@")
	}
	return c.name, c.group
}

// deriveComponent is the component the asset at p of format belongs to:
// comp.DeriveAsset, and for docker a manifest cached by tag (a
// proxy's), which is the component image:tag. A hosted docker repository's
// components are its tags (tag changes).
func deriveComponent(format, p string) (group, name, version string, ok bool) {
	if format == "docker" {
		i := strings.LastIndex(p, "/manifests/")
		if i <= 0 {
			return "", "", "", false
		}
		ref := p[i+len("/manifests/"):]
		if ref == "" || strings.Contains(ref, ":") { // a digest is algorithm:hex
			return "", "", "", false
		}
		return "", p[:i], ref, true
	}
	return comp.DeriveAsset(format, p)
}

// compAgg is what one commit did to one component's assets.
type compAgg struct {
	content
	firstPath string
	touched   map[string]bool // its paths the commit wrote
	newPuts   int
	oldest    time.Time // the earliest creation time of its new assets
	replaced  bool      // an existing asset got new content
	deleted   bool
}

// contents turns the changes of one commit into asset events and, where
// comps says the repository's component events are wanted, component
// events. A put that left an asset's content as it was is no event. A
// component was created when the commit wrote its first asset, deleted when
// it deleted the last one, and updated otherwise; which is the case is read
// from the other assets of the component now, so it is best effort when
// another commit changed the component in between.
func (d *Dispatcher) contents(ctx context.Context, cs []meta.Change, comps func(repo string) bool) []content {
	var out []content
	aggs := map[string]*compAgg{}
	var order []string
	for _, c := range cs {
		format := d.format(c.Repo)
		switch {
		case c.Kind == meta.ChangeAsset:
			action := actionDeleted
			if c.Op == meta.ChangePut {
				if c.Asset == nil || c.Prev == c.Asset.SHA256 {
					continue
				}
				action = actionCreated
				if c.Prev != "" {
					action = actionUpdated
				}
			}
			out = append(out, content{typ: "asset", action: action, repo: c.Repo, format: format, path: c.Path})
			if !comps(c.Repo) {
				continue
			}
			group, name, version, ok := deriveComponent(format, c.Path)
			if !ok {
				continue
			}
			k := content{repo: c.Repo, group: group, name: name, version: version}.compKey()
			a := aggs[k]
			if a == nil {
				a = &compAgg{content: content{typ: "component", repo: c.Repo, format: format, group: group, name: name, version: version},
					firstPath: c.Path, touched: map[string]bool{}}
				aggs[k] = a
				order = append(order, k)
			}
			a.touched[c.Path] = true
			switch {
			case action == actionDeleted:
				a.deleted = true
			case action == actionUpdated:
				a.replaced = true
			default:
				if a.newPuts == 0 || c.Asset.Created.Before(a.oldest) {
					a.oldest = c.Asset.Created
				}
				a.newPuts++
			}
		case c.Kind == meta.ChangeTag && format == "docker" && comps(c.Repo):
			action := actionDeleted
			if c.Op == meta.ChangePut {
				if c.Prev == c.Version {
					continue
				}
				action = actionCreated
				if c.Prev != "" {
					action = actionUpdated
				}
			}
			out = append(out, content{typ: "component", action: action, repo: c.Repo, format: format, name: c.Name, version: c.Tag})
		}
	}
	for _, k := range order {
		a := aggs[k]
		before, after := a.replaced || a.deleted, a.replaced || a.newPuts > 0
		if !before || !after {
			others, err := d.others(ctx, a)
			if err != nil {
				d.Log.Warn("could not read a component's assets for its webhook event", "repository", a.repo, "component", a.name, "err", err.Error())
			}
			for _, o := range others {
				after = true
				before = before || a.newPuts > 0 && o.CreatedAt.Before(a.oldest)
			}
		}
		switch {
		case !before && after:
			a.action = actionCreated
		case before && !after:
			a.action = actionDeleted
		case before && after:
			a.action = actionUpdated
		default:
			continue
		}
		out = append(out, a.content)
	}
	return out
}

// others returns the assets of a's component the commit did not write.
// They are read under the longest path prefix all of them share in the
// layouts of the formats: the directory, and the component's name when the
// file name starts with it (flat directories of helm, yum, conda, ...);
// for the layouts that nest a component's files deeper, its root.
// ponytail: one prefix query per component event; index the derived
// components if huge flat directories make it slow.
func (d *Dispatcher) others(ctx context.Context, a *compAgg) ([]meta.Asset, error) {
	dir, base := path.Split(a.firstPath)
	prefix := dir
	if n := path.Base(a.name); strings.HasPrefix(base, n) {
		prefix += n
	}
	segs := strings.Split(a.firstPath, "/")
	switch {
	case a.format == "pypi": // packages/<project>/[<version>/]<file>
		prefix = strings.Join(segs[:2], "/") + "/"
	case a.format == "conan": // v2/conans/<name>/<version>/<user>/<channel>/revisions/<rrev>/...
		prefix = strings.Join(segs[:8], "/") + "/"
	case a.format == "huggingface": // <repo id>/resolve/<sha>/<file path>
		head, rest, _ := strings.Cut(a.firstPath, "/resolve/")
		sha, _, _ := strings.Cut(rest, "/")
		prefix = head + "/resolve/" + sha + "/"
	}
	list, err := d.DB.ListAssets(ctx, a.repo, prefix)
	if err != nil {
		return nil, err
	}
	var out []meta.Asset
	for _, x := range list {
		if a.touched[x.Path] {
			continue
		}
		if g, n, v, ok := deriveComponent(a.format, x.Path); ok && g == a.group && n == a.name && v == a.version {
			out = append(out, x)
		}
	}
	return out, nil
}

// format is the repository's format, "" when it is unknown (deleted).
func (d *Dispatcher) format(name string) string {
	if d.Repos != nil {
		if rp := d.Repos.Get(name); rp != nil {
			return rp.Format
		}
	}
	return ""
}

// auditDomain is the Nexus audit domain of an activity kind's prefix.
var auditDomain = map[string]string{
	"component": "repository.component", "asset": "repository.asset",
	"user": "security.user", "role": "security.role", "realm": "security.realm",
	"cleanup-policy": "cleanupPolicy", "task": "tasks", "cleanup": "tasks", "auth": "security.user",
}

// activityBody renders the activity event e, whose payload data is data,
// for the Nexus event name (repository or audit).
func activityBody(name string, e event, data map[string]any, head nexusHead) any {
	prefix, typ, _ := strings.Cut(e.kind, ".")
	if name == "repository" {
		b := nexusRepositoryBody{nexusHead: head, Action: strings.ToUpper(typ)}
		b.Repository.Name, _ = data["name"].(string)
		f, _ := data["format"].(string)
		b.Repository.Format = nexusFormat(f)
		b.Repository.Type, _ = data["type"].(string)
		return b
	}
	b := nexusAuditBody{NodeID: head.NodeID, Initiator: head.Initiator}
	b.Audit.Domain, b.Audit.Type, b.Audit.Context, b.Audit.Attributes = prefix, typ, e.subject, maps.Clone(data)
	if dom := auditDomain[prefix]; dom != "" {
		b.Audit.Domain = dom
	}
	if e.repo != "" {
		b.Audit.Attributes["repository.name"] = e.repo
	}
	return b
}

// marshal is json.Marshal for the bodies here, which always encode.
func marshal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
