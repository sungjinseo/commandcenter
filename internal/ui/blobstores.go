package ui

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"commandcenter/internal/blob"
	"commandcenter/internal/events"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/sysinfo"
)

// Blob-store admin API (plan 12 Decision 7). Reads need an admin; mutations
// need an interactive admin and the UI header. A store's path, bucket and
// prefix are immutable. Deleting a store leaves its files on disk and its
// objects in the bucket.
//
// Create and delete run under the registry's write lock, which repository
// create and update hold while they check that a store exists: a store cannot
// be deleted between that check and the repository's commit, and two store
// creates cannot both pass the isolation check with overlapping paths.

func (u *UI) blobStoreRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/blobstores", u.listBlobStores)
	mux.HandleFunc("POST /-/api/blobstores", u.createBlobStore)
	mux.HandleFunc("GET /-/api/blobstores/{name}", u.getBlobStore)
	mux.HandleFunc("PUT /-/api/blobstores/{name}", u.updateBlobStore)
	mux.HandleFunc("DELETE /-/api/blobstores/{name}", u.deleteBlobStore)
	u.blobGroupRoutes(mux)
}

// blobStoreDeleteHook, if set, runs inside a delete after its in-use check,
// with the registry lock held (a test hook).
var blobStoreDeleteHook func()

// blobStoreRequest is the body of POST. Type is "file" (the default) or
// "s3", which takes the bucket in S3. An empty path puts the store (for an
// S3 store: its temp files) under <dataDir>/blobstores/<name> (recorded
// relative to the data directory); an absolute path is used as given; a
// relative one is refused (QA N-22). RelativePath, what the Nexus API's
// relative paths map to, puts it under <dataDir>/blobstores/ instead.
type blobStoreRequest struct {
	Name string         `json:"name"`
	Path string         `json:"path"`
	Type string         `json:"type"`
	S3   *blob.S3Config `json:"s3"`

	RelativePath string `json:"relativePath,omitempty"`

	Quota *meta.Quota `json:"quota"` // soft quota; null or absent is none
	// An azure store's container and credentials (Type "azure"); on PUT its
	// credentials alone, see setAzureAuth.
	Azure *blob.AzureConfig `json:"azure"`
	// A group's (Type "group"), see blobgroups.go.
	Members    []blob.GroupMember `json:"members"`
	FillPolicy string             `json:"fillPolicy"`
	// A google store's (Type "google"): its bucket and credentials.
	Google *blob.GCSConfig `json:"google"`
}

// blobStoreJSON is one store's wire shape. Path is the directory the store
// opens at. FsTotal/FsFree are null when the filesystem cannot be queried,
// and for an S3 store, whose bucket S3 describes (without the secret key).
// Warning is set when a file store lies outside the data directory (QA N-23).
type blobStoreJSON struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Path      string  `json:"path"`
	S3        *s3JSON `json:"s3,omitempty"`
	BlobCount int64   `json:"blobCount"`
	UsedBytes int64   `json:"usedBytes"`
	FsTotal   *uint64 `json:"fsTotal"`
	FsFree    *uint64 `json:"fsFree"`
	RepoCount int     `json:"repoCount"`
	Warning   string  `json:"warning,omitempty"`
	// Quota is the soft quota (null when none); QuotaViolated whether the
	// store is over it right now (writes are still accepted).
	Quota         *meta.Quota `json:"quota"`
	QuotaViolated bool        `json:"quotaViolated"`
	// A group's members in read order and its fill policy; MemberOf names
	// the group a file or S3 store belongs to.
	Members    []blob.GroupMember `json:"members,omitempty"`
	FillPolicy string             `json:"fillPolicy,omitempty"`
	MemberOf   string             `json:"memberOf,omitempty"`
	// An azure store's container and credentials, without the secrets.
	Azure  *azureJSON `json:"azure,omitempty"`
	Google *gcsJSON   `json:"google,omitempty"`

	// Unavailable says why the store did not open on this node (its
	// repositories answer 503 meanwhile) or why its last probe failed.
	Unavailable string `json:"unavailable,omitempty"`
}

// azureJSON is blob.AzureConfig without its secrets: what GET shows.
type azureJSON struct {
	AccountName     string `json:"accountName"`
	ContainerName   string `json:"containerName"`
	Prefix          string `json:"prefix"`
	Endpoint        string `json:"endpoint"`
	Auth            string `json:"authenticationMethod"`
	TenantID        string `json:"tenantId"`
	ClientID        string `json:"clientId"`
	AuthorityHost   string `json:"authorityHost"`
	IMDSEndpoint    string `json:"imdsEndpoint"`
	AccountKeySet   bool   `json:"accountKeySet"` // the secrets themselves are never shown
	SASTokenSet     bool   `json:"sasTokenSet"`
	ClientSecretSet bool   `json:"clientSecretSet"`
}

// gcsJSON is blob.GCSConfig without its account key: what GET shows.
type gcsJSON struct {
	Bucket         string `json:"bucket"`
	Prefix         string `json:"prefix"`
	Endpoint       string `json:"endpoint"`
	Authentication string `json:"authentication"`
	AccountKeySet  bool   `json:"accountKeySet"`  // the key itself is never shown
	ServiceAccount string `json:"serviceAccount"` // the key's client_email; "" without a key
}

// s3JSON is blob.S3Config without its secret: what GET shows.
type s3JSON struct {
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	Region    string `json:"region"`
	Endpoint  string `json:"endpoint"`
	PathStyle bool   `json:"pathStyle"`
	AccessKey string `json:"accessKeyId"`
	SecretSet bool   `json:"secretSet"` // the secret itself is never shown
	SSE       string `json:"sse"`
	// Encryption is null unless set; Failover is [] without replicas.
	Encryption *blob.S3Encryption `json:"encryption"`
	Failover   []blob.S3Failover  `json:"failover"`

	MaxConnections int `json:"maxConnections,omitempty"`
}

// blobStoreView builds the DTO for one stored row: usage scoped to the store's
// repositories (repo.ReposForStore, the rule routing and GC use) and the
// filesystem totals of its blob tree.
func (u *UI) blobStoreView(ctx context.Context, bs meta.BlobStore) (blobStoreJSON, error) {
	repos := repo.ReposForStore(u.Repos.Snapshot(), bs.Name)
	count, bytes, err := u.DB.BlobStatsInRepos(ctx, repos)
	if err != nil {
		return blobStoreJSON{}, err
	}
	root := repo.StoreRoot(u.DataDir, bs)
	out := blobStoreJSON{
		Name: bs.Name, Type: bs.Type, Path: root,
		BlobCount: count, UsedBytes: bytes, RepoCount: len(repos),
	}
	if out.Type == "" {
		out.Type = meta.BlobStoreFile
	}
	out.Quota = bs.Quota
	if u.Store != nil {
		out.MemberOf = u.Store.GroupOf(bs.Name)
		out.Unavailable = cmp.Or(u.Store.Unopened(bs.Name), u.probeFailure(bs.Name))
	}
	if bs.Type == meta.BlobStoreGroup && bs.Group != nil {
		out.Members, out.FillPolicy = bs.Group.Members, bs.Group.FillPolicy
		out.QuotaViolated, _ = quotaState(out)
		return out, nil
	}
	if bs.Type == meta.BlobStoreS3 && bs.S3 != nil {
		c := bs.S3
		out.S3 = &s3JSON{Bucket: c.Bucket, Prefix: c.Prefix, Region: c.Region, Endpoint: c.Endpoint,
			PathStyle: c.PathStyle, AccessKey: c.AccessKey, SecretSet: c.SecretKey != "", SSE: c.SSE,
			Encryption: c.Encryption, Failover: append([]blob.S3Failover{}, c.Failover...), MaxConnections: c.MaxConnections}
		out.QuotaViolated, _ = quotaState(out)
		return out, nil
	}
	if bs.Type == meta.BlobStoreAzure && bs.Azure != nil {
		c := bs.Azure
		out.Azure = &azureJSON{AccountName: c.AccountName, ContainerName: c.ContainerName, Prefix: c.Prefix, Endpoint: c.Endpoint,
			Auth: c.Auth, TenantID: c.TenantID, ClientID: c.ClientID, AuthorityHost: c.AuthorityHost, IMDSEndpoint: c.IMDSEndpoint,
			AccountKeySet: c.AccountKey != "", SASTokenSet: c.SASToken != "", ClientSecretSet: c.ClientSecret != ""}
		out.QuotaViolated, _ = quotaState(out) // a container: no local filesystem figures, as for S3
		return out, nil
	}
	if bs.Type == meta.BlobStoreGoogle && bs.Google != nil {
		c := bs.Google
		out.Google = &gcsJSON{Bucket: c.Bucket, Prefix: c.Prefix, Endpoint: c.Endpoint, Authentication: c.Authentication,
			AccountKeySet: c.AccountKey != "", ServiceAccount: c.ServiceAccount()}
		out.QuotaViolated, _ = quotaState(out)
		return out, nil
	}
	if total, free, ok := sysinfo.DiskUsage(filepath.Join(root, "blobs")); ok {
		out.FsTotal, out.FsFree = &total, &free
	}
	out.QuotaViolated, _ = quotaState(out)
	dd, err1 := realPath(u.DataDir)
	rt, err2 := realPath(root)
	if err1 == nil && err2 == nil && !isSubpath(dd, rt) {
		out.Warning = "outside the data directory " + u.DataDir + ": mount and back it up separately"
	}
	return out, nil
}

func (u *UI) listBlobStores(w http.ResponseWriter, r *http.Request) {
	if !u.requirePicker(w, r, "blobstores") {
		return
	}
	rows, err := u.DB.ListBlobStores(r.Context())
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.storeProblems() // the views read the probes
	out := make([]blobStoreJSON, 0, len(rows))
	for _, bs := range rows {
		v, err := u.blobStoreView(r.Context(), bs)
		if err != nil {
			u.fail(w, r, err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"blobstores": out})
}

func (u *UI) getBlobStore(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.requireApp(w, r, "blobstores", meta.ActionRead); !ok {
		return
	}
	bs, err := u.DB.GetBlobStore(r.Context(), r.PathValue("name"))
	if err != nil {
		u.writeBlobStoreError(w, r, err)
		return
	}
	u.storeProblems() // the view reads the probes
	v, err := u.blobStoreView(r.Context(), bs)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (u *UI) createBlobStore(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	if u.Store == nil {
		writeError(w, http.StatusNotImplemented, "blob stores are not available")
		return
	}
	var body blobStoreRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if err := repo.CheckName("blob store", body.Name); err != nil {
		u.writeBlobStoreError(w, r, err)
		return
	}
	row := meta.BlobStore{Name: body.Name, Path: filepath.Join("blobstores", body.Name), Type: body.Type, Quota: body.Quota}
	if err := meta.ValidateQuota(body.Quota); err != nil {
		u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{err.Error()}})
		return
	}
	switch {
	case filepath.IsAbs(body.Path):
		row.Path = filepath.Clean(body.Path)
	case body.Path != "":
		u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{
			"blob store path must be absolute; leave it empty for <dataDir>/blobstores/" + body.Name}})
		return
	case body.RelativePath != "":
		p := filepath.Clean(body.RelativePath)
		if !filepath.IsLocal(p) || p == "." {
			u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{"path: a relative path must name a directory inside <dataDir>/blobstores"}})
			return
		}
		row.Path = filepath.Join("blobstores", p)
	}
	switch row.Type {
	case "":
		row.Type = meta.BlobStoreFile
	case meta.BlobStoreFile:
	case meta.BlobStoreGroup:
		cfg := blob.GroupConfig{Members: body.Members, FillPolicy: body.FillPolicy}
		if err := cfg.Validate(); err != nil {
			u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{err.Error()}})
			return
		}
		if body.S3 != nil {
			u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{`"s3" is an s3 store's ("type": "s3")`}})
			return
		}
		row.Group = &cfg
	case meta.BlobStoreS3:
		if body.S3 == nil {
			u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{"an s3 blob store needs \"s3\" with at least its bucket"}})
			return
		}
		if err := body.S3.Validate(); err != nil {
			u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{"s3: " + err.Error()}})
			return
		}
		row.S3 = body.S3
	case meta.BlobStoreAzure:
		if body.Azure == nil {
			u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{`an azure blob store needs "azure" with its accountName, containerName and credentials`}})
			return
		}
		if err := body.Azure.Validate(); err != nil {
			u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{"azure: " + err.Error()}})
			return
		}
		row.Azure = body.Azure
	case meta.BlobStoreGoogle:
		if body.Google == nil {
			u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{`a google blob store needs "google" with at least its bucket`}})
			return
		}
		if err := body.Google.Validate(); err != nil {
			u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{"google: " + err.Error()}})
			return
		}
		row.Google = body.Google
	default:
		u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{`blob store type must be "file", "s3", "group", "azure" or "google"`}})
		return
	}
	if row.Type != meta.BlobStoreGoogle && body.Google != nil {
		u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{`"google" is a google store's ("type": "google")`}})
		return
	}
	if row.Type != meta.BlobStoreGroup && (body.Members != nil || body.FillPolicy != "") {
		u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{`members and fillPolicy are a group's ("type": "group")`}})
		return
	}
	err := u.Repos.WithWriteLock(r.Context(), func(snap *repo.Snapshot) error {
		ctx := r.Context()
		// A duplicate name is a conflict, not an isolation problem.
		if _, err := u.DB.GetBlobStore(ctx, body.Name); err == nil {
			return meta.ErrExists
		} else if !errors.Is(err, meta.ErrNotFound) {
			return err
		}
		root := repo.StoreRoot(u.DataDir, row)
		if row.Type == meta.BlobStoreGroup {
			if err := u.checkGroupMembers(ctx, snap, body.Name, *row.Group, nil); err != nil {
				return err
			}
		}
		if loc := bucketLocation(row); loc != "" {
			if err := u.checkBucketIsolation(ctx, loc); err != nil {
				return err
			}
		} else if row.Type == meta.BlobStoreAzure {
			if err := u.checkAzureIsolation(ctx, *row.Azure); err != nil {
				return err
			}
		} else {
			if err := u.checkStoreIsolation(ctx, root); err != nil {
				return err
			}
			if err := checkNoBlobs(root); err != nil {
				return &repo.ValidationError{Errors: []string{fmt.Sprintf("blob store %q: %v", body.Name, err)}}
			}
		}
		var bs *blob.Store
		var err error
		if row.Type == meta.BlobStoreGroup {
			bs, err = repo.OpenGroup(u.DataDir, row, u.Store.StoreByName)
		} else {
			bs, err = repo.OpenStore(u.DataDir, row)
		}
		if err != nil {
			return &repo.ValidationError{Errors: []string{fmt.Sprintf("blob store %q: directory %s is not usable: %v", body.Name, root, err)}}
		}
		if err := probeWritable(root); err != nil {
			return &repo.ValidationError{Errors: []string{fmt.Sprintf("blob store %q: directory %s is not writable: %v", body.Name, root, err)}}
		}
		if loc := bucketLocation(row); loc != "" {
			if err := bs.Check(); err != nil {
				return &repo.ValidationError{Errors: []string{fmt.Sprintf("blob store %q: bucket %s is not usable: %v", body.Name, loc, err)}}
			}
		}
		if row.Type == meta.BlobStoreAzure {
			if err := bs.Check(); err != nil {
				return &repo.ValidationError{Errors: []string{fmt.Sprintf("blob store %q: container %s is not usable: %v", body.Name, row.Azure.ContainerName, err)}}
			}
		}
		if err := u.DB.CreateBlobStore(ctx, row); err != nil {
			return err
		}
		u.Store.AddStore(body.Name, bs)
		return nil
	})
	if err != nil {
		u.writeBlobStoreError(w, r, err)
		return
	}
	stored, err := u.DB.GetBlobStore(r.Context(), body.Name)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	detail := repo.StoreRoot(u.DataDir, stored)
	if stored.Type == meta.BlobStoreS3 && stored.S3 != nil {
		detail = "s3://" + stored.S3.Bucket + "/" + stored.S3.Prefix
	}
	if stored.Type == meta.BlobStoreGoogle && stored.Google != nil {
		detail = "gs://" + stored.Google.Bucket + "/" + stored.Google.Prefix
	}
	if stored.Group != nil {
		detail = groupDetail(*stored.Group)
	}
	if stored.Azure != nil {
		detail = azureDetail(*stored.Azure)
	}
	u.notify(r.Context(), "blobstores")
	u.emit(r, "blobstore.create", "", body.Name, detail)
	v, err := u.blobStoreView(r.Context(), stored)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// updateBlobStore changes the soft quota: {"quota": {type, limitBytes}} sets
// it, {"quota": null} removes it; a group's members and fill policy; an
// azure store's credentials ({"azure": {...}}, setAzureAuth); and an S3
// store's credentials, endpoint, region, path style and connection bound
// ({"s3": {...}}, setS3Connection).
func (u *UI) updateBlobStore(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	var raw map[string]json.RawMessage
	if !decodeBody(w, r, &raw) {
		return
	}
	var body blobStoreRequest
	b, _ := json.Marshal(raw)
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	_, setQuota := raw["quota"]
	_, members := raw["members"]
	_, fill := raw["fillPolicy"]
	_, setAzure := raw["azure"]
	_, setS3 := raw["s3"]
	setGroup := members || fill
	name := r.PathValue("name")
	if !setQuota && !setGroup && !setAzure && !setS3 {
		u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{"nothing to change: send quota, a group's members and fillPolicy, an azure store's credentials or an s3 store's connection settings"}})
		return
	}
	if setQuota {
		if err := meta.ValidateQuota(body.Quota); err != nil {
			u.writeBlobStoreError(w, r, &repo.ValidationError{Errors: []string{err.Error()}})
			return
		}
	}
	if setGroup {
		if u.Store == nil {
			writeError(w, http.StatusNotImplemented, "blob stores are not available")
			return
		}
		cfg, err := u.setGroupMembers(r.Context(), name, body)
		if err != nil {
			u.writeBlobStoreError(w, r, err)
			return
		}
		u.notify(r.Context(), "blobstores")
		u.syncNodes(r.Context(), name)
		u.emit(r, "blobstore.update", "", name, groupDetail(cfg))
	}
	if setAzure {
		if u.Store == nil {
			writeError(w, http.StatusNotImplemented, "blob stores are not available")
			return
		}
		cfg, err := u.setAzureAuth(r.Context(), name, body.Azure)
		if err != nil {
			u.writeBlobStoreError(w, r, err)
			return
		}
		u.notify(r.Context(), "blobstores")
		u.emit(r, "blobstore.update", "", name, "credentials: "+azureDetail(cfg))
	}
	if setS3 {
		if u.Store == nil {
			writeError(w, http.StatusNotImplemented, "blob stores are not available")
			return
		}
		cfg, changed, err := u.setS3Connection(r.Context(), name, body.S3)
		if err != nil {
			u.writeBlobStoreError(w, r, err)
			return
		}
		if changed {
			u.notify(r.Context(), "blobstores")
			u.syncNodes(r.Context(), name) // an old key may be revoked once this answers
			u.emit(r, "blobstore.update", "", name, "connection: "+cfg.Location())
		}
	}
	if setQuota {
		if err := u.DB.SetBlobStoreQuota(r.Context(), name, body.Quota); err != nil {
			u.writeBlobStoreError(w, r, err)
			return
		}
		detail := "quota removed"
		if q := body.Quota; q != nil {
			detail = fmt.Sprintf("quota %s %d", q.Type, q.LimitBytes)
		}
		u.emit(r, "blobstore.update", "", name, detail)
	}
	stored, err := u.DB.GetBlobStore(r.Context(), name)
	if err != nil {
		u.writeBlobStoreError(w, r, err)
		return
	}
	v, err := u.blobStoreView(r.Context(), stored)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// quotaState evaluates v's soft quota: spaceUsedBytes against the bytes its
// repositories use, spaceRemainingBytes against the free space of its
// filesystem (unknown for an S3 store: never violated). The message says
// which.
func quotaState(v blobStoreJSON) (violated bool, msg string) {
	q := v.Quota
	switch {
	case q == nil:
		return false, "no quota"
	case q.Type == meta.QuotaSpaceUsed:
		return v.UsedBytes > q.LimitBytes, fmt.Sprintf("%d of %d bytes used", v.UsedBytes, q.LimitBytes)
	case v.FsFree == nil:
		return false, "free space unknown"
	}
	return int64(*v.FsFree) < q.LimitBytes, fmt.Sprintf("%d bytes free, %d required", *v.FsFree, q.LimitBytes)
}

// quotaChecks is one "Blob store quota: <name>" health check per store with a
// quota. A store found over its quota, or back within it, records a
// blobstore.quota event once per flip (the System actor).
func (u *UI) quotaChecks(ctx context.Context) []check {
	rows, err := u.DB.ListBlobStores(ctx)
	if err != nil {
		return []check{runCheck("Blob store quota", func() (bool, string) { return false, "cannot list blob stores: " + err.Error() })}
	}
	var out []check
	for _, bs := range rows {
		if bs.Quota == nil {
			continue
		}
		out = append(out, runCheck("Blob store quota: "+bs.Name, func() (bool, string) {
			v, err := u.blobStoreView(ctx, bs)
			if err != nil {
				return false, "cannot read usage: " + err.Error()
			}
			violated, msg := quotaState(v)
			u.quotaMu.Lock()
			flipped := violated != u.quotaViolated[bs.Name]
			if u.quotaViolated == nil {
				u.quotaViolated = map[string]bool{}
			}
			u.quotaViolated[bs.Name] = violated
			u.quotaMu.Unlock()
			if flipped && u.Events != nil {
				state := "violated: "
				if !violated {
					state = "within limit: "
				}
				u.Events.Emit("blobstore.quota", events.System, "", bs.Name, state+msg)
			}
			if violated {
				return false, "soft quota exceeded: " + msg
			}
			return true, msg
		}))
	}
	return out
}

// CheckQuotas runs the soft quota checks without a client asking (Nexus
// checks every minute): a store over its quota is logged each time, and a
// flip records its event as in quotaChecks. The leader runs it. ponytail: a
// full count of each store with a quota per minute, what a status check has
// always cost; keep running totals if it shows on stores of millions of assets.
func (u *UI) CheckQuotas(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, statsTimeout)
	defer cancel()
	for _, c := range u.quotaChecks(ctx) {
		if !c.Healthy && u.Log != nil && ctx.Err() == nil {
			u.Log.Warn("a blob store is over its soft quota", "check", c.Name, "detail", c.Message)
		}
	}
}

// quotaAggregate is the "Blob Stores Quota" check: one verdict over the
// per-store checks.
func quotaAggregate(checks []check) check {
	return runCheck("Blob Stores Quota", func() (bool, string) {
		var over []string
		for _, c := range checks {
			if !c.Healthy {
				over = append(over, strings.TrimPrefix(c.Name, "Blob store quota: ")+": "+c.Message)
			}
		}
		if len(over) > 0 {
			return false, fmt.Sprintf("%d of %d blob stores with a soft quota are over it: %s", len(over), len(checks), strings.Join(over, "; "))
		}
		return true, fmt.Sprintf("none of the %d blob stores with a soft quota is over it", len(checks))
	})
}

// ---- blob store probes ----

// storeProbeTTL is how long a probe's answer stands: a status page, a
// Prometheus scrape and a blob store list all ask, and a probe of a bucket is
// a request to it. A var for tests.
var storeProbeTTL = 30 * time.Second

// storeProbe is one store's last probe on this node (a store's bucket or disk
// can fail on one node alone).
type storeProbe struct {
	why     string        // what is wrong; "": nothing
	at      time.Time     // when it answered; zero until it did
	done    chan struct{} // while it runs
	started time.Time     // and since when
}

func (u *UI) probeExpired(p *storeProbe) bool {
	return p.at.IsZero() || u.clock().Sub(p.at) >= storeProbeTTL
}

// storeProblems probes the stores this node has open whose last answer is
// old, each in a goroutine of its own, gives each blobCheckTimeout, and
// returns what is wrong per store: why it did not open or what its probe says,
// a probe that has not answered being a problem. One that hangs keeps its
// goroutine, no other starts on top of it, and nobody waits for it again.
func (u *UI) storeProblems() map[string]string {
	if u.Store == nil {
		return nil
	}
	out := u.Store.UnopenedStores()
	names := u.Store.StoreNames()
	var running []chan struct{}
	var patience time.Duration
	u.probeMu.Lock()
	if u.probes == nil {
		u.probes = map[string]*storeProbe{}
	}
	for name := range u.probes {
		if !slices.Contains(names, name) {
			delete(u.probes, name)
		}
	}
	for _, name := range names {
		bs := u.Store.StoreByName(name)
		p := u.probes[name]
		if p == nil {
			p = &storeProbe{}
			u.probes[name] = p
		}
		if p.done == nil && bs != nil && u.probeExpired(p) {
			p.done, p.started = make(chan struct{}), time.Now()
			go u.runProbe(bs, p)
		}
		if p.done != nil {
			running = append(running, p.done)
			patience = max(patience, blobCheckTimeout-time.Since(p.started))
		}
	}
	u.probeMu.Unlock()
	timeout := time.NewTimer(patience)
	defer timeout.Stop()
wait:
	for _, done := range running {
		select {
		case <-done:
		case <-timeout.C:
			break wait
		}
	}
	u.probeMu.Lock()
	defer u.probeMu.Unlock()
	for _, name := range names {
		if p := u.probes[name]; p != nil && p.done != nil && u.probeExpired(p) {
			out[name] = fmt.Sprintf("no answer to a probe within %s", blobCheckTimeout)
		} else if p != nil && p.why != "" {
			out[name] = p.why
		}
	}
	return out
}

func (u *UI) runProbe(bs *blob.Store, p *storeProbe) {
	var bad []string
	for _, c := range bs.Health() {
		if !c.OK {
			bad = append(bad, c.Target+": "+c.Error)
		}
	}
	u.probeMu.Lock()
	defer u.probeMu.Unlock()
	p.why, p.at = strings.Join(bad, "; "), u.clock()
	close(p.done)
	p.done = nil
}

// probeFailure is what the last probe of the named store found, "" when it
// found nothing; it asks nothing.
func (u *UI) probeFailure(name string) string {
	u.probeMu.Lock()
	defer u.probeMu.Unlock()
	if p := u.probes[name]; p != nil {
		return p.why
	}
	return ""
}

// forgetProbe drops the store's last probe: its settings changed.
func (u *UI) forgetProbe(name string) {
	u.probeMu.Lock()
	defer u.probeMu.Unlock()
	delete(u.probes, name)
}

// errStoreInUse refuses deleting a store a repository uses.
type errStoreInUse struct {
	name  string
	repos []string
}

func (e *errStoreInUse) Error() string {
	return fmt.Sprintf("blob store %q is used by repositories: %s", e.name, strings.Join(e.repos, ", "))
}

func (u *UI) deleteBlobStore(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	name := r.PathValue("name")
	if name == repo.DefaultStore {
		writeError(w, http.StatusConflict, "the default blob store cannot be deleted")
		return
	}
	err := u.Repos.WithWriteLock(r.Context(), func(s *repo.Snapshot) error {
		if users := repo.ReposForStore(s, name); len(users) > 0 {
			return &errStoreInUse{name: name, repos: users}
		}
		if err := u.refuseGroupMember(r.Context(), name); err != nil {
			return err
		}
		if u.Store != nil { // a move from or to it is unfinished
			if moving := u.Store.Moving(name); len(moving) > 0 {
				return &errStoreInUse{name: name, repos: moving}
			}
		}
		if blobStoreDeleteHook != nil {
			blobStoreDeleteHook()
		}
		if err := u.DB.DeleteBlobStore(r.Context(), name); err != nil {
			return err
		}
		if u.Store != nil {
			u.Store.RemoveStore(name) // the directory is left on disk
		}
		return nil
	})
	var inUse *errStoreInUse
	if errors.As(err, &inUse) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": inUse.Error(), "repositories": inUse.repos})
		return
	}
	if err != nil {
		u.writeBlobStoreError(w, r, err)
		return
	}
	u.notify(r.Context(), "blobstores")
	u.emit(r, "blobstore.delete", "", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// bucketLocation is the object namespace of an S3 or google row (its
// Location), "" for a file store or a group.
func bucketLocation(bs meta.BlobStore) string {
	switch {
	case bs.Type == meta.BlobStoreS3 && bs.S3 != nil:
		return bs.S3.Location()
	case bs.Type == meta.BlobStoreGoogle && bs.Google != nil:
		return bs.Google.Location()
	}
	return ""
}

// checkBucketIsolation rejects a new S3 or google store on the same bucket
// and prefix (at the same endpoint) as an existing one of either type (GCS
// serves the S3 API too): their GCs would delete each other's blobs.
func (u *UI) checkBucketIsolation(ctx context.Context, loc string) error {
	rows, err := u.DB.ListBlobStores(ctx)
	if err != nil {
		return err
	}
	for _, bs := range rows {
		if bucketLocation(bs) == loc {
			return &repo.ValidationError{Errors: []string{fmt.Sprintf("bucket and prefix %s are already the %q store's", loc, bs.Name)}}
		}
	}
	return nil
}

// checkStoreIsolation rejects a new store whose blob tree equals, contains or
// is contained by an existing store's blob tree. Trees are compared on their
// /blobs subdirectories (not the roots, since the data directory contains
// every relative store) after resolving symlinks, so a link or an alias of
// another store's directory is caught too.
func (u *UI) checkStoreIsolation(ctx context.Context, root string) error {
	newTree, err := realPath(filepath.Join(root, "blobs"))
	if err != nil {
		return &repo.ValidationError{Errors: []string{"invalid blob store path"}}
	}
	rows, err := u.DB.ListBlobStores(ctx)
	if err != nil {
		return err
	}
	for _, bs := range rows {
		other, err := realPath(filepath.Join(repo.StoreRoot(u.DataDir, bs), "blobs"))
		if err != nil {
			// Fail closed (final review M1): a tree that cannot be resolved
			// (a dangling symlink) cannot be shown to be disjoint.
			return &repo.ValidationError{Errors: []string{
				fmt.Sprintf("cannot check the blob store path against the %q store at %s: its path cannot be resolved", bs.Name, repo.StoreRoot(u.DataDir, bs))}}
		}
		if treeConflicts(newTree, other) {
			return &repo.ValidationError{Errors: []string{
				fmt.Sprintf("blob store path conflicts with the %q store at %s", bs.Name, repo.StoreRoot(u.DataDir, bs))}}
		}
	}
	return nil
}

// realPath makes p absolute and resolves the symlinks of its longest
// existing prefix; the part that does not exist yet is appended as is.
func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	existing, rest := abs, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing { // reached the root without finding anything
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, rest), nil
}

// checkNoBlobs refuses a root whose blobs/sha256 directory already holds
// entries: a new store has no repositories, so its GC would delete them.
func checkNoBlobs(root string) error {
	d, err := os.Open(filepath.Join(root, "blobs", "sha256"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer d.Close()
	if _, err := d.Readdirnames(1); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("directory already contains blobs")
}

// treeConflicts reports whether two blob trees are the same, or one nests
// inside the other.
func treeConflicts(a, b string) bool {
	return isSubpath(a, b) || isSubpath(b, a)
}

// isSubpath reports whether child is at or below parent.
func isSubpath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// probeWritable confirms a store's tmp directory accepts a file, which is what
// blob writes use. blob.Open has already created root/tmp.
func probeWritable(root string) error {
	f, err := os.CreateTemp(filepath.Join(root, "tmp"), "probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// writeBlobStoreError maps blob-store errors: validation 400 with the list,
// a name taken 409, not found 404, the registry's HTTP errors as they are,
// anything else 500.
func (u *UI) writeBlobStoreError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *repo.ValidationError
	var he *repo.Error
	switch {
	case errors.As(err, &ve):
		first := "invalid blob store"
		if len(ve.Errors) > 0 {
			first = ve.Errors[0]
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": first, "errors": ve.Errors})
	case errors.Is(err, meta.ErrExists):
		writeError(w, http.StatusConflict, "a blob store with this name already exists")
	case errors.Is(err, meta.ErrNotFound):
		writeError(w, http.StatusNotFound, "blob store not found")
	case errors.As(err, &he):
		writeError(w, he.Status, he.Msg)
	default:
		u.fail(w, r, err)
	}
}

// checkAzureIsolation rejects a new azure store on the same container and
// prefix (at the same endpoint) as an existing one: their GCs would delete
// each other's blobs.
func (u *UI) checkAzureIsolation(ctx context.Context, c blob.AzureConfig) error {
	rows, err := u.DB.ListBlobStores(ctx)
	if err != nil {
		return err
	}
	for _, bs := range rows {
		if o := bs.Azure; bs.Type == meta.BlobStoreAzure && o != nil && o.Location() == c.Location() {
			return &repo.ValidationError{Errors: []string{
				fmt.Sprintf("azure container %s prefix %q is already the %q store's", c.ContainerName, c.Prefix, bs.Name)}}
		}
	}
	return nil
}

// setS3Connection replaces the credentials, endpoint, region, path style and
// connection bound of the S3 store name with those of n under the registry
// lock, taken as sent, but for a secret left empty beside an access key,
// which keeps the stored one (no key at all is the AWS_* environment or an AWS
// role) unless the endpoint changes: as SMTP's and LDAP's, a stored secret does
// not follow the settings to another server, where a signature made with it
// could be guessed at. Bucket, prefix, encryption and failover buckets cannot
// change (send them empty or unchanged). The bucket must list with the new
// settings before they are stored and used, and the other nodes take them from
// the row (repo.Store.SyncStores). changed is false when n says nothing new.
func (u *UI) setS3Connection(ctx context.Context, name string, n *blob.S3Config) (c blob.S3Config, changed bool, err error) {
	err = u.Repos.WithWriteLock(ctx, func(*repo.Snapshot) error {
		row, err := u.DB.GetBlobStore(ctx, name)
		if err != nil {
			return err
		}
		if row.Type != meta.BlobStoreS3 || row.S3 == nil || n == nil {
			return &repo.ValidationError{Errors: []string{`"s3" is an s3 store's ("type": "s3")`}}
		}
		cur := *row.S3
		c = *n
		if c.AccessKey != "" && c.SecretKey == "" {
			if c.Endpoint != cur.Endpoint {
				return &repo.ValidationError{Errors: []string{"s3: re-enter the secret key when changing the endpoint"}}
			}
			c.SecretKey = cur.SecretKey
		}
		if c.Bucket != "" && c.Bucket != cur.Bucket || c.Prefix != "" && strings.Trim(c.Prefix, "/") != strings.Trim(cur.Prefix, "/") {
			return &repo.ValidationError{Errors: []string{"s3: the bucket and prefix cannot change; create another store and move the repositories"}}
		}
		c.Bucket, c.Prefix = cur.Bucket, cur.Prefix
		if c.SSE == "" && c.Encryption == nil {
			c.SSE, c.Encryption = cur.SSE, cur.Encryption
		}
		if c.Failover == nil {
			c.Failover = cur.Failover
		}
		if err := c.Validate(); err != nil {
			return &repo.ValidationError{Errors: []string{"s3: " + err.Error()}}
		}
		if c.SSE != cur.SSE || kmsKeyOf(c) != kmsKeyOf(cur) || !slices.Equal(c.Failover, cur.Failover) {
			return &repo.ValidationError{Errors: []string{"s3: encryption and failover cannot change; create another store and move the repositories"}}
		}
		if c.SameConnection(cur) {
			return nil
		}
		if loc := c.Location(); loc != cur.Location() {
			if err := u.checkBucketIsolation(ctx, loc); err != nil {
				return err
			}
		}
		if err := blob.CheckS3(c); err != nil {
			return &repo.ValidationError{Errors: []string{fmt.Sprintf("blob store %q: bucket %s is not usable with these settings: %v", name, c.Location(), err)}}
		}
		if err := u.DB.UpdateBlobStoreS3(ctx, name, c); err != nil {
			return err
		}
		changed = true
		u.forgetProbe(name)
		if bs := u.Store.StoreByName(name); bs != nil {
			return bs.SetS3(c)
		}
		return nil // not open on this node: SyncStores opens it
	})
	return c, changed, err
}

func kmsKeyOf(c blob.S3Config) string {
	if c.Encryption != nil {
		return c.Encryption.KMSKeyID
	}
	return ""
}

// azureDetail names an azure store's container and method for the activity log.
func azureDetail(c blob.AzureConfig) string {
	return "azure://" + c.AccountName + "/" + c.ContainerName + "/" + c.Prefix + " (" + c.Auth + ")"
}

// setAzureAuth replaces the credentials of the azure store name with those
// of n under the registry lock: its account, container, prefix and endpoint
// cannot change (send them empty or unchanged), the method defaults to the
// stored one, a secret left empty under the same method keeps the stored one,
// and tenantId, clientId, authorityHost and imdsEndpoint are taken as sent.
// The credentials must list the container before they are stored and used.
func (u *UI) setAzureAuth(ctx context.Context, name string, n *blob.AzureConfig) (blob.AzureConfig, error) {
	var c blob.AzureConfig
	err := u.Repos.WithWriteLock(ctx, func(*repo.Snapshot) error {
		row, err := u.DB.GetBlobStore(ctx, name)
		if err != nil {
			return err
		}
		if row.Type != meta.BlobStoreAzure || row.Azure == nil || n == nil {
			return &repo.ValidationError{Errors: []string{`"azure" is an azure store's ("type": "azure")`}}
		}
		cur := *row.Azure
		c = *n
		for _, f := range [][3]string{{"accountName", c.AccountName, cur.AccountName}, {"containerName", c.ContainerName, cur.ContainerName},
			{"prefix", strings.Trim(c.Prefix, "/"), cur.Prefix}, {"endpoint", c.Endpoint, cur.Endpoint}} {
			if f[1] != "" && f[1] != f[2] {
				return &repo.ValidationError{Errors: []string{"azure: " + f[0] + " cannot change; create another store and move the repositories"}}
			}
		}
		c.AccountName, c.ContainerName, c.Prefix, c.Endpoint = cur.AccountName, cur.ContainerName, cur.Prefix, cur.Endpoint
		c.Auth = cmp.Or(c.Auth, cur.Auth)
		if c.Auth == cur.Auth {
			c.AccountKey, c.SASToken, c.ClientSecret = cmp.Or(c.AccountKey, cur.AccountKey), cmp.Or(c.SASToken, cur.SASToken), cmp.Or(c.ClientSecret, cur.ClientSecret)
		}
		if err := c.Validate(); err != nil {
			return &repo.ValidationError{Errors: []string{"azure: " + err.Error()}}
		}
		if err := blob.CheckAzure(c); err != nil {
			return &repo.ValidationError{Errors: []string{fmt.Sprintf("blob store %q: container %s is not usable with these credentials: %v", name, c.ContainerName, err)}}
		}
		if err := u.DB.UpdateBlobStoreAzure(ctx, name, c); err != nil {
			return err
		}
		if bs := u.Store.StoreByName(name); bs != nil {
			return bs.SetAzure(c)
		}
		return nil
	})
	return c, err
}
