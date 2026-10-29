package ui

import (
	"errors"
	"net/http"
	"net/url"

	"commandcenter/internal/config"
	"commandcenter/internal/repo"
)

// Repository create/update/delete API (plan 9 Decision 6).
//
// config.Repository carries the remote credentials, so it is never marshalled
// into a response: these DTOs are the only wire shapes, and they expose the
// credentials only as passwordSet / bearerTokenSet.

func (u *UI) repoAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/repositories/{name}/config", u.getRepoConfig)
	mux.HandleFunc("POST /-/api/repositories", u.createRepo)
	mux.HandleFunc("PUT /-/api/repositories/{name}", u.updateRepo)
	mux.HandleFunc("DELETE /-/api/repositories/{name}", u.deleteRepo)
}

// repoAuthRequest is the request form of remoteAuth. An empty password or
// bearer token on update keeps the stored one unless the matching clear flag
// is set.
type repoAuthRequest struct {
	Username         string `json:"username"`
	Password         string `json:"password"`
	BearerToken      string `json:"bearerToken"`
	ClearPassword    bool   `json:"clearPassword"`
	ClearBearerToken bool   `json:"clearBearerToken"`
}

// repoRequest is the body of POST and PUT. Durations are config-format
// strings ("24h", "-1", or "" for the default); anonymousRead null or absent
// means inherit.
type repoRequest struct {
	Name       string           `json:"name"`
	Format     string           `json:"format"`
	Type       string           `json:"type"`
	Remote     string           `json:"remote"`
	RemoteAuth *repoAuthRequest `json:"remoteAuth"`
	// ClearRemoteHiddenParts stores the remote as submitted even when it is the
	// stripped form of a stored remote with userinfo or a query.
	ClearRemoteHiddenParts bool     `json:"clearRemoteHiddenParts"`
	MetadataMaxAge         string   `json:"metadataMaxAge"`
	ContentMaxAge          string   `json:"contentMaxAge"`
	NegativeCacheTTL       string   `json:"negativeCacheTTL"`
	Members                []string `json:"members"`
	WriteMember            string   `json:"writeMember"`
	// RoutingRule names the proxy's routing rule; "" is none.
	RoutingRule   string `json:"routingRule"`
	VersionPolicy string `json:"versionPolicy"`
	WritePolicy   string `json:"writePolicy"`
	Depth         int    `json:"depth"` // yum hosted: 0 | 1
	AnonymousRead *bool  `json:"anonymousRead"`
	// CleanupPolicy: absent (null) keeps the stored policy on update and
	// means none on create; "" clears it; a name sets it.
	CleanupPolicy *string `json:"cleanupPolicy"`
	// Online null or absent means online on create and unchanged on update.
	Online *bool `json:"online"`
	// BlobStore is set on create (empty defaults to "default"); it is immutable,
	// so an update must repeat it or leave it empty.
	BlobStore string `json:"blobStore"`
	// apt hosted: distribution and component ("" for the defaults), and the
	// signing key, write-only: an empty one on update keeps the stored key
	// unless clearSigningKey is set.
	Distribution    string `json:"distribution"`
	Component       string `json:"component"`
	SigningKey      string `json:"signingKey"`
	ClearSigningKey bool   `json:"clearSigningKey"`
}

// repoAuthResponse never carries a secret.
type repoAuthResponse struct {
	Username       string `json:"username"`
	PasswordSet    bool   `json:"passwordSet"`
	BearerTokenSet bool   `json:"bearerTokenSet"`
}

type repoConfigResponse struct {
	Name                 string            `json:"name"`
	Format               string            `json:"format"`
	Type                 string            `json:"type"`
	Remote               string            `json:"remote"`
	RemoteHasHiddenParts bool              `json:"remoteHasHiddenParts"`
	RemoteAuth           *repoAuthResponse `json:"remoteAuth"`
	MetadataMaxAge       string            `json:"metadataMaxAge"`
	ContentMaxAge        string            `json:"contentMaxAge"`
	NegativeCacheTTL     string            `json:"negativeCacheTTL"`
	Members              []string          `json:"members"`
	WriteMember          string            `json:"writeMember"`
	RoutingRule          string            `json:"routingRule"`
	VersionPolicy        string            `json:"versionPolicy"`
	WritePolicy          string            `json:"writePolicy"`
	Depth                int               `json:"depth"`
	AnonymousRead        *bool             `json:"anonymousRead"`
	CleanupPolicy        string            `json:"cleanupPolicy"`
	Online               bool              `json:"online"`
	BlobStore            string            `json:"blobStore"`
	Distribution         string            `json:"distribution,omitempty"`
	Component            string            `json:"component,omitempty"`
	SigningKeySet        bool              `json:"signingKeySet"`
}

// publicRemote drops the userinfo, query and fragment of a remote, which may
// embed credentials (see the repository detail endpoint).
// ok is false when remote does not parse; callers must not treat that as a
// match with anything.
func publicRemote(remote string) (pub string, hidden, ok bool) {
	ru, err := url.Parse(remote)
	if err != nil {
		return "", false, false
	}
	hidden = ru.User != nil || ru.RawQuery != "" || ru.ForceQuery || ru.Fragment != ""
	ru.User = nil
	ru.RawQuery, ru.ForceQuery, ru.Fragment, ru.RawFragment = "", false, "", ""
	return ru.String(), hidden, true
}

func repoConfigJSON(d config.Repository) repoConfigResponse {
	pub, hidden, _ := publicRemote(d.Remote)
	out := repoConfigResponse{
		Name: d.Name, Format: d.Format, Type: d.Type,
		Remote:               pub,
		RemoteHasHiddenParts: hidden,
		MetadataMaxAge:       d.MetadataMaxAge.Text(),
		ContentMaxAge:        d.ContentMaxAge.Text(),
		NegativeCacheTTL:     d.NegativeCacheTTL.Text(),
		Members:              append([]string{}, d.Members...),
		WriteMember:          d.WriteMember,
		RoutingRule:          d.RoutingRule,
		VersionPolicy:        d.VersionPolicy,
		WritePolicy:          d.WritePolicy,
		Depth:                d.Depth,
		CleanupPolicy:        d.CleanupPolicy,
		Online:               d.IsOnline(),
		BlobStore:            d.BlobStore,
		Distribution:         d.Distribution,
		Component:            d.Component,
		SigningKeySet:        d.SigningKey != "",
	}
	if d.AnonymousRead != nil {
		v := *d.AnonymousRead
		out.AnonymousRead = &v
	}
	if a := d.RemoteAuth; a != nil {
		out.RemoteAuth = &repoAuthResponse{Username: a.Username, PasswordSet: a.Password != "", BearerTokenSet: a.BearerToken != ""}
	}
	return out
}

// definition converts a request into a definition, or returns the problem.
func (b *repoRequest) definition() (config.Repository, repo.ClearSecrets, error) {
	d := config.Repository{
		Name: b.Name, Format: b.Format, Type: b.Type, Remote: b.Remote,
		Members: b.Members, WriteMember: b.WriteMember, RoutingRule: b.RoutingRule,
		VersionPolicy: b.VersionPolicy, WritePolicy: b.WritePolicy, Depth: b.Depth,
		AnonymousRead: b.AnonymousRead, Online: b.Online,
		BlobStore:    b.BlobStore,
		Distribution: b.Distribution, Component: b.Component, SigningKey: b.SigningKey,
	}
	clear := repo.ClearSecrets{SigningKey: b.ClearSigningKey}
	if b.CleanupPolicy != nil {
		d.CleanupPolicy = *b.CleanupPolicy
	} else {
		clear.KeepCleanupPolicy = true // update only; create ignores it
	}
	for _, f := range []struct {
		field string
		in    string
		out   *config.Duration
	}{
		{"metadataMaxAge", b.MetadataMaxAge, &d.MetadataMaxAge},
		{"contentMaxAge", b.ContentMaxAge, &d.ContentMaxAge},
		{"negativeCacheTTL", b.NegativeCacheTTL, &d.NegativeCacheTTL},
	} {
		v, err := config.ParseDuration(f.in)
		if err != nil {
			return d, clear, &repo.ValidationError{Errors: []string{f.field + ": " + err.Error()}}
		}
		*f.out = v
	}
	if a := b.RemoteAuth; a != nil {
		d.RemoteAuth = &config.RemoteAuth{Username: a.Username, Password: a.Password, BearerToken: a.BearerToken}
		clear.Password = clear.Password || a.ClearPassword
		clear.BearerToken = clear.BearerToken || a.ClearBearerToken
	}
	return d, clear, nil
}

// writeRepoError maps registry errors: validation 400 with the list, the
// registry's HTTP errors (404, 409) as they are, anything else 500.
func (u *UI) writeRepoError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *repo.ValidationError
	var he *repo.Error
	switch {
	case errors.As(err, &ve):
		first := "invalid repository"
		if len(ve.Errors) > 0 {
			first = ve.Errors[0]
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": first, "errors": ve.Errors})
	case errors.As(err, &he):
		writeError(w, he.Status, he.Msg)
	default:
		u.fail(w, r, err)
	}
}

func (u *UI) getRepoConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	rp := u.Repos.Get(r.PathValue("name"))
	if rp == nil {
		writeError(w, http.StatusNotFound, "repository not found")
		return
	}
	writeJSON(w, http.StatusOK, repoConfigJSON(rp.Repository))
}

func (u *UI) createRepo(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var body repoRequest
	if !decodeBody(w, r, &body) {
		return
	}
	def, _, err := body.definition()
	if err == nil {
		// The registry checks the blob store exists, under the lock that
		// blob-store create and delete take too.
		def, err = u.Repos.CreateDef(r.Context(), def)
	}
	if err != nil {
		u.writeRepoError(w, r, err)
		return
	}
	u.repoCache.reset() // a name may be re-used; do not show the old counts (QA L-8)
	u.emit(r, "repo.create", def.Name, def.Name, def.Format+"/"+def.Type)
	writeJSON(w, http.StatusCreated, repoConfigJSON(def))
}

func (u *UI) updateRepo(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	name := r.PathValue("name")
	var body repoRequest
	if !decodeBody(w, r, &body) {
		return
	}
	def, clear, err := body.definition()
	if err == nil {
		// The form shows the remote without userinfo/query (and says so with
		// remoteHasHiddenParts); sending that back unchanged must not strip
		// them from the stored remote, unless clearRemoteHiddenParts asks.
		if cur := u.Repos.Get(name); cur != nil && cur.Remote != "" && !body.ClearRemoteHiddenParts {
			if pub, _, ok := publicRemote(cur.Remote); ok && def.Remote == pub {
				def.Remote = cur.Remote
			}
		}
		def, err = u.Repos.UpdateDef(r.Context(), name, def, clear)
	}
	if err != nil {
		u.writeRepoError(w, r, err)
		return
	}
	u.emit(r, "repo.update", def.Name, def.Name, def.Format+"/"+def.Type)
	writeJSON(w, http.StatusOK, repoConfigJSON(def))
}

func (u *UI) deleteRepo(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	name := r.PathValue("name")
	// The delete removes the roles' privileges on the repository (QA N-25):
	// a role edit read before it must not write one back (final review I3).
	u.accessMu.Lock()
	defer u.accessMu.Unlock()
	if err := u.Repos.Delete(r.Context(), name); err != nil {
		u.writeRepoError(w, r, err)
		return
	}
	u.repoCache.reset()
	u.emit(r, "repo.delete", name, name, "")
	w.WriteHeader(http.StatusNoContent)
}
