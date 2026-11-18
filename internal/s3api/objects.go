package s3api

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/format/raw"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
)

// attrs is what S3 keeps of an object beyond its content and Content-Type
// (the asset's content type): the ETag of a multipart upload, the
// x-amz-meta-* user metadata (names in lower case) and the standard headers
// S3 stores (Cache-Control, Content-Disposition, Content-Encoding,
// Content-Language, Expires). They are kept, without a schema change, as
// JSON after attrsPrefix in the asset's remote_etag column, which hosted
// raw assets do not use otherwise (a proxy's assets keep their remote's
// ETag there): it travels with the asset through export and import,
// staging moves and the reconcile journal, and an upload through
// /repository/ writes a new row without it, as an S3 PutObject would.
type attrs struct {
	ETag    string            `json:"etag,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

const attrsPrefix = "s3:"

func readAttrs(a meta.Asset) attrs {
	var at attrs
	if v, ok := strings.CutPrefix(a.RemoteETag, attrsPrefix); ok {
		json.Unmarshal([]byte(v), &at)
	}
	return at
}

func (at attrs) encode() string {
	if at.ETag == "" && len(at.Headers) == 0 {
		return ""
	}
	b, _ := json.Marshal(at)
	return attrsPrefix + string(b)
}

// storedHeaders are the standard headers an object keeps (AWS: "Working
// with object metadata", system-defined metadata a user can set).
var storedHeaders = []string{"Cache-Control", "Content-Disposition", "Content-Encoding", "Content-Language", "Expires"}

// maxUserMeta bounds the stored headers (AWS: 2 KB of user metadata).
const maxUserMeta = 2 << 10

// objectHeaders returns the content type and attributes a write sets: the
// request's, with aws-chunked dropped from Content-Encoding. Without a
// Content-Type the path's extension decides, as for raw.
func objectHeaders(r *http.Request, key string) (string, attrs, error) {
	at := attrs{Headers: map[string]string{}}
	size := 0
	for name, vs := range r.Header {
		if strings.HasPrefix(name, "X-Amz-Meta-") {
			name = strings.ToLower(name) // as S3 keeps and answers them
			at.Headers[name] = strings.Join(vs, ",")
			size += len(name) + len(at.Headers[name])
		}
	}
	for _, name := range storedHeaders {
		v := r.Header.Get(name)
		if name == "Content-Encoding" {
			var keep []string
			for _, e := range strings.Split(v, ",") {
				if e = strings.TrimSpace(e); e != "" && e != "aws-chunked" {
					keep = append(keep, e)
				}
			}
			v = strings.Join(keep, ",")
		}
		if v != "" {
			at.Headers[name] = v
			size += len(name) + len(v)
		}
	}
	if size > maxUserMeta {
		return "", attrs{}, errf(http.StatusBadRequest, "MetadataTooLarge", "the metadata headers exceed %d bytes", maxUserMeta)
	}
	ct := r.Header.Get("Content-Type")
	if _, _, err := mime.ParseMediaType(ct); ct == "" || err != nil {
		ct = raw.ContentType(key)
	}
	return ct, at, nil
}

// etagOf is the object's S3 ETag: a multipart upload's, else the MD5 of
// the content (the raw endpoint keeps its own, Nexus's {SHA1{...}} form).
func etagOf(a meta.Asset) string {
	if at := readAttrs(a); at.ETag != "" {
		return `"` + at.ETag + `"`
	}
	if a.MD5 != "" {
		return `"` + a.MD5 + `"`
	}
	return `"` + a.BlobSHA256[:32] + `"`
}

// checkKey refuses keys raw cannot store (raw.CheckUploadPath: no "//",
// ".", "..", "\", control characters, ?, #, ; and no trailing "/") and keys
// longer than S3's 1024 bytes.
func checkKey(key string) error {
	if len(key) > 1024 {
		return errf(http.StatusBadRequest, "KeyTooLongError", "the key is longer than 1024 bytes")
	}
	if err := raw.CheckUploadPath(key); err != nil {
		return errf(http.StatusBadRequest, "InvalidArgument", "Command Center cannot store this key as a raw path: %v", err)
	}
	return nil
}

// getObject serves GetObject and HeadObject.
func (h *Handler) getObject(w http.ResponseWriter, r *http.Request, p auth.Principal, bucket, key string) error {
	rp, err := h.bucket(r, p, bucket, auth.Read, key)
	if err != nil {
		return err
	}
	if !utf8.ValidString(key) {
		return errNoSuchKey
	}
	a, err := h.Store.Get(r.Context(), rp.Name, key)
	if errors.Is(err, repo.ErrNotFound) {
		return errNoSuchKey
	}
	if err != nil {
		return err
	}
	etag := etagOf(a)
	if err := preconditions(r.Header, etag, a.LastModified, true); err != nil {
		return err
	}
	f, err := h.Store.Open(a)
	if err != nil {
		return err
	}
	defer f.Close()
	hd := w.Header()
	hd.Set("Content-Type", a.ContentType)
	if a.ContentType == "" {
		hd.Set("Content-Type", raw.ContentType(key))
	}
	hd.Set("ETag", etag)
	for k, v := range readAttrs(a).Headers {
		hd[k] = []string{v} // x-amz-meta-* in lower case, not canonicalized
	}
	if r.Method == http.MethodGet { // response-* overrides (AWS: GetObject)
		q := r.URL.Query()
		for _, name := range []string{"Content-Type", "Content-Language", "Expires", "Cache-Control", "Content-Disposition", "Content-Encoding"} {
			if v := q.Get("response-" + strings.ToLower(name)); v != "" {
				hd.Set(name, v)
			}
		}
	}
	// The conditions are answered above, by the S3 ETag; ServeContent is
	// left the range (and If-Range).
	for _, c := range []string{"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		r.Header.Del(c)
	}
	rec := &recorder{ResponseWriter: w}
	http.ServeContent(rec, r, "", a.LastModified, f)
	if r.Method == http.MethodGet && rec.status >= 200 && rec.status < 300 && h.Store.Downloads != nil {
		h.Store.Downloads(a.Repo, a.Path)
	}
	return nil
}

// preconditions evaluates If-Match, If-Unmodified-Since, If-None-Match and
// If-Modified-Since in RFC 9110's order, which is S3's: a true If-Match
// overrides a false If-Unmodified-Since, a false If-None-Match a true
// If-Modified-Since. A read answers 304 where a write answers 412.
func preconditions(hd http.Header, etag string, modified time.Time, read bool) error {
	modified = modified.Truncate(time.Second)
	if v := hd.Get("If-Match"); v != "" {
		if !etagMatch(v, etag) {
			return errPrecondition
		}
	} else if t, err := http.ParseTime(hd.Get("If-Unmodified-Since")); err == nil && modified.After(t) {
		return errPrecondition
	}
	if v := hd.Get("If-None-Match"); v != "" {
		if etagMatch(v, etag) {
			if read {
				return errNotModified
			}
			return errPrecondition
		}
	} else if t, err := http.ParseTime(hd.Get("If-Modified-Since")); read && err == nil && !modified.After(t) {
		return errNotModified
	}
	return nil
}

// etagMatch reports whether the list of ETags v ("*", quoted or not, weak
// or not) names etag.
func etagMatch(v, etag string) bool {
	for _, e := range strings.Split(v, ",") {
		e = strings.TrimPrefix(strings.TrimSpace(e), "W/")
		if e == "*" || strings.Trim(e, `"`) == strings.Trim(etag, `"`) {
			return true
		}
	}
	return false
}

// write is one object write's checks, as raw's PUT makes them: the
// repository accepts writes (writePolicy), a new key needs add, an existing
// one edit and writePolicy allow, and the request's conditions hold. Run
// before the content is read (to refuse early) and again under the path's
// lock with the content stored.
func (h *Handler) checkWrite(r *http.Request, p auth.Principal, rp *repo.Repository, key string) error {
	if rp.WritePolicy == "deny" {
		return errf(http.StatusForbidden, "AccessDenied", "repository %q is read-only", rp.Name)
	}
	a, err := h.Store.Get(r.Context(), rp.Name, key)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		if r.Header.Get("If-Match") != "" {
			return errNoSuchKey
		}
		return nil
	case err != nil:
		return err
	case r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Match") != "":
		if err := preconditions(r.Header, etagOf(a), a.LastModified, false); err != nil {
			return err
		}
	}
	if rp.WritePolicy == "allow_once" {
		return errf(http.StatusForbidden, "AccessDenied", "repository %q does not allow updating assets: %s", rp.Name, key)
	}
	if !h.Auth.CanPath(p, rp.Name, auth.Edit, key) {
		return errf(http.StatusForbidden, "AccessDenied", "user %q may not overwrite %s", p.User, key)
	}
	return nil
}

// store records content written to the blob store as rp/key, under raw's
// lock of the path, the checks repeated there. It emits raw's upload event.
func (h *Handler) store(r *http.Request, p auth.Principal, rp *repo.Repository, key, ct string, at attrs, sums blob.Sums) (meta.Asset, error) {
	ctx := r.Context()
	unlock, err := h.Store.Lock(ctx, "raw\x00"+rp.Name+"\x00"+key)
	if err != nil {
		return meta.Asset{}, err
	}
	defer unlock()
	if err := h.checkWrite(r, p, rp, key); err != nil {
		return meta.Asset{}, err
	}
	a := h.Store.NewAsset(rp.Name, key, ct, sums)
	a.RemoteETag = at.encode()
	repo.Published(ctx)(&a)
	if err := h.Store.DB.PutAsset(ctx, a); err != nil {
		return meta.Asset{}, err
	}
	h.Store.Emit(r, "upload", rp.Name, key, "")
	return a, nil
}

func (h *Handler) putObject(w http.ResponseWriter, r *http.Request, p auth.Principal, sig *signer, bucket, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	rp, err := h.bucket(r, p, bucket, auth.Add, key)
	if err != nil {
		return err
	}
	if err := h.checkWrite(r, p, rp, key); err != nil {
		return err
	}
	ct, at, err := objectHeaders(r, key)
	if err != nil {
		return err
	}
	b, err := newBody(r, sig, h.maxUpload())
	if err != nil {
		return err
	}
	content, err := repo.StrictBody(rp, key, ct, b)
	if err != nil {
		return err
	}
	sums, err := h.Store.WriteBlob(rp.Name, content, nil)
	if err != nil {
		return err
	}
	a, err := h.store(r, p, rp, key, ct, at, sums)
	if err != nil {
		return err
	}
	b.echo(w)
	w.Header().Set("ETag", etagOf(a))
	return nil
}

func (h *Handler) deleteObject(w http.ResponseWriter, r *http.Request, p auth.Principal, bucket, key string) error {
	rp, err := h.bucket(r, p, bucket, auth.Delete, key)
	if err != nil {
		return err
	}
	if err := h.remove(r, rp, key); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// remove deletes rp/key as raw's DELETE does; a missing key is no error
// (S3 answers a delete of one 204 too).
func (h *Handler) remove(r *http.Request, rp *repo.Repository, key string) error {
	if rp.WritePolicy == "deny" {
		return errf(http.StatusForbidden, "AccessDenied", "repository %q is read-only", rp.Name)
	}
	err := h.Store.Delete(r.Context(), rp.Name, key)
	if errors.Is(err, repo.ErrNotFound) {
		return nil
	}
	if err == nil {
		h.Store.Emit(r, "delete", rp.Name, key, "")
	}
	return err
}

// deleteObjects serves DeleteObjects: up to 1000 keys, each checked and
// deleted as DeleteObject would; quiet leaves out the ones deleted.
func (h *Handler) deleteObjects(w http.ResponseWriter, r *http.Request, p auth.Principal, sig *signer, rp *repo.Repository) error {
	var req struct {
		Quiet  bool
		Object []struct{ Key string }
	}
	if err := readXML(r, sig, &req); err != nil {
		return err
	}
	if len(req.Object) > 1000 {
		return errf(http.StatusBadRequest, "MalformedXML", "at most 1000 keys may be deleted at once")
	}
	type deleted struct{ Key string }
	type failed struct{ Key, Code, Message string }
	var out struct {
		XMLName struct{}  `xml:"http://s3.amazonaws.com/doc/2006-03-01/ DeleteResult"`
		Deleted []deleted `xml:"Deleted"`
		Error   []failed  `xml:"Error"`
	}
	for _, o := range req.Object {
		var err error
		if rp, err = h.bucket(r, p, rp.Name, auth.Delete, o.Key); err == nil {
			err = h.remove(r, rp, o.Key)
		}
		if err != nil {
			e := asS3Error(err)
			out.Error = append(out.Error, failed{o.Key, e.code, e.msg})
		} else if !req.Quiet {
			out.Deleted = append(out.Deleted, deleted{o.Key})
		}
	}
	writeXML(w, out)
	return nil
}

// readXML decodes a small XML request body, its digests checked.
func readXML(r *http.Request, sig *signer, v any) error {
	b, err := newBody(r, sig, 0)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(b, 2<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 2<<20 || xml.Unmarshal(data, v) != nil {
		return errf(http.StatusBadRequest, "MalformedXML", "the XML in the request body is not well-formed or too large")
	}
	return nil
}

// copySource parses x-amz-copy-source ("[/]bucket/key", URL-encoded) and
// returns the source asset when p may read it.
func (h *Handler) copySource(r *http.Request, p auth.Principal) (meta.Asset, error) {
	v := r.Header.Get("X-Amz-Copy-Source")
	if i := strings.Index(v, "?versionId="); i >= 0 {
		v = v[:i]
	}
	v, err := url.PathUnescape(v)
	bucket, key, ok := strings.Cut(strings.TrimPrefix(v, "/"), "/")
	if err != nil || !ok || key == "" {
		return meta.Asset{}, errf(http.StatusBadRequest, "InvalidArgument", "x-amz-copy-source must be <bucket>/<key>")
	}
	rp, err := h.bucket(r, p, bucket, auth.Read, key)
	if err != nil {
		return meta.Asset{}, err
	}
	a, err := h.Store.Get(r.Context(), rp.Name, key)
	if errors.Is(err, repo.ErrNotFound) {
		return meta.Asset{}, errNoSuchKey
	}
	if err != nil {
		return meta.Asset{}, err
	}
	// x-amz-copy-source-if-*: the conditions on the source, all 412.
	cond := http.Header{}
	for _, c := range []string{"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		cond.Set(c, r.Header.Get("X-Amz-Copy-Source-"+c))
	}
	if err := preconditions(cond, etagOf(a), a.LastModified, true); err != nil {
		return meta.Asset{}, errPrecondition
	}
	return a, nil
}

// sumsOf are the stored digests of a.
func sumsOf(a meta.Asset) blob.Sums {
	return blob.Sums{SHA256: a.BlobSHA256, SHA1: a.SHA1, MD5: a.MD5, SHA512: a.SHA512, Size: a.Size}
}

// copyBlob makes src's content available in dst's blob store: the same
// blob when both repositories use one store (blobs are content-addressed),
// else a copy.
func (h *Handler) copyBlob(src meta.Asset, dst *repo.Repository) (blob.Sums, error) {
	from, err := h.Store.BlobStore(src.Repo)
	if err != nil {
		return blob.Sums{}, err
	}
	to, err := h.Store.BlobStore(dst.Name)
	if err != nil {
		return blob.Sums{}, err
	}
	if from == to && src.MD5 != "" {
		return sumsOf(src), nil
	}
	f, err := h.Store.Open(src)
	if err != nil {
		return blob.Sums{}, err
	}
	defer f.Close()
	return h.Store.WriteBlob(dst.Name, f, nil)
}

// copyObject serves CopyObject: the content, and with the COPY metadata
// directive (the default) the source's type and attributes, else the
// request's. The copy's ETag is its MD5, even of a multipart source.
func (h *Handler) copyObject(w http.ResponseWriter, r *http.Request, p auth.Principal, sig *signer, bucket, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	src, err := h.copySource(r, p)
	if err != nil {
		return err
	}
	rp, err := h.bucket(r, p, bucket, auth.Add, key)
	if err != nil {
		return err
	}
	ct, at, err := objectHeaders(r, key)
	if err != nil {
		return err
	}
	switch d := r.Header.Get("X-Amz-Metadata-Directive"); d {
	case "", "COPY":
		if src.Repo == rp.Name && src.Path == key {
			return errf(http.StatusBadRequest, "InvalidRequest", "copying an object to itself needs the REPLACE metadata directive")
		}
		ct, at = src.ContentType, readAttrs(src)
	case "REPLACE":
	default:
		return errf(http.StatusBadRequest, "InvalidArgument", "unknown x-amz-metadata-directive %q", d)
	}
	at.ETag = ""
	if err := h.checkWrite(r, p, rp, key); err != nil {
		return err
	}
	if max := h.maxUpload(); max > 0 && src.Size > max {
		return errf(http.StatusRequestEntityTooLarge, "EntityTooLarge", "the object is larger than the maxUploadMiB setting allows")
	}
	io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	slow(w, r, func() (any, error) {
		sums, err := h.copyBlob(src, rp)
		if err != nil {
			return nil, err
		}
		a, err := h.store(r, p, rp, key, ct, at, sums)
		if err != nil {
			return nil, err
		}
		return struct {
			XMLName      struct{} `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CopyObjectResult"`
			LastModified string
			ETag         string
		}{LastModified: isoTime(a.LastModified), ETag: etagOf(a)}, nil
	})
	return nil
}

// isoTime is S3's time format in XML documents.
func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
