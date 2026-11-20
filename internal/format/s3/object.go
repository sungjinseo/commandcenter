package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"hash"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/sigv4"
)

// storedHeaders are the request headers an object keeps and answers with,
// besides x-amz-meta-*.
var storedHeaders = []string{"Cache-Control", "Content-Disposition", "Content-Encoding", "Content-Language", "Expires"}

// maxMetaBytes bounds the user metadata of one object, as S3 does (2 KiB).
const maxMetaBytes = 2 << 10

// objectHeaders collects the headers of an upload that the object keeps,
// keyed in lower case.
func objectHeaders(hd http.Header) (map[string]string, error) {
	out := map[string]string{}
	size := 0
	for k, v := range hd {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-meta-") {
			out[lk] = strings.Join(v, ",")
			size += len(lk) - len("x-amz-meta-") + len(out[lk])
		}
	}
	if size > maxMetaBytes {
		return nil, s3err(http.StatusBadRequest, "MetadataTooLarge", "Your metadata headers exceed the maximum allowed metadata size.")
	}
	for _, k := range storedHeaders {
		v := hd.Get(k)
		if k == "Content-Encoding" { // aws-chunked is the transfer's, not the object's
			var keep []string
			for _, e := range strings.Split(v, ",") {
				if e = strings.TrimSpace(e); e != "" && !strings.EqualFold(e, "aws-chunked") {
					keep = append(keep, e)
				}
			}
			v = strings.Join(keep, ",")
		}
		if v != "" {
			out[strings.ToLower(k)] = v
		}
	}
	return out, nil
}

// contentType is the type an upload declares, else binary/octet-stream
// (S3's default).
func contentType(hd http.Header) string {
	if ct := hd.Get("Content-Type"); ct != "" {
		if _, _, err := mime.ParseMediaType(ct); err == nil {
			return ct
		}
	}
	return "binary/octet-stream"
}

// checksums collects the digests an upload states for its body: the
// signed payload hash, Content-MD5 and an x-amz-checksum-* header. wrap
// tees the body through the ones WriteBlob does not compute; verify
// compares them once the body is read.
type checksums struct {
	md5, sha256 string // hex, from Content-MD5 and the payload hash or x-amz-checksum-sha256
	header      string // the x-amz-checksum-* header checked here
	want        string // its base64 value
	h           hash.Hash
}

func (q *req) checksums() (*checksums, error) {
	c := &checksums{sha256: q.payloadHash()}
	if v := q.r.Header.Get("Content-Md5"); v != "" {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(b) != md5.Size {
			return nil, s3err(http.StatusBadRequest, "InvalidDigest", "The Content-MD5 you specified was invalid.")
		}
		c.md5 = hex.EncodeToString(b)
	}
	trailer := strings.ToLower(q.r.Header.Get("X-Amz-Trailer"))
	for k, v := range q.r.Header {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, "x-amz-checksum-") || lk == "x-amz-checksum-algorithm" || lk == "x-amz-checksum-type" || lk == trailer || len(v) == 0 {
			continue
		}
		if lk == "x-amz-checksum-sha256" {
			b, err := base64.StdEncoding.DecodeString(v[0])
			if err != nil || len(b) != sha256.Size {
				return nil, invalidArgument("x-amz-checksum-sha256 is not a base64 SHA-256")
			}
			if c.sha256 != "" && c.sha256 != hex.EncodeToString(b) {
				return nil, errBadDigest
			}
			c.sha256 = hex.EncodeToString(b)
			continue
		}
		if c.h = sigv4.NewChecksum(lk); c.h == nil {
			return nil, invalidArgument("unsupported checksum " + lk)
		}
		c.header, c.want = lk, v[0]
	}
	return c, nil
}

func (c *checksums) wrap(r io.Reader) io.Reader {
	if c.h == nil {
		return r
	}
	return io.TeeReader(r, c.h)
}

func (c *checksums) verify(s blob.Sums) error {
	if c.md5 != "" && c.md5 != s.MD5 || c.sha256 != "" && c.sha256 != s.SHA256 {
		return errBadDigest
	}
	if c.h != nil && base64.StdEncoding.EncodeToString(c.h.Sum(nil)) != c.want {
		return errBadDigest
	}
	return nil
}

func lockKey(bucket, key string) string { return "s3\x00" + bucket + "\x00" + key }

// current returns the object at key and its ETag; ok false when there is
// none.
func (h *Handler) current(q *req, rp *repo.Repository, key string) (a meta.Asset, m meta.S3ObjectMeta, ok bool, err error) {
	a, err = h.Store.Get(q.ctx, rp.Name, key)
	if errors.Is(err, repo.ErrNotFound) {
		return meta.Asset{}, meta.S3ObjectMeta{}, false, nil
	}
	if err != nil {
		return meta.Asset{}, meta.S3ObjectMeta{}, false, err
	}
	m, err = h.objectMeta(q.ctx, a)
	return a, m, true, err
}

// objectMeta returns the S3 row of a, or one with a's MD5 as the ETag when
// it has none (or one left from another blob).
func (h *Handler) objectMeta(ctx context.Context, a meta.Asset) (meta.S3ObjectMeta, error) {
	m, err := h.Store.DB.GetS3ObjectMeta(ctx, a.Repo, a.Path)
	if err != nil && !errors.Is(err, meta.ErrNotFound) {
		return meta.S3ObjectMeta{}, err
	}
	if err != nil || m.BlobSHA256 != a.BlobSHA256 {
		m = meta.S3ObjectMeta{BlobSHA256: a.BlobSHA256}
	}
	if m.ETag == "" {
		m.ETag = a.MD5
	}
	return m, nil
}

// preconditions applies the If-Match and If-None-Match of a write to the
// object there now.
func preconditions(r *http.Request, exists bool, etag string) error {
	if v := r.Header.Get("If-None-Match"); v != "" && exists && (strings.TrimSpace(v) == "*" || matchETag(v, etag)) {
		return errPrecondition
	}
	if v := r.Header.Get("If-Match"); v != "" {
		if !exists {
			return errNoSuchKey
		}
		if strings.TrimSpace(v) != "*" && !matchETag(v, etag) {
			return errPrecondition
		}
	}
	return nil
}

func matchETag(list, etag string) bool {
	for _, e := range strings.Split(list, ",") {
		if strings.Trim(strings.TrimSpace(e), `"`) == etag {
			return true
		}
	}
	return false
}

// commit records the stored blob sums as key's object under the key lock,
// after the write checks: the preconditions, writePolicy allow_once, and
// Edit to replace an object.
func (h *Handler) commit(q *req, rp *repo.Repository, key, ct string, sums blob.Sums, m meta.S3ObjectMeta) (meta.Asset, error) {
	unlock, err := h.Store.Lock(q.ctx, lockKey(rp.Name, key))
	if err != nil {
		return meta.Asset{}, err
	}
	defer unlock()
	_, cur, exists, err := h.current(q, rp, key)
	if err != nil {
		return meta.Asset{}, err
	}
	if err := preconditions(q.r, exists, cur.ETag); err != nil {
		return meta.Asset{}, err
	}
	if exists && rp.WritePolicy == "allow_once" {
		return meta.Asset{}, s3err(http.StatusForbidden, "AccessDenied", "The bucket does not allow replacing objects.")
	}
	if exists && !h.Auth.CanPath(q.p, rp.Name, auth.Edit, key) {
		return meta.Asset{}, s3err(http.StatusForbidden, "AccessDenied", "You may not replace this object.")
	}
	a := h.Store.NewAsset(rp.Name, key, ct, sums)
	m.BlobSHA256 = sums.SHA256
	err = h.Store.Tx(q.ctx, rp, func(tx *meta.Tx) error {
		if err := tx.PutAsset(q.ctx, a); err != nil {
			return err
		}
		return tx.PutS3ObjectMeta(q.ctx, rp.Name, key, m)
	})
	if err != nil {
		return meta.Asset{}, err
	}
	h.Store.Emit(q.r, "upload", rp.Name, key, "")
	return a, nil
}

func (h *Handler) putObject(q *req, rp *repo.Repository) error {
	hdrs, err := objectHeaders(q.r.Header)
	if err != nil {
		return err
	}
	sums, err := q.checksums()
	if err != nil {
		return err
	}
	if q.p.Anonymous() {
		return errAccessDenied
	}
	stored, err := h.Store.WriteBlob(rp.Name, sums.wrap(q.body), sums.verify)
	if err != nil {
		return err
	}
	a, err := h.commit(q, rp, q.key, contentType(q.r.Header), stored, meta.S3ObjectMeta{Headers: hdrs})
	if err != nil {
		return err
	}
	q.w.Header().Set("ETag", quoteETag(a.MD5))
	q.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) getObject(q *req, rp *repo.Repository) error {
	a, err := h.Store.Get(q.ctx, rp.Name, q.key)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return errNoSuchKey
		}
		return err
	}
	if v := q.get("partNumber"); v != "" && v != "1" {
		return s3err(http.StatusRequestedRangeNotSatisfiable, "InvalidPartNumber", "The requested partnumber is not satisfiable")
	}
	h.serveObject(q.w, q.r, rp, a, q.q)
	return nil
}

// responseOverrides are the query parameters that replace a response
// header of a GET (a presigned URL's).
var responseOverrides = map[string]string{"response-content-type": "Content-Type", "response-content-language": "Content-Language",
	"response-expires": "Expires", "response-cache-control": "Cache-Control",
	"response-content-disposition": "Content-Disposition", "response-content-encoding": "Content-Encoding"}

// serveObject streams a with its S3 headers; it handles HEAD, Range and
// the conditional headers.
func (h *Handler) serveObject(w http.ResponseWriter, r *http.Request, rp *repo.Repository, a meta.Asset, query url.Values) {
	m, err := h.objectMeta(r.Context(), a)
	if err != nil {
		writeAny(w, r, err)
		return
	}
	f, err := h.Store.Open(a)
	if err != nil {
		writeAny(w, r, err)
		return
	}
	defer f.Close()
	hd := w.Header()
	for k, v := range m.Headers {
		hd.Set(k, v)
	}
	hd.Set("Content-Type", a.ContentType)
	hd.Set("ETag", quoteETag(m.ETag))
	hd.Set("Accept-Ranges", "bytes")
	for p, name := range responseOverrides {
		if v := query.Get(p); v != "" {
			hd.Set(name, v)
		}
	}
	sw := &statusWriter{ResponseWriter: w}
	http.ServeContent(sw, r, "", a.LastModified, f)
	if r.Method == http.MethodGet && sw.status >= 200 && sw.status < 300 && h.Store.Downloads != nil {
		h.Store.Downloads(a.Repo, a.Path)
	}
}

// writeAny answers err in the S3 form on the S3 API and as text under
// /repository/.
func writeAny(w http.ResponseWriter, r *http.Request, err error) {
	if strings.HasPrefix(r.URL.Path, "/repository/") {
		repo.WriteError(w, r, err)
		return
	}
	writeError(w, r, err)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (h *Handler) deleteObject(q *req, rp *repo.Repository) error {
	if err := h.remove(q, rp, q.key); err != nil {
		return err
	}
	q.w.WriteHeader(http.StatusNoContent)
	return nil
}

// remove deletes key's object; a missing one is no error (S3's rule).
func (h *Handler) remove(q *req, rp *repo.Repository, key string) error {
	unlock, err := h.Store.Lock(q.ctx, lockKey(rp.Name, key))
	if err != nil {
		return err
	}
	defer unlock()
	_, cur, exists, err := h.current(q, rp, key)
	if err != nil || !exists {
		return err
	}
	if v := q.r.Header.Get("If-Match"); v != "" && !matchETag(v, cur.ETag) && strings.TrimSpace(v) != "*" {
		return errPrecondition
	}
	deleted := false
	err = h.Store.Tx(q.ctx, rp, func(tx *meta.Tx) error {
		_, err := tx.DeleteAsset(q.ctx, rp.Name, key)
		if errors.Is(err, meta.ErrNotFound) {
			return nil
		}
		deleted = err == nil
		return err
	})
	if err == nil && deleted {
		h.Store.Emit(q.r, "delete", rp.Name, key, "")
	}
	return err
}

type deleteRequest struct {
	Quiet   bool `xml:"Quiet"`
	Objects []struct {
		Key string `xml:"Key"`
	} `xml:"Object"`
}

type deleteResult struct {
	XMLName xml.Name        `xml:"DeleteResult"`
	Xmlns   string          `xml:"xmlns,attr"`
	Deleted []deletedObject `xml:"Deleted"`
	Errors  []deleteError   `xml:"Error"`
}

type deletedObject struct {
	Key string `xml:"Key"`
}

type deleteError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// maxXMLBody bounds the XML documents a request sends (a delete of 1000
// keys of 1 KiB each is about 1 MiB).
const maxXMLBody = 2 << 20

// readXML reads q's body (at most maxXMLBody), checks it against the
// digests the request states, and decodes it into v.
func (q *req) readXML(v any) error {
	sums, err := q.checksums()
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(sums.wrap(q.body), maxXMLBody+1))
	if err != nil {
		return err
	}
	if len(b) > maxXMLBody {
		return errMalformedXML
	}
	m, s := md5.Sum(b), sha256.Sum256(b)
	if err := sums.verify(blob.Sums{MD5: hex.EncodeToString(m[:]), SHA256: hex.EncodeToString(s[:])}); err != nil {
		return err
	}
	if err := xml.NewDecoder(bytes.NewReader(b)).Decode(v); err != nil {
		return errMalformedXML
	}
	return nil
}

func (h *Handler) deleteObjects(q *req, rp *repo.Repository) error {
	var body deleteRequest
	if err := q.readXML(&body); err != nil {
		return err
	}
	if len(body.Objects) == 0 || len(body.Objects) > 1000 {
		return errMalformedXML
	}
	out := deleteResult{Xmlns: xmlns, Deleted: []deletedObject{}, Errors: []deleteError{}}
	for _, o := range body.Objects {
		err := checkKey(o.Key)
		if err == nil && !h.Auth.CanPath(q.p, rp.Name, auth.Delete, o.Key) {
			err = errAccessDenied
		}
		if err == nil {
			err = h.remove(q, rp, o.Key)
		}
		if err != nil {
			e := toS3(q.r, err)
			out.Errors = append(out.Errors, deleteError{Key: o.Key, Code: e.Code, Message: e.Msg})
			continue
		}
		if !body.Quiet {
			out.Deleted = append(out.Deleted, deletedObject{Key: o.Key})
		}
	}
	writeXML(q.w, q.r, out)
	return nil
}

type copyResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	Xmlns        string   `xml:"xmlns,attr"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

// copyObject copies x-amz-copy-source to q's key. Within one blob store the
// new object shares the source's blob: no bytes are copied.
func (h *Handler) copyObject(q *req, rp *repo.Repository) error {
	src := q.r.Header.Get("X-Amz-Copy-Source")
	if i := strings.Index(src, "?"); i >= 0 {
		return errNotImplemented // versionId
	}
	src, err := url.PathUnescape(src)
	if err != nil {
		return invalidArgument("x-amz-copy-source is not URL-encoded")
	}
	sb, sk, ok := strings.Cut(strings.TrimPrefix(src, "/"), "/")
	if !ok || checkKey(sk) != nil {
		return invalidArgument("x-amz-copy-source must name a bucket and a key")
	}
	srp := h.Registry.Lookup(q.ctx, sb)
	if srp == nil || srp.Format != "s3" {
		return errNoSuchBucket
	}
	if !h.Auth.CanPath(q.p, srp.Name, auth.Read, sk) {
		return errAccessDenied
	}
	if !srp.IsOnline() {
		return s3err(http.StatusServiceUnavailable, "ServiceUnavailable", "The source bucket's repository is offline.")
	}
	a, err := h.Store.Get(q.ctx, srp.Name, sk)
	if errors.Is(err, repo.ErrNotFound) {
		return errNoSuchKey
	}
	if err != nil {
		return err
	}
	sm, err := h.objectMeta(q.ctx, a)
	if err != nil {
		return err
	}
	if err := copyPreconditions(q.r, a, sm.ETag); err != nil {
		return err
	}
	replace := strings.EqualFold(q.r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE")
	if srp.Name == rp.Name && sk == q.key && !replace {
		return s3err(http.StatusBadRequest, "InvalidRequest", "This copy request is illegal because it is trying to copy an object to itself without changing the object's metadata.")
	}
	ct, hdrs := a.ContentType, sm.Headers
	if replace {
		if hdrs, err = objectHeaders(q.r.Header); err != nil {
			return err
		}
		ct = contentType(q.r.Header)
	}
	sums := blob.Sums{SHA256: a.BlobSHA256, SHA1: a.SHA1, MD5: a.MD5, SHA512: a.SHA512, Size: a.Size}
	from, err := h.Store.BlobStore(srp.Name)
	if err != nil {
		return err
	}
	to, err := h.Store.BlobStore(rp.Name)
	if err != nil {
		return err
	}
	if from != to {
		f, err := h.Store.Open(a)
		if err != nil {
			return err
		}
		sums, err = h.Store.WriteBlob(rp.Name, f, nil)
		f.Close()
		if err != nil {
			return err
		}
	}
	na, err := h.commit(q, rp, q.key, ct, sums, meta.S3ObjectMeta{Headers: hdrs})
	if err != nil {
		return err
	}
	writeXML(q.w, q.r, copyResult{Xmlns: xmlns, LastModified: s3Time(na.LastModified), ETag: quoteETag(na.MD5)})
	return nil
}

func copyPreconditions(r *http.Request, a meta.Asset, etag string) error {
	hd := r.Header
	if v := hd.Get("X-Amz-Copy-Source-If-Match"); v != "" && !matchETag(v, etag) {
		return errPrecondition
	}
	if v := hd.Get("X-Amz-Copy-Source-If-None-Match"); v != "" && matchETag(v, etag) {
		return errPrecondition
	}
	if t, err := http.ParseTime(hd.Get("X-Amz-Copy-Source-If-Modified-Since")); err == nil && !a.LastModified.After(t) {
		return errPrecondition
	}
	if t, err := http.ParseTime(hd.Get("X-Amz-Copy-Source-If-Unmodified-Since")); err == nil && a.LastModified.Truncate(time.Second).After(t) {
		return errPrecondition
	}
	return nil
}

type accessControlPolicy struct {
	XMLName xml.Name `xml:"AccessControlPolicy"`
	Xmlns   string   `xml:"xmlns,attr"`
	Owner   owner    `xml:"Owner"`
	Grants  []grant  `xml:"AccessControlList>Grant"`
}

type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type grant struct {
	Grantee struct {
		XMLNS string `xml:"xmlns:xsi,attr"`
		Type  string `xml:"xsi:type,attr"`
		ID    string `xml:"ID"`
		Name  string `xml:"DisplayName"`
	} `xml:"Grantee"`
	Permission string `xml:"Permission"`
}

// acl answers a fixed ACL: the caller has full control. Access is the
// roles' business; the document only keeps clients that ask happy.
func (h *Handler) acl(q *req) error {
	o := owner{ID: q.p.Owner(), DisplayName: q.p.User}
	g := grant{Permission: "FULL_CONTROL"}
	g.Grantee.XMLNS, g.Grantee.Type, g.Grantee.ID, g.Grantee.Name = "http://www.w3.org/2001/XMLSchema-instance", "CanonicalUser", o.ID, o.DisplayName
	writeXML(q.w, q.r, accessControlPolicy{Xmlns: xmlns, Owner: o, Grants: []grant{g}})
	return nil
}

// serveDir lists a directory of the read-only /repository/ view.
func (h *Handler) serveDir(w http.ResponseWriter, r *http.Request, rp *repo.Repository, dir string) {
	entries, truncated, err := h.Store.DB.ListDir(r.Context(), rp.Name, dir, 1000)
	if err != nil {
		repo.WriteError(w, r, err)
		return
	}
	if len(entries) == 0 && dir != "" {
		repo.WriteError(w, r, repo.ErrNotFound)
		return
	}
	title := html.EscapeString("/repository/" + rp.Name + "/" + dir)
	var sb strings.Builder
	sb.WriteString("<!DOCTYPE html>\n<html><head><meta charset=\"utf-8\"><title>Index of " + title + "</title></head>\n<body>\n<h1>Index of " + title + "</h1>\n<ul>\n")
	if dir != "" {
		sb.WriteString("<li><a href=\"../\">../</a></li>\n")
	}
	for _, e := range entries {
		name, href := e.Name, url.PathEscape(e.Name)
		if e.Dir {
			name, href = name+"/", href+"/"
		}
		sb.WriteString("<li><a href=\"" + html.EscapeString(href) + "\">" + html.EscapeString(name) + "</a></li>\n")
	}
	sb.WriteString("</ul>\n")
	if truncated {
		sb.WriteString("<p>(listing truncated)</p>\n")
	}
	sb.WriteString("</body></html>\n")
	repo.ServeText(w, r, sb.String(), "text/html; charset=utf-8", time.Time{})
}
