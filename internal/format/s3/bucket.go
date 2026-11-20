package s3

import (
	"encoding/base64"
	"encoding/xml"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"commandcenter/internal/auth"
	"commandcenter/internal/config"
	"commandcenter/internal/meta"
	"commandcenter/internal/repo"
	"commandcenter/internal/sigv4"
)

type listAllMyBucketsResult struct {
	XMLName xml.Name     `xml:"ListAllMyBucketsResult"`
	Xmlns   string       `xml:"xmlns,attr"`
	Owner   owner        `xml:"Owner"`
	Buckets []bucketInfo `xml:"Buckets>Bucket"`
}

type bucketInfo struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
	Region       string `xml:"BucketRegion,omitempty"`
}

// listBuckets answers the s3 repositories the caller may read.
func (h *Handler) listBuckets(q *req) error {
	if q.p.Anonymous() {
		return errAccessDenied
	}
	out := listAllMyBucketsResult{Xmlns: xmlns, Owner: owner{ID: q.p.Owner(), DisplayName: q.p.User}, Buckets: []bucketInfo{}}
	for _, rp := range h.Registry.Snapshot().Sorted {
		if rp.Format == "s3" && h.Auth.Can(q.p, rp.Name, auth.Read) {
			out.Buckets = append(out.Buckets, bucketInfo{Name: rp.Name, CreationDate: s3Time(h.created(q, rp.Name)), Region: h.Config.Region})
		}
	}
	writeXML(q.w, q.r, out)
	return nil
}

// created is when a repository was last created or changed (the registry
// keeps no creation time of its own); zero when unknown.
func (h *Handler) created(q *req, name string) (t time.Time) {
	t, _ = h.Store.DB.RepositoriesUpdatedAt(q.ctx, []string{name})
	return t
}

type locationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	Xmlns   string   `xml:"xmlns,attr"`
	Value   string   `xml:",chardata"`
}

// location answers the configured region; us-east-1 is the empty value.
func (h *Handler) location(q *req) error {
	v := h.Config.Region
	if v == "us-east-1" {
		v = ""
	}
	writeXML(q.w, q.r, locationConstraint{Xmlns: xmlns, Value: v})
	return nil
}

type versioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Xmlns   string   `xml:"xmlns,attr"`
}

// createBucket creates an s3 hosted repository in the default blob store.
// It needs the privilege to create s3 repositories.
func (h *Handler) createBucket(q *req) error {
	if q.p.Anonymous() || !h.Auth.CanAdminRepo(q.p, "", "s3", auth.Add) {
		return errAccessDenied
	}
	if h.ReadOnly != nil {
		if why := h.ReadOnly(); why != "" {
			return s3err(http.StatusServiceUnavailable, "ServiceUnavailable", why)
		}
	}
	if rp := h.Registry.Lookup(q.ctx, q.bucket); rp != nil {
		if rp.Format == "s3" && h.Auth.Can(q.p, rp.Name, auth.Read) {
			return s3err(http.StatusConflict, "BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.")
		}
		return s3err(http.StatusConflict, "BucketAlreadyExists", "The requested bucket name is not available.")
	}
	if !config.ValidBucketName(q.bucket) {
		return s3err(http.StatusBadRequest, "InvalidBucketName", "The specified bucket is not valid.")
	}
	def := config.Repository{Name: q.bucket, Format: "s3", Type: "hosted"}
	config.ApplyRepositoryDefaults(&def)
	if _, err := h.Registry.CreateDef(q.ctx, def); err != nil {
		var ve *repo.ValidationError
		if errors.As(err, &ve) {
			msg := "The specified bucket is not valid."
			if len(ve.Errors) > 0 {
				msg = ve.Errors[0]
			}
			return s3err(http.StatusBadRequest, "InvalidBucketName", msg)
		}
		if repo.StatusOf(err) == http.StatusConflict {
			return s3err(http.StatusConflict, "BucketAlreadyExists", "The requested bucket name is not available.")
		}
		return err
	}
	h.Store.Emit(q.r, "repo.create", def.Name, def.Name, "s3/hosted")
	q.w.Header().Set("Location", "/"+q.bucket)
	q.w.WriteHeader(http.StatusOK)
	return nil
}

// deleteBucket deletes an empty s3 repository. It needs the privilege to
// delete it as an administrator.
func (h *Handler) deleteBucket(q *req) error {
	rp := h.Registry.Lookup(q.ctx, q.bucket)
	if rp == nil || rp.Format != "s3" {
		if !h.Auth.Can(q.p, q.bucket, auth.Read) {
			return errAccessDenied
		}
		return errNoSuchBucket
	}
	if q.p.Anonymous() || !h.Auth.CanAdminRepo(q.p, rp.Name, "s3", auth.Delete) {
		return errAccessDenied
	}
	if h.ReadOnly != nil {
		if why := h.ReadOnly(); why != "" {
			return s3err(http.StatusServiceUnavailable, "ServiceUnavailable", why)
		}
	}
	has, err := h.Store.DB.RepoHasAssets(q.ctx, rp.Name)
	if err != nil {
		return err
	}
	if has {
		return s3err(http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty")
	}
	// The delete drops the roles' privileges on the repository: under the
	// lock of the access writes, as the web UI's delete.
	unlock, err := h.Store.DB.Lock(q.ctx, accessLock)
	if err != nil {
		return err
	}
	defer unlock()
	if err := h.Registry.Delete(q.ctx, rp.Name); err != nil {
		return err
	}
	h.Store.Emit(q.r, "repo.delete", rp.Name, rp.Name, "s3/hosted")
	q.w.WriteHeader(http.StatusNoContent)
	return nil
}

type listBucketResult struct {
	XMLName        xml.Name       `xml:"ListBucketResult"`
	Xmlns          string         `xml:"xmlns,attr"`
	Name           string         `xml:"Name"`
	Prefix         string         `xml:"Prefix"`
	Delimiter      string         `xml:"Delimiter,omitempty"`
	EncodingType   string         `xml:"EncodingType,omitempty"`
	MaxKeys        int            `xml:"MaxKeys"`
	IsTruncated    bool           `xml:"IsTruncated"`
	Contents       []objectInfo   `xml:"Contents"`
	CommonPrefixes []commonPrefix `xml:"CommonPrefixes"`
	// V2
	KeyCount              *int   `xml:"KeyCount,omitempty"`
	ContinuationToken     string `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string `xml:"NextContinuationToken,omitempty"`
	StartAfter            string `xml:"StartAfter,omitempty"`
	// V1
	Marker     *string `xml:"Marker,omitempty"`
	NextMarker string  `xml:"NextMarker,omitempty"`
}

type objectInfo struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
	Owner        *owner `xml:"Owner,omitempty"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// listing is what one page of a listing found.
type listing struct {
	contents  []meta.Asset
	prefixes  []string
	truncated bool
	next      cursor // where the next page starts
}

// cursor is a position in a bucket's keys: from, inclusive or not.
type cursor struct {
	from      string
	exclusive bool
}

func (c cursor) token() string {
	k := "i"
	if c.exclusive {
		k = "e"
	}
	return base64.RawURLEncoding.EncodeToString([]byte(k + c.from))
}

func parseToken(t string) (cursor, bool) {
	b, err := base64.RawURLEncoding.DecodeString(t)
	if err != nil || len(b) == 0 || (b[0] != 'i' && b[0] != 'e') || !utf8.Valid(b[1:]) {
		return cursor{}, false
	}
	return cursor{from: string(b[1:]), exclusive: b[0] == 'e'}, true
}

// list reads up to max keys and common prefixes of rp under prefix from
// start. A common prefix counts as one key, and the keys it rolls up are
// skipped in the database, not read.
func (h *Handler) list(q *req, rp *repo.Repository, prefix, delim string, start cursor, max int) (listing, error) {
	var out listing
	cur := start
	batch := max + 1
	if batch > 1001 {
		batch = 1001
	}
	for {
		rows, err := h.Store.DB.ListObjects(q.ctx, rp.Name, prefix, cur.from, cur.exclusive, batch)
		if err != nil {
			return listing{}, err
		}
		jumped := false
		for _, a := range rows {
			if len(out.contents)+len(out.prefixes) == max {
				out.truncated = true
				return out, nil
			}
			rest := a.Path[len(prefix):]
			if i := strings.Index(rest, delim); delim != "" && i >= 0 {
				cp := prefix + rest[:i+len(delim)]
				out.prefixes = append(out.prefixes, cp)
				end, ok := prefixEnd(cp)
				if !ok {
					return out, nil // nothing can follow
				}
				cur, out.next = cursor{from: end}, cursor{from: end}
				jumped = true
				break
			}
			out.contents = append(out.contents, a)
			cur = cursor{from: a.Path, exclusive: true}
			out.next = cur
		}
		if !jumped && len(rows) < batch {
			return out, nil
		}
	}
}

// prefixEnd is the least string above every string that starts with
// prefix (meta's rule: its last code point raised by one).
func prefixEnd(prefix string) (string, bool) {
	r := []rune(prefix)
	for len(r) > 0 {
		last := r[len(r)-1]
		r = r[:len(r)-1]
		if last == utf8.MaxRune {
			continue
		}
		next := last + 1
		if next >= 0xD800 && next <= 0xDFFF {
			next = 0xE000
		}
		return string(append(r, next)), true
	}
	return "", false
}

func (h *Handler) listObjects(q *req, rp *repo.Repository) error {
	v2 := q.get("list-type") == "2"
	prefix, delim := q.get("prefix"), q.get("delimiter")
	if !utf8.ValidString(prefix) || !utf8.ValidString(delim) {
		return invalidArgument("prefix and delimiter must be UTF-8")
	}
	max := 1000
	if v := q.get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return invalidArgument("max-keys must be a non-negative integer")
		}
		max = min(n, 1000)
	}
	enc := q.get("encoding-type")
	if enc != "" && enc != "url" {
		return invalidArgument("Invalid Encoding Method specified in Request")
	}
	start := cursor{from: prefix}
	out := listBucketResult{Xmlns: xmlns, Name: rp.Name, Prefix: encodeKey(prefix, enc), Delimiter: encodeKey(delim, enc),
		EncodingType: enc, MaxKeys: max, Contents: []objectInfo{}, CommonPrefixes: []commonPrefix{}}
	if v2 {
		if t := q.get("continuation-token"); t != "" {
			c, ok := parseToken(t)
			if !ok {
				return invalidArgument("The continuation token provided is incorrect")
			}
			start, out.ContinuationToken = c, t
		} else if sa := q.get("start-after"); sa != "" {
			start, out.StartAfter = cursor{from: sa, exclusive: true}, encodeKey(sa, enc)
		}
	} else {
		marker := q.get("marker")
		out.Marker = &marker
		if marker != "" {
			start = cursor{from: marker, exclusive: true}
			// A marker that is a common prefix continues after its keys.
			if delim != "" && strings.HasPrefix(marker, prefix) && strings.HasSuffix(marker, delim) {
				if end, ok := prefixEnd(marker); ok {
					start = cursor{from: end}
				}
			}
			m := encodeKey(marker, enc)
			out.Marker = &m
		}
	}
	var l listing
	if max > 0 {
		var err error
		if l, err = h.list(q, rp, prefix, delim, start, max); err != nil {
			return err
		}
	}
	fetchOwner := !v2 || q.get("fetch-owner") == "true"
	for _, a := range l.contents {
		m, err := h.objectMeta(q.ctx, a)
		if err != nil {
			return err
		}
		o := objectInfo{Key: encodeKey(a.Path, enc), LastModified: s3Time(a.LastModified), ETag: quoteETag(m.ETag), Size: a.Size, StorageClass: "STANDARD"}
		if fetchOwner {
			o.Owner = &owner{ID: rp.Name, DisplayName: rp.Name}
		}
		out.Contents = append(out.Contents, o)
	}
	for _, p := range l.prefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{Prefix: encodeKey(p, enc)})
	}
	out.IsTruncated = l.truncated
	if v2 {
		n := len(out.Contents) + len(out.CommonPrefixes)
		out.KeyCount = &n
		if l.truncated {
			out.NextContinuationToken = l.next.token()
		}
	} else if l.truncated {
		last := ""
		if len(l.prefixes) > 0 && (len(l.contents) == 0 || l.prefixes[len(l.prefixes)-1] > l.contents[len(l.contents)-1].Path) {
			last = l.prefixes[len(l.prefixes)-1]
		} else if len(l.contents) > 0 {
			last = l.contents[len(l.contents)-1].Path
		}
		out.NextMarker = encodeKey(last, enc)
	}
	writeXML(q.w, q.r, out)
	return nil
}

// encodeKey encodes a key for encoding-type=url ("/" stays).
func encodeKey(s, enc string) string {
	if enc != "url" {
		return s
	}
	return sigv4.Escape(s, false)
}
