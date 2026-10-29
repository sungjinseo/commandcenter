package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/component"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/tasks"
)

// Tags, staging and blob store moves (Nexus Pro features). Components are
// selected with the search API's parameters and matching (FindComponents,
// nexusapi.Handler.Find): repository, format, group, name, version, q,
// tag and the format-specific ones, exact with "*" wildcards. The tag
// association and staging answers use Nexus's shape, so the REST API under
// /service/rest/v1 passes them through.

// ComponentFinder returns the components of repos (groups skipped) that
// the search parameters q select, each with the paths of its files.
type ComponentFinder func(ctx context.Context, repos []*repo.Repository, q url.Values) ([]component.Component, error)

const (
	tagNameMax  = 256
	tagAttrsMax = 16 << 10
	// maxStaged bounds the components one staging request moves or deletes.
	maxStaged = 5000
)

var tagNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// selectorParams are the search parameters that select components; a
// staging request needs one of them besides repository and format.
var selectorParams = []string{"q", "keyword", "group", "name", "version", "tag", "maven.groupId", "maven.artifactId",
	"maven.baseVersion", "maven.extension", "maven.classifier", "npm.scope", "docker.imageName", "docker.imageTag"}

func (u *UI) stagingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/api/tags", u.listTags)
	mux.HandleFunc("POST /-/api/tags", u.createTag)
	mux.HandleFunc("GET /-/api/tags/{name}", u.getTag)
	mux.HandleFunc("PUT /-/api/tags/{name}", u.updateTag)
	mux.HandleFunc("DELETE /-/api/tags/{name}", u.deleteTag)
	mux.HandleFunc("POST /-/api/tags/{name}/associate", u.associateTag)
	mux.HandleFunc("DELETE /-/api/tags/{name}/associate", u.associateTag)
	mux.HandleFunc("POST /-/api/staging/move", u.stagingMove)
	mux.HandleFunc("POST /-/api/staging/delete", u.stagingDelete)
	mux.HandleFunc("POST /-/api/repositories/{name}/blobstore", u.changeBlobStore)
}

// ---- tags ----

// TagTime is how tag times are written: RFC 3339 in UTC with milliseconds.
func TagTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

func tagJSON(t meta.Tag) map[string]any {
	return map[string]any{"name": t.Name, "attributes": json.RawMessage(t.Attributes),
		"firstCreated": TagTime(t.Created), "lastUpdated": TagTime(t.Updated)}
}

// tagAttributes checks a tag's attributes (absent or null: none) and
// returns them compacted.
func tagAttributes(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return "{}", nil
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return "", errors.New("attributes must be a JSON object")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", errors.New("attributes must be a JSON object")
	}
	if buf.Len() > tagAttrsMax {
		return "", fmt.Errorf("attributes must be at most %d bytes of JSON", tagAttrsMax)
	}
	return buf.String(), nil
}

func checkTagName(name string) error {
	if len(name) > tagNameMax || !tagNameRE.MatchString(name) {
		return fmt.Errorf("invalid tag name %q: use at most %d letters, digits, '.', '_' and '-', starting with a letter, digit or '_'", name, tagNameMax)
	}
	return nil
}

func writeValidation(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg, "errors": []string{msg}})
}

// GET /-/api/tags?continuationToken=<last name> (signed in): 100 tags a
// page, by name.
func (u *UI) listTags(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireUser(w, r); !ok {
		return
	}
	const page = 100
	list, err := u.DB.ListTags(r.Context(), r.URL.Query().Get("continuationToken"), page+1)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	out := map[string]any{}
	if len(list) > page {
		list = list[:page]
		out["continuationToken"] = list[page-1].Name
	}
	items := make([]map[string]any, 0, len(list))
	for _, t := range list {
		items = append(items, tagJSON(t))
	}
	out["tags"] = items
	writeJSON(w, http.StatusOK, out)
}

func (u *UI) getTag(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireUser(w, r); !ok {
		return
	}
	t, err := u.DB.GetTag(r.Context(), r.PathValue("name"))
	if errors.Is(err, meta.ErrNotFound) {
		writeError(w, http.StatusNotFound, "tag not found")
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tagJSON(t))
}

type tagRequest struct {
	Name       string          `json:"name"`
	Attributes json.RawMessage `json:"attributes"`
}

// POST /-/api/tags {"name", "attributes"} (admin): 201 with the tag, 409
// when the name is taken.
func (u *UI) createTag(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok || !requireUIHeader(w, r) {
		return
	}
	var body tagRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if err := checkTagName(body.Name); err != nil {
		writeValidation(w, err.Error())
		return
	}
	attrs, err := tagAttributes(body.Attributes)
	if err != nil {
		writeValidation(w, err.Error())
		return
	}
	t, err := u.DB.CreateTag(r.Context(), body.Name, attrs)
	if errors.Is(err, meta.ErrExists) {
		writeError(w, http.StatusConflict, fmt.Sprintf("tag %q already exists", body.Name))
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "tag.create", "", t.Name, attrs)
	writeJSON(w, http.StatusCreated, tagJSON(t))
}

// PUT /-/api/tags/{name} {"attributes"} (admin): replaces the attributes;
// a name in the body must be the tag's.
func (u *UI) updateTag(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok || !requireUIHeader(w, r) {
		return
	}
	name := r.PathValue("name")
	var body tagRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Name != "" && body.Name != name {
		writeValidation(w, "a tag cannot be renamed")
		return
	}
	attrs, err := tagAttributes(body.Attributes)
	if err != nil {
		writeValidation(w, err.Error())
		return
	}
	t, err := u.DB.UpdateTag(r.Context(), name, attrs)
	if errors.Is(err, meta.ErrNotFound) {
		writeError(w, http.StatusNotFound, "tag not found")
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "tag.update", "", name, attrs)
	writeJSON(w, http.StatusOK, tagJSON(t))
}

// DELETE /-/api/tags/{name} (admin): the tag and its associations; 204.
func (u *UI) deleteTag(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok || !requireUIHeader(w, r) {
		return
	}
	name := r.PathValue("name")
	err := u.DB.DeleteTag(r.Context(), name)
	if errors.Is(err, meta.ErrNotFound) {
		writeError(w, http.StatusNotFound, "tag not found")
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, "tag.delete", "", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// stagedJSON is one component in a tag or staging answer.
func stagedJSON(c component.Component) map[string]any {
	return map[string]any{"repository": c.Repo, "format": c.Format, "group": c.Group, "name": c.Name, "version": c.Version}
}

func keyOfComponent(c component.Component) meta.ComponentKey {
	return meta.ComponentKey{Repo: c.Repo, Group: c.Group, Name: c.Name, Version: c.Version}
}

// selection returns the components the search parameters of r select in
// repos, or answers the error.
func (u *UI) selection(w http.ResponseWriter, r *http.Request, repos []*repo.Repository, q url.Values) ([]component.Component, bool) {
	if u.FindComponents == nil {
		writeError(w, http.StatusNotImplemented, "component search is not available")
		return nil, false
	}
	if k := q.Get("keyword"); k != "" && q.Get("q") == "" { // the UI's name for q
		q.Set("q", k)
	}
	for _, vs := range q {
		for _, v := range vs {
			if len(v) > maxQuery {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("parameters must be at most %d characters", maxQuery))
				return nil, false
			}
		}
	}
	comps, err := u.FindComponents(r.Context(), repos, q)
	if err != nil {
		u.fail(w, r, err)
		return nil, false
	}
	return comps, true
}

// POST|DELETE /-/api/tags/{name}/associate?<search parameters> (signed in):
// associates (POST) or dissociates (DELETE) the tag with the components the
// search selects, in the readable repositories or the one named by
// repository. Every repository involved needs the edit privilege, or
// nothing changes (403).
func (u *UI) associateTag(w http.ResponseWriter, r *http.Request) {
	p, ok := requireUser(w, r)
	if !ok || !requireUIHeader(w, r) {
		return
	}
	name := r.PathValue("name")
	switch _, err := u.DB.GetTag(r.Context(), name); {
	case errors.Is(err, meta.ErrNotFound):
		writeError(w, http.StatusNotFound, "tag not found")
		return
	case err != nil:
		u.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	if len(q) == 0 {
		writeError(w, http.StatusBadRequest, "give search parameters (repository, format, group, name, version, q, tag, ...)")
		return
	}
	dissociate := r.Method == http.MethodDelete
	if dissociate {
		q.Set("tag", name) // only the components that have it
	}
	var repos []*repo.Repository
	if n := q.Get("repository"); n != "" {
		rp := u.Repos.Get(n)
		if rp == nil || !u.Auth.Can(p, n, auth.Read) {
			writeError(w, http.StatusNotFound, "repository \""+n+"\" not found")
			return
		}
		repos = []*repo.Repository{rp}
	} else {
		for _, rp := range u.Repos.Snapshot().Sorted {
			if u.Auth.Can(p, rp.Name, auth.Read) {
				repos = append(repos, rp)
			}
		}
	}
	comps, ok := u.selection(w, r, repos, q)
	if !ok {
		return
	}
	denied := map[string]bool{}
	keys := make([]meta.ComponentKey, 0, len(comps))
	out := make([]map[string]any, 0, len(comps))
	for _, c := range comps {
		if !u.Auth.CanPath(p, c.Repo, auth.Edit, "") {
			denied[c.Repo] = true
		}
		keys = append(keys, keyOfComponent(c))
		out = append(out, stagedJSON(c))
	}
	if len(denied) > 0 {
		writeError(w, http.StatusForbidden, fmt.Sprintf("user %q may not edit in %s", p.User, strings.Join(sortedNames(denied), ", ")))
		return
	}
	var err error
	kind, msg, field := "tag.associate", "Association successful", "components associated"
	if dissociate {
		kind, msg, field = "tag.dissociate", "Disassociation successful", "components disassociated"
		err = u.DB.DissociateTag(r.Context(), name, keys)
	} else {
		err = u.DB.AssociateTag(r.Context(), name, keys)
	}
	if errors.Is(err, meta.ErrNotFound) {
		writeError(w, http.StatusNotFound, "tag not found")
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.emit(r, kind, q.Get("repository"), name, fmt.Sprintf("components=%d %s", len(keys), clip(q.Encode(), 300)))
	writeJSON(w, http.StatusOK, map[string]any{"status": http.StatusOK, "message": msg, "data": map[string]any{field: out}})
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---- staging ----

// stagedFailure is one component a staging request could not handle.
func stagedFailure(c component.Component, err error) map[string]any {
	j := stagedJSON(c)
	j["error"] = err.Error()
	var he *repo.Error
	if errors.As(err, &he) {
		j["error"] = he.Msg
	}
	return j
}

// stagingSource resolves the repository parameter of a staging request and
// checks what both staging requests need of it, or answers the error.
func (u *UI) stagingSource(w http.ResponseWriter, r *http.Request, p auth.Principal, q url.Values) (*repo.Repository, bool) {
	name := q.Get("repository")
	if name == "" {
		writeError(w, http.StatusBadRequest, "repository is required: the repository the components are in")
		return nil, false
	}
	selects := false
	for _, k := range selectorParams {
		selects = selects || strings.TrimSpace(q.Get(k)) != ""
	}
	if !selects {
		writeError(w, http.StatusBadRequest, "give tag or a search parameter besides repository and format (q, group, name, version, ...)")
		return nil, false
	}
	src := u.Repos.Get(name)
	if src == nil || !u.Auth.Can(p, name, auth.Read) {
		writeError(w, http.StatusNotFound, "repository \""+name+"\" not found")
		return nil, false
	}
	switch {
	case !u.Auth.CanPath(p, name, auth.Delete, ""):
		writeError(w, http.StatusForbidden, fmt.Sprintf("user %q may not delete in %q", p.User, name))
	case src.Type == "group":
		writeError(w, http.StatusBadRequest, fmt.Sprintf("repository %q is a group: name its member", name))
	case !src.IsOnline():
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("repository %q is offline", name))
	case src.Type == "hosted" && src.WritePolicy == "deny":
		writeError(w, http.StatusBadRequest, fmt.Sprintf("repository %q is read-only", name))
	case u.Deleters == nil:
		writeError(w, http.StatusNotImplemented, "component deletion is not available")
	default:
		return src, true
	}
	return nil, false
}

// movingRepo answers 409 when a repository's blobs move to another store.
func (u *UI) movingRepo(w http.ResponseWriter, names ...string) bool {
	if u.Store == nil {
		return false
	}
	for _, n := range names {
		if m, ok := u.Store.MoveOf(n); ok {
			writeError(w, http.StatusConflict, fmt.Sprintf("repository %q is moving to blob store %q: stage its components when the move has finished", n, m.Target))
			return true
		}
	}
	return false
}

// stagedResult writes a staging answer: 200 when every component was
// handled, else 409 with the failures listed.
func stagedResult(w http.ResponseWriter, verb string, data map[string]any, done, failed []map[string]any) {
	status, msg := http.StatusOK, verb+" Successful"
	if len(failed) > 0 {
		status = http.StatusConflict
		msg = fmt.Sprintf("%s failed for %d of %d components", verb, len(failed), len(done)+len(failed))
	}
	data["components failed"] = failed
	writeJSON(w, status, map[string]any{"status": status, "message": msg, "data": data})
}

// notStaged gives why a staging move refuses a format it has no importer for.
var notStaged = map[string]string{
	"docker": "an image's manifests and blobs are shared between tags and images",
}

// POST /-/api/staging/move?destination=<hosted>&repository=<hosted>&<search
// parameters> (signed in): moves the components the search selects in
// repository to destination, of the same format. Each one is copied (rows,
// files, blobs when the stores differ; destination's writePolicy applies)
// and then deleted from repository with its format's deleter; its tags go
// with it. Read and delete on repository and add on destination are needed
// (edit too to replace what destination has, writePolicy allow). A
// component that fails stays in repository and is listed; the others move.
func (u *UI) stagingMove(w http.ResponseWriter, r *http.Request) {
	p, ok := requireUser(w, r)
	if !ok || !requireUIHeader(w, r) {
		return
	}
	q := r.URL.Query()
	dstName := q.Get("destination")
	q.Del("destination")
	if dstName == "" {
		writeError(w, http.StatusBadRequest, "destination is required")
		return
	}
	src, ok := u.stagingSource(w, r, p, q)
	if !ok {
		return
	}
	dst := u.Repos.Get(dstName)
	if dst == nil || !u.Auth.Can(p, dstName, auth.Browse) {
		writeError(w, http.StatusNotFound, "repository \""+dstName+"\" not found")
		return
	}
	imp := u.Importers[src.Format]
	switch {
	case src.Type != "hosted" || dst.Type != "hosted":
		writeError(w, http.StatusBadRequest, "staging moves components between hosted repositories")
	case src.Format != dst.Format:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("repository %q is %s and %q is %s: staging moves within one format", src.Name, src.Format, dst.Name, dst.Format))
	case src.Name == dst.Name:
		writeError(w, http.StatusBadRequest, "repository and destination are the same")
	case imp == nil && notStaged[src.Format] != "":
		writeError(w, http.StatusBadRequest, fmt.Sprintf("format %s cannot be staged yet: %s", src.Format, notStaged[src.Format]))
	case imp == nil:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("format %s cannot be staged yet", src.Format))
	case !u.Auth.CanPath(p, dst.Name, auth.Add, ""):
		writeError(w, http.StatusForbidden, fmt.Sprintf("user %q may not add to %q", p.User, dst.Name))
	case !dst.IsOnline():
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("repository %q is offline", dst.Name))
	case dst.WritePolicy == "deny":
		writeError(w, http.StatusBadRequest, fmt.Sprintf("repository %q is read-only", dst.Name))
	case u.movingRepo(w, src.Name, dst.Name):
	default:
		comps, ok := u.selection(w, r, []*repo.Repository{src}, q)
		if !ok {
			return
		}
		if len(comps) > maxStaged {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%d components match: narrow the search (at most %d a request)", len(comps), maxStaged))
			return
		}
		overwrite := dst.WritePolicy == "allow" && u.Auth.CanPath(p, dst.Name, auth.Edit, "")
		moved, failed := []map[string]any{}, []map[string]any{}
		// A component started is finished even if the client goes; the rest
		// are skipped then.
		ctx := context.WithoutCancel(r.Context())
		for _, c := range comps {
			if r.Context().Err() != nil {
				failed = append(failed, stagedFailure(c, errors.New("not moved: the request was cancelled")))
				continue
			}
			if err := u.moveComponent(ctx, imp, src, dst, c, overwrite, p.Owner()); err != nil {
				failed = append(failed, stagedFailure(c, err))
				continue
			}
			moved = append(moved, stagedJSON(c))
		}
		u.repoCache.reset()
		u.emit(r, "staging.move", dst.Name, clip(q.Encode(), 300), fmt.Sprintf("from=%s moved=%d failed=%d", src.Name, len(moved), len(failed)))
		stagedResult(w, "Move", map[string]any{"destination": dst.Name, "components moved": moved}, moved, failed)
	}
}

// farFuture disables the download check of cleanup.CheckUnchanged: a
// staging move's delete must skip a component that changed, not one that
// was downloaded.
var farFuture = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// moveComponent copies c from src into dst and deletes it from src, only
// if its files there are still the ones copied.
func (u *UI) moveComponent(ctx context.Context, imp repo.ComponentImporter, src, dst *repo.Repository, c component.Component, overwrite bool, actor string) error {
	copied, err := imp.ImportComponent(ctx, src, dst, c, overwrite)
	if err != nil {
		return err
	}
	ev := &component.Evaluated{Paths: map[string]bool{}, LastDownloaded: farFuture}
	for _, a := range copied {
		ev.Paths[a.Path] = true
		ev.MaxCreated = maxTime(ev.MaxCreated, a.CreatedAt)
		ev.MaxModified = maxTime(ev.MaxModified, a.LastModified)
	}
	c.Evaluated = ev
	if err := u.DB.MoveComponentTags(ctx, keyOfComponent(c), dst.Name); err != nil {
		return fmt.Errorf("copied to %q, but its tags could not move, so it stays in %q too: %w", dst.Name, src.Name, err)
	}
	res, err := u.Deleters.Delete(ctx, src, c, actor)
	switch {
	case errors.Is(err, component.ErrChanged):
		return fmt.Errorf("copied to %q, but it changed in %q during the move, so it stays there too", dst.Name, src.Name)
	case err != nil:
		return fmt.Errorf("copied to %q, but not deleted from %q: %w", dst.Name, src.Name, err)
	case res.Assets == 0:
		return fmt.Errorf("copied to %q, but it was gone from %q already", dst.Name, src.Name)
	}
	return nil
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// POST /-/api/staging/delete?repository=<name>&<search parameters> (signed
// in, delete on repository): deletes the components the search selects with
// their format's deleter. Failures are listed; the others are deleted.
func (u *UI) stagingDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := requireUser(w, r)
	if !ok || !requireUIHeader(w, r) {
		return
	}
	q := r.URL.Query()
	src, ok := u.stagingSource(w, r, p, q)
	if !ok {
		return
	}
	comps, ok := u.selection(w, r, []*repo.Repository{src}, q)
	if !ok {
		return
	}
	if len(comps) > maxStaged {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%d components match: narrow the search (at most %d a request)", len(comps), maxStaged))
		return
	}
	ctx := context.WithoutCancel(r.Context())
	deleted, failed := []map[string]any{}, []map[string]any{}
	for _, c := range comps {
		if r.Context().Err() != nil {
			failed = append(failed, stagedFailure(c, errors.New("not deleted: the request was cancelled")))
			continue
		}
		dc := c
		if src.Format == "docker" && len(c.Assets) > 0 {
			dc.Version = path.Base(c.Assets[0].Path) // the manifest digest the tag points at
		}
		res, err := u.Deleters.Delete(ctx, src, dc, p.Owner())
		switch {
		case err != nil:
			failed = append(failed, stagedFailure(c, err))
		case res.Assets == 0:
			failed = append(failed, stagedFailure(c, errors.New("not found")))
		default:
			deleted = append(deleted, stagedJSON(c))
		}
	}
	u.repoCache.reset()
	u.emit(r, "staging.delete", src.Name, clip(q.Encode(), 300), fmt.Sprintf("deleted=%d failed=%d", len(deleted), len(failed)))
	stagedResult(w, "Delete", map[string]any{"components deleted": deleted}, deleted, failed)
}

// ---- change repository blob store ----

type moveJSON struct {
	Source string `json:"source"`
	Target string `json:"target"`
	State  string `json:"state"` // copying, switching, failed or interrupted
	Copied int64  `json:"copied"`
	Total  int64  `json:"total"`
}

// blobStoreMove is what GET /-/api/repositories shows admins of a move.
func (u *UI) blobStoreMove(name string) *moveJSON {
	if u.Store == nil {
		return nil
	}
	m, ok := u.Store.MoveOf(name)
	if !ok {
		return nil
	}
	return &moveJSON{Source: m.Source, Target: m.Target, State: m.State, Copied: m.Copied, Total: m.Total}
}

// POST /-/api/repositories/{name}/blobstore {"blobStore": "<target>"}
// (interactive admin): starts moving the repository's blobs to the target
// store in the background, as its repository.change-blobstore task. 202
// with the task; 400 for a missing or the same store, 409 while a task runs.
func (u *UI) changeBlobStore(w http.ResponseWriter, r *http.Request) {
	if !requireAdminInteractive(w, r) || !requireUIHeader(w, r) {
		return
	}
	if !u.tasksAvailable(w) {
		return
	}
	name := r.PathValue("name")
	if u.Repos.Get(name) == nil {
		writeError(w, http.StatusNotFound, "repository not found")
		return
	}
	var body struct {
		BlobStore string `json:"blobStore"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	t, err := u.Tasks.StartBlobStoreMove(r.Context(), name, body.BlobStore, principal(r).Owner())
	var ve *repo.ValidationError
	var busy *tasks.BusyError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": ve.Errors[0], "errors": ve.Errors})
		return
	case errors.As(err, &busy):
		writeError(w, http.StatusConflict, busy.Error()+": the move starts when its task is run again (the tasks page), or retry this")
		return
	case err != nil:
		u.fail(w, r, err)
		return
	}
	u.emit(r, "repo.blobstore", name, name, "to="+body.BlobStore+" task="+t.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"repository": name, "target": body.BlobStore, "task": t.ID})
}

// sortedNames returns the keys of a set, sorted.
func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// scanTagged fills b, for the component search's tag parameter, with the
// components of repos associated with c.tag (as the search API selects
// them) that match the other criteria, with their files.
func (u *UI) scanTagged(ctx context.Context, b *builder, repos []*repo.Repository, c criteria) error {
	if u.FindComponents == nil {
		return errors.New("tag search is not available")
	}
	comps, err := u.FindComponents(ctx, repos, url.Values{"tag": {c.tag}})
	if err != nil {
		return err
	}
	byName := map[string]*repo.Repository{}
	for _, rp := range repos {
		byName[rp.Name] = rp
	}
	for _, comp := range comps {
		rp := byName[comp.Repo]
		if rp == nil || !c.matchComponent(comp.Format, comp.Group, comp.Name, comp.Version) {
			continue
		}
		found := false
		for _, a := range comp.Assets {
			if !c.matchAsset(comp.Format, comp.Name, comp.Version, a.Path) {
				continue
			}
			ma, err := u.DB.GetAsset(ctx, comp.Repo, a.Path)
			if errors.Is(err, meta.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			found = true
			b.add(rp, comp.Group, comp.Name, comp.Version,
				&ComponentAsset{Path: ma.Path, Size: ma.Size, Downloads: ma.Downloads, LastDownloaded: ma.LastDownloaded}, ma.LastModified)
		}
		if !found && c.classifier == "" && c.extension == "" {
			b.add(rp, comp.Group, comp.Name, comp.Version, nil, time.Time{})
		}
	}
	return nil
}
